"""Fault-injection runner. All services and workload data must be disposable.

SPDX-License-Identifier: Apache-2.0
"""
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time
import uuid

from ramet import DockerRuntime, Ramet


@contextmanager
def worker(binary, data, config):
    data.mkdir(parents=True, exist_ok=True)
    with (data / "worker.log").open("w+") as log:
        command = [str(binary.resolve()), "serve", "--data", str(data), "--listen", "127.0.0.1:0"]
        if config:
            command += ["--config", str(config.resolve())]
        proc = subprocess.Popen(command, env={**os.environ, "RAMET_TOKEN": ""},
                                stdout=subprocess.DEVNULL, stderr=log)
        try:
            client = None
            for _ in range(200):
                log.seek(0)
                for line in log:
                    try:
                        event = json.loads(line)
                    except ValueError:
                        continue
                    if event.get("msg") == "Ramet worker ready":
                        client = Ramet("http://" + event["address"], token="")
                if client:
                    break
                if proc.poll() is not None:
                    raise RuntimeError("worker exited; inspect disposable provider configuration")
                time.sleep(.05)
            if client is None:
                raise RuntimeError("worker startup timed out")
            yield client
        finally:
            if proc.poll() is None:
                proc.kill()  # Deliberately no graceful worker shutdown.
            proc.wait(timeout=20)


def run_case(root, args, fault):
    root.mkdir()
    source = root / "project"
    source.mkdir()
    fixture = Path(__file__).resolve().parents[1] / "document_session"
    for name in ("requirements.lock", "install.py", "task.py"):
        shutil.copyfile(fixture / name, source / name)
    scope = uuid.uuid4().hex
    (source / "session-id.txt").write_text(scope)
    state_dir = root / "harness"
    state_dir.mkdir()
    environment = DockerRuntime(args.image).dependency_environment().to_dict()
    command = [sys.executable, str(Path(__file__).with_name("graph.py").resolve()),
               "--image", args.image, "--state-dir", str(state_dir)]
    start = time.monotonic()
    with worker(args.binary, root / "first-worker", args.worker_config) as client:
        base = client.snapshot(source)
        initial = {"scope": scope, "snapshot": base.id, "environment": environment}
        (state_dir / "initial.json").write_text(json.dumps(initial))
        with (root / "first.log").open("w") as log:
            process = subprocess.run(command + ["--url", client.url, "--fault", fault],
                                     stdout=log, stderr=log, timeout=240)
        expected = -signal.SIGKILL if fault in ("before_publish", "after_publish", "after_graph") else (1 if fault == "lost_reply" else 0)
        if process.returncode != expected:
            raise RuntimeError(f"{fault or 'baseline'}: unexpected child exit {process.returncode}; see {root / 'first.log'}")

    # The graph DB survives independently of the worker store. Provider mode
    # replaces the worker disk; local mode can only restart with its durable disk.
    data = root / ("replacement-worker" if args.worker_config else "first-worker")
    with worker(args.binary, data, args.worker_config) as client:
        with (root / "resume.log").open("w") as log:
            subprocess.run(command + ["--url", client.url, "--resume"],
                           stdout=log, stderr=log, timeout=240, check=True)
        state = json.loads((state_dir / "result.json").read_text())
        calls = (state_dir / "attempts.log").read_text().splitlines()
        assert calls.count("draft") == (2 if fault == "before_publish" else 1), calls
        assert calls.count("continue_document") == 1, calls
        publications = [s for s in client.snapshots() if s.manifest.get("publication")]
        # A shared provider namespace may contain earlier test cases: restrict
        # the verification to this case's ancestry.
        descendants = [s for s in publications if s.manifest["parent"] == base.id]
        assert len(descendants) == 1, descendants
        draft = descendants[0]
        final = client.publication(state["publication_id"])
        assert final.id == state["snapshot"]
        assert final.manifest["parent"] == draft.id
        with client.execution_pool(final, concurrency=1) as pool:
            with pool.acquire() as execution:
                for record in (draft.manifest["publication"]["result"], state["result"]):
                    actual = hashlib.sha256((execution.path / record["artifact"]).read_bytes()).hexdigest()
                    assert actual == record["sha256"], record
                DockerRuntime(args.image, memory_mb=512).run(
                    execution, ["python", "task.py", "continue"], check=True)
        assert client.doctor()["ok"]
    return {"fault": fault or "none", "passed": True, "draft_calls": calls.count("draft"),
            "continue_calls": calls.count("continue_document"),
            "seconds_including_restart": round(time.monotonic() - start, 3),
            "worker_storage": "empty_replacement" if args.worker_config else "retained_local"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="digest-pinned document_session image")
    parser.add_argument("--binary", type=Path, default=Path("bin/ramet"))
    parser.add_argument("--worker-config", type=Path)
    parser.add_argument("--output", type=Path, required=True, help="new directory for private logs and result JSON")
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    reports = []
    for fault in ("", "before_publish", "after_publish", "lost_reply", "after_graph"):
        reports.append(run_case(output / (fault or "baseline"), args, fault))
        print(json.dumps(reports[-1]), flush=True)
    (output / "results.json").write_text(json.dumps(reports, indent=2) + "\n")
    print("PASS: graph progress and workspace recovery across five crash/reply boundaries")


if __name__ == "__main__":
    main()
