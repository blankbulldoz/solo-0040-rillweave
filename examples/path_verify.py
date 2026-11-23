"""Independently verify bounded least-cost path search over the HTTP service.

This harness deliberately does not import, link or shell out to the Go path
search. Expected results come from an exhaustive walk enumerator over small
fixtures (see ``expectation``), and every witness the service returns is then
re-validated arc by arc against the stored graph. A rewrite of the search can
therefore be judged against the published contract rather than against itself.

Covered semantics:

* a cheap prefix that cannot finish inside the hop limit (the cheaper prefix
  loses to a dearer route that fits);
* equal total cost with witnesses of different lengths and different arc ID
  sequences, including parallel arcs between the same endpoints;
* reversed steps (``direction`` ``in``/``both``) that must keep the stored arc
  ID and cost and report vertices in traversal order;
* zero-hop results, positive costs and self-loops;
* a deterministic sweep over small graphs and arc-kind selections;
* export of one revision followed by installation under a new name, with the
  source and installed witnesses compared field by field;
* expansion-budget exhaustion as its own outcome: a capacity error, never a
  ``found:false`` route and never compared against an implementation answer.

Each failure is written as a JSON artifact containing the exact setup, request,
expected value, enumerated hand-computable walks and observed response, and can
be replayed with ``--replay ARTIFACT``.

Usage::

    python3 examples/path_verify.py                    # enforce documented ties
    python3 examples/path_verify.py --tie-policy any   # after a tie-break change
    python3 examples/path_verify.py --artifacts DIR    # keep failure records
    python3 examples/path_verify.py --replay FILE      # reproduce one failure

Exit status is non-zero when any query disagrees with the oracle. A server that
exhausts its expansion budget mid-search is expected to answer HTTP 422
``budget_exhausted``; that outcome is counted in its own column and never
silently accepted as "no route" or as a path answer.
"""

import argparse
from contextlib import contextmanager
import itertools
import json
from pathlib import Path
import selectors
import subprocess
import sys
import tempfile
from urllib.error import HTTPError
from urllib.request import Request, urlopen


ROOT = Path(__file__).resolve().parents[1]
GRAPHS = "/rill/v1/graphs"

SCHEMA = {
    "vertices": {"node": {}},
    "arcs": {
        "link": {"from": "node", "to": "node"},
        **{f"k{i}": {"from": "node", "to": "node"} for i in range(5)},
    },
}


# --------------------------------------------------------------------------
# Independent oracle: enumerate every walk up to max_hops and rank them.
# Arc costs are positive integers, so a minimum-cost walk is simple; the
# enumerator still explores all walks, which makes it obviously correct rather
# than dependent on that theorem for pruning.
# --------------------------------------------------------------------------


def steps(arcs, vertex, direction, kinds):
    hops = []
    for edge in arcs:
        if direction in ("out", "both") and edge["from"] == vertex:
            hops.append((edge, edge["to"]))
        elif direction in ("in", "both") and edge["to"] == vertex:
            hops.append((edge, edge["from"]))
    if kinds:
        hops = [(edge, target) for edge, target in hops if edge["kind"] in kinds]
    hops.sort(key=lambda pair: pair[0]["id"])
    return hops


def rank(cost, arcs):
    """Published ordering: cheapest, then fewest arcs, then arc-ID sequence."""
    return (cost, len(arcs), tuple(arcs))


