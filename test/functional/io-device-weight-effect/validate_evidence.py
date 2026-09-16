#!/usr/bin/env python3
"""Validate retained packaged-daemon weighted-I/O contention evidence."""
import argparse
import hashlib
import json
from pathlib import Path
import re


PROVENANCE = "resman-nq6.40.5-ol9-rhck-20260915"
MANAGER_VERSION = "252-67.0.1.el9_8.2"
PACKAGE_IDENTITY = "resman-1.38.0-8.el9.x86_64"
KERNEL_RELEASE = "5.14.0-687.46.1.el9_8.x86_64"
MECHANISMS = ("bfq", "io_cost")
PHASES = {
    "equal": (100, 100),
    "unequal": (100, 1000),
    "reversed": (1000, 100),
}
PROFILES = {
    "smoke": {"interval_count": 1, "minimum_duration_ns": 1_500_000_000,
              "interval_seconds": 2, "settle_seconds": 2,
              "scope": "packaged-daemon-controlled-contention-smoke"},
    "qualification": {"interval_count": 3, "minimum_duration_ns": 5_000_000_000,
                      "interval_seconds": 10, "settle_seconds": 2,
                      "scope": "packaged-daemon-controlled-contention"},
}
COMPLETED_STAGES = ("preflight", "workloads", "io_cost", "bfq", "authority", "public",
                    "composition", "lifecycle", "release")


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


def aggregate(intervals, device, profile="qualification", phase="equal"):
    profile_config = PROFILES[profile]
    require(len(intervals) >= profile_config["interval_count"],
            "delivery phase has too few raw intervals")
    totals = [0, 0]
    elapsed = 0
    for item in intervals:
        require(item["device"] == device and
                item["duration_ns"] >= profile_config["minimum_duration_ns"],
                "invalid delivery interval identity or duration")
        deltas = item["read_bytes_delta"]
        require(len(deltas) == 2 and all(isinstance(value, int) for value in deltas),
                "delivery values must be two integers")
        if profile == "qualification" or phase == "equal":
            require(all(value > 0 for value in deltas),
                    "both sibling slices must deliver nonzero measured I/O")
        else:
            require(all(value >= 0 for value in deltas) and sum(deltas) > 0,
                    "smoke control must deliver measured I/O")
        totals[0] += deltas[0]
        totals[1] += deltas[1]
        elapsed += item["duration_ns"]
    total = sum(totals)
    return {"bytes": totals, "shares": [totals[0] / total, totals[1] / total], "duration_ns": elapsed}


def validate_delivery(mechanism, evidence, devices, profile):
    require(evidence["outcome"] == "MEASURED", mechanism + " is not a completed measurement")
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
        require(phase["settle_duration_ns"] >= PROFILES[profile]["settle_seconds"] * 1_000_000_000,
                "delivery phase lacks the declared scheduler settling interval")
        computed[name] = aggregate(phase["intervals"], device, profile, name)
    require(0.30 <= computed["equal"]["shares"][0] <= 0.70,
            mechanism + " equal-weight control is materially asymmetric")
    for interval in evidence["phases"]["unequal"]["intervals"]:
        require(interval["read_bytes_delta"][1] >= 1.5 * interval["read_bytes_delta"][0],
                mechanism + " unequal-weight effect is not stable in every interval")
    for interval in evidence["phases"]["reversed"]["intervals"]:
        require(interval["read_bytes_delta"][0] >= 1.5 * interval["read_bytes_delta"][1],
                mechanism + " reversed effect is not stable in every interval")
    unequal = computed["unequal"]["bytes"]
    reversed_values = computed["reversed"]["bytes"]
    require(unequal[1] >= 1.5 * unequal[0] and computed["unequal"]["shares"][1] >= 0.60,
            mechanism + " did not favor the higher-weight second slice")
    require(reversed_values[0] >= 1.5 * reversed_values[1] and computed["reversed"]["shares"][0] >= 0.60,
            mechanism + " reversed control did not reverse delivery")
    require(evidence["reported_aggregates"] == computed,
            mechanism + " reported delivery does not match raw intervals")


