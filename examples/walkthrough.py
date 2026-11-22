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
    "vertices": {"strut": {"properties": {
        "tag": {"kind": "text", "unique": True},
        "gauge": {"kind": "int"},
    }}},
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


def put_vertex(key, tag=None, gauge=None):
    props = {}
    if tag is not None:
        props["tag"] = tag
    if gauge is not None:
        props["gauge"] = gauge
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
        put_vertex("hinge", "north", 10), put_vertex("keel"),
        put_vertex("spar", "south", 10), put_vertex("tip", None, 20),
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

    # Sparse equality keeps its exact count even when the window is tiny.
    dense = {"kind": "strut", "property": "gauge", "value": 10, "limit": 1}
    posting = request(base, path, "POST", dense)["result"]
    assert posting["ids"] == ["hinge"] and posting["matched"] == 2 and posting["truncated"]

    # Ordered range: typed value order, then vertex ID for the 10/10 tie.
    rng = {"from": {"value": 5}}
    first = request(base, path, "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "limit": 2,
    })
    page = first["result"]
    assert page["ordered"] == ["hinge", "spar"] and page["matched"] == 3 and page["truncated"]
    assert first["revision"] == 2 and page["range"] == rng

    # A commit between pages must not move the continuation.
    commit(base, 2, [put_vertex("tip", None, 6), put_vertex("wing", "west", 7)])
    older = request(base, path, "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "limit": 2, "after": page["next"],
    })
    assert older["revision"] == 2 and older["result"]["ordered"] == ["tip"], older
    assert not older["result"]["truncated"]

    # A fresh lookup at the newer head continues through it consistently.
    current = request(base, path, "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "limit": 2,
    })
    assert current["revision"] == 3 and current["result"]["ordered"] == ["tip", "wing"]
    assert current["result"]["matched"] == 4
    tail = request(base, path, "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "limit": 2,
        "after": current["result"]["next"],
    })
    assert tail["revision"] == 3 and tail["result"]["ordered"] == ["hinge", "spar"]
    assert not tail["result"]["truncated"] and "next" not in tail["result"]
    request(base, path, "POST", {
        "kind": "strut", "property": "gauge", "range": rng,
        "after": current["result"]["next"],
    }, 404, "not_found")  # spent tokens cannot be replayed

    # Absent properties never enter a range.
    assert request(base, path, "POST", {
        "kind": "strut", "property": "gauge", "range": {"to": {"value": 5}},
    })["result"]["matched"] == 0

    # Exclusive bounds drop the endpoint; text ranges compare by code point.
    exclusive = request(base, path, "POST", {
        "kind": "strut", "property": "gauge",
        "range": {"from": {"value": 5, "exclusive": True}, "to": {"value": 10, "exclusive": True}},
    })["result"]
    assert exclusive["ordered"] == ["tip", "wing"] and exclusive["matched"] == 2
    by_tag = request(base, path, "POST", {
        "kind": "strut", "property": "tag", "range": {"from": {"value": "n"}},
    })["result"]
    assert by_tag["ordered"] == ["hinge", "spar", "wing"] and by_tag["matched"] == 3

    # A pin keeps later pages on that pin's revision, and release ends them.
    pin = hold(base)
    pinned = request(base, path + "?view=" + pin, "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "limit": 2,
    })
    assert pinned["revision"] == 3 and pinned["result"]["ordered"] == ["tip", "wing"]
    commit(base, 3, [put_vertex("rope", "east", 8)])
    older_page = request(base, path + "?view=" + pin, "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "limit": 2,
        "after": pinned["result"]["next"],
    })
    assert older_page["revision"] == 3 and older_page["result"]["ordered"] == ["hinge", "spar"]
    request(base, GRAPH + "/pins/" + pin, "DELETE", status=204)
    request(base, path + "?view=" + pin, "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "after": older_page["result"].get("next", "x"),
    }, 404, "not_found")

    # Wrong-typed or unordered bounds and foreign cursors are rejected.
    request(base, path, "POST", {
        "kind": "strut", "property": "tag", "range": {"from": {"value": 3}},
    }, 400, "invalid")
    request(base, path, "POST", {
        "kind": "strut", "property": "gauge", "value": 1, "range": {"from": {"value": 5}},
    }, 400, "invalid")
    request(base, GRAPHS, "POST", {"name": "pier", "schema": SCHEMA}, 201)
    fresh_cursor = request(base, path, "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "limit": 1,
    })["result"]["next"]
    request(base, GRAPHS + "/pier/lookup", "POST", {
        "kind": "strut", "property": "gauge", "range": rng, "after": fresh_cursor,
    }, 404, "not_found")
    return "equality counts intact; ordered ranges and snapshot-bound continuation verified"


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


WORKFLOWS = {
    "catalog": expect_catalog, "commit": expect_commit, "vertex": expect_vertex,
    "lookup": expect_lookup, "adjacency": expect_adjacency, "reach": expect_reach,
    "path": expect_path, "components": expect_components, "pin": expect_pin,
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
