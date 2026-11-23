"""Prove the /rill/v1 -> /rill/v2 SDK migration preserves commit semantics.

Raw v1 commit requests (examples/migration/samples plus two generated
bulk bodies) are replayed against both commit entries of one server. Each
request is converted with the rillmigrate SDK tool and the run checks:

* the v2 typed envelope commits as one batch with the same acceptance
  result and the same written properties as v1 (image payloads identical),
* the v2 body survives a browser's JSON parse/serialize round trip, while
  the v1 body with adjacent 64-bit integers does not,
* a converted batch that exceeds the shared 8 MiB request limit is flagged
  by the tool, rejected by the v2 entry, and still replayable via v1.
"""

import base64
from contextlib import contextmanager
import json
import shutil
import subprocess
import tempfile
from pathlib import Path
import selectors
from urllib.error import HTTPError
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "examples" / "migration" / "samples"
GENERATOR = ROOT / "examples" / "migration" / "generate.py"
GRAPHS_V1 = "/rill/v1/graphs"


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


def exchange(req, status, code):
    try:
        with urlopen(req, timeout=60) as response:
            actual, payload = response.status, response.read()
    except HTTPError as response:
        with response:
            actual, payload = response.code, response.read()
    value = json.loads(payload) if payload else None
    assert actual == status, (req.full_url, actual, status, value)
    if code is not None:
        assert value["code"] == code, value
    return value


def request(base, path, verb="GET", body=None, status=200, code=None):
    data = json.dumps(body).encode() if body is not None else None
    req = Request(base + path, data, {"Content-Type": "application/json"}, method=verb)
    return exchange(req, status, code)


def post_raw(base, path, raw, status=200, code=None):
    req = Request(base + path, raw, {"Content-Type": "application/json"}, method="POST")
    return exchange(req, status, code)


def create_graph(base, name, create_body):
    created = request(base, GRAPHS_V1, "POST", {**create_body, "name": name}, 201)
    assert created == {"name": name, "revision": 1, "vertices": 0, "arcs": 0}


def graph_doc(base, name):
    envelope = request(base, f"{GRAPHS_V1}/{name}/image")
    doc = json.loads(base64.b64decode(envelope["payload"]))
    doc["name"] = ""
    return doc


def without_name(summary):
    return {key: value for key, value in summary.items() if key != "name"}


JS_ROUNDTRIP = (
    "let chunks=[];process.stdin.on('data',c=>chunks.push(c)).on('end',()=>{"
    "process.stdout.write(JSON.stringify(JSON.parse(Buffer.concat(chunks).toString('utf8'))))});"
)


def js_roundtrip(raw):
    """Apply a browser's JSON.parse followed by JSON.stringify."""
    if shutil.which("node"):
        return subprocess.run(
            ["node", "-e", JS_ROUNDTRIP], input=raw, capture_output=True, check=True,
        ).stdout
    # Fallback model: JavaScript numbers are float64; integral doubles below
    # 1e21 stringify without a decimal point. All sample numbers qualify.
    def fix(value):
        if isinstance(value, float):
            assert value.is_integer() and abs(value) < 1e21
            return int(value)
        if isinstance(value, dict):
            return {k: fix(v) for k, v in value.items()}
        if isinstance(value, list):
            return [fix(v) for v in value]
        return value

    data = json.loads(raw, parse_int=float, parse_float=float)
    return json.dumps(fix(data), separators=(",", ":")).encode()


def migrate(tool, commit_path, out_path, force=False):
    cmd = [str(tool), "-in", str(commit_path), "-out", str(out_path)]
    if force:
        cmd.append("-force")
    return subprocess.run(cmd, capture_output=True, text=True)


def replay_pair(base, tool, tmp, sid, create_path, commit_path):
    create_body = json.loads(create_path.read_text())
    commit_raw = commit_path.read_bytes()

    name_v1, name_v2, name_js = f"{sid}v1", f"{sid}v2", f"{sid}js"
    create_graph(base, name_v1, create_body)
    accepted_v1 = post_raw(base, f"{GRAPHS_V1}/{name_v1}/commit", commit_raw)

    out = tmp / f"{sid}.v2.json"
    proc = migrate(tool, commit_path, out)
    assert proc.returncode == 0, (sid, proc.returncode, proc.stderr)
    converted = out.read_bytes()

    create_graph(base, name_v2, create_body)
    accepted_v2 = post_raw(base, f"/rill/v2/graphs/{name_v2}/commit", converted)
    assert without_name(accepted_v2) == without_name(accepted_v1), (sid, accepted_v1, accepted_v2)

    create_graph(base, name_js, create_body)
    accepted_js = post_raw(base, f"/rill/v2/graphs/{name_js}/commit", js_roundtrip(converted))
    assert without_name(accepted_js) == without_name(accepted_v1), (sid, accepted_js)

    docs = [graph_doc(base, name) for name in (name_v1, name_v2, name_js)]
    assert docs[0] == docs[1] == docs[2], sid
    assert docs[0]["revision"] == 2, sid

    # A stale replay is rejected identically on both entries.
    post_raw(base, f"{GRAPHS_V1}/{name_v1}/commit", commit_raw, 409, "conflict")
    post_raw(base, f"/rill/v2/graphs/{name_v2}/commit", converted, 409, "conflict")

    summary = without_name(accepted_v1)
    print(f"{sid}: v1 {len(commit_raw)} B -> v2 {len(converted)} B; both accepted "
          f"{summary} as one batch; image payloads identical after a browser round trip")
    return create_body, commit_raw, converted


