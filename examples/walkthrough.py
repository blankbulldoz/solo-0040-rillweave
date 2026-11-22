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


def expect_matrix(base):
    path = GRAPH + "/matrix"
    q = {
        "entries": ["hinge", "keel"], "exits": ["spar", "tip", "hinge"],
        "max_hops": 2, "kinds": ["join"],
        "witness": [{"entry": "hinge", "exit": "tip"}, {"entry": "keel", "exit": "hinge"}],
    }
    value = request(base, path, "POST", q)["result"]
    assert value["entries"] == q["entries"] and value["exits"] == q["exits"]
    assert value["complete"] and value["searches"] == 2 and value["scanned"] == 14
    assert value["summary"] == {"routes": 6, "no_route": 0, "undetermined": 0}
    cells = {(cell["entry"], cell["exit"]): cell for cell in value["cells"]}
    assert [(cell["entry"], cell["exit"]) for cell in value["cells"]] == [
        (entry, exit) for entry in q["entries"] for exit in q["exits"]]
    expected = {
        ("hinge", "spar"): (2, ["bypass", "cross"]),
        ("hinge", "tip"): (9, ["brace", "stem"]),
        ("hinge", "hinge"): (0, []),
        ("keel", "spar"): (1, ["cross"]),
        ("keel", "tip"): (2, ["cross", "stem"]),
        ("keel", "hinge"): (3, ["cross", "return"]),
    }
    for cell_key, (cost, arcs) in expected.items():
        cell = cells[cell_key]
        assert cell["status"] == "route" and cell["cost"] == cost, cell
    # Only requested cells carry an arc witness; the rest expose cost alone.
    assert cells[("hinge", "tip")]["witness"] == {
        "vertices": ["hinge", "spar", "tip"], "arcs": ["brace", "stem"]}
    assert cells[("keel", "hinge")]["witness"] == {
        "vertices": ["keel", "spar", "hinge"], "arcs": ["cross", "return"]}
    for cell_key in expected.keys() - {("hinge", "tip"), ("keel", "hinge")}:
        assert "witness" not in cells[cell_key]
    for (entry, exit), (cost, arcs) in expected.items():
        single = request(base, GRAPH + "/path", "POST", {
            "from": entry, "to": exit, "max_hops": 2, "kinds": ["join"]})["result"]
        assert single["found"] and single["cost"] == cost
        if cells[(entry, exit)].get("witness"):
            assert single["arcs"] == arcs

    # One shared search per entry covers the whole row: two entries scan
    # 11 + 3 candidates instead of 9 + 11 + 1 + 3 + 3 for five pair queries.
    for entries, exit_names, scan in ((["hinge"], q["exits"], 11),
                                      (["keel"], q["exits"], 3)):
        row = request(base, path, "POST", {
            "entries": entries, "exits": exit_names, "max_hops": 2,
            "kinds": ["join"]})["result"]
        assert row["searches"] == 1 and row["scanned"] == scan

    # With fewer exits than entries the batch searches backwards once per exit
    # and still labels each cell in forward orientation.
    wide = {"entries": ["hinge", "keel", "tip"], "exits": ["spar"], "max_hops": 3,
            "kinds": ["join"]}
    by_exit = request(base, path, "POST", {**wide, "witness": [
        {"entry": "hinge", "exit": "spar"}, {"entry": "keel", "exit": "spar"}]})["result"]
    assert by_exit["searches"] == 1 and by_exit["scanned"] == 13
    assert by_exit["complete"]
    assert by_exit["summary"] == {"routes": 2, "no_route": 1, "undetermined": 0}
    exits = {(cell["entry"], cell["exit"]): cell for cell in by_exit["cells"]}
    assert exits[("hinge", "spar")]["cost"] == 2 and exits[("keel", "spar")]["cost"] == 1
    assert exits[("tip", "spar")] == {
        "entry": "tip", "exit": "spar", "status": "no_route"}
    # A backward search reports the same forward-oriented witness as /path.
    for entry, arcs in (("hinge", ["bypass", "cross"]), ("keel", ["cross"])):
        single = request(base, GRAPH + "/path", "POST", {
            "from": entry, "to": "spar", "max_hops": 3, "kinds": ["join"]})["result"]
        assert exits[(entry, "spar")]["witness"]["arcs"] == single["arcs"] == arcs

    # Exhausting the backward search marks the untouched cells undetermined.
    partial_exit = request(base, path, "POST", {**wide, "budget": 4})["result"]
    assert not partial_exit["complete"] and partial_exit["searches"] == 1
    assert partial_exit["scanned"] == 4
    assert partial_exit["summary"] == {
        "routes": 0, "no_route": 0, "undetermined": 3}

    # A budget that stops midway keeps proved no-route cells apart from cells
    # that were never decided; the shared diagonal route still needs no work.
    mix = {"entries": ["tip", "hinge"], "exits": ["spar", "keel", "hinge"],
           "max_hops": 2, "kinds": ["join"], "budget": 2}
    partial = request(base, path, "POST", mix)["result"]
    assert not partial["complete"] and partial["searches"] == 2
    assert partial["scanned"] == 2
    assert partial["summary"] == {"routes": 1, "no_route": 3, "undetermined": 2}
    decided = {(cell["entry"], cell["exit"]): (cell["status"], cell.get("cost"))
               for cell in partial["cells"]}
    assert decided[("tip", "spar")] == decided[("tip", "keel")] == ("no_route", None)
    assert decided[("tip", "hinge")] == ("no_route", None)
    assert decided[("hinge", "spar")] == ("undetermined", None)
    assert decided[("hinge", "keel")] == ("undetermined", None)
    assert decided[("hinge", "hinge")] == ("route", 0)

    # Every cell comes from one selected view, and direction and kind filters
    # are the same for every cell in the batch.
    pin = hold(base)
    commit(base, 2, [put_arc("brace", "hinge", "spar", 20),
                     put_arc("brace-alt", "hinge", "spar", 20)])
    held = request(base, path + "?view=" + pin, "POST", q)["result"]
    head = request(base, path, "POST", q)["result"]
    assert {(c["entry"], c["exit"]): c["cost"] for c in held["cells"]}[("hinge", "tip")] == 9
    assert {(c["entry"], c["exit"]): c["cost"] for c in head["cells"]}[("hinge", "tip")] == 21
    reversed_q = {**q, "entries": ["tip"], "exits": ["hinge"], "direction": "in",
                  "witness": [], "max_hops": 2}
    reversed_cells = request(base, path + "?view=" + pin, "POST", reversed_q)["result"]
    assert reversed_cells["cells"][0]["cost"] == 9
    guides = request(base, path, "POST", {
        **q, "kinds": ["guide"], "max_hops": 1})["result"]
    assert guides["summary"] == {"routes": 1, "no_route": 5, "undetermined": 0}
    assert guides["cells"][2]["status"] == "route" and guides["cells"][2]["cost"] == 0
    zero = request(base, path, "POST", {**q, "max_hops": 0})["result"]
    assert zero["complete"] and zero["summary"] == {
        "routes": 1, "no_route": 5, "undetermined": 0}

    # Oracle: every cell agrees with an individual /path query on the same
    # view, for both the forward and the backward search shape.
    commit(base, 3, [
        put_vertex("wing"), put_arc("spur", "tip", "wing", 3),
        put_arc("wingback", "wing", "keel", 2, "guide"),
        put_arc("shortcut", "hinge", "tip", 4),
    ])
    nodes = ["hinge", "keel", "spar", "tip"]
    for direction in ("out", "in", "both"):
        for hop in (1, 2, 3):
            for entries, exits in ((nodes, nodes[:2]), (nodes[:2], nodes)):
                batch = request(base, path, "POST", {
                    "entries": entries, "exits": exits, "max_hops": hop,
                    "direction": direction,
                    "witness": [{"entry": e, "exit": x} for e in entries
                                for x in exits],
                })["result"]
                assert batch["complete"]
                for cell in batch["cells"]:
                    single = request(base, GRAPH + "/path", "POST", {
                        "from": cell["entry"], "to": cell["exit"], "max_hops": hop,
                        "direction": direction})["result"]
                    if single["found"]:
                        assert cell["status"] == "route", (direction, hop, cell)
                        assert cell["cost"] == single["cost"], (direction, hop, cell, single)
                        assert cell["witness"] == {
                            "vertices": single["vertices"], "arcs": single["arcs"]}
                    else:
                        assert cell["status"] == "no_route", (direction, hop, cell)
                        assert "cost" not in cell and "witness" not in cell

    request(base, path, "POST", {"entries": [], "exits": ["spar"]}, 400, "invalid")
    request(base, path, "POST", {"entries": ["hinge"], "exits": ["spar", "spar"]}, 400, "invalid")
    request(base, path, "POST", {**q, "witness": [{"entry": "hinge", "exit": "keel"}]}, 400, "invalid")
    request(base, path, "POST", {**q, "entries": ["nope"]}, 404, "not_found")
    request(base, path, "POST", {**q, "max_hops": 33}, 400, "invalid")
    request(base, path, "POST", {**q, "entries": ["hinge"] * 65}, 400, "invalid")
    return ("shared searches resolve a cost matrix from either side; witnesses "
            "match single paths; no-route, undetermined and budget stay distinct")


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
    "path": expect_path, "matrix": expect_matrix, "components": expect_components,
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
