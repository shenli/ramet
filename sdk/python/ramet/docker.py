"""Same-host Docker execution. No Docker socket or credentials enter workloads.

SPDX-License-Identifier: Apache-2.0
"""
from __future__ import annotations

import math
import json
import os
import selectors
import subprocess
import time
import uuid
from datetime import datetime, timezone
from typing import Sequence

from .client import Execution, _lease_expiry
from .environment import DependencyEnvironment


def _require_local_daemon() -> None:
    context = subprocess.run(["docker", "context", "inspect", "--format",
                              "{{.Endpoints.docker.Host}}"], capture_output=True,
                             text=True, check=True, timeout=15).stdout.strip()
    endpoint = context if os.getenv("DOCKER_CONTEXT") else (os.getenv("DOCKER_HOST") or context)
    if not endpoint.startswith("unix://"):
        raise RuntimeError("DockerRuntime requires a local Unix-socket Docker daemon")


class DockerRuntime:
    """Run foreground commands using a local Docker daemon and copy workspaces.

    Pull the image beforehand. Containers have no network, a read-only root,
    no Linux capabilities, bounded resources, and only the execution bind mount.
    This adapter is not a security boundary against kernel/container escapes.
    Use an execution context manager so lease renewal remains active.
    """

    def __init__(self, image: str, *, cpus: float = 1, memory_mb: int = 256,
                 pids: int = 128, max_output_bytes: int = 1 << 20):
        if not image or image.startswith("-"):
            raise ValueError("provide a Docker image reference")
        if not math.isfinite(cpus) or cpus <= 0 or memory_mb < 16 or pids < 1:
            raise ValueError("resource limits must be positive (memory >= 16 MiB)")
        if type(max_output_bytes) is not int or max_output_bytes < 1:
            raise ValueError("max_output_bytes must be a positive integer")
        self.image, self.cpus, self.memory_mb, self.pids = image, cpus, memory_mb, pids
        self.max_output_bytes = max_output_bytes

    def dependency_environment(self) -> DependencyEnvironment:
        """Inspect the pre-pulled, digest-pinned image used by this adapter.

        Tags are rejected. The returned platform comes from the local Docker
        daemon's image, not the Python client's host architecture. Containers
        launched by this adapter always mount at /workspace.
        """
        DependencyEnvironment(self.image, "linux/unknown")  # Validate the pin before inspecting.
        _require_local_daemon()
        result = subprocess.run(["docker", "image", "inspect", self.image],
                                capture_output=True, text=True, check=True, timeout=15)
        info = json.loads(result.stdout)[0]
        platform = info["Os"] + "/" + info["Architecture"]
        if info.get("Variant"):
            platform += "/" + info["Variant"]
        return DependencyEnvironment(self.image, platform)

    def run(self, execution: Execution, args: Sequence[str], *, timeout: float = 60,
            check: bool = False) -> subprocess.CompletedProcess[str]:
        if isinstance(args, (str, bytes)) or not args:
            raise ValueError("pass a nonempty argument list")
        if not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("timeout must be positive and finite")
        if os.name != "posix":
            raise RuntimeError("DockerRuntime requires a POSIX client host")
        with execution._lock:
            if execution._heartbeat_thread is None:
                raise RuntimeError("use DockerRuntime inside 'with pool.acquire() as execution'")
            execution.stop_runtime()
            execution.heartbeat()
            path = execution.path.resolve(strict=True)
            if "," in str(path) or "\n" in str(path):
                raise ValueError("workspace path cannot contain commas or newlines")
            # A remote daemon would interpret worker paths on the wrong host.
            _require_local_daemon()
            name = "ramet-run-" + uuid.uuid4().hex

            def cleanup() -> None:
                removed = subprocess.run(["docker", "rm", "--force", name],
                                         capture_output=True, text=True, timeout=30)
                if removed.returncode and "No such container:" not in removed.stderr:
                    raise RuntimeError(f"container cleanup failed for {name}; "
                                       "restore Docker access and call execution.stop_runtime()")

            with execution.pool._runtime_lock:
                if execution.pool._closed or execution.pool._closing:
                    raise RuntimeError("pool is closing or closed")
                execution.pool._runtime_executions.add(execution)
                execution._runtime_cleanup = cleanup
            command = ["docker", "create", "--name", name, "--pull=never",
                       "--label", "ramet.runtime=python", "--network=none",
                       "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
                       "--user", f"{os.getuid()}:{os.getgid()}", "--cpus", str(self.cpus),
                       "--memory", f"{self.memory_mb}m", "--memory-swap", f"{self.memory_mb}m",
                       "--pids-limit", str(self.pids), "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
                       "--mount", f"type=bind,src={path},dst=/workspace",
                       "--workdir", "/workspace", "--env", "HOME=/tmp",
                       "--env", "PYTHONDONTWRITEBYTECODE=1", "--entrypoint", args[0],
                       self.image, *args[1:]]
            proc = None
            started = time.monotonic()
            try:
                subprocess.run(command, capture_output=True, text=True, check=True,
                               timeout=min(timeout, 30))
                proc = subprocess.Popen(["docker", "start", "--attach", name],
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                output = {"stdout": bytearray(), "stderr": bytearray()}
                output_bytes = 0
                with selectors.DefaultSelector() as streams:
                    streams.register(proc.stdout, selectors.EVENT_READ, "stdout")
                    streams.register(proc.stderr, selectors.EVENT_READ, "stderr")
                    while streams.get_map() or proc.poll() is None:
                        remaining = timeout - (time.monotonic() - started)
                        if remaining <= 0:
                            raise subprocess.TimeoutExpired(list(args), timeout)
                        if execution._heartbeat_error is not None:
                            raise RuntimeError("lease renewal failed; stopping workload") from execution._heartbeat_error
                        # A stalled renewal may hold the state lock. Inspect
                        # the last acknowledged expiry without waiting for it.
                        if _lease_expiry(execution._data["lease_expires_at"]) <= datetime.now(timezone.utc):
                            raise RuntimeError("lease expired; stopping workload")
                        for key, _ in streams.select(timeout=min(.2, remaining)):
                            chunk = os.read(key.fd, 65536)
                            if not chunk:
                                streams.unregister(key.fileobj)
                                continue
                            output_bytes += len(chunk)
                            if output_bytes > self.max_output_bytes:
                                raise RuntimeError("command output exceeded max_output_bytes; stopping workload")
                            output[key.data].extend(chunk)
                result = subprocess.CompletedProcess(list(args), proc.returncode,
                    output["stdout"].decode("utf-8", errors="replace"),
                    output["stderr"].decode("utf-8", errors="replace"))
                if check:
                    result.check_returncode()
                return result
            finally:
                try:
                    execution.stop_runtime()
                finally:
                    if proc is not None:
                        if proc.poll() is None:
                            proc.kill()
                        proc.communicate()