def replay_overflow(base, tool, tmp, sid, create_path, commit_path):
    create_body = json.loads(create_path.read_text())
    commit_raw = commit_path.read_bytes()

    name_v1 = f"{sid}v1"
    create_graph(base, name_v1, create_body)
    accepted = post_raw(base, f"{GRAPHS_V1}/{name_v1}/commit", commit_raw)
    assert accepted["revision"] == 2 and accepted["vertices"] == 8

    out = tmp / f"{sid}.v2.json"
    proc = migrate(tool, commit_path, out)
    assert proc.returncode == 3, (sid, proc.returncode, proc.stderr)
    assert "exceeding the 8388608-byte request limit" in proc.stderr, proc.stderr
    assert "POST /rill/v1/graphs/{name}/commit" in proc.stderr, proc.stderr
    assert not out.exists(), "an overflowing conversion must not be emitted by default"

    forced = tmp / f"{sid}.v2-forced.json"
    proc = migrate(tool, commit_path, forced, force=True)
    assert proc.returncode == 3 and forced.exists(), (proc.returncode, proc.stderr)
    oversized = forced.read_bytes()
    assert len(oversized) > 8 << 20

    name_v2 = f"{sid}v2"
    create_graph(base, name_v2, create_body)
    post_raw(base, f"/rill/v2/graphs/{name_v2}/commit", oversized, 422, "budget_exhausted")
    assert request(base, f"{GRAPHS_V1}/{name_v2}/catalog")["revision"] == 1

    print(f"{sid}: v1 {len(commit_raw)} B accepted via /rill/v1 {without_name(accepted)}; "
          f"converted {len(oversized)} B flagged as overflow and refused by /rill/v2 (422); "
          "the v1 replay path remains")


def expect_bigints(base, create_body, commit_raw):
    # The v1 body through browser tooling collapses the adjacent int64
    # serials and overflows int64 max, so the old entry rejects it.
    create_graph(base, "mig03old", create_body)
    post_raw(base, f"{GRAPHS_V1}/mig03old/commit", js_roundtrip(commit_raw), 400, "invalid")
    assert request(base, f"{GRAPHS_V1}/mig03old/catalog")["revision"] == 1

    # The v2-written graph keeps both adjacent serials distinct and every
    # numeric-looking text property typed as text.
    path = f"{GRAPHS_V1}/mig03v2/lookup"
    q = {"kind": "record", "property": "serial"}
    assert request(base, path, "POST", {**q, "value": 9007199254740992})["result"]["ids"] == ["ra"]
    assert request(base, path, "POST", {**q, "value": 9007199254740993})["result"]["ids"] == ["rb"]
    text = {"kind": "record", "property": "label"}
    assert request(base, path, "POST", {**text, "value": "9007199254740993"})["result"]["ids"] == ["rb"]
    request(base, path, "POST", {**text, "value": 9007199254740993}, 400, "invalid")
    request(base, path, "POST", {**q, "value": "9007199254740993"}, 400, "invalid")
    props = request(base, f"{GRAPHS_V1}/mig03v2/vertices/rb")["result"]["properties"]
    assert props == {"serial": 9007199254740993, "label": "9007199254740993", "marked": False}
    print("mig03: v1 body mangled by browser JSON (400 invalid, revision unchanged); "
          "v2 keeps 9007199254740992/9007199254740993 distinct and numeric-looking text typed")


