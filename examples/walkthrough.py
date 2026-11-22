"""Exercise the HTTP service against disposable data directories."""

import argparse
import base64
from contextlib import contextmanager
import hashlib
import json
from pathlib import Path
import selectors
import subprocess
import tempfile
from urllib.error import HTTPError
from urllib.request import Request, urlopen


ROOT = Path(__file__).resolve().parents[1]
GRAPHS = "/rill/v1/graphs"
GRAPH = GRAPHS + "/abutment"
SCHEMA = {
    "vertices": {"strut": {"properties": {"tag": {"kind": "text", "unique": True}}}},
    "arcs": {
        "join": {"from": "strut", "to": "strut"},
        "guide": {"from": "strut", "to": "strut"},
    },
}


def command(binary, data):
    return [str(binary), "-listen", "127.0.0.1:0", "-data", str(data)]


@contextmanager
def serve(binary, data):
    proc = subprocess.Popen(command(binary, data), stdout=subprocess.PIPE, text=True)
    try:
        with selectors.DefaultSelector() as monitor:
            monitor.register(proc.stdout, selectors.EVENT_READ)
            if not monitor.select(timeout=10):
                raise RuntimeError("server did not announce its bind address")
            startup = proc.stdout.readline().strip().split()
        if len(startup) != 2 or startup[0] != "listening":
            raise RuntimeError(f"unexpected startup: {startup!r}")
        yield "http://" + startup[1]
    finally:
        if proc.poll() is None:
            proc.terminate()
        try:
            proc.wait(timeout=6)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
            raise
        finally:
            proc.stdout.close()


def request(base, path, verb="GET", body=None, status=200, code=None):
    data = json.dumps(body).encode() if body is not None else None
    req = Request(base + path, data, {"Content-Type": "application/json"}, method=verb)
    try:
        with urlopen(req, timeout=5) as response:
            actual, payload = response.status, response.read()
    except HTTPError as response:
        with response:
            actual, payload = response.code, response.read()
    value = json.loads(payload) if payload else None
    assert actual == status, (verb, path, actual, status, value)
    if code is not None:
        assert value["code"] == code, value
    return value


def put_vertex(key, tag=None):
    props = {"tag": tag} if tag is not None else {}
    return {"op": "put_vertex", "vertex": {"id": key, "kind": "strut", "properties": props}}


def put_arc(key, source, target, cost, kind="join"):
    return {"op": "put_arc", "arc": {
        "id": key, "kind": kind, "from": source, "to": target, "cost": cost,
    }}


def commit(base, revision, edits, status=200, code=None):
    return request(base, GRAPH + "/commit", "POST", {"base": revision, "edits": edits}, status, code)


def catalog(base, view=""):
    return request(base, GRAPH + "/catalog" + view)


def seed(base):
    request(base, "/ready")
    created = request(base, GRAPHS, "POST", {"name": "abutment", "schema": SCHEMA}, 201)
    assert created["revision"] == 1
    result = commit(base, 1, [
        put_arc("brace", "hinge", "spar", 8), put_arc("brace-alt", "hinge", "spar", 8),
        put_arc("bypass", "hinge", "keel", 1), put_arc("cross", "keel", "spar", 1),
        put_arc("stem", "spar", "tip", 1), put_arc("return", "spar", "hinge", 2),
        put_arc("loop", "hinge", "hinge", 1), put_arc("track", "tip", "hinge", 1, "guide"),
        put_vertex("hinge", "north"), put_vertex("keel"), put_vertex("spar", "south"), put_vertex("tip"),
    ])
    assert result == {"name": "abutment", "revision": 2, "vertices": 4, "arcs": 8}


def hold(base, graph=GRAPH):
    return request(base, graph + "/pins", "POST", status=201)["handle"]


