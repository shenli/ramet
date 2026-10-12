#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Reproducible Linux pressure soak; use a dedicated checkout and test providers."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import platform
import secrets
import subprocess
import tempfile
import threading
import time

from ramet import Ramet, RametError


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", type=Path, default=Path(".test-cache/ramet"))
    parser.add_argument("--duration", type=int, default=1800)
    parser.add_argument("--concurrency", type=int, default=8)
    parser.add_argument("--port", type=int, default=17443)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--skip-runtime", action="store_true")
    args = parser.parse_args()
    assert platform.system() == "Linux" and args.duration > 0 and args.concurrency >= 2
    provenance = {}
    for name in sorted(Path(".").rglob("*")):
        if any(part.startswith(".") for part in name.parts) or "third_party" in name.parts:
            continue
        if name.is_file() and (name.suffix in (".go", ".py") or name.name in ("go.mod", "go.sum")):
            provenance[str(name)] = hashlib.sha256(name.read_bytes()).hexdigest()
    binary = args.binary.resolve()
    result = {"platform": platform.platform(), "requested_seconds": args.duration,
              "concurrency": args.concurrency, "files": 64, "file_bytes": 65536,
              "cache_bytes_per_tenant": 1048576, "source_hashes": provenance,
              "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
              "samples": [], "errors": [], "runtime_isolation": "not_run"}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    Path(".test-cache").mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="phase2-soak-", dir=".test-cache") as temporary:
        root = Path(temporary).resolve()
        source = root / "source"
        source.mkdir()
        for i in range(64):
            (source / f"file-{i:02d}").write_bytes(bytes([i]) * 65536)
        env = dict(os.environ)
        env.setdefault("RAMET_DATABASE_URL", "postgres://ramet_test:ramet-local-integration-only@127.0.0.1:17432/ramet_phase1_test?sslmode=disable")
        env.setdefault("RAMET_S3_ACCESS_KEY", "ramet_test")
        env.setdefault("RAMET_S3_SECRET_KEY", "ramet-local-integration-only")
        tenant_configs = []
        clients = {}
        for backend in ("copy", "lazy"):
            token_env = "RAMET_SOAK_" + backend.upper()
            env[token_env] = secrets.token_hex(24)
            namespace = "soak-" + backend + "-" + secrets.token_hex(8)
            config = {"server": {"token_env": token_env, "max_inflight": 32},
                      "worker": {"data": str(root / backend), "backend": backend,
                                 "cache_bytes": 1048576, "max_executions": args.concurrency * 2,
                                 "max_idle_executions": args.concurrency, "idle_ttl": "30s",
                                 "min_free_disk_bytes": 1073741824, "source_roots": [str(source)]},
                      "metadata": {"provider": "postgres", "dsn_env": "RAMET_DATABASE_URL", "namespace": namespace},
                      "content": {"provider": "s3", "endpoint": "http://127.0.0.1:17900",
                                  "bucket": "ramet-phase1-integration", "prefix": namespace,
                                  "access_key_env": "RAMET_S3_ACCESS_KEY", "secret_key_env": "RAMET_S3_SECRET_KEY"}}
            name = root / (backend + ".yaml")
            name.write_text(json.dumps(config))
            tenant_configs.append({"id": backend, "config": str(name)})
            clients[backend] = Ramet(f"http://127.0.0.1:{args.port}", env[token_env])
        gateway = root / "gateway.yaml"
        gateway.write_text(json.dumps({"server": {"listen": f"127.0.0.1:{args.port}"}, "tenants": tenant_configs}))
        log_path = args.output.with_suffix(".worker.log")
        with log_path.open("w") as log:
            process = subprocess.Popen([str(binary), "serve", "--config", str(gateway)], env=env, stdout=log, stderr=log)
            try:
                for _ in range(120):
                    if process.poll() is not None:
                        raise RuntimeError("worker exited; see " + str(log_path))
                    try:
                        clients["copy"]._request("GET", "/ready")
                        break
                    except (OSError, RametError):
                        time.sleep(.25)
                else:
                    raise RuntimeError("worker did not become ready")
                bases = {backend: client.snapshot(source) for backend, client in clients.items()}
                pools = {backend: client.execution_pool(bases[backend], concurrency=args.concurrency * 2)
                         for backend, client in clients.items()}
                result["preparation"] = {}
                for backend, pool in pools.items():
                    start = time.monotonic()
                    e = pool.acquire()
                    cold = time.monotonic() - start
                    e.release()
                    pool.warm(min(2, args.concurrency))
                    start = time.monotonic()
                    e = pool.acquire()
                    warm = time.monotonic() - start
                    result["preparation"][backend] = {"cold_acquire_seconds": cold, "warm_acquire_seconds": warm}
                    if backend == "copy" and not args.skip_runtime:
                        check_runtime(e.path)
                        result["runtime_isolation"] = "passed: network none, cpu 0.5, memory 64MiB, pids 32, readonly root, no capabilities, no-new-privileges"
                    e.release()
                # A tenant cannot access a neighbor's pool even with an explicit tenant header.
                try:
                    clients["copy"]._request("GET", "/v1/pools/" + pools["lazy"].id)
                    raise AssertionError("cross-tenant pool access")
                except RametError as error:
                    assert error.status == 404
                stop = threading.Event()
                lock = threading.Lock()
                latencies = {backend: [] for backend in clients}
                counts = {backend: 0 for backend in clients}
                start = time.monotonic()
                deadline = start + args.duration

                def workload(index):
                    backend = ("copy", "lazy")[index % 2]
                    client, pool = clients[backend], pools[backend]
                    iteration = 0
                    while time.monotonic() < deadline and not stop.is_set():
                        began = time.monotonic()
                        try:
                            e = pool.acquire()
                            file_index = (iteration + index) % 64
                            name = Path(e.path) / f"file-{file_index:02d}"
                            assert name.read_bytes() == bytes([file_index]) * 65536
                            name.write_bytes(f"private:{index}:{iteration}".encode())
                            if iteration % 17 == 0:
                                child = e.fork()
                                assert (Path(child.path) / name.name).read_bytes() == name.read_bytes()
                                child.discard()
                            if iteration % 31 == 0:
                                snapshot = e.promote()
                                with client.execution_pool(snapshot, concurrency=1) as restored_pool:
                                    restored = restored_pool.acquire()
                                    assert (Path(restored.path) / name.name).read_bytes() == name.read_bytes()
                                    restored.discard()
                                client._request("DELETE", "/v1/snapshots/" + snapshot.id)
                            if iteration % 7 == 0:
                                e.reset()
                                assert (Path(e.path) / name.name).read_bytes() == bytes([file_index]) * 65536
                            e.release()
                            with lock:
                                counts[backend] += 1
                                latencies[backend].append(time.monotonic() - began)
                            iteration += 1
                        except Exception as error:
                            with lock:
                                result["errors"].append({"backend": backend, "error": repr(error)})
                            stop.set()

                with ThreadPoolExecutor(max_workers=args.concurrency) as executor:
                    futures = [executor.submit(workload, i) for i in range(args.concurrency)]
                    while time.monotonic() < deadline and not stop.is_set():
                        sample = sample_process(process.pid, root)
                        sample["seconds"] = time.monotonic() - start
                        sample["cycles"] = dict(counts)
                        sample["cache"] = {backend: client.cache_status() for backend, client in clients.items()}
                        for status in sample["cache"].values():
                            assert status["bytes"] <= status["limit_bytes"]
                            assert status["idle_executions"] <= status["max_idle_executions"]
                        result["samples"].append(sample)
                        for backend, client in clients.items():
                            # Concurrent pressure and collection: pinned pressure is an
                            # explicit admission result, not permission to delete a reader.
                            try:
                                client.trim_cache(0)
                            except RametError as error:
                                if error.status != 429:
                                    raise
                            client.prefetch(bases[backend])
                            client._request("POST", "/v1/gc", {"grace_seconds": 0, "dry_run": False})
                        print(json.dumps({"seconds": round(sample["seconds"], 1), "cycles": sample["cycles"], "rss_kib": sample["rss_kib"], "fds": sample["fds"]}), flush=True)
                        stop.wait(min(15, max(0, deadline - time.monotonic())))
                    for future in futures:
                        future.result()
                result["elapsed_seconds"] = time.monotonic() - start
                result["cycles"] = counts
                result["cycle_latency_seconds"] = {}
                for backend, values in latencies.items():
                    values.sort()
                    result["cycle_latency_seconds"][backend] = {"p50": percentile(values, .5), "p95": percentile(values, .95), "p99": percentile(values, .99)}
                    if result["errors"]:
                        continue
                    pools[backend].close()
                    assert clients[backend].doctor()["ok"]
                    result.setdefault("final_cache", {})[backend] = clients[backend].cache_status()
                    assert result["final_cache"][backend]["reserved_disk_bytes"] == 0
                    assert result["final_cache"][backend]["pinned_objects"] == 0
                result["samples"].append(dict(sample_process(process.pid, root), seconds=time.monotonic()-start, cycles=dict(counts)))
            finally:
                process.terminate()
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    result["errors"].append({"error": "worker required forced shutdown"})
                result["worker_exit_code"] = process.returncode
                args.output.write_text(json.dumps(result, indent=2) + "\n")
    assert not result["errors"], result["errors"]
    assert result["worker_exit_code"] == 0
    print("Phase 2 soak passed:", args.output, flush=True)


