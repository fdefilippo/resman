#!/usr/bin/env python3
"""Independently validate an IODeviceWeight QEMU matrix evidence bundle."""

import argparse
import hashlib
import json
from pathlib import Path
import re

from guest_probe import WEIGHT, bfq_weight, keyed_line, keyed_weight


PLATFORMS = ("el8", "el9", "el10")
BASE_SHA256 = {
    "el8": "cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c",
    "el9": "b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b",
    "el10": "8e59326c4bf7cfa58a6cac404db8ed583fe3a5f4c460e2b73c64988785bb4f0f",
}


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
        require(name.startswith("./") and name not in entries and ".." not in Path(name).parts,
                "unsafe or duplicate manifest path")
        entries[name] = value
    actual = {
        "./" + str(path.relative_to(root)): digest(path)
        for path in root.rglob("*")
        if path.is_file() and path != manifest
    }
    require(entries == actual, "evidence files and SHA256SUMS differ: " + str(root))


def words(value):
    return set(value.split()) if isinstance(value, str) else set()


def verify_property_phase(phase, device, property_device, cgroup):
    require(phase["request_exit_code"] == 0 and phase["reset_exit_code"] == 0,
            "successful row lacks an acknowledged request/reset")
    for name in ("before", "during", "after"):
        snapshot = phase[name]
        require(snapshot["property"]["exit_code"] == 0,
                "D-Bus property readback failed during " + name)
        chain = snapshot["controller_chain"]
        require([item["path"] for item in chain] == [
            "/sys/fs/cgroup", "/sys/fs/cgroup/user.slice", cgroup],
            "controller ancestry differs")
        require(all(item["inode"] for item in chain), "controller ancestry was not materialized")
    require(str(WEIGHT) in phase["during"]["property"]["value"] and
            property_device in phase["during"]["property"]["value"],
            "requested IODeviceWeight is absent from D-Bus readback")
    require(phase["after"]["property"]["value"].split()[-1:] == ["0"],
            "empty IODeviceWeight reset is absent from D-Bus readback")
    require("io" in words(phase["during"]["controller_chain"][0]["subtree_control"]),
            "root did not enable the io controller for user.slice")
    require("io" in words(phase["during"]["controller_chain"][1]["controllers"]),
            "user.slice did not receive the io controller")
    for filename in ("io.weight", "io.bfq.weight"):
        require(keyed_line(phase["before"]["kernel"][filename], device) ==
                keyed_line(phase["after"]["kernel"][filename], device),
                filename + " was not reset exactly")


