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
            "schema_version": 2,
            "scope": "test-only-systemd-iodeviceweight-platform-characterization",
            "result": "CHARACTERIZED",
            "platform": "el9", "run_id": "r20260914070000-1",
            "source_revision": self.revision,
            "probe_sha256": validator.digest(probe_path), "requested_weight": validator.WEIGHT,
            "environment": {
                "os_release": 'NAME="Oracle Linux"\nID="ol"\nVERSION_ID="9.8"\n',
                "systemd_package": systemd, "kernel": "6.12.0-test",
                "major_minor": self.device, "device": "/dev/vdb",
                "scheduler_before": "[none] mq-deadline bfq",
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

    def test_rhck_evidence_requires_and_labels_a_non_uek_kernel(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            result = self.fixture(probe)
            row = validator.validate_guest(result, "el9", self.revision, probe, "rhck")
            self.assertEqual(row["platform"], "OL9/RHCK")

            result["environment"]["kernel"] = "6.12.0-test.el9uek"
            with self.assertRaisesRegex(AssertionError, "RHCK evidence booted UEK"):
                validator.validate_guest(result, "el9", self.revision, probe, "rhck")

    def test_mutations_are_rejected(self):
        mutations = {
            "signature": lambda value: value["dbus"].update(property_signature="t"),
            "revision": lambda value: value.update(source_revision="b" * 40),
            "bfq readback": lambda value: value["rows"]["bfq"]["phase"]["during"]["kernel"].update(
                {"io.bfq.weight": "default 100"}),
            "iocost disabled": lambda value: value["rows"]["iocost"]["phase"]["during"]["kernel"].update(
                {"root.io.cost.qos": "252:16 enable=0"}),
            "simultaneous BFQ absent": lambda value: (
                value["rows"]["simultaneous"]["phase"]["during"].update(
                    scheduler="[none] mq-deadline bfq"),
                value["rows"]["simultaneous"]["phase"]["during"]["kernel"].update(
                    {"io.bfq.weight": "default 100"})),
            "simultaneous io.cost absent": lambda value: (
                value["rows"]["simultaneous"]["phase"]["during"]["kernel"].update(
                    {"root.io.cost.qos": "252:16 enable=0"}),
                value["rows"]["simultaneous"]["phase"]["during"]["kernel"].update(
                    {"io.weight": "default 100"})),
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

    def test_bfq_unsupported_requires_the_measured_failed_path(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            result = self.fixture(probe)
            phase = self.phase("bfq")
            phase["during"]["kernel"]["io.bfq.weight"] = "default 100"
            result["rows"]["bfq"] = {
                "mechanism": "bfq", "outcome": "UNSUPPORTED",
                "reason_code": validator.REASON_BFQ_WEIGHT_NOT_APPLIED,
                "reason": "active BFQ did not expose the expected per-device weight",
                "phase": phase}
            result["rows"]["simultaneous"] = {
                "mechanism": "simultaneous", "outcome": "NOT_APPLICABLE",
                "reason_code": validator.REASON_MECHANISM_PREREQUISITE_UNAVAILABLE,
                "reason": "BFQ and io.cost did not both pass their individual systemd-to-kernel paths"}
            validator.validate_guest(result, "el9", self.revision, probe)
            mutations = {
                "phase absent": lambda value: value["rows"]["bfq"].pop("phase"),
                "BFQ not selected": lambda value: value["rows"]["bfq"]["phase"]["during"].update(
                    scheduler="[none] mq-deadline bfq"),
                "tuple not read back": lambda value: value["rows"]["bfq"]["phase"]["during"][
                    "property"].update(value="a(st) 0"),
                "request failed": lambda value: value["rows"]["bfq"]["phase"].update(
                    request_exit_code=1),
            }
            for name, mutation in mutations.items():
                with self.subTest(name=name):
                    changed = copy.deepcopy(result)
                    mutation(changed)
                    with self.assertRaises(AssertionError):
                        validator.validate_guest(changed, "el9", self.revision, probe)

    def test_vacuous_bfq_unsupported_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            result = self.fixture(probe)
            result["schema_version"] = 1
            result["rows"]["bfq"] = {
                "mechanism": "bfq", "outcome": "UNSUPPORTED",
                "reason": "active BFQ did not expose the expected per-device weight"}
            result["rows"]["simultaneous"] = {
                "mechanism": "simultaneous", "outcome": "UNSUPPORTED",
                "reason": "BFQ and io.cost did not both pass their individual systemd-to-kernel paths"}
            with self.assertRaises(AssertionError):
                validator.validate_guest(result, "el9", self.revision, probe)

    def test_iocost_unsupported_requires_typed_coherent_observations(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            result = self.fixture(probe)
            for name in ("before", "during", "after"):
                result["transport"][name]["kernel"]["root.io.cost.qos"] = None
                result["transport"][name]["kernel"]["root.io.cost.model"] = None
            result["rows"]["iocost"] = {
                "mechanism": "iocost", "outcome": "UNSUPPORTED",
                "reason_code": validator.REASON_IOCOST_INTERFACE_UNAVAILABLE,
                "reason": "kernel does not expose root io.cost.qos and io.cost.model"}
            result["rows"]["simultaneous"] = {
                "mechanism": "simultaneous", "outcome": "NOT_APPLICABLE",
                "reason_code": validator.REASON_MECHANISM_PREREQUISITE_UNAVAILABLE,
                "reason": "BFQ and io.cost did not both pass their individual systemd-to-kernel paths"}
            validator.validate_guest(result, "el9", self.revision, probe)
            result["schema_version"] = 1
            result["rows"]["iocost"].pop("reason_code")
            result["rows"]["iocost"]["reason"] = "not tried"
            with self.assertRaises(AssertionError):
                validator.validate_guest(result, "el9", self.revision, probe)

    def test_missing_dbus_property_is_valid_unsupported_platform_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = Path(directory) / "guest_probe.py"
            probe.write_text("probe\n")
            result = self.fixture(probe)
            result["dbus"]["property_signature"] = None
            result["transport"] = {
                "outcome": "UNSUPPORTED",
                "reason_code": validator.REASON_SYSTEMD_PROPERTY_UNAVAILABLE,
                "reason": "systemd does not expose IODeviceWeight with signature a(st)"}
            result["cleanup"]["property_reset"] = None
            result["rows"] = {
                "bfq": {"mechanism": "bfq", "outcome": "UNSUPPORTED",
                        "reason_code": validator.REASON_SYSTEMD_PROPERTY_UNAVAILABLE,
                        "reason": "systemd does not expose IODeviceWeight with signature a(st)"},
                "iocost": {"mechanism": "iocost", "outcome": "UNSUPPORTED",
                           "reason_code": validator.REASON_SYSTEMD_PROPERTY_UNAVAILABLE,
                           "reason": "systemd does not expose IODeviceWeight with signature a(st)"},
                "simultaneous": {
                    "mechanism": "simultaneous", "outcome": "NOT_APPLICABLE",
                    "reason_code": validator.REASON_MECHANISM_PREREQUISITE_UNAVAILABLE,
                    "reason": "BFQ and io.cost did not both pass their individual systemd-to-kernel paths"},
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
            "platform": "OL8/UEK", "os_version": "8.10", "systemd": "systemd-239",
            "kernel": "5.15", "bfq": "UNSUPPORTED", "iocost": "SUPPORTED",
            "simultaneous": "NOT_APPLICABLE", "simultaneous_policy": "not available",
        }], self.revision)
        self.assertIn("UNSUPPORTED", rendered)
        self.assertIn("NOT_APPLICABLE", rendered)
        self.assertIn("no claim crosses those line coordinates", rendered)
        self.assertIn("`BLOCKED` is not published", rendered)


if __name__ == "__main__":
    unittest.main()
