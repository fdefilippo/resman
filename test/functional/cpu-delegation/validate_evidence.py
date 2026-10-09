#!/usr/bin/env python3
"""Independently validate one EL8 cpu-delegation reproduction archive.

The archive carries measurements only. Every typed outcome below is recomputed
here from those measurements, so a conclusion never rests on a verdict written
by the guest that produced the data.
"""

import argparse
import hashlib
import json
from pathlib import Path
import re
import sys


BASE_SHA256 = "cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c"
EINVAL = 22
CAPABILITY_MARKER = "required_capability_unavailable"
CPU_INTERFACE = "cpu.max"
CONFIGURATION_STATUS = 78
VERDICTS = frozenset({"REPRODUCED", "NOT_REPRODUCED"})
REMEDIES = frozenset({"REMEDIED", "NOT_REMEDIED", "NOT_MEASURED"})
OPERATOR_OBSERVATION = "operator_requested_observation"
CAPABILITY_OBSERVATION = "mandatory_capability_unavailable"


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def fields(path):
    result = {}
    for line in Path(path).read_text().splitlines():
        key, separator, value = line.partition("=")
        require(separator and key, "invalid field in " + str(path))
        require(key not in result, "duplicate field " + key + " in " + str(path))
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


def record(root, name):
    path = root / "guest" / (name + ".json")
    require(path.is_file(), "guest record is absent: " + name)
    return json.loads(path.read_text())


def shown(state, key):
    for line in state["output"]:
        name, separator, value = line.partition("=")
        if separator and name == key:
            return value
    return None


def verify_provenance(root, qualification_revision, package, build_manifest):
    environment = fields(root / "environment.txt")
    build = fields(build_manifest)
    require(environment["qualification_revision"] == qualification_revision,
            "qualification revision differs across evidence")
    require(environment["source_revision"] == build["source_revision"],
            "package source revision differs across evidence")
    require(environment["base_sha256"] == BASE_SHA256,
            "archive does not pin the reviewed Oracle base image digest")
    require(environment["package_sha256"] == digest(package),
            "archive does not identify the supplied package")
    require(build["package_sha256"] == digest(package),
            "build manifest does not identify the supplied package")
    require(environment["package_identity"].endswith(".el8.x86_64"),
            "the package under test is not an EL8 package")
    retained = root / "build-manifest.txt"
    require(retained.is_file() and fields(retained) == build,
            "retained build manifest differs from the supplied one")
    return environment


def verify_boot_contract(root):
    initial = root / "initial-boot"
    qualified = root / "qualified-boot"
    require((initial / "cgroup-filesystem.txt").read_text().strip() != "cgroup2fs",
            "initial boot did not characterize the distribution default")
    require((initial / "boot-id.txt").read_text() != (qualified / "boot-id.txt").read_text(),
            "boot configuration was not followed by a distinct boot")
    require((qualified / "systemd-version.txt").read_text().startswith("systemd 239 "),
            "qualified guest does not run systemd 239")
    require((qualified / "cgroup-filesystem.txt").read_text().strip() == "cgroup2fs",
            "qualified guest does not use cgroup v2")
    require('VERSION_ID="8.10"' in (qualified / "os-release.txt").read_text(),
            "qualified guest is not Oracle Linux 8.10")
    kernel = (qualified / "kernel.txt").read_text().strip()
    require(kernel and "uek" not in kernel, "qualified guest does not run an RHCK kernel")
    require((qualified / "running-kernel-package.txt").read_text().startswith("kernel-core-"),
            "qualified running kernel is not owned by kernel-core")
    arguments = set((qualified / "cmdline.txt").read_text().split())
    require("systemd.unified_cgroup_hierarchy=1" in arguments,
            "qualified boot lacks the unified cgroup hierarchy argument")
    return kernel


def verify_platform(root, kernel):
    platform = record(root, "platform")
    require(platform["kernel"] == kernel, "guest and host disagree on the running kernel")
    require(platform["rt_group_sched"] == "CONFIG_RT_GROUP_SCHED=y",
            "the reproduction requires a kernel built with CONFIG_RT_GROUP_SCHED=y")
    require("cpu" in platform["controllers"],
            "the root cgroup does not publish the cpu controller")
    require("cpu" not in platform["subtree_control"],
            "the root cgroup already delegated cpu before the measurement")
    require(platform["resman_package"]["status"] == 0,
            "the package under test is not installed in the guest")
    return platform


def classify_delegation(attempt):
    """Classify one write of +cpu to the root subtree control."""
    written = attempt["attempt"]
    require(written["written"] == "+cpu", "the recorded write is not a cpu delegation")
    if written["accepted"]:
        require("cpu" in written["subtree_control"],
                "an accepted delegation did not publish the cpu controller")
        return "DELEGABLE"
    require(written["errno"] is not None, "a refused delegation recorded no errno")
    require(written["errno"] == EINVAL and written["errno_name"] == "EINVAL",
            "the kernel refused the delegation for an unexpected reason: "
            + str(written["errno_name"]))
    require("cpu" not in written["subtree_control"],
            "a refused delegation nevertheless published the cpu controller")
    return "NOT_DELEGABLE"


