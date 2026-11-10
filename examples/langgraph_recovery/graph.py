"""Two document tools with durable graph state and workspace publications.

SPDX-License-Identifier: Apache-2.0
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import sqlite3
from typing import TypedDict
from urllib.error import URLError

from langgraph.checkpoint.sqlite import SqliteSaver
from langgraph.graph import StateGraph, START, END
from ramet import DockerRuntime, Ramet, WorkspaceStep, prepare_dependencies


class State(TypedDict):
    scope: str
    snapshot: str
    environment: dict
    publication_id: str
    result: dict


class FaultClient(Ramet):
    """Test-only failure at the publication request/acknowledgment boundary."""
    def __init__(self, url, fault):
        super().__init__(url, token="")
        self.fault = fault

    def _request(self, method, path, body=None, **kwargs):
        publishing = method == "POST" and path.endswith("/publish")
        if publishing and self.fault == "before_publish":
            os.kill(os.getpid(), signal.SIGKILL)
        result = super()._request(method, path, body, **kwargs)
        if publishing and self.fault == "after_publish":
            os.kill(os.getpid(), signal.SIGKILL)
        if publishing and self.fault == "lost_reply":
            self.fault = ""
            raise URLError("injected lost publication reply")
        return result


def make_graph(client, runtime, checkpointer, attempts, *, interrupt_after=None):
    environment = runtime.dependency_environment()

    def prepare(execution):
        def rebuild(e):
            return runtime.run(e, ["python", "install.py"], timeout=120, check=True)
        def verify(e):
            return runtime.run(e, ["python", "task.py", "verify"]).returncode == 0
        prepare_dependencies(execution, environment, directory=".deps",
                             inputs=["requirements.lock", "install.py", "task.py"],
                             rebuild=rebuild, verify=verify)

    def node(stage, command, artifact):
        def run(state):
            if state["environment"] != environment.to_dict():
                raise RuntimeError("resume requires the recorded runtime identity")
            # These are approved application scripts captured in the base. A real
            # adapter should bind its tool implementation/version and all inputs.
            step = WorkspaceStep(state["scope"], stage, state["snapshot"],
                                 environment, {"command": command, "artifact": artifact},
                                 tool_version="document-session-v1")

            def tool(execution):
                # External test witness only: counts reruns, not application state.
                with attempts.open("a") as log:
                    log.write(stage + "\n")
                    log.flush()
                    os.fsync(log.fileno())
                runtime.run(execution, ["python", "task.py", command], check=True)
                digest = hashlib.sha256((execution.path / artifact).read_bytes()).hexdigest()
                return {"stage": stage, "artifact": artifact, "sha256": digest}

            return step.run(client, tool, prepare=prepare)
        return run

    builder = StateGraph(State)
    builder.add_node("draft", node("draft", "start", "reports/report.docx"))
    builder.add_node("continue_document", node("continue_document", "continue", "reports/continued.docx"))
    builder.add_edge(START, "draft")
    builder.add_edge("draft", "continue_document")
    builder.add_edge("continue_document", END)
    return builder.compile(checkpointer=checkpointer, interrupt_after=interrupt_after or [])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--state-dir", type=Path, required=True)
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--fault", default="", choices=["", "before_publish", "after_publish", "lost_reply", "after_graph"])
    args = parser.parse_args()
    initial = json.loads((args.state_dir / "initial.json").read_text())
    connection = sqlite3.connect(args.state_dir / "graph.sqlite", check_same_thread=False)
    connection.execute("PRAGMA journal_mode=WAL")
    connection.execute("PRAGMA synchronous=FULL")
    try:
        saver = SqliteSaver(connection)
        graph = make_graph(FaultClient(args.url, args.fault), DockerRuntime(args.image, memory_mb=512),
                           saver, args.state_dir / "attempts.log",
                           interrupt_after=["draft"] if args.fault == "after_graph" else None)
        config = {"configurable": {"thread_id": initial["scope"]}}
        result = graph.invoke(None if args.resume else initial, config, durability="sync")
        if args.fault == "after_graph":
            assert graph.get_state(config).next == ("continue_document",)
            os.kill(os.getpid(), signal.SIGKILL)
        assert result["result"]["stage"] == "continue_document"
        (args.state_dir / "result.json").write_text(json.dumps(result))
    finally:
        connection.close()


if __name__ == "__main__":
    main()
