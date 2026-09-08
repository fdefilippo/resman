#!/usr/bin/env python3
"""Negative controls for reference-only comparability evidence."""
import contextlib
import copy
import io
import json
import tempfile
import unittest
import importlib.util
from unittest.mock import Mock, patch
from pathlib import Path

from native_reference import ReferenceDiagnostic, analyze, comparable
from native_placement import assert_equal_layout, inspect, observe
from native_proportional_test import frames, stamp
from native_proportional import ProportionalGate

spec = importlib.util.spec_from_file_location("native_workload", Path(__file__).with_name("native-workload.py"))
workload = importlib.util.module_from_spec(spec)
spec.loader.exec_module(workload)


def samples():
    _, frame = frames()
    frame["nodes"]["referencea"] = frame["nodes"].pop("native")
    frame["nodes"]["referenceb"] = frame["nodes"].pop("oracle")
    result = []
    for step in range(73):
        current = copy.deepcopy(frame)
        current["time"] = step * 5
        for group, nodes in current["nodes"].items():
            for node, values in nodes.items():
                # 1.2 CPU delivered by each parent; valid synchronized children.
                usage = {"parent": 6000000, "a": 1000000, "b": 1000000,
                         "root": 2000000, "best": 2000000}[node]
                if group == "stale" and node in ("a", "b"):
                    usage = 1200000
                if group == "stale" and node == "best":
                    usage = 1600000
                values["stat"] = {"usage_usec": usage * step, "nr_periods": 50 * step,
                                  "nr_throttled": 45 * step, "throttled_usec": 1000000 * step}
        stamp(current)
        result.append(current)
    return result