def expectation(vertices, arcs, query):
    source, target = query["from"], query["to"]
    if source not in vertices or target not in vertices:
        raise ValueError("fixture asks for a missing endpoint")
    max_hops = int(query.get("max_hops", 0))
    direction = query.get("direction") or "out"
    kinds = set(query.get("kinds") or ())
    walks = []

    def walk(vertex, cost, path, sequence, depth):
        if vertex == target:
            walks.append((cost, tuple(path), tuple(sequence)))
            return
        if depth == max_hops:
            return
        for edge, next_vertex in steps(arcs, vertex, direction, kinds):
            walk(next_vertex, cost + edge["cost"], path + (edge["id"],),
                 sequence + (next_vertex,), depth + 1)

    walk(source, 0, (), (source,), 0)
    walks.sort(key=lambda found: rank(found[0], found[1]))
    observed = [(cost, list(path)) for cost, path, _ in walks]
    if not walks:
        return {"kind": "none", "found": False, "walks": observed}
    best_cost, best_path, best_sequence = walks[0]
    ties = [list(path) for cost, path, _ in walks if cost == best_cost]
    return {
        "kind": "route",
        "found": True,
        "cost": best_cost,
        "arcs": list(best_path),
        "vertices": list(best_sequence),
        "ties": ties,
        "walks": observed,
    }


# --------------------------------------------------------------------------
# Fixtures. Every input is literal and deterministic; the "note" is the hand
# computation a reader can repeat without running anything.
# --------------------------------------------------------------------------


def arc(identifier, source, target, cost, kind="link"):
    return {"id": identifier, "kind": kind, "from": source, "to": target,
            "cost": cost}


def fixture(identifier, vertices, arcs, note, queries):
    return {"id": identifier, "vertices": vertices, "arcs": arcs,
            "note": note, "queries": queries}


HOP_PREFIX = fixture(
    "hop-prefix", ["s", "a", "b", "c", "d", "t"],
    [
        arc("s_a", "s", "a", 1),
        arc("a_b", "a", "b", 1),
        arc("b_t", "b", "t", 1),
        arc("a_d", "a", "d", 1),
        arc("s_c", "s", "c", 2),
        arc("c_t", "c", "t", 3),
        arc("s_t", "s", "t", 9),
    ],
    "s->a->b->t costs 3 but spends 3 hops, and the cheap 1-hop prefix "
    "s->a (cost 1) then dead-ends through a->d or needs two more hops through "
    "b. With only 2 hops the answer is s->c->t at cost 5, not a cost-3 "
    "prefix. A 1-hop limit leaves only the direct arc at cost 9. The last "
    "query allows budget=1: the correct answer costs 5, so at least two arcs "
    "must be inspected and a one-arc allowance is provably too small.",
    [
        {"from": "s", "to": "t", "max_hops": 3},
        {"from": "s", "to": "t", "max_hops": 2},
        {"from": "s", "to": "t", "max_hops": 1},
        {"from": "s", "to": "t", "max_hops": 0},
        {"from": "t", "to": "s", "max_hops": 4},
        {"from": "s", "to": "t", "max_hops": 2, "budget": 1, "capacity": True},
    ],
)

TIE_LENGTH = fixture(
    "tie-length", ["s", "m", "t"],
    [
        arc("long", "s", "m", 1),
        arc("long2", "m", "t", 5),
        arc("short", "s", "t", 6),
    ],
    "Both witnesses cost 6: the direct arc 'short' (1 hop) and "
    "'long'+'long2' (2 hops). The tie prefers fewer arcs, so 'short' wins; "
    "the longer equal-cost witness must still be a feasible route.",
    [{"from": "s", "to": "t", "max_hops": 3}],
)

TIE_LEX = fixture(
    "tie-lex", ["s", "aa", "zz", "t"],
    [
        arc("a_first", "s", "zz", 2),
        arc("a_second", "zz", "t", 3),
        arc("z_first", "s", "aa", 2),
        arc("z_second", "aa", "t", 3),
    ],
    "Two 2-hop witnesses both cost 5. Arc-ID lexicographic order picks "
    "['a_first','a_second'] through zz; ordering by vertex IDs would pick aa "
    "first, so this fixture separates the two rules.",
    [{"from": "s", "to": "t", "max_hops": 2}],
)

