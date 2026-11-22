#!/usr/bin/env python3
"""Independent oracle for the typed-degree census.

The server is treated as a black box. This script rebuilds every expected
degree, bucket and the candidate-arc scan charge from the raw arcs it seeded,
then compares them with the census and census/evidence HTTP responses. It also
exercises the sparse and near-total cohort extremes and the budget contract.
"""

import argparse
from collections import defaultdict
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
NAME = "oracle"
G = GRAPHS + "/" + NAME
SCHEMA = {
    "vertices": {
        "hub": {"properties": {"zone": {"kind": "text"}, "active": {"kind": "bool"}}},
        "leaf": {"properties": {"zone": {"kind": "text"}}},
    },
    "arcs": {
        "relay": {"from": "hub", "to": "hub"},
        "feed": {"from": "hub", "to": "leaf"},
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
        yield "http://" + startup[1]
    finally:
        if proc.poll() is None:
            proc.terminate()
            proc.wait(timeout=6)
        proc.stdout.close()


def request(base, path, verb="POST", body=None, status=200, code=None):
    data = json.dumps(body).encode() if body is not None else None
    req = Request(base + path, data, {"Content-Type": "application/json"}, method=verb)
    try:
        with urlopen(req, timeout=10) as response:
            actual, payload = response.status, response.read()
    except HTTPError as response:
        with response:
            actual, payload = response.code, response.read()
    value = json.loads(payload)
    assert actual == status, (path, actual, status, value)
    if code is not None:
        assert value["code"] == code, value
    return value


def arc(kind, arc_id, source, target, cost=1):
    return {"op": "put_arc", "arc": {"id": arc_id, "kind": kind, "from": source, "to": target, "cost": cost}}


def hub(key, zone, active=None):
    props = {"zone": zone}
    if active is not None:
        props["active"] = active
    return {"op": "put_vertex", "vertex": {"id": key, "kind": "hub", "properties": props}}


def build_dataset():
    """Return (vertices, arcs) as plain Python records, plus the commit edits."""
    vertices = {f"h{i}": ("hub", {"zone": "north" if i < 3 else "south"}) for i in range(60)}
    vertices["h1"][1]["active"] = True
    # One fully isolated hub must remain in the table at the 0x0 bucket.
    vertices["h60"] = ("hub", {"zone": "south"})
    # One isolated hub and one hub carrying only a feed arc.
    arcs = []
    edits = [hub(v, props["zone"], props.get("active")) for v, (_, props) in vertices.items()]
    edits.append({"op": "put_vertex", "vertex": {"id": "lf", "kind": "leaf", "properties": {"zone": "north"}}})
    for i in range(2, 30):
        arcs.append({"id": f"r{i}", "kind": "relay", "from": "h0", "to": f"h{i}"})
        edits.append(arc("relay", f"r{i}", "h0", f"h{i}"))
    for k in range(3):
        arcs.append({"id": f"p{k}", "kind": "relay", "from": "h1", "to": "h2"})
        edits.append(arc("relay", f"p{k}", "h1", "h2"))
    arcs.append({"id": "loop", "kind": "relay", "from": "h1", "to": "h1"})
    edits.append(arc("relay", "loop", "h1", "h1"))
    arcs.append({"id": "feed0", "kind": "feed", "from": "h59", "to": "lf"})
    edits.append(arc("feed", "feed0", "h59", "lf"))
    return vertices, arcs, edits


def oracle(vertices, arcs, filters, arc_kinds, distinct):
    selected = {a["id"]: a for a in arcs if a["kind"] in arc_kinds}
    members = []
    for vid, (kind, props) in vertices.items():
        if kind != "hub":
            continue
        if all(props.get(prop) == value for prop, value in filters):
            members.append(vid)
    members.sort()
    rows = {}
    charge = 0
    out_post = defaultdict(list)
    in_post = defaultdict(list)
    for a in arcs:
        out_post[a["from"]].append(a)
        in_post[a["to"]].append(a)
    for vid in members:
        out_arcs = [a for a in out_post[vid] if a["id"] in selected]
        in_arcs = [a for a in in_post[vid] if a["id"] in selected]
        charge += len(out_post[vid]) + len(in_post[vid])
        if distinct:
            out_deg = len({a["to"] for a in out_arcs})
            in_deg = len({a["from"] for a in in_arcs})
        else:
            out_deg, in_deg = len(out_arcs), len(in_arcs)
        rows[vid] = (in_deg, out_deg, len(in_arcs), len(out_arcs))
    return members, rows, charge


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data", help="reuse a data directory instead of a temp one")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="rillweave-oracle-") as temp:
        tmp = Path(temp)
        binary = tmp / "rillweaved"
        subprocess.run(["go", "build", "-o", binary, "./cmd/rillweaved"], cwd=ROOT, check=True, timeout=90)
        data = Path(args.data) if args.data else tmp / "data"
        with serve(binary, data) as base:
            request(base, GRAPHS, "POST", {"name": NAME, "schema": SCHEMA}, 201)
            vertices, arcs, edits = build_dataset()
            request(base, G + "/commit", "POST", {"base": 1, "edits": edits})

            cases = [
                ([], ["relay"], False),
                ([], ["relay"], True),
                ([("active", True)], ["relay"], False),
                ([("zone", "north")], ["relay", "feed"], False),
            ]
            for filters, kinds, distinct in cases:
                members, expected, charge = oracle(vertices, arcs, filters, kinds, distinct)
                body = {"kind": "hub", "arc_kinds": kinds, "distinct": distinct,
                        "filters": [{"property": p, "value": v} for p, v in filters]}
                result = request(base, G + "/census", "POST", body)["result"]
                assert result["cohort_size"] == len(members), body
                assert result["scanned"] == charge, (body, result["scanned"], charge)
                got = {r["vertex"]: (r["in_neighbors"] if distinct else r["in_degree"],
                                     r["out_neighbors"] if distinct else r["out_degree"],
                                     r["in_degree"], r["out_degree"]) for r in result["rows"]}
                for vid in members:
                    assert got[vid] == expected[vid], (body, vid, got[vid], expected[vid])
                # Aggregate counts sum to the whole cohort, zero-degree included.
                assert sum(b["count"] for b in result["buckets"]) == len(members)
                zero_members = {vid for vid in members if expected[vid][:2] == (0, 0)}
                zero_buckets = {b["bucket"]: b["count"] for b in result["buckets"] if b["bucket"] == "0x0"}
                assert zero_buckets.get("0x0", 0) == len(zero_members)
                # Every zero-degree member still appears as its own row.
                assert all(got[vid][:2] == (0, 0) for vid in zero_members)

            # Sparse cohort has one member; near-total cohort covers every hub.
            sparse = request(base, G + "/census", "POST",
                             {"kind": "hub", "arc_kinds": ["relay"],
                              "filters": [{"property": "active", "value": True}]})["result"]
            assert sparse["cohort_size"] == 1 and sparse["rows"][0]["vertex"] == "h1"
            assert sparse["rows"][0]["bucket"] == "1x4"
            total = request(base, G + "/census", "POST",
                            {"kind": "hub", "arc_kinds": ["relay", "feed"]})["result"]
            assert total["cohort_size"] == len(vertices)

            # Budget exhaustion must reject, never relabel unscanned vertices zero.
            request(base, G + "/census", "POST",
                    {"kind": "hub", "arc_kinds": ["relay"], "budget": 1}, 422, "budget_exhausted")
            # An allowance large enough for one vertex still fails on the second.
            request(base, G + "/census", "POST",
                    {"kind": "hub", "arc_kinds": ["relay"], "budget": 2}, 422, "budget_exhausted")

            # Evidence window matches the independently computed bucket members.
            members, expected, _ = oracle(vertices, arcs, [], ["relay"], False)
            want = {vid for vid in members if expected[vid][:2] == (1, 0)}
            evidence = request(base, G + "/census/evidence", "POST",
                               {"kind": "hub", "arc_kinds": ["relay"], "bucket": "1x0", "limit": 3})["result"]
            assert evidence["matched"] == len(want)
            assert {item["vertex"] for item in evidence["items"]} <= want
            assert evidence["truncated"] == (len(want) > 3)
            print("oracle: census degrees, distinct counts, buckets, scan charge, evidence and budget all agree")


if __name__ == "__main__":
    main()
