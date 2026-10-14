#!/usr/bin/env python3
"""Compute the early-boot qualification verdict from raw guest measurements.

The guest records facts only. Every outcome below is recomputed here, so no
conclusion rests on a verdict written by the machine that produced the data.
"""

import argparse
import hashlib
import json
import pathlib
import sys

# rt_group_sched is the measured distribution-kernel build option: the EL8
# kernel enables it, the EL9 and EL10 kernels do not. On a kernel without it no
# realtime admission can be revoked, so nothing needs protecting.
PLATFORMS = {
    "el8": {
        "systemd": "239",
        "rt_group_sched": True,
        "base_sha256": "cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c",
    },
    "el9": {
        "systemd": "252",
        "rt_group_sched": False,
        "base_sha256": "b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b",
    },
    "el10": {
        "systemd": "257",
        "rt_group_sched": False,
        "base_sha256": "8e59326c4bf7cfa58a6cac404db8ed583fe3a5f4c460e2b73c64988785bb4f0f",
    },
}

SCENARIOS = {
    "protected-refusal": {"mode": "systemd_native", "protected": "vcs.service"},
    "protected-observation": {"mode": "auto", "protected": "vcs.service"},
    "consented-enforcement": {"mode": "systemd_native", "protected": ""},
}

SCHED_FIFO = 1
EPERM = 1


class EvidenceError(Exception):
    """The archive cannot support any verdict."""


def read(directory, name):
    path = directory / name
    if not path.is_file():
        raise EvidenceError(f"missing evidence file {path}")
    return path.read_text(encoding="utf-8")


def key_values(text):
    values = {}
    for line in text.splitlines():
        if "=" in line:
            key, value = line.split("=", 1)
            values[key.strip()] = value.strip()
    return values


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def monotonic(values, key):
    try:
        return int(values.get(key, "0"))
    except ValueError:
        return 0


def check_platform(environment, platform, revision, package, manifest):
    findings = []
    if environment.get("platform") != platform:
        findings.append(f"environment platform {environment.get('platform')!r} is not {platform}")
    if environment.get("result") != "PASS":
        findings.append(f"guest harness result is {environment.get('result')!r}")
    if environment.get("base_sha256") != PLATFORMS[platform]["base_sha256"]:
        findings.append("base image digest is not the reviewed Oracle template")
    if environment.get("kernel_family") != "rhck":
        findings.append("guest kernel family is not the distribution kernel")
    package_digest = sha256(package)
    if environment.get("package_sha256") != package_digest or manifest.get("package_sha256") != package_digest:
        findings.append("package digest differs between the RPM, the manifest and the guest record")
    if environment.get("source_revision") != manifest.get("source_revision"):
        findings.append("guest source revision differs from the build manifest")
    if environment.get("qualification_revision") != revision:
        findings.append("guest qualification revision differs from the requested revision")
    return findings


def check_boot(directory, platform):
    findings = []
    kernel = read(directory, "kernel.txt").strip()
    if "uek" in kernel:
        findings.append(f"booted UEK kernel {kernel}")
    systemd = read(directory, "systemd-version.txt").split()
    if len(systemd) < 2 or systemd[1] != PLATFORMS[platform]["systemd"]:
        findings.append(f"systemd version {systemd[1:2]} is not {PLATFORMS[platform]['systemd']}")
    if read(directory, "cgroup-filesystem.txt").strip() != "cgroup2fs":
        findings.append("the unified cgroup v2 hierarchy is not mounted")
    rt_group_sched = read(directory, "rt-group-sched.txt").strip() == "CONFIG_RT_GROUP_SCHED=y"
    if rt_group_sched != PLATFORMS[platform]["rt_group_sched"]:
        findings.append(f"kernel realtime group scheduling is {rt_group_sched}, "
                        f"not the {PLATFORMS[platform]['rt_group_sched']} recorded for {platform}")
    try:
        runtime = int(read(directory, "sched-rt-runtime-us.txt").strip())
    except ValueError:
        runtime = -1
    if rt_group_sched and runtime < 0:
        findings.append("kernel.sched_rt_runtime_us is unlimited, so no admission can be revoked")
    sysinit = key_values(read(directory, "sysinit-target.txt"))
    if monotonic(sysinit, "ActiveEnterTimestampMonotonic") <= 0:
        findings.append("sysinit.target was never reached")
    if read(directory, "bus-failures.txt").strip() not in ("", "0"):
        findings.append("the boot journal reports a system bus failure")
    return findings


