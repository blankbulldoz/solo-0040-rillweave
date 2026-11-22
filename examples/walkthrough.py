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


def expect_impact(base):
    path = GRAPH + "/impact"
    shared = request(base, path, "POST", {"sources": ["hinge", "keel"], "kinds": ["join"]})["result"]
    assert shared["sources"] == ["hinge", "keel"]
    assert shared["matched"] == 4 and shared["attributions"] == 6
    assert [(c["vertex"], c["hops"], c["driven_by"], c.get("also_source", False))
            for c in shared["consumers"]] == [
        ("keel", 1, "single", True), ("spar", 1, "shared", False),
        ("hinge", 2, "single", True), ("tip", 2, "shared", False),
    ]
    assert shared["source_scope"] == [
        {"source": "hinge", "consumers": 3, "nearest_hop": 1},
        {"source": "keel", "consumers": 3, "nearest_hop": 1},
    ]
    spar = shared["consumers"][1]
    assert [a["source"] for a in spar["sources"]] == ["hinge", "keel"]
    assert [a["hops"] for a in spar["sources"]] == [1, 1]
    assert spar["sources"][0]["evidence"] == {"vertices": ["hinge", "spar"], "arcs": ["brace"]}
    assert shared["consumers"][0]["sources"][0]["evidence"] == {
        "vertices": ["hinge", "keel"], "arcs": ["bypass"],
    }

    cycle = request(base, path, "POST", {"sources": ["keel"], "kinds": ["join"]})["result"]
    assert [(c["vertex"], c["hops"], c["driven_by"]) for c in cycle["consumers"]] == [
        ("spar", 1, "single"), ("hinge", 2, "single"), ("tip", 2, "single"),
    ]
    hinge = cycle["consumers"][1]["sources"][0]["evidence"]
    assert hinge["vertices"] == ["keel", "spar", "hinge"] and hinge["arcs"] == ["cross", "return"]

    reverse = request(base, path, "POST", {
        "sources": ["tip"], "kinds": ["join"], "direction": "in",
    })["result"]
    assert [(c["vertex"], c["hops"]) for c in reverse["consumers"]] == [
        ("spar", 1), ("hinge", 2), ("keel", 2),
    ]

    window = request(base, path, "POST", {"sources": ["hinge"], "kinds": ["join"], "limit": 1})["result"]
    assert window["matched"] == 3 and window["truncated"] and len(window["consumers"]) == 1

    stopped = request(base, path, "POST", {
        "sources": ["hinge"], "kinds": ["join"], "max_hops": 0,
    })["result"]
    assert stopped["matched"] == 0 and stopped["consumers"] == []

    request(base, path, "POST", {"sources": []}, 400, "invalid")
    request(base, path, "POST", {"sources": ["hinge", "hinge"]}, 400, "invalid")
    request(base, path, "POST", {"sources": ["nowhere"]}, 404, "not_found")
    request(base, path, "POST", {"sources": ["hinge"], "budget": 2}, 422, "budget_exhausted")

    pin = hold(base)
    pinned = request(base, path + "?view=" + pin, "POST", {"sources": ["hinge", "keel"], "kinds": ["join"]})
    assert (pinned["graph"], pinned["revision"]) == ("abutment", 2)
    assert pinned["result"]["matched"] == shared["matched"]

    dense(base)
    return "per-consumer attribution, single/shared split, cycle relay and overlapping paths verified"


WEAVE = "/rill/v1/graphs/weave"
WEAVE_ARCS = [
    ("ab", "a", "b"), ("bc", "b", "c"), ("cd", "c", "d"), ("da", "d", "a"),
    ("ce", "c", "e"), ("df", "d", "f"),
    ("gm", "g", "m"), ("hm", "h", "m"), ("mn", "m", "n"), ("no", "n", "o"),
    ("gp", "g", "p"),
]


def dense(base):
    """Seed one cycle cluster full of sources and one pair sharing a path."""
    request(base, "/rill/v1/graphs", "POST", {"name": "weave", "schema": SCHEMA}, 201)
    edits = [put_vertex(key) for key in "abcdefghmnop"]
    edits += [put_arc(arc, source, target, 1) for arc, source, target in WEAVE_ARCS]
    request(base, WEAVE + "/commit", "POST", {"base": 1, "edits": edits})
    path = WEAVE + "/impact"

    crowded = request(base, path, "POST", {"sources": ["c", "a", "b"]})["result"]
    assert crowded["sources"] == ["a", "b", "c"]
    assert [(c["vertex"], c["hops"], c["driven_by"], c.get("also_source", False))
            for c in crowded["consumers"]] == [
        ("b", 1, "shared", True), ("c", 1, "shared", True), ("d", 1, "shared", False),
        ("e", 1, "shared", False), ("a", 2, "shared", True), ("f", 2, "shared", False),
    ]
    # Every source in the cycle reaches the other two, and each consumer merges
    # the sources that reach it, never itself.
    for row in crowded["consumers"]:
        expected = 2 if row.get("also_source") else 3
        assert len(row["sources"]) == expected, row
        assert row["vertex"] not in [a["source"] for a in row["sources"]]
    assert [a["source"] for a in crowded["consumers"][2]["sources"]] == ["a", "b", "c"]
    assert [a["hops"] for a in crowded["consumers"][2]["sources"]] == [3, 2, 1]
    assert crowded["source_scope"] == [
        {"source": "a", "consumers": 5, "nearest_hop": 1},
        {"source": "b", "consumers": 5, "nearest_hop": 1},
        {"source": "c", "consumers": 5, "nearest_hop": 1},
    ]
    assert crowded["matched"] == 6 and crowded["attributions"] == 15
    # The a-b-c-d cycle keeps relaying every source, but a source is expanded
    # from each vertex at most once, so the shared sweep stops at 18 instead of
    # re-walking the loop for each origin.
    assert crowded["scanned"] == 18

    overlap = request(base, path, "POST", {"sources": ["h", "g"]})["result"]
    assert overlap["sources"] == ["g", "h"]
    assert [(c["vertex"], c["hops"], c["driven_by"]) for c in overlap["consumers"]] == [
        ("m", 1, "shared"), ("p", 1, "single"), ("n", 2, "shared"), ("o", 3, "shared"),
    ]
    assert overlap["source_scope"] == [
        {"source": "g", "consumers": 4, "nearest_hop": 1},
        {"source": "h", "consumers": 3, "nearest_hop": 1},
    ]
    assert overlap["matched"] == 4 and overlap["attributions"] == 7
    # g and h share the m-n-o tail, which is expanded once for both sources.
    assert overlap["scanned"] == 5
    wide = request(base, path, "POST", {"sources": ["h", "g"], "limit": 9})["result"]
    assert wide["scanned"] == overlap["scanned"] and not wide["truncated"]
    return crowded, overlap


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
    "path": expect_path, "components": expect_components, "impact": expect_impact,
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