class ReferenceTests(unittest.TestCase):
    def test_live_worker_validation_is_wired_into_each_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = ReferenceDiagnostic(directory, "runit", "revision")
            with patch.object(gate, "placement", side_effect=AssertionError("wrong affinity")) as placement:
                with self.assertRaisesRegex(AssertionError, "wrong affinity"):
                    gate.snapshot()
                placement.assert_called_once()
            second = Path(directory) / "second"
            second.mkdir()
            gate = ProportionalGate(second, "runit", "revision")
            with patch.object(gate, "placement", side_effect=AssertionError("wrong affinity")):
                with self.assertRaisesRegex(AssertionError, "wrong affinity"):
                    gate.snapshot()

    def test_declared_worker_layout_must_be_observed(self):
        identity = {"pid": 100, "uid": 0, "start_time": "90", "children": [101, 102, 103, 104, 105, 106]}
        stat = "100 (worker) " + " ".join(["R", "100"] + ["0"] * 17 + ["90"])
        with patch("native_placement.field", return_value=stat), patch("native_placement.Path.stat") as owner, \
                patch("native_placement.os.sched_getaffinity", side_effect=lambda pid: {(pid - 101) % 4}):
            owner.return_value.st_uid = 0
            workers = inspect(identity)
            assert_equal_layout({"a": workers, "b": dict(reversed(list(workers.items())))})
            old = {"a": dict.fromkeys(workers, "other birth")}
            with self.assertRaisesRegex(AssertionError, "identity changed"):
                observe({"a": identity}, old)
            owner.return_value.st_uid = 1
            with self.assertRaisesRegex(AssertionError, "UID"):
                inspect(identity)
        with patch("native_placement.field", return_value=stat), patch("native_placement.Path.stat") as owner, \
                patch("native_placement.os.sched_getaffinity", return_value={0, 1, 2, 3}):
            owner.return_value.st_uid = 0
            with self.assertRaisesRegex(AssertionError, "affinity"):
                inspect(identity)
        for state in ("Z", "S", "D", "T"):
            with patch("native_placement.field", return_value=stat.replace(" R ", " " + state + " ")), \
                    patch("native_placement.Path.stat") as owner:
                owner.return_value.st_uid = 0
                with self.assertRaisesRegex(AssertionError, "runnable state"):
                    inspect(identity)

    def test_multiset_not_just_allowed_cpus_or_equal_counts(self):
        def leaf(cpus):
            return {str(i): {"affinity": [cpu]} for i, cpu in enumerate(cpus)}
        valid = leaf([0, 1, 2, 3, 0, 1])
        assert_equal_layout({"a": valid, "b": leaf([1, 0, 3, 2, 1, 0])})
        with self.assertRaisesRegex(AssertionError, "different CPU assignment"):
            assert_equal_layout({"a": valid, "b": leaf([0, 1, 2, 3, 2, 3])})
        with self.assertRaises(AssertionError):
            assert_equal_layout({"a": valid, "b": leaf([0, 1, 2, 3, 0])})
        with self.assertRaises(AssertionError):
            assert_equal_layout({"a": leaf([0] * 6), "b": leaf([0] * 6)})
        def observed(cpus):
            return {pid: dict(worker, birth="90", state="R") for pid, worker in leaf(cpus).items()}
        with patch("native_placement.inspect", side_effect=[observed([0, 1, 2, 3, 0, 1]), observed([0, 1, 2, 3, 2, 3])]):
            with self.assertRaisesRegex(AssertionError, "different CPU assignment"):
                observe({"a": {}, "b": {}}, None)

    def test_affinity_control_is_explicit_and_does_not_change_default_workers(self):
        self.assertEqual(workload.worker_cpus([]), [None] * 6)
        with patch.object(workload.os, "sched_getaffinity", return_value={0, 1, 2, 3}):
            self.assertEqual(workload.worker_cpus(["--one-worker-per-cpu"]), [0, 1, 2, 3])
            self.assertEqual(workload.worker_cpus(["--six-pinned-workers"]), [0, 1, 2, 3, 0, 1])
        with tempfile.TemporaryDirectory() as directory:
            gate = ReferenceDiagnostic(directory, "runit", "revision")
            self.assertEqual(gate.reference_workload_args(), ())
            gate.pinned = True
            self.assertEqual(gate.reference_workload_args(), ("--one-worker-per-cpu",))
            gate.six_pinned = True
            self.assertEqual(gate.reference_workload_args(), ("--six-pinned-workers",))
            self.assertEqual(gate.scenario(), "systemd-native-reference-pinned-six")
        with patch.object(workload.os, "sched_getaffinity", return_value={0, 1}):
            with self.assertRaises(ValueError):
                workload.worker_cpus(["--one-worker-per-cpu"])
        with self.assertRaises(ValueError):
            workload.worker_cpus(["--unknown"])
        calls = []
        ready = Mock()
        ready.set.side_effect = lambda: calls.append("ready")
        with patch.object(workload.os, "sched_setaffinity", side_effect=lambda *_: calls.append("affinity")) as affinity, \
                patch("builtins.sum", side_effect=RuntimeError("stop burner")):
            with self.assertRaises(RuntimeError):
                workload.burn(2, ready)
            affinity.assert_called_once_with(0, {2})
            ready.set.assert_called_once_with()
            self.assertEqual(calls, ["affinity", "ready"])

    def test_worker_identity_is_not_published_before_every_worker_is_ready(self):
        calls = []

        class Event:
            def is_set(self):
                calls.append("inspect-ready")
                return True

        class Process:
            def __init__(self, target, args):
                self.target = target
                self.args = args

            def start(self):
                calls.append("start")

            def is_alive(self):
                return True

        children = workload.start_workers([0, 1], Event, Process)
        self.assertEqual(len(children), 2)
        self.assertEqual(calls, ["start", "start", "inspect-ready", "inspect-ready"])

    def test_worker_readiness_refuses_an_exited_or_silent_child(self):
        ready = Mock()
        ready.is_set.return_value = False
        exited = Mock()
        exited.is_alive.return_value = False
        with self.assertRaisesRegex(RuntimeError, "exited before acknowledging"):
            workload.wait_for_worker_readiness([exited], [ready])

        alive = Mock()
        alive.is_alive.return_value = True
        with patch.object(workload.time, "monotonic", side_effect=[0, 1]):
            with self.assertRaisesRegex(RuntimeError, "acknowledgement timed out"):
                workload.wait_for_worker_readiness([alive], [ready], timeout=1)

    def test_main_publishes_ready_workers_without_starting_them_twice(self):
        children = [Mock(pid=101), Mock(pid=102)]

        class EmptyResident:
            def __len__(self):
                return 0

        with tempfile.TemporaryDirectory() as directory, \
                patch.object(workload.sys, "argv", ["native-workload.py", directory]), \
                patch.object(workload, "worker_cpus", return_value=[None, None]), \
                patch.object(workload, "start_workers", return_value=children), \
                patch.object(workload.signal, "signal"), \
                patch.object(workload.time, "monotonic", side_effect=[0, 1801]), \
                patch("builtins.bytearray", return_value=EmptyResident()):
            with self.assertRaises(SystemExit):
                workload.main()
            identity = json.loads((Path(directory) / "identity.json").read_text())
        self.assertEqual(identity["children"], [101, 102])
        for child in children:
            child.start.assert_not_called()
            child.terminate.assert_called_once_with()
            child.join.assert_called_once_with(5)

    def test_all_fixed_windows_are_required(self):
        results = analyze(samples())
        self.assertEqual(len(results), 9)
        self.assertTrue(comparable(results))
        for index in range(6):
            with self.subTest(index=index):
                missing = results[:index] + results[index + 1:]
                self.assertFalse(comparable(missing))
        self.assertFalse(comparable(results[6:]))

    def test_non_discriminating_control_cannot_pass(self):
        data = samples()
        for frame in data:
            frame["nodes"]["stale"] = copy.deepcopy(frame["nodes"]["referencea"])
            stamp(frame)
        self.assertFalse(comparable(analyze(data)))

    def test_one_bad_window_cannot_be_hidden_by_longer_averages(self):
        results = analyze(samples())
        results[3]["reference_comparable"] = False
        self.assertFalse(comparable(results))

    def test_real_counter_difference_fails_comparison(self):
        for leaf, usage in (("a", 200000), ("root", 100000)):
            with self.subTest(leaf=leaf):
                data = samples()
                for step, frame in enumerate(data):
                    frame["nodes"]["referencea"][leaf]["stat"]["usage_usec"] += step * usage
                    frame["nodes"]["referencea"]["best"]["stat"]["usage_usec"] -= step * usage
                self.assertFalse(comparable(analyze(data)))

    def test_invalid_intermediate_observation_fails(self):
        for kind in ("missing", "identity", "skew", "reset", "starved", "unthrottled"):
            with self.subTest(kind=kind):
                data = samples()
                if kind == "missing":
                    data.pop()
                elif kind == "identity":
                    data[7]["nodes"]["referencea"]["a"]["identity"] = [99, 99]
                elif kind == "skew":
                    data[7]["skew"] = 0.201
                elif kind == "reset":
                    data[7]["nodes"]["referencea"]["a"]["stat"]["usage_usec"] = 0
                else:
                    for frame in data:
                        for node in frame["nodes"]["referencea"].values():
                            if kind == "starved":
                                node["stat"]["usage_usec"] //= 2
                            else:
                                node["stat"]["nr_throttled"] = 0
                with self.assertRaises(AssertionError):
                    analyze(data)

    def test_cleanup_failure_overrides_a_complete_measurement(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
            gate = ReferenceDiagnostic(directory, "runit", "revision")
            gate.helper = Path(directory) / "helper"
            (Path(directory) / "native-workload.py").write_text("# fixture\n")
            stack.enter_context(patch.object(gate, "preflight"))
            stack.enter_context(patch.object(gate, "start_references"))
            stack.enter_context(patch.object(gate, "record_topology"))
            stack.enter_context(patch.object(gate, "snapshot", side_effect=samples()))
            stack.enter_context(patch("native_reference.time.sleep"))
            stack.enter_context(patch.object(gate, "cleanup", side_effect=RuntimeError("cleanup failure")))
            daemon = stack.enter_context(patch.object(gate, "start_daemon"))
            sessions = stack.enter_context(patch.object(gate, "start_sessions"))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
            self.assertEqual(gate.run(), 1)
            self.assertTrue((gate.evidence / "reference-analysis.json").exists())
            self.assertIn("cleanup=FAIL", (gate.evidence / "environment.txt").read_text())
            daemon.assert_not_called()
            sessions.assert_not_called()

    def test_unbound_dispersion_is_characterization_not_a_delivery_verdict(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
            gate = ReferenceDiagnostic(directory, "runit", "revision")
            gate.helper = Path(directory) / "helper"
            (Path(directory) / "native-workload.py").write_text("# fixture\n")
            for name in ("preflight", "start_references", "record_topology", "cleanup"):
                stack.enter_context(patch.object(gate, name))
            data = samples()
            for row in data:
                row["nodes"]["stale"] = copy.deepcopy(row["nodes"]["referencea"])
                stamp(row)
            stack.enter_context(patch.object(gate, "snapshot", side_effect=data))
            stack.enter_context(patch("native_reference.time.sleep"))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            self.assertEqual(gate.run(), 0)
            self.assertEqual((gate.evidence / "result").read_text(), "CHARACTERIZATION\n")
            report = (gate.evidence / "reference-analysis.json").read_text()
            self.assertNotIn("reference_comparable", report)
            self.assertNotIn("stale_discriminating", report)


if __name__ == "__main__":
    unittest.main()
