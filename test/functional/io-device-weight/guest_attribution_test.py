#!/usr/bin/env python3
"""Unit tests for the OL8/UEK direct BFQ attribution probe."""

from pathlib import Path
import tempfile
import unittest

import guest_attribution


class GuestAttributionTests(unittest.TestCase):
    def test_keyed_weight_requires_one_exact_numeric_device_entry(self):
        self.assertEqual(
            guest_attribution.keyed_weight("default 100\n251:0 121\n", "251:0"), 121)
        self.assertIsNone(
            guest_attribution.keyed_weight("default 100\n", "251:0"))
        for value in ("251:0 bad", "251:0 121 extra", "251:0 121\n251:0 122\n"):
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                guest_attribution.keyed_weight(value, "251:0")

    def test_kernel_option_distinguishes_values_disabled_and_absent(self):
        config = "\n".join((
            "CONFIG_BFQ_GROUP_IOSCHED=y",
            "# CONFIG_BLK_CGROUP_IOCOST is not set",
            "CONFIG_UNRELATED=m",
        ))
        self.assertEqual(
            guest_attribution.kernel_option(config, "CONFIG_BFQ_GROUP_IOSCHED"), "y")
        self.assertEqual(
            guest_attribution.kernel_option(config, "CONFIG_BLK_CGROUP_IOCOST"), "not_set")
        self.assertEqual(
            guest_attribution.kernel_option(config, "CONFIG_MISSING"), "absent")

    def test_invalid_identity_is_rejected_before_evidence_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence = Path(directory)
            for run_id, revision in (("../../operator", "a" * 40),
                                     ("r20260916080000-1", "short")):
                with self.subTest(run_id=run_id, revision=revision), \
                     self.assertRaises(RuntimeError):
                    guest_attribution.AttributionProbe(
                        evidence, run_id, revision, "/dev/vda")


if __name__ == "__main__":
    unittest.main()