def validate_guest(result, platform, revision, retained_probe):
    require(result["schema_version"] == 1 and
            result["scope"] == "test-only-systemd-iodeviceweight-platform-characterization",
            "wrong characterization schema or scope")
    require(result["result"] == "CHARACTERIZED", "guest did not produce valid characterization")
    require(result["platform"] == platform and result["source_revision"] == revision,
            "guest provenance differs")
    require(result["probe_sha256"] == digest(retained_probe), "guest probe digest differs")
    version_match = re.search(r'(?m)^VERSION_ID="?([^"\n]+)', result["environment"]["os_release"])
    require(version_match and version_match.group(1).split(".")[0] == platform[2:],
            "guest distribution major version differs")
    require(result["environment"]["systemd_package"], "systemd package identity is absent")
    require(result["requested_weight"] == WEIGHT, "unexpected characterization weight")
    require(result["dbus"]["interface"] == "org.freedesktop.systemd1.Slice" and
            result["dbus"]["property"] == "IODeviceWeight",
            "IODeviceWeight D-Bus property identity differs")
    request = result["dbus"]["request"]
    require(request["method"] == "SetUnitProperties" and request["signature"] == "sba(sv)" and
            request["runtime"] is True and request["variant_signature"] == "a(st)" and
            request["weight"] == WEIGHT,
            "D-Bus request contract differs")
    device = result["environment"]["major_minor"]
    require(re.fullmatch(r"[0-9]+:[0-9]+", device) is not None and
            request["device"] == result["environment"]["device"],
            "owned device identity differs")
    cgroup = "/sys/fs/cgroup" + result["unit"]["control_group"]
    require(result["unit"]["control_group"].startswith("/user.slice/user-resman_iow_"),
            "probe slice is not directly below user.slice")
    transport = result["transport"]
    if result["dbus"]["property_signature"] == "a(st)":
        require(transport["outcome"] == "SUPPORTED", "D-Bus transport was not exercised")
        verify_property_phase(transport, device, request["device"], cgroup)
    else:
        require(transport["outcome"] == "UNSUPPORTED" and transport["reason"],
                "missing IODeviceWeight property was not classified as unsupported")

    rows = result["rows"]
    require(set(rows) == {"bfq", "iocost", "simultaneous"}, "mechanism row inventory differs")
    for mechanism, row in rows.items():
        require(row["mechanism"] == mechanism and row["outcome"] in {"SUPPORTED", "UNSUPPORTED"} and
                row["reason"], "invalid platform-mechanism outcome")
        if row["outcome"] == "SUPPORTED":
            verify_property_phase(row["phase"], device, request["device"], cgroup)
    if result["dbus"]["property_signature"] != "a(st)":
        require({row["outcome"] for row in rows.values()} == {"UNSUPPORTED"},
                "mechanism support was claimed without the required D-Bus property")
    if rows["bfq"]["outcome"] == "SUPPORTED":
        phase = rows["bfq"]["phase"]
        require("[bfq]" in phase["during"]["scheduler"], "BFQ was not selected")
        require(keyed_weight(phase["during"]["kernel"]["io.bfq.weight"], device) == bfq_weight(WEIGHT),
                "BFQ did not read the expected per-device value")
    if rows["iocost"]["outcome"] == "SUPPORTED":
        phase = rows["iocost"]["phase"]
        require(keyed_weight(phase["during"]["kernel"]["io.weight"], device) == WEIGHT,
                "io.cost did not read the expected per-device value")
        qos = keyed_line(phase["during"]["kernel"]["root.io.cost.qos"], device)
        require(qos and "enable=1" in qos.split()[1:], "io.cost was not enabled")
    simultaneous = rows["simultaneous"]
    if simultaneous["outcome"] == "SUPPORTED":
        require(rows["bfq"]["outcome"] == rows["iocost"]["outcome"] == "SUPPORTED",
                "simultaneous row lacks both individual mechanisms")
        require(simultaneous.get("policy_status") == "mechanism_ambiguous",
                "simultaneous mechanisms were assigned an unproved precedence")
    cleanup = result["cleanup"]
    expected_property_reset = True if result["dbus"]["property_signature"] == "a(st)" else None
    require(cleanup["result"] == "PASS" and not cleanup["errors"] and
            cleanup["property_reset"] is expected_property_reset and
            cleanup["io_cost_disabled"] is True and not cleanup["owned_drop_ins"] and
            cleanup["service_active_state"] == "inactive" and
            cleanup["all_schedulers_after"] == result["environment"]["all_schedulers_before"],
            "guest cleanup is incomplete")
    return {
        "platform": platform,
        "os_version": version_match.group(1),
        "systemd": result["environment"]["systemd_package"],
        "kernel": result["environment"]["kernel"],
        "bfq": rows["bfq"]["outcome"],
        "iocost": rows["iocost"]["outcome"],
        "simultaneous": rows["simultaneous"]["outcome"],
        "simultaneous_policy": rows["simultaneous"].get("policy_status", "not available"),
    }


def validate(root, revision):
    root = Path(root)
    require(re.fullmatch(r"[0-9a-f]{40}", revision) is not None, "full revision required")
    verify_manifest(root)
    matrix = fields(root / "matrix.txt")
    require(matrix["source_revision"] == revision and matrix["result"] == "PASS" and
            matrix["exit_code"] == "0", "matrix provenance or result differs")
    require((root / "result").read_text().strip() == "PASS", "matrix terminal result differs")
    rows = []
    for platform in PLATFORMS:
        platform_root = root / platform
        verify_manifest(platform_root)
        environment = fields(platform_root / "environment.txt")
        require(environment["platform"] == platform and environment["source_revision"] == revision,
                "platform provenance differs")
        require(environment["base_sha256"] == BASE_SHA256[platform], "base image digest differs")
        require(environment["result"] == environment["cleanup"] == "PASS" and
                environment["exit_code"] == "0" and environment["guest_exit_code"] == "0",
                "platform run or cleanup did not pass")
        require((platform_root / "result").read_text().strip() == "PASS",
                "platform terminal result differs")
        guest = json.loads((platform_root / "guest/result.json").read_text())
        rows.append(validate_guest(guest, platform, revision,
                                   platform_root / "guest-probe.py"))
    return rows


def table(rows, revision):
    lines = [
        "# IODeviceWeight platform characterization",
        "",
        "Source revision: `%s`. `UNSUPPORTED` is a valid platform result; "
        "`BLOCKED` is not published by this table." % revision,
        "",
        "| Platform | Representative | systemd | Kernel | BFQ | io.cost | Both active | Policy status |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for row in rows:
        lines.append("| {platform} | Oracle Linux {os_version} | `{systemd}` | `{kernel}` | "
                     "{bfq} | {iocost} | {simultaneous} | `{simultaneous_policy}` |".format(**row))
    lines.extend(("", "The simultaneous row records observed availability only. It does not establish "
                       "precedence or composition between BFQ and io.cost.", ""))
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence", type=Path)
    parser.add_argument("revision")
    parser.add_argument("--table", type=Path)
    args = parser.parse_args()
    rows = validate(args.evidence, args.revision)
    if args.table:
        args.table.write_text(table(rows, args.revision))


if __name__ == "__main__":
    main()
