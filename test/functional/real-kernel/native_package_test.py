#!/usr/bin/env python3
"""Package acceptance cannot be satisfied by a source binary or an empty run."""
import contextlib
import io
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from native_gate import NativeGate
from native_package import (CURRENT_SCHEMA_VERSION, PREVIOUS_SCHEMA_VERSION,
                            PackageGate, REQUIRED_CHECKS, matching_package)


class PackageTests(unittest.TestCase):
    def test_package_schema_contract_rejects_six_and_creates_seven(self):
        self.assertEqual(PREVIOUS_SCHEMA_VERSION, 6)
        self.assertEqual(CURRENT_SCHEMA_VERSION, 7)

    def test_both_installed_identity_and_actual_payload_bytes_are_required(self):
        matching_package("resman-1.33.0-2", "resman-1.33.0-2", b"package", b"package")
        for identity, binary in (("resman-1.33.0-1", b"package"), ("resman-1.33.0-2", b"source")):
            with self.subTest(identity=identity, binary=binary), self.assertRaises(AssertionError):
                matching_package(identity, "resman-1.33.0-2", binary, b"package")

    def test_missing_rpm_is_blocked_before_common_host_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = PackageGate(directory, "rpackage", "revision")
            with patch.object(NativeGate, "preflight") as common, self.assertRaisesRegex(RuntimeError, "exact installed RPM"):
                gate.preflight()
            common.assert_not_called()

    def test_package_projection_requires_full_lifecycle_and_real_database_rows(self):
        for mutation in ("none", "failed-lifecycle", "previous-schema", "no-rows", "missing-identity"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                gate = PackageGate(directory, "rpackage", "revision")
                def lifecycle(instance):
                    with sqlite3.connect(instance.db) as database:
                        database.execute("PRAGMA user_version=" + str(
                            PREVIOUS_SCHEMA_VERSION if mutation == "previous-schema" else CURRENT_SCHEMA_VERSION))
                        database.execute("CREATE TABLE user_metrics(uid INTEGER)")
                        if mutation != "no-rows": database.execute("INSERT INTO user_metrics VALUES(1006)")
                    for key in ("installed-identity", "shipped-defaults", "upgrade-750-rejected"):
                        if mutation != "missing-identity" or key != "installed-identity": instance.package_passed(key, {"measured": True})
                    instance.save("graceful-stop", {"journal_absent": True})
                    status = "FAIL" if mutation == "failed-lifecycle" else "PASS"
                    (instance.evidence / "result").write_text(status)
                    (instance.evidence / "environment.txt").write_text("scenario=systemd-native-lifecycle\nresult=" + status + "\n")
                    return 1 if status == "FAIL" else 0
                with patch.object(NativeGate, "run", lifecycle), contextlib.redirect_stderr(io.StringIO()):
                    code = gate.run()
                self.assertEqual(code, 0 if mutation == "none" else 1)
                self.assertEqual((gate.evidence / "result").read_text().strip(), "PASS" if mutation == "none" else "FAIL")
                self.assertEqual(set(json.loads((gate.evidence / "checks.json").read_text())), REQUIRED_CHECKS)
                self.assertIn("scenario=native-package-acceptance", (gate.evidence / "environment.txt").read_text())


if __name__ == "__main__":
    unittest.main()