def expect_catalog(base):
    expanded = {**SCHEMA, "vertices": {"strut": {"properties": {
        **SCHEMA["vertices"]["strut"]["properties"], "marked": {"kind": "bool"},
    }}}}
    required = {**SCHEMA, "vertices": {"strut": {"properties": {
        "tag": {"kind": "text", "unique": True, "required": True},
    }}}}
    commit(base, 2, [{"op": "set_schema", "schema": required}], 400, "invalid")
    assert catalog(base)["result"]["schema"] == SCHEMA
    commit(base, 2, [{"op": "set_schema", "schema": expanded}])
    value = catalog(base)
    assert value["revision"] == 3 and value["result"]["schema"] == expanded
    return "incompatible schema rejected; compatible schema installed at revision 3"


def expect_commit(base):
    edits = [put_vertex("hinge", "south"), put_vertex("spar", "north")]
    commit(base, 2, edits[:1], 409, "conflict")
    commit(base, 2, edits)
    commit(base, 2, edits, 409, "conflict")
    commit(base, 3, [{"op": "drop_vertex", "id": "tip"}], 409, "conflict")
    assert catalog(base)["revision"] == 3
    assert request(base, GRAPH + "/vertices/hinge")["result"]["properties"]["tag"] == "south"
    return "unique-value swap committed; stale base and dangling arc blocked"


def expect_vertex(base):
    pin = hold(base)
    commit(base, 2, [put_vertex("wing", "west")])
    value = request(base, GRAPH + "/vertices/wing")
    assert value["revision"] == 3 and value["result"]["id"] == "wing"
    request(base, GRAPH + "/vertices/wing?view=" + pin, status=404, code="not_found")
    return "new vertex visible only in the later revision"


def expect_lookup(base):
    q = {"kind": "strut", "property": "tag", "value": "north"}
    path = GRAPH + "/lookup"
    assert request(base, path, "POST", q)["result"]["ids"] == ["hinge"]
    assert request(base, path, "POST", {**q, "value": ""})["result"]["matched"] == 0
    request(base, path, "POST", {**q, "value": False}, 400, "invalid")
    return "typed posting correct; absent tag is not empty text"


def expect_adjacency(base):
    q = {"vertex": "hinge", "direction": "both", "kinds": ["join"]}
    path = GRAPH + "/adjacency"
    value = request(base, path, "POST", {**q, "limit": 4})["result"]
    assert [a["id"] for a in value["arcs"]] == ["brace", "brace-alt", "bypass", "loop"]
    assert value["truncated"] and value["matched"] == 5 and value["scanned"] == 6
    request(base, path, "POST", {**q, "kinds": ["unknown"]}, 400, "invalid")
    return "parallel arcs retained; loop counted once; type filter and window respected"


def expect_reach(base):
    q = {"root": "hinge", "max_hops": 2, "kinds": ["join"]}
    path = GRAPH + "/reach"
    value = request(base, path, "POST", q)["result"]
    assert {row["vertex"]: row["hops"] for row in value["distances"]} == {
        "hinge": 0, "keel": 1, "spar": 1, "tip": 2,
    }
    request(base, path, "POST", {**q, "budget": 2}, 422, "budget_exhausted")
    return "minimum hop distances returned; exhausted budget emits no partial result"


def expect_path(base):
    q = {"from": "hinge", "to": "tip", "max_hops": 2, "kinds": ["join"]}
    path = GRAPH + "/path"
    bounded = request(base, path, "POST", q)["result"]
    assert bounded["cost"] == 9 and bounded["arcs"] == ["brace", "stem"]
    longer = request(base, path, "POST", {**q, "max_hops": 3})["result"]
    assert longer["cost"] == 3 and longer["arcs"] == ["bypass", "cross", "stem"]
    reverse = request(base, path, "POST", {**q, "from": "tip", "to": "hinge", "direction": "in"})
    assert reverse["result"]["vertices"] == ["tip", "spar", "hinge"]
    assert not request(base, path, "POST", {**q, "max_hops": 1})["result"]["found"]
    request(base, path, "POST", {**q, "budget": 1}, 422, "budget_exhausted")
    return "hop-sensitive prefixes, parallel ties, reverse witness and budget verified"


