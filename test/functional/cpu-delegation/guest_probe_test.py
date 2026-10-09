#!/usr/bin/env python3
"""Tests for the pure parts of the cpu-delegation guest probe."""
import errno
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


path = Path(__file__).with_name("guest_probe.py")
spec = importlib.util.spec_from_file_location("guest_probe", path)
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


PS_OUTPUT = """\
    1 TS       0 0::/init.scope
  900 TS       0 0::/system.slice/sshd.service
10244 FF      97 0::/system.slice/vcs.service
10255 FF      97 0::/system.slice/vcs.service
   17 FF      99 0::/
  120 RR      50 0::/system.slice/other.service
"""


class RealtimeInventoryTests(unittest.TestCase):
    def test_only_realtime_threads_are_selected(self):
        tasks = probe.parse_realtime_tasks(PS_OUTPUT)
        self.assertEqual([task["tid"] for task in tasks["all"]], [10244, 10255, 17, 120])

    def test_kernel_threads_in_the_root_are_not_counted_as_outside(self):
        tasks = probe.parse_realtime_tasks(PS_OUTPUT)
        self.assertEqual([task["tid"] for task in tasks["outside_root"]],
                         [10244, 10255, 120])

    def test_round_robin_is_realtime(self):
        tasks = probe.parse_realtime_tasks("  120 RR      50 0::/system.slice/other.service\n")
        self.assertEqual(tasks["outside_root"][0]["policy"], "RR")

    def test_headers_and_short_lines_are_ignored(self):
        tasks = probe.parse_realtime_tasks("  TID CLS RTPRIO CGROUP\n4242 FF\n")
        self.assertEqual(tasks["all"], [])

    def test_empty_inventory_is_empty(self):
        self.assertEqual(probe.parse_realtime_tasks(""), {"all": [], "outside_root": []})


class ConfiguredLogTests(unittest.TestCase):
    def test_configured_path_is_used(self):
        self.assertEqual(probe.configured_log_path("LOG_FILE=/var/log/resman/resman.log\n"),
                         "/var/log/resman/resman.log")

    def test_quoted_path_is_accepted(self):
        self.assertEqual(probe.configured_log_path('LOG_FILE="/var/log/other.log"\n'),
                         "/var/log/other.log")

    def test_absent_key_falls_back(self):
        self.assertEqual(probe.configured_log_path("OTHER=1\n"), probe.DEFAULT_LOG)

    def test_relative_path_falls_back(self):
        self.assertEqual(probe.configured_log_path("LOG_FILE=resman.log\n"), probe.DEFAULT_LOG)


class LogTailTests(unittest.TestCase):
    def test_only_the_new_tail_is_published(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "resman.log"
            log.write_text("first\n")
            offset = log.stat().st_size
            log.write_text("first\nsecond\n")
            with mock.patch.object(probe, "log_path", return_value=str(log)):
                self.assertEqual(probe.log_tail(offset), ["second"])

    def test_a_truncated_log_does_not_raise(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "resman.log"
            log.write_text("short\n")
            with mock.patch.object(probe, "log_path", return_value=str(log)):
                self.assertEqual(probe.log_tail(4096), [])

    def test_an_absent_log_is_empty(self):
        with mock.patch.object(probe, "log_path", return_value="/nonexistent/resman.log"):
            self.assertEqual(probe.log_tail(0), [])


class SubtreeControlTests(unittest.TestCase):
    def write(self, side_effect=None):
        """Write one token against a stand-in control file and report both sides."""
        with tempfile.TemporaryDirectory() as directory:
            control = Path(directory) / "cgroup.subtree_control"
            control.write_text("memory pids\n")
            with mock.patch.object(probe, "ROOT_SUBTREE_CONTROL", str(control)):
                if side_effect is None:
                    record = probe.write_subtree_control("+cpu")
                else:
                    with mock.patch.object(probe.os, "write", side_effect=side_effect):
                        record = probe.write_subtree_control("+cpu")
            return record, control.read_text()

    def test_an_accepted_write_is_recorded_with_the_resulting_set(self):
        record, written = self.write()
        self.assertTrue(record["accepted"])
        self.assertIsNone(record["errno"])
        # A kernel control file consumes one whole write; the stand-in regular
        # file keeps whatever the token did not overwrite.
        self.assertTrue(written.startswith("+cpu"))

    def test_a_refused_write_records_the_exact_errno(self):
        record, written = self.write(side_effect=OSError(errno.EINVAL, "Invalid argument"))
        self.assertEqual(written, "memory pids\n")
        self.assertFalse(record["accepted"])
        self.assertEqual(record["errno"], errno.EINVAL)
        self.assertEqual(record["errno_name"], "EINVAL")
        self.assertEqual(record["subtree_control"], ["memory", "pids"])

    def test_an_unknown_errno_is_named_explicitly(self):
        record, _ = self.write(side_effect=OSError(4095, "unknown"))
        self.assertEqual(record["errno_name"], "UNKNOWN")

    def test_an_unopenable_control_blocks_the_measurement(self):
        with mock.patch.object(probe, "ROOT_SUBTREE_CONTROL", "/nonexistent/subtree_control"):
            with self.assertRaises(probe.Blocked):
                probe.write_subtree_control("+cpu")


class CommandTests(unittest.TestCase):
    def test_a_non_zero_status_is_recorded_without_raising(self):
        record = probe.run("sh", "-c", "echo out; exit 3")
        self.assertEqual(record["status"], 3)
        self.assertEqual(record["output"], ["out"])

    def test_an_unexpected_status_blocks_the_measurement(self):
        with self.assertRaises(probe.Blocked):
            probe.run("sh", "-c", "exit 3", expected=0)

    def test_an_expected_status_is_accepted(self):
        self.assertEqual(probe.run("sh", "-c", "exit 0", expected=0)["status"], 0)


class ArgumentTests(unittest.TestCase):
    def test_a_malformed_run_id_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(RuntimeError):
                probe.main(["--evidence", directory, "--run-id", "today",
                            "--source-revision", "a" * 40])

    def test_a_short_revision_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(RuntimeError):
                probe.main(["--evidence", directory, "--run-id", "r20261009120000-1",
                            "--source-revision", "abc"])

    def test_an_unsafe_unit_name_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(RuntimeError):
                probe.main(["--evidence", directory, "--run-id", "r20261009120000-1",
                            "--source-revision", "a" * 40, "--unit", "fixture; rm -rf /"])

    def test_a_priority_outside_the_realtime_range_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(RuntimeError):
                probe.main(["--evidence", directory, "--run-id", "r20261009120000-1",
                            "--source-revision", "a" * 40, "--priority", "0"])

    def test_a_blocked_measurement_publishes_its_reason(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence = os.path.join(directory, "evidence")
            with mock.patch.object(probe, "measure",
                                   side_effect=probe.Blocked("no fixture")):
                status = probe.main(["--evidence", evidence, "--run-id", "r20261009120000-1",
                                     "--source-revision", "a" * 40])
            self.assertEqual(status, 77)
            published = Path(evidence, "result.json").read_text()
        self.assertIn("BLOCKED", published)
        self.assertIn("no fixture", published)


if __name__ == "__main__":
    unittest.main(verbosity=1)
