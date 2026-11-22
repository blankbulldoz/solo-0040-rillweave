"""Exercise the compression workflow end to end against disposable servers.

The scenario is a handoff graph: a few ring clusters (strongly connected
components), many internal vertices that never touch a cross-group arc,
several cross-group arcs attached to the same cluster, parallel cross arcs of
different kinds and costs, a deliberately expensive intra-cluster detour that
must not be spliced into a cheap route, and endpoints with no forward route.

The workflow proves that the installed compressed graph answers the agreed
endpoint set exactly (replayable original-arc witnesses or definitive
no-route), marks everything else as overview, survives a full process restart
without the source instance, and verifies witnesses from its retained arc
dictionary alone.
"""

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
SOURCE = GRAPHS + "/handoff"
COMPACT = "/rill/v1/compressed/handoff-mini"
SCHEMA = {
    "vertices": {"node": {"properties": {}}},
    "arcs": {
        "join": {"from": "node", "to": "node"},
        "guide": {"from": "node", "to": "node"},
    },
}

# Exact-tier endpoints: cross-cluster pairs among these must be exact.
ENDPOINTS = ["a-in", "a-out", "b-in", "b-out", "c-in", "c-out", "sink"]


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


def vertex(key):
    return {"op": "put_vertex", "vertex": {"id": key, "kind": "node"}}


def arc(key, source, target, cost, kind="join"):
    return {"op": "put_arc", "arc": {
        "id": key, "kind": kind, "from": source, "to": target, "cost": cost,
    }}


def seed(base):
    request(base, "/ready")
    request(base, GRAPHS, "POST", {"name": "handoff", "schema": SCHEMA}, 201)
    edits = [vertex(v) for v in (
        [f"a{i}" for i in range(1, 25)] +
        [f"b{i}" for i in range(1, 21)] +
        [f"c{i}" for i in range(1, 16)] +
        ["a-in", "a-out", "a-detour", "b-in", "b-out", "c-in", "c-out", "dead", "sink"]
    )]
    # Cluster A: a1..a24 form a long directed ring.
    for i in range(1, 24):
        edits.append(arc(f"a-ring-{i}", f"a{i}", f"a{i + 1}", 1))
    edits.append(arc("a-ring-24", "a24", "a1", 1))
    # Dense internal filler: expensive shortcuts and reverse links. They make
    # the cluster strongly connected and multiply the source arc count, but an
    # agreed-endpoint minimum-cost walk never chooses them over the unit ring.
    for i in range(1, 23):
        edits.append(arc(f"a-fill-fwd-{i}", f"a{i}", f"a{i + 2}", 7))
    for i in range(2, 25):
        edits.append(arc(f"a-fill-back-{i}", f"a{i}", f"a{i - 1}", 6))
    # Agreed entry/exit boundaries. a-detour is a separate one-vertex cluster:
    # reaching it from the A ring and returning are both deliberately costly,
    # so a cheap cross arc leaving it cannot be spliced onto a ring route.
    edits += [
        arc("a-entry", "a-in", "a1", 1),
        arc("a-back", "a12", "a-in", 1),
        arc("a-exit", "a20", "a-out", 1),
        arc("a-return", "a-out", "a21", 1),
        arc("a-to-detour", "a-in", "a-detour", 25),
        arc("a-detour-back", "a-detour", "a-out", 25),
    ]
    # Cluster B: b1..b20 ring with its own boundary pair.
    for i in range(1, 20):
        edits.append(arc(f"b-ring-{i}", f"b{i}", f"b{i + 1}", 1))
    edits.append(arc("b-ring-20", "b20", "b1", 1))
    for i in range(1, 19):
        edits.append(arc(f"b-fill-fwd-{i}", f"b{i}", f"b{i + 2}", 7))
    for i in range(2, 21):
        edits.append(arc(f"b-fill-back-{i}", f"b{i}", f"b{i - 1}", 6))
    edits += [
        arc("b-entry", "b-in", "b1", 1),
        arc("b-back", "b10", "b-in", 1),
        arc("b-exit", "b15", "b-out", 1),
        arc("b-return", "b-out", "b16", 1),
    ]
    # Cluster C: c1..c15 ring.
    for i in range(1, 15):
        edits.append(arc(f"c-ring-{i}", f"c{i}", f"c{i + 1}", 1))
    edits.append(arc("c-ring-15", "c15", "c1", 1))
    for i in range(1, 14):
        edits.append(arc(f"c-fill-fwd-{i}", f"c{i}", f"c{i + 2}", 7))
    for i in range(2, 16):
        edits.append(arc(f"c-fill-back-{i}", f"c{i}", f"c{i - 1}", 6))
    edits += [
        arc("c-entry", "c-in", "c1", 1),
        arc("c-back", "c7", "c-in", 1),
        arc("c-exit", "c10", "c-out", 1),
        arc("c-return", "c-out", "c11", 1),
    ]
    # Cross-group arcs: several attach to the same A cluster, including
    # parallel arcs of different kinds/costs between one entry/exit pair.
    edits += [
        arc("x-a-b-cheap", "a-out", "b-in", 4, "guide"),
        arc("x-a-b-costly", "a-out", "b-in", 9, "join"),
        arc("x-a-b-alt", "a-detour", "b-in", 2, "join"),
        arc("x-b-c", "b-out", "c-in", 3, "join"),
        arc("x-a-c-direct", "a24", "c-out", 50, "join"),
        arc("x-dead", "dead", "sink", 1, "join"),
    ]
    result = request(base, SOURCE + "/commit", "POST", {"base": 1, "edits": edits})
    expected_vertices = len([e for e in edits if e["op"] == "put_vertex"])
    expected_arcs = len([e for e in edits if e["op"] == "put_arc"])
    assert result["vertices"] == expected_vertices and result["arcs"] == expected_arcs, result
    return f"handoff graph: {expected_vertices} vertices, {expected_arcs} arcs across three ring clusters"