def validate(directory, expected_revision=None, expected_package_sha=None,
             profile="qualification", mechanisms=None, expected_provenance=PROVENANCE,
             expected_source_tree=None, expected_package_identity=PACKAGE_IDENTITY,
             expected_platform=None):
    require(profile in PROFILES, "unknown evidence profile")
    profile_config = PROFILES[profile]
    directory = Path(directory)
    declared = validate_manifest(directory)
    summary = json.loads((directory / "summary.json").read_text())
    require(summary["schema"] == 1 and summary["scope"] == profile_config["scope"] and
            summary["profile"] == profile,
            "wrong evidence schema or scope")
    require(summary["cadence"] == {"interval_count": profile_config["interval_count"],
                                    "interval_seconds": profile_config["interval_seconds"],
                                    "settle_seconds": profile_config["settle_seconds"]},
            "wrong evidence cadence")
    require(tuple(summary["completed_stages"]) == COMPLETED_STAGES,
            "campaign checkpoints are incomplete")
    require(summary["provenance"] == expected_provenance, "wrong qualification provenance")
    require(re.fullmatch(r"[0-9a-f]{40}", summary["source"]["revision"]) is not None,
            "source revision is not immutable")
    if expected_revision is not None:
        require(summary["source"]["revision"] == expected_revision, "unexpected source revision")
    require(re.fullmatch(r"[0-9a-f]{40}", summary["source"]["tree"]) is not None,
            "source tree is not immutable")
    if expected_source_tree is not None:
        require(summary["source"]["tree"] == expected_source_tree, "unexpected source tree")
    require(re.fullmatch(r"[0-9a-f]{40}", summary["source"]["qualification_revision"]) is not None and
            re.fullmatch(r"[0-9a-f]{40}", summary["source"]["qualification_tree"]) is not None,
            "qualification harness revision is not immutable")
    package = summary["package"]
    require(package["identity"] == expected_package_identity and
            re.fullmatch(r"[0-9a-f]{64}", package["sha256"]) is not None and
            package["installed_binary_matches_payload"] is True,
            "package identity or installed payload proof is invalid")
    allowed_verification = {"clean"} if profile == "smoke" else {"clean", "config_mtime_only"}
    require(package["verification"] in allowed_verification,
            "package verification does not match the campaign profile")
    if expected_package_sha is not None:
        require(package["sha256"] == expected_package_sha, "unexpected package digest")
    platform = summary["platform"]
    if expected_platform is None:
        expected_platform = {"id": "ol", "version_id": "9.8",
                             "manager_version": MANAGER_VERSION,
                             "kernel_release": KERNEL_RELEASE}
    require(platform["id"] == expected_platform["id"] and
            platform["version_id"] == expected_platform["version_id"] and
            platform["manager_version"] == expected_platform["manager_version"] and
            platform["kernel_release"] == expected_platform["kernel_release"] and
            platform["kernel_package_owner"].startswith("kernel-core-"),
            "platform is not the exact retained OL9/RHCK representative")
    devices = {item["major_minor"] for item in summary["devices"]
               if item["owned_disposable"] and item["identity_stable"]}
    require(len(devices) == 2, "two stable disposable device identities are required")
    selected_mechanisms = tuple(MECHANISMS if mechanisms is None else mechanisms)
    require(selected_mechanisms and set(selected_mechanisms).issubset(MECHANISMS),
            "unknown or empty mechanism selection")
    require(set(selected_mechanisms).issubset(summary["mechanisms"]),
            "selected mechanism evidence is incomplete")
    for mechanism in selected_mechanisms:
        validate_delivery(mechanism, summary["mechanisms"][mechanism], devices, profile)
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
            public["prometheus"]["effect_qualified"] == 0 and
            public["prometheus"]["provenance"] == "none" and
            public["sqlite"]["schema"] == 9 and
            public["sqlite"]["effect_qualified"] == 0 and
            public["sqlite"]["provenance"] == "none",
            "first-campaign public state pre-claims effect qualification")
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
            cleanup["leases_removed"] and cleanup["kernel_weights_removed"] and
            cleanup["systemd_weights_removed"], "cleanup is incomplete")
    raw = summary["raw_files"]
    require({"daemon-log.json", "systemd-journal.json", "capability-probe-journal.json",
             "final-prometheus.json",
             "unit-state.json", "unit-drop-ins.json"}.issubset(raw) and
            set(raw).issubset(declared), "raw evidence references are incomplete")
    for index, stage in enumerate(COMPLETED_STAGES, start=1):
        name = "checkpoint-%02d-%s.json" % (index, stage)
        require(name in raw, "missing retained campaign checkpoint: " + stage)
        checkpoint = json.loads((directory / name).read_text())
        require(checkpoint["profile"] == profile and checkpoint["stage"] == stage and
                checkpoint["status"] == "PASS" and
                tuple(checkpoint["completed_stages"]) == COMPLETED_STAGES[:index],
                "invalid retained campaign checkpoint: " + stage)
    for name, digest in raw.items():
        require(declared[name] == digest, "raw evidence digest differs: " + name)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--revision")
    parser.add_argument("--source-tree")
    parser.add_argument("--package-identity", default=PACKAGE_IDENTITY)
    parser.add_argument("--package-sha")
    parser.add_argument("--provenance", default=PROVENANCE)
    parser.add_argument("--mechanism", action="append", choices=MECHANISMS)
    parser.add_argument("--distribution-id", default="ol")
    parser.add_argument("--distribution-version", default="9.8")
    parser.add_argument("--manager-version", default=MANAGER_VERSION)
    parser.add_argument("--kernel-release", default=KERNEL_RELEASE)
    parser.add_argument("--profile", choices=tuple(PROFILES), default="qualification")
    args = parser.parse_args()
    mechanisms = tuple(args.mechanism) if args.mechanism else MECHANISMS
    expected_platform = {"id": args.distribution_id,
                         "version_id": args.distribution_version,
                         "manager_version": args.manager_version,
                         "kernel_release": args.kernel_release}
    validate(args.directory, args.revision, args.package_sha, args.profile, mechanisms,
             args.provenance, args.source_tree, args.package_identity, expected_platform)
    print("PASS: packaged-daemon weighted-I/O %s evidence for %s" %
          (args.profile, ",".join(mechanisms)))


if __name__ == "__main__":
    main()
