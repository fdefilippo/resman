#!/usr/bin/env python3
"""Independently validate one EL8 systemd 239 QEMU evidence bundle."""
import hashlib
import json
from pathlib import Path
import re
import sys


REQUIRED_CONTROLLERS = {"cpu", "io", "memory"}
REQUIRED_PSI = {"cpu", "io", "memory"}
BASE_SHA256 = "cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c"
PACKAGE_IDENTITY = "resman-1.36.6-5.el8.x86_64"


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def fields(path):
    result = {}
    for line in Path(path).read_text().splitlines():
        key, separator, value = line.partition("=")
        require(separator and key and key not in result, "invalid or duplicate field in " + str(path))
        result[key] = value
    return result


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def verify_manifest(root):
    manifest = root / "SHA256SUMS"
    require(manifest.is_file(), "evidence manifest is absent")
    entries = {}
    for line in manifest.read_text().splitlines():
        value, separator, name = line.partition("  ")
        require(separator and re.fullmatch(r"[0-9a-f]{64}", value), "malformed evidence manifest")
        require(name.startswith("./") and name not in entries, "unsafe or duplicate manifest path")
        entries[name] = value
    actual = {"./" + str(path.relative_to(root)): digest(path)
              for path in root.rglob("*") if path.is_file() and path != manifest}
    require(entries == actual, "evidence files and SHA256SUMS differ")


def all_pass(path, required=None):
    values = json.loads(Path(path).read_text())
    if required is not None:
        require(set(values) == set(required), "check inventory differs in " + str(path))
    require(values and set(values.values()) == {"PASS"}, "non-PASS check in " + str(path))
    return values


def verify_revision_provenance(environment, build, qualification_revision):
    require(environment["qualification_revision"] == qualification_revision,
            "qualification revision differs across evidence")
    require(environment["source_revision"] == build["source_revision"],
            "package source revision differs across evidence")


def verify_post_run(post):
    service_stopped = (re.search(r"(?m)^Result=success$", post) and
                       re.search(r"(?m)^ActiveState=inactive$", post) and
                       re.search(r"(?m)^SubState=dead$", post))
    systemd_unlimited = re.search(r"(?m)^CPUQuotaPerSecUSec=infinity$", post)
    kernel_unlimited = (re.search(r"(?m)^max 100000$", post) or
                        re.search(r"(?m)^cpu\.max=unavailable$", post))
    require(service_stopped and systemd_unlimited and kernel_unlimited,
            "post-run service or parent baseline is wrong")


def verify_el8_boot_contract(root):
    root = Path(root)
    initial = root / "initial-boot"
    qualified = root / "qualified-boot"
    require((initial / "cgroup-filesystem.txt").read_text().strip() != "cgroup2fs",
            "initial boot did not characterize the distribution default")
    require((initial / "firmware.txt").read_text().strip() == "uefi",
            "reviewed hybrid image was not booted through its EFI configuration")
    require((initial / "boot-id.txt").read_text() != (qualified / "boot-id.txt").read_text(),
            "boot configuration was not followed by a distinct boot")
    require((qualified / "systemd-version.txt").read_text().startswith("systemd 239 "),
            "qualified guest does not run systemd 239")
    require((qualified / "cgroup-filesystem.txt").read_text().strip() == "cgroup2fs",
            "qualified guest does not use cgroup v2")
    require((qualified / "firmware.txt").read_text().strip() == "uefi",
            "qualified guest changed firmware path")
    before_grubby = (root / "bootloader-before" / "grubby-info.txt").read_text()
    after_grubby = (root / "bootloader-after" / "grubby-info.txt").read_text()
    after_grubenv = (root / "bootloader-after" / "grubenv.txt").read_text()
    for argument in ("systemd.unified_cgroup_hierarchy=1", "psi=1"):
        require(argument not in before_grubby,
                "required argument was already present before the documented procedure")
        require(argument in after_grubby and argument in after_grubenv,
                "bootloader state did not record required argument " + argument)
    require("GRUB_ENABLE_BLSCFG=true" in
            (root / "bootloader-after" / "bls-config.txt").read_text(),
            "qualified image does not enable BLS configuration")
    require(" -> ../efi/EFI/redhat/grubenv" in
            (root / "bootloader-after" / "grubenv-link.txt").read_text(),
            "reviewed hybrid-image grubenv layout changed")
    require("options $kernelopts" in
            (root / "bootloader-after" / "entries.txt").read_text(),
            "BLS entry no longer consumes kernelopts")
    command_line = set((qualified / "cmdline.txt").read_text().split())
    require({"systemd.unified_cgroup_hierarchy=1", "psi=1"} <= command_line,
            "qualified boot lacks required kernel arguments")
    require(REQUIRED_CONTROLLERS <= set((qualified / "controllers.txt").read_text().split()),
            "qualified guest lacks required controllers")
    require(REQUIRED_PSI <= set((qualified / "psi-files.txt").read_text().split()),
            "qualified guest lacks PSI files")
    require("ControlGroupId=" not in (qualified / "user-slice.txt").read_text(),
            "systemd show unexpectedly exposes ControlGroupId")
    require(not re.search(r"(?m)^\s*ControlGroupId\s", (qualified / "slice-interface.txt").read_text()),
            "systemd interface unexpectedly exposes ControlGroupId")
    require(re.search(r"(?m)^sda\s+8:0\s+disk", (qualified / "block-devices.txt").read_text()),
            "qualified guest does not expose the tested 8:0 device")


