# Rillweave

Rillweave is a persistent directed property graph database with an HTTP JSON
API. Graph keys, typed properties, multigraph semantics and snapshot selection
are part of the database contract, not an application-specific data model.

## Run

Use Go 1.22+ on macOS or Linux. The server has no third-party dependencies.
The data directory must be controlled by one server on a filesystem supporting
exclusive file locking, atomic replacement and directory synchronization.

```sh
go build -o rillweaved ./cmd/rillweaved
./rillweaved -listen 127.0.0.1:8174 -data ./rillweave-data
```

The service prints its bound address and provides `GET /ready` (HTTP 200 on a
usable instance, 503 on a detected durability fault). The service is meant for
a trusted network and does not authenticate requests. A data directory is a
database, not a place to stage arbitrary inputs or share writes between servers.

## API

Let `G` be `/rill/v1/graphs/{name}`. Pass `?view={handle}` on an applicable
read to use a fixed view rather than the head. Normal reads return `graph`,
`revision` and `result`. Errors give `code` and `details`: 400 invalid input,
404 absent resource, 409 conflicting current graph state, 422 budget/capacity,
503 storage unavailable. Unrecognized routes use the HTTP mux response.

| Method | Resource | Input / Result |
| --- | --- | --- |
| POST | `/rill/v1/graphs` | `{"name":"...","schema":{...}}`; creates revision 1 |
| GET | `G/catalog` | Schema, vertex and arc counts |
| POST | `G/commit` | `{"base":1,"edits":[...]}`; new head summary |
| GET | `G/vertices/{id}` | One vertex in the selected revision |
| POST | `G/lookup` | `kind`, `property`, `value`, optional `limit`; IDs and exact count |
| POST | `G/adjacency` | `vertex`, optional selection and `limit`; matching arcs by ID |
| POST | `G/reach` | `root`, `max_hops`, optional selection; vertices with minimum hops |
| POST | `G/path` | `from`, `to`, `max_hops`, optional selection; cheapest bounded path |
| POST | `G/components` | Optional `kinds`, `budget`; strongly connected groups and directed links |
| POST | `G/pins` | Holds current state; returns a handle and its revision |
| DELETE | `G/pins/{handle}` | Releases handle; in-flight reads already resolved may finish |
| POST | `G/compare` | `from` and `to` handles (empty means head); identity-based difference |
| POST | `G/compare/contracts` | Same pin pair plus registered read contracts; per-contract disposition, full match movement and bounded witnesses |
| GET | `G/image` | Format, SHA-256 digest and base64 payload for the chosen revision |
| PUT | `G/image` | Image envelope; installs a new graph at 1 and returns source and installed summaries |

For example, install a schema and submit an arc before its two vertices in
the same commit. The `join` rule requires `strut` at both ends.

```sh
curl -fsS http://127.0.0.1:8174/rill/v1/graphs \
  -H 'Content-Type: application/json' \
  -d '{"name":"abutment","schema":{"vertices":{"strut":{"properties":{"tag":{"kind":"text","unique":true}}}},"arcs":{"join":{"from":"strut","to":"strut"}}}}'
curl -fsS http://127.0.0.1:8174/rill/v1/graphs/abutment/commit \
  -H 'Content-Type: application/json' \
  -d '{"base":1,"edits":[{"op":"put_arc","arc":{"id":"brace","kind":"join","from":"hinge","to":"spar","cost":4}},{"op":"put_vertex","vertex":{"id":"hinge","kind":"strut","properties":{"tag":"north"}}},{"op":"put_vertex","vertex":{"id":"spar","kind":"strut","properties":{"tag":"south"}}}]}'
curl -fsS http://127.0.0.1:8174/rill/v1/graphs/abutment/path \
  -H 'Content-Type: application/json' \
  -d '{"from":"hinge","to":"spar","max_hops":2}'
```

### Changes and selection

Edit payloads: `put_vertex` takes `vertex`, `put_arc` takes `arc`,
`drop_vertex` and `drop_arc` take `id`, and `set_schema` takes `schema`.
Puts replace full records. Existing records are validated against the schema
after all edits; drop operations still require an existing ID at their
position in the request. Required and unique vertex properties are checked by
kind. Optional missing values do not claim a unique value. Arc properties may
be required but not unique. Names are lowercase ASCII identifiers up to 64
characters (digits, hyphens and underscores are allowed after the first
alphanumeric). Values are text up to 1024 bytes, int64 or boolean.

