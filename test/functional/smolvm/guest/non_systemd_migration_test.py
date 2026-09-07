#!/usr/bin/env python3
"""Unprivileged pinning of the namespace fixture's identity and provenance."""
import importlib.util
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("legacy_fixture", Path(__file__).with_name("non-systemd-migration.py"))
legacy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(legacy)
spec = importlib.util.spec_from_file_location("metadata_fixture", Path(__file__).with_name("evidence-metadata.py"))
metadata = importlib.util.module_from_spec(spec)
spec.loader.exec_module(metadata)
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "final"))
from gate import fields


class LegacyFixtureTests(unittest.TestCase):
    def test_real_guest_producers_and_host_finalizer_have_one_value_per_field(self):
        copyfile = shutil.copyfile
        for guest_status, exit_code, host_cleanup, expected in (
                ("PASS", 0, "PASS", "PASS"), ("BLOCKED", 77, "PASS", "BLOCKED"),
                ("FAIL", 1, "PASS", "FAIL"), ("BLOCKED", 77, "FAIL", "FAIL"),
                ("PASS", 0, "FAIL", "FAIL")):
            with self.subTest(guest=guest_status, cleanup=host_cleanup), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                binary = root / "source-binary"
                binary.write_bytes(b"source-bound test binary")
                (root / "environment.txt").write_text("requested_scenario=non-systemd-migration\nsource_revision=" + "a" * 40 + "\n")
                with patch.object(metadata.shutil, "copyfile", side_effect=lambda _source, target: copyfile(binary, target)):
                    metadata.write_metadata(root, "runit", "a" * 40, "non-systemd-migration")
                legacy.write_guest_outcome(root, guest_status, "measured guest outcome", "PASS")
                subprocess.run([sys.executable, str(Path(metadata.__file__)), "finalize", str(root), host_cleanup, str(exit_code)], check=True)
                values = fields(root / "environment.txt")
                self.assertEqual(values["source_revision"], "a" * 40)
                self.assertEqual(values["scenario"], "non-systemd-migration")
                self.assertEqual(values["guest_result"], guest_status)
                self.assertEqual(values["guest_cleanup"], "PASS")
                self.assertEqual(values["cleanup"], host_cleanup)
                self.assertEqual(values["result"], expected)
                self.assertEqual((root / "result").read_text().strip(), expected)

    def test_blocked_before_guest_keeps_host_revision_and_scenario(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "environment.txt").write_text("requested_scenario=non-systemd-migration\nsource_revision=" + "a" * 40 + "\n")
            metadata.finalize(root, "PASS", 77)
            values = fields(root / "environment.txt")
            self.assertEqual(values["scenario"], "non-systemd-migration")
            self.assertEqual(values["source_revision"], "a" * 40)
            self.assertEqual(values["result"], "BLOCKED")

    def test_live_restore_requires_both_identity_and_exact_origin(self):
        for actual_birth, actual_path, valid in (("100", "0::/origin", True),
                                                 ("101", "0::/origin", False),
                                                 ("100", "0::/recovery", False)):
            with self.subTest(birth=actual_birth, path=actual_path), \
                    patch.object(legacy, "birth", return_value=actual_birth), \
                    patch.object(legacy, "field", return_value=actual_path):
                if valid:
                    legacy.assert_restored(123, "100", "0::/origin")
                else:
                    with self.assertRaises(AssertionError):
                        legacy.assert_restored(123, "100", "0::/origin")

    def test_all_four_independent_contracts_are_named(self):
        self.assertEqual(legacy.CHECKS, {"non-systemd-namespace", "legacy-cpu-ingress",
                                        "legacy-live-restoration", "legacy-cleanup"})

    def test_revision_is_not_optional_or_a_short_hash(self):
        for revision in ("", "HEAD", "abc1234", "x" * 40):
            with self.subTest(revision=revision), patch.object(metadata.shutil, "copyfile") as copy:
                with self.assertRaises(ValueError):
                    metadata.write_metadata("/unused", "runit", revision, "non-systemd-migration")
                copy.assert_not_called()


if __name__ == "__main__":
    unittest.main()
