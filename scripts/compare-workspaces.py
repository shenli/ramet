#!/usr/bin/env python3
"""Compare preparation/reset of full copies, Git worktrees, and Ramet.

Requires a same-host Ramet worker and the Python SDK. This measures filesystem
state, not agent execution. Git worktrees share repository administration;
Ramet and full copies provide independent Git metadata.
SPDX-License-Identifier: Apache-2.0
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import platform
import shutil
import statistics
import subprocess
import time

from ramet import Ramet


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True, help="new directory")
    parser.add_argument("--files", type=int, default=128)
    parser.add_argument("--file-bytes", type=int, default=65536)
    parser.add_argument("--attempts", type=int, default=16)
    parser.add_argument("--concurrency", type=int, default=4)
    args = parser.parse_args()
    if min(args.files, args.file_bytes, args.attempts, args.concurrency) < 1:
        parser.error("sizes and concurrency must be positive")
    root = args.output.resolve()
    root.mkdir(parents=True, exist_ok=False)
    source = root / "source"
    source.mkdir()
    expected = {}
    for i in range(args.files):
        # Deterministic distinct content, without accidentally benchmarking zeros.
        payload = hashlib.shake_256(f"ramet-comparison-{i}".encode()).digest(args.file_bytes)
        name = f"file-{i:05d}"
        (source / name).write_bytes(payload)
        expected[name] = hashlib.sha256(payload).hexdigest()

    def git(*argv):
        return subprocess.run(["git", "-C", str(source), *argv], check=True,
                              capture_output=True, text=True).stdout.strip()
    git("init", "-q")
    git("add", ".")
    git("-c", "user.name=Ramet benchmark", "-c", "user.email=benchmark@example.invalid",
        "-c", "commit.gpgsign=false", "commit", "-qm", "Deterministic base")
    client = Ramet()
    start = time.perf_counter()
    base = client.snapshot(source)
    capture_seconds = time.perf_counter() - start

    def verify(path):
        observed = {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                    for p in path.iterdir() if p.name != ".git"}
        if observed != expected:
            raise RuntimeError("workspace contamination or missing data")

    def summary(values):
        ordered = sorted(values)
        return {"median": statistics.median(values), "max": max(values),
                "p95": ordered[max(0, (len(ordered) * 95 + 99) // 100 - 1)]}

    results = []
    try:
        for strategy in ("full-copy", "git-worktree", "ramet"):
            with client.execution_pool(base, concurrency=args.concurrency) as pool:
                def attempt(i):
                    path = root / f"{strategy}-{i}"
                    execution = None
                    def materialize():
                        if strategy == "full-copy":
                            shutil.copytree(source, path)
                        else:
                            git("worktree", "add", "--quiet", "--detach", str(path), "HEAD")
                    def remove():
                        if strategy == "full-copy":
                            shutil.rmtree(path)
                        else:
                            git("worktree", "remove", "--force", str(path))
                    try:
                        started = time.perf_counter()
                        if strategy == "ramet":
                            execution = pool.acquire().__enter__()
                            path = execution.path
                        else:
                            materialize()
                        ready = time.perf_counter() - started
                        verify(path)
                        (path / "file-00000").write_bytes(b"changed")
                        (path / "attempt").write_text(str(i))
                        started = time.perf_counter()
                        if execution:
                            execution.reset()
                            path = execution.path
                        else:
                            remove()
                            materialize()
                        reset = time.perf_counter() - started
                        verify(path)
                        return ready, reset
                    finally:
                        if execution:
                            execution.release()
                        elif path.exists():
                            remove()
                started = time.perf_counter()
                with ThreadPoolExecutor(max_workers=args.concurrency) as executor:
                    timings = list(executor.map(attempt, range(args.attempts)))
                elapsed = time.perf_counter() - started
                results.append({"strategy": strategy, "prepare_seconds": summary([x[0] for x in timings]),
                                "reset_seconds": summary([x[1] for x in timings]),
                                "wall_seconds": elapsed, "cycles_per_second": args.attempts / elapsed,
                                "verified_cycles": len(timings), "verification_failures": 0})
        report = {"platform": platform.platform(), "python": platform.python_version(),
                  "git": git("--version"), "logical_source_bytes": args.files * args.file_bytes,
                  "files": args.files, "attempts": args.attempts, "concurrency": args.concurrency,
                  "capture_seconds": capture_seconds, "worker": client.doctor(),
                  "results": results,
                  "limitations": ["Fixed strategy order; host page cache is not flushed. These are warm-host measurements.",
                                  "Git worktrees share repository metadata and do not provide independent Git state.",
                                  "Full copies use Python copytree; filesystem copy acceleration may apply.",
                                  "Wall time includes verification and cleanup; prepare/reset timers exclude the common verifier.",
                                  "No physical shared-extent, cold-cache, remote-I/O, or agent-runtime claim."]}
        (root / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(results, indent=2))
    finally:
        # Keep source and report, but remove the benchmark's durable snapshot.
        client._request("DELETE", "/v1/snapshots/" + base.id)


if __name__ == "__main__":
    main()