PARALLEL = fixture(
    "parallel", ["s", "t"],
    [
        arc("p_hi", "s", "t", 5),
        arc("p_lo", "s", "t", 5),
        arc("p_mid", "s", "t", 7),
    ],
    "Three parallel arcs share endpoints (s,t). The two cost-5 arcs are "
    "distinct witnesses; the tie picks the smaller arc ID 'p_lo'. The cost-7 "
    "arc must not be merged into the same endpoint pair.",
    [{"from": "s", "to": "t", "max_hops": 1}],
)

REVERSE = fixture(
    "reverse-arc", ["a", "b", "c", "t"],
    [
        arc("tc", "t", "c", 1),
        arc("ta", "t", "a", 9),
        arc("ct", "c", "t", 2),
        arc("cb", "c", "b", 1),
        arc("ba", "b", "a", 1),
    ],
    "Under direction 'in', a->t can follow ba, cb, tc backwards. With one "
    "hop only the reversed ta is usable (cost 9, stored ID still ta); with "
    "three hops the reversed chain costs 3 and lists vertices in traversal "
    "order a,b,c,t. Under 'out' there is no route. No step may rename an "
    "arc or invent a forward pair. The last query allows budget=1: the "
    "correct reversed answer costs 9 through ta, so one inspected arc is "
    "not enough to establish it.",
    [
        {"from": "a", "to": "t", "max_hops": 1, "direction": "in"},
        {"from": "a", "to": "t", "max_hops": 3, "direction": "in"},
        {"from": "a", "to": "t", "max_hops": 3, "direction": "both"},
        {"from": "a", "to": "t", "max_hops": 1, "direction": "both"},
        {"from": "a", "to": "t", "max_hops": 3, "direction": "out"},
        {"from": "a", "to": "t", "max_hops": 1, "direction": "in",
         "budget": 1, "capacity": True},
    ],
)

LOOP = fixture(
    "loop-zero", ["s", "a", "t"],
    [
        arc("s_s", "s", "s", 1),
        arc("s_a", "s", "a", 1),
        arc("a_a", "a", "a", 1),
        arc("a_t", "a", "t", 1),
    ],
    "Costs are strictly positive, so no cycle can beat the empty path: "
    "s->s with 0 hops is found at cost 0, and s->t is s->a->t at cost 2 even "
    "when loops are available.",
    [
        {"from": "s", "to": "s", "max_hops": 0},
        {"from": "s", "to": "s", "max_hops": 4},
        {"from": "s", "to": "t", "max_hops": 4},
        {"from": "t", "to": "t", "max_hops": 0},
    ],
)

CURATED = [HOP_PREFIX, TIE_LENGTH, TIE_LEX, PARALLEL, REVERSE, LOOP]

SWEEP_ARCS = [
    arc("e0", "s", "t", 2, "k0"),
    arc("e1", "s", "a", 1, "k1"),
    arc("e2", "a", "t", 1, "k2"),
    arc("e3", "s", "t", 2, "k3"),
    arc("e4", "a", "s", 1, "k4"),
]


def sweep_queries():
    """Deterministic kind selections over one fixed tiny graph."""
    queries = []
    choices = []
    for size in range(1, len(SWEEP_ARCS) + 1):
        for combination in itertools.combinations(SWEEP_ARCS, size):
            choices.append([edge["kind"] for edge in combination])
    choices.append(None)  # no kind filter == every arc
    for kinds in choices:
        for max_hops in range(0, 4):
            for direction in ("out", "in"):
                query = {"from": "s", "to": "t", "max_hops": max_hops,
                         "direction": direction}
                if kinds is not None:
                    query["kinds"] = kinds
                queries.append(query)
    return queries


SWEEP = fixture(
    "kind-sweep", ["s", "a", "t"], SWEEP_ARCS,
    "Every nonempty selection of five arcs with distinct kinds, queried over "
    "0..3 hops in both directions. Enumerated exhaustively, not sampled.",
    sweep_queries(),
)

FIXTURES = CURATED + [SWEEP]


# --------------------------------------------------------------------------
# HTTP plumbing
# --------------------------------------------------------------------------


def command(binary, data):
    return [str(binary), "-listen", "127.0.0.1:0", "-data", str(data)]


