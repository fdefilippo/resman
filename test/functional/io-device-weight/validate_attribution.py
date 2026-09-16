#!/usr/bin/env python3
"""Validate retained OL8/UEK direct BFQ attribution evidence."""

import argparse
import hashlib
import json
from pathlib import Path
import re

from guest_attribution import BFQ_WEIGHT, keyed_line, keyed_weight


BASE_SHA256 = "cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c"
OUTCOMES = frozenset({"KERNEL_ACCEPTS", "KERNEL_LIMIT"})


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def fields(path):
    result = {}
    for line in Path(path).read_text().splitlines():
        key, separator, value = line.partition("=")
        require(separator and key and key not in result,
                "invalid or duplicate field in " + str(path))
        result[key] = value
    return result


def verify_manifest(root):
    root = Path(root)
    manifest = root / "SHA256SUMS"
    require(manifest.is_file(), "evidence manifest is absent: " + str(root))
    entries = {}
    for line in manifest.read_text().splitlines():
        value, separator, name = line.partition("  ")
        require(separator and re.fullmatch(r"[0-9a-f]{64}", value),
                "malformed evidence manifest")
        require(name.startswith("./") and name not in entries and
                ".." not in Path(name).parts,
                "unsafe or duplicate manifest path")
        entries[name] = value
    actual = {
        "./" + str(path.relative_to(root)): digest(path)
        for path in root.rglob("*")
        if path.is_file() and path.name != "SHA256SUMS"
    }
    require(entries == actual,
            "evidence files and SHA256SUMS differ: " + str(root))


def validate(root, revision):
    root = Path(root)
    require(re.fullmatch(r"[0-9a-f]{40}", revision) is not None,
            "full revision required")
    verify_manifest(root)
    matrix = fields(root / "matrix.txt")
    require(matrix["source_revision"] == revision and
            matrix["campaign"] == "ol8-uek-direct-bfq-attribution" and
            matrix["result"] == "PASS" and matrix["exit_code"] == "0",
            "attribution campaign provenance or result differs")
    require((root / "result").read_text().strip() == "PASS",
            "campaign terminal result differs")

    platform = root / "el8"
    verify_manifest(platform)
    environment = fields(platform / "environment.txt")
    require(environment["platform"] == "el8" and
            environment["kernel_family"] == "uek" and
            environment["source_revision"] == revision and
            environment["base_sha256"] == BASE_SHA256,
            "OL8/UEK representative identity differs")
    require(environment["result"] == environment["cleanup"] == "PASS" and
            environment["exit_code"] == environment["guest_exit_code"] == "0",
            "platform run or cleanup did not pass")
    require((platform / "result").read_text().strip() == "PASS",
            "platform terminal result differs")

    retained_probe = platform / "guest-attribution.py"
    result = json.loads((platform / "guest" / "result.json").read_text())
    require(result["schema_version"] == 1 and
            result["scope"] == "test-only-ol8-uek-bfq-direct-write-attribution" and
            result["result"] == "ATTRIBUTED",
            "guest did not produce attribution evidence")
    require(result["source_revision"] == revision and
            result["probe_sha256"] == digest(retained_probe),
            "guest probe provenance differs")
    environment_record = result["environment"]
    version_match = re.search(
        r'(?m)^VERSION_ID="?([^"\n]+)', environment_record["os_release"])
    require("uek" in environment_record["kernel"].lower() and
            environment_record["kernel_package"].startswith("kernel-uek-core-") and
            environment_record["systemd_package"].startswith("systemd-239-") and
            re.search(r'(?m)^ID="?ol"?$', environment_record["os_release"]) and
            version_match and version_match.group(1).split(".")[0] == "8",
            "guest is not the required OL8/systemd239/UEK representative")
    device = environment_record["major_minor"]
    require(re.fullmatch(r"[0-9]+:[0-9]+", device) is not None,
            "owned device identity is invalid")

    config = environment_record["kernel_config"]
    if config["status"] == "captured":
        retained_config = platform / "guest" / config["retained_file"]
        require(retained_config.is_file() and digest(retained_config) == config["sha256"],
                "retained kernel configuration differs")
        require(set(config["options"]) == {
            "CONFIG_BFQ_GROUP_IOSCHED", "CONFIG_BLK_CGROUP_IOCOST"},
            "kernel option inventory differs")
        require(all(value in {"y", "m", "not_set", "absent"}
                    for value in config["options"].values()),
                "kernel option value is invalid")
    else:
        require(config == {
            "status": "unavailable",
            "checked_sources": ["/boot/config-" + environment_record["kernel"],
                                "/proc/config.gz"]},
                "unavailable kernel configuration is not proved")

    direct = result["direct_bfq"]
    require(direct["outcome"] in OUTCOMES and direct["major_minor"] == device and
            direct["expected_weight"] == BFQ_WEIGHT and
            direct["cgroup"].startswith("/sys/fs/cgroup/resman-iow-direct-") and
            direct["weight_file"] == direct["cgroup"] + "/io.bfq.weight" and
            "[bfq]" in direct["scheduler_during"],
            "direct BFQ attribution identity differs")
    if direct["outcome"] == "KERNEL_ACCEPTS":
        require(direct["write"]["error"] is None and
                direct["write"]["request"] == "%s %d\n" % (device, BFQ_WEIGHT) and
                keyed_weight(direct["during"], device) == BFQ_WEIGHT and
                direct["reset"] is not None and direct["reset"]["error"] is None and
                direct["reset"]["request"] == device + " default\n" and
                keyed_line(direct["after"], device) ==
                keyed_line(direct["before"], device),
                "accepted direct BFQ write lacks exact readback or reset")
    else:
        require(direct["write"] is None or direct["write"]["error"] is not None or
                keyed_weight(direct["during"], device) != BFQ_WEIGHT,
                "kernel limitation contradicts the direct write evidence")

    cleanup = result["cleanup"]
    require(cleanup["result"] == "PASS" and not cleanup["errors"] and
            cleanup["cgroup_removed"] is True and
            cleanup["root_subtree_after"] == environment_record["root_subtree_before"] and
            cleanup["scheduler_after"] == environment_record["scheduler_before"],
            "guest cleanup is incomplete")
    return direct["outcome"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence", type=Path)
    parser.add_argument("revision")
    args = parser.parse_args()
    print(validate(args.evidence, args.revision))


if __name__ == "__main__":
    main()
