#!/usr/bin/env python3
"""Mutation tests for the effect-evidence validator."""
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import validate_evidence as validator


def make_interval(device, first, second, duration_ns=10_000_000_000):
    return {"device": device, "duration_ns": duration_ns,
            "read_bytes_delta": [first, second]}


def fixture(directory, profile="qualification"):
    device = "252:16"
    profile_config = validator.PROFILES[profile]
    count = profile_config["interval_count"]
    duration = profile_config["minimum_duration_ns"]
    phases = {
        "equal": {"public_weights": [100, 100],
                  "intervals": [make_interval(device, 100, 110, duration) for _ in range(count)]},
        "unequal": {"public_weights": [100, 1000],
                    "intervals": [make_interval(device, 100, 400, duration) for _ in range(count)]},
        "reversed": {"public_weights": [1000, 100],
                     "intervals": [make_interval(device, 400, 100, duration) for _ in range(count)]},
    }
    aggregates = {name: validator.aggregate(value["intervals"], device, profile, name)
                  for name, value in phases.items()}
    mechanisms = {}
    for name in validator.MECHANISMS:
        mechanisms[name] = {
            "outcome": "EFFECT_QUALIFIED", "device": device,
            "setup": {"daemon_mutated_scheduler_or_iocost": False,
                      "owned_disposable_device": True,
                      "selected_scheduler": "bfq" if name == "bfq" else "mq-deadline",
                      "io_cost_enabled": name == "io_cost"},
            "transport": {"dbus_property": "IODeviceWeight", "dbus_signature": "a(st)",
                          "exact_readback": True, "exact_kernel_entry": True,
                          "systemd_values": [100, 10000] if name == "bfq" else [100, 1000],
                          "kernel_values": [100, 1000]},
            "phases": copy.deepcopy(phases), "reported_aggregates": copy.deepcopy(aggregates),
        }
    raw_files = {}
    for name in ("raw.json", "daemon-log.json", "systemd-journal.json", "final-prometheus.json",
                 "unit-state.json", "unit-drop-ins.json"):
        raw = directory / name
        raw.write_text('{"measured":true}\n')
        raw_files[name] = hashlib.sha256(raw.read_bytes()).hexdigest()
    for index, stage in enumerate(validator.COMPLETED_STAGES, start=1):
        name = "checkpoint-%02d-%s.json" % (index, stage)
        raw = directory / name
        raw.write_text(json.dumps({"profile": profile, "stage": stage, "status": "PASS",
                                   "completed_stages": list(validator.COMPLETED_STAGES[:index]),
                                   "time_ns": index}, sort_keys=True) + "\n")
        raw_files[name] = hashlib.sha256(raw.read_bytes()).hexdigest()
    summary = {
        "schema": 1, "scope": profile_config["scope"], "profile": profile,
        "cadence": {"interval_count": count,
                    "interval_seconds": profile_config["interval_seconds"]},
        "completed_stages": list(validator.COMPLETED_STAGES),
        "provenance": validator.PROVENANCE,
        "source": {"revision": "a" * 40, "tree": "b" * 40,
                   "qualification_revision": "d" * 40, "qualification_tree": "e" * 40},
        "package": {"identity": "resman-1.38.0-5.el9.x86_64", "sha256": "c" * 64,
                    "installed_binary_matches_payload": True},
        "platform": {"id": "ol", "version_id": "9.8",
                     "manager_version": validator.MANAGER_VERSION,
                     "kernel_release": validator.KERNEL_RELEASE,
                     "kernel_package_owner": "kernel-core-" + validator.KERNEL_RELEASE},
        "devices": [{"major_minor": device, "owned_disposable": True, "identity_stable": True},
                    {"major_minor": "252:32", "owned_disposable": True, "identity_stable": True}],
        "mechanisms": mechanisms,
        "authority": {"complete": {"coverage": "complete", "complete_users": 2,
                                     "partial_users": 1, "aggregate_coverage": "partial"},
                      "partial": {"coverage": "partial", "complete_users": 1,
                                  "partial_users": 2, "aggregate_coverage": "partial",
                                  "programmed": True}},
        "public_observability": {
            "prometheus": {"functionally_accepted": 1, "effect_qualified": 1,
                           "provenance": validator.PROVENANCE},
            "sqlite": {"schema": 9, "effect_qualified": 1, "provenance": validator.PROVENANCE}},
        "composition": {"intersection": {"weight": True, "hard_cap": True},
                        "weight_only": {"weight": True, "hard_cap": False},
                        "hard_cap_only": {"weight": False, "hard_cap": True},
                        "independent_release": True, "separate_leases": True},
        "lifecycle": {"blackout_release": "PASS", "restart_recovery": "PASS",
                      "capability_loss_release": "PASS", "compare_before_restore": "PASS"},
        "cleanup": {"result": "PASS", "scheduler_restored": True,
                    "io_cost_restored": True, "units_removed": True, "leases_removed": True},
        "raw_files": raw_files,
    }
    (directory / "summary.json").write_text(json.dumps(summary, sort_keys=True) + "\n")
    with (directory / "SHA256SUMS").open("w") as manifest:
        for path in sorted(directory.iterdir()):
            if path.name != "SHA256SUMS":
                manifest.write(hashlib.sha256(path.read_bytes()).hexdigest() + "  " + path.name + "\n")
    return summary