@contextmanager
def serve(binary, data):
    proc = subprocess.Popen(command(binary, data), stdout=subprocess.PIPE,
                            text=True)
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


def call(base, path, verb="GET", body=None):
    data = json.dumps(body).encode() if body is not None else None
    request = Request(base + path, data, {"Content-Type": "application/json"},
                      method=verb)
    try:
        with urlopen(request, timeout=10) as response:
            status, payload = response.status, response.read()
    except HTTPError as response:
        with response:
            status, payload = response.code, response.read()
    return status, json.loads(payload) if payload else None


def post(base, path, body):
    return call(base, path, "POST", body)


def graph_setup(name, value):
    edits = [{"op": "put_vertex", "vertex": {"id": vertex, "kind": "node"}}
             for vertex in value["vertices"]]
    edits += [{"op": "put_arc", "arc": edge} for edge in value["arcs"]]
    return {"kind": "graph", "graph": name, "schema": SCHEMA, "edits": edits}


def install(base, setup):
    status, body = post(base, GRAPHS,
                        {"name": setup["graph"], "schema": setup["schema"]})
    if status != 201:
        raise RuntimeError(f"create {setup['graph']}: {status} {body}")
    status, body = post(base, f"{GRAPHS}/{setup['graph']}/commit",
                        {"base": 1, "edits": setup["edits"]})
    if status != 200:
        raise RuntimeError(f"seed {setup['graph']}: {status} {body}")


# --------------------------------------------------------------------------
# Checks. These never trust the implementation for an expected value.
# --------------------------------------------------------------------------


def classify(status, body):
    if status == 200 and isinstance(body, dict) and isinstance(body.get("result"), dict):
        return "found" if body["result"].get("found") else "no_route"
    if status == 422 and isinstance(body, dict) and body.get("code") == "budget_exhausted":
        return "budget_exhausted"
    return "error"


def permits(edge, source, target, direction):
    forward = edge["from"] == source and edge["to"] == target
    backward = edge["from"] == target and edge["to"] == source
    if direction == "out":
        return forward
    if direction == "in":
        return backward
    return forward or backward


def witness_problems(vertices, arcs, query, result):
    """Re-check a returned witness arc by arc against the stored graph."""
    problems = []
    known = set(vertices)
    edges = {edge["id"]: edge for edge in arcs}
    direction = query.get("direction") or "out"
    kinds = set(query.get("kinds") or ())
    sequence = result.get("vertices")
    path = result.get("arcs")
    cost = result.get("cost")
    if not isinstance(sequence, list) or not isinstance(path, list):
        return ["vertices/arcs are not lists"]
    if len(sequence) != len(path) + 1:
        return [f"{len(path)} arcs but {len(sequence)} vertices"]
    if not sequence or sequence[0] != query["from"] or sequence[-1] != query["to"]:
        problems.append("vertex sequence does not join the requested endpoints")
    if len(path) > query.get("max_hops", 0):
        problems.append(f"witness uses {len(path)} hops above the limit")
    if len(set(path)) != len(path):
        problems.append("witness reuses an arc")
    total = 0
    for index, identifier in enumerate(path):
        edge = edges.get(identifier)
        if edge is None:
            problems.append(f"arc {identifier!r} is not an arc of this graph")
            continue
        if kinds and edge["kind"] not in kinds:
            problems.append(f"arc {identifier!r} is outside the kind selection")
        if sequence[index] not in known or sequence[index + 1] not in known:
            problems.append("witness visits a vertex outside the graph")
        if not permits(edge, sequence[index], sequence[index + 1], direction):
            problems.append(
                f"arc {identifier!r} cannot join {sequence[index]!r}->"
                f"{sequence[index + 1]!r} under direction {direction!r}")
        total += edge["cost"]
    if total != cost:
        problems.append(f"reported cost {cost} but stored arc costs sum to {total}")
    return problems


