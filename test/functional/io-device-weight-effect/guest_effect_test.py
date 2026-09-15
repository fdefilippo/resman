#!/usr/bin/env python3
"""Pure guest-runner parsing tests."""
import unittest
from pathlib import Path

import guest_effect


class GuestEffectTests(unittest.TestCase):
    def test_io_stat_requires_exact_device(self):
        text = "252:16 rbytes=4096 wbytes=0 rios=1 wios=0\n252:32 rbytes=8192 wbytes=0 rios=2 wios=0\n"
        self.assertEqual(guest_effect.io_stat_read_bytes(text, "252:16"), 4096)
        with self.assertRaisesRegex(RuntimeError, "lacks selected"):
            guest_effect.io_stat_read_bytes(text, "252:48")

    def test_metric_parser_matches_complete_label(self):
        text = ('resman_io_device_weight_effect_qualification_info{provenance="none"} 0\n'
                'resman_io_device_weight_effect_qualification_info{provenance="evidence"} 1\n')
        self.assertEqual(guest_effect.parse_metric(text,
            "resman_io_device_weight_effect_qualification_info", {"provenance": "evidence"}), 1)
        with self.assertRaises(KeyError):
            guest_effect.parse_metric(text, "resman_missing")

    def test_active_metric_label_requires_one_active_sample(self):
        text = ('resman_authority{coverage="complete"} 0\n'
                'resman_authority{coverage="partial"} 1\n')
        self.assertEqual(guest_effect.active_metric_label(
            text, "resman_authority", "coverage"), "partial")
        with self.assertRaisesRegex(RuntimeError, "unique active"):
            guest_effect.active_metric_label(text.replace(" 0", " 1"),
                                              "resman_authority", "coverage")

    def test_selected_scheduler_is_unambiguous(self):
        self.assertEqual(guest_effect.selected_scheduler("none [bfq] mq-deadline"), "bfq")
        with self.assertRaisesRegex(RuntimeError, "unique"):
            guest_effect.selected_scheduler("none bfq")

    def test_device_evidence_converts_internal_sysfs_path(self):
        projected = guest_effect.device_evidence([{
            "major_minor": "252:0", "block": Path("/sys/block/vda"),
            "owned_disposable": True,
        }])
        self.assertEqual(projected, [{
            "major_minor": "252:0", "sysfs_path": "/sys/block/vda",
            "owned_disposable": True,
        }])

    def test_profiles_separate_smoke_from_retained_qualification(self):
        self.assertEqual(guest_effect.PROFILES["smoke"]["interval_count"], 1)
        self.assertEqual(guest_effect.PROFILES["smoke"]["interval_seconds"], 1)
        self.assertEqual(guest_effect.PROFILES["qualification"]["interval_count"], 3)
        self.assertEqual(guest_effect.PROFILES["qualification"]["interval_seconds"], 10)
        self.assertNotEqual(guest_effect.PROFILES["smoke"]["scope"],
                            guest_effect.PROFILES["qualification"]["scope"])


if __name__ == "__main__":
    unittest.main()
