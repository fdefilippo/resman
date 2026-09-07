#!/usr/bin/env python3
"""Negative controls for reference-only comparability evidence."""
import contextlib
import copy
import io
import tempfile
import unittest
from unittest.mock import patch

from native_reference import ReferenceDiagnostic, analyze, comparable
from native_proportional_test import frames


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
        result.append(current)
    return result


class ReferenceTests(unittest.TestCase):
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
            # No daemon/session startup belongs to this diagnostic.
            stack.enter_context(patch.object(gate, "preflight", side_effect=RuntimeError("fixture failure")))
            stack.enter_context(patch.object(gate, "cleanup", side_effect=RuntimeError("cleanup failure")))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
            self.assertEqual(gate.run(), 1)
            self.assertIn("cleanup=FAIL", (gate.evidence / "environment.txt").read_text())


if __name__ == "__main__":
    unittest.main()