def budget_problems(query, status, body):
    problems = []
    if classify(status, body) != "budget_exhausted":
        problems.append("budget exhaustion was not reported as HTTP 422 "
                        f"budget_exhausted (got status {status})")
    if isinstance(body, dict) and "result" in body:
        problems.append("budget exhaustion leaked a partial result")
    return problems


def route_problems(value, query, expected, status, body, canonical):
    problems = []
    if classify(status, body) != "found":
        return [f"expected a route, got status {status}: {body}"]
    result = body["result"]
    if result.get("cost") != expected["cost"]:
        problems.append(f"cost: expected {expected['cost']!r}, "
                        f"got {result.get('cost')!r}")
    if not canonical:
        problems.extend(witness_problems(value["vertices"], value["arcs"],
                                         query, result))
        return problems
    for field in ("arcs", "vertices"):
        if result.get(field) != expected[field]:
            problems.append(f"{field}: expected {expected[field]!r}, "
                            f"got {result.get(field)!r}")
    problems.extend(witness_problems(value["vertices"], value["arcs"],
                                     query, result))
    scanned = result.get("scanned")
    if not isinstance(scanned, int) or scanned < 0:
        problems.append(f"scanned is not a candidate count: {scanned!r}")
    return problems


def no_route_problems(status, body):
    if classify(status, body) != "no_route":
        return [f"expected found:false, got status {status}: {body}"]
    if body["result"].get("arcs") or body["result"].get("vertices"):
        return ["a no-route result still listed a witness"]
    return []


# --------------------------------------------------------------------------
# Reporting and replay
# --------------------------------------------------------------------------


def body_for(query):
    keys = ("from", "to", "max_hops", "direction", "kinds", "budget")
    return {key: query[key] for key in keys if key in query}


def describe(query):
    bits = [f"{query['from']}->{query['to']}", f"hops={query['max_hops']}",
            f"dir={query.get('direction') or 'out'}"]
    if "kinds" in query:
        bits.append("kinds=" + ",".join(query["kinds"]))
    if "budget" in query:
        bits.append(f"budget={query['budget']}")
    return " ".join(bits)


def report_failure(identifier, value, query, setup, expected, status, body,
                   problems, artifacts, ordinal):
    print(f"FAIL {identifier}: {describe(query)}")
    print(f"  hand note: {value['note']}")
    print(f"  request:   POST {GRAPHS}/{setup_graph(setup)}/path "
          f"{json.dumps(body_for(query), sort_keys=True)}")
    if expected["kind"] == "route":
        print(f"  expected:  found=true cost={expected['cost']} "
              f"arcs={expected['arcs']} vertices={expected['vertices']}")
    elif expected["kind"] == "none":
        print("  expected:  found=false (no walk within the hop limit)")
    else:
        print(f"  expected:  HTTP 422 budget_exhausted; a route exists at cost "
              f"{expected['feasible_cost']} through {expected['feasible_arcs']}")
    print(f"  actual:    status={status} body={json.dumps(body, sort_keys=True)}")
    if expected["kind"] == "route":
        if len(expected.get("ties", [])) > 1:
            print("  equal-cost witnesses:")
            for path in expected["ties"]:
                print(f"    {','.join(path) or '(empty)'}")
        if expected.get("walks"):
            print("  enumerated walks (cost, hops, arcs), cheapest first:")
            for cost, path in expected["walks"][:12]:
                print(f"    {cost:4} {len(path):2} {','.join(path) or '(empty)'}")
    for problem in problems:
        print(f"  problem:   {problem}")
    if artifacts is not None:
        artifacts.mkdir(parents=True, exist_ok=True)
        path = artifacts / f"path-failure-{safe(identifier)}-{ordinal}.json"
        payload = {
            "fixture": identifier,
            "note": value["note"],
            "setup": setup,
            "request": {"method": "POST",
                        "path": f"{GRAPHS}/{setup_graph(setup)}/path",
                        "body": body_for(query)},
            "expected": expected,
            "actual": {"status": status, "body": body},
            "problems": problems,
        }
        path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
        print(f"  artifact:  {path}")
        print(f"  replay:    python3 examples/path_verify.py --replay {path}")


