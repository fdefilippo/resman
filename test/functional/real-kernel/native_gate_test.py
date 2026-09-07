#!/usr/bin/env python3
"""Unprivileged regression tests for the native evidence boundary."""
import json
import contextlib
import io
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from native_gate import Blocked, NativeGate, checks_pass, field


class NativeGateTests(unittest.TestCase):
    def test_unplaced_helper_is_cleaned_only_with_exact_recorded_identity(self):
        for valid in (True, False):
            with self.subTest(valid=valid), tempfile.TemporaryDirectory() as directory:
                gate = NativeGate(directory, "runit", "revision")
                gate.owned_host = True
                gate.accounts = [SimpleNamespace(pw_uid=1001)]
                gate.helper = Path(directory) / "helper"
                output = gate.helper / "1001"
                output.mkdir(parents=True)
                identity = {"pid": 2000000000, "start_time": "123", "children": []}
                (output / "identity.json").write_text(json.dumps(identity))
                proc_files = Path(directory) / "fake-proc"
                proc_files.mkdir()
                (proc_files / "cgroup").write_text("0::/system.slice/crond.service")
                (proc_files / "stat").write_text("1 (python3) " + " ".join(["0"] * 19 + ["123" if valid else "456"]))
                (proc_files / "cmdline").write_bytes(b"\0".join([
                    b"/usr/bin/python3", os.fsencode(gate.helper / "workload.py"), os.fsencode(output), b"",
                ]))

                class FakeProcess:
                    alive = True
                    def exists(self):
                        return self.alive
                    def stat(self):
                        return SimpleNamespace(st_uid=1001)
                    def __truediv__(self, name):
                        return proc_files / name

                proc = FakeProcess()
                def kill(pid, _signal):
                    self.assertEqual(pid, identity["pid"])
                    proc.alive = False

                with patch("native_gate.Path", side_effect=lambda p: proc if str(p) == "/proc/2000000000" else Path(p)), \
                        patch("native_gate.os.kill", side_effect=kill) as terminate, \
                        patch.object(gate, "command"), patch.object(gate, "stop_daemon"), patch.object(gate, "assert_released"):
                    if valid:
                        gate.cleanup()
                        terminate.assert_called_once()
                        self.assertFalse(proc.alive)
                    else:
                        with self.assertRaises(AssertionError):
                            gate.cleanup()
                        terminate.assert_not_called()
                        self.assertTrue(gate.helper.exists())

    def test_ordinary_live_stop_failure_prevents_the_crash_phase(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
            gate = NativeGate(directory, "runit", "revision")
            for name in ("preflight", "validate", "start_daemon", "start_sessions", "blackout",
                         "assert_applied", "resources", "cleanup", "stop_daemon"):
                stack.enter_context(patch.object(gate, name))
            crash = stack.enter_context(patch.object(gate, "restart"))
            stack.enter_context(patch.object(gate, "authority_split"))
            stack.enter_context(patch.object(gate, "assert_released", side_effect=RuntimeError("live restore incomplete")))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
            self.assertEqual(gate.run(), 1)
            crash.assert_not_called()
            self.assertEqual(gate.checks["graceful-stop"], "FAIL")

    def test_helper_remains_traversable_under_the_remote_private_umask(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = NativeGate(directory, "runit", "revision")
            gate.helper = Path(directory) / "helper"
            gate.cron = Path(directory) / "cron"
            (gate.bundle / "native-workload.py").write_text("# fixture\n")
            previous = os.umask(0o077)
            try:
                gate.start_sessions()
            finally:
                os.umask(previous)
            self.assertEqual(gate.helper.stat().st_mode & 0o777, 0o755)

    def test_every_mandatory_result_is_required(self):
        # Independent consumer list: deleting a producer key must fail this test.
        keys = (
            "full-budget-rejection", "pam-sessions", "blackout-suppression", "blackout-observation",
            "native-plan", "resource-properties", "authority-split", "crash-reclaim", "graceful-stop", "root-only-release",
        )
        complete = dict.fromkeys(keys, "PASS")
        self.assertTrue(checks_pass(complete))
        for key in keys:
            with self.subTest(key=key):
                missing = dict(complete)
                del missing[key]
                self.assertFalse(checks_pass(missing))
                for state in ("FAIL", "BLOCKED", "", None):
                    self.assertFalse(checks_pass(dict(complete, **{key: state})))
        self.assertFalse(checks_pass({}))
        self.assertFalse(checks_pass(dict(complete, unrelated="PASS")))

    def test_only_absence_uses_the_optional_fallback(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "value"
            self.assertEqual(field(path, "unavailable"), "unavailable")
            with patch.object(Path, "read_text", side_effect=PermissionError("denied")):
                with self.assertRaises(PermissionError):
                    field(path, "unavailable")

    def test_preflight_block_retains_the_result_and_never_runs_workloads(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = NativeGate(directory, "runit", "revision")
            with patch.object(gate, "preflight", side_effect=Blocked("active analyst")), \
                    patch.object(gate, "start_sessions") as sessions, contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(gate.run(), 77)
                sessions.assert_not_called()
            self.assertEqual((gate.evidence / "result").read_text(), "BLOCKED\n")
            self.assertIn("active analyst", (gate.evidence / "environment.txt").read_text())
            self.assertEqual(set(json.loads((gate.evidence / "checks.json").read_text()).values()), {"BLOCKED"})

    def test_cleanup_failure_cannot_leave_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = NativeGate(directory, "runit", "revision")
            with patch.object(gate, "preflight", side_effect=Blocked("preflight")), \
                    patch.object(gate, "cleanup", side_effect=RuntimeError("journal remains")), \
                    contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(gate.run(), 1)
            self.assertEqual((gate.evidence / "result").read_text(), "FAIL\n")
            self.assertIn("cleanup=FAIL", (gate.evidence / "environment.txt").read_text())


if __name__ == "__main__":
    unittest.main()
