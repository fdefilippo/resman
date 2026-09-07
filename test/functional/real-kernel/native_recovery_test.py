#!/usr/bin/env python3
"""Unprivileged negative tests for the real adapter recovery evidence boundary."""
import contextlib
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

from native_gate import Blocked
from native_recovery import RecoveryGate, checks_pass
import native_recovery


class RecoveryGateTests(unittest.TestCase):
    def test_command_line_uses_the_module_bundle_and_rejects_other_scenarios(self):
        with patch.object(native_recovery, "RecoveryGate") as constructor, patch.object(native_recovery.signal, "signal"):
            constructor.return_value.run.return_value = 77
            self.assertEqual(native_recovery.main(["systemd-native-recovery", "runit", "a" * 40]), 77)
            constructor.assert_called_once_with(Path(native_recovery.__file__).parent, "runit", "a" * 40)
            for arguments in ([], ["wrong", "runit", "a" * 40]):
                with self.assertRaises(AssertionError):
                    native_recovery.main(arguments)

    def test_subprocess_cancellation_runs_real_finally_and_retains_failed_evidence(self):
        code = '''
import signal, sys
from pathlib import Path
import native_recovery
class Probe(native_recovery.RecoveryGate):
    def __init__(self, bundle, run_id, revision):
        super().__init__(sys.argv[1], run_id, revision)
    def preflight(self):
        (self.evidence / "ready").write_text("ready")
        signal.pause()
    def cleanup(self):
        (self.evidence / "cleanup-ran").write_text("yes")
native_recovery.RecoveryGate = Probe
sys.exit(native_recovery.main(["systemd-native-recovery", "runit", "a" * 40]))
'''
        for sent in (signal.SIGTERM, signal.SIGINT):
            with self.subTest(signal=sent), tempfile.TemporaryDirectory() as directory:
                environment = dict(os.environ, PYTHONDONTWRITEBYTECODE="1", PYTHONPATH=str(Path(native_recovery.__file__).parent))
                process = subprocess.Popen([sys.executable, "-c", code, directory], env=environment,
                                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    ready = Path(directory) / "evidence/ready"
                    deadline = time.monotonic() + 5
                    while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                        time.sleep(0.01)
                    self.assertTrue(ready.exists(), "subprocess never reached the cancellable campaign")
                    process.send_signal(sent)
                    stdout, stderr = process.communicate(timeout=5)
                    self.assertEqual(process.returncode, 1, stdout + stderr)
                    self.assertTrue((ready.parent / "cleanup-ran").exists())
                    self.assertEqual((ready.parent / "result").read_text().strip(), "FAIL")
                    self.assertIn("recovery campaign interrupted", (ready.parent / "environment.txt").read_text())
                finally:
                    if process.poll() is None:
                        process.kill()
                    process.communicate(timeout=5)

    def test_every_recovery_boundary_is_mandatory(self):
        names = ("recovery-pam-session", "crash-before-reload", "automatic-crash-recovery",
                 "persistent-operator-conflict", "runtime-operator-conflict",
                 "recreated-unit-recovery", "exact-recovery-cleanup")
        complete = dict.fromkeys(names, "PASS")
        self.assertTrue(checks_pass(complete))
        for name in names:
            with self.subTest(name=name):
                self.assertFalse(checks_pass({key: value for key, value in complete.items() if key != name}))
                for value in ("FAIL", "BLOCKED", "", None):
                    self.assertFalse(checks_pass(dict(complete, **{name: value})))
        self.assertFalse(checks_pass(dict(complete, extra="PASS")))

    def test_failed_preflight_retains_blocked_evidence_without_starting_a_session(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = RecoveryGate(directory, "runit", "a" * 40)
            with patch.object(gate, "preflight", side_effect=Blocked("existing ownership")), \
                    patch.object(gate, "cleanup"), patch.object(gate, "start_sessions") as start, \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(gate.run(), 77)
            start.assert_not_called()
            self.assertEqual((gate.evidence / "result").read_text().strip(), "BLOCKED")
            self.assertTrue(all(value == "BLOCKED" for value in json.loads(
                (gate.evidence / "checks.json").read_text()).values()))

    def test_recovery_cannot_pass_without_the_real_stale_window(self):
        valid = {"phase": "reloading", "disk_footprint_absent": True,
                 "reload_executed": False, "cpu_weight_before_reload": 321,
                 "drop_in_paths_before_reload": ["/run/systemd/system.control/user-1006.slice.d/50-CPUWeight.conf"]}
        for key, value in (("phase", "applied"), ("disk_footprint_absent", False),
                           ("reload_executed", True), ("cpu_weight_before_reload", 100),
                           ("drop_in_paths_before_reload", [])):
            with self.subTest(key=key), tempfile.TemporaryDirectory() as directory:
                gate = RecoveryGate(directory, "runit", "a" * 40)
                with patch.object(gate, "operation", side_effect=[
                        {"cpu_weight": 321, "mutable_paths": ["owned"]}, dict(valid, **{key: value})]) as operation, \
                        patch.object(gate, "runtime_files", return_value={}):
                    with self.assertRaises(AssertionError):
                        gate.crash_recovery()
                self.assertEqual(operation.call_count, 2)
                self.assertNotIn("automatic-crash-recovery", gate.checks)

    def test_cleanup_preserves_an_operator_fixture_that_changed_externally(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = RecoveryGate(directory, "runit", "a" * 40)
            operator = Path(directory) / "operator.conf"
            operator.write_text("changed by somebody else\n")
            gate.operator_files[operator] = "0" * 64
            with self.assertRaises(AssertionError):
                gate.remove_operator_file(operator)
            self.assertTrue(operator.exists())
            self.assertIn(operator, gate.operator_files)


if __name__ == "__main__":
    unittest.main()
