# SPDX-License-Identifier: Apache-2.0
import json
import unittest
from dataclasses import replace
from unittest.mock import Mock
from urllib.error import URLError

from ramet import DependencyEnvironment, PublicationUnknownError, Snapshot, WorkspaceStep
from ramet.client import Execution, ExecutionPool, _unresolved


class WorkspaceStepTests(unittest.TestCase):
    def setUp(self):
        self.step = WorkspaceStep("thread/branch", "draft", "a" * 64,
            DependencyEnvironment("sha256:" + "b" * 64, "linux/amd64"),
            {"topic": "quarterly report"}, "draft-v1")
        self.client = Mock()
        self.records = {}
        self.client.publication.side_effect = self.records.get
        self.pool = Mock()
        self.client.execution_pool.return_value = self.pool
        self.pool.__enter__ = Mock(return_value=self.pool)
        self.pool.__exit__ = Mock(return_value=False)
        self.execution = Mock()
        self.execution.__enter__ = Mock(return_value=self.execution)
        self.execution.__exit__ = Mock(return_value=False)
        self.pool.acquire.return_value = self.execution
        def publish(publication_id, digest, result, mode):
            snap = Snapshot("c" * 64, {"parent": self.step.base, "save": {"mode": mode},
                "publication": {"id": publication_id, "request_digest": digest, "result": result}})
            self.records[publication_id] = snap
            return snap
        self.execution.publish.side_effect = publish

    def test_completed_publication_recovers_without_tool_or_pool(self):
        tool = Mock(return_value={"report": "reports/report.docx"})
        first = self.step.run(self.client, tool)
        self.assertEqual(first["snapshot"], "c" * 64)
        self.assertEqual(first["result"], {"report": "reports/report.docx"})
        self.client.execution_pool.reset_mock()
        self.assertEqual(self.step.run(self.client, tool), first)
        self.assertEqual(tool.call_count, 1)
        self.client.execution_pool.assert_not_called()

    def test_lost_reply_then_recover_uses_published_result(self):
        original = self.execution.publish.side_effect
        def lose_reply(*args, **kwargs):
            original(*args, **kwargs)
            raise PublicationUnknownError(args[0])
        self.execution.publish.side_effect = lose_reply
        tool = Mock(return_value={"done": True})
        with self.assertRaises(PublicationUnknownError):
            self.step.run(self.client, tool)
        self.assertTrue(self.step.run(self.client, tool)["result"]["done"])
        self.assertEqual(tool.call_count, 1)

    def test_changed_inputs_runtime_or_tool_version_conflict(self):
        self.step.run(self.client, lambda e: "done")
        tool = Mock()
        for changed in (
                replace(self.step, inputs={"topic": "another report"}),
                replace(self.step, tool_version="draft-v2"),
                replace(self.step, mode="promotion"),
                replace(self.step, environment=replace(self.step.environment, platform="linux/arm64"))):
            with self.assertRaisesRegex(RuntimeError, "conflicts"):
                changed.run(self.client, tool)
        tool.assert_not_called()
        self.assertNotEqual(replace(self.step, scope="another-branch").identity()[0], self.step.identity()[0])

    def test_inputs_changed_by_callback_never_publish(self):
        def tool(execution):
            self.step.inputs["topic"] = "mutated"
            return "wrong inputs"
        with self.assertRaisesRegex(RuntimeError, "inputs changed"):
            self.step.run(self.client, tool)
        self.execution.publish.assert_not_called()

    def test_tool_failure_and_lookup_outage_never_publish_or_advance(self):
        with self.assertRaisesRegex(RuntimeError, "tool failed"):
            self.step.run(self.client, Mock(side_effect=RuntimeError("tool failed")))
        self.execution.publish.assert_not_called()
        self.client.publication.side_effect = URLError("provider unavailable")
        tool = Mock()
        with self.assertRaises(URLError):
            self.step.run(self.client, tool)
        tool.assert_not_called()

    def test_invalid_identity_rejected_before_network(self):
        for changed in (replace(self.step, scope=""), replace(self.step, base="not a digest"),
                        replace(self.step, inputs={"bad": float("nan")}),
                        replace(self.step, base=None),
                        replace(self.step, inputs={"nested": {1: "coerced key"}}),
                        replace(self.step, inputs={"nested": {1: "integer", "1": "string"}}),
                        replace(self.step, inputs={"large": "x" * 65537})):
            with self.assertRaises(ValueError):
                changed.run(self.client, Mock())
        self.client.publication.assert_not_called()

    def test_sdk_unknown_publication_preserves_context_for_reconciliation(self):
        client = Mock()
        pool = ExecutionPool(client, "pool", "a" * 64, 1)
        execution = Execution(pool, {"id": "exec", "lease": "lease", "generation": 1})
        execution.stop_runtime = Mock()
        execution.heartbeat = Mock()
        client._request.side_effect = URLError("reply lost")
        with self.assertRaises(PublicationUnknownError) as caught:
            execution.publish("c" * 64, "d" * 64, {"done": True})
        self.assertTrue(_unresolved(caught.exception))
        self.assertEqual(caught.exception.publication_id, "c" * 64)
        self.assertEqual(client._request.call_count, 1)
        execution.__exit__(type(caught.exception), caught.exception, None)
        pool.__exit__(type(caught.exception), caught.exception, None)
        self.assertEqual(client._request.call_count, 1)
