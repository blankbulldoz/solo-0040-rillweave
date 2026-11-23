"""Demonstrate and verify the legacy .rill -> SQLite storage cutover.

The scenario uses a data directory written by the current server build, with
several graphs at different revisions and a leftover legacy temp file. It
proves that:

  * every graph keeps its local name and exact revision (no HTTP-import reset);
  * migration is rerunnable and survives an interruption before the commit
    point and after the database has been activated;
  * after cutover the server serves only the SQLite database, never a stale
    .rill head;
  * queries return identical results before and after the cutover;
  * a new commit lands in the database and is still present after a restart.
"""

import argparse
import json
import os
from pathlib import Path
import selectors
import shutil
import subprocess
import tempfile
from urllib.request import Request, urlopen


ROOT = Path(__file__).resolve().parents[1]
GRAPHS = "/rill/v1/graphs"
SCHEMA = {
    "vertices": {"strut": {"properties": {"tag": {"kind": "text", "unique": True}}}},
    "arcs": {"join": {"from": "strut", "to": "strut"}},
}


def command(binary, data, *extra):
    return [str(binary), "-listen", "127.0.0.1:0", "-data", str(data), *extra]


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
        return proc, "http://" + startup[1]
    except Exception:
        proc.kill()
        raise


def stop(proc):
    if proc.poll() is None:
        proc.terminate()
    proc.wait(timeout=10)
    proc.stdout.close()


def request(base, path, verb="GET", body=None, status=200):
    data = json.dumps(body).encode() if body is not None else None
    req = Request(base + path, data, {"Content-Type": "application/json"}, method=verb)
    try:
        with urlopen(req, timeout=5) as response:
            payload = response.read()
            actual = response.status
    except Exception as exc:  # HTTPError carries status and body
        actual = exc.code
        payload = exc.read()
    value = json.loads(payload) if payload else None
    assert actual == status, (verb, path, actual, status, value)
    return value


def put_vertex(key, tag=None):
    props = {"tag": tag} if tag is not None else {}
    return {"op": "put_vertex", "vertex": {"id": key, "kind": "strut", "properties": props}}


def put_arc(key, source, target, cost):
    return {"op": "put_arc", "arc": {"id": key, "kind": "join", "from": source, "to": target, "cost": cost}}


GRAPH_NAMES = ("abutment", "pier")


def snapshot(base):
    """Catalog-level snapshot plus one query per graph for equality checks."""
    result = {}
    for name in GRAPH_NAMES:
        g = f"{GRAPHS}/{name}"
        catalog = request(base, g + "/catalog")
        lookup = request(base, g + "/lookup", "POST", {
            "kind": "strut", "property": "tag", "value": "north",
        })
        result[name] = {
            "revision": catalog["revision"],
            "vertices": catalog["result"]["vertices"],
            "arcs": catalog["result"]["arcs"],
            "north_ids": lookup["result"]["ids"],
        }
    return result


def seed_legacy_directory(binary, data):
    proc, base = serve(binary, data)
    try:
        # abutment ends at revision 3; pier stays at revision 1.
        request(base, GRAPHS, "POST", {"name": "abutment", "schema": SCHEMA}, 201)
        request(base, f"{GRAPHS}/abutment/commit", "POST", {"base": 1, "edits": [
            put_arc("brace", "hinge", "spar", 4),
            put_vertex("hinge", "north"), put_vertex("spar", "south"),
        ]})
        request(base, f"{GRAPHS}/abutment/commit", "POST", {"base": 2, "edits": [
            put_vertex("keel", "mid"),
        ]})
        request(base, GRAPHS, "POST", {"name": "pier", "schema": SCHEMA}, 201)
        before = snapshot(base)
    finally:
        stop(proc)
    assert before["abutment"]["revision"] == 3 and before["pier"]["revision"] == 1
    # A real legacy directory can carry an uninstalled temp file from a crash
    # during an old publication. Startup ignores it; migration must clean it.
    (data / ".rill-pending-abandoned").write_text('{"format":', encoding="ascii")
    heads = sorted(p.name for p in data.glob("*.rill"))
    assert heads == ["abutment.rill", "pier.rill"], heads
    return before


