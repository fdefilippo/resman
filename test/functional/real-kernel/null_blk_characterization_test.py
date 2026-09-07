#!/usr/bin/env python3
"""Contract tests for kernel-only BFQ characterization; no host mutation."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from null_blk_characterization import Characterization, counters, summarize


class CharacterizationTests(unittest.TestCase):
    def interval(self):
        first = {"time_ns": 100, "leaves": [
            {"identity": ["a", 1], "io_stat": "251:0 rbytes=100 rios=1"},
            {"identity": ["b", 2], "io_stat": "251:0 rbytes=100 rios=1"}]}
        last = copy.deepcopy(first)
        last["time_ns"] += 20_000_000_000
        last["leaves"][0]["io_stat"] = "251:0 rbytes=200 rios=2"
        last["leaves"][1]["io_stat"] = "251:0 rbytes=400 rios=4"
        return first, last

    def test_raw_deltas_not_cumulative_ratios(self):
        result = summarize(*self.interval(), "251:0")
        self.assertEqual(result["read_share_percent"], [25, 75])
        self.assertEqual(result["seconds"], 20)
        self.assertNotIn("verdict", result)

    def test_incomplete_or_uncomparable_interval_is_rejected(self):
        for mutation in ("identity", "inode", "zero", "reset", "device", "count", "time"):
            with self.subTest(mutation=mutation):
                first, last = self.interval()
                if mutation == "identity":
                    last["leaves"][0]["identity"][0] = "replacement"
                elif mutation == "inode":
                    last["leaves"][0]["identity"][1] = 3
                elif mutation == "zero":
                    last["leaves"][0]["io_stat"] = first["leaves"][0]["io_stat"]
                elif mutation == "reset":
                    last["leaves"][0]["io_stat"] = "251:0 rbytes=1 rios=0"
                elif mutation == "device":
                    last["leaves"][0]["io_stat"] = "8:0 rbytes=200 rios=2"
                elif mutation == "count":
                    first["leaves"].append(first["leaves"][0])
                else:
                    last["time_ns"] = first["time_ns"]
                with self.assertRaises(RuntimeError):
                    summarize(first, last, "251:0")

    def test_counter_ambiguity_is_rejected(self):
        for value in ("", "251:0 rbytes=1", "251:0 rbytes=1 rios=1 rios=2",
                      "251:0 rbytes=1 rios=1\n251:0 rbytes=1 rios=1"):
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                counters(value, "251:0")

    def test_success_is_characterization_and_cleanup_failure_cannot_pass(self):
        for failed_cleanup in (False, True):
            with self.subTest(failed_cleanup=failed_cleanup), tempfile.TemporaryDirectory() as directory:
                gate = Characterization(Path(directory) / "evidence", "r123456")
                cleanup = RuntimeError("device remains") if failed_cleanup else None
                with patch.object(gate, "prepare"), patch.object(gate, "phase"), \
                     patch.object(gate, "cleanup", side_effect=cleanup):
                    result = gate.run()
                observed = json.loads((gate.evidence / "characterization.json").read_text())
                self.assertEqual(result, 1 if failed_cleanup else 0)
                self.assertEqual(observed["result"], "FAIL" if failed_cleanup else "CHARACTERIZATION")
                self.assertIn("not-daemon-enforcement", observed["scope"])

    def test_unsafe_run_identifier_is_rejected_before_path_creation(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence = Path(directory) / "evidence"
            with self.assertRaises(RuntimeError):
                Characterization(evidence, "../../operator")
            self.assertFalse(evidence.exists())


if __name__ == "__main__":
    unittest.main()
