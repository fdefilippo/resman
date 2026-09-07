#!/usr/bin/env python3
"""Exercise actual guest metadata and shell kernel fields through the final consumer."""
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).parent
FINAL = HERE.parents[1] / "final"
sys.path.insert(0, str(FINAL))
from catalog import ROWS
from gate import inspect_evidence

spec = importlib.util.spec_from_file_location("guest_metadata", HERE / "evidence-metadata.py")
metadata = importlib.util.module_from_spec(spec)
spec.loader.exec_module(metadata)


class MetadataIntegrationTests(unittest.TestCase):
    def test_actual_guest_kernel_printf_and_metadata_reach_final_consumer(self):
        source = (HERE / "run-functional.sh").read_text()
        lines = [line.strip() for line in source.splitlines() if re.match(r"\s*printf '(kernel|uname)=", line)]
        self.assertEqual(len(lines), 2)
        produced = subprocess.check_output(["bash", "-c", "\n".join(lines)], text=True)
        row = next(row for row in ROWS if row.scenario == "mcp-filter-reload")
        revision = "a" * 40
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / "source-binary"
            binary.write_bytes(b"source fixture binary")
            evidence = root / "evidence"
            evidence.mkdir()
            copyfile = shutil.copyfile
            def copy(source, destination):
                self.assertEqual(source, "/usr/bin/resman")
                return copyfile(binary, destination)
            with patch.object(metadata.shutil, "copyfile", copy):
                metadata.write_metadata(evidence, "rintegration", revision, row.scenario)
            with (evidence / "environment.txt").open("a") as stream:
                stream.write(produced + "scenario=mcp-filter-reload\ncleanup=PASS\nresult=PASS\n")
            (evidence / "result").write_text("PASS\n")
            (evidence / "checks.json").write_text(json.dumps({row.scenario: "PASS"}))
            (evidence / (row.scenario + ".json")).write_text(json.dumps({"persisted_and_effective": True}))
            identity = json.loads((evidence / "environment.json").read_text())
            artifact = {"source_revision": revision, "binary_sha256": identity["tested_binary_sha256"]}
            status, observed = inspect_evidence(row, evidence, revision, artifact)
            self.assertEqual(status, "PASS")
            self.assertEqual(observed["kernel"], os.uname().release)
            # The previous producer shape must be rejected even with valid JSON
            # identity and an otherwise complete, successful scenario.
            before = (evidence / "environment.txt").read_text()
            (evidence / "environment.txt").write_text(before.replace("kernel=" + os.uname().release,
                "kernel=" + subprocess.check_output(["uname", "-srvmo"], text=True).strip()))
            with self.assertRaisesRegex(ValueError, "identity differs"):
                inspect_evidence(row, evidence, revision, artifact)


if __name__ == "__main__":
    unittest.main()
