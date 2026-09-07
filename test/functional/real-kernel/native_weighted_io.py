#!/usr/bin/env python3
"""Adapter-only IOWeight proof; device traffic is characterization, never policy."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import signal
import subprocess
import sys
import time
import traceback

from native_gate import NativeGate, Blocked, eventually, field, require, sha
from null_blk_characterization import counters, summarize


SCENARIO = "systemd-native-weighted-io-adapter"
SCOPE = "systemd-io-weight-adapter-only; no daemon policy or delivery guarantee"
PROBE_SCOPE = "adapter-programmed-weight-only; not daemon or device-delivery evidence"
REQUIRED_CHECKS = frozenset({"weighted-io-pam-sessions", "owned-null-block-device",
    "bfq-device-capability", "non-bfq-negative-capability", "adapter-weight-roundtrip",
    "exact-weight-restoration", "owned-device-cleanup"})


def device_name(run_id):
    # Linux null_blk copies the configfs name into disk_name[DISK_NAME_LEN=32].
    # Keep the entire name within 31 bytes instead of relying on truncation.
    return "resmanweight" + hashlib.sha256(run_id.encode("ascii")).hexdigest()[:19]


def selected_scheduler(text):
    selected = re.findall(r"\[([^\]\s]+)\]", text)
    if len(selected) != 1:
        raise ValueError("scheduler must have exactly one selected value")
    return selected[0]


def iocost_disabled(text, device):
    if text is None:
        return True
    matches = [line.split()[1:] for line in text.splitlines() if line.split() and line.split()[0] == device]
    return not matches or (len(matches) == 1 and "enable=0" in matches[0])


def validate_proof(read_json, metadata):
    """Recheck raw probe/scheduler evidence instead of trusting check labels."""
    def insist(ok, detail):
        if not ok:
            raise ValueError(detail)
    scope = read_json("weighted-io-scope.json")
    insist(scope["scope"] == SCOPE and scope["daemon_exercised"] is False and
            scope["adapter_exercised"] is True, "weighted-I/O evidence is not adapter-only")
    insist(scope["source_revision"] == metadata["source_revision"] and
            scope["run_id"] == metadata["run_id"] and
            scope["probe_sha256"] == metadata["tested_binary_sha256"], "weighted probe provenance differs")
    device = read_json("owned-null-block-device.json")
    insist(re.fullmatch(r"resmanweight[0-9a-f]{19}", device["name"]) is not None and
            device["created_by_run"] is True and re.fullmatch(r"\d+:\d+", device["major_minor"]),
            "device was not created by this run")
    insist(device["name"] == device_name(metadata["run_id"]),
            "device is not run-specific")
    sessions = read_json("weighted-io-pam-sessions.json")
    insist(len(sessions) == 2 and all(int(uid) > 0 for uid in sessions), "two non-root PAM users required")
    for uid, item in sessions.items():
        insist(re.fullmatch(r"0::/user.slice/user-%s.slice/session-[a-z0-9]+.scope" % uid,
                           item["cgroup"]) is not None, "not a real PAM user slice")
    phases = read_json("adapter-weight-roundtrip.json")["phases"]
    insist([p["name"] for p in phases] == ["bfq-equal", "bfq-weighted", "non-bfq"], "missing adapter phase")
    filenames = set()
    identities = {}
    for phase, expected in zip(phases, ([100, 100], [100, 2300], [100, 2300])):
        bfq = phase["name"] != "non-bfq"
        insist(selected_scheduler(phase["scheduler_before"]) == selected_scheduler(phase["scheduler_after"]),
                "scheduler changed across adapter operations")
        insist((selected_scheduler(phase["scheduler_before"]) == "bfq") is bfq,
                "wrong per-device scheduler capability")
        insist(phase["effective_weight_capability"] is bfq and phase["device"] == device["major_minor"],
                "programmed weight confused with effective device capability")
        if not bfq:
            insist(iocost_disabled(phase["io_cost_qos_before"], device["major_minor"]) and
                    iocost_disabled(phase["io_cost_qos_after"], device["major_minor"]),
                    "negative phase still has per-device io.cost enabled")
        insist(len(phase["operations"]) == 2, "missing user operation")
        for item, weight, uid in zip(phase["operations"], expected, sorted(sessions, key=int)):
            filename = item["file"]
            insist(re.fullmatch(r"probe-[0-9]+-apply.json", filename) is not None and filename not in filenames,
                    "missing or reused apply probe")
            filenames.add(filename)
            result = read_json(filename)
            insist(item["uid"] == int(uid) and result["scope"] == PROBE_SCOPE and
                    result["operation"] == "apply" and result["requested_io_weight"] == weight and
                    result["after_io_weight"] == weight, "wrong adapter operation or weight")
            identity = result["identity"]
            insist(identity["Name"] == "user-%s.slice" % uid and identity["ControlGroupID"] > 0 and
                    any(identity["InvocationID"]), "missing authoritative user identity")
            insist(identity == identities.setdefault(uid, identity), "user slice changed during roundtrip")
            insist(result["expected_bfq_weight"] == (100 if weight == 100 else 300), "wrong BFQ conversion")
            insist(isinstance(result["after_kernel"].get("io.bfq.weight"), str) and
                    result["after_kernel"]["io.bfq.weight"].strip() == "default " + str(result["expected_bfq_weight"]),
                    "missing exact BFQ kernel readback")
            units = result["journal"]["units"]
            insist(result["mutable_paths"] and len(units) == 1 and units[0]["unit"] == "user-%s.slice" % uid and
                    units[0]["phase"] == "applied" and len(units[0]["properties"]) == 1 and
                    units[0]["properties"][0]["property"] == "IOWeight" and
                    units[0]["properties"][0]["last_applied"] == weight and
                    units[0]["properties"][0]["uncertain"] is False, "adapter did not retain its exact durable lease")
    for name, index, capable in (("bfq-device-capability", 1, True), ("non-bfq-negative-capability", 2, False)):
        proof = read_json(name + ".json")
        insist(proof == phases[index] and proof["effective_weight_capability"] is capable,
                "capability check is not backed by the measured adapter phase")
    restore = read_json("exact-weight-restoration.json")
    insist(set(restore) == set(sessions), "missing restoration")
    for uid, item in restore.items():
        insist(re.fullmatch(r"probe-[0-9]+-restore.json", item["file"]) is not None, "not an adapter restore")
        insist(re.fullmatch(r"probe-[0-9]+-recover.json", item["baseline_file"]) is not None, "unsafe baseline path")
        raw = read_json(item["file"])
        initial = read_json(item["baseline_file"])
        insist(raw["scope"] == PROBE_SCOPE and raw["operation"] == "restore" and
                raw["after_io_weight"] == item["baseline"] and not raw["mutable_paths"] and not raw["journal"]["units"] and
                item["journal_absent"] is True, "restoration left ownership or wrong baseline")
        insist(initial["operation"] == "recover" and not initial["mutable_paths"] and not initial["journal"]["units"] and
                initial["after_io_weight"] == item["baseline"] and initial["identity"] == raw["identity"],
                "restore baseline was not measured from the same pristine unit")
        insist(raw["identity"] == identities[uid] and item["uid"] == int(uid), "restore belongs to another user")
    for name in ("equal", "weighted"):
        traffic = read_json("traffic-" + name + ".json")
        insist(traffic["result"] == "CHARACTERIZATION" and "verdict" not in traffic and
                traffic["device"] == device["major_minor"], "traffic is not scoped characterization")
        insist(traffic["interval"] == summarize(traffic["before"], traffic["after"], device["major_minor"]),
                "traffic summary differs from raw device counters")
    cleanup = read_json("owned-device-cleanup.json")
    insist(cleanup["device_absent"] is True and cleanup["config_absent"] is True and
            cleanup["system_schedulers_before"] == cleanup["system_schedulers_after"] and
            (not cleanup["module_preexisting"] or cleanup["module_present"]), "device cleanup changed external state")
    return {"scope": SCOPE, "daemon_exercised": False, "adapter_exercised": True,
            "delivery_verdict": "not asserted"}


class WeightedIOGate(NativeGate):
    def __init__(self, bundle, run_id, revision):
        super().__init__(bundle, run_id, revision)
        self.probe = self.bundle / "systemdunit-real.test"
        self.name = device_name(run_id)
        self.device_config = Path("/sys/kernel/config/nullb") / self.name
        self.block = Path("/sys/block") / self.name
        self.device = Path("/dev") / self.name
        self.device_owned = False
        self.module_owned = False
        self.module_preexisting = False
        self.schedulers = {}
        self.lock = None
        self.sequence = 0
        self.baselines = {}
        self.baseline_files = {}
        self.phases = []

    def session_workload(self):
        return self.bundle / "weighted-io-workload.py"

    def workload_args(self):
        return (str(self.device),)

    def passed(self, name, evidence):
        super().passed("weighted-io-pam-sessions" if name == "pam-sessions" else name, evidence)

    def preflight(self):
        if os.geteuid() != 0 or not Path("/run/systemd/system").is_dir():
            raise Blocked("root on a quiescent disposable systemd host required")
        for tool in ("systemctl", "loginctl", "flock", "python3", "modprobe", "udevadm", "lsblk", "dd"):
            if not shutil.which(tool):
                raise Blocked("missing tool: " + tool)
        if not self.probe.is_file() or not os.access(self.probe, os.X_OK):
            raise Blocked("revision-built adapter probe missing")
        if self.command("pgrep", "-x", "resman", check=False).returncode == 0 or self.journal.exists():
            raise Blocked("ResMan or its ownership journal is not quiescent")
        if self.command("systemctl", "is-active", "resman", check=False).stdout.strip() == "active":
            raise Blocked("installed service is active")
        if self.command("systemctl", "is-active", "crond", check=False).stdout.strip() != "active":
            raise Blocked("cron/PAM is required")
        for name in ("resman-t1", "resman-t2"):
            try:
                account = pwd.getpwnam(name)
            except KeyError as err:
                raise Blocked("missing disposable account " + name) from err
            target = "user-%d.slice" % account.pw_uid
            if account.pw_uid == 0 or self.command("pgrep", "-u", str(account.pw_uid), check=False).returncode == 0:
                raise Blocked("account is not disposable: " + name)
            if self.command("systemctl", "list-units", "--all", "--plain", "--no-legend", target).stdout.strip():
                raise Blocked("user unit already exists: " + target)
            for root in ("/etc/systemd/system", "/run/systemd/system", "/etc/systemd/system.control", "/run/systemd/system.control"):
                if (Path(root) / (target + ".d")).exists():
                    raise Blocked("existing operator drop-ins: " + target)
            self.accounts.append(account)
        self.accounts.sort(key=lambda account: account.pw_uid)
        if self.cron.exists() or self.helper.exists() or self.device_config.exists() or self.block.exists() or self.device.exists():
            raise Blocked("fixture path collision")
        self.lock = os.open("/run/resman-nullblk-characterization.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        fcntl.flock(self.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.schedulers = {str(path): field(path) for path in Path("/sys/block").glob("*/queue/scheduler")}
        self.module_preexisting = Path("/sys/module/null_blk").exists()
        artifact = self.evidence / "artifacts/systemdunit-real.test"
        artifact.parent.mkdir(mode=0o700, exist_ok=True)
        shutil.copyfile(self.probe, artifact)
        artifact.chmod(0o700)
        scripts = {}
        for source in (Path(__file__), self.session_workload(), self.bundle / "null_blk_characterization.py"):
            shutil.copyfile(source, artifact.parent / source.name)
            scripts[source.name] = sha(source)
        self.save("environment", {"source_revision": self.revision, "run_id": self.run_id,
            "host_identity": field("/etc/machine-id"), "boot_id": field("/proc/sys/kernel/random/boot_id"),
            "kernel": os.uname().release, "artifact_path": "artifacts/systemdunit-real.test",
            "tested_binary_sha256": sha(self.probe), "runner_sha256": sha(__file__),
            "workload_sha256": sha(self.session_workload()),
            "fixture_sha256": scripts,
            "tested_artifact": "adapter-probe, not the daemon or installed package"})
        self.save("weighted-io-scope", {"scope": SCOPE, "adapter_exercised": True, "daemon_exercised": False,
            "source_revision": self.revision, "run_id": self.run_id, "probe_sha256": sha(self.probe)})
        self.owned_host = True

    def prepare_device(self):
        if not self.module_preexisting:
            self.command("modprobe", "null_blk", "nr_devices=0")
            self.module_owned = True
        if not self.device_config.parent.is_dir():
            raise Blocked("null_blk configfs support is unavailable; no existing device is reused")
        self.device_config.mkdir()
        self.device_owned = True
        self.config_inode = self.device_config.stat().st_ino
        settings = {"size": 256, "no_sched": 0, "queue_mode": 2, "submit_queues": 1,
                    "hw_queue_depth": 1, "irqmode": 2, "completion_nsec": 1000000, "memory_backed": 0}
        for key, value in settings.items():
            (self.device_config / key).write_text(str(value))
            require(field(self.device_config / key) == str(value), "null device setting mismatch")
        (self.device_config / "power").write_text("1")
        self.command("udevadm", "settle", "--timeout=10")
        require(self.device.is_block_device(), "dedicated device node absent")
        self.dev = field(self.block / "dev")
        require(self.dev == "%d:%d" % (os.major(self.device.stat().st_rdev), os.minor(self.device.stat().st_rdev)),
                "dedicated device identity differs")
        require(not self.command("lsblk", "-nr", "-o", "MOUNTPOINTS", self.device).stdout.strip(), "device is mounted")
        self.device.chmod(0o644)  # Read permission on this run-owned synthetic device only.
        self.set_scheduler("bfq")
        self.passed("owned-null-block-device", {"name": self.name, "major_minor": self.dev,
            "created_by_run": True, "settings": settings, "module_preexisting": self.module_preexisting})

    def set_scheduler(self, name):
        require(self.device_owned and self.device_config.stat().st_ino == self.config_inode,
                "device ownership changed")
        require(field(self.block / "dev") == self.dev, "device identity changed")
        available = field(self.block / "queue/scheduler").replace("[", "").replace("]", "").split()
        if name not in available:
            raise Blocked("required scheduler unavailable on dedicated device: " + name)
        (self.block / "queue/scheduler").write_text(name)
        require(selected_scheduler(field(self.block / "queue/scheduler")) == name, "scheduler write not confirmed")
        if name == "bfq":
            (self.block / "queue/iosched/low_latency").write_text("0")
            require(field(self.block / "queue/iosched/low_latency") == "0", "BFQ heuristics not disabled")

    def lease_path(self, uid):
        return self.work / ("weight-%d.json" % uid)

    def operation(self, uid, operation, weight=None):
        self.sequence += 1
        output = self.evidence / ("probe-%d-%s.json" % (self.sequence, operation))
        env = os.environ.copy()
        env.pop("RESMAN_REAL_IO_WEIGHT", None)
        env.update(RESMAN_REAL_IO_WEIGHT_GATE="1", RESMAN_REAL_SYSTEMD_UID=str(uid),
                   RESMAN_REAL_IO_WEIGHT_JOURNAL=str(self.lease_path(uid)),
                   RESMAN_REAL_IO_WEIGHT_OUTPUT=str(output), RESMAN_REAL_IO_WEIGHT_OPERATION=operation,
                   RESMAN_REAL_IO_WEIGHT_DEVICE=self.dev)
        if weight is not None:
            env["RESMAN_REAL_IO_WEIGHT"] = str(weight)
        args = [str(self.probe), "-test.run=^TestRealIOWeightGateProbe$", "-test.v", "-test.timeout=55s"]
        with (self.evidence / "commands.jsonl").open("a") as log:
            log.write(json.dumps({"argv": args, "uid": uid, "operation": operation, "weight": weight,
                                 "device": self.dev, "output": output.name}) + "\n")
        result = subprocess.run(args, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
        output.with_suffix(".log").write_text(result.stdout)
        require(result.returncode == 0 and "--- PASS: TestRealIOWeightGateProbe" in result.stdout,
                "adapter probe did not execute successfully: " + result.stdout)
        require(output.is_file(), "adapter probe omitted evidence")
        value = json.loads(output.read_text())
        require(value["scope"] == PROBE_SCOPE and value["operation"] == operation, "probe scope/operation mismatch")
        self.assert_membership()
        return {"uid": uid, "file": output.name}, value

    def phase(self, name, weights):
        before = field(self.block / "queue/scheduler")
        qos = Path("/sys/fs/cgroup/io.cost.qos")
        qos_before = field(qos) if qos.exists() else None
        if selected_scheduler(before) != "bfq" and not iocost_disabled(qos_before, self.dev):
            raise Blocked("negative phase cannot assert inert device weights with io.cost enabled")
        records = []
        for account, weight in zip(self.accounts, weights):
            record, value = self.operation(account.pw_uid, "apply", weight)
            require(value["after_io_weight"] == weight, "programmed weight differs")
            records.append(record)
        after = field(self.block / "queue/scheduler")
        qos_after = field(qos) if qos.exists() else None
        require(selected_scheduler(before) == selected_scheduler(after), "scheduler changed during phase")
        phase = {"name": name, "scheduler_before": before, "scheduler_after": after,
            "device": self.dev, "effective_weight_capability": selected_scheduler(after) == "bfq", "operations": records,
            "io_cost_qos_before": qos_before, "io_cost_qos_after": qos_after}
        self.phases.append(phase)
        self.save("adapter-weight-roundtrip", {"phases": self.phases})
        return phase

    def characterize(self, name):
        def ready():
            try:
                return all(counters(field(self.slice(account.pw_uid) / "io.stat"), self.dev)["rbytes"] > 0
                           for account in self.accounts)
            except RuntimeError:
                return False
        eventually(ready, "dedicated-device readers produced no observed I/O", 20)
        def sample():
            self.assert_membership()
            require(selected_scheduler(field(self.block / "queue/scheduler")) == "bfq", "BFQ changed during traffic")
            leaves = []
            for account in self.accounts:
                path = self.slice(account.pw_uid)
                leaves.append({"identity": [self.command("systemctl", "show", "user-%d.slice" % account.pw_uid,
                    "-p", "InvocationID", "--value").stdout.strip(), path.stat().st_ino],
                    "io_stat": field(path / "io.stat")})
            return {"time_ns": time.monotonic_ns(), "leaves": leaves}
        # Fixed observation window, not a readiness condition or delivery tolerance.
        first = sample()
        time.sleep(20)
        last = sample()
        self.save("traffic-" + name, {"result": "CHARACTERIZATION", "device": self.dev,
            "scope": "synthetic direct-read traffic; no daemon or ratio verdict", "before": first,
            "after": last, "interval": summarize(first, last, self.dev)})

    def restore_weights(self):
        restored = {}
        for account in self.accounts:
            uid = account.pw_uid
            if uid not in self.baselines:
                require(not self.lease_path(uid).exists(), "unexpected lease before baseline")
                continue
            record, value = self.operation(uid, "restore")
            require(value["after_io_weight"] == self.baselines[uid] and not value["mutable_paths"] and
                    not self.lease_path(uid).exists(), "adapter restoration is incomplete")
            record.update(baseline=self.baselines[uid], baseline_file=self.baseline_files[uid], journal_absent=True)
            restored[str(uid)] = record
        if len(restored) == 2:
            self.passed("exact-weight-restoration", restored)

    def assert_released(self):
        # This adapter-only fixture never owns the parent or another user's units.
        for account in self.accounts:
            require(not self.lease_path(account.pw_uid).exists(), "private lease survived cleanup")
            target = Path("/run/systemd/system.control/user-%d.slice.d" % account.pw_uid)
            require(not list(target.glob("*.conf")), "owned user override survived cleanup")

    def cleanup_device(self):
        if self.device_owned:
            require(self.device_config.stat().st_ino == self.config_inode, "refusing changed configfs device")
            (self.device_config / "power").write_text("0")
            self.device_config.rmdir()
            self.command("udevadm", "settle", "--timeout=10")
            require(not self.block.exists() and not self.device.exists(), "owned device survived cleanup")
            self.device_owned = False
        foreign_configs = ([path for path in self.device_config.parent.iterdir() if path.is_dir()]
                           if self.device_config.parent.exists() else [])
        if self.module_owned and not list(Path("/sys/block").glob("nullb*")) and not foreign_configs:
            self.command("modprobe", "-r", "null_blk")
        observed = {path: field(path) for path in self.schedulers}
        require(observed == self.schedulers, "existing device scheduler changed")
        require(not self.module_preexisting or Path("/sys/module/null_blk").exists(), "preexisting module was removed")
        self.passed("owned-device-cleanup", {"device_absent": not self.device.exists(),
            "config_absent": not self.device_config.exists(), "system_schedulers_before": self.schedulers,
            "system_schedulers_after": observed, "module_preexisting": self.module_preexisting,
            "module_present": Path("/sys/module/null_blk").exists()})

    def cleanup(self):
        if not self.owned_host:
            return
        errors = []
        for action in (self.restore_weights, self.cleanup_sessions, self.cleanup_device):
            try:
                action()
            except Exception as err:
                errors.append(str(err))
        require(not errors, "; ".join(errors))

    def cleanup_sessions(self):
        # Unlike the daemon fixture, this row owns no daemon/transient service.
        # Keep the same procfs identity checks without stopping unrelated units.
        if self.cron_created:
            self.cron.unlink()
            self.cron_created = False
        for account in self.accounts:
            recorded = self.helper / str(account.pw_uid) / "identity.json"
            if not recorded.exists():
                continue
            identity = json.loads(recorded.read_text())
            proc = Path("/proc/%d" % identity["pid"])
            if not proc.exists():
                continue
            require(proc.stat().st_uid == account.pw_uid and
                    field(proc / "stat").rsplit(")", 1)[1].split()[19] == identity["start_time"],
                    "refusing to terminate a changed helper identity")
            match = re.fullmatch(r"0::/user.slice/user-%d.slice/session-([a-z0-9]+).scope" % account.pw_uid,
                                 field(proc / "cgroup"))
            require(match is not None, "helper has no authoritative PAM session; manual cleanup required")
            self.command("loginctl", "terminate-session", match[1])
            eventually(lambda p=proc: not p.exists(), "PAM workload survived cleanup", 20)
        self.assert_released()
        if self.helper.exists():
            shutil.rmtree(self.helper)

    def run(self):
        status, detail, cleanup = "FAIL", "adapter campaign incomplete", "PASS"
        try:
            self.preflight()
            self.prepare_device()
            self.start_sessions()
            for account in self.accounts:
                # Inspection of an empty private journal, not proof of recovery.
                # Crash/reclaimed behavior belongs to the separate recovery row.
                initial, baseline = self.operation(account.pw_uid, "recover")
                require(not baseline["mutable_paths"], "preexisting mutable unit files")
                self.baselines[account.pw_uid] = baseline["after_io_weight"]
                self.baseline_files[account.pw_uid] = initial["file"]
            self.phase("bfq-equal", [100, 100])
            self.characterize("equal")
            phase = self.phase("bfq-weighted", [100, 2300])
            self.passed("bfq-device-capability", phase)
            self.characterize("weighted")
            choices = field(self.block / "queue/scheduler").replace("[", "").replace("]", "").split()
            negative = next((name for name in ("none", "mq-deadline") if name in choices), None)
            if negative is None:
                raise Blocked("dedicated device offers no negative scheduler capability")
            self.set_scheduler(negative)
            phase = self.phase("non-bfq", [100, 2300])
            require(phase["effective_weight_capability"] is False, "negative device capability was promoted")
            self.passed("non-bfq-negative-capability", phase)
            self.passed("adapter-weight-roundtrip", {"phases": self.phases})
            status, detail = "PASS", "adapter IOWeight programmed/verified/restored; daemon delivery not asserted"
        except Blocked as err:
            status, detail = "BLOCKED", str(err)
        except Exception as err:
            detail = str(err)
            traceback.print_exc()
        finally:
            try:
                self.cleanup()
                if status == "PASS":
                    require(set(self.checks) == REQUIRED_CHECKS and all(v == "PASS" for v in self.checks.values()),
                            "adapter check missing")
                    validate_proof(lambda name: json.loads((self.evidence / name).read_text()),
                                   json.loads((self.evidence / "environment.json").read_text()))
            except Exception as err:
                status, cleanup = "FAIL", "FAIL"
                detail += "; cleanup/verification: " + str(err)
                traceback.print_exc()
            for name in REQUIRED_CHECKS:
                self.checks.setdefault(name, "BLOCKED")
            self.save("checks", self.checks)
            (self.evidence / "result").write_text(status + "\n")
            (self.evidence / "environment.txt").write_text(
                "scenario=%s\nsource_revision=%s\nkernel=%s\ncleanup=%s\nresult=%s\ndetail=%s\n" %
                (SCENARIO, self.revision, os.uname().release, cleanup, status, detail.replace("\n", " ")))
            if self.lock is not None:
                os.close(self.lock)
                self.lock = None
        return {"PASS": 0, "BLOCKED": 77, "FAIL": 1}[status]


def main(arguments=None):
    arguments = sys.argv[1:] if arguments is None else arguments
    require(len(arguments) == 3 and arguments[0] == SCENARIO, "expected scenario RUN_ID REVISION")
    gate = WeightedIOGate(Path(__file__).parent, arguments[1], arguments[2])
    def interrupted(_signal, _frame):
        raise RuntimeError("weighted I/O campaign interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    return gate.run()


if __name__ == "__main__":
    sys.exit(main())
