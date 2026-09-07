#!/usr/bin/env python3
"""Require three fresh six-window controlled reference trials; report worst cases."""
import json
from pathlib import Path
import sys

from native_gate import require
from native_placement import LAYOUT, SCOPE, assert_equal_layout
from native_reference import analyze, comparable


def summarize(directories, revision):
    require(len(directories) == 3 and len(set(map(str, directories))) == 3, "three independent runs are required")
    runs, primary = [], []
    for directory in map(Path, directories):
        scope = json.loads((directory / "measurement-scope.json").read_text())
        require(scope["scope"] == SCOPE and scope["daemon_exercised"] is False,
                "wrong calibration scope; a pinned PASS does not cover unbound workloads")
        require(scope["source_revision"] == revision and scope["cpu_assignment_multiset"] == LAYOUT,
                "wrong revision or placement contract")
        require((directory / "result").read_text().strip() == "PASS", "replica did not pass")
        environment = (directory / "environment.txt").read_text().splitlines()
        require("cleanup=PASS" in environment, "replica cleanup was not verified")
        require(scope["run_id"] not in runs, "duplicate run identity")
        runs.append(scope["run_id"])
        frames = json.loads((directory / "reference-raw.json").read_text())
        previous = None
        for frame in frames:
            placement = frame["placement"]
            require(set(placement) == {group + "/" + leaf for group in ("referencea", "referenceb", "stale")
                                      for leaf in ("a", "b", "root", "best")}, "missing compared leaf")
            assert_equal_layout(placement)
            for workers in placement.values():
                require(len(workers) == 6 and all(w["state"] == "R" and len(w["affinity"]) == 1 for w in workers.values()),
                        "worker was not runnable with singleton affinity")
            births = {leaf: {pid: w["birth"] for pid, w in workers.items()} for leaf, workers in placement.items()}
            require(previous is None or births == previous, "worker identity changed")
            previous = births
        rows = analyze(frames)
        require(comparable(rows), "all six primary windows must pass without wider thresholds")
        primary.extend(row for row in rows if row["window_seconds"] == 60)
    return {"scope": SCOPE, "result": "PASS", "source_revision": revision, "runs": runs,
            "primary_windows": len(primary), "aggregate_tolerance_pp": 0.5, "leaf_tolerance_pp": 1.0,
            "minimum_stale_separation_pp": 2.0,
            "worst_aggregate_gap_pp": max(abs(row["differences_pp"]["mapped"]) for row in primary),
            "worst_leaf_gap_pp": max(abs(row["differences_pp"][leaf]) for row in primary for leaf in ("a", "b", "root", "best")),
            "worst_stale_separation_pp": min(row["stale_separation_pp"] for row in primary)}


if __name__ == "__main__":
    print(json.dumps(summarize(sys.argv[2:], sys.argv[1]), indent=2))
