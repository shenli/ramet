# SPDX-License-Identifier: Apache-2.0
from __future__ import annotations

import json
import os
import re
import signal
import subprocess
import threading
from datetime import datetime, timezone
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence
from http.client import HTTPException
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen


def _lease_expiry(value: str) -> datetime:
    """Parse Go's RFC3339Nano timestamps on every supported Python version."""
    match = re.fullmatch(
        r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})",
        value,
    )
    if match is None:
        raise ValueError("invalid lease expiration timestamp")
    # Python 3.10's fromisoformat accepts three or six fractional digits.
    # Truncation renews at most one microsecond early, never after expiry.
    fraction = (match[2] or "").ljust(6, "0")[:6]
    offset = "+00:00" if match[3] == "Z" else match[3]
    return datetime.fromisoformat(f"{match[1]}.{fraction}{offset}")


class RametError(RuntimeError):
    def __init__(self, status: int, code: str, message: str, *, request_id: str | None = None,
                 retry_after: str | None = None, operation_id: str | None = None):
        super().__init__(f"Ramet HTTP {status} ({code}): {message}")
        self.status = status
        self.code = code
        self.request_id = request_id
        self.retry_after = retry_after
        self.operation_id = operation_id


class OutcomeUnknownError(RuntimeError):
    """A keyed mutation may have completed; inspect or reuse its exact operation ID."""
    def __init__(self, operation_id: str, method: str, path: str):
        super().__init__(f"Operation {operation_id} outcome unknown; inspect its receipt or retry the identical request with the same ID")
        self.operation_id, self.method, self.path = operation_id, method, path


def _unresolved(error: BaseException | None) -> bool:
    return isinstance(error, OutcomeUnknownError) or (
        isinstance(error, RametError) and error.code in {
            "operation_pending", "operation_expired", "operation_session_changed",
            "operation_result_unavailable", "operation_conflict",
        })


class RebuildError(RuntimeError):
    def __init__(self, message: str, report: Mapping[str, Any]):
        super().__init__(message)
        self.report = dict(report)


@dataclass(frozen=True)
class Snapshot:
    id: str
    manifest: Mapping[str, Any]


