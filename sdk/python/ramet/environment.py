"""Compatibility checks for workspace-local dependencies.

SPDX-License-Identifier: Apache-2.0
"""
from __future__ import annotations

from dataclasses import asdict, dataclass
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import tempfile
from typing import Callable, Sequence, TYPE_CHECKING, Any

if TYPE_CHECKING:
    from .client import Execution


@dataclass(frozen=True)
class DependencyEnvironment:
    """Caller-verified runtime identity, not an attestation of a running sandbox.

    Use a registry image digest for recovery on another host. A local Docker
    image ID is also accepted, but that image must be transferred separately.
    Include toolchain/configuration inputs in ``prepare_dependencies(inputs=)``.
    """

    image: str
    platform: str
    workspace_path: str = "/workspace"

    def __post_init__(self) -> None:
        if not isinstance(self.image, str) or not re.fullmatch(
                r"(?:[^\s@]+@)?sha256:[0-9a-f]{64}", self.image) or self.image.startswith("-"):
            raise ValueError("image must be pinned by a full SHA-256 digest")
        if not isinstance(self.platform, str) or not re.fullmatch(
                r"[a-z0-9_]+/[a-z0-9_]+(?:/[a-z0-9_]+)?", self.platform):
            raise ValueError("platform must be os/architecture[/variant]")
        path = PurePosixPath(self.workspace_path)
        if (not path.is_absolute() or str(path) != self.workspace_path
                or ".." in path.parts or self.workspace_path.startswith("//")):
            raise ValueError("workspace_path must be a normalized absolute POSIX path")

    def to_dict(self) -> dict[str, str]:
        return asdict(self)


def _relative(value: str) -> PurePosixPath:
    if not isinstance(value, str):
        raise ValueError("workspace paths must be strings")
    path = PurePosixPath(value)
    if (not value or path.is_absolute() or str(path) != value
            or any(part in ("..", ".git") for part in path.parts) or value == "."):
        raise ValueError("provide a normalized relative workspace path outside .git")
    return path


def _checked_path(root: Path, relative: PurePosixPath) -> Path:
    """Reject symlink traversal; writers must already have been stopped."""
    current = root
    for part in relative.parts:
        current = current / part
        if current.is_symlink():
            raise ValueError("dependency paths and inputs must not traverse symlinks")
    return current


def _regular(path: Path) -> bool:
    try:
        return stat.S_ISREG(path.lstat().st_mode)
    except FileNotFoundError:
        return False


def prepare_dependencies(execution: Execution, environment: DependencyEnvironment, *,
                         directory: str, inputs: Sequence[str],
                         rebuild: Callable[[Execution], Any],
                         verify: Callable[[Execution], bool]) -> dict[str, Any]:
    """Reuse compatible dependencies, or invoke the application's trusted builder.

    Requires same-host filesystem access and an active execution context.
    Stop external writers first. The builder owns ONLY ``directory`` and must
    replace stale dependencies there. Both callbacks must stop their writers
    before returning. A verifier must return exactly True to establish readiness.
    Metadata is a cache hint: never execute commands or select images from it.
    Keep the directory durable to reuse it after promotion.
    """
    dependency_path = _relative(directory)
    if isinstance(inputs, (str, bytes)) or not inputs:
        raise ValueError("declare at least one lockfile or build input")
    input_paths = [_relative(item) for item in inputs]
    if len(set(input_paths)) != len(input_paths):
        raise ValueError("dependency inputs must be unique")
    if any(path == dependency_path or dependency_path in path.parents for path in input_paths):
        raise ValueError("dependency inputs must live outside the dependency directory")
    if not isinstance(environment, DependencyEnvironment):
        raise TypeError("environment must be a DependencyEnvironment")

    with execution._lock:
        if execution._heartbeat_thread is None:
            raise RuntimeError("prepare dependencies inside 'with pool.acquire() as execution'")
        execution.stop_runtime()
        execution.heartbeat()
        root = execution.path.resolve(strict=True)
        marker_relative = dependency_path / ".ramet-environment.json"

        def marker_path() -> Path:
            return _checked_path(root, marker_relative)

        def identity() -> dict[str, Any]:
            digests = {}
            for relative in sorted(input_paths):
                path = _checked_path(root, relative)
                if not _regular(path):
                    raise ValueError("dependency inputs must be existing regular files: " + str(relative))
                digest = hashlib.sha256()
                with path.open("rb") as stream:
                    for block in iter(lambda: stream.read(1 << 20), b""):
                        digest.update(block)
                digests[str(relative)] = digest.hexdigest()
            return {"version": 1, "environment": environment.to_dict(),
                    "directory": directory, "inputs": digests}

        expected = identity()
        marker = marker_path()
        previous = None
        if marker.exists():
            if not _regular(marker):
                raise ValueError("dependency marker must be a regular file")
            if marker.stat().st_size <= 65536:
                try:
                    previous = json.loads(marker.read_text())
                except (ValueError, UnicodeError):
                    pass  # A malformed or old marker is a cache miss.
            # Failure must leave no reusable stamp, even if a callback raises.
            marker.unlink()

        def stable() -> None:
            execution.stop_runtime()
            execution.heartbeat()
            if identity() != expected:
                raise RuntimeError("dependency inputs changed during preparation")

        def ready() -> bool:
            try:
                result = verify(execution)
            finally:
                execution.stop_runtime()
            stable()
            return result is True

        reused = previous == expected and ready()
        if not reused:
            try:
                result = rebuild(execution)
            finally:
                execution.stop_runtime()
            if result is False or (hasattr(result, "returncode") and result.returncode != 0):
                raise RuntimeError("dependency rebuild reported failure")
            stable()
            if not ready():
                raise RuntimeError("dependency readiness verification failed")

        # The directory is created by the builder, never inferred from metadata.
        marker = marker_path()
        if not marker.parent.is_dir():
            raise RuntimeError("dependency builder did not create its directory")
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=marker.parent,
                                             prefix=".ramet-environment-", delete=False) as stream:
                temporary = Path(stream.name)
                json.dump(expected, stream, sort_keys=True)
                stream.write("\n")
            os.replace(temporary, marker)
        finally:
            if temporary is not None:
                temporary.unlink(missing_ok=True)
        return {"ready": True, "reused": reused, **expected}
