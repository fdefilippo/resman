#!/usr/bin/env python3
"""Tests for the early-boot verdict: one faithful archive, then one defect each."""

import hashlib
import json
import pathlib
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import validate_evidence  # noqa: E402

REVISION = "a" * 40
SOURCE = "b" * 40


def write(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


def faithful_archive(root, platform="el8"):
    package = root / "package.rpm"
    package.write_bytes(b"rpm payload")
    digest = hashlib.sha256(b"rpm payload").hexdigest()
    manifest = root / "build-manifest.txt"
    write(manifest, f"source_revision={SOURCE}\nsource_tree={'c' * 40}\npackage_sha256={digest}\n")
    evidence = root / "evidence"
    info = validate_evidence.PLATFORMS[platform]
    write(evidence / "environment.txt",
          f"platform={platform}\nqualification_revision={REVISION}\nsource_revision={SOURCE}\n"
          f"base_sha256={info['base_sha256']}\nkernel_family=rhck\n"
          f"package_identity=resman-1.39.2-1.el8.dev318.x86_64\npackage_sha256={digest}\n"
          "cleanup=PASS\nresult=PASS\n")
    boot = 0
    for name, declared in validate_evidence.SCENARIOS.items():
        boot += 1
        directory = evidence / name
        write(directory / "boot-id.txt", f"boot-{boot}\n")
        write(directory / "kernel.txt", "4.18.0-553.el8_10.x86_64\n")
        write(directory / "systemd-version.txt", f"systemd {info['systemd']} (x)\n")
        write(directory / "cgroup-filesystem.txt", "cgroup2fs\n")
        write(directory / "rt-group-sched.txt", "CONFIG_RT_GROUP_SCHED=y\n")
        write(directory / "sched-rt-runtime-us.txt", "950000\n")
        write(directory / "declaration.txt",
              f"ENFORCEMENT_MODE={declared['mode']}\nREALTIME_PROTECTED_UNITS={declared['protected']}\n"
              "RAM_LIMIT_ENABLED=false\nIO_LIMIT_ENABLED=false\n")
        write(directory / "sysinit-target.txt", "ActiveEnterTimestampMonotonic=4000000\n")
        write(directory / "dbus-socket.txt", "ActiveEnterTimestampMonotonic=4100000\n")
        write(directory / "bus-failures.txt", "0\n")
        write(directory / "resman-journal.log", "")
        write(directory / "user-slice-subtree-control.txt", "memory pids\n")
        if name == "consented-enforcement":
            write(directory / "resman-unit.txt",
                  "ActiveState=active\nSubState=running\nResult=success\nExecMainStatus=0\nNRestarts=0\n"
                  "InactiveExitTimestampMonotonic=2000000\nActiveEnterTimestampMonotonic=2600000\n")
            write(directory / "resman.log", "[INFO] Systemd-native CPU enforcement selected enforcement_mode=systemd_native\n")
            write(directory / "root-subtree-control.txt", "cpu memory pids\n")
            write(directory / "fixture.json", json.dumps({"result": "refused", "errno": 1, "error": "Operation not permitted"}))
        else:
            write(directory / "root-subtree-control.txt", "memory pids\n")
            write(directory / "fixture.json", json.dumps({"result": "granted", "policy": 1}))
            if name == "protected-refusal":
                write(directory / "resman-unit.txt",
                      "ActiveState=failed\nSubState=failed\nResult=exit-code\nExecMainStatus=78\nNRestarts=0\n")
                write(directory / "resman.log",
                      "[ERROR] Failed to initialize systemd-native enforcement failure_class=realtime_admission_protected "
                      "retryable=false error=... vcs.service ...\n")
            else:
                write(directory / "resman-unit.txt",
                      "ActiveState=active\nSubState=running\nResult=success\nExecMainStatus=0\nNRestarts=0\n")
                write(directory / "resman.log",
                      "[WARN] Realtime admission protected; observing instead reason=realtime_admission_protected "
                      "protected_unit=vcs.service\n")
    return evidence, package, manifest


class VerdictTest(unittest.TestCase):
    def evaluate(self, mutate=None, platform="el8"):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            evidence, package, manifest = faithful_archive(root, platform)
            if mutate:
                mutate(evidence)
            return validate_evidence.evaluate(evidence, platform, REVISION, package, manifest)

    def test_faithful_archive_qualifies(self):
        for platform in validate_evidence.PLATFORMS:
            with self.subTest(platform=platform):
                self.assertEqual(self.evaluate(platform=platform)["verdict"], "QUALIFIED")

    def assertNotQualified(self, mutate, scenario=None):
        report = self.evaluate(mutate)
        self.assertEqual(report["verdict"], "NOT_QUALIFIED", report)
        if scenario:
            self.assertEqual(report["scenarios"][scenario]["verdict"], "FAIL", report)

    def test_bus_failure_at_boot(self):
        self.assertNotQualified(lambda e: write(e / "consented-enforcement/bus-failures.txt", "2\n"),
                                "consented-enforcement")

    def test_enforcement_not_selected(self):
        self.assertNotQualified(lambda e: write(e / "consented-enforcement/resman.log", "[WARN] remaining observation-only\n"),
                                "consented-enforcement")

    def test_ready_after_sysinit_is_a_stall(self):
        self.assertNotQualified(lambda e: write(
            e / "consented-enforcement/resman-unit.txt",
            "ActiveState=active\nResult=success\nNRestarts=0\nActiveEnterTimestampMonotonic=9000000\n"),
            "consented-enforcement")

    def test_restart_needed_is_not_a_first_boot_success(self):
        self.assertNotQualified(lambda e: write(
            e / "consented-enforcement/resman-unit.txt",
            "ActiveState=active\nResult=success\nNRestarts=1\nActiveEnterTimestampMonotonic=2600000\n"),
            "consented-enforcement")

    def test_consent_without_revocation_is_unmeasured(self):
        self.assertNotQualified(lambda e: write(e / "consented-enforcement/fixture.json",
                                                json.dumps({"result": "granted", "policy": 1})),
                                "consented-enforcement")

    def test_protected_fixture_lost_realtime(self):
        self.assertNotQualified(lambda e: write(e / "protected-refusal/fixture.json",
                                                json.dumps({"result": "refused", "errno": 1})),
                                "protected-refusal")

    def test_root_cpu_from_another_component(self):
        self.assertNotQualified(lambda e: write(e / "protected-observation/root-subtree-control.txt", "cpu memory\n"),
                                "protected-observation")

    def test_refusal_must_be_permanent(self):
        self.assertNotQualified(lambda e: write(
            e / "protected-refusal/resman-unit.txt",
            "ActiveState=failed\nResult=exit-code\nExecMainStatus=1\nNRestarts=0\n"),
            "protected-refusal")

    def test_observation_must_name_its_reason(self):
        self.assertNotQualified(lambda e: write(e / "protected-observation/resman.log",
                                                "[WARN] mandatory capability unavailable\n"),
                                "protected-observation")

    def test_uek_kernel(self):
        self.assertNotQualified(lambda e: write(e / "protected-refusal/kernel.txt", "5.15.0-uek\n"),
                                "protected-refusal")

    def test_kernel_without_realtime_group_scheduling(self):
        self.assertNotQualified(lambda e: write(e / "protected-observation/rt-group-sched.txt",
                                                "# CONFIG_RT_GROUP_SCHED is not set\n"),
                                "protected-observation")

    def test_wrong_systemd_version(self):
        self.assertNotQualified(lambda e: write(e / "protected-refusal/systemd-version.txt", "systemd 252 (x)\n"),
                                "protected-refusal")

    def test_shared_boot(self):
        self.assertNotQualified(lambda e: write(e / "protected-observation/boot-id.txt", "boot-1\n"))

    def test_package_digest_mismatch(self):
        def mutate(e):
            text = (e / "environment.txt").read_text(encoding="utf-8")
            write(e / "environment.txt", text.replace("package_sha256=", "package_sha256=0"))
        self.assertNotQualified(mutate)

    def test_missing_scenario_is_invalid(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            evidence, package, manifest = faithful_archive(root)
            for path in sorted((evidence / "protected-refusal").iterdir()):
                path.unlink()
            (evidence / "protected-refusal").rmdir()
            with self.assertRaises(validate_evidence.EvidenceError):
                validate_evidence.evaluate(evidence, "el8", REVISION, package, manifest)


if __name__ == "__main__":
    unittest.main()
