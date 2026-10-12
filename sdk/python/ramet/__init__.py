"""Workspace state for parallel agents. Requires a running Ramet worker.

SPDX-License-Identifier: Apache-2.0
"""

from .client import Execution, ExecutionPool, Ramet, RametEnv, RametError, OutcomeUnknownError, RebuildError, Snapshot
from .docker import DockerRuntime

__all__ = ["DockerRuntime", "Execution", "ExecutionPool", "Ramet", "RametEnv", "RametError", "OutcomeUnknownError", "RebuildError", "Snapshot"]
__version__ = "0.3.0a1"
