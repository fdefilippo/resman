#!/usr/bin/env python3
"""Mutation tests for the independent IODeviceWeight evidence consumer."""

import copy
import json
from pathlib import Path
import tempfile
import unittest

import validate_evidence as validator


class EvidenceValidatorTests(unittest.TestCase):
    revision = "a" * 40
    device = "252:16"
    cgroup = "/sys/fs/cgroup/user.slice/user-resman_iow_0123456789ab.slice"

    def snapshot(self, weight=None, bfq=None, qos="252:16 enable=0"):
        property_value = "a(st) 0" if weight is None else 'a(st) 1 "/dev/vdb" %d' % weight
        return {
            "property": {"exit_code": 0, "value": property_value},
            "controller_chain": [
                {"path": "/sys/fs/cgroup", "inode": 1,
                 "controllers": "cpu io memory", "subtree_control": "cpu io memory"},
                {"path": "/sys/fs/cgroup/user.slice", "inode": 2,
                 "controllers": "cpu io memory", "subtree_control": "io"},
                {"path": self.cgroup, "inode": 3,
                 "controllers": "", "subtree_control": ""},
            ],
            "kernel": {
                "io.weight": "default 100" + ("\n%s %d" % (self.device, weight) if weight else ""),
                "io.bfq.weight": "default 100" + ("\n%s %d" % (self.device, bfq) if bfq else ""),
                "root.io.cost.qos": qos,
                "root.io.cost.model": "252:16 ctrl=auto model=linear",
            },
            "scheduler": "none [bfq]",
            "low_latency": "0",
        }

    def phase(self, mechanism):
        before = self.snapshot()
        if mechanism == "bfq":
            during = self.snapshot(weight=validator.WEIGHT,
                                   bfq=validator.bfq_weight(validator.WEIGHT))
        else:
            during = self.snapshot(weight=validator.WEIGHT,
                                   bfq=validator.bfq_weight(validator.WEIGHT),
                                   qos="252:16 enable=1 ctrl=auto")
        return {"name": mechanism, "request_exit_code": 0, "request_output": "",
                "reset_exit_code": 0, "reset_output": "", "before": before,
                "during": during, "after": self.snapshot()}

    def fixture(self, probe_path):
        systemd = "systemd-252-46.el9.x86_64"
        rows = {
            "bfq": {"mechanism": "bfq", "outcome": "SUPPORTED", "reason": "measured",
                    "phase": self.phase("bfq")},
            "iocost": {"mechanism": "iocost", "outcome": "SUPPORTED", "reason": "measured",
                       "phase": self.phase("iocost")},
            "simultaneous": {"mechanism": "simultaneous", "outcome": "SUPPORTED",
                             "reason": "measured", "policy_status": "mechanism_ambiguous",
                             "phase": self.phase("simultaneous")},
        }
        return {
            "schema_version": 1,
            "scope": "test-only-systemd-iodeviceweight-platform-characterization",
            "result": "CHARACTERIZED",
            "platform": "el9", "run_id": "r20260914070000-1",
            "source_revision": self.revision,
            "probe_sha256": validator.digest(probe_path), "requested_weight": validator.WEIGHT,
            "environment": {
                "os_release": 'NAME="Oracle Linux"\nVERSION_ID="9.8"\n',
                "systemd_package": systemd, "kernel": "6.12.0-test",
                "major_minor": self.device, "device": "/dev/vdb",
                "all_schedulers_before": {"/sys/block/vda/queue/scheduler": "[none]"},
            },
            "dbus": {
                "object_path": "/org/freedesktop/systemd1/unit/test",
                "interface": "org.freedesktop.systemd1.Slice", "property": "IODeviceWeight",
                "property_signature": "a(st)", "introspection_exit_code": 0, "introspection": "",
                "request": {"method": "SetUnitProperties", "signature": "sba(sv)",
                            "runtime": True, "variant_signature": "a(st)", "weight": validator.WEIGHT,
                            "device": "/dev/vdb"},
            },
            "unit": {"control_group": self.cgroup.removeprefix("/sys/fs/cgroup")},
            "transport": {"outcome": "SUPPORTED", "reason": "measured", **self.phase("transport")},
            "rows": rows,
            "cleanup": {"result": "PASS", "errors": [], "io_cost_disabled": True,
                        "property_reset": True,
                        "owned_drop_ins": [], "service_active_state": "inactive",
                        "all_schedulers_after": {"/sys/block/vda/queue/scheduler": "[none]"}},
        }

    def test_valid_guest_evidence_is_accepted(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            result = self.fixture(probe)
            row = validator.validate_guest(result, "el9", self.revision, probe)
            self.assertEqual(row["bfq"], "SUPPORTED")
            self.assertEqual(row["simultaneous_policy"], "mechanism_ambiguous")

    def test_mutations_are_rejected(self):
        mutations = {
            "signature": lambda value: value["dbus"].update(property_signature="t"),
            "revision": lambda value: value.update(source_revision="b" * 40),
            "bfq readback": lambda value: value["rows"]["bfq"]["phase"]["during"]["kernel"].update(
                {"io.bfq.weight": "default 100"}),
            "iocost disabled": lambda value: value["rows"]["iocost"]["phase"]["during"]["kernel"].update(
                {"root.io.cost.qos": "252:16 enable=0"}),
            "precedence invented": lambda value: value["rows"]["simultaneous"].update(
                policy_status="bfq_wins"),
            "scheduler leaked": lambda value: value["cleanup"]["all_schedulers_after"].update(
                {"/sys/block/vda/queue/scheduler": "none [bfq]"}),
            "drop-in leaked": lambda value: value["cleanup"].update(owned_drop_ins=["override.conf"]),
        }
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            pristine = self.fixture(probe)
            for name, mutation in mutations.items():
                with self.subTest(name=name):
                    result = copy.deepcopy(pristine)
                    mutation(result)
                    with self.assertRaises((AssertionError, RuntimeError)):
                        validator.validate_guest(result, "el9", self.revision, probe)

    def test_unsupported_is_valid_but_requires_a_reason(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            result = self.fixture(probe)
            result["rows"]["bfq"] = {
                "mechanism": "bfq", "outcome": "UNSUPPORTED", "reason": "scheduler absent"}
            result["rows"]["simultaneous"] = {
                "mechanism": "simultaneous", "outcome": "UNSUPPORTED", "reason": "BFQ absent"}
            validator.validate_guest(result, "el9", self.revision, probe)
            result["rows"]["bfq"]["reason"] = ""
            with self.assertRaises(AssertionError):
                validator.validate_guest(result, "el9", self.revision, probe)

    def test_missing_dbus_property_is_valid_unsupported_platform_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            result = self.fixture(probe)
            result["dbus"]["property_signature"] = None
            result["transport"] = {"outcome": "UNSUPPORTED", "reason": "property absent"}
            result["cleanup"]["property_reset"] = None
            result["rows"] = {
                mechanism: {"mechanism": mechanism, "outcome": "UNSUPPORTED",
                            "reason": "property absent"}
                for mechanism in ("bfq", "iocost", "simultaneous")
            }
            validator.validate_guest(result, "el9", self.revision, probe)

    def test_manifest_rejects_missing_extra_and_malformed_entries(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            retained = root / "raw.txt"
            retained.write_text("raw\n")
            manifest = root / "SHA256SUMS"
            manifest.write_text("%s  ./raw.txt\n" % validator.digest(retained))
            validator.verify_manifest(root)
            for value in ("", "0" * 64 + "  raw.txt\n", "g" * 64 + "  ./raw.txt\n"):
                with self.subTest(value=value):
                    manifest.write_text(value)
                    with self.assertRaises(AssertionError):
                        validator.verify_manifest(root)

    def test_table_keeps_unsupported_distinct_from_blocked(self):
        rendered = validator.table([{
            "platform": "el8", "os_version": "8.10", "systemd": "systemd-239",
            "kernel": "5.15", "bfq": "UNSUPPORTED", "iocost": "SUPPORTED",
            "simultaneous": "UNSUPPORTED", "simultaneous_policy": "not available",
        }], self.revision)
        self.assertIn("UNSUPPORTED", rendered)
        self.assertIn("`BLOCKED` is not published", rendered)


if __name__ == "__main__":
    unittest.main()
