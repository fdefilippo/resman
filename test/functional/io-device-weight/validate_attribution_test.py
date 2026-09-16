#!/usr/bin/env python3
"""Unit tests for retained OL8/UEK direct BFQ attribution evidence."""

import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import validate_attribution


REVISION = "a" * 40
DEVICE = "251:0"


def write_manifest(root):
    entries = []
    for path in sorted(path for path in Path(root).rglob("*")
                       if path.is_file() and path.name != "SHA256SUMS"):
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        entries.append("%s  ./%s" % (digest, path.relative_to(root)))
    (Path(root) / "SHA256SUMS").write_text("\n".join(entries) + "\n")


class AttributionValidatorTests(unittest.TestCase):
    def make_evidence(self, root):
        root = Path(root)
        platform = root / "el8"
        guest = platform / "guest"
        guest.mkdir(parents=True)
        probe = b"retained attribution probe\n"
        (platform / "guest-attribution.py").write_bytes(probe)
        config = b"CONFIG_BFQ_GROUP_IOSCHED=y\n# CONFIG_BLK_CGROUP_IOCOST is not set\n"
        (guest / "kernel-config.txt").write_bytes(config)
        result = {
            "schema_version": 1,
            "scope": "test-only-ol8-uek-bfq-direct-write-attribution",
            "run_id": "r20260916080000-1",
            "source_revision": REVISION,
            "probe_sha256": hashlib.sha256(probe).hexdigest(),
            "requested_systemd_weight": 333,
            "expected_bfq_weight": 121,
            "result": "ATTRIBUTED",
            "environment": {
                "kernel": "5.15.0-test.el8uek.x86_64",
                "os_release": 'ID="ol"\nVERSION_ID="8.10"\n',
                "systemd_version": "systemd 239\n",
                "systemd_package": "systemd-239-test.x86_64",
                "kernel_package": "kernel-uek-core-5.15.0-test.el8uek.x86_64",
                "device": "/dev/vda",
                "major_minor": DEVICE,
                "scheduler_before": "[none] mq-deadline bfq",
                "root_controllers": "cpu io memory",
                "root_subtree_before": "memory pids",
                "kernel_config": {
                    "status": "captured",
                    "source": "/boot/config-5.15.0-test.el8uek.x86_64",
                    "retained_file": "kernel-config.txt",
                    "sha256": hashlib.sha256(config).hexdigest(),
                    "options": {
                        "CONFIG_BFQ_GROUP_IOSCHED": "y",
                        "CONFIG_BLK_CGROUP_IOCOST": "not_set",
                    },
                },
            },
            "direct_bfq": {
                "outcome": "KERNEL_ACCEPTS",
                "reason": "accepted",
                "cgroup": "/sys/fs/cgroup/resman-iow-direct-deadbeef0000",
                "weight_file": "/sys/fs/cgroup/resman-iow-direct-deadbeef0000/io.bfq.weight",
                "major_minor": DEVICE,
                "expected_weight": 121,
                "scheduler_during": "none mq-deadline [bfq]",
                "before": "default 100",
                "write": {"request": DEVICE + " 121\n", "error": None},
                "during": "default 100\n" + DEVICE + " 121",
                "reset": {"request": DEVICE + " default\n", "error": None},
                "after": "default 100",
            },
            "cleanup": {
                "result": "PASS",
                "errors": [],
                "cgroup_removed": True,
                "root_subtree_after": "memory pids",
                "scheduler_after": "[none] mq-deadline bfq",
            },
        }
        (guest / "result.json").write_text(json.dumps(result))
        (platform / "environment.txt").write_text("\n".join((
            "platform=el8",
            "kernel_family=uek",
            "source_revision=" + REVISION,
            "base_sha256=" + validate_attribution.BASE_SHA256,
            "guest_exit_code=0",
            "cleanup=PASS",
            "result=PASS",
            "exit_code=0",
        )) + "\n")
        (platform / "result").write_text("PASS\n")
        write_manifest(platform)
        (root / "matrix.txt").write_text("\n".join((
            "source_revision=" + REVISION,
            "campaign=ol8-uek-direct-bfq-attribution",
            "result=PASS",
            "exit_code=0",
        )) + "\n")
        (root / "result").write_text("PASS\n")
        write_manifest(root)
        return result

    def test_accepts_exact_direct_write_and_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            self.make_evidence(directory)
            self.assertEqual(
                validate_attribution.validate(directory, REVISION), "KERNEL_ACCEPTS")

    def test_rejects_false_readback_and_incomplete_cleanup(self):
        for mutation in ("readback", "cleanup"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                result = self.make_evidence(directory)
                if mutation == "readback":
                    result["direct_bfq"]["during"] = "default 100"
                else:
                    result["cleanup"]["cgroup_removed"] = False
                path = Path(directory) / "el8/guest/result.json"
                path.write_text(json.dumps(result))
                write_manifest(Path(directory) / "el8")
                write_manifest(directory)
                with self.assertRaises(AssertionError):
                    validate_attribution.validate(directory, REVISION)


if __name__ == "__main__":
    unittest.main()
