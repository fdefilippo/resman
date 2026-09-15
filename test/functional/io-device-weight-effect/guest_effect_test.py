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


if __name__ == "__main__":
    unittest.main()