class EvidenceTests(unittest.TestCase):
    def test_valid_fixture(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            fixture(directory)
            validator.validate(directory, "a" * 40, "c" * 64)

    def test_valid_smoke_fixture(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            fixture(directory, "smoke")
            validator.validate(directory, "a" * 40, "c" * 64, "smoke")

    def test_smoke_allows_starved_low_weight_control_but_not_equal_or_qualification(self):
        interval = [make_interval("252:16", 0, 400, 1_000_000_000)]
        self.assertEqual(validator.aggregate(interval, "252:16", "smoke", "unequal")["bytes"],
                         [0, 400])
        with self.assertRaisesRegex(ValueError, "both sibling slices"):
            validator.aggregate(interval, "252:16", "smoke", "equal")
        qualification = [make_interval("252:16", 0, 400) for _ in range(3)]
        with self.assertRaisesRegex(ValueError, "both sibling slices"):
            validator.aggregate(qualification, "252:16", "qualification", "unequal")

    def test_mutations_are_rejected(self):
        cases = [
            lambda x: x.update(scope="adapter-only"),
            lambda x: x["package"].update(installed_binary_matches_payload=False),
            lambda x: x["platform"].update(kernel_release="later-kernel"),
            lambda x: x["mechanisms"].pop("io_cost"),
            lambda x: x["mechanisms"]["bfq"]["transport"].update(exact_kernel_entry=False),
            lambda x: x["mechanisms"]["bfq"]["setup"].update(daemon_mutated_scheduler_or_iocost=True),
            lambda x: x["mechanisms"]["io_cost"]["setup"].update(io_cost_enabled=False),
            lambda x: x["mechanisms"]["bfq"]["phases"]["equal"]["intervals"].clear(),
            lambda x: x["mechanisms"]["bfq"]["phases"]["unequal"]["intervals"][0].update(read_bytes_delta=[0, 0]),
            lambda x: x["mechanisms"]["bfq"]["phases"]["unequal"]["intervals"][0].update(read_bytes_delta=[400, 100]),
            lambda x: x["mechanisms"]["bfq"]["phases"]["reversed"].update(public_weights=[100, 1000]),
            lambda x: x["mechanisms"]["bfq"].update(reported_aggregates={}),
            lambda x: x["authority"]["partial"].update(coverage="complete"),
            lambda x: x["public_observability"]["sqlite"].update(provenance="none"),
            lambda x: x["composition"]["weight_only"].update(hard_cap=True),
            lambda x: x["lifecycle"].update(compare_before_restore="FAIL"),
            lambda x: x["cleanup"].update(io_cost_restored=False),
            lambda x: x["completed_stages"].pop(),
            lambda x: x.update(raw_files={}),
        ]
        for index, mutate in enumerate(cases):
            with self.subTest(mutation=index), tempfile.TemporaryDirectory() as tmp:
                directory = Path(tmp)
                original = fixture(directory)
                changed = copy.deepcopy(original)
                mutate(changed)
                (directory / "summary.json").write_text(json.dumps(changed, sort_keys=True) + "\n")
                with (directory / "SHA256SUMS").open("w") as manifest:
                    for path in sorted(directory.iterdir()):
                        if path.name != "SHA256SUMS":
                            manifest.write(hashlib.sha256(path.read_bytes()).hexdigest() + "  " + path.name + "\n")
                with self.assertRaises((ValueError, KeyError, ZeroDivisionError)):
                    validator.validate(directory)

    def test_manifest_mutation_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            fixture(directory)
            (directory / "raw.json").write_text("changed\n")
            with self.assertRaisesRegex(ValueError, "digest mismatch"):
                validator.validate(directory)


if __name__ == "__main__":
    unittest.main()
