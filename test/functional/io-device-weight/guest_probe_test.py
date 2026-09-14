#!/usr/bin/env python3
"""Unit tests for the IODeviceWeight guest characterization."""

import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import guest_probe


class GuestProbeTests(unittest.TestCase):
    def test_bfq_weight_matches_systemd_scale_boundaries(self):
        cases = {
            1: 1,
            100: 100,
            333: 121,
            2300: 300,
            10000: 1000,
        }
        for requested, expected in cases.items():
            with self.subTest(requested=requested):
                self.assertEqual(guest_probe.bfq_weight(requested), expected)

    def test_keyed_rows_require_one_exact_device_entry(self):
        self.assertEqual(
            guest_probe.keyed_weight("default 100\n252:16 333\n", "252:16"), 333)
        self.assertIsNone(guest_probe.keyed_weight("default 100\n", "252:16"))
        for value in ("252:16 value", "252:16 1 extra", "252:16 1\n252:16 2\n"):
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                guest_probe.keyed_weight(value, "252:16")

    def make_probe(self, directory):
        evidence = Path(directory) / "evidence"
        evidence.mkdir()
        return guest_probe.Probe(evidence, "el9", "r20260914070000-1",
                                 "a" * 40, "/dev/vdb")

    def test_run_distinguishes_characterized_blocked_and_cleanup_failure(self):
        cases = (
            (None, None, "CHARACTERIZED", 0),
            (guest_probe.Blocked("invalid guest"), None, "BLOCKED", 77),
            (None, RuntimeError("unit survived"), "FAIL", 1),
        )
        for execution_error, cleanup_error, expected, exit_code in cases:
            with self.subTest(expected=expected), tempfile.TemporaryDirectory() as directory:
                probe = self.make_probe(directory)

                def characterize():
                    if execution_error:
                        raise execution_error
                    probe.record["rows"] = {
                        name: {"outcome": "UNSUPPORTED"}
                        for name in ("bfq", "iocost", "simultaneous")
                    }

                with mock.patch.object(probe, "preflight"), \
                     mock.patch.object(probe, "start_unit"), \
                     mock.patch.object(probe, "characterize", side_effect=characterize), \
                     mock.patch.object(probe, "cleanup", side_effect=cleanup_error):
                    self.assertEqual(probe.run(), exit_code)
                result = json.loads((probe.evidence / "result.json").read_text())
                self.assertEqual(result["result"], expected)

    def test_invalid_identity_is_rejected_without_touching_the_device(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence = Path(directory) / "evidence"
            evidence.mkdir()
            for platform, run_id, revision in (
                    ("el7", "r20260914070000-1", "a" * 40),
                    ("el9", "../../operator", "a" * 40),
                    ("el9", "r20260914070000-1", "short")):
                with self.subTest(platform=platform, run_id=run_id, revision=revision), \
                     self.assertRaises(RuntimeError):
                    guest_probe.Probe(evidence, platform, run_id, revision, "/dev/vdb")

    def test_simultaneous_prerequisite_absence_is_not_applicable(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = self.make_probe(directory)
            row = probe.row(
                "simultaneous", False,
                guest_probe.REASON_MECHANISM_PREREQUISITE_UNAVAILABLE,
                "BFQ and io.cost did not both pass their individual systemd-to-kernel paths")
            self.assertEqual(probe.record["schema_version"], 2)
            self.assertEqual(row["outcome"], "NOT_APPLICABLE")
            self.assertEqual(
                row["reason_code"],
                guest_probe.REASON_MECHANISM_PREREQUISITE_UNAVAILABLE)


if __name__ == "__main__":
    unittest.main()