def setup_graph(setup):
    return setup["graph"] if setup["kind"] == "graph" else setup["name"]


def safe(identifier):
    return "".join(c if c.isalnum() or c in "-_" else "-" for c in identifier)


def run_fixture(base, value, artifacts):
    name = "pv-" + value["id"]
    setup = graph_setup(name, value)
    install(base, setup)
    counts = {"found": 0, "no_route": 0, "budget_exhausted": 0, "error": 0}
    failures = 0
    checked = 0
    for query in value["queries"]:
        expected = expectation(value["vertices"], value["arcs"], query)
        if query.get("capacity"):
            if expected["kind"] != "route":
                raise RuntimeError(f"{name}: capacity case has no feasible route")
            expected = {"kind": "budget", "walks": expected["walks"],
                        "feasible_cost": expected["cost"],
                        "feasible_arcs": expected["arcs"]}
        status, body = post(base, f"{GRAPHS}/{name}/path", body_for(query))
        outcome = classify(status, body)
        checked += 1
        if expected["kind"] == "budget":
            problems = budget_problems(query, status, body)
        elif expected["kind"] == "none":
            problems = no_route_problems(status, body)
        else:
            problems = route_problems(value, query, expected, status, body,
                                      canonical=True)
        counts[outcome] += 1
        if problems:
            failures += 1
            report_failure(value["id"], value, query, setup, expected, status,
                           body, problems, artifacts, checked)
    print(f"{value['id']}: {checked} queries -> {counts['found']} routed, "
          f"{counts['no_route']} no-route, {counts['budget_exhausted']} "
          f"budget-exhausted, {counts['error']} unexpected; "
          f"{failures} failure(s)")
    return failures


def mirror(base, source, value, artifacts):
    status, envelope = call(base, f"{GRAPHS}/{source}/image")
    if status != 200:
        raise RuntimeError(f"export {source}: {status} {envelope}")
    target = "pv-mirror-" + value["id"]
    status, receipt = call(base, f"{GRAPHS}/{target}/image", "PUT", envelope)
    if status != 201:
        raise RuntimeError(f"install {target}: {status} {receipt}")
    if receipt["installed"]["name"] != target or receipt["installed"]["revision"] != 1:
        raise RuntimeError(f"install {target}: unexpected receipt {receipt}")
    setup = {"kind": "image", "name": target, "envelope": envelope,
             "source": source}
    queries = [query for query in value["queries"]
               if not query.get("capacity")]
    failures = 0
    for ordinal, query in enumerate(queries, start=1):
        expected = expectation(value["vertices"], value["arcs"], query)
        source_status, source_body = post(base, f"{GRAPHS}/{source}/path",
                                          body_for(query))
        status, body = post(base, f"{GRAPHS}/{target}/path", body_for(query))
        problems = []
        if source_status != 200 or status != 200:
            problems.append(f"status differs: source {source_status}, "
                            f"installed {status}")
        else:
            if body.get("graph") != target:
                problems.append(f"installed response names {body.get('graph')!r}")
            if body.get("result") != source_body.get("result"):
                problems.append("installed witness differs from the source "
                                f"witness: {body.get('result')} vs "
                                f"{source_body.get('result')}")
            if expected["kind"] == "none":
                if body["result"].get("found"):
                    problems.append("installed graph found a route the oracle "
                                    "enumerated as absent")
            elif classify(status, body) != "found":
                problems.append("installed graph lost a route the source found")
            else:
                for field in ("cost", "arcs", "vertices"):
                    if body["result"].get(field) != expected.get(field):
                        problems.append(f"{field} differs from the oracle")
        if problems:
            failures += 1
            report_failure(value["id"] + "-mirror", value, query, setup,
                           expected, status, body, problems, artifacts, ordinal)
    same = "all witnesses identical" if not failures else "differences reported"
    print(f"{target}: installed source export at revision 1; "
          f"{len(queries)} witness queries over the same graph: {same}")
    return failures


