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


ATLAS = GRAPHS + "/atlas"
ATLAS_SCHEMA = {
    "vertices": {
        "hub": {"properties": {"zone": {"kind": "text"}, "active": {"kind": "bool"}}},
        "leaf": {"properties": {"zone": {"kind": "text"}}},
    },
    "arcs": {
        "relay": {"from": "hub", "to": "hub"},
        "feed": {"from": "hub", "to": "leaf"},
    },
}


def hub_vertex(key, zone, active=None):
    props = {"zone": zone}
    if active is not None:
        props["active"] = active
    return {"op": "put_vertex", "vertex": {"id": key, "kind": "hub", "properties": props}}


def leaf_vertex(key, zone):
    return {"op": "put_vertex", "vertex": {"id": key, "kind": "leaf", "properties": {"zone": zone}}}


def typed_arc(key, kind, source, target, cost=1):
    return {"op": "put_arc", "arc": {"id": key, "kind": kind, "from": source, "to": target, "cost": cost}}


def seed_atlas(base):
    request(base, GRAPHS, "POST", {"name": "atlas", "schema": ATLAS_SCHEMA}, 201)
    edits = [
        hub_vertex("h0", "south"), hub_vertex("h1", "north"),
        hub_vertex("h2", "north"), hub_vertex("h3", "north", True),
        hub_vertex("h4", "south"), hub_vertex("h5", "south"), hub_vertex("h6", "south"),
        leaf_vertex("lf1", "north"), leaf_vertex("lf2", "south"),
        # Three parallel arcs h1 -> h2 and a self-loop on h1.
        typed_arc("p1", "relay", "h1", "h2"), typed_arc("p2", "relay", "h1", "h2"),
        typed_arc("p3", "relay", "h1", "h2"), typed_arc("lp1", "relay", "h1", "h1"),
        typed_arc("r1", "relay", "h2", "h3"),
        # One hub collaborating with three distinct neighbours.
        typed_arc("c1", "relay", "h3", "h4"), typed_arc("c2", "relay", "h3", "h5"),
        typed_arc("c3", "relay", "h3", "h6"),
        # A second arc kind that relay-only censuses must skip but still charge.
        typed_arc("f1", "feed", "h1", "lf1"), typed_arc("f2", "feed", "h3", "lf2"),
    ]
    request(base, ATLAS + "/commit", "POST", {"base": 1, "edits": edits})