class Ramet:
    """Client scoped to one worker or one tenant gateway credential.

    Paths in responses are worker-local. No operation is automatically retried;
    a network timeout can leave the outcome of a mutation unknown.
    """

    def __init__(self, url: str | None = None, token: str | None = None,
                 timeout: float = 125):
        self.url = (url or os.getenv("RAMET_URL", "http://127.0.0.1:7433")).rstrip("/")
        self.token = token if token is not None else os.getenv("RAMET_TOKEN", "")
        self.timeout = timeout

    def _request(self, method: str, path: str, body: Any = None, *, operation_id: str | None = None) -> Any:
        data = None if body is None else json.dumps(body).encode()
        headers = {"Accept": "application/json"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        if operation_id:
            headers["Idempotency-Key"] = operation_id
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        request = Request(self.url + path, data=data, headers=headers, method=method)
        try:
            with urlopen(request, timeout=self.timeout) as response:
                return None if response.status == 204 else json.load(response)
        except HTTPError as error:
            try:
                payload = json.loads(error.read(1 << 20))
                if not isinstance(payload, dict):
                    payload = {"code": "http_error", "error": error.reason}
            except (ValueError, UnicodeError):
                payload = {"code": "http_error", "error": error.reason}
            finally:
                error.close()
            raise RametError(error.code, payload.get("code", "http_error"),
                             payload.get("error", error.reason),
                             request_id=payload.get("request_id") or error.headers.get("X-Request-ID"),
                             retry_after=error.headers.get("Retry-After"), operation_id=operation_id) from error
        except (OSError, HTTPException, ValueError) as error:
            if operation_id and method in ("POST", "DELETE"):
                raise OutcomeUnknownError(operation_id, method, path) from error
            raise

    def new_operation(self) -> str:
        """Issue a one-hour, worker-session-scoped mutation ID. Persist before use."""
        return self._request("GET", "/v1/operations/new")["id"]

    def operation(self, operation_id: str) -> Mapping[str, Any]:
        return self._request("GET", "/v1/operations/" + quote(operation_id, safe=""))

    def capabilities(self) -> Mapping[str, Any]:
        return self._request("GET", "/v1/capabilities")

    def snapshot(self, source: str | Path | None = None, *,
                 repository: str | None = None, revision: str | None = None,
                 policy: Mapping[str, Any] | None = None,
                 operation_id: str | None = None) -> Snapshot:
        if (source is None) == (repository is None):
            raise ValueError("provide exactly one of source or repository")
        body: dict[str, Any] = {"policy": policy if policy is not None else {}}
        if repository is not None:
            if not revision:
                raise ValueError("an explicit Git revision is required")
            body["git"] = {"repository": repository, "revision": revision}
        else:
            body["source"] = str(source)
        return Snapshot(**self._request("POST", "/v1/snapshots", body, operation_id=operation_id))

    def snapshots(self) -> list[Snapshot]:
        return [Snapshot(**s) for s in self._request("GET", "/v1/snapshots")]

    def execution_pool(self, base: Snapshot | str, concurrency: int = 8, *,
                       lease_seconds: int = 900, operation_id: str | None = None) -> ExecutionPool:
        data = self._request("POST", "/v1/pools", {
            "base": base.id if isinstance(base, Snapshot) else base,
            "concurrency": concurrency, "lease_seconds": lease_seconds,
        }, operation_id=operation_id)
        return ExecutionPool(self, **data)

    def restore_plan(self, snapshot: Snapshot | str) -> Mapping[str, Any]:
        snapshot_id = snapshot.id if isinstance(snapshot, Snapshot) else snapshot
        return self._request("GET", "/v1/snapshots/" + quote(snapshot_id, safe="") + "/restore-plan")

    def locality(self, snapshot: Snapshot | str) -> Mapping[str, Any]:
        snapshot_id = snapshot.id if isinstance(snapshot, Snapshot) else snapshot
        return self._request("GET", "/v1/snapshots/" + quote(snapshot_id, safe="") + "/locality")

    def prefetch(self, snapshot: Snapshot | str) -> Mapping[str, Any]:
        snapshot_id = snapshot.id if isinstance(snapshot, Snapshot) else snapshot
        return self._request("POST", "/v1/snapshots/" + quote(snapshot_id, safe="") + "/prefetch")

    def cache_status(self) -> Mapping[str, Any]:
        return self._request("GET", "/v1/worker/cache")

    def trim_cache(self, target_bytes: int = 0) -> Mapping[str, Any]:
        return self._request("POST", "/v1/worker/cache/trim", {"target_bytes": target_bytes})

    def trim_idle(self) -> Mapping[str, Any]:
        return self._request("POST", "/v1/worker/idle/trim")

    def doctor(self) -> Mapping[str, Any]:
        return self._request("GET", "/v1/doctor")


class ExecutionPool:
    def __init__(self, client: Ramet, id: str, base: str, concurrency: int):
        self.client, self.id, self.base, self.concurrency = client, id, base, concurrency
        self._closed = False
        self._closing = False
        self._runtime_lock = threading.RLock()
        self._runtime_executions: set[Execution] = set()

    @property
    def _path(self) -> str:
        return "/v1/pools/" + quote(self.id, safe="")

    def acquire(self, count: int | None = None, *, operation_id: str | None = None) -> Execution | list[Execution]:
        if self._closed:
            raise RuntimeError("pool is closed")
        if count is not None:
            return [Execution(self, data) for data in self.client._request(
                "POST", self._path + "/acquire-many", {"count": count}, operation_id=operation_id)]
        return Execution(self, self.client._request("POST", self._path + "/acquire", operation_id=operation_id))

    def warm(self, count: int) -> Mapping[str, Any]:
        if self._closed:
            raise RuntimeError("pool is closed")
        return self.client._request("POST", self._path + "/warm", {"count": count})

    def reset(self, execution: Execution) -> Execution:
        self._check_owner(execution)
        return execution.reset()

    def release(self, execution: Execution) -> None:
        self._check_owner(execution)
        execution.release()

    def _check_owner(self, execution: Execution) -> None:
        if execution.pool is not self:
            raise ValueError("execution belongs to a different pool")

    def close(self) -> None:
        with self._runtime_lock:
            if self._closed:
                return
            self._closing = True
            running = list(self._runtime_executions)
        try:
            for execution in running:
                execution.stop_runtime()
            self.client._request("DELETE", self._path)
            self._closed = True
        finally:
            with self._runtime_lock:
                self._closing = False

    def __enter__(self) -> ExecutionPool:
        return self

    def __exit__(self, exc_type: Any, error: BaseException | None, traceback: Any) -> None:
        if _unresolved(error):
            # Preserve the recovery error; do not hide it with a close on an active pool.
            with self._runtime_lock:
                running = list(self._runtime_executions)
            for execution in running:
                execution.stop_runtime()
            return
        self.close()


class Execution:
    """One leased directory. Stop external writers before any lifecycle call.

    The lock serializes this handle's run/reset/release operations. It cannot
    fence arbitrary processes or other clients with a copied lease.
    """

    def __init__(self, pool: ExecutionPool, data: Mapping[str, Any]):
        self.pool = pool
        self._data = dict(data)
        self._closed = False
        self._lock = threading.RLock()
        self._state_lock = threading.RLock()
        self._heartbeat_stop = threading.Event()
        self._heartbeat_thread: threading.Thread | None = None
        self._heartbeat_error: Exception | None = None
        self._runtime_cleanup: Callable[[], None] | None = None

    def stop_runtime(self) -> None:
        """Retry runtime cleanup. Failed cleanup blocks lifecycle mutations."""
        with self._lock:
            if self._runtime_cleanup is not None:
                self._runtime_cleanup()
                self._runtime_cleanup = None
            with self.pool._runtime_lock:
                self.pool._runtime_executions.discard(self)

    @property
    def id(self) -> str:
        return self._data["id"]

    @property
    def path(self) -> Path:
        return Path(self._data["path"])

    def _action(self, name: str, *, operation_id: str | None = None) -> Any:
        with self._state_lock:
            if self._closed:
                raise RuntimeError("execution is released or discarded")
            path = self.pool._path + "/executions/" + quote(self.id, safe="") + "/" + name
            return self.pool.client._request("POST", path, {
                "lease": self._data["lease"], "generation": self._data["generation"],
            }, operation_id=operation_id)

    def heartbeat(self) -> Execution:
        """Renew this lease without changing its path or generation."""
        with self._state_lock:
            self._data = self._action("heartbeat")
            self._heartbeat_error = None
        return self

    def _keep_alive(self) -> None:
        while not self._heartbeat_stop.is_set():
            try:
                with self._state_lock:
                    expiry = _lease_expiry(self._data["lease_expires_at"])
            except (KeyError, TypeError, ValueError) as error:
                self._heartbeat_error = error
                return
            remaining = (expiry - datetime.now(timezone.utc)).total_seconds()
            if self._heartbeat_stop.wait(max(.1, min(30., remaining / 3))):
                return
            try:
                self.heartbeat()
            except Exception as error:
                self._heartbeat_error = error
                if isinstance(error, RametError) and error.status in (404, 409):
                    return
                # Renewals are safe to retry. Other mutations are never retried.

    def _stop_keep_alive(self) -> None:
        self._heartbeat_stop.set()
        if self._heartbeat_thread is not None:
            self._heartbeat_thread.join()
            self._heartbeat_thread = None

    def reset(self, *, operation_id: str | None = None) -> Execution:
        with self._lock, self._state_lock:
            self.stop_runtime()
            self._data = self._action("reset", operation_id=operation_id)
            if operation_id is not None:
                # A receipt is historical: confirm its lease is still current.
                self.heartbeat()
                if self._heartbeat_thread is not None and not self._heartbeat_thread.is_alive():
                    self._heartbeat_stop.clear()
                    self._heartbeat_thread = threading.Thread(target=self._keep_alive, name="ramet-lease", daemon=True)
                    self._heartbeat_thread.start()
        return self

    def fork(self, *, operation_id: str | None = None) -> Execution:
        with self._lock:
            self.stop_runtime()
            return Execution(self.pool, self._action("fork", operation_id=operation_id))

    def verify(self) -> Mapping[str, Any]:
        with self._lock:
            return self._action("verify")

    def checkpoint(self, *, operation_id: str | None = None) -> Snapshot:
        with self._lock:
            self.stop_runtime()
            return Snapshot(**self._action("checkpoint", operation_id=operation_id))

    def preview_promotion(self) -> Mapping[str, Any]:
        """Inspect a quiesced execution; publishes no snapshot or content."""
        with self._lock:
            self.stop_runtime()
            return self._action("preview-promotion")

    def restore_plan(self) -> Mapping[str, Any]:
        """Check declared recipe inputs; does not execute reconstruction."""
        with self._lock:
            self.stop_runtime()
            return self._action("restore-plan")

    def rebuild(self, handlers: Mapping[str, Callable[[Execution, Mapping[str, Any]], Any]], *,
                verify: Callable[[Execution], bool]) -> Mapping[str, Any]:
        """Execute explicitly approved step IDs using caller-owned runtime callbacks.

        Metadata recipe strings are never interpreted as commands. Callbacks
        must stop writers before returning. Readiness is a point-in-time result
        of the caller's verifier, not a durable flag or a determinism guarantee.
        """
        with self._lock:
            plan = self.restore_plan()
            report: dict[str, Any] = {"snapshot": plan["snapshot"], "ready": False,
                                      "completed": [], "failed_step": None}
            if not plan["valid"]:
                raise RebuildError("recipe inputs do not match the restore plan", report)
            missing = [step["id"] for step in plan["steps"] if step["id"] not in handlers]
            if missing:
                raise RebuildError("no approved handler for steps: " + ", ".join(missing), report)
            for step in plan["steps"]:
                report["failed_step"] = step["id"]
                try:
                    self.heartbeat()
                    if not self.restore_plan()["valid"]:
                        raise RuntimeError("recipe inputs changed during reconstruction")
                    result = handlers[step["id"]](self, step)
                    self.stop_runtime()
                    if result is False or (hasattr(result, "returncode") and result.returncode != 0):
                        raise RuntimeError("reconstruction callback reported failure")
                except Exception as error:
                    raise RebuildError("reconstruction failed for " + step["path"], report) from error
                report["completed"].append(step["id"])
            report["failed_step"] = None
            try:
                self.stop_runtime()
                self.heartbeat()
                if not self.restore_plan()["valid"]:
                    raise RuntimeError("reconstruction changed declared inputs")
                if verify(self) is not True:
                    raise RuntimeError("workspace readiness verification failed")
                self.stop_runtime()
                self.heartbeat()
                if not self.restore_plan()["valid"]:
                    raise RuntimeError("verification changed declared inputs")
            except Exception as error:
                raise RebuildError("workspace is not ready", report) from error
            report["ready"] = True
            return report

    def promote(self, *, operation_id: str | None = None) -> Snapshot:
        with self._lock:
            self.stop_runtime()
            return Snapshot(**self._action("promote", operation_id=operation_id))

    def release(self, *, operation_id: str | None = None) -> None:
        with self._lock:
            self.stop_runtime()
            self._stop_keep_alive()
            with self._state_lock:
                if not self._closed:
                    self._action("release", operation_id=operation_id)
                    self._closed = True

    def discard(self, *, operation_id: str | None = None) -> None:
        with self._lock:
            self.stop_runtime()
            self._stop_keep_alive()
            with self._state_lock:
                if not self._closed:
                    self._action("discard", operation_id=operation_id)
                    self._closed = True

    def run(self, args: Sequence[str], *, timeout: float | None = None,
            check: bool = False, env: Mapping[str, str] | None = None) -> subprocess.CompletedProcess[str]:
        """Run a trusted foreground command on the SAME HOST as the worker.

        This is a convenience for local experiments, not a sandbox. For agent
        code use your container/VM runtime with execution.path as its workspace.
        POSIX process groups are killed on exit/timeout to stop ordinary children.
        """
        if isinstance(args, (str, bytes)) or not args:
            raise ValueError("pass a nonempty argument list, not a shell string")
        if os.name != "posix":
            raise RuntimeError("local run requires a POSIX host")
        with self._lock:
            if self._closed:
                raise RuntimeError("execution is released or discarded")
            self.stop_runtime()
            proc = subprocess.Popen(list(args), cwd=self.path, env=env,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                    text=True, start_new_session=True)
            try:
                stdout, stderr = proc.communicate(timeout=timeout)
            finally:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                proc.communicate()
            result = subprocess.CompletedProcess(list(args), proc.returncode, stdout, stderr)
            if check:
                result.check_returncode()
            return result

    def __enter__(self) -> Execution:
        with self._lock:
            self.heartbeat()
            if self._heartbeat_thread is None:
                self._heartbeat_stop.clear()
                self._heartbeat_thread = threading.Thread(
                    target=self._keep_alive, name="ramet-lease", daemon=True)
                self._heartbeat_thread.start()
        return self

    def __exit__(self, exc_type: Any, error: BaseException | None, traceback: Any) -> None:
        if _unresolved(error):
            self.stop_runtime()
            self._stop_keep_alive()
            return
        self.release()


class RametEnv:
    """Small framework-neutral reset / step / state / close adapter.

    The harness supplies observation and action functions; reward computation
    and sandbox execution stay in the application or its runtime integration.
    """

    def __init__(self, pool: ExecutionPool, observe: Callable[[Execution], Any],
                 act: Callable[[Execution, Any], Any]):
        self.pool, self.observe, self.act = pool, observe, act
        self.execution: Execution | None = None

    def reset(self) -> Any:
        if self.execution is None:
            self.execution = self.pool.acquire().__enter__()
        else:
            self.execution.reset()
        return self.state()

    def state(self) -> Any:
        if self.execution is None:
            raise RuntimeError("call reset before reading state")
        return self.observe(self.execution)

    def step(self, action: Any) -> Any:
        if self.execution is None:
            raise RuntimeError("call reset before step")
        return self.act(self.execution, action)

    def close(self) -> None:
        if self.execution is not None:
            self.execution.release()
            self.execution = None

    def __enter__(self) -> RametEnv:
        return self

    def __exit__(self, *_: Any) -> None:
        self.close()
