"""Exercise the HTTP service against disposable data directories."""

import argparse
from contextlib import contextmanager
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


def expect_inventory(base):
    schema = {
        "vertices": {"post": {"properties": {"zone": {"kind": "text"}}}},
        "arcs": {
            "link": {"from": "post", "to": "post"},
            "guide": {"from": "post", "to": "post"},
        },
    }
    yard = GRAPHS + "/stockyard"
    request(base, GRAPHS, "POST", {"name": "stockyard", "schema": schema}, 201)
    edits = [put_arc("forward", "p-a", "p-b", 1, "link")]
    edits += [
        put_arc("backward", "p-b", "p-a", 1, "link"),
        put_arc("wrong-kind", "p-c", "p-c", 1, "guide"),
    ]
    for key, zone in (("p-a", "z1"), ("p-b", "z1"), ("p-c", "z1"), ("p-d", "z2")):
        edits.append({"op": "put_vertex", "vertex": {
            "id": key, "kind": "post", "properties": {"zone": zone},
        }})
    request(base, yard + "/commit", "POST", {"base": 1, "edits": edits})
    query = {"from_kind": "post", "to_kind": "post", "property": "zone",
             "arc_kind": "link"}
    path = yard + "/inventory"

    first = request(base, path, "POST", {**query, "limit": 2})["result"]
    assert first["complete"] is False and first["truncated"] is True
    assert [(row["from"]["id"], row["to"]["id"]) for row in first["gaps"]] == [
        ("p-a", "p-a"), ("p-a", "p-c"),
    ]
    # Each gap points back at both original records with their properties.
    assert first["gaps"][0]["from"]["properties"] == {"zone": "z1"}
    assert first["scanned"] >= 2 and first["matched"] >= 2

    pages = [first]
    while not pages[-1]["complete"]:
        page = request(base, path, "POST", {**query, "limit": 2,
                                            "cursor": pages[-1]["cursor"]})["result"]
        if not page["complete"]:
            assert page["gaps"], "a non-final page must continue the boundary"
        pages.append(page)
    pairs = [(row["from"]["id"], row["to"]["id"])
             for page in pages for row in page["gaps"]]
    assert len(pairs) == len(set(pairs)), "cursor pagination must not repeat pairs"
    assert pairs == [
        ("p-a", "p-a"), ("p-a", "p-c"),
        ("p-b", "p-b"), ("p-b", "p-c"),
        ("p-c", "p-a"), ("p-c", "p-b"), ("p-c", "p-c"),
        ("p-d", "p-d"),
    ]
    # Forward link covers (p-a,p-b); the reverse link only covers its own
    # (p-b,p-a) pair and never closes the forward gap. The guide-kind self arc
    # cannot cover the link requirement either, so (p-c,p-c) stays a gap.
    assert ("p-a", "p-b") not in pairs and ("p-b", "p-a") not in pairs
    assert ("p-c", "p-c") in pairs
    # Values never cross cohorts: the z2 vertex only pairs with itself.
    assert not any(("p-d" in pair) and pair != ("p-d", "p-d") for pair in pairs)

    # A scan stopped by the budget keeps a boundary and resumes without losing
    # or replaying pairs: replaying every page under a tight budget yields the
    # same total set as the limit-driven walk above.
    budgeted = []
    cursor = ""
    while True:
        page = request(base, path, "POST", {**query, "budget": 3,
                                            "cursor": cursor})["result"]
        budgeted.extend((r["from"]["id"], r["to"]["id"]) for r in page["gaps"])
        if page["complete"]:
            break
        assert page["scanned"] == 3 and page["cursor"]
        cursor = page["cursor"]
    assert budgeted == pairs

    request(base, path, "POST", {**query, "from_kind": "ghost"}, 400, "invalid")
    request(base, path, "POST", {**query, "arc_kind": "missing-kind"}, 400, "invalid")
    request(base, path, "POST", {**query, "value": True}, 400, "invalid")
    request(base, path, "POST", {**query, "cursor": "not-base64-!!!"}, 400, "invalid")

    # Cursors are bound to the revision that issued them: a head move rejects
    # the old boundary instead of silently scanning a shifted pair space.
    boundary = request(base, path, "POST", {**query, "limit": 1})["result"]["cursor"]
    request(base, yard + "/commit", "POST", {"base": 2, "edits": [
        {"op": "put_vertex", "vertex": {"id": "p-e", "kind": "post",
                                        "properties": {"zone": "z1"}}},
    ]})
    request(base, path, "POST", {**query, "cursor": boundary}, 409, "conflict")

    # Inventory reads the pinned revision: an arc added at the head does not
    # close a gap visible through an older view.
    pin = hold(base, yard)
    request(base, yard + "/commit", "POST", {"base": 3, "edits": [
        put_arc("close-self", "p-a", "p-a", 1, "link"),
    ]})
    pinned = request(base, path + "?view=" + pin, "POST", query)["result"]
    assert ("p-a", "p-a") in [(r["from"]["id"], r["to"]["id"]) for r in pinned["gaps"]]
    head = request(base, path, "POST", query)["result"]
    assert ("p-a", "p-a") not in [(r["from"]["id"], r["to"]["id"]) for r in head["gaps"]]
    return "same-value pairs audited; reverse/other-kind arcs and unpaired values excluded; cursor pages never claim a false empty finish"


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
    "path": expect_path, "components": expect_components, "inventory": expect_inventory,
    "pin": expect_pin,
    "compare": expect_compare, "backup": expect_backup, "restart": expect_restart,
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workflow", choices=["all", *WORKFLOWS], default="all")
    choice = parser.parse_args().workflow
    with tempfile.TemporaryDirectory(prefix="rillweave-") as temp:
        tmp = Path(temp)
        binary = tmp / "rillweaved"
        subprocess.run(["go", "build", "-o", binary, "./cmd/rillweaved"], cwd=ROOT, check=True, timeout=90)
        for name in WORKFLOWS if choice == "all" else [choice]:
            data = tmp / name
            if name == "restart":
                output = expect_restart(binary, data)
            else:
                with serve(binary, data) as base:
                    seed(base)
                    output = WORKFLOWS[name](base)
            print(f"{name}: {output}")


if __name__ == "__main__":
    main()
