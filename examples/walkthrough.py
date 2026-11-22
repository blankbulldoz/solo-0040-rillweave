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


def put_typed_vertex(kind, key, **props):
    return {"op": "put_vertex", "vertex": {"id": key, "kind": kind, "properties": props}}


def put_arc(key, source, target, cost, kind="join"):
    return {"op": "put_arc", "arc": {
        "id": key, "kind": kind, "from": source, "to": target, "cost": cost,
    }}


def commit(base, revision, edits, status=200, code=None):
    return request(base, GRAPH + "/commit", "POST", {"base": revision, "edits": edits}, status, code)


def safe_commit(base, revision, edits, status=200, code=None):
    body = {"base": revision, "edits": edits, "rebase": "safe"}
    return request(base, GRAPH + "/commit", "POST", body, status, code)


def current_revision(base):
    return catalog(base)["revision"]


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


def expect_rebase(base):
    # 1. Unrelated intervening writes: a stale batch replays onto the head.
    r0 = current_revision(base)
    commit(base, r0, [put_vertex("rivet", "east")])
    head = current_revision(base)
    assert head == r0 + 1
    receipt = safe_commit(base, r0, [put_vertex("pinion", "west")])
    assert receipt["revision"] == head + 1
    assert request(base, GRAPH + "/vertices/rivet")["result"]["properties"]["tag"] == "east"
    assert request(base, GRAPH + "/vertices/pinion")["result"]["properties"]["tag"] == "west"

    # 2. The same key changed on the head and then changed back is a no-op diff
    #    against the client's base, so the batch remains safe.
    r2 = current_revision(base)
    commit(base, r2, [put_vertex("rivet", "restored")])
    commit(base, r2 + 1, [put_vertex("rivet", "east")])
    # The same restore rule applies to an arc key: head changes its cost and
    # puts it back, so the net diff against the client base is empty.
    commit(base, current_revision(base), [put_arc("bypass", "hinge", "keel", 7)])
    commit(base, current_revision(base), [put_arc("bypass", "hinge", "keel", 1)])
    assert current_revision(base) == r2 + 4
    receipt = safe_commit(base, r2, [put_vertex("socket", "mid")])
    assert receipt["revision"] == r2 + 5
    assert request(base, GRAPH + "/vertices/rivet")["result"]["properties"]["tag"] == "east"

    # 3. A divergent write to one entity key cannot be resolved automatically.
    r3 = current_revision(base)
    commit(base, r3, [put_vertex("rivet", "diverging-head")])
    safe_commit(base, r3, [put_vertex("rivet", "diverge")], 409, "rebase_conflict")
    assert current_revision(base) == r3 + 1
    assert request(base, GRAPH + "/vertices/rivet")["result"]["properties"]["tag"] == "diverging-head"
    # The same entity-key rule applies to arcs: a competing cost is rejected.
    r3b = current_revision(base)
    commit(base, r3b, [put_arc("bypass", "hinge", "keel", 4)])
    safe_commit(base, r3b, [put_arc("bypass", "hinge", "keel", 6)], 409, "rebase_conflict")
    assert request(base, GRAPH + "/adjacency", "POST",
                   {"vertex": "hinge", "kinds": ["join"], "limit": 100})["result"]["matched"]

    # 4. Different keys fighting over one unique value: same contention class,
    #    even though neither client touched the head's entity key.
    r4 = current_revision(base)
    commit(base, r4, [put_vertex("collar", "shared")])
    safe_commit(base, r4, [put_vertex("bushing", "shared")], 409, "rebase_conflict")
    assert request(base, GRAPH + "/vertices/collar")["result"]["properties"]["tag"] == "shared"
    request(base, GRAPH + "/vertices/bushing", status=404, code="not_found")

    # 4b. Crossed write sets: the head removes a vertex that a stale batch arc
    #     still points at, and a stale vertex drop leaves a head arc dangling.
    r4b = current_revision(base)
    commit(base, r4b, [
        {"op": "drop_arc", "id": "stem"}, {"op": "drop_arc", "id": "track"},
        {"op": "drop_vertex", "id": "tip"},
    ])
    safe_commit(base, r4b, [put_arc("stem", "spar", "tip", 2)], 409, "rebase_conflict")
    r4c = current_revision(base)
    commit(base, r4c, [put_vertex("keel", "mid-keel")])
    # The batch is valid on its own base because it drops every arc it anchors,
    # but the head's untouched cross arc makes the replay cross the write sets.
    safe_commit(base, r4c, [
        {"op": "drop_arc", "id": "bypass"}, {"op": "drop_arc", "id": "cross"},
        {"op": "drop_vertex", "id": "keel"},
    ], 409, "rebase_conflict")
    assert request(base, GRAPH + "/vertices/keel")["result"]["properties"]["tag"] == "mid-keel"

    # 5. An endpoint rule for an arc kind the batch uses forces manual review;
    #    an unrelated rule widening stays compatible and still rebases.
    r5 = current_revision(base)
    retargeted = {**SCHEMA, "vertices": {
        **SCHEMA["vertices"],
        "bracket": {"properties": {"code": {"kind": "text", "unique": True}}},
    }, "arcs": {**SCHEMA["arcs"], "join": {"from": "strut", "to": "bracket"}}}
    join_arcs = [
        ("brace", "hinge", "brace-end", 8), ("brace-alt", "hinge", "brace-alt-end", 8),
        ("bypass", "hinge", "bypass-end", 1), ("cross", "keel", "cross-end", 1),
        ("stem", "spar", "stem-end", 1), ("return", "spar", "return-end", 2),
        ("loop", "hinge", "loop-end", 1),
    ]
    migration = [
        {"op": "set_schema", "schema": retargeted},
        put_typed_vertex("bracket", "bracket-anchor", code="bracket-code"),
    ]
    migration += [
        put_arc(arc_id, source, target, cost) for arc_id, source, target, cost in join_arcs
    ]
    migration += [put_typed_vertex("bracket", target, code=f"code-{target}")
                  for _, _, target, _ in join_arcs]
    commit(base, r5, migration)
    # A stale client still assumes join terminates on a strut vertex.
    stale_join = put_arc("brace", "hinge", "spar", 9)
    safe_commit(base, r5, [stale_join], 409, "rebase_conflict")
    receipt = safe_commit(base, r5, [put_vertex("gusset", "trim")])
    assert receipt["revision"] == current_revision(base)

    # 6. The rebase window is bounded: once the base snapshot ages out, the
    #    server must refuse rather than guess. MaxRebaseWindow retains 8 heads.
    r6 = current_revision(base)
    for n in range(9):
        commit(base, current_revision(base), [put_vertex(f"filler-{n}", f"fill-{n}")])
    assert current_revision(base) == r6 + 9
    safe_commit(base, r6, [put_vertex("late", "late")], 409, "rebase_unavailable")

    # 7. Protocol guards: unknown modes are invalid input, a non-stale safe
    #    commit behaves like a normal commit, and legacy stale rejection is
    #    unchanged for callers that never opt in.
    body = {"base": r6, "edits": [put_vertex("refused", "x")], "rebase": "force"}
    request(base, GRAPH + "/commit", "POST", body, 400, "invalid")
    now = current_revision(base)
    receipt = safe_commit(base, now, [put_vertex("fresh", "bright")])
    assert receipt["revision"] == now + 1
    commit(base, r6, [put_vertex("legacy", "old")], 409, "conflict")
    return ("safe rebase accepts unrelated and restored writes; divergent keys, "
            "unique contention and endpoint rules route to manual handling; "
            "aged-out bases are undecidable and legacy stale calls still reject")


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
        # Rebase history is process-local: a stale base known before the restart
        # is undecidable afterwards, never silently merged.
        safe_commit(base, 2, [put_vertex("after", "restart")], 409, "rebase_unavailable")
    (data / "abutment.rill").write_text('{"format":', encoding="ascii")
    blocked_start(binary, data, 'recover graph "abutment"')
    return "directory held exclusively; durable head and index recovered; corruption fails startup"


WORKFLOWS = {
    "catalog": expect_catalog, "commit": expect_commit, "rebase": expect_rebase,
    "vertex": expect_vertex, "lookup": expect_lookup, "adjacency": expect_adjacency,
    "reach": expect_reach, "path": expect_path, "components": expect_components,
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
