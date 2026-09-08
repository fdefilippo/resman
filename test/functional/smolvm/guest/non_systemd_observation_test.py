#!/usr/bin/env python3
"""Unprivileged contract tests for the non-systemd observation fixture."""
import importlib.util
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch


spec = importlib.util.spec_from_file_location(
    "observation_fixture", Path(__file__).with_name("non-systemd-observation.py"))
observation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(observation)
spec = importlib.util.spec_from_file_location(
    "metadata_fixture", Path(__file__).with_name("evidence-metadata.py"))
metadata = importlib.util.module_from_spec(spec)
spec.loader.exec_module(metadata)
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "final"))
from gate import fields


class NonSystemdObservationFixtureTests(unittest.TestCase):
    def test_cleanup_targets_only_the_recorded_process_lifetime(self):
        workload = Mock(pid=123, returncode=-15)
        workload.poll.return_value = None
        workload.wait.return_value = -15
        with patch.object(observation, "birth", return_value="100"), \
                patch.object(observation.os, "getpgid", return_value=123), \
                patch.object(observation.os, "killpg") as kill:
            result = observation.stop_owned_workload(workload, "100")
        kill.assert_called_once_with(123, observation.signal.SIGTERM)
        self.assertFalse(result["forced_termination"])

    def test_cleanup_refuses_a_reused_process_lifetime(self):
        workload = Mock(pid=123)
        workload.poll.return_value = None
        with patch.object(observation, "birth", return_value="101"), \
                patch.object(observation.os, "killpg") as kill:
            with self.assertRaises(AssertionError):
                observation.stop_owned_workload(workload, "100")
        kill.assert_not_called()

    def test_real_guest_producer_and_host_finalizer_keep_one_value_per_field(self):
        copyfile = shutil.copyfile
        for guest_status, exit_code, host_cleanup, expected in (
                ("PASS", 0, "PASS", "PASS"), ("BLOCKED", 77, "PASS", "BLOCKED"),
                ("FAIL", 1, "PASS", "FAIL"), ("PASS", 0, "FAIL", "FAIL")):
            with self.subTest(guest=guest_status, cleanup=host_cleanup), \
                    tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                binary = root / "source-binary"
                binary.write_bytes(b"source-bound test binary")
                (root / "environment.txt").write_text(
                    "requested_scenario=non-systemd-observation\nsource_revision=" + "a" * 40 + "\n")
                with patch.object(metadata.shutil, "copyfile",
                                  side_effect=lambda _source, target: copyfile(binary, target)):
                    metadata.write_metadata(root, "runit", "a" * 40,
                                            "non-systemd-observation")
                observation.write_guest_outcome(root, guest_status,
                                                "measured guest outcome", "PASS")
                subprocess.run([sys.executable, str(Path(metadata.__file__)), "finalize",
                                str(root), host_cleanup, str(exit_code)], check=True)
                values = fields(root / "environment.txt")
                self.assertEqual(values["source_revision"], "a" * 40)
                self.assertEqual(values["scenario"], "non-systemd-observation")
                self.assertEqual(values["guest_result"], guest_status)
                self.assertEqual(values["result"], expected)

    def test_all_negative_contracts_are_named(self):
        self.assertEqual(observation.CHECKS, {
            "non-systemd-namespace", "observation-only-mode",
            "no-pid-relocation", "no-managed-hierarchy",
        })


if __name__ == "__main__":
    unittest.main()