def compress(base):
    body = {"name": "handoff-mini", "kinds": ["join", "guide"], "endpoints": ENDPOINTS}
    result = request(base, SOURCE + "/compress", "POST", body)
    stats = result["stats"]
    assert stats["clusters"] >= 4, stats
    # Only boundary vertices and referenced arcs survive.
    assert stats["retained_vertices"] < stats["original_vertices"], stats
    assert stats["retained_arcs"] < stats["original_arcs"], stats
    assert result["payload_bytes"] < result["source_bytes"], result
    assert result["byte_ratio"] < 1.0, result
    receipt = request(base, "/rill/v1/compressed", "PUT", result["envelope"], 201)
    assert receipt["installed"] == "handoff-mini"
    # A damaged envelope never installs.
    damaged = dict(result["envelope"], sha256="0" * 64)
    request(base, "/rill/v1/compressed", "PUT", damaged, 400, "invalid")
    # Occupied names are not overwritten.
    request(base, "/rill/v1/compressed", "PUT", result["envelope"], 409, "conflict")
    print("  compression stats:", json.dumps(stats, sort_keys=True))
    print(f"  payload {result['source_bytes']} -> {result['payload_bytes']} bytes "
          f"(ratio {result['byte_ratio']:.3f})")
    return result["envelope"]


def route(base, source, target, **extra):
    body = {"from": source, "to": target, **extra}
    return request(base, COMPACT + "/route", "POST", body)["result"]


def replay_witness(base, arcs):
    return request(base, COMPACT + "/replay", "POST", {"arcs": arcs})["result"]


def expect_exact_routes(base):
    # Cheapest A->B uses the guide cross arc, not the costly join parallel.
    answer = route(base, "a-in", "b-in")
    assert answer["exact"] and answer["found"], answer
    assert answer["arcs"][-1] == "x-a-b-cheap", answer
    replay = replay_witness(base, answer["arcs"])
    assert replay["valid"] and replay["cost"] == answer["cost"], replay
    assert replay["vertices"][0] == "a-in" and replay["vertices"][-1] == "b-in"

    # Full A->C chain must not splice the cheap cross arc leaving a-detour:
    # reaching that cluster from a-in costs 25, while the ring exit route via
    # a-out is far cheaper. A cluster-level minimum-only merge would get this
    # wrong; the boundary-level summary keeps the pairs honest.
    chain = route(base, "a-in", "c-in")
    assert chain["exact"] and chain["found"], chain
    assert "x-a-b-alt" not in chain["arcs"], chain
    assert "x-a-b-cheap" in chain["arcs"] and "x-b-c" in chain["arcs"], chain
    replay = replay_witness(base, chain["arcs"])
    assert replay["valid"] and replay["cost"] == chain["cost"], replay
    # Witness shares its A-cluster prefix with the A->B query.
    assert chain["arcs"][:2] == answer["arcs"][:2], (chain["arcs"], answer["arcs"])

    # The trap cross arc is still a valid honest route when the walk starts
    # inside the detour cluster itself.
    trap = route(base, "a-detour", "b-in")
    assert trap["found"] and trap["arcs"] == ["x-a-b-alt"] and trap["cost"] == 2, trap

    # Definitive no-route between agreed endpoints in different clusters.
    unreachable = route(base, "a-in", "sink")
    assert unreachable["exact"] and not unreachable["found"], unreachable
    return "exact routes replayed; parallel kinds and the detour trap handled; no-route is definitive"