def expect_components(base):
    path = GRAPH + "/components"
    value = request(base, path, "POST", {"kinds": ["join"]})["result"]
    assert value["groups"] == [
        {"id": "hinge", "vertices": ["hinge", "keel", "spar"]},
        {"id": "tip", "vertices": ["tip"]},
    ]
    assert value["links"] == [{"from": "hinge", "to": "tip"}]
    assert len(request(base, path, "POST", {})["result"]["groups"]) == 1
    request(base, path, "POST", {"budget": 1}, 422, "budget_exhausted")
    return "selected kind changes strong connectivity; directed quotient link verified"


def expect_pin(base):
    pin = hold(base)
    query = "?view=" + pin
    commit(base, 2, [put_vertex("wing", "west")])
    assert catalog(base, query)["revision"] == 2 and catalog(base)["revision"] == 3
    request(base, GRAPH + "/pins/" + pin, "DELETE", status=204)
    request(base, GRAPH + "/catalog" + query, status=404, code="not_found")
    return "head advances independently; released pin no longer resolves"


def expect_compare(base):
    pin = hold(base)
    commit(base, 2, [put_vertex("keel", "mid"), {"op": "drop_arc", "id": "brace-alt"}])
    value = request(base, GRAPH + "/compare", "POST", {"from": pin})
    assert (value["from"], value["to"]) == (2, 3) and not value["schema_changed"]
    assert value["vertices"]["updated"] == ["keel"] and value["arcs"]["removed"] == ["brace-alt"]
    request(base, GRAPHS, "POST", {"name": "pier", "schema": SCHEMA}, 201)
    request(base, GRAPH + "/compare", "POST", {"from": hold(base, GRAPHS + "/pier")}, 404, "not_found")
    return "logical changes have correct identity and direction; foreign handle rejected"


def expect_backup(base):
    pin = hold(base)
    commit(base, 2, [put_vertex("wing")])
    envelope = request(base, GRAPH + "/image?view=" + pin)
    path = GRAPHS + "/pier/image"
    request(base, path, "PUT", {**envelope, "sha256": "0" * 64}, 400, "invalid")
    receipt = request(base, path, "PUT", envelope, 201)
    assert receipt["source"]["revision"] == 2 and receipt["installed"]["revision"] == 1
    assert receipt["installed"]["vertices"] == 4 and catalog(base)["revision"] == 3
    request(base, path, "PUT", envelope, 409, "conflict")
    return "pinned image restored under new identity; damaged image and overwrite rejected"


def renamed_envelope(envelope, renames):
    """Return an image envelope whose payload renames vertices and arcs.

    The new envelope has a different digest because the bytes changed; the
    collation workflow must never treat a digest as an equivalence premise.
    Graph payloads encode vertices and arcs as keyed maps.
    """
    graph_doc = json.loads(base64.b64decode(envelope["payload"]))
    vertices = {}
    for old_id, vertex in graph_doc["vertices"].items():
        new_id = renames.get(old_id, old_id)
        vertices[new_id] = {**vertex, "id": new_id}
    arcs = {}
    for old_id, arc in graph_doc["arcs"].items():
        new_id = renames.get(old_id, old_id)
        arcs[new_id] = {
            **arc, "id": new_id,
            "from": renames.get(arc["from"], arc["from"]),
            "to": renames.get(arc["to"], arc["to"]),
        }
    graph_doc["vertices"], graph_doc["arcs"] = vertices, arcs
    return encode_envelope(graph_doc, envelope["format"])


def envelope_graph(envelope):
    return json.loads(base64.b64decode(envelope["payload"]))