def expect_atomic_and_separate(base, tool, tmp, create_body, commit_raw, converted):
    create_graph(base, "migatomic", create_body)
    body = (b'{"base":"1","edits":[{"put_vertex":{"id":"p1","kind":"counter",'
            b'"properties":{"seq":{"int":"1"}}}},{"drop_vertex":"ghost"}]}')
    post_raw(base, "/rill/v2/graphs/migatomic/commit", body, 404, "not_found")
    assert request(base, f"{GRAPHS_V1}/migatomic/catalog")["revision"] == 1
    request(base, f"{GRAPHS_V1}/migatomic/vertices/p1", status=404, code="not_found")

    create_graph(base, "migcross1", create_body)
    post_raw(base, f"{GRAPHS_V1}/migcross1/commit", converted, 400, "invalid")
    create_graph(base, "migcross2", create_body)
    post_raw(base, f"/rill/v2/graphs/migcross2/commit", commit_raw, 400, "invalid")
    print("migatomic: a failing edit rejects the whole v2 batch (revision unchanged); "
          "v1 and v2 entries reject each other's bodies")

    # Envelope strictness mirrors v1 rejections; both entries stay at revision 1.
    vertex = b'{"id":"p1","kind":"counter","properties":{"seq":{"int":"1"}}}'
    bad = [
        b'{"base":"1","edits":[]}',  # empty batch, rejected like v1
        b'{"base":"1","edits":[{"put_vertex":{"id":"p1","kind":"counter",'
        b'"properties":{"seq":{"int":"007"}}}}]}',  # non-canonical integer
        b'{"base":"1","edits":[{"put_vertex":{"id":"p1","kind":"counter",'
        b'"properties":{"seq":{"int":"1","text":"1"}}}}]}',  # two typed values
        b'{"base":"1","edits":[{"put_vertex":' + vertex + b',"drop_arc":"a1"}]',  # two operations
    ]
    for raw in bad:
        post_raw(base, "/rill/v2/graphs/migcross2/commit", raw, 400, "invalid")
    post_raw(base, f"{GRAPHS_V1}/migcross1/commit", b'{"base":1,"edits":[]}', 400, "invalid")
    assert request(base, f"{GRAPHS_V1}/migcross2/catalog")["revision"] == 1

    # A v1 edit whose operation and payload disagree has no envelope form.
    mismatched = tmp / "mismatched.json"
    mismatched.write_bytes(b'{"base":1,"edits":[{"op":"put_vertex","arc":{"id":"a1",'
                           b'"kind":"link","from":"c1","to":"c2","cost":1}}]}')
    proc = migrate(tool, mismatched, tmp / "mismatched.v2.json")
    assert proc.returncode == 4 and "POST /rill/v1/graphs/{name}/commit" in proc.stderr, proc.stderr
    print("envelopes: empty batches, non-canonical integers and multi-key envelopes are "
          "invalid on v2; unconvertible v1 edits exit 4 and keep the v1 replay path")


def main():
    with tempfile.TemporaryDirectory(prefix="rillweave-migration-") as temp:
        tmp = Path(temp)
        server, tool = tmp / "rillweaved", tmp / "rillmigrate"
        subprocess.run(["go", "build", "-o", server, "./cmd/rillweaved"], cwd=ROOT, check=True, timeout=120)
        subprocess.run(["go", "build", "-o", tool, "./cmd/rillmigrate"], cwd=ROOT, check=True, timeout=120)
        generated = tmp / "generated"
        subprocess.run(["python3", GENERATOR, "--out", generated], check=True)

        samples = [
            ("mig01", SAMPLES / "01-replace-same-id.create.json", SAMPLES / "01-replace-same-id.commit.json"),
            ("mig02", SAMPLES / "02-final-state.create.json", SAMPLES / "02-final-state.commit.json"),
            ("mig03", SAMPLES / "03-bigints.create.json", SAMPLES / "03-bigints.commit.json"),
            ("mig04", generated / "04-small-ints-bulk.create.json", generated / "04-small-ints-bulk.commit.json"),
        ]
        with serve(server, tmp / "data") as base:
            kept = {}
            for sid, create_path, commit_path in samples:
                kept[sid] = replay_pair(base, tool, tmp, sid, create_path, commit_path)
            replay_overflow(base, tool, tmp, "mig05",
                            generated / "05-overflow-text.create.json",
                            generated / "05-overflow-text.commit.json")
            bigints_create = json.loads((SAMPLES / "03-bigints.create.json").read_text())
            bigints_commit = (SAMPLES / "03-bigints.commit.json").read_bytes()
            expect_bigints(base, bigints_create, bigints_commit)
            create_body, commit_raw, converted = kept["mig01"]
            expect_atomic_and_separate(base, tool, tmp, create_body, commit_raw, converted)
        print("migration comparison: all v1 samples replay identically through the v2 "
              "typed envelope; overflow stays on the v1 entry")


if __name__ == "__main__":
    main()
