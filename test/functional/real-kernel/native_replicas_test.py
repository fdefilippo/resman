#!/usr/bin/env python3
"""The calibration gate cannot accept missing replicas or an unbound verdict."""
import copy
import json
from pathlib import Path
import tempfile
import unittest

from native_placement import LAYOUT, SCOPE
from native_reference_test import samples
from native_replicas import summarize


class ReplicaTests(unittest.TestCase):
    def test_three_fresh_runs_scope_and_each_window_are_required(self):
        data = samples()
        for frame in data:
            frame["placement"] = {group + "/" + leaf: {
                str(i): {"birth": "90", "state": "R", "affinity": [cpu]} for i, cpu in enumerate(LAYOUT)}
                for group in ("referencea", "referenceb", "stale") for leaf in ("a", "b", "root", "best")}
        for mutation in ("none", "scope", "revision", "duplicate", "result", "cleanup", "missing_window",
                         "mismatch", "blocked_worker", "birth", "control", "missing_leaf"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                paths = []
                for index in range(3):
                    path = Path(directory) / str(index)
                    path.mkdir()
                    paths.append(path)
                    scope = {"scope": SCOPE, "daemon_exercised": False, "source_revision": "revision",
                             "cpu_assignment_multiset": LAYOUT, "run_id": str(index)}
                    rows = copy.deepcopy(data)
                    status, environment = "PASS", "cleanup=PASS\n"
                    if index == 2:
                        if mutation == "scope": scope["scope"] = "unbound"
                        if mutation == "revision": scope["source_revision"] = "old"
                        if mutation == "duplicate": scope["run_id"] = "0"
                        if mutation == "result": status = "CHARACTERIZATION"
                        if mutation == "cleanup": environment = "cleanup=FAIL\n"
                        if mutation == "missing_window": rows.pop()
                        if mutation == "mismatch": rows[12]["placement"]["referencea/a"]["0"]["affinity"] = [3]
                        if mutation == "blocked_worker": rows[12]["placement"]["referencea/a"]["0"]["state"] = "S"
                        if mutation == "birth": rows[12]["placement"]["referencea/a"]["0"]["birth"] = "91"
                        if mutation == "missing_leaf": del rows[12]["placement"]["referencea/a"]
                        if mutation == "control":
                            for row in rows:
                                row["nodes"]["stale"] = copy.deepcopy(row["nodes"]["referencea"])
                    (path / "measurement-scope.json").write_text(json.dumps(scope))
                    (path / "reference-raw.json").write_text(json.dumps(rows))
                    (path / "result").write_text(status)
                    (path / "environment.txt").write_text(environment)
                if mutation == "none":
                    result = summarize(paths, "revision")
                    self.assertEqual(result["primary_windows"], 18)
                    self.assertEqual(result["worst_aggregate_gap_pp"], 0)
                    self.assertEqual(result["aggregate_tolerance_pp"], 0.5)
                    with self.assertRaises(AssertionError): summarize(paths[:2], "revision")
                    with self.assertRaises(AssertionError): summarize([paths[0]] * 3, "revision")
                else:
                    with self.assertRaises(AssertionError): summarize(paths, "revision")


if __name__ == "__main__":
    unittest.main()
