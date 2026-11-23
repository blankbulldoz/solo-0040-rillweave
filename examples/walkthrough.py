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


def restore_command(base, path, envelope, expect=None, operation=None, status=200, code=None):
    body = dict(envelope)
    if expect is not None:
        body["expect"] = expect
    if operation is not None:
        body["operation"] = operation
    return request(base, path, "PUT", body, status, code)


def expect_overwrite(base):
    source = request(base, GRAPH + "/image")
    path = GRAPHS + "/pier/image"
    legacy = request(base, path, "PUT", source, 201)
    assert legacy["installed"]["revision"] == 1 and "operation" not in legacy

    # An import that carries an identifier is retryable even at a free name.
    fresh = restore_command(base, GRAPHS + "/jetty/image", source, operation="import-1", status=201)
    assert fresh["installed"]["revision"] == 1 and "expected" not in fresh
    assert restore_command(base, GRAPHS + "/jetty/image", source, operation="import-1") == fresh
    assert request(base, GRAPHS + "/jetty/catalog")["revision"] == 1
    restore_command(base, GRAPHS + "/jetty/image", source, expect=1, operation="import-1",
                   status=409, code="conflict")

    # A view held on the target survives the replacement at its old revision.
    target_pin = hold(base, GRAPHS + "/pier")

    # An occupied name now needs an explicit expected head.
    restore_command(base, path, source, status=409, code="conflict")
    restore_command(base, path, source, expect=0, operation="deploy-1", status=409, code="conflict")
    first = restore_command(base, path, source, expect=1, operation="deploy-1")
    assert first["operation"] == "deploy-1" and first["expected"] == 1
    assert first["installed"]["revision"] == 2 and first["source"]["revision"] == 2
    assert request(base, GRAPHS + "/pier/catalog?view=" + target_pin)["revision"] == 1

    # A lost response is reconciled by replaying the same operation: the
    # recorded receipt returns and the current head does not move.
    replay = restore_command(base, path, source, expect=1, operation="deploy-1")
    assert replay == first
    assert request(base, GRAPHS + "/pier/catalog")["revision"] == 2

    # The same identifier cannot switch image or expected head while retained.
    restore_command(base, path, source, expect=2, operation="deploy-1", status=409, code="conflict")
    wrong = {**source, "sha256": "f" * 64}
    restore_command(base, path, wrong, expect=1, operation="deploy-1", status=409, code="conflict")
    # A damaged retransmission fails verification instead of replaying.
    damaged = {**source, "payload": source["payload"][:-4] + "AAAA"}
    restore_command(base, path, damaged, expect=1, operation="deploy-1", status=400, code="invalid")
    request(base, path, "PUT", {**source, "expect": 1, "operation": "x" * 129}, 400, "invalid")

    # Another client commits on top of the replacement; the old integer
    # precondition stays stale because replacement never rewinds the head.
    commit_path = GRAPHS + "/pier/commit"
    request(base, commit_path, "POST", {"base": 2, "edits": [put_vertex("wing", "east")]})
    request(base, commit_path, "POST", {"base": 1, "edits": [put_vertex("keel")]}, 409, "conflict")
    assert restore_command(base, path, source, expect=1, operation="deploy-1") == first
    assert request(base, GRAPHS + "/pier/catalog")["revision"] == 3

    # Only the latest successful overwrite is retained; a newer one makes the
    # older retry fail explicitly instead of replaying.
    second = restore_command(base, path, source, expect=3, operation="deploy-2")
    assert second["installed"]["revision"] == 4
    restore_command(base, path, source, expect=1, operation="deploy-1", status=409, code="conflict")
    plain = restore_command(base, path, source, expect=4)
    assert plain["installed"]["revision"] == 5 and "operation" not in plain
    restore_command(base, path, source, expect=3, operation="deploy-2", status=409, code="conflict")
    return "conditional overwrite, idempotent replay, conflict on reuse and one retained receipt"


def expect_restore_restart(binary, data):
    with serve(binary, data) as base:
        seed(base)
        source = request(base, GRAPH + "/image")
        path = GRAPHS + "/pier/image"
        restore_command(base, path, source, operation="nightly", status=201)
        receipt = restore_command(base, path, source, expect=1, operation="try-1")
        assert receipt["installed"]["revision"] == 2
    with serve(binary, data) as base:
        # A normal commit coexists with the retained receipt, and the replay
        # still returns the original source and installed revision.
        assert request(base, GRAPHS + "/pier/catalog")["revision"] == 2
        request(base, GRAPHS + "/pier/commit", "POST", {"base": 2, "edits": [put_vertex("wing")]})
        assert restore_command(base, path, source, expect=1, operation="try-1") == receipt
        assert request(base, GRAPHS + "/pier/catalog")["revision"] == 3
        restore_command(base, path, source, expect=3, operation="try-1", status=409, code="conflict")
    return "lost response, later commit and restart all reconcile the same operation receipt"


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
    "compare": expect_compare, "backup": expect_backup, "overwrite": expect_overwrite,
    "restart": expect_restart,
}

MULTI_SERVER = {"restart": expect_restart, "overwrite-restart": expect_restore_restart}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workflow", choices=["all", *WORKFLOWS, "overwrite-restart"], default="all")
    choice = parser.parse_args().workflow
    with tempfile.TemporaryDirectory(prefix="rillweave-") as temp:
        tmp = Path(temp)
        binary = tmp / "rillweaved"
        subprocess.run(["go", "build", "-o", binary, "./cmd/rillweaved"], cwd=ROOT, check=True, timeout=90)
        names = [*WORKFLOWS, "overwrite-restart"] if choice == "all" else [choice]
        for name in names:
            data = tmp / name
            if name in MULTI_SERVER:
                output = MULTI_SERVER[name](binary, data)
            else:
                with serve(binary, data) as base:
                    seed(base)
                    output = WORKFLOWS[name](base)
            print(f"{name}: {output}")


if __name__ == "__main__":
    main()