def replay(binary, artifact_path):
    artifact = json.loads(Path(artifact_path).read_text())
    setup = artifact["setup"]
    query = artifact["request"]["body"]
    expected = artifact["expected"]
    name = setup_graph(setup)
    with tempfile.TemporaryDirectory(prefix="rillweave-replay-") as temp:
        with serve(binary, Path(temp)) as base:
            if setup["kind"] == "graph":
                install(base, setup)
            else:
                status, receipt = call(base, f"{GRAPHS}/{name}/image", "PUT",
                                       setup["envelope"])
                if status != 201:
                    raise RuntimeError(f"replay install: {status} {receipt}")
            status, body = post(base, f"{GRAPHS}/{name}/path", query)
            outcome = classify(status, body)
            problems = []
            if expected["kind"] == "budget":
                problems = budget_problems(query, status, body)
            elif expected["kind"] == "none":
                problems = no_route_problems(status, body)
            elif outcome != "found":
                problems = [f"route not reproduced (status {status})"]
            else:
                for field in ("cost", "arcs", "vertices"):
                    if body["result"].get(field) != expected.get(field):
                        problems.append(f"{field} differs from the artifact")
            print(f"replay {artifact['fixture']}: status={status} "
                  f"body={json.dumps(body, sort_keys=True)}")
            for problem in problems:
                print(f"  problem: {problem}")
            return 0 if not problems else 1


# --------------------------------------------------------------------------
# Entry point
# --------------------------------------------------------------------------


def main():
    parser = argparse.ArgumentParser(
        description="Verify bounded least-cost path search independently of "
                    "the implementation.")
    parser.add_argument("--replay", metavar="ARTIFACT",
                        help="re-run one recorded failure on a fresh database")
    parser.add_argument("--artifacts", metavar="DIR",
                        help="directory for failure artifacts")
    parser.add_argument("--tie-policy", choices=["canonical", "any"],
                        default="canonical",
                        help="'canonical' enforces the documented tie-break "
                             "(fewest arcs, then smallest arc-ID sequence); "
                             "'any' accepts any minimum-cost witness and is "
                             "only for a deliberate tie-break change")
    options = parser.parse_args()

    with tempfile.TemporaryDirectory(prefix="rillweave-verify-") as temp:
        tmp = Path(temp)
        binary = tmp / "rillweaved"
        subprocess.run(["go", "build", "-o", binary, "./cmd/rillweaved"],
                       cwd=ROOT, check=True, timeout=180)
        if options.replay:
            return replay(binary, options.replay)
        if options.tie_policy != "canonical":
            print("tie policy 'any': enforcing minimum cost and witness "
                  "validity, not canonical selection")
            global route_problems
            original = route_problems
            route_problems = _any_tie(original)
        artifacts = Path(options.artifacts) if options.artifacts else None
        failures = 0
        with serve(binary, tmp / "data") as base:
            for value in FIXTURES:
                failures += run_fixture(base, value, artifacts)
            for value in CURATED:
                failures += mirror(base, "pv-" + value["id"], value, artifacts)
        routes = no_route = budget = 0
        for value in FIXTURES:
            for query in value["queries"]:
                if query.get("capacity"):
                    budget += 1
                elif expectation(value["vertices"], value["arcs"],
                                 query)["kind"] == "route":
                    routes += 1
                else:
                    no_route += 1
        print(f"oracle expectations: {routes} routes, {no_route} no-route, "
              f"{budget} budget cases")
        if failures:
            print(f"{failures} problem(s)")
            return 1
        print("path verification passed")
        return 0


def _any_tie(original):
    def wrapped(value, query, expected, status, body, canonical):
        if expected["kind"] != "route":
            return original(value, query, expected, status, body, canonical)
        return original(value, query, expected, status, body, False)
    return wrapped


if __name__ == "__main__":
    sys.exit(main())
