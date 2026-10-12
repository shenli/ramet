"""Run with a same-host `ramet dev` worker and the Python SDK installed.

SPDX-License-Identifier: Apache-2.0
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from ramet import Ramet

parser = argparse.ArgumentParser()
parser.add_argument("source", type=Path, help="quiesced source directory outside the worker data directory")
parser.add_argument("--concurrency", type=int, default=8)
parser.add_argument("--attempts", type=int, default=64)
args = parser.parse_args()

store = Ramet()
base = store.snapshot(source=args.source.resolve())
with store.execution_pool(base, concurrency=args.concurrency) as pool:
    def evaluate(attempt: int) -> dict:
        with pool.acquire() as execution:
            marker = execution.path / ".ramet-eval-attempt"
            assert not marker.exists(), "contamination detected"
            marker.write_text(str(attempt))
            # Replace this write with your sandbox/environment integration.
            return {"attempt": attempt, "execution": execution.id}

    with ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        results = list(executor.map(evaluate, range(args.attempts)))
print(f"Completed {len(results)} isolated attempts from {base.id}")