def expect_overview_separation(base):
    # Same-cluster agreed endpoints are deliberately overview.
    same = route(base, "a-in", "a-out")
    assert not same["exact"] and same["tier"] == "overview" and same["found"], same
    # A retained boundary outside the agreed set is overview, not an error.
    outside = route(base, "a24", "c-out")
    assert not outside["exact"] and outside["found"], outside
    # Hop bounds downgrade an otherwise exact query with a visible note.
    bounded = route(base, "a-in", "b-in", max_hops=32)
    assert not bounded["exact"] and bounded["tier"] == "overview", bounded
    # Internal vertices never made it into the compressed graph.
    request(base, COMPACT + "/route", "POST", {"from": "a5", "to": "c-in"}, 404, "not_found")
    # Tampered witnesses fail offline replay.
    bad = replay_witness(base, ["a-ring-1", "b-ring-1"])
    assert not bad["valid"] and bad["first_problem"], bad
    return "same-cluster, non-endpoint, hop-bounded and internal cases visibly separated"


def expect_restart(binary, data, envelope):
    # Move to a brand-new data directory holding only the compressed head,
    # proving the image is self-sufficient once the source instance is gone.
    offline = data.parent / "offline-data"
    offline.mkdir()
    head = next(data.glob("*.rillc"))
    (offline / head.name).write_bytes(head.read_bytes())
    with serve(binary, offline) as base:
        overview = request(base, COMPACT)["result"]
        assert overview["name"] == "handoff-mini", overview
        assert overview["source"]["name"] == "handoff", overview
        answer = route(base, "a-in", "c-in")
        assert answer["exact"] and answer["found"], answer
        replay = replay_witness(base, answer["arcs"])
        assert replay["valid"] and replay["cost"] == answer["cost"], replay
        # The portable image can be re-exported byte-identically.
        image = request(base, COMPACT + "/image")
        assert image == envelope, "re-exported compressed image drifted from the installed one"
        # The source graph namespace is genuinely absent offline.
        request(base, SOURCE + "/catalog", status=404, code="not_found")
    # A corrupt compressed head fails startup instead of silently opening.
    (offline / head.name).write_text('{"format":', encoding="ascii")
    failed = subprocess.run(command(binary, offline), capture_output=True, text=True, timeout=8)
    assert failed.returncode != 0 and "recover graph" in failed.stderr, failed.stderr
    return "compressed graph reopened in a clean directory without the source; witnesses verified; corruption blocks startup"


def expect_components_agree(base):
    components = request(base, SOURCE + "/components", "POST",
                         {"kinds": ["join", "guide"]})["result"]
    overview = request(base, COMPACT)["result"]
    assert overview["clusters"] == len(components["groups"]), (overview, components["groups"])
    return "compressed cluster count matches the read-side SCC grouping"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workflow", default="all")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="rillweave-compress-") as temp:
        tmp = Path(temp)
        binary = tmp / "rillweaved"
        subprocess.run(["go", "build", "-o", binary, "./cmd/rillweaved"],
                       cwd=ROOT, check=True, timeout=90)
        data = tmp / "data"
        with serve(binary, data) as base:
            print("seed:", seed(base))
            envelope = compress(base)
            print("components:", expect_components_agree(base))
            print("exact:", expect_exact_routes(base))
            print("overview:", expect_overview_separation(base))
        print("restart:", expect_restart(binary, data, envelope))
        print("compression workflow complete")


if __name__ == "__main__":
    main()