def percentile(values, fraction):
    return values[min(len(values)-1, int(len(values)*fraction))] if values else None


def sample_process(pid, root):
    status = Path(f"/proc/{pid}/status").read_text()
    fields = {line.split(":", 1)[0]: line.split(":", 1)[1].strip() for line in status.splitlines()}
    return {"rss_kib": int(fields["VmRSS"].split()[0]), "threads": int(fields["Threads"]),
            "fds": len(list(Path(f"/proc/{pid}/fd").iterdir())),
            "disk_kib": int(subprocess.run(["du", "-sk", "--exclude=root", str(root)], text=True, capture_output=True).stdout.split()[0])}


def check_runtime(path):
    command = """set -eu
test "$(cat /sys/fs/cgroup/memory.max)" = 67108864
test "$(cat /sys/fs/cgroup/pids.max)" = 32
test "$(cat /sys/fs/cgroup/cpu.max)" = '50000 100000'
grep -q '^NoNewPrivs:.*1' /proc/self/status
grep -q '^CapEff:.*0000000000000000' /proc/self/status
test "$(wc -l < /proc/net/route)" -eq 1
if touch /forbidden 2>/dev/null; then exit 1; fi
echo isolated > /work/runtime-isolation.txt
"""
    subprocess.run(["docker", "run", "--rm", "--network=none", "--memory=64m", "--cpus=0.5", "--pids-limit=32",
                    "--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only", "--user", f"{os.getuid()}:{os.getgid()}",
                    "--mount", f"type=bind,source={path},target=/work", "debian:bookworm-slim", "sh", "-ec", command], check=True, timeout=120)
    assert (Path(path) / "runtime-isolation.txt").read_text() == "isolated\n"


if __name__ == "__main__":
    main()
