"""Selective save and verified restore using trusted, generated fixture commands.

Run with PYTHONPATH=sdk/python python3 examples/persistence/run.py.
SPDX-License-Identifier: Apache-2.0
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

from ramet import Ramet, RebuildError


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/ramet"))
    parser.add_argument("--docker", action="store_true", help="reconstruct inside the included Docker adapter")
    parser.add_argument("--image", default="python:3.12-slim-bookworm", help="pre-pulled Docker image")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="ramet-persistence-") as directory:
        root = Path(directory)
        source = root / "project"
        source.mkdir()
        (source / "schema.json").write_text('{"message": "restored successfully"}\n')
        (source / "build.py").write_text(
            'import json\nfrom pathlib import Path\n'
            'Path("generated").mkdir(exist_ok=True)\n'
            'Path("generated/message.txt").write_text(json.loads(Path("schema.json").read_text())["message"])\n')
        (source / "generated").mkdir()
        (source / "generated/message.txt").write_text("restored successfully")
        (source / "scratch").mkdir()
        (source / "scratch/debug.log").write_text("temporary" * 100)
        with (root / "worker.log").open("w+") as log:
            worker = subprocess.Popen([str(args.binary.resolve()), "serve", "--data", str(root / "store"),
                                       "--listen", "127.0.0.1:0"],
                                      env=dict(os.environ, RAMET_TOKEN=""), stderr=log, stdout=subprocess.DEVNULL)
            try:
                ramet = None
                for _ in range(200):
                    log.seek(0)
                    for line in log:
                        try:
                            event = json.loads(line)
                        except ValueError:
                            continue
                        if event.get("msg") == "Ramet worker ready":
                            ramet = Ramet("http://" + event["address"], token="")
                    if ramet:
                        break
                    if worker.poll() is not None:
                        raise RuntimeError("worker exited; inspect its configuration")
                    time.sleep(.05)
                if ramet is None:
                    raise RuntimeError("worker did not start")
                runtime_identity = args.image if args.docker else "trusted-host-python-" + sys.version.split()[0]
                base = ramet.snapshot(source, policy={"rules": [
                    {"path": "generated", "class": "rebuildable", "recipe": "python build.py",
                     "inputs": ["schema.json", "build.py"], "runtime": runtime_identity},
                    {"path": "scratch", "class": "ephemeral"},
                ]})
                with ramet.execution_pool(base, concurrency=1) as pool:
                    with pool.acquire() as execution:
                        (execution.path / "selected.txt").write_text("useful agent work")
                        preview = execution.preview_promotion()
                        assert preview["valid"], preview["violations"]
                        for entry in preview["paths"]:
                            print(f'{entry["path"]:28} {entry["action"]:14} {entry["bytes"]:8} bytes')
                        saved = execution.promote()
                    with pool.acquire() as clean:
                        assert not (clean.path / "selected.txt").exists()
                plan = ramet.restore_plan(saved)
                step = plan["steps"][0]
                # Approve a known recipe and exact captured inputs. Never turn
                # arbitrary remote recipe strings into shell commands.
                assert step["recipe"] == "python build.py"
                assert step["runtime"] == runtime_identity
                expected_inputs = {"schema.json", "build.py"}
                assert {item["path"] for item in step["inputs"]} == expected_inputs
                for item in step["inputs"]:
                    assert item["sha256"] == hashlib.sha256((source / item["path"]).read_bytes()).hexdigest()
                if args.docker:
                    from ramet import DockerRuntime
                    runtime = DockerRuntime(args.image)
                    def rebuild(execution, step):
                        return runtime.run(execution, ["python", "build.py"], check=True)
                else:
                    def rebuild(execution, step):
                        return execution.run([sys.executable, "build.py"], check=True, timeout=30)
                def ready(execution):
                    return ((execution.path / "generated/message.txt").read_text() == "restored successfully"
                            and (execution.path / "selected.txt").read_text() == "useful agent work")
                with ramet.execution_pool(saved, concurrency=1) as pool:
                    with pool.acquire() as restored:
                        assert not (restored.path / "generated").exists()
                        assert not (restored.path / "scratch").exists()
                        report = restored.rebuild({step["id"]: rebuild}, verify=ready)
                        assert report["ready"]
                        print(json.dumps(report, indent=2))
                        (restored.path / "schema.json").write_text("changed after restore")
                        try:
                            restored.rebuild({step["id"]: rebuild}, verify=ready)
                        except RebuildError as error:
                            assert not error.report["ready"]
                            print("Changed recipe input correctly rejected")
                        else:
                            raise AssertionError("changed input accepted")
                assert not (source / "selected.txt").exists()
                print("PASS: selective save, clean recycle, verified reconstruction, input-drift rejection")
            finally:
                worker.terminate()
                worker.wait(timeout=20)


if __name__ == "__main__":
    main()
