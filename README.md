# Rillweave

Rillweave is a persistent directed property graph database with an HTTP JSON
API. Graph keys, typed properties, multigraph semantics and snapshot selection
are part of the database contract, not an application-specific data model.

## Run

Use Go 1.22+ on macOS or Linux. The server has one third-party Go module
(a pure-Go SQLite driver, so no cgo or separate SQLite installation is
needed). The data directory must be controlled by one server on a filesystem
supporting exclusive file locking, atomic replacement and directory
synchronization.

```sh
go build -o rillweaved ./cmd/rillweaved
./rillweaved -listen 127.0.0.1:8174 -data ./rillweave-data
```

The service prints its bound address and provides `GET /ready` (HTTP 200 on a
usable instance, 503 on a detected durability fault). The service is meant for
a trusted network and does not authenticate requests. A data directory is a
database, not a place to stage arbitrary inputs or share writes between servers.

### Storage backends

A data directory uses exactly one active storage backend, never both:

| Mode | Active storage | When used |
| --- | --- | --- |
| legacy file mode | one `{name}.rill` head file per graph | directories created before the SQLite cutover that have never been migrated |
| SQLite mode | the single `rillweave.db` database | every directory after a successful migration, including new directories written by a migrated server |

Once `rillweave.db` exists it is authoritative. A `.rill` file copied back into
the directory is logged and ignored; the server cannot silently fall back to an
old head and lose a commit already confirmed in the database. Each head row in
SQLite stores the same versioned image envelope (format, SHA-256 digest and
payload) as the legacy file, with the graph's local name and exact revision;
migration never reuses the HTTP import path, so revisions are never reset to 1.

### Migrating a data directory to SQLite

The cutover is an offline operation run against the real data directory. It is
crash-safe and rerunnable; it takes the same exclusive directory lock as the
server, so it cannot run while a server holds the directory.

1. **Stop-write boundary.** Stop the running server (SIGTERM and wait for
   exit). No traffic can be served during the cutover, and a migration
   attempted while a server is running fails with `cannot acquire graph
   directory lock` instead of touching any file.
2. **Back up the data directory** (a plain copy is sufficient).
3. **Run the cutover:**

   ```sh
   ./rillweaved -data ./rillweave-data -migrate
   ```

   It reads every valid `*.rill` head, verifies each image's digest and
   contents, writes them into a staged `rillweave.db.stage`, compares the
   staged names and revisions against the source heads, then atomically
   renames the staged database to `rillweave.db` and syncs the directory.
   Legacy heads move to `legacy-heads/` (kept for rollback, never reopened),
   and uninstalled `.rill-pending-*` temp files are deleted. A JSON report
   lists every converted graph with its preserved revision.
4. **Start the server normally.** It opens `rillweave.db`, reconstructs all
   in-memory indexes from the stored images and serves the identical API.

Deciding which storage is usable after an interrupted conversion:

| Observation on disk | Meaning | Action |
| --- | --- | --- |
| no `rillweave.db`; `*.rill` heads present; only `rillweave.db.stage*` leftovers | interruption before the commit point | the legacy heads are intact and the old server still works; rerun `-migrate` (staging files are removed automatically) |
| `rillweave.db` present, with or without `*.rill` files | commit point reached; interruption happened during archiving/cleanup | SQLite mode is active; rerun `-migrate` to verify the database, finish archiving and remove temp files; the report carries `already_migrated: true` |
| migration reported an error and no database exists | a source head failed verification | fix or restore the head from backup; the directory stays in legacy mode and no partial database is left behind |

Rerunning `-migrate` on an already migrated directory only verifies the
database schema and completes any unfinished cleanup; it never rebuilds or
overwrites stored heads. Rollback after a successful cutover requires stopping
the server, moving `rillweave.db` (and any `-wal`/`-shm` sidecars) aside and
moving the files from `legacy-heads/` back into the data directory.

`RILLWEAVE_MIGRATE_ABORT_BEFORE_RENAME=1` makes the migration command fail on
purpose after staging but before the activation rename; it exists solely to
exercise the pre-commit interruption path during drills and is not part of
normal operation.

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

### Storage and Verification

A namespace has at most 10000 vertices, 40000 arcs and a 12 MiB image payload;
a commit has at most 2000 edits. At most 32 namespaces and 16 simultaneous
pins exist per process; pins are not durable, and two pins can hold the same
revision. Use a successful image export for an offline representation of a
pinned revision. Export does not freeze ongoing writes. In a restore, the
local graph identity and revision are new; the original graph remains intact.
Copying an existing `.rill` file into a fresh legacy-mode data directory
reopens that durable graph as-is. In legacy mode startup checks all active
head files and ignores uninstalled temp files; in SQLite mode startup reads
the heads table of `rillweave.db` and ignores any stray `.rill` file. See the
migration section above and [PROJECT_SPEC.md](PROJECT_SPEC.md).

```sh
go build ./...
go vet ./...
python3 examples/walkthrough.py --workflow all
python3 examples/migration_walkthrough.py
```

The Python 3 walkthroughs start temporary server processes with bounded
synthetic inputs and print success and rejection evidence. The migration
walkthrough builds a legacy directory with graphs at different revisions plus
an abandoned temp file, exercises the blocked, interrupted and rerun cutover
paths, and proves query equality across the cutover plus restart durability of
a new commit. Use `--workflow catalog`, `commit`, `vertex`, `lookup`,
`adjacency`, `reach`, `path`, `components`, `pin`, `compare`, `backup` or
`restart` to select one workflow of the main walkthrough. Formal tests are
deferred; no test runner is required.
