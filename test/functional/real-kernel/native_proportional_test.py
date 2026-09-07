#!/usr/bin/env python3
"""Unprivileged guards for the independent native proportional oracle."""
import contextlib
import copy
import io
import tempfile
import unittest
from unittest.mock import patch

from native_proportional import ProportionalGate, checks_pass, compare_reference, ratios


def frames():
    nodes = {}
    for group in ("native", "oracle", "stale"):
        nodes[group] = {}
        for node, value in {"parent": 60000000, "a": 10000000, "b": 10000000, "root": 20000000, "best": 20000000}.items():
            nodes[group][node] = {"identity": [1, node], "stat": {
                "usage_usec": value, "nr_periods": 600, "nr_throttled": 550, "throttled_usec": 10000000,
            }}
    after = {"time": 160, "skew": 0.01, "nodes": nodes}
    before = copy.deepcopy(after)
    before["time"] = 100
    for group in before["nodes"].values():
        for node in group.values():
            node["stat"] = dict.fromkeys(node["stat"], 0)
    return before, after


class ProportionalTests(unittest.TestCase):
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

    def test_invalid_measurements_are_not_zero_or_pass(self):
        for kind in ("short", "skew", "identity", "decrease", "empty_parent", "overflowing_leaves", "throttle_reset"):
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
                else:
                    before["nodes"]["native"]["parent"]["stat"]["nr_throttled"] = 551
                with self.assertRaises(AssertionError):
                    ratios(before, after)

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