def check_scenario(name, directory, platform):
    expected = SCENARIOS[name]
    findings = check_boot(directory, platform)
    declaration = key_values(read(directory, "declaration.txt"))
    if declaration.get("ENFORCEMENT_MODE") != expected["mode"] or \
            declaration.get("REALTIME_PROTECTED_UNITS") != expected["protected"]:
        findings.append(f"declaration {declaration} is not the {name} scenario")
    unit = key_values(read(directory, "resman-unit.txt"))
    fixture = json.loads(read(directory, "fixture.json"))
    root = read(directory, "root-subtree-control.txt").split()
    log = read(directory, "resman.log") + read(directory, "resman-journal.log")
    sysinit = monotonic(key_values(read(directory, "sysinit-target.txt")), "ActiveEnterTimestampMonotonic")
    dbus = monotonic(key_values(read(directory, "dbus-socket.txt")), "ActiveEnterTimestampMonotonic")
    ready = monotonic(unit, "ActiveEnterTimestampMonotonic")

    if name == "consented-enforcement" or not PLATFORMS[platform]["rt_group_sched"]:
        # Without realtime group scheduling the kernel never refuses a
        # realtime request, so the guard must stay out of the way.
        if not PLATFORMS[platform]["rt_group_sched"] and "realtime_admission_protected" in log:
            findings.append("realtime admission was protected on a kernel that cannot revoke it")
        if unit.get("ActiveState") != "active" or unit.get("Result") != "success" or unit.get("NRestarts") != "0":
            findings.append(f"resman did not run first time: {unit}")
        if "Systemd-native CPU enforcement selected" not in log:
            findings.append("systemd-native enforcement was not selected at boot")
        if not (0 < ready < sysinit):
            findings.append(f"resman was not ready before sysinit.target (ready={ready}, sysinit={sysinit})")
        if dbus and not ready < dbus:
            findings.append(f"resman was not ready before dbus.socket (ready={ready}, dbus={dbus})")
        # At an idle boot the probe restores user.slice, and whether systemd
        # then withdraws cpu from the root is a property of its version. The
        # kernel outcome must agree with the delegation actually left behind.
        if "cpu" in root and PLATFORMS[platform]["rt_group_sched"]:
            if fixture.get("result") != "refused" or fixture.get("errno") != EPERM:
                findings.append(f"root delegates cpu but the fixture's realtime request was not refused with EPERM: {fixture}")
        elif ("cpu" not in root or not PLATFORMS[platform]["rt_group_sched"]) and \
                (fixture.get("result") != "granted" or fixture.get("policy") != SCHED_FIFO):
            findings.append(f"no realtime admission could be revoked but the fixture did not get SCHED_FIFO: {fixture}")
        return findings

    # Both protected scenarios require that nothing else enabled cpu at the
    # root; otherwise the fixture's outcome says nothing about ResMan.
    if "cpu" in root:
        findings.append("another component enabled cpu at the root, so protection is unmeasurable")
    if fixture.get("result") != "granted" or fixture.get("policy") != SCHED_FIFO:
        findings.append(f"the protected fixture did not keep SCHED_FIFO: {fixture}")
    if name == "protected-refusal":
        if unit.get("ActiveState") != "failed" or unit.get("Result") != "exit-code" or \
                unit.get("ExecMainStatus") != "78" or unit.get("NRestarts") != "0":
            findings.append(f"resman did not refuse permanently with status 78: {unit}")
        if "failure_class=realtime_admission_protected" not in log or "vcs.service" not in log:
            findings.append("the refusal does not name realtime_admission_protected and vcs.service")
    else:
        if unit.get("ActiveState") != "active" or unit.get("Result") != "success":
            findings.append(f"resman did not keep observing: {unit}")
        if "reason=realtime_admission_protected" not in log or "protected_unit=vcs.service" not in log:
            findings.append("the observation does not publish realtime_admission_protected for vcs.service")
    return findings


def evaluate(evidence, platform, revision, package, manifest_path):
    manifest = key_values(pathlib.Path(manifest_path).read_text(encoding="utf-8"))
    environment = key_values(read(evidence, "environment.txt"))
    report = {"platform": platform, "package_identity": environment.get("package_identity"),
              "source_revision": manifest.get("source_revision"), "scenarios": {}}
    findings = check_platform(environment, platform, revision, package, manifest)
    boot_ids = set()
    for name in SCENARIOS:
        directory = evidence / name
        if not directory.is_dir():
            raise EvidenceError(f"scenario {name} was not measured")
        boot_ids.add(read(directory, "boot-id.txt").strip())
        scenario_findings = check_scenario(name, directory, platform)
        report["scenarios"][name] = {
            "verdict": "PASS" if not scenario_findings else "FAIL",
            "findings": scenario_findings,
            "root_delegates_cpu_after_boot": "cpu" in read(directory, "root-subtree-control.txt").split(),
            "fixture": json.loads(read(directory, "fixture.json")),
        }
    if len(boot_ids) != len(SCENARIOS):
        findings.append("the scenarios did not each run on a distinct boot")
    report["platform_findings"] = findings
    passed = not findings and all(s["verdict"] == "PASS" for s in report["scenarios"].values())
    report["verdict"] = "QUALIFIED" if passed else "NOT_QUALIFIED"
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence", type=pathlib.Path)
    parser.add_argument("platform", choices=sorted(PLATFORMS))
    parser.add_argument("revision")
    parser.add_argument("package")
    parser.add_argument("manifest")
    parser.add_argument("--expect", choices=["QUALIFIED", "NOT_QUALIFIED", "any"], default="QUALIFIED")
    args = parser.parse_args(argv)
    try:
        report = evaluate(args.evidence, args.platform, args.revision, args.package, args.manifest)
    except (EvidenceError, json.JSONDecodeError) as error:
        print(f"invalid evidence: {error}", file=sys.stderr)
        return 1
    print(json.dumps(report, indent=2, sort_keys=True))
    if args.expect != "any" and report["verdict"] != args.expect:
        print(f"verdict {report['verdict']} differs from expected {args.expect}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
