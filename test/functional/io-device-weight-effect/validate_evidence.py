#!/usr/bin/env python3
"""Validate retained packaged-daemon weighted-I/O contention evidence."""
import argparse
import hashlib
import json
from pathlib import Path
import re


PROVENANCE = "resman-nq6.40.5-ol9-rhck-20260915"
MANAGER_VERSION = "252-67.0.1.el9_8.2"
PACKAGE_IDENTITY = "resman-1.38.0-2.el9.x86_64"
KERNEL_RELEASE = "5.14.0-687.46.1.el9_8.x86_64"
MECHANISMS = ("bfq", "io_cost")
PHASES = {
    "equal": (100, 100),
    "unequal": (100, 1000),
    "reversed": (1000, 100),
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def validate_manifest(directory):
    manifest = directory / "SHA256SUMS"
    require(manifest.is_file() and not manifest.is_symlink(), "missing evidence manifest")
    declared = {}
    for line in manifest.read_text().splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  (\./)?([^\n]+)", line)
        require(match is not None, "malformed evidence manifest row")
        name = match.group(3)
        require(name != "SHA256SUMS" and name not in declared, "invalid manifest member")
        path = directory / name
        require(path.is_file() and not path.is_symlink() and path.resolve().is_relative_to(directory.resolve()),
                "manifest member is missing or unsafe")
        require(sha256(path) == match.group(1), "manifest digest mismatch: " + name)
        declared[name] = match.group(1)
    actual = {str(path.relative_to(directory)) for path in directory.rglob("*")
              if path.is_file() and path.name != "SHA256SUMS"}
    require(set(declared) == actual, "manifest does not cover the exact evidence bundle")
    return declared


def aggregate(intervals, device):
    require(len(intervals) >= 3, "each delivery phase requires at least three raw intervals")
    totals = [0, 0]
    elapsed = 0
    for item in intervals:
        require(item["device"] == device and item["duration_ns"] >= 5_000_000_000,
                "invalid delivery interval identity or duration")
        deltas = item["read_bytes_delta"]
        require(len(deltas) == 2 and all(isinstance(value, int) and value > 0 for value in deltas),
                "both sibling slices must deliver nonzero measured I/O")
        totals[0] += deltas[0]
        totals[1] += deltas[1]
        elapsed += item["duration_ns"]
    total = sum(totals)
    return {"bytes": totals, "shares": [totals[0] / total, totals[1] / total], "duration_ns": elapsed}


def validate_delivery(mechanism, evidence, devices):
    require(evidence["outcome"] == "EFFECT_QUALIFIED", mechanism + " is not effect-qualified")
    device = evidence["device"]
    require(device in devices, mechanism + " uses an unowned device")
    require(evidence["setup"]["daemon_mutated_scheduler_or_iocost"] is False,
            "daemon was credited with harness setup mutation")
    require(evidence["setup"]["owned_disposable_device"] is True,
            "delivery device is not explicitly disposable")
    require(evidence["transport"]["dbus_property"] == "IODeviceWeight" and
            evidence["transport"]["dbus_signature"] == "a(st)" and
            evidence["transport"]["exact_readback"] is True and
            evidence["transport"]["exact_kernel_entry"] is True,
            mechanism + " lacks exact property-to-kernel proof")
    expected_kernel = {"bfq": [100, 1000], "io_cost": [100, 1000]}[mechanism]
    require(evidence["transport"]["kernel_values"] == expected_kernel,
            mechanism + " has wrong kernel-domain values")
    if mechanism == "bfq":
        require(evidence["transport"]["systemd_values"] == [100, 10000],
                "BFQ systemd conversion is absent")
        require(evidence["setup"]["selected_scheduler"] == "bfq" and
                evidence["setup"]["io_cost_enabled"] is False,
                "BFQ was not the sole active mechanism")
    else:
        require(evidence["transport"]["systemd_values"] == [100, 1000],
                "io.cost systemd values differ")
        require(evidence["setup"]["selected_scheduler"] != "bfq" and
                evidence["setup"]["io_cost_enabled"] is True,
                "io.cost was not the sole active mechanism")
    computed = {}
    for name, weights in PHASES.items():
        phase = evidence["phases"][name]
        require(tuple(phase["public_weights"]) == weights, "wrong public weights in " + name)
        computed[name] = aggregate(phase["intervals"], device)
    require(0.30 <= computed["equal"]["shares"][0] <= 0.70,
            mechanism + " equal-weight control is materially asymmetric")
    unequal = computed["unequal"]["bytes"]
    reversed_values = computed["reversed"]["bytes"]
    require(unequal[1] / unequal[0] >= 1.5 and computed["unequal"]["shares"][1] >= 0.60,
            mechanism + " did not favor the higher-weight second slice")
    require(reversed_values[0] / reversed_values[1] >= 1.5 and computed["reversed"]["shares"][0] >= 0.60,
            mechanism + " reversed control did not reverse delivery")
    require(evidence["reported_aggregates"] == computed,
            mechanism + " reported delivery does not match raw intervals")


def validate(directory, expected_revision=None, expected_package_sha=None):
    directory = Path(directory)
    declared = validate_manifest(directory)
    summary = json.loads((directory / "summary.json").read_text())
    require(summary["schema"] == 1 and summary["scope"] == "packaged-daemon-controlled-contention",
            "wrong evidence schema or scope")
    require(summary["provenance"] == PROVENANCE, "wrong qualification provenance")
    require(re.fullmatch(r"[0-9a-f]{40}", summary["source"]["revision"]) is not None,
            "source revision is not immutable")
    if expected_revision is not None:
        require(summary["source"]["revision"] == expected_revision, "unexpected source revision")
    require(re.fullmatch(r"[0-9a-f]{40}", summary["source"]["tree"]) is not None,
            "source tree is not immutable")
    require(re.fullmatch(r"[0-9a-f]{40}", summary["source"]["qualification_revision"]) is not None and
            re.fullmatch(r"[0-9a-f]{40}", summary["source"]["qualification_tree"]) is not None,
            "qualification harness revision is not immutable")
    package = summary["package"]
    require(package["identity"] == PACKAGE_IDENTITY and
            re.fullmatch(r"[0-9a-f]{64}", package["sha256"]) is not None and
            package["installed_binary_matches_payload"] is True,
            "package identity or installed payload proof is invalid")
    if expected_package_sha is not None:
        require(package["sha256"] == expected_package_sha, "unexpected package digest")
    platform = summary["platform"]
    require(platform["id"] == "ol" and platform["version_id"] == "9.8" and
            platform["manager_version"] == MANAGER_VERSION and
            platform["kernel_release"] == KERNEL_RELEASE and
            platform["kernel_package_owner"].startswith("kernel-core-"),
            "platform is not the exact retained OL9/RHCK representative")
    devices = {item["major_minor"] for item in summary["devices"]
               if item["owned_disposable"] and item["identity_stable"]}
    require(len(devices) == 2, "two stable disposable device identities are required")
    require(set(summary["mechanisms"]) == set(MECHANISMS), "mechanism evidence is incomplete")
    for mechanism in MECHANISMS:
        validate_delivery(mechanism, summary["mechanisms"][mechanism], devices)
    authority = summary["authority"]
    require(authority["complete"]["coverage"] == "complete" and
            authority["complete"]["complete_users"] >= 2 and
            authority["partial"]["coverage"] == "partial" and
            authority["partial"]["complete_users"] < authority["complete"]["complete_users"] and
            authority["partial"]["partial_users"] > authority["complete"]["partial_users"] and
            authority["partial"]["programmed"] is True,
            "complete and partial authority paths were not both observed")
    public = summary["public_observability"]
    require(public["prometheus"]["functionally_accepted"] == 1 and
            public["prometheus"]["effect_qualified"] == 1 and
            public["prometheus"]["provenance"] == PROVENANCE and
            public["sqlite"]["schema"] == 9 and
            public["sqlite"]["effect_qualified"] == 1 and
            public["sqlite"]["provenance"] == PROVENANCE,
            "public functional and qualification dimensions are incomplete")
    composition = summary["composition"]
    require(composition == {
        "intersection": {"weight": True, "hard_cap": True},
        "weight_only": {"weight": True, "hard_cap": False},
        "hard_cap_only": {"weight": False, "hard_cap": True},
        "independent_release": True,
        "separate_leases": True,
    }, "W/H selector or lease independence was not proved")
    lifecycle = summary["lifecycle"]
    for name in ("blackout_release", "restart_recovery", "capability_loss_release",
                 "compare_before_restore"):
        require(lifecycle[name] == "PASS", "missing lifecycle proof: " + name)
    cleanup = summary["cleanup"]
    require(cleanup["result"] == "PASS" and cleanup["scheduler_restored"] and
            cleanup["io_cost_restored"] and cleanup["units_removed"] and
            cleanup["leases_removed"], "cleanup is incomplete")
    raw = summary["raw_files"]
    require(raw and set(raw).issubset(declared), "raw evidence references are incomplete")
    for name, digest in raw.items():
        require(declared[name] == digest, "raw evidence digest differs: " + name)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--revision")
    parser.add_argument("--package-sha")
    args = parser.parse_args()
    validate(args.directory, args.revision, args.package_sha)
    print("PASS: packaged-daemon weighted-I/O effect evidence")


if __name__ == "__main__":
    main()
