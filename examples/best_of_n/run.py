"""Try independent fixes, test in Docker, promote and resume the winner.

The candidates are deterministic fixtures, not model-generated output.
SPDX-License-Identifier: Apache-2.0
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import shutil
import subprocess
import time

from ramet import DockerRuntime, Ramet


CANDIDATES = {
    "clamp-only": """def total_cents(items, discount_cents=0):
    subtotal = sum(items)
    return max(0, subtotal - discount_cents) + (500 if subtotal < 5000 else 0)
""",
    "discount-first": """def total_cents(items, discount_cents=0):
    net = max(0, sum(items) - discount_cents)
    return net + (500 if net < 5000 else 0)
""",
    "zero-order-aware": """def total_cents(items, discount_cents=0):
    net = max(0, sum(items) - discount_cents)
    return net + (500 if 0 < net < 5000 else 0)
""",
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/ramet"))
    parser.add_argument("--image", default="python:3.12-slim-bookworm")
    parser.add_argument("--output", type=Path, required=True,
                        help="new directory for report, patch, logs, and persistent worker state")
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    source = output / "source"
    shutil.copytree(Path(__file__).parent / "fixture", source,
                    ignore=shutil.ignore_patterns("__pycache__", "*.pyc"))
    def git(*argv):
        return subprocess.run(["git", *argv], cwd=source, check=True,
                              capture_output=True, text=True).stdout
    git("init", "-q")
    git("add", ".")
    git("-c", "user.name=Ramet demo", "-c", "user.email=demo@example.invalid",
        "-c", "commit.gpgsign=false", "commit", "-qm", "Checkout task")
    runtime = DockerRuntime(args.image)
    with (output / "worker.log").open("w+") as log:
        worker = subprocess.Popen([str(binary), "serve", "--data", str(output / "store"),
                                   "--listen", "127.0.0.1:0"],
                                  env={**os.environ, "RAMET_TOKEN": ""},
                                  stdout=subprocess.DEVNULL, stderr=log)
        try:
            client = None
            for _ in range(200):
                log.seek(0)
                for line in log:
                    try:
                        entry = json.loads(line)
                    except ValueError:
                        continue
                    if entry.get("address"):
                        client = Ramet("http://" + entry["address"], token="")
                        break
                if client is not None:
                    break
                if worker.poll() is not None:
                    raise RuntimeError("worker exited; see worker.log")
                time.sleep(.05)
            if client is None:
                raise RuntimeError("worker startup timed out")
            started = time.perf_counter()
            base = client.snapshot(source)
            baseline = (source / "checkout.py").read_text()
            with client.execution_pool(base, concurrency=3) as pool:
                def attempt(item):
                    name, candidate = item
                    t = time.perf_counter()
                    with pool.acquire() as execution:
                        ready = time.perf_counter() - t
                        assert (execution.path / "checkout.py").read_text() == baseline
                        (execution.path / "checkout.py").write_text(candidate)
                        result = runtime.run(execution, ["python", "-m", "unittest", "-v"])
                        (output / (name + ".log")).write_text(result.stdout + result.stderr)
                        selected, patch = None, None
                        if result.returncode == 0:
                            patch = execution.run(["git", "diff", "--binary", "HEAD", "--",
                                                   "checkout.py"], check=True).stdout
                            selected = execution.promote()
                        return {"candidate": name, "passed": result.returncode == 0,
                                "ready_seconds": ready}, selected, patch
                with ThreadPoolExecutor(max_workers=3) as executor:
                    outcomes = list(executor.map(attempt, CANDIDATES.items()))
                winners = [(row, snapshot, patch) for row, snapshot, patch in outcomes if snapshot]
                assert len(winners) == 1, "expected exactly one passing candidate"
                t = time.perf_counter()
                with pool.acquire() as clean:
                    assert clean.verify()["valid"], "recycled state was contaminated"
                    assert (clean.path / "checkout.py").read_text() == baseline
                recycle = time.perf_counter() - t
            row, selected, patch = winners[0]
            (output / "winner.patch").write_text(patch)
            with client.execution_pool(selected, concurrency=1) as resumed:
                with resumed.acquire() as execution:
                    runtime.run(execution, ["python", "-m", "unittest", "-v"], check=True)
            report = {"mode": "deterministic candidates; real container tests",
                      "base": base.id, "selected": selected.id, "winner": row["candidate"],
                      "attempts": [result[0] for result in outcomes], "recycle_verified": True,
                      "resume_verified": True, "recycle_seconds": recycle,
                      "total_seconds": time.perf_counter() - started,
                      "cache": client.cache_status(), "patch": "winner.patch"}
            (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
            for item, _, _ in outcomes:
                print(f"{'PASS' if item['passed'] else 'FAIL'}  {item['candidate']}")
            print(f"Selected {row['candidate']}; clean recycle and resumed tests verified.")
            print(f"Patch, report, and logs: {output}")
        finally:
            worker.terminate()
            worker.wait(timeout=30)


if __name__ == "__main__":
    main()
