"""Resume document work after worker and sandbox replacement.

SPDX-License-Identifier: Apache-2.0
"""
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

from ramet import DockerRuntime, Ramet, prepare_dependencies


@contextmanager
def worker(binary, data, config):
    data.mkdir(parents=True, exist_ok=True)
    with (data / "worker.log").open("w+") as log:
        command = [str(binary.resolve()), "serve", "--data", str(data), "--listen", "127.0.0.1:0"]
        if config:
            command += ["--config", str(config.resolve())]
        process = subprocess.Popen(command, env={**os.environ, "RAMET_TOKEN": ""},
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
                if process.poll() is not None:
                    raise RuntimeError("worker failed to start; check provider configuration")
                time.sleep(.05)
            if client is None:
                raise RuntimeError("worker startup timed out")
            yield client
        finally:
            process.terminate()
            process.wait(timeout=20)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="prebuilt image ID or registry digest; no tags")
    parser.add_argument("--binary", type=Path, default=Path("bin/ramet"))
    parser.add_argument("--worker-config", type=Path,
                        help="PostgreSQL/S3 worker config; use disposable providers and copy backend")
    args = parser.parse_args()
    runtime = DockerRuntime(args.image, memory_mb=512)
    environment = runtime.dependency_environment()
    installations = 0

    def rebuild(execution):
        nonlocal installations
        installations += 1
        # Trusted application code owns this directory. Do not execute a recipe
        # or choose an image supplied by untrusted snapshot metadata.
        return runtime.run(execution, ["python", "install.py"], timeout=120, check=True)

    def verify(execution):
        return runtime.run(execution, ["python", "task.py", "verify"]).returncode == 0

    def prepare(execution):
        return prepare_dependencies(execution, environment, directory=".deps",
                                    inputs=["requirements.lock", "install.py", "task.py"],
                                    rebuild=rebuild, verify=verify)

    with tempfile.TemporaryDirectory(prefix="ramet-document-session-") as directory:
        root = Path(directory).resolve()
        source = root / "project"
        source.mkdir()
        for name in ("requirements.lock", "install.py", "task.py"):
            shutil.copyfile(Path(__file__).with_name(name), source / name)
        with worker(args.binary, root / "first-worker", args.worker_config) as client:
            base = client.snapshot(source, policy={"rules": [
                {"path": ".deps", "class": "durable"},
                {"path": "reports", "class": "durable"},
                {"path": "scratch", "class": "ephemeral"},
            ]})
            with client.execution_pool(base, concurrency=1) as pool:
                with pool.acquire() as execution:
                    assert not prepare(execution)["reused"]
                    runtime.run(execution, ["python", "task.py", "start"], check=True)
                    original = hashlib.sha256((execution.path / "reports/report.docx").read_bytes()).hexdigest()
                    (execution.path / "scratch").mkdir()
                    (execution.path / "scratch/temp.txt").write_text("discard me")
                    saved = execution.promote()
                    # Your application's DB stores this AFTER publication. It is
                    # not an atomic transaction with the Ramet snapshot.
                    record = {"snapshot": saved.id, "environment": environment.to_dict(),
                              "task": "continue-document"}
                    (root / "session.json").write_text(json.dumps(record))

        # No old pool, lease, execution directory, or running container survives.
        # Remote providers permit an empty data directory. Local mode must retain
        # the original worker's durable store and proves restart, not disk loss.
        data = root / ("replacement-worker" if args.worker_config else "first-worker")
        with worker(args.binary, data, args.worker_config) as client:
            record = json.loads((root / "session.json").read_text())
            assert record["environment"] == runtime.dependency_environment().to_dict()
            with client.execution_pool(record["snapshot"], concurrency=1) as pool:
                with pool.acquire() as execution:
                    assert not (execution.path / "scratch").exists()
                    assert hashlib.sha256((execution.path / "reports/report.docx").read_bytes()).hexdigest() == original
                    assert prepare(execution)["reused"]
                    assert installations == 1, "compatible restore unexpectedly reinstalled dependencies"
                    runtime.run(execution, ["python", "task.py", "continue"], check=True)
                    print("PASS: replacement sandbox continues DOCX work without reinstalling")
                    # A conservative byte-level lockfile change forces rebuild.
                    with (execution.path / "requirements.lock").open("a") as lock:
                        lock.write("\n# New dependency configuration revision\n")
                    (execution.path / ".deps/stale-package.txt").write_text("must disappear")
                    assert not prepare(execution)["reused"]
                    assert installations == 2
                    assert not (execution.path / ".deps/stale-package.txt").exists()
                    assert hashlib.sha256((execution.path / "reports/report.docx").read_bytes()).hexdigest() == original
                    runtime.run(execution, ["python", "task.py", "continue"], check=True)
                    continued = execution.promote()
                    print("PASS: input change rebuilds dependencies and preserves the original document")
            with client.execution_pool(continued, concurrency=1) as pool:
                with pool.acquire() as execution:
                    assert prepare(execution)["reused"]
                    assert installations == 2
                    runtime.run(execution, ["python", "task.py", "continue"], check=True)
                    print("PASS: rebuilt dependencies and continued document survive another restore")
            assert client.doctor()["ok"]
        print("PASS: " + ("fresh worker directory with PostgreSQL/S3" if args.worker_config
                         else "worker restart with retained local storage"))


if __name__ == "__main__":
    main()