def encode_envelope(doc, fmt=1):
    payload = json.JSONEncoder(separators=(",", ":")).encode(doc).encode()
    return {
        "format": fmt,
        "sha256": hashlib.sha256(payload).hexdigest(),
        "payload": base64.b64encode(payload).decode(),
    }


def write_envelope(folder, name, envelope):
    path = folder / name
    path.write_text(json.dumps(envelope), encoding="utf-8")
    return path


def run_collate(collate_binary, left_path, right_path, anchors="", budget=None):
    cmd = [str(collate_binary)]
    if anchors:
        cmd += ["--anchors", anchors]
    if budget is not None:
        cmd += ["--budget", str(budget)]
    cmd += [str(left_path), str(right_path)]
    run = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    return run.returncode, json.loads(run.stdout)


def expect_collate(base, tmp):
    collate_binary = tmp / "rillweave-collate"
    subprocess.run(
        ["go", "build", "-o", collate_binary, "./cmd/rillweave-collate"],
        cwd=ROOT, check=True, timeout=90,
    )
    folder = tmp / "collation"
    folder.mkdir()

    # A renamed copy under an unused namespace: renamed vertices and arcs, a new
    # local revision and necessarily a different digest, but the same structure.
    original = request(base, GRAPH + "/image")
    renames = {
        "hinge": "pylon-a", "keel": "pylon-b", "spar": "pylon-c", "tip": "pylon-d",
        "brace": "edge-1", "brace-alt": "edge-2", "bypass": "edge-3", "cross": "edge-4",
        "stem": "edge-5", "return": "edge-6", "loop": "edge-7", "track": "edge-8",
    }
    renamed = renamed_envelope(original, renames)
    assert renamed["sha256"] != original["sha256"]
    left_path = write_envelope(folder, "left.json", original)
    right_path = write_envelope(folder, "right.json", renamed)
    code, verdict = run_collate(collate_binary, left_path, right_path)
    assert code == 0 and verdict["status"] == "equivalent", verdict
    assert verdict["mapping"]["vertices"]["hinge"] == "pylon-a"
    assert verdict["mapping"]["vertices"]["tip"] == "pylon-d"
    assert verdict["mapping"]["arcs"]["brace-alt"] == "edge-2"
    assert verdict["mapping"]["arcs"]["loop"] == "edge-7"

    # The imported copy really installs and answers reads under its new name.
    receipt = request(base, GRAPHS + "/pier/image", "PUT", renamed, 201)
    assert receipt["source"]["revision"] == 2 and receipt["installed"]["revision"] == 1

    # A wrong anchor cannot force a mapping; a compatible anchor is accepted.
    _, bad_anchor = run_collate(collate_binary, left_path, right_path, "hinge=pylon-c")
    assert bad_anchor["status"] == "inequivalent", bad_anchor
    _, anchored = run_collate(collate_binary, left_path, right_path, "hinge=pylon-a")
    assert anchored["status"] == "equivalent", anchored

    # Changing one arc cost breaks equivalence with reviewable evidence, even
    # though every vertex keeps the same kind and properties.
    altered_doc = envelope_graph(renamed)
    for arc in altered_doc["arcs"].values():
        if arc["id"] == "edge-1":
            arc["cost"] = arc["cost"] + 5
    altered = encode_envelope(altered_doc)
    altered_path = write_envelope(folder, "altered.json", altered)
    code, verdict = run_collate(collate_binary, left_path, altered_path)
    assert code == 3 and verdict["status"] == "inequivalent", verdict
    assert verdict["reason"], "inequivalence must name an invariant-level reason"

    # A symmetric, locally indistinguishable pair can exhaust a deliberately
    # tiny budget; the verdict is undetermined rather than a false mismatch.
    ring_schema = {
        "vertices": {"strut": {"properties": {}}},
        "arcs": {"join": {"from": "strut", "to": "strut"}},
    }
    ring_doc = {
        "name": "ring", "revision": 1, "schema": ring_schema,
        "vertices": {f"v{i}": {"id": f"v{i}", "kind": "strut"} for i in range(6)},
        "arcs": {
            f"a{i}": {"id": f"a{i}", "kind": "join", "from": f"v{i}",
                      "to": f"v{(i + 1) % 6}", "cost": 1}
            for i in range(6)
        },
    }
    split_doc = {
        "name": "split", "revision": 1, "schema": ring_schema,
        "vertices": {f"v{i}": {"id": f"v{i}", "kind": "strut"} for i in range(6)},
        "arcs": {},
    }
    for seq, cycle in enumerate(([0, 1, 2], [3, 4, 5])):
        for i, source in enumerate(cycle):
            arc_id = f"b{seq}{i}"
            split_doc["arcs"][arc_id] = {
                "id": arc_id, "kind": "join", "from": f"v{source}",
                "to": f"v{cycle[(i + 1) % len(cycle)]}", "cost": 1,
            }
    # Raw graph documents (no envelope) are also accepted as inputs.
    ring_path = write_envelope(folder, "ring.json", ring_doc)
    split_path = write_envelope(folder, "split.json", split_doc)
    code, verdict = run_collate(collate_binary, ring_path, split_path, budget=1)
    assert code == 4 and verdict["status"] == "undetermined" and verdict["budget_exhausted"], verdict
    code, verdict = run_collate(collate_binary, ring_path, split_path)
    assert code == 3 and verdict["status"] == "inequivalent", verdict
    return "renamed image matched without trusting digests; anchors, cost evidence and budget verdict verified"


