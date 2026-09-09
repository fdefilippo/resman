#!/usr/bin/env python3
"""Real adapter crash/ownership gate; never a claim about packaged daemon code."""
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import traceback

from native_gate import NativeGate, Blocked, eventually, field, require, sha


REQUIRED_CHECKS = frozenset({
    "recovery-pam-session", "crash-before-reload", "automatic-crash-recovery",
    "persistent-operator-conflict", "runtime-operator-conflict",
    "recreated-unit-recovery", "exact-recovery-cleanup",
})


def checks_pass(checks):
    return set(checks) == REQUIRED_CHECKS and all(value == "PASS" for value in checks.values())


class RecoveryGate(NativeGate):
    def __init__(self, bundle, run_id, revision):
        super().__init__(bundle, run_id, revision)
        self.probe = self.bundle / "systemdunit-real.test"
        self.probe_journal = self.work / "recovery-leases.json"
        self.operator_files = {}
        self.probe_sequence = 0

    def cron_accounts(self):
        return self.accounts[:1]

    def session_accounts(self):
        return self.accounts[:1]

    def passed(self, name, evidence):
        super().passed("recovery-pam-session" if name == "pam-sessions" else name, evidence)

    def preflight(self):
        super().preflight()
        if not self.probe.is_file() or not os.access(self.probe, os.X_OK):
            raise Blocked("the revision-built systemdunit-real.test probe is missing")
        self.target = "user-%d.slice" % self.accounts[0].pw_uid
        for root in ("/etc/systemd/system", "/run/systemd/system", "/etc/systemd/system.control"):
            if (Path(root) / (self.target + ".d")).exists():
                raise Blocked("preexisting operator overrides are not disposable: " + root)
        environment = json.loads((self.evidence / "environment.json").read_text())
        artifacts = self.evidence / "artifacts"
        artifacts.mkdir(mode=0o700, exist_ok=True)
        retained_probe = artifacts / "systemdunit-real.test"
        shutil.copyfile(self.probe, retained_probe)
        retained_probe.chmod(0o700)
        environment.update({"tested_binary_sha256": sha(self.probe),
                            "tested_binary_path": "artifacts/systemdunit-real.test",
                            "artifact_path": "artifacts/systemdunit-real.test",
                            "probe_sha256": sha(self.probe),
                            "probe_path": "artifacts/systemdunit-real.test",
                            "tested_artifact": "adapter-probe, not the daemon or installed package",
                            "host_identity": field("/etc/machine-id"),
                            "boot_id": field("/proc/sys/kernel/random/boot_id"),
                            "runner_sha256": sha(__file__)})
        self.save("environment", environment)

    def operation(self, operation, crash=False):
        self.probe_sequence += 1
        output = self.evidence / ("probe-%02d-%s.json" % (self.probe_sequence, operation))
        environment = os.environ.copy()
        environment.update({"RESMAN_REAL_RECOVERY_GATE": "1",
                            "RESMAN_REAL_SYSTEMD_UID": str(self.accounts[0].pw_uid),
                            "RESMAN_REAL_RECOVERY_JOURNAL": str(self.probe_journal),
                            "RESMAN_REAL_RECOVERY_OUTPUT": str(output),
                            "RESMAN_REAL_RECOVERY_OPERATION": operation})
        args = [str(self.probe), "-test.run=^TestRealRecoveryGateProbe$", "-test.v", "-test.timeout=55s"]
        with (self.evidence / "commands.jsonl").open("a") as stream:
            stream.write(json.dumps({"argv": args, "operation": operation,
                                     "journal": str(self.probe_journal), "output": str(output)}) + "\n")
        result = subprocess.run(args, env=environment, text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, timeout=60)
        output.with_suffix(".log").write_text(result.stdout)
        require(result.returncode == (-signal.SIGKILL if crash else 0),
                "probe %s exited %s: %s" % (operation, result.returncode, result.stdout))
        require(output.is_file(), "probe returned without structured evidence")
        return json.loads(output.read_text())

    def runtime_files(self):
        directory = Path("/run/systemd/system.control") / (self.target + ".d")
        return {str(path): sha(path) for path in sorted(directory.glob("*.conf"))}

    def restored(self, value):
        require(value["cpu_weight"] == self.baseline, "CPUWeight was not restored exactly")
        require(value["io_weight"] == self.baseline_io, "IOWeight was not restored exactly")
        require(not value["mutable_paths"] and not self.runtime_files(), "mutable drop-ins survived restore")
        require(not self.probe_journal.exists(), "private recovery journal survived restore")
        for filename, baseline in self.baseline_io_kernel.items():
            eventually(lambda f=filename, b=baseline: field(self.slice(self.accounts[0].pw_uid) / f, b) == b,
                       "operator fixture I/O weight did not converge to its kernel baseline")
        self.assert_membership()

    def crash_recovery(self):
        applied = self.operation("apply")
        require(applied["cpu_weight"] == 321 and len(applied["mutable_paths"]) == 1,
                "real Apply did not create the owned weight")
        crashed = self.operation("crash-restore", crash=True)
        require(crashed["phase"] == "reloading" and crashed["disk_footprint_absent"],
                "crash did not reach the durable post-revert boundary")
        require(crashed["reload_executed"] is False and crashed["cpu_weight_before_reload"] == 100,
                "crash did not retain the verified effective baseline before reload")
        require(crashed["drop_in_paths_before_reload"], "systemd did not expose the stale DropInPaths window")
        require(not self.runtime_files(), "revert left files before restart")
        self.passed("crash-before-reload", crashed)
        recovered = self.operation("recover")
        require(any(item["State"] == "reclaimed" for item in recovered["recovery"]),
                "fresh adapter did not report automatic reclaimed recovery")
        self.restored(recovered)
        self.passed("automatic-crash-recovery", {"restart": recovered, "manual_reload": False})

    def remove_operator_file(self, path):
        expected = self.operator_files[path]
        require(path.is_file() and not path.is_symlink() and sha(path) == expected,
                "operator fixture changed externally; refusing cleanup: " + str(path))
        path.unlink()
        del self.operator_files[path]
        # An empty directory created by the fixture has no surviving owner state.
        if not any(path.parent.iterdir()):
            path.parent.rmdir()

    def restore_runtime_operator_io(self, path):
        expected = self.operator_files[path]
        require(path.is_file() and not path.is_symlink() and sha(path) == expected,
                "operator fixture changed externally; refusing I/O restoration: " + str(path))
        weights = set()
        for baseline in self.baseline_io_kernel.values():
            match = re.fullmatch(r"default ([1-9][0-9]*)", baseline)
            require(match is not None, "unsupported kernel I/O weight baseline: " + baseline)
            weights.add(int(match.group(1)))
        require(len(weights) == 1, "kernel I/O weight baselines disagree")
        self.command("systemctl", "set-property", "--runtime", self.target,
                     "IOWeight=" + str(weights.pop()))
        self.operator_files[path] = sha(path)
        for filename, baseline in self.baseline_io_kernel.items():
            eventually(lambda f=filename, b=baseline: field(self.slice(self.accounts[0].pw_uid) / f, b) == b,
                       "operator fixture could not restore its kernel I/O weight baseline")

    def conflict(self, persistent):
        self.operation("apply")
        before = self.runtime_files()
        if persistent:
            directory = Path("/etc/systemd/system") / (self.target + ".d")
            directory.mkdir(mode=0o755)
            path = directory / "10-resman-recovery-probe.conf"
            path.write_text("[Slice]\nCPUWeight=500\n")
            path.chmod(0o600)
        else:
            self.command("systemctl", "set-property", "--runtime", self.target, "IOWeight=200")
            path = Path("/run/systemd/system.control") / (self.target + ".d") / "50-IOWeight.conf"
        self.operator_files[path] = sha(path)
        self.command("systemctl", "daemon-reload")
        refused_apply = self.operation("conflict-apply")
        refused_restore = self.operation("conflict-restore")
        require(sha(path) == self.operator_files[path], "adapter altered the operator file")
        after = self.runtime_files()
        require(all(after.get(name) == digest for name, digest in before.items()),
                "adapter changed owned files despite the external conflict")
        require(refused_apply["conflict"] and refused_restore["conflict"], "conflict was not typed")
        evidence = {"operator_file": str(path), "operator_sha256": sha(path),
                    "owned_files_before": before, "owned_files_after": after,
                    "apply": refused_apply, "restore": refused_restore}
        if not persistent:
            self.restore_runtime_operator_io(path)
        self.remove_operator_file(path)
        self.command("systemctl", "daemon-reload")
        self.restored(self.operation("restore"))
        self.passed("persistent-operator-conflict" if persistent else "runtime-operator-conflict", evidence)

    def recreation(self):
        before = self.operation("apply")
        uid = self.accounts[0].pw_uid
        original = self.sessions.pop(uid)
        self.command("loginctl", "terminate-session", original["session"])
        eventually(lambda: not Path("/proc/%d" % original["pid"]).exists(), "old session survived termination")
        eventually(lambda: self.command("systemctl", "show", self.target, "-p", "ActiveState", "--value",
                                        check=False).stdout.strip() != "active",
                   "user slice did not depart after logout; lingering is not a recreation", 90)
        require(self.probe_journal.exists() and self.runtime_files(), "logout unexpectedly removed the orphan footprint")
        shutil.rmtree(self.helper)
        self.start_sessions()
        after = self.operation("recover")
        require(after["identity"] != before["identity"], "new login did not recreate the unit identity")
        require(after["cpu_weight"] == 321, "recreated unit did not inherit the owned footprint")
        require(any(item["State"] == "orphaned_resman_footprint" for item in after["recovery"]),
                "fresh adapter did not identify the orphaned ResMan footprint")
        self.restored(self.operation("restore"))
        self.passed("recreated-unit-recovery", {"before": before, "after": after})

    def cleanup(self):
        if not self.owned_host:
            return
        for path in list(self.operator_files):
            self.remove_operator_file(path)
            self.command("systemctl", "daemon-reload")
        if self.probe_journal.exists():
            # Keep the PAM session alive until the authoritative adapter restores
            # it. A failed restore retains evidence and is never an unguarded revert.
            self.restored(self.operation("restore"))
        super().cleanup()
        require(not self.probe_journal.exists(), "recovery journal survived cleanup")
        require(not self.operator_files, "operator fixtures survived cleanup")

    def run(self):
        status, detail, cleanup = "FAIL", "recovery campaign incomplete", "PASS"
        try:
            self.preflight()
            self.start_sessions()
            initial = self.operation("recover")
            require(not initial["mutable_paths"], "initial unit has mutable override files")
            self.baseline = initial["cpu_weight"]
            self.baseline_io = initial["io_weight"]
            self.baseline_io_kernel = {name: field(self.slice(self.accounts[0].pw_uid) / name, "default 100")
                                       for name in ("io.weight", "io.bfq.weight")}
            self.crash_recovery()
            self.conflict(persistent=True)
            self.conflict(persistent=False)
            self.recreation()
            self.restored(self.operation("recover"))
            self.passed("exact-recovery-cleanup", {"baseline_cpu_weight": self.baseline,
                                                   "journal_absent": True, "runtime_files": self.runtime_files()})
            status = "PASS" if checks_pass(self.checks) else "FAIL"
            detail = "real adapter recovery and conflict boundaries completed"
        except Blocked as err:
            status, detail = "BLOCKED", str(err)
        except Exception as err:
            detail = str(err)
            traceback.print_exc()
        finally:
            try:
                self.cleanup()
            except Exception as err:
                cleanup, status = "FAIL", "FAIL"
                detail += "; cleanup: " + str(err)
                traceback.print_exc()
            for name in REQUIRED_CHECKS:
                self.checks.setdefault(name, "BLOCKED")
            self.save("checks", self.checks)
            (self.evidence / "result").write_text(status + "\n")
            (self.evidence / "environment.txt").write_text(
                "scenario=systemd-native-recovery\nsource_revision=%s\nkernel=%s\ncleanup=%s\nresult=%s\ndetail=%s\n" %
                (self.revision, os.uname().release, cleanup, status, detail.replace("\n", " ")))
            print(status + ": " + detail, flush=True)
        return {"PASS": 0, "BLOCKED": 77, "FAIL": 1}[status]


def main(arguments=None):
    arguments = sys.argv[1:] if arguments is None else arguments
    require(len(arguments) == 3 and arguments[0] == "systemd-native-recovery",
            "usage: native_recovery.py systemd-native-recovery RUN_ID REVISION")
    gate = RecoveryGate(Path(__file__).parent, arguments[1], arguments[2])

    def interrupted(_signal, _frame):
        raise RuntimeError("recovery campaign interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    return gate.run()


if __name__ == "__main__":
    sys.exit(main())
