#!/usr/bin/env python3
"""Independently validate an IODeviceWeight QEMU matrix evidence bundle."""

import argparse
import hashlib
import json
from pathlib import Path
import re

from guest_probe import (
    REASON_BFQ_SCHEDULER_UNAVAILABLE,
    REASON_BFQ_WEIGHT_NOT_APPLIED,
    REASON_IOCOST_INTERFACE_UNAVAILABLE,
    REASON_IOCOST_WEIGHT_NOT_APPLIED,
    REASON_MECHANISM_PREREQUISITE_UNAVAILABLE,
    REASON_SYSTEMD_PROPERTY_UNAVAILABLE,
    REASON_SYSTEMD_REQUEST_REJECTED,
    WEIGHT,
    bfq_weight,
    keyed_line,
    keyed_weight,
)


PLATFORMS = ("el8", "el9", "el10")
BASE_SHA256 = {
    "el8": "cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c",
    "el9": "b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b",
    "el10": "8e59326c4bf7cfa58a6cac404db8ed583fe3a5f4c460e2b73c64988785bb4f0f",
}
LEGACY_REASON_CODES = {
    "systemd does not expose IODeviceWeight with signature a(st)":
        REASON_SYSTEMD_PROPERTY_UNAVAILABLE,
    "systemd rejected the IODeviceWeight D-Bus request":
        REASON_SYSTEMD_REQUEST_REJECTED,
    "systemd rejected IODeviceWeight while the mechanism was active":
        REASON_SYSTEMD_REQUEST_REJECTED,
    "BFQ is unavailable on the owned virtual device":
        REASON_BFQ_SCHEDULER_UNAVAILABLE,
    "active BFQ did not expose the expected per-device weight":
        REASON_BFQ_WEIGHT_NOT_APPLIED,
    "kernel does not expose root io.cost.qos and io.cost.model":
        REASON_IOCOST_INTERFACE_UNAVAILABLE,
    "active io.cost did not expose the expected per-device weight":
        REASON_IOCOST_WEIGHT_NOT_APPLIED,
    "BFQ and io.cost did not both pass their individual systemd-to-kernel paths":
        REASON_MECHANISM_PREREQUISITE_UNAVAILABLE,
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
        if path.is_file() and path.name != "SHA256SUMS"
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


def verify_rejected_property_phase(phase, device, cgroup):
    require(phase["request_exit_code"] != 0 and phase["reset_exit_code"] == 0,
            "rejected request evidence lacks a failed request and acknowledged reset")
    for name in ("before", "during", "after"):
        snapshot = phase[name]
        require(snapshot["property"]["exit_code"] == 0,
                "D-Bus property readback failed during " + name)
        require([item["path"] for item in snapshot["controller_chain"]] == [
            "/sys/fs/cgroup", "/sys/fs/cgroup/user.slice", cgroup],
            "controller ancestry differs")
        require(all(item["inode"] for item in snapshot["controller_chain"]),
                "controller ancestry was not materialized")
    require(phase["after"]["property"]["value"].split()[-1:] == ["0"],
            "empty IODeviceWeight reset is absent from D-Bus readback")
    for filename in ("io.weight", "io.bfq.weight"):
        require(keyed_line(phase["before"]["kernel"][filename], device) ==
                keyed_line(phase["after"]["kernel"][filename], device),
                filename + " was not reset exactly")


def unsupported_reason_code(row, schema_version):
    require(row.get("reason"), "unsupported outcome lacks an explanation")
    if schema_version >= 2:
        require(row.get("reason_code"), "unsupported outcome lacks a typed reason")
        return row["reason_code"]
    code = row.get("reason_code") or LEGACY_REASON_CODES.get(row["reason"])
    require(code, "legacy unsupported outcome has an unknown reason")
    return code


def verify_unsupported_row(result, mechanism, row, rows, transport, device, cgroup,
                           schema_version):
    code = unsupported_reason_code(row, schema_version)
    phase = row.get("phase")
    if code == REASON_SYSTEMD_PROPERTY_UNAVAILABLE:
        require(result["dbus"]["property_signature"] != "a(st)" and phase is None,
                "systemd property absence is inconsistent with the retained evidence")
    elif code == REASON_SYSTEMD_REQUEST_REJECTED:
        if phase is None:
            require(transport["outcome"] == "UNSUPPORTED" and
                    unsupported_reason_code(transport, schema_version) == code,
                    "row does not inherit a demonstrated transport rejection")
        else:
            verify_rejected_property_phase(phase, device, cgroup)
            if mechanism == "bfq":
                require("[bfq]" in phase["during"]["scheduler"],
                        "BFQ request rejection lacks an active BFQ scheduler")
            elif mechanism == "iocost":
                qos = keyed_line(phase["during"]["kernel"]["root.io.cost.qos"], device)
                require(qos and "enable=1" in qos.split()[1:],
                        "io.cost request rejection lacks an active io.cost policy")
    elif code == REASON_BFQ_SCHEDULER_UNAVAILABLE:
        require(mechanism == "bfq" and phase is None and
                "bfq" not in {word.strip("[]") for word in
                              result["environment"]["scheduler_before"].split()},
                "BFQ absence is not demonstrated by the scheduler inventory")
    elif code == REASON_BFQ_WEIGHT_NOT_APPLIED:
        require(mechanism == "bfq" and phase is not None,
                "BFQ path limitation lacks a property phase")
        verify_property_phase(phase, device, result["dbus"]["request"]["device"], cgroup)
        weight_file = phase["during"]["kernel"]["io.bfq.weight"]
        require("[bfq]" in phase["during"]["scheduler"] and weight_file is not None and
                keyed_weight(weight_file, device) != bfq_weight(WEIGHT),
                "BFQ path limitation is not demonstrated")
    elif code == REASON_IOCOST_INTERFACE_UNAVAILABLE:
        require(mechanism == "iocost" and phase is None and
                all(snapshot["kernel"]["root.io.cost.qos"] is None and
                    snapshot["kernel"]["root.io.cost.model"] is None
                    for snapshot in (transport["before"], transport["during"], transport["after"])),
                "io.cost interface absence is not demonstrated")
    elif code == REASON_IOCOST_WEIGHT_NOT_APPLIED:
        require(mechanism == "iocost" and phase is not None,
                "io.cost path limitation lacks a property phase")
        verify_property_phase(phase, device, result["dbus"]["request"]["device"], cgroup)
        weight_file = phase["during"]["kernel"]["io.weight"]
        qos = keyed_line(phase["during"]["kernel"]["root.io.cost.qos"], device)
        require(weight_file is not None and qos and "enable=1" in qos.split()[1:] and
                keyed_weight(weight_file, device) != WEIGHT,
                "io.cost path limitation is not demonstrated")
    elif code == REASON_MECHANISM_PREREQUISITE_UNAVAILABLE:
        require(mechanism == "simultaneous" and phase is None and
                not (rows["bfq"]["outcome"] == rows["iocost"]["outcome"] == "SUPPORTED"),
                "simultaneous prerequisites are not demonstrated as unavailable")
    else:
        raise AssertionError("unknown unsupported reason code: " + str(code))
    return code


def validate_guest(result, platform, revision, retained_probe):
    schema_version = result["schema_version"]
    require(schema_version in {1, 2} and
            result["scope"] == "test-only-systemd-iodeviceweight-platform-characterization",
            "wrong characterization schema or scope")
    require(result["result"] == "CHARACTERIZED", "guest did not produce valid characterization")
    require(result["platform"] == platform and result["source_revision"] == revision,
            "guest provenance differs")
    require(result["probe_sha256"] == digest(retained_probe), "guest probe digest differs")
    version_match = re.search(r'(?m)^VERSION_ID="?([^"\n]+)', result["environment"]["os_release"])
    oracle_match = re.search(r'(?m)^ID="?ol"?$', result["environment"]["os_release"])
    require(version_match and oracle_match and version_match.group(1).split(".")[0] == platform[2:],
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
        if transport["outcome"] == "SUPPORTED":
            verify_property_phase(transport, device, request["device"], cgroup)
        else:
            require(transport["outcome"] == "UNSUPPORTED" and
                    unsupported_reason_code(transport, schema_version) ==
                    REASON_SYSTEMD_REQUEST_REJECTED,
                    "D-Bus transport was not exercised")
            verify_rejected_property_phase(transport, device, cgroup)
    else:
        require(transport["outcome"] == "UNSUPPORTED" and
                unsupported_reason_code(transport, schema_version) ==
                REASON_SYSTEMD_PROPERTY_UNAVAILABLE,
                "missing IODeviceWeight property was not classified as unsupported")

    rows = result["rows"]
    require(set(rows) == {"bfq", "iocost", "simultaneous"}, "mechanism row inventory differs")
    reason_codes = {}
    for mechanism, row in rows.items():
        allowed = ({"SUPPORTED", "UNSUPPORTED", "NOT_APPLICABLE"}
                   if mechanism == "simultaneous" else {"SUPPORTED", "UNSUPPORTED"})
        require(row["mechanism"] == mechanism and row["outcome"] in allowed and row["reason"],
                "invalid platform-mechanism outcome")
        if row["outcome"] == "SUPPORTED":
            verify_property_phase(row["phase"], device, request["device"], cgroup)
        else:
            reason_codes[mechanism] = verify_unsupported_row(
                result, mechanism, row, rows, transport, device, cgroup, schema_version)
    if schema_version >= 2 and reason_codes.get("simultaneous") == \
            REASON_MECHANISM_PREREQUISITE_UNAVAILABLE:
        require(rows["simultaneous"]["outcome"] == "NOT_APPLICABLE",
                "unavailable simultaneous prerequisites must be not applicable")
    if result["dbus"]["property_signature"] != "a(st)":
        require(rows["bfq"]["outcome"] == rows["iocost"]["outcome"] == "UNSUPPORTED" and
                rows["simultaneous"]["outcome"] in {"UNSUPPORTED", "NOT_APPLICABLE"},
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
    simultaneous_outcome = ("NOT_APPLICABLE"
                            if reason_codes.get("simultaneous") ==
                            REASON_MECHANISM_PREREQUISITE_UNAVAILABLE
                            else simultaneous["outcome"])
    simultaneous_policy = ("not applicable" if simultaneous_outcome == "NOT_APPLICABLE" else
                           simultaneous.get("policy_status", "not available"))
    kernel = result["environment"]["kernel"]
    kernel_family = "UEK" if "uek" in kernel.lower() else "non-UEK"
    cleanup = result["cleanup"]
    expected_property_reset = True if result["dbus"]["property_signature"] == "a(st)" else None
    require(cleanup["result"] == "PASS" and not cleanup["errors"] and
            cleanup["property_reset"] is expected_property_reset and
            cleanup["io_cost_disabled"] is True and not cleanup["owned_drop_ins"] and
            cleanup["service_active_state"] == "inactive" and
            cleanup["all_schedulers_after"] == result["environment"]["all_schedulers_before"],
            "guest cleanup is incomplete")
    return {
        "platform": "OL%s/%s" % (platform[2:], kernel_family),
        "os_version": version_match.group(1),
        "systemd": result["environment"]["systemd_package"],
        "kernel": kernel,
        "bfq": rows["bfq"]["outcome"],
        "iocost": rows["iocost"]["outcome"],
        "simultaneous": simultaneous_outcome,
        "simultaneous_policy": simultaneous_policy,
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
        "| Tested pair | Representative | systemd | Kernel | BFQ | io.cost | Both active | Policy status |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for row in rows:
        lines.append("| {platform} | Oracle Linux {os_version} | `{systemd}` | `{kernel}` | "
                     "{bfq} | {iocost} | {simultaneous} | `{simultaneous_policy}` |".format(**row))
    lines.extend(("", "Each row applies only to the exact distribution, systemd, and kernel family/version "
                       "shown. An unlisted kernel family is uncharacterized.", "",
                       "`NOT_APPLICABLE` means that BFQ and io.cost were not both individually supported, "
                       "so no simultaneous phase was run. A simultaneous `SUPPORTED` result records observed "
                       "availability only and does not establish precedence or composition.", ""))
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
