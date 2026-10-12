#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Git-aware fan-out evidence against a colocated Ramet worker (no sandbox)."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import platform
import subprocess
import tempfile
import time
from urllib.request import Request, urlopen

from ramet import Ramet

parser = argparse.ArgumentParser()
parser.add_argument("--concurrency", type=int, default=1000)
parser.add_argument("--rounds", type=int, default=3)
parser.add_argument("--parallelism", type=int, default=32)
parser.add_argument("--output", type=Path, required=True)
args = parser.parse_args()
client = Ramet(timeout=125)


def metrics():
    request = Request(client.url + "/metrics", headers={"Authorization": "Bearer " + client.token})
    with urlopen(request, timeout=10) as response:
        return {line.rsplit(" ", 1)[0]: float(line.rsplit(" ", 1)[1])
                for line in response.read().decode().splitlines() if line.startswith("ramet_")}


with tempfile.TemporaryDirectory(prefix="ramet-fanout-", dir=os.getenv("RAMET_TEST_WORKDIR")) as temp:
    source = Path(temp)
    for index in range(8):
        (source / f"source-{index}.txt").write_text((f"immutable Git base {index}\n" * 1536))

    def git(*arguments):
        return subprocess.check_output(["git", "-C", str(source), "-c", "user.name=Ramet Test",
                                        "-c", "user.email=test@example.invalid", *arguments],
                                       stderr=subprocess.DEVNULL, text=True).strip()

    git("init", "--initial-branch=main")
    git("add", ".")
    git("commit", "-m", "fanout fixture")
    commit = git("rev-parse", "HEAD")
    base = client.snapshot(repository=str(source), revision=commit)
    baseline = metrics()
    rounds = []
    with client.execution_pool(base, concurrency=args.concurrency) as pool:
        for round_number in range(args.rounds):
            start = time.monotonic()
            executions = pool.acquire(count=args.concurrency)
            ready_seconds = time.monotonic() - start
            assert len({e.path for e in executions}) == args.concurrency
            assert len({e.id for e in executions}) == args.concurrency
            active = metrics()["ramet_active_executions"]
            assert active == args.concurrency, active

            def attempt(pair):
                index, execution = pair
                start = time.monotonic()
                marker = execution.path / "attempt.txt"
                assert not marker.exists(), "previous attempt leaked"
                assert execution.verify()["valid"], "base verification failed"
                assert (execution.path / "source-0.txt").read_text().startswith("immutable Git base 0\n")
                marker.write_text(f"round={round_number}, execution={index}\n")
                (execution.path / "source-0.txt").write_text(f"isolated mutation {index}\n")
                execution.release()
                return time.monotonic() - start

            with ThreadPoolExecutor(max_workers=args.parallelism) as executor:
                latencies = sorted(executor.map(attempt, enumerate(executions)))
            elapsed = time.monotonic() - start
            rounds.append({"round": round_number + 1, "concurrent_slots": int(active),
                           "ready_seconds": ready_seconds, "seconds": elapsed,
                           "executions_per_second": args.concurrency / elapsed,
                           "attempt_recycle_p95_seconds": latencies[int(.95 * (len(latencies) - 1))]})
            assert git("status", "--porcelain") == "", "source Git base changed"
            assert metrics()["ramet_remote_bytes_uploaded_total"] == baseline["ramet_remote_bytes_uploaded_total"]
    final = metrics()
    assert final["ramet_active_executions"] == final["ramet_idle_executions"] == 0
    assert final["ramet_verification_failures_total"] == baseline["ramet_verification_failures_total"]
    assert final["ramet_quarantines_total"] == baseline["ramet_quarantines_total"]
    report = {"platform": platform.platform(), "fixture_commit": commit,
              "fixture_logical_bytes": sum(e.get("size", 0) for e in base.manifest["entries"]),
              "fixture_files": len(base.manifest["entries"]), "rounds": rounds,
              "successful": args.concurrency * args.rounds, "parallelism": args.parallelism,
              "metrics_delta": {k: final[k] - v for k, v in baseline.items()},
              "doctor": client.doctor()}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
