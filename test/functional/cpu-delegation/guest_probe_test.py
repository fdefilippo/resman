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


def status(policy, priority, name="migration/0"):
    """One synthetic /proc/<tid>/stat body with the numbered fields in place."""
    tail = ["0"] * 39
    tail[0] = "S"
    tail[37] = str(priority)
    tail[38] = str(policy)
    return "4242 (" + name + ") " + " ".join(tail) + "\n"


class RealtimeInventoryTests(unittest.TestCase):
    def test_policy_and_priority_are_read_from_the_numbered_fields(self):
        self.assertEqual(probe.parse_task_policy(status(1, 97)), (1, 97))

    def test_a_parenthesis_in_the_thread_name_does_not_shift_the_fields(self):
        self.assertEqual(probe.parse_task_policy(status(2, 50, "odd) name")), (2, 50))

    def test_a_truncated_status_is_refused(self):
        with self.assertRaises(RuntimeError):
            probe.parse_task_policy("4242 (short) S 0 0 0\n")

    def test_only_realtime_policies_are_selected(self):
        inventory = probe.realtime_inventory([
            (1, status(0, 0), "0::/init.scope"),
            (2, status(1, 97), "0::/system.slice/vcs.service"),
            (3, status(2, 50), "0::/system.slice/other.service"),
            (4, status(5, 0), "0::/system.slice/idle.service"),
        ])
        self.assertEqual([task["tid"] for task in inventory["all"]], [2, 3])
        self.assertEqual([task["policy"] for task in inventory["all"]], ["FF", "RR"])

    def test_a_kernel_thread_in_the_root_is_not_outside_it(self):
        inventory = probe.realtime_inventory([(18, status(1, 99), "0::/\n")])
        self.assertEqual(len(inventory["all"]), 1)
        self.assertEqual(inventory["outside_root"], [])

    def test_an_unreadable_cgroup_is_not_counted_as_outside_the_root(self):
        inventory = probe.realtime_inventory([(18, status(1, 99), "")])
        self.assertEqual(inventory["outside_root"], [])

    def test_a_service_thread_is_outside_the_root(self):
        inventory = probe.realtime_inventory([
            (10244, status(1, 97), "0::/system.slice/vcs.service\n")])
        self.assertEqual(inventory["outside_root"][0]["cgroup"],
                         "0::/system.slice/vcs.service")
        self.assertEqual(inventory["outside_root"][0]["priority"], "97")

    def test_threads_are_reported_in_a_stable_order(self):
        inventory = probe.realtime_inventory([
            (99, status(1, 10), "0::/a.service"), (11, status(1, 10), "0::/b.service")])
        self.assertEqual([task["tid"] for task in inventory["all"]], [11, 99])

    def test_an_empty_inventory_is_empty(self):
        self.assertEqual(probe.realtime_inventory([]), {"all": [], "outside_root": []})

    def test_the_live_inventory_separates_the_root_consistently(self):
        inventory = probe.realtime_tasks()
        self.assertEqual(
            [task for task in inventory["outside_root"]
             if task["cgroup"] in probe.ROOT_CGROUPS], [])
        self.assertTrue(set(task["tid"] for task in inventory["outside_root"])
                        <= set(task["tid"] for task in inventory["all"]))


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
