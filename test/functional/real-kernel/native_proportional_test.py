#!/usr/bin/env python3
"""Unprivileged guards for the independent native proportional oracle."""
import contextlib
import copy
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from native_proportional import (LEAVES, ProportionalGate, checks_pass, compare_reference, ratios,
                                 _sampling_spans, validate_sample_interval)


def stamp(frame, node_gap=0.0002, read_width=0.0001):
    cursor = frame["time"] + 0.001
    frame["sampling"] = {}
    for group, nodes in frame["nodes"].items():
        frame["sampling"][group] = {"maximum_instantaneous_capacity_usec_per_second": 4000000}
        for node in ("parent",) + LEAVES:
            nodes[node]["read"] = {"started": cursor, "finished": cursor + read_width}
            cursor += node_gap + read_width
    frame["skew"] = cursor - frame["time"] + 0.001


def set_read_sequence(frame, sequence):
    cursor = frame["time"] + 0.001
    for group, node in sequence:
        frame["nodes"][group][node]["read"] = {"started": cursor, "finished": cursor + 0.0001}
        cursor += 0.0002
    frame["skew"] = cursor - frame["time"] + 0.001


def frames():
    nodes = {}
    for group in ("native", "oracle", "stale"):
        nodes[group] = {}
        for node, value in {"parent": 60000000, "a": 10000000, "b": 10000000, "root": 20000000, "best": 20000000}.items():
            nodes[group][node] = {"identity": [1, node], "stat": {
                "usage_usec": value, "nr_periods": 600, "nr_throttled": 550, "throttled_usec": 10000000,
            }}
    after = {"time": 160, "nodes": nodes}
    stamp(after)
    before = copy.deepcopy(after)
    before["time"] = 100
    stamp(before)
    for group in before["nodes"].values():
        for node in group.values():
            node["stat"] = dict.fromkeys(node["stat"], 0)
    return before, after