def blocked_start(binary, data, reason):
    result = subprocess.run(command(binary, data), capture_output=True, text=True, timeout=8)
    assert result.returncode != 0 and reason in result.stderr, result


def expect_restart(binary, data):
    with serve(binary, data) as base:
        seed(base)
        pin = hold(base)
        commit(base, 2, [put_vertex("keel", "mid")])
        blocked_start(binary, data, "directory lock")
    with serve(binary, data) as base:
        assert catalog(base)["revision"] == 3
        q = {"kind": "strut", "property": "tag", "value": "mid"}
        assert request(base, GRAPH + "/lookup", "POST", q)["result"]["ids"] == ["keel"]
        request(base, GRAPH + "/catalog?view=" + pin, status=404, code="not_found")
    (data / "abutment.rill").write_text('{"format":', encoding="ascii")
    blocked_start(binary, data, 'recover graph "abutment"')
    return "directory held exclusively; durable head and index recovered; corruption fails startup"


WORKFLOWS = {
    "catalog": expect_catalog, "commit": expect_commit, "vertex": expect_vertex,
    "lookup": expect_lookup, "adjacency": expect_adjacency, "reach": expect_reach,
    "path": expect_path, "components": expect_components, "pin": expect_pin,
    "compare": expect_compare, "backup": expect_backup, "restart": expect_restart,
}
# Workflows that need the shared temporary directory rather than only the URL.
NEEDS_TMP = {"collate": expect_collate}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workflow", choices=["all", *WORKFLOWS, *NEEDS_TMP], default="all")
    choice = parser.parse_args().workflow
    names = [*WORKFLOWS, *NEEDS_TMP] if choice == "all" else [choice]
    with tempfile.TemporaryDirectory(prefix="rillweave-") as temp:
        tmp = Path(temp)
        binary = tmp / "rillweaved"
        subprocess.run(["go", "build", "-o", binary, "./cmd/rillweaved"], cwd=ROOT, check=True, timeout=90)
        for name in names:
            data = tmp / name
            if name == "restart":
                output = expect_restart(binary, data)
            elif name in NEEDS_TMP:
                with serve(binary, data) as base:
                    seed(base)
                    output = NEEDS_TMP[name](base, tmp)
            else:
                with serve(binary, data) as base:
                    seed(base)
                    output = WORKFLOWS[name](base)
            print(f"{name}: {output}")


if __name__ == "__main__":
    main()