def expect_census(base):
    seed_atlas(base)
    census_path = ATLAS + "/census"
    relay = {"kind": "hub", "arc_kinds": ["relay"]}

    full = request(base, census_path, "POST", relay)["result"]
    by_vertex = {row["vertex"]: row for row in full["rows"]}
    # Zero-degree vertex stays in the table and the population counts it once.
    assert full["cohort_size"] == 7 and len(full["rows"]) == 7
    assert by_vertex["h0"]["bucket"] == "0x0"
    assert (by_vertex["h1"]["in_degree"], by_vertex["h1"]["out_degree"]) == (1, 4)
    assert by_vertex["h1"]["bucket"] == "1x4"
    assert (by_vertex["h2"]["in_degree"], by_vertex["h2"]["out_degree"]) == (3, 1)
    assert (by_vertex["h3"]["in_degree"], by_vertex["h3"]["out_degree"]) == (1, 3)
    assert {row["vertex"] for row in full["rows"] if row["bucket"] == "1x0"} == {"h4", "h5", "h6"}
    sizes = {b["bucket"]: b["count"] for b in full["buckets"]}
    assert sizes == {"0x0": 1, "1x0": 3, "1x3": 1, "1x4": 1, "3x1": 1}
    # Every candidate on every cohort posting is charged, feed arcs included.
    assert full["scanned"] == 18 and not full["truncated"]

    # Distinct neighbours separate three parallel arcs from three collaborators.
    distinct = request(base, census_path, "POST", {**relay, "distinct": True})["result"]
    d_row = {row["vertex"]: row for row in distinct["rows"]}
    assert d_row["h1"]["bucket"] == "1x2" and d_row["h1"]["out_neighbors"] == 2
    assert d_row["h2"]["bucket"] == "1x1" and d_row["h2"]["in_neighbors"] == 1
    assert d_row["h3"]["bucket"] == "1x3" and d_row["h3"]["out_neighbors"] == 3
    assert distinct["scanned"] == 18

    # Sparse cohort: one typed property match; near-total cohort: all seven hubs.
    sparse = request(base, census_path, "POST",
                     {"kind": "hub", "arc_kinds": ["relay"], "filters": [
                         {"property": "active", "value": True}]})["result"]
    assert sparse["cohort_size"] == 1 and sparse["rows"][0]["vertex"] == "h3"
    assert sparse["rows"][0]["bucket"] == "1x3" and sparse["scanned"] == 5
    north = request(base, census_path, "POST",
                    {"kind": "hub", "arc_kinds": ["relay"], "filters": [
                        {"property": "zone", "value": "north"}]})["result"]
    assert north["cohort_size"] == 3
    # Selecting both arc kinds pulls the feed arcs into h1/h3 degrees.
    both_kinds = request(base, census_path, "POST",
                         {"kind": "hub", "arc_kinds": ["relay", "feed"]})["result"]
    b_row = {row["vertex"]: row for row in both_kinds["rows"]}
    assert (b_row["h1"]["in_degree"], b_row["h1"]["out_degree"]) == (1, 5)
    assert (b_row["h3"]["in_degree"], b_row["h3"]["out_degree"]) == (1, 4)

    # A too-small budget rejects the whole census instead of faking zero degrees.
    request(base, census_path, "POST", {**relay, "budget": 1}, 422, "budget_exhausted")
    request(base, census_path, "POST", {**relay, "arc_kinds": ["unknown"]}, 400, "invalid")

    # Fixed-view evidence for one bucket, then a commit must not move the pin.
    pin = request(base, ATLAS + "/pins", "POST", status=201)["handle"]
    evidence_path = ATLAS + "/census/evidence"
    evidence_q = {"kind": "hub", "arc_kinds": ["relay"], "bucket": "1x0", "limit": 2}
    evidence = request(base, evidence_path + "?view=" + pin, "POST", evidence_q)["result"]
    assert evidence["matched"] == 3 and evidence["truncated"]
    assert [item["vertex"] for item in evidence["items"]] == ["h4", "h5"]
    assert evidence["items"][0]["in_arcs"] == ["c1"]
    loop_witness = request(base, evidence_path, "POST",
                           {"kind": "hub", "arc_kinds": ["relay"], "bucket": "1x4"})["result"]
    assert loop_witness["matched"] == 1
    item = loop_witness["items"][0]
    assert item["vertex"] == "h1" and item["out_arcs"] == ["lp1", "p1", "p2", "p3"]
    assert item["in_arcs"] == ["lp1"]
    request(base, evidence_path, "POST", {**evidence_q, "bucket": "9z9"}, 400, "invalid")
    request(base, ATLAS + "/commit", "POST",
            {"base": 2, "edits": [typed_arc("c4", "relay", "h3", "h0")]})
    pinned = request(base, evidence_path + "?view=" + pin, "POST", evidence_q)["result"]
    assert pinned["matched"] == 3
    head = request(base, evidence_path, "POST", evidence_q)["result"]
    assert head["matched"] == 4  # h0 joined 1x0 at the new revision
    request(base, ATLAS + "/pins/" + pin, "DELETE", status=204)
    return "cohort buckets, distinct neighbours, loops, zero degrees, evidence and budgets verified"


WORKFLOWS = {
    "catalog": expect_catalog, "commit": expect_commit, "vertex": expect_vertex,
    "lookup": expect_lookup, "adjacency": expect_adjacency, "reach": expect_reach,
    "path": expect_path, "components": expect_components, "pin": expect_pin,
    "compare": expect_compare, "backup": expect_backup, "restart": expect_restart,
    "census": expect_census,
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
