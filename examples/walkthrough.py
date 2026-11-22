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


def expect_via(base):
    ramp, chain = GRAPHS + "/ramp", GRAPHS + "/chain"
    for name in ("ramp", "chain"):
        request(base, GRAPHS, "POST", {"name": name, "schema": SCHEMA}, 201)
    ramp_arcs = [
        ("sx", "s", "x", 1), ("xy", "x", "y", 1), ("ym", "y", "m", 1),
        ("sm", "s", "m", 10), ("sn", "s", "n", 1), ("mn", "m", "n", 1),
        ("nm", "n", "m", 2), ("np", "n", "p", 1), ("pt", "p", "t", 1),
        ("nt", "n", "t", 100),
    ]
    edits = [put_arc(*a) for a in ramp_arcs]
    edits += [put_vertex(v) for v in ("s", "x", "y", "m", "n", "p", "t")]
    request(base, ramp + "/commit", "POST", {"base": 1, "edits": edits})
    edits = []
    for i in range(19):
        for k in (1, 2, 3):
            if i + k <= 19:
                edits.append(put_arc(f"e{i:02d}{k}", f"f{i:02d}", f"f{i + k:02d}", (i * 3 + k) % 5 + 1))
    edits += [put_vertex(f"f{i:02d}") for i in range(20)]
    request(base, chain + "/commit", "POST", {"base": 1, "edits": edits})

    budget = 100  # one fixed candidate limit for every query below

    def route(graph, q, status=200, code=None):
        return request(base, graph + "/path", "POST", {**q, "budget": budget}, status, code)

    # Cheapest segments taken greedily cost 3+1+100 over 4 hops; the joint
    # search pays 10 for a one-hop first segment and fits the chain in 4 hops.
    first = route(ramp, {"from": "s", "to": "m", "max_hops": 4})["result"]
    assert first["cost"] == 3 and first["arcs"] == ["sn", "nm"]
    assert route(ramp, {"from": "m", "to": "n", "max_hops": 2})["result"]["cost"] == 1
    assert route(ramp, {"from": "n", "to": "t", "max_hops": 1})["result"]["cost"] == 100
    joint = route(ramp, {"from": "s", "to": "t", "via": ["m", "n"], "max_hops": 4})["result"]
    assert joint["cost"] == 13 and joint["arcs"] == ["sm", "mn", "np", "pt"]
    assert joint["via_hops"] == [1, 2]
    tight = route(ramp, {"from": "s", "to": "t", "via": ["m", "n"], "max_hops": 3})["result"]
    assert tight["cost"] == 111 and tight["arcs"] == ["sm", "mn", "nt"]
    assert not route(ramp, {"from": "n", "to": "t", "max_hops": 0})["result"]["found"]

    # An early pass satisfies no later stage; a repeated waypoint needs a
    # distinct later visit; the start position counts as the first visit.
    early = route(ramp, {"from": "s", "to": "t", "via": ["m", "n"], "max_hops": 6})["result"]
    assert early["vertices"] == ["s", "n", "m", "n", "p", "t"] and early["via_hops"] == [2, 3]
    twice = route(ramp, {"from": "s", "to": "t", "via": ["m", "m"], "max_hops": 8})["result"]
    assert twice["cost"] == 9 and twice["via_hops"] == [2, 4]
    assert twice["arcs"] == ["sn", "nm", "mn", "nm", "mn", "np", "pt"]
    assert route(ramp, {"from": "s", "to": "t", "via": ["m", "m"], "max_hops": 6})["result"]["cost"] == 16
    assert not route(ramp, {"from": "s", "to": "t", "via": ["m", "m"], "max_hops": 4})["result"]["found"]
    loop = request(base, GRAPH + "/path", "POST", {"from": "hinge", "to": "spar",
                   "via": ["hinge", "hinge"], "max_hops": 3, "budget": budget})["result"]
    assert loop["cost"] == 3 and loop["arcs"] == ["loop", "bypass", "cross"]
    assert loop["via_hops"] == [0, 1]

    many = ["s", "x", "y", "m", "n", "p", "t", "s", "x"]
    route(ramp, {"from": "s", "to": "t", "via": many, "max_hops": 32}, 400, "invalid")
    route(ramp, {"from": "s", "to": "t", "via": ["m", "ghost"], "max_hops": 8}, 404, "not_found")

    # The fixed limit proves every dense ramp query; the far-apart chain only
    # becomes conclusive once the candidate limit is raised.
    far_q = {"from": "f00", "to": "f19", "via": ["f05", "f10", "f15"], "max_hops": 24}
    route(chain, far_q, 422, "budget_exhausted")
    far = request(base, chain + "/path", "POST", far_q)["result"]
    assert far["cost"] == 19 and far["via_hops"] == [3, 6, 9] and far["scanned"] == 543
    return ("joint 4-hop chain costs 13 against greedy 104; early pass, repeated waypoint and "
            "self-loop replay from via_hops; dense queries proven under 100 candidates, "
            "far chain inconclusive at 100 and proven at 543")


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
    "path": expect_path, "via": expect_via, "components": expect_components,
    "pin": expect_pin, "compare": expect_compare, "backup": expect_backup,
    "restart": expect_restart,
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