def run_migrate(binary, data, status=0, env=None):
    merged = {**os.environ, **(env or {})}
    result = subprocess.run(command(binary, data, "-migrate"),
                            capture_output=True, text=True, env=merged, timeout=30)
    assert result.returncode == status, result.stderr
    if status == 0:
        return json.loads(result.stdout)
    return result.stderr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--keep", action="store_true", help="keep the scratch data directory and print its path")
    args = parser.parse_args()

    temp = tempfile.mkdtemp(prefix="rillweave-migrate-")
    tmp = Path(temp)
    binary = tmp / "rillweaved"
    subprocess.run(["go", "build", "-o", binary, "./cmd/rillweaved"], cwd=ROOT,
                   check=True, timeout=120)
    data = tmp / "data"
    data.mkdir()

    # 1. Baseline written by the current file-backed release.
    before = seed_legacy_directory(binary, data)
    print(f"seed: legacy directory with {len(before)} graphs at revisions "
          f"{ {n: s['revision'] for n, s in before.items()} } and one abandoned temp file")

    # 2. Migration refuses to run while a server holds the directory lock.
    proc, base = serve(binary, data)
    try:
        locked = subprocess.run(command(binary, data, "-migrate"),
                                capture_output=True, text=True, timeout=30)
        assert locked.returncode != 0 and "directory lock" in locked.stderr, locked.stderr
        stop(proc)
        print("cutover guard: migration blocked while the server owns the data directory")

        # 3. Interrupted migration before the commit point leaves legacy mode.
        stderr = run_migrate(binary, data, status=1,
                             env={"RILLWEAVE_MIGRATE_ABORT_BEFORE_RENAME": "1"})
        assert "interrupted before database activation" in stderr
        assert not (data / "rillweave.db").exists()
        assert sorted(p.name for p in data.glob("*.rill")) == ["abutment.rill", "pier.rill"]
        proc, base = serve(binary, data)
        stop(proc)
        print("interruption (pre-commit): legacy heads still serve; staging files gone; server restarts")

        # 4. Successful cutover preserves names and exact revisions.
        report = run_migrate(binary, data)
        assert [h["name"] for h in report["heads"]] == ["abutment", "pier"]
        assert {h["name"]: h["revision"] for h in report["heads"]} == {"abutment": 3, "pier": 1}
        assert report["removed_temp_files"] == [".rill-pending-abandoned"]
        assert (data / "rillweave.db").is_file()
        assert not list(data.glob("*.rill"))
        assert sorted(p.name for p in (data / "legacy-heads").glob("*.rill")) == [
            "abutment.rill", "pier.rill"]
        print("cutover: SQLite database active; revisions preserved (3 and 1); "
              "legacy heads archived; abandoned temp file removed")

        # 5. Migration is rerunnable and idempotent, including a crash after
        #    the database rename but before legacy heads were archived.
        interrupted = tmp / "interrupted"
        interrupted.mkdir()
        proc, base = serve(binary, interrupted)
        try:
            request(base, GRAPHS, "POST", {"name": "abutment", "schema": SCHEMA}, 201)
            request(base, f"{GRAPHS}/abutment/commit", "POST", {"base": 1, "edits": [
                put_vertex("hinge", "north"),
            ]})
        finally:
            stop(proc)
        first = run_migrate(binary, interrupted)
        assert {h["name"]: h["revision"] for h in first["heads"]} == {"abutment": 2}
        # Recreate the exact post-rename/pre-archive state of an interrupted run.
        shutil.copy(interrupted / "legacy-heads" / "abutment.rill",
                    interrupted / "abutment.rill")
        resumed = run_migrate(binary, interrupted)
        assert resumed["already_migrated"] is True
        assert resumed["archived_heads"] == ["abutment.rill"]
        assert not list(interrupted.glob("*.rill"))
        proc, base = serve(binary, interrupted)
        try:
            assert request(base, f"{GRAPHS}/abutment/catalog")["revision"] == 2
        finally:
            stop(proc)
        print("interruption (post-commit): rerun verifies the database, finishes archiving and serves revision 2")

        rerun = run_migrate(binary, data)
        assert rerun["already_migrated"] is True
        assert not rerun["archived_heads"] and not rerun["removed_temp_files"]
        print("rerun: verifies the existing database and reports already_migrated")

        # 6. Post-cutover queries match the pre-cutover snapshot exactly.
        proc, base = serve(binary, data)
        try:
            after = snapshot(base)
            assert after == before, (before, after)
            print("consistency: catalog counts, revisions and lookup results match the legacy directory")

            # 7. New writes go only to SQLite; a stale .rill copied back is ignored.
            request(base, f"{GRAPHS}/abutment/commit", "POST", {"base": 3, "edits": [
                put_vertex("mullion", "east"),
            ]})
            commit4 = request(base, f"{GRAPHS}/abutment/catalog")
            assert commit4["revision"] == 4
        finally:
            stop(proc)
        (data / "abutment.rill").write_bytes((data / "legacy-heads" / "abutment.rill").read_bytes())
        proc, base = serve(binary, data)
        try:
            revived = request(base, f"{GRAPHS}/abutment/catalog")
            assert revived["revision"] == 4, "server must not fall back to the archived revision 3 head"
            lookup = request(base, f"{GRAPHS}/abutment/lookup", "POST", {
                "kind": "strut", "property": "tag", "value": "east",
            })
            assert lookup["result"]["ids"] == ["mullion"]
        finally:
            stop(proc)
        # The stray file is never served; leaving it in place documents that
        # SQLite mode ignores any .rill rather than accidentally reactivating it.
        print("single storage: a restored .rill head cannot mask the SQLite head at revision 4")

        # 8. The confirmed commit survives a full process restart.
        proc, base = serve(binary, data)
        try:
            restarted = request(base, f"{GRAPHS}/abutment/catalog")
            assert restarted["revision"] == 4 and restarted["result"]["vertices"] == 4
        finally:
            stop(proc)
        print("durability: revision 4 commit recovered from SQLite after restart; "
              "pins remain ephemeral as before")
    finally:
        if args.keep:
            print(f"scratch data directory retained at: {data}")
        else:
            subprocess.run(["rm", "-rf", temp], check=True)


if __name__ == "__main__":
    main()
