# SPDX-License-Identifier: Apache-2.0
import unittest
from unittest.mock import Mock
from datetime import datetime, timedelta, timezone

from ramet.client import Execution, ExecutionPool, _lease_expiry
from ramet import DockerRuntime, RebuildError, OutcomeUnknownError


class LeaseTimestampTests(unittest.TestCase):
    def test_fractional_precision_and_offsets(self):
        for digits in range(10):
            fraction = "123456789"[:digits]
            micros = int(fraction.ljust(6, "0")[:6])
            for suffix, offset in (("Z", 0), ("+05:30", 330), ("-07:00", -420)):
                with self.subTest(digits=digits, suffix=suffix):
                    stamp = "2026-10-09T07:27:31" + ("." + fraction if fraction else "") + suffix
                    self.assertEqual(_lease_expiry(stamp), datetime(
                        2026, 10, 9, 7, 27, 31, micros,
                        timezone(timedelta(minutes=offset))))

    def test_malformed_expiration_is_recorded(self):
        for value in ("invalid", "2026-10-09T07:27:31", "2026-10-09T07:27:31.1234567890Z", None):
            with self.subTest(value=value):
                execution = Execution(None, {"lease_expires_at": value})
                execution._keep_alive()
                self.assertIsInstance(execution._heartbeat_error, (TypeError, ValueError))


class RuntimeSafetyTests(unittest.TestCase):
    def test_cleanup_failure_blocks_lifecycle_and_pool_deletion(self):
        client = Mock()
        pool = ExecutionPool(client, "pool", "base", 1)
        execution = Execution(pool, {"id": "execution", "lease": "lease", "generation": 1})
        cleanup = Mock(side_effect=RuntimeError("daemon unavailable"))
        execution._runtime_cleanup = cleanup
        pool._runtime_executions.add(execution)
        for action in (execution.reset, execution.fork, execution.promote,
                       execution.checkpoint, execution.preview_promotion, execution.restore_plan,
                       execution.release, execution.discard, pool.close):
            with self.subTest(action=action.__name__):
                with self.assertRaisesRegex(RuntimeError, "daemon unavailable"):
                    action()
                client._request.assert_not_called()
        cleanup.side_effect = None
        execution.stop_runtime()
        pool.close()
        client._request.assert_called_once_with("DELETE", "/v1/pools/pool")

    def test_unknown_operation_preserves_error_and_stops_runtime_on_exit(self):
        client = Mock()
        pool = ExecutionPool(client, "pool", "base", 1)
        execution = Execution(pool, {"id": "execution", "lease": "lease", "generation": 1})
        cleanup = Mock()
        execution._runtime_cleanup = cleanup
        pool._runtime_executions.add(execution)
        error = OutcomeUnknownError("operation", "POST", "/reset")
        execution.__exit__(type(error), error, None)
        pool.__exit__(type(error), error, None)
        cleanup.assert_called_once_with()
        client._request.assert_not_called()
        self.assertTrue(execution._heartbeat_stop.is_set())
        self.assertFalse(execution._closed)
        self.assertFalse(pool._closed)

    def test_requires_renewed_context_and_valid_limits(self):
        for limits in ({"cpus": 0}, {"cpus": float("nan")}, {"memory_mb": 0}, {"pids": 0},
                       {"max_output_bytes": 0}, {"max_output_bytes": 1.5}, {"max_output_bytes": True}):
            with self.assertRaises(ValueError):
                DockerRuntime("python:3.12", **limits)
        with self.assertRaisesRegex(RuntimeError, "pool.acquire"):
            DockerRuntime("python:3.12").run(Execution(None, {}), ["true"])


class RebuildTests(unittest.TestCase):
    def execution(self):
        execution = Execution(Mock(), {})
        execution.restore_plan = Mock(return_value={
            "snapshot": "snapshot", "valid": True,
            "steps": [{"id": "approved", "path": "deps", "recipe": "never execute me"}],
        })
        execution.heartbeat = Mock()
        execution.stop_runtime = Mock()
        return execution

    def test_failed_step_and_verifier_never_report_ready(self):
        for handler in (Mock(side_effect=RuntimeError("install failed")),
                        Mock(return_value=False), Mock(return_value=Mock(returncode=1))):
            execution = self.execution()
            verify = Mock(return_value=True)
            with self.assertRaises(RebuildError) as result:
                execution.rebuild({"approved": handler}, verify=verify)
            self.assertFalse(result.exception.report["ready"])
            self.assertEqual(result.exception.report["failed_step"], "approved")
            verify.assert_not_called()
        execution = self.execution()
        with self.assertRaises(RebuildError) as result:
            execution.rebuild({"approved": lambda *args: None}, verify=lambda e: False)
        self.assertEqual(result.exception.report["completed"], ["approved"])
        self.assertFalse(result.exception.report["ready"])

    def test_approval_is_required_for_every_step_before_any_execution(self):
        execution = self.execution()
        execution.restore_plan.return_value["steps"].append({"id": "unapproved", "path": "other"})
        handler = Mock()
        with self.assertRaises(RebuildError):
            execution.rebuild({"approved": handler}, verify=lambda e: True)
        handler.assert_not_called()

    def test_input_changes_during_rebuild_and_verification_fail_closed(self):
        for check in (2, 3):
            execution = self.execution()
            good = execution.restore_plan.return_value
            replies = [good.copy() for _ in range(4)]
            replies[check] = dict(good, valid=False)
            execution.restore_plan.side_effect = replies
            with self.assertRaises(RebuildError):
                execution.rebuild({"approved": lambda *args: None}, verify=lambda e: True)


if __name__ == "__main__":
    unittest.main()
