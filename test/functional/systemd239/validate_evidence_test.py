#!/usr/bin/env python3
"""Negative tests for the independent EL8 QEMU evidence consumer."""
import importlib.util
from pathlib import Path
import tempfile
import unittest


path = Path(__file__).with_name("validate_evidence.py")
spec = importlib.util.spec_from_file_location("validate_evidence", path)
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)


class EvidenceConsumerTests(unittest.TestCase):
    def test_fields_reject_duplicate_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Path(directory) / "fields.txt"
            fixture.write_text("result=PASS\nresult=FAIL\n")
            with self.assertRaisesRegex(AssertionError, "duplicate"):
                validator.fields(fixture)

    def test_manifest_rejects_missing_and_extra_files(self):
        for mutation in ("missing", "extra"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                retained = root / "retained.txt"
                retained.write_text("retained\n")
                (root / "SHA256SUMS").write_text(
                    validator.digest(retained) + "  ./retained.txt\n")
                validator.verify_manifest(root)
                if mutation == "missing":
                    retained.unlink()
                else:
                    (root / "extra.txt").write_text("extra\n")
                with self.assertRaisesRegex(AssertionError, "differ"):
                    validator.verify_manifest(root)

    def test_all_pass_requires_exact_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Path(directory) / "checks.json"
            fixture.write_text('{"one":"PASS","two":"PASS"}\n')
            validator.all_pass(fixture, {"one", "two"})
            with self.assertRaisesRegex(AssertionError, "inventory"):
                validator.all_pass(fixture, {"one", "two", "three"})
            fixture.write_text('{"one":"PASS","two":"FAIL"}\n')
            with self.assertRaisesRegex(AssertionError, "non-PASS"):
                validator.all_pass(fixture, {"one", "two"})


if __name__ == "__main__":
    unittest.main()