def verify_baseline(root):
    baseline = record(root, "baseline-delegation")
    require(not baseline["realtime"]["outside_root"],
            "the baseline was measured while realtime tasks already lived outside the root")
    outcome = classify_delegation(baseline)
    if outcome == "DELEGABLE":
        require(baseline["revert"] is not None and baseline["revert"]["accepted"],
                "the baseline delegation was not reverted")
    require("cpu" not in baseline["subtree_control_after"],
            "the baseline left the cpu controller delegated")
    return outcome


def verify_fixture(root):
    fixture = record(root, "realtime-fixture")
    policy = "\n".join(fixture["policy"]["output"])
    require("SCHED_FIFO" in policy, "the fixture task does not run under SCHED_FIFO")
    require(re.search(r"priority[^0-9]*" + str(fixture["requested_priority"]), policy),
            "the fixture task does not hold the requested realtime priority")
    expected = "0::/system.slice/" + fixture["unit"] + ".service"
    require(fixture["cgroup"] == expected,
            "the fixture task is not inside its own non-root cgroup: " + str(fixture["cgroup"]))
    require(shown(fixture["state"], "ActiveState") == "active",
            "the fixture unit is not active")
    tids = [task["tid"] for task in fixture["realtime"]["outside_root"]]
    require(fixture["main_pid"] in tids,
            "the fixture task is absent from the realtime inventory outside the root")
    return "ESTABLISHED"


def verify_daemon(run, expect_started):
    """Classify one recorded start of the installed service."""
    state = run["state"]
    active = shown(state, "ActiveState")
    status = shown(state, "ExecMainStatus")
    capability = [line for line in run["capability_errors"]
                  if CPU_INTERFACE in line and CAPABILITY_MARKER in line]
    if expect_started:
        require(active == "active", "the service did not activate: " + str(active))
        require(shown(state, "Result") == "success", "the service activated with a failed result")
        require(not run["capability_errors"],
                "the service reported a capability error although it activated")
        require(run["enforcement_mode"],
                "the service activated without publishing its enforcement mode")
        return "STARTED"
    require(active == "failed", "the service did not refuse to start: " + str(active))
    require(status == str(CONFIGURATION_STATUS),
            "the refusal did not use the permanent configuration status: " + str(status))
    require(shown(state, "Result") == "exit-code",
            "the refusal was not reported as an exit-code failure")
    require(capability, "the refusal did not publish the typed cpu capability error")
    return "REFUSED_CONFIGURATION"


def optional_record(root, name):
    path = root / "guest" / (name + ".json")
    if not path.is_file():
        return None
    return json.loads(path.read_text())


def verify_declared_mode(run, mode, reason, names_capability):
    """Classify one start made under an explicit operator declaration.

    Both observing declarations must activate. They differ in what they are
    allowed to publish: a host declared observation-only probes nothing and so
    can name no missing capability, while auto must name the one it could not
    obtain, because an unexplained downgrade would be a silent one.
    """
    require(run["declaration"]["effective"] == ["ENFORCEMENT_MODE=" + mode],
            "the declaration was not the one measured: " + str(run["declaration"]["effective"]))
    state = run["state"]
    active = shown(state, "ActiveState")
    require(active == "active", "the declared mode did not activate: " + str(active))
    require(shown(state, "Result") == "success", "the declared mode activated with a failed result")
    require(shown(state, "ExecMainStatus") == "0",
            "the declared mode activated with a non-zero main status")
    published = [line for line in run["enforcement_mode"] if reason in line]
    require(published, "the published state does not carry the reason " + reason)
    require(any("observation_only" in line for line in published),
            "the published state does not name observation_only")
    require(not run["probe_slices"]["cgroups"],
            "a declared observing mode left a capability probe slice behind")
    if not names_capability:
        require(not run["capability_errors"],
                "a host declared observation-only probed a capability anyway")
        return "OBSERVING"
    detail = [line for line in published
              if "feature=" in line and "controller=" in line and "interface=" in line]
    require(detail, "the auto declaration did not name the capability it could not obtain")
    require(CPU_INTERFACE in "\n".join(detail),
            "the named capability is not the missing cpu interface")
    return "OBSERVING"


