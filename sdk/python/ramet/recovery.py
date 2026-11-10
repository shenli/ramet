"""Framework-neutral tool-boundary publication; no workflow scheduler.

SPDX-License-Identifier: Apache-2.0
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import re
from typing import Any, Callable, Mapping

from .client import Execution, Ramet, Snapshot
from .environment import DependencyEnvironment


def _json(value: Any) -> bytes:
    try:
        encoded = json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False,
                             ensure_ascii=True).encode()
    except (TypeError, RecursionError) as error:
        raise ValueError("step identity must contain JSON-compatible values") from error
    # JSON otherwise silently coerces integer keys to strings. Different
    # application inputs must not resolve to the same recovery identity.
    pending = [value]
    while pending:
        item = pending.pop()
        if isinstance(item, dict):
            if any(not isinstance(key, str) for key in item):
                raise ValueError("step identity object keys must be strings")
            pending.extend(item.values())
        elif isinstance(item, (list, tuple)):
            pending.extend(item)
    return encoded


@dataclass(frozen=True)
class WorkspaceStep:
    """A stable tool invocation derived from a DURABLE harness checkpoint.

    Persist scope, step, base, inputs and tool_version before calling run.
    Scope includes the session AND branch; use a new scope for intentional forks.
    Callbacks may modify only their isolated workspace, with no external effects.
    """

    scope: str
    step: str
    base: str
    environment: DependencyEnvironment
    inputs: Mapping[str, Any]
    tool_version: str
    mode: str = "checkpoint"

    def identity(self) -> tuple[str, str]:
        for value in (self.scope, self.step, self.tool_version):
            if not isinstance(value, str) or not value or len(value.encode()) > 1024:
                raise ValueError("scope, step and tool_version must contain 1–1024 UTF-8 bytes")
        if not isinstance(self.base, str) or not re.fullmatch(r"[0-9a-f]{64}", self.base):
            raise ValueError("base must be a snapshot SHA-256")
        if self.mode not in ("checkpoint", "promotion"):
            raise ValueError("mode must be checkpoint or promotion")
        if not isinstance(self.inputs, Mapping):
            raise ValueError("inputs must be a JSON object")
        key = {"version": 1, "scope": self.scope, "step": self.step, "base": self.base}
        publication_id = hashlib.sha256(_json(key)).hexdigest()
        request = {"key": key, "environment": self.environment.to_dict(),
                   "inputs": dict(self.inputs), "tool_version": self.tool_version, "mode": self.mode}
        encoded = _json(request)
        if len(encoded) > 65536:
            raise ValueError("step identity exceeds 64 KiB")
        return publication_id, hashlib.sha256(encoded).hexdigest()

    def _result(self, snapshot: Snapshot) -> dict[str, Any]:
        publication_id, digest = self.identity()
        manifest = snapshot.manifest
        publication = manifest.get("publication", {})
        if (publication.get("id") != publication_id or publication.get("request_digest") != digest
                or manifest.get("parent") != self.base or manifest.get("save", {}).get("mode") != self.mode):
            raise RuntimeError("publication conflicts with the requested tool inputs or environment")
        return {"snapshot": snapshot.id, "publication_id": publication_id,
                "environment": self.environment.to_dict(), "result": publication["result"]}

    def recover(self, client: Ramet) -> dict[str, Any] | None:
        """Read-only resolution. A missing result does not fence a live publisher."""
        publication_id, _ = self.identity()
        snapshot = client.publication(publication_id)
        return None if snapshot is None else self._result(snapshot)

    def run(self, client: Ramet, tool: Callable[[Execution], Any], *,
            prepare: Callable[[Execution], None] | None = None) -> dict[str, Any]:
        """Resolve first; otherwise restore base, run, and durably publish.

        Return this record as the harness node's output ONLY after success.
        A crash may repeat uncommitted workspace work. It cannot safely repeat
        an email, payment, or other external effect. Runtime adapters still own
        fencing/cleanup, and the harness must serialize its session's execution.
        """
        publication_id, digest = self.identity()
        recovered = self.recover(client)
        if recovered is not None:
            return recovered
        with client.execution_pool(self.base, concurrency=1) as pool:
            with pool.acquire() as execution:
                if prepare is not None:
                    prepare(execution)
                    execution.stop_runtime()
                result = tool(execution)
                execution.stop_runtime()
                if self.identity() != (publication_id, digest):
                    raise RuntimeError("step inputs changed during execution")
                snapshot = execution.publish(publication_id, digest, result, mode=self.mode)
                return self._result(snapshot)
