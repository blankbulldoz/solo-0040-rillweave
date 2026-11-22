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
| POST | `G/inventory` | Endpoint kinds, shared property and arc kind; bounded page of missing directed arcs |
| POST | `G/pins` | Holds current state; returns a handle and its revision |
| DELETE | `G/pins/{handle}` | Releases handle; in-flight reads already resolved may finish |
| POST | `G/compare` | `from` and `to` handles (empty means head); identity-based difference |
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

### Dependency inventory

`POST G/inventory` audits which registered same-value pairs lack a required
directed arc. The request gives `from_kind`, `to_kind`, `property`,
`arc_kind`, optional `limit` (default 100, capped at 1000), `budget`
(default 300000) and an opaque `cursor`. Pairs exist only between vertices of
the two endpoint kinds that both carry the same property value: a value
registered on just one side cannot pair, and a missing property never claims
a value. An optional `value` only supplies the scalar kind for schema
validation; the audit covers every shared value, never coerces types and the
declared arc kind must connect the two endpoint kinds.

A pair is covered only by an arc of `arc_kind` going from the `from` record to
the `to` record. Reverse arcs, other arc kinds and parallel arcs do not change
the answer; each surviving gap returns both original vertex records. The scan
walks shared-value cohorts in deterministic value order without materializing
the Cartesian product. Each page reports `gaps`, `matched`, `scanned`,
`truncated`, `complete` and, when more pairs remain, a `cursor`. A page is
`complete` only after every pair has been inspected; a budget or limit stop
keeps the boundary in `cursor`, so an unfinished page never reads as "no
gaps". The cursor is bound to the revision that issued it and a changed head
rejects it with 409; hold a pin to page through a fixed view.

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
`path`, `components`, `inventory`, `pin`, `compare`, `backup` or `restart`
to select one.
Formal tests are deferred; no test runner is required.