def validate(root, qualification_revision, package, build_manifest):
    root = Path(root)
    package = Path(package)
    build_manifest = Path(build_manifest)
    verify_manifest(root)
    environment = fields(root / "environment.txt")
    build = fields(root / "build-manifest.txt")
    expected_build = fields(build_manifest)
    require(build == expected_build, "retained build manifest differs from supplied manifest")
    require(environment["result"] == "PASS" and environment["cleanup"] == "PASS",
            "host run or cleanup did not pass")
    verify_revision_provenance(environment, build, qualification_revision)
    require(build.get("build_kind") == "el8-rootful-podman",
            "RPM was not produced by the declared EL8 container path")
    for key in ("source_tree", "source_archive_sha256", "builder_image",
                "builder_image_digest", "go_version", "package_identity"):
        require(build.get(key), "build manifest lacks " + key)
    require(build["package_identity"] == PACKAGE_IDENTITY,
            "build manifest identifies another RPM")
    require(environment["base_sha256"] == BASE_SHA256, "unreviewed EL8 base image")
    require(environment["package_identity"] == PACKAGE_IDENTITY, "unexpected RPM identity")
    require(environment["package_sha256"] == digest(package) == build["package_sha256"],
            "RPM digest differs across build, request and runtime")

    verify_el8_boot_contract(root)

    guest = root / "guest"
    require((guest / "result").read_text().strip() == "PASS", "guest package gate failed")
    all_pass(guest / "el8-checks.json",
             {"el8-runtime-contract", "negative-identity", "package-lifecycle"})
    all_pass(guest / "checks.json",
             {"installed-identity", "shipped-defaults", "schema-reset",
              "upgrade-750-rejected", "graceful-stop"})
    all_pass(guest / "package-lifecycle-checks.json",
             {"full-budget-rejection", "pam-sessions", "blackout-suppression",
              "blackout-observation", "native-plan", "resource-properties",
              "authority-split", "crash-reclaim", "graceful-stop", "root-only-release"})
    identity = json.loads((guest / "installed-identity.json").read_text())
    require(identity["package_identity"] == PACKAGE_IDENTITY, "guest installed another package identity")
    require(identity["package_sha256"] == digest(package), "guest tested another package payload")
    negative = json.loads((guest / "negative-identity.json").read_text())
    require(negative == {"exit_code": negative["exit_code"], "systemd_native_absent": True,
                        "journal_absent": True, "probe_drop_ins_absent": True},
            "negative identity proof is incomplete")
    verify_post_run((root / "post-run.txt").read_text())


if __name__ == "__main__":
    require(len(sys.argv) == 5,
            "usage: validate_evidence.py EVIDENCE QUALIFICATION_REVISION PACKAGE BUILD_MANIFEST")
    validate(Path(sys.argv[1]), sys.argv[2], Path(sys.argv[3]), Path(sys.argv[4]))