Selection `direction` is `out` (default), `in` or `both`. A reversed step
uses the stored arc ID and cost; the result lists vertices in traversal
sequence. `kinds` empty means all. `max_hops` in [0,32] includes zero hops
(missing defaults to zero). Costs are strictly positive, so a return to the
starting vertex cannot beat the empty path. Ties prefer fewer arcs, then
the lexicographically smallest arc ID sequence. Reach lists vertices in
deterministic breadth-first discovery order. Components use only forward
links for grouping and give each group its smallest vertex ID.

`budget` counts candidate arc inspections, including candidates rejected by
kind selection and repeated inspections during graph search. Zero or omitted
uses 300000; exhaustion rejects the whole query. `limit` for lookup and
adjacency defaults to 100, capped at 1000. It limits returned rows, not
matching count or budget charge. A valid no-route result is `found:false`;
that is different from a missing endpoint or a query that ran out of budget.

### Registered read contracts

`POST G/compare` still explains identity changes. Add an optional `contracts`
list (plus `witness_limit`) and the same call also audits registered read
conditions; the identity difference is returned as before and the audit
appears under `contract_audit`. Without `contracts` the response is exactly
the existing compare object. `POST G/compare/contracts` serves the same audit
on its own.

Either way the body takes the `from`/`to` handles (empty means head), optional
`witness_limit` in [1,50] (default 5) and 1..256 `contracts`. Each contract
has a unique `id` and exactly one selector:

- `{"vertex":{"kind":"strut","property":"tag","value":"north",
  "requires":["required","unique"]}}` is a typed-equality point query.
  `requires` names guarantees the consumer reads through; each must already
  hold on the old revision.
- `{"arc":{"kind":"join","from":"strut","to":"strut"}}` selects original arcs
  by arc kind and the expected kinds of their start and end vertices.

Registration is checked against the older revision: it must be executable
there (declared kind and property, matching scalar type, existing guarantee,
matching arc endpoint rule) or the whole request is `400 invalid`. The audit
repeats `from`/`to` revisions and the handles used, so a conclusion is bound
to its pin pair. Per contract:

- `inapplicable` with `reason`: the newer revision cannot execute the
  original condition, because the kind/property or its scalar type is gone or
  the arc kind's registered endpoint kinds changed. No match data is reported.
- `changed`: still executable. `matches` lists complete added and removed
  match IDs with `from_total`/`to_total`; `guarantees` lists every `required`
  or `unique` guarantee the registration depends on whose value changed and
  whether it was `lost`.
- `unaffected`: no guarantee movement and no match movement.

`matches` is computed from the full revisions, never from a `limit`-truncated
lookup or adjacency window. `witnesses` samples at most `witness_limit`
original vertex or arc records per direction for spot checks, with explicit
`added_truncated`/`removed_truncated` flags; the complete identity movement
stays in `matches`. `summary` counts dispositions and reports
`evaluated_conditions` and `shared_registrations`, so many contracts naming
the same property or arc kind are evaluated once rather than per consumer.
Unrelated writes elsewhere in the graph leave a contract unaffected.

### Storage and Verification

A namespace has at most 10000 vertices, 40000 arcs and a 12 MiB image payload;
a commit has at most 2000 edits. At most 32 namespaces and 16 simultaneous
pins exist per process; pins are not durable, and two pins can hold the same
revision. Use a successful image export for an offline representation of a
pinned revision. Export does not freeze ongoing writes. In a restore, the
local graph identity and revision are new; the original graph remains intact.
Copying an existing `.rill` file into a fresh data directory instead reopens
that durable graph as-is. Startup checks all active head files and ignores
uninstalled temp files. See [PROJECT_SPEC.md](PROJECT_SPEC.md).

```sh
go build ./...
go vet ./...
python3 examples/walkthrough.py --workflow all
```

The Python 3 walkthrough starts temporary server processes with bounded
synthetic inputs and prints success and rejection evidence. Use
`--workflow catalog`, `commit`, `vertex`, `lookup`, `adjacency`, `reach`,
`path`, `components`, `pin`, `compare`, `backup` or `restart` to select one.
Formal tests are deferred; no test runner is required.