def verify_remedy(root):
    """Recompute whether the two observing declarations rescued this host."""
    observation = optional_record(root, "declared-observation")
    auto = optional_record(root, "declared-auto")
    restored = optional_record(root, "restored-declaration")
    if observation is None and auto is None:
        return "NOT_MEASURED", {}
    require(observation is not None and auto is not None,
            "only one of the two observing declarations was measured")
    outcomes = {
        "declared_observation": verify_declared_mode(
            observation, "observation_only", OPERATOR_OBSERVATION, names_capability=False),
        "declared_auto": verify_declared_mode(
            auto, "auto", CAPABILITY_OBSERVATION, names_capability=True),
    }
    require(not observation["probe_slices"]["units"]["output"],
            "a host declared observation-only created a capability probe unit")
    require(restored is not None
            and restored["effective"] == ["ENFORCEMENT_MODE=systemd_native"],
            "the measurement did not restore the default declaration")
    remedied = set(outcomes.values()) == {"OBSERVING"}
    return "REMEDIED" if remedied else "NOT_REMEDIED", outcomes


def verify_control(root):
    control = record(root, "control-without-realtime")
    require(not control["realtime_after_removal"]["outside_root"],
            "realtime tasks still lived outside the root during the control measurement")
    delegation = classify_delegation(control["delegation"])
    require(delegation == "DELEGABLE",
            "removing the realtime fixture did not restore cpu delegation")
    require("cpu" not in control["delegation"]["subtree_control_after"],
            "the control measurement left the cpu controller delegated by hand")
    return delegation, verify_daemon(control["daemon"], expect_started=True)


def verify_residue(root):
    residue = record(root, "residue")
    require(shown(residue["daemon"], "ActiveState") == "inactive",
            "the service was left running")
    require(shown(residue["fixture"], "ActiveState") in ("inactive", "failed", ""),
            "the realtime fixture was left active")
    require(residue["subtree_control"] == residue["initial_subtree_control"],
            "the delegation set was not restored to its initial value")
    require(not residue["leases_present"], "a durable property lease was left behind")
    require(not residue["system_control"], "a systemd drop-in was left behind")
    require(not residue["probe_slices"]["cgroups"], "a capability probe slice was left behind")
    require(not residue["realtime"]["outside_root"],
            "a realtime task was left outside the root cgroup")


def validate(root, qualification_revision, package, build_manifest, expect,
             expect_remedy="REMEDIED"):
    root = Path(root)
    verify_manifest(root)
    environment = verify_provenance(root, qualification_revision, package, build_manifest)
    kernel = verify_boot_contract(root)
    platform = verify_platform(root, kernel)
    result = record(root, "result")
    require(result["result"] == "MEASURED",
            "the guest did not complete the measurement: " + str(result.get("detail")))

    outcomes = {
        "delegation_baseline": verify_baseline(root),
        "realtime_fixture": verify_fixture(root),
        "delegation_under_realtime": classify_delegation(record(root, "delegation-under-realtime")),
        "daemon_under_realtime": verify_daemon(record(root, "daemon-under-realtime"),
                                               expect_started=False),
    }
    under = record(root, "delegation-under-realtime")
    require(under["realtime"]["outside_root"],
            "the refusal was measured without any realtime task outside the root")
    outcomes["delegation_without_realtime"], outcomes["daemon_without_realtime"] = \
        verify_control(root)
    verify_residue(root)

    reproduced = (outcomes["delegation_baseline"] == "DELEGABLE"
                  and outcomes["realtime_fixture"] == "ESTABLISHED"
                  and outcomes["delegation_under_realtime"] == "NOT_DELEGABLE"
                  and outcomes["daemon_under_realtime"] == "REFUSED_CONFIGURATION"
                  and outcomes["delegation_without_realtime"] == "DELEGABLE"
                  and outcomes["daemon_without_realtime"] == "STARTED")
    verdict = "REPRODUCED" if reproduced else "NOT_REPRODUCED"
    require(expect in VERDICTS | {"any"}, "unsupported expected verdict: " + expect)
    require(expect == "any" or verdict == expect,
            "computed verdict " + verdict + " differs from the expected " + expect)
    remedy, remedy_outcomes = verify_remedy(root)
    require(expect_remedy in REMEDIES | {"any"},
            "unsupported expected remedy: " + expect_remedy)
    require(expect_remedy == "any" or remedy == expect_remedy,
            "computed remedy " + remedy + " differs from the expected " + expect_remedy)
    outcomes.update(remedy_outcomes)
    return {"verdict": verdict, "remedy": remedy, "outcomes": outcomes, "kernel": kernel,
            "package_identity": environment["package_identity"],
            "rt_group_sched": platform["rt_group_sched"]}


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence")
    parser.add_argument("qualification_revision")
    parser.add_argument("package")
    parser.add_argument("build_manifest")
    parser.add_argument("--expect", default="REPRODUCED")
    parser.add_argument("--expect-remedy", default="REMEDIED")
    options = parser.parse_args(argv)
    summary = validate(options.evidence, options.qualification_revision,
                       options.package, options.build_manifest, options.expect,
                       options.expect_remedy)
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
