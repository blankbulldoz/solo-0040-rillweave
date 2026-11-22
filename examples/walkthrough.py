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


def commit(base, revision, edits, status=200, code=None, graph=GRAPH):
    return request(base, graph + "/commit", "POST", {"base": revision, "edits": edits}, status, code)


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


def expect_route(base):
    dep_schema = {
        "vertices": {"component": {}},
        "arcs": {kind: {"from": "component", "to": "component"}
                 for kind in ("upstream", "handoff", "owned")},
    }
    fork_schema = {
        "vertices": {"component": {}},
        "arcs": {kind: {"from": "component", "to": "component"}
                 for kind in ("alpha", "beta", "gamma", "delta")},
    }

    def dep(key, source, target, cost, kind):
        return {"op": "put_arc", "arc": {
            "id": key, "kind": kind, "from": source, "to": target, "cost": cost,
        }}

    def component(key):
        return {"op": "put_vertex", "vertex": {"id": key, "kind": "component"}}

    def route(graph, query, status=200, code=None, view=""):
        return request(base, graph + "/route" + view, "POST", query, status, code)

    # looplane: upstream cycles guard the only handoff arc. Vertex-level
    # detours that violate the stage pattern (o9 from the start, u8+o4 after
    # the handoff) are cheaper than the answer and must not be reported.
    loop = GRAPHS + "/looplane"
    request(base, GRAPHS, "POST", {"name": "looplane", "schema": dep_schema}, 201)
    commit(base, 1, [
        dep("u1", "entry", "a1", 1, "upstream"), dep("u2", "a1", "a2", 1, "upstream"),
        dep("u3", "a2", "entry", 1, "upstream"), dep("u4", "entry", "mid", 1, "upstream"),
        dep("u5", "a2", "a1", 1, "upstream"), dep("u6", "a1", "a1", 1, "upstream"),
        dep("u8", "b1", "b2", 1, "upstream"), dep("h1", "entry", "b1", 5, "handoff"),
        dep("o1", "b1", "exit", 8, "owned"), dep("o2", "exit", "b1", 1, "owned"),
        dep("o4", "b2", "exit", 1, "owned"), dep("o9", "entry", "exit", 1, "owned"),
    ] + [component(v) for v in ("entry", "a1", "a2", "mid", "b1", "b2", "exit")], graph=loop)
    stages = {"seq": [
        {"plus": {"kinds": ["upstream"]}},
        {"kinds": ["handoff"]},
        {"plus": {"kinds": ["owned"]}},
    ]}
    q = {"from": "entry", "to": "exit", "max_hops": 5, "pattern": stages}
    value = route(loop, q)["result"]
    assert value["found"] and value["cost"] == 16, value
    assert value["arcs"] == ["u1", "u2", "u3", "h1", "o1"], value
    assert value["vertices"] == ["entry", "a1", "a2", "entry", "b1", "exit"], value
    assert not route(loop, {**q, "max_hops": 4})["result"]["found"]
    route(loop, {**q, "budget": 2}, 422, "budget_exhausted")
    mirror = {"seq": [
        {"plus": {"kinds": ["owned"]}},
        {"kinds": ["handoff"]},
        {"plus": {"kinds": ["upstream"]}},
    ]}
    back = route(loop, {"from": "exit", "to": "entry", "max_hops": 5,
                        "direction": "in", "pattern": mirror})["result"]
    assert back["arcs"] == ["o1", "h1", "u3", "u2", "u1"] and back["cost"] == 16, back
    cycle = route(loop, {"from": "entry", "to": "entry", "max_hops": 5,
                         "pattern": {"plus": {"kinds": ["upstream"]}}})["result"]
    assert cycle["arcs"] == ["u1", "u2", "u3"] and cycle["cost"] == 3, cycle
    empty = route(loop, {"from": "entry", "to": "entry", "max_hops": 5,
                         "pattern": {"star": {"kinds": ["upstream"]}}})["result"]
    assert empty["found"] and empty["cost"] == 0 and empty["arcs"] == [], empty
    route(loop, {**q, "pattern": {}}, 400, "invalid")
    route(loop, {**q, "pattern": {"kinds": ["ghost"]}}, 400, "invalid")

    # forkmill: many alternation branches. Only branch shapes the pattern
    # admits may be reported, and only after the search is truly exhaustive.
    fork = GRAPHS + "/forkmill"
    request(base, GRAPHS, "POST", {"name": "forkmill", "schema": fork_schema}, 201)
    commit(base, 1, [
        dep("a1", "root", "x1", 1, "alpha"), dep("b1", "x1", "x2", 1, "beta"),
        dep("b2", "x2", "goal", 1, "beta"), dep("a2", "root", "x3", 1, "alpha"),
        dep("b3", "x3", "x4", 1, "beta"), dep("a3", "root", "y1", 1, "alpha"),
        dep("b4", "y1", "y2", 1, "beta"), dep("b5", "y2", "y1", 1, "beta"),
        dep("g1", "root", "z1", 1, "gamma"), dep("g2", "z1", "goal", 1, "gamma"),
        dep("g3", "root", "z2", 1, "gamma"), dep("g4", "z2", "z3", 1, "gamma"),
        dep("g5", "z3", "goal", 1, "gamma"), dep("d1", "root", "goal", 5, "delta"),
        dep("d2", "root", "w1", 1, "delta"), dep("d3", "w1", "w2", 1, "delta"),
        dep("d4", "w2", "goal", 1, "delta"),
    ] + [component(v) for v in (
        "root", "x1", "x2", "x3", "x4", "y1", "y2", "z1", "z2", "z3", "w1", "w2", "goal")],
        graph=fork)
    branches = {"alt": [
        {"seq": [{"kinds": ["alpha"]}, {"plus": {"kinds": ["beta"]}}]},
        {"seq": [{"kinds": ["gamma"]}, {"kinds": ["gamma"]}]},
        {"plus": {"kinds": ["delta"]}},
    ]}
    q = {"from": "root", "to": "goal", "max_hops": 4, "pattern": branches}
    value = route(fork, q)["result"]
    assert value["found"] and value["cost"] == 2 and value["arcs"] == ["g1", "g2"], value
    short = route(fork, {**q, "max_hops": 1})["result"]
    assert short["arcs"] == ["d1"] and short["cost"] == 5, short
    pair = {"seq": [{"kinds": ["gamma"]}, {"kinds": ["gamma"]}]}
    two = route(fork, {"from": "root", "to": "goal", "max_hops": 4, "pattern": pair})["result"]
    assert two["arcs"] == ["g1", "g2"], two  # the three-gamma detour is not a match
    qz = {"from": "root", "max_hops": 4, "pattern": pair}
    assert not route(fork, {**qz, "to": "z2"})["result"]["found"]
    assert route(fork, {**qz, "to": "z3"})["result"]["arcs"] == ["g3", "g4"]
    route(fork, {**q, "budget": 3}, 422, "budget_exhausted")
    pin = hold(base, fork)
    no_delta = {"vertices": fork_schema["vertices"],
                "arcs": {k: v for k, v in fork_schema["arcs"].items() if k != "delta"}}
    commit(base, 2, [{"op": "drop_arc", "id": key} for key in ("d1", "d2", "d3", "d4")]
           + [{"op": "set_schema", "schema": no_delta}], graph=fork)
    route(fork, q, 400, "invalid")  # the head catalog no longer declares delta
    pinned = route(fork, q, view="?view=" + pin)["result"]
    assert pinned["arcs"] == ["g1", "g2"], pinned  # the pinned revision still does
    return "stage-aware revisits, kind traps, branch choice and per-revision catalogs verified"


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
    "path": expect_path, "route": expect_route, "components": expect_components, "pin": expect_pin,
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
