"""Deterministically generate the bulky v1 migration samples.

04-small-ints-bulk: 2000 put_vertex edits (the commit edit cap) rewriting
eight vertices with 32 small-integer properties each. That is the largest
commit body small-integer writes can reach, while the final graph stays at
eight vertices.

05-overflow-text: a v1 commit body just under the 8 MiB request limit whose
typed-envelope conversion exceeds it, so the migration tool must flag the
overflow and keep the v1 replay path. The final graph is again tiny.

Both are raw /rill/v1 request bodies, replayable as-is once the namespace
from the matching create body exists.
"""

import argparse
import json
from pathlib import Path

LIMIT = 8 << 20  # wire.MaxRequest
EDITS = 2000     # change.MaxEdits
VERTICES = 8
PROPS = 32       # graph.MaxProps
TEXT_PROPS = ["ta", "tb", "tc", "td"]

# The v2 envelope adds {"text":...} / {"int":...} wrappers and quotes the
# base, while dropping the "op"/"vertex" keys. The generator computes the
# exact converted body to prove sample 05 crosses the limit.
V2_TARGET = LIMIT + 20_000


def pad(name):
    assert 0 < len(name) <= 64
    return name + "x" * (64 - len(name))


def dump(doc):
    return (json.dumps(doc, separators=(",", ":")) + "\n").encode()


def wrap_scalar(value):
    if isinstance(value, bool):
        return {"bool": value}
    if isinstance(value, int):
        return {"int": str(value)}
    return {"text": value}


def to_v2(commit):
    """Mirror of the rillmigrate conversion, for size verification only."""
    edits = []
    for edit in commit["edits"]:
        vertex = edit["vertex"]
        edits.append({"put_vertex": {
            "id": vertex["id"], "kind": vertex["kind"],
            "properties": {k: wrap_scalar(v) for k, v in vertex["properties"].items()},
        }})
    return {"base": str(commit["base"]), "edits": edits}


def small_ints():
    kind = pad("counter")
    props = [pad(f"metric{i:02d}") for i in range(PROPS)]
    ids = [pad(f"p{n}") for n in range(VERTICES)]
    create = {"name": "mig04", "schema": {"vertices": {
        kind: {"properties": {p: {"kind": "int"} for p in props}},
    }}}
    edits = []
    for e in range(EDITS):
        values = {p: (e + i) % 10 for i, p in enumerate(props)}
        edits.append({"op": "put_vertex", "vertex": {
            "id": ids[e % VERTICES], "kind": kind, "properties": values,
        }})
    return create, {"base": 1, "edits": edits}


def overflow_text():
    create = {"name": "mig05", "schema": {"vertices": {
        "blob": {"properties": {p: {"kind": "text"} for p in TEXT_PROPS}},
    }}}
    ids = [f"p{c}" for c in "abcdefgh"]

    def build(fill, bumped):
        edits = []
        for e in range(EDITS):
            values = {p: "x" * fill for p in TEXT_PROPS}
            if e < bumped:
                values["ta"] += "x"
            edits.append({"op": "put_vertex", "vertex": {
                "id": ids[e % VERTICES], "kind": "blob", "properties": values,
            }})
        return {"base": 1, "edits": edits}

    per_edit_props = EDITS * len(TEXT_PROPS)
    fixed = len(dump(build(0, 0)))
    fill = (V2_TARGET - 44_000 - fixed) // per_edit_props
    bumped = V2_TARGET - 44_000 - fixed - fill * per_edit_props
    assert 0 <= bumped < per_edit_props and fill + 1 <= 1024
    return create, build(fill, bumped)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, type=Path, help="output directory")
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    for stem, build in [("04-small-ints-bulk", small_ints), ("05-overflow-text", overflow_text)]:
        create, commit = build()
        raw = dump(commit)
        converted = dump(to_v2(commit))
        assert len(commit["edits"]) == EDITS
        assert len(raw) <= LIMIT, (stem, len(raw))
        if stem.startswith("04"):
            assert len(converted) <= LIMIT, (stem, len(converted))
        else:
            assert len(converted) > LIMIT, (stem, len(converted))
        (args.out / f"{stem}.create.json").write_bytes(dump(create))
        (args.out / f"{stem}.commit.json").write_bytes(raw)
        print(f"{stem}: {EDITS} edits, v1 {len(raw)} bytes, "
              f"v2 envelope {len(converted)} bytes, request limit {LIMIT} bytes")


if __name__ == "__main__":
    main()
