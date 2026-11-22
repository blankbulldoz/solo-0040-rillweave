# Schema Inference Tool (`rillweave-infer`)

`rillweave-infer` turns externally exported vertex/arc JSON into an
installable Rillweave graph without anyone reading a few records and
guessing the types. It scans **every** record, keeps original scalar
tokens distinct, reports conflicts with record-level evidence and
several split plans, and emits an image that passes the same
`catalog.Validate` path used by service-side restore.

## Why two phases

| Phase | Cost | Inputs | Outputs |
| --- | --- | --- | --- |
| `infer` | full scan, once | export files | `profile.json`, `decisions.starter.json` |
| `build` | mapping + validation only | same export files, profile, confirmed decisions | `graph.json`, `schema.json`, `image.json`, `build-report.json` |

The profile records the SHA-256 of every input file. `build` recomputes
those checksums and refuses to run if any input changed, so a naming
iteration never silently runs against mutated source data. Editing a
naming suggestion only reruns `build` (milliseconds); you do not pay for
a full re-inference.

## Scalar identity is never coerced

`ingest.Classify` inspects the raw JSON token rather than letting a
decoder widen it:

| Token | Observed as |
| --- | --- |
| `"5432"` | text |
| `5432` | int |
| `true` | bool |
| `"true"` | text (not bool) |
| `1.5`, `1e3` | unsupported float — blocker, original token kept as evidence |
| `null` | unsupported — blocker; absent is not a value |
| object/array | unsupported nested value — blocker |

A missing property is counted under `missing`, never as empty text,
zero or false. The only way a value changes type is an `explicit_cast`
action carrying an `owner_note`; inference itself never emits one.

## What the profile reports

Findings carry a severity, a traceable subject, evidence (`file`,
`ordinal`, record `id`, original raw token) and ready-to-copy plans:

- `vertex_property_type_conflict` / `arc_property_type_conflict` — one
  property name holds multiple scalar identities. Plans: split into
  `name_text`/`name_int`/`name_bool`, split the entity kind, or an
  owner-authorized explicit cast.
- `arc_endpoint_pair_conflict` — one arc kind connects multiple
  endpoint kind pairs. Plans: one arc kind per pair, unify endpoint
  kinds through renames, or exclude non-canonical pairs.
- `unsupported_scalar`, `invalid_identifier`, `duplicate_vertex_id`,
  `duplicate_arc_id`, `dangling_arc_endpoint`, `invalid_arc_cost`,
  `vertex_kind_mismatch`, `capacity_exceeded`.

## Workflow

```sh
go build -o rillweave-infer ./cmd/rillweave-infer

# 1. Full scan (inputs may be files or directories of .json)
./rillweave-infer infer -out ./preview -name my-graph ./exports

# 2. Review findings and their plans
./rillweave-infer check -profile ./preview/profile.json

# 3. The data owner confirms or edits the decisions
cp ./preview/decisions.starter.json ./preview/decisions.json
$EDITOR ./preview/decisions.json

# 4. Assemble and validate against the real catalog rules
./rillweave-infer build \
  -profile ./preview/profile.json \
  -decisions ./preview/decisions.json \
  -out ./preview/build ./exports

# 5. Install the validated image
curl -fsS -X PUT http://127.0.0.1:8174/rill/v1/graphs/my-graph/image \
  -H 'Content-Type: application/json' --data @./preview/build/image.json
```

The build fails loudly instead of dropping data when:

- an input checksum no longer matches the profile;
- an unsupported scalar was neither dropped nor excluded;
- an arc kind still connects multiple endpoint pairs after decisions;
- an arc references an absent endpoint without an exclude decision;
- the assembled graph fails `catalog.Validate` (for example a unique
  rule collides, a required property is absent, a cost is not a positive
  integer).

## Decisions document

```json
{
  "version": 1,
  "profile_digest": "...",
  "graph_name": "my-graph",
  "actions": [
    {"op": "split_vertex_property", "entity_kind": "database",
     "property": "port", "observed_kind": "int", "suffix": "_int"},
    {"op": "split_arc_kind", "arc_kind": "depends_on",
     "from": "service", "to": "database", "new_name": "depends_on_service_database"},
    {"op": "rename_vertex", "scope_ref": "exports/part1.json#4", "new_name": "bad_id"},
    {"op": "drop_property", "scope_ref": "exports/part1.json#4", "property": "ratio"},
    {"op": "explicit_cast", "entity_kind": "database", "property": "port",
     "new_name": "int", "owner_note": "owner-approved ticket SCHEMA-42"}
  ],
  "required_vertex_properties": {"service": ["name"]},
  "unique_vertex_properties": {"service": ["name"]}
}
```

`scope_ref` is always `file#ordinal`, so every record-level action is
traceable to the exact export row. `build-report.json` lists every
included count, exclusion, dropped value and rename.

## Untrusted inputs

Treat export directories as untrusted data: keep them separate from
this repository and from the output directory, and pass paths as
arguments. The decoder uses strict mode (one JSON object, no unknown
record fields) and never imports from the input directory.
