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
    # Scanned counts only this page's candidate inspections; the window stops
    # once four matched rows are delivered, so later rows are not billed here.
    assert value["truncated"] and value["matched"] == 5 and value["scanned"] == 4
    request(base, path, "POST", {**q, "kinds": ["unknown"]}, 400, "invalid")

    # Page through the whole incidence with a continuation cursor. The second
    # page resumes strictly after the last emitted arc ID and does not rescan
    # the first window: only the final join row and the rejected guide remain.
    cursor = value["next_cursor"]
    both_cursor = cursor
    assert cursor, "a truncated window must return a continuation cursor"
    second = request(base, path, "POST", {**q, "limit": 4, "cursor": cursor})["result"]
    assert [a["id"] for a in second["arcs"]] == ["return"]
    assert second["matched"] == 5 and second["scanned"] == 2
    assert not second["truncated"] and "next_cursor" not in second

    # A full window followed by an empty continuation proves the cursor is
    # positional rather than an offset that would re-emit or skip rows.
    windows = request(base, path, "POST", {**q, "limit": 4})["result"]
    mid_cursor = windows["next_cursor"]
    tail = request(base, path, "POST", {**q, "limit": 4, "cursor": mid_cursor})["result"]
    assert [a["id"] for a in tail["arcs"]] == ["return"] and tail["matched"] == 5
    assert not tail["truncated"] and "next_cursor" not in tail
    # Re-reading from the same cursor is deterministic and yields the same tail.
    empty = request(base, path, "POST", {**q, "limit": 4, "cursor": mid_cursor})["result"]
    assert [a["id"] for a in empty["arcs"]] == ["return"] and empty["matched"] == 5
    # A window that exactly covers the full match set ends on the same page.
    exact = request(base, path, "POST", {**q, "limit": 5})["result"]
    assert len(exact["arcs"]) == 5 and not exact["truncated"] and "next_cursor" not in exact

    # Parallel arcs share endpoints but keep distinct identities: every one of
    # them must appear across the pages in arc ID order.
    parallel = {"vertex": "hinge", "direction": "out", "kinds": ["join"], "limit": 1}
    page = request(base, path, "POST", parallel)["result"]
    seen = []
    while True:
        seen.extend(a["id"] for a in page["arcs"])
        cursor = page.get("next_cursor")
        if not cursor:
            break
        page = request(base, path, "POST", {**parallel, "cursor": cursor})["result"]
    assert seen == ["brace", "brace-alt", "bypass", "loop"]

    # The same multigraph in the reverse direction exposes the incoming
    # parallel arcs as their own distinct identities.
    incoming = {"vertex": "spar", "direction": "in", "kinds": ["join"], "limit": 1}
    page = request(base, path, "POST", incoming)["result"]
    seen = []
    while True:
        seen.extend(a["id"] for a in page["arcs"])
        cursor = page.get("next_cursor")
        if not cursor:
            break
        page = request(base, path, "POST", {**incoming, "cursor": cursor})["result"]
    assert seen == ["brace", "brace-alt", "cross"]

    # A loop occupies one row in each directional posting but is emitted once;
    # self-loop pagination must not double-count or duplicate it.
    loop_page = request(base, path, "POST", {
        "vertex": "hinge", "direction": "both", "kinds": ["join"], "limit": 4,
    })["result"]
    assert sum(a["id"] == "loop" for a in loop_page["arcs"]) == 1
    loop_again = request(base, path, "POST", {
        "vertex": "hinge", "direction": "both", "kinds": ["join"],
        "cursor": loop_page["next_cursor"],
    })["result"]
    assert all(a["id"] != "loop" for a in loop_again["arcs"])

    # Malformed cursor and a cursor that changes the scan are rejected rather
    # than silently interpreted as a new query.
    request(base, path, "POST", {**q, "cursor": "not-a-token"}, 400, "invalid")
    request(base, path, "POST", {
        **q, "cursor": both_cursor, "direction": "in",
    }, 400, "invalid")
    request(base, path, "POST", {
        "vertex": "keel", "direction": "both", "kinds": ["join"], "cursor": both_cursor,
    }, 400, "invalid")

    # Hold the current revision before any write so the pin can keep serving
    # the original scan accurately while the head moves on.
    pin = hold(base)

    # Writes advance the revision; reusing an old head cursor is a recognizable
    # continuation conflict, even when the previous last arc still exists.
    commit(base, 2, [put_arc("afterward", "hinge", "tip", 1)])
    request(base, path, "POST", {**q, "cursor": both_cursor}, 409, "conflict")
    # A cursor issued on the new head carries revision 3.
    head_cursor = request(base, path, "POST", {**q, "limit": 4})["result"]["next_cursor"]

    # The frozen pin keeps paging the original revision 2 scan accurately.
    pinned_first = request(base, GRAPH + "/adjacency?view=" + pin, "POST", {
        **q, "limit": 4,
    })
    assert pinned_first["revision"] == 2
    pinned_tail = request(base, GRAPH + "/adjacency?view=" + pin, "POST", {
        **q, "cursor": pinned_first["result"]["next_cursor"],
    })["result"]
    assert [a["id"] for a in pinned_tail["arcs"]] == ["return"]
    assert pinned_tail["matched"] == 5
    # A newer head cursor cannot resume against the older pinned revision.
    request(base, GRAPH + "/adjacency?view=" + pin, "POST", {
        **q, "cursor": head_cursor,
    }, 409, "conflict")
    # The original revision-2 cursor remains valid against the matching pin.
    request(base, GRAPH + "/adjacency?view=" + pin, "POST", {
        **q, "cursor": both_cursor,
    }, 200)

    # Deleting the previous page's last arc on the head makes the old cursor
    # fail cleanly instead of returning a silently shifted window.
    commit(base, 3, [{"op": "drop_arc", "id": "afterward"}])
    fresh = request(base, path, "POST", {**q, "limit": 4})["result"]
    request(base, path, "POST", {**q, "cursor": fresh["next_cursor"]}, 200)
    commit(base, 4, [{"op": "drop_arc", "id": "return"}])
    request(base, path, "POST", {**q, "cursor": fresh["next_cursor"]}, 409, "conflict")
    return "parallel arcs retained; loop counted once; cursor paging, revision conflicts and pinned continuation respected"


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