class ProportionalTests(unittest.TestCase):
    def test_root_uses_ssh_and_is_not_assumed_to_have_a_cron_session(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = ProportionalGate(directory, "runit", "revision")
            self.assertEqual(gate.cron_accounts(), [])
            self.assertEqual([a.pw_uid for a in gate.session_accounts()], [0])

    def test_each_result_is_mandatory(self):
        keys = ("full-budget-rejection", "pam-sessions", "native-plan", "full-contention", "stale-control",
                "work-conserving-lending", "root-progress", "unchanged-membership", "unchanged-weights", "graceful-stop")
        valid = dict.fromkeys(keys, "PASS")
        self.assertTrue(checks_pass(valid))
        for key in keys:
            missing = dict(valid)
            del missing[key]
            self.assertFalse(checks_pass(missing))
            for value in ("FAIL", "BLOCKED", None):
                self.assertFalse(checks_pass(dict(valid, **{key: value})))
        self.assertFalse(checks_pass(dict(valid, foreign="PASS")))

    def test_measured_parent_is_the_denominator(self):
        before, after = frames()
        result = ratios(before, after)
        self.assertAlmostEqual(result["native"]["mapped"], 100 / 3)
        self.assertEqual(result["native"]["throttling"]["nr_throttled"], 550)
        compare_reference(result)

    def test_snapshot_records_the_counter_read_contract_at_the_producer(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths = {}
            for node in ("parent",) + LEAVES:
                path = root / node
                path.mkdir()
                (path / "cpu.stat").write_text(
                    "usage_usec 1\nnr_periods 1\nnr_throttled 1\nthrottled_usec 1\n"
                )
                paths[node] = path
                if node != "parent":
                    (path / "cpu.weight").write_text(str({"a": 5000, "b": 5000, "root": 10000, "best": 10000}[node]))
            (paths["parent"] / "cpu.max").write_text("120000 100000")
            gate = ProportionalGate(root, "runit", "revision")
            gate.paths = {"native": paths}
            with patch.object(gate, "placement", return_value={}):
                frame = gate.snapshot()
            spans = _sampling_spans(frame)
            self.assertGreater(spans["native"], 0)
            self.assertEqual(
                sorted(frame["nodes"]["native"], key=lambda node: frame["nodes"]["native"][node]["read"]["started"]),
                ["parent", "a", "b", "root", "best"],
            )

    def test_invalid_measurements_are_not_zero_or_pass(self):
        for kind in ("short", "skew", "identity", "decrease", "empty_parent", "overflowing_leaves",
                     "underflowing_leaves", "throttle_reset"):
            with self.subTest(kind=kind):
                before, after = frames()
                if kind == "short":
                    after["time"] = 159
                elif kind == "skew":
                    before["skew"] = 0.201
                elif kind == "identity":
                    after["nodes"]["native"]["a"]["identity"] = [1, "recreated"]
                elif kind == "decrease":
                    before["nodes"]["native"]["a"]["stat"]["usage_usec"] = 10000001
                elif kind == "empty_parent":
                    after["nodes"]["native"]["parent"]["stat"]["usage_usec"] = 0
                elif kind == "overflowing_leaves":
                    after["nodes"]["native"]["root"]["stat"]["usage_usec"] = 60000001
                elif kind == "underflowing_leaves":
                    after["nodes"]["native"]["best"]["stat"]["usage_usec"] = 0
                else:
                    before["nodes"]["native"]["parent"]["stat"]["nr_throttled"] = 551
                with self.assertRaises(AssertionError):
                    ratios(before, after)

    def test_intermediate_conservation_uses_measured_timing_and_is_bilateral(self):
        for direction in (-1, 1):
            with self.subTest(direction=direction):
                before, after = frames()
                after["time"] = 105
                stamp(after, node_gap=0.01, read_width=0.001)
                stamp(before, node_gap=0.01, read_width=0.001)
                after["nodes"]["native"]["root"]["stat"]["usage_usec"] += direction * 200000
                report = validate_sample_interval(before, after)
                self.assertEqual(report["native"]["error_usec"], direction * 200000)
                self.assertEqual(report["native"]["permitted_error_usec"], 360010)

                after["nodes"]["native"]["root"]["stat"]["usage_usec"] += direction * 200000
                with self.assertRaisesRegex(AssertionError, "measured read-timing uncertainty"):
                    validate_sample_interval(before, after)

    def test_primary_conservation_remains_fixed_at_one_percent_in_both_directions(self):
        for direction in (-1, 1):
            with self.subTest(direction=direction):
                before, after = frames()
                after["nodes"]["native"]["root"]["stat"]["usage_usec"] += direction * 600001
                with self.assertRaisesRegex(AssertionError, "full-window difference exceeds one percent"):
                    ratios(before, after)

    def test_per_node_timing_and_adjacent_group_reads_are_mandatory(self):
        for kind in ("missing", "reordered", "interleaved"):
            with self.subTest(kind=kind):
                before, after = frames()
                if kind == "missing":
                    del after["nodes"]["native"]["a"]["read"]
                elif kind == "reordered":
                    reads = after["nodes"]["native"]
                    reads["parent"]["read"], reads["a"]["read"] = reads["a"]["read"], reads["parent"]["read"]
                else:
                    sequence = [("native", "parent"), ("native", "a")]
                    sequence += [("oracle", node) for node in ("parent",) + LEAVES]
                    sequence += [("native", node) for node in ("b", "root", "best")]
                    sequence += [("stale", node) for node in ("parent",) + LEAVES]
                    set_read_sequence(after, sequence)
                with self.assertRaises(AssertionError):
                    validate_sample_interval(before, after)

    def test_aggregate_and_leaf_tolerances_are_independent(self):
        for dimension, delta in (("mapped", 0.501), ("a", 1.001)):
            with self.subTest(dimension=dimension):
                measured = ratios(*frames())
                measured["native"][dimension] += delta
                with self.assertRaises(AssertionError):
                    compare_reference(measured)

    def test_failed_cleanup_cannot_publish_pass(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
            gate = ProportionalGate(directory, "runit", "revision")
            stack.enter_context(patch.object(gate, "preflight", side_effect=RuntimeError("fixture failure")))
            stack.enter_context(patch.object(gate, "cleanup", side_effect=RuntimeError("cleanup failure")))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
            self.assertEqual(gate.run(), 1)
            self.assertEqual((gate.evidence / "result").read_text(), "FAIL\n")
            self.assertIn("cleanup=FAIL", (gate.evidence / "environment.txt").read_text())


if __name__ == "__main__":
    unittest.main()
