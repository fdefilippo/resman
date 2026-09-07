#!/usr/bin/env python3
"""Characterize BFQ on a disposable null_blk device, never daemon enforcement.

No filesystem is created and no existing device is opened for writing. The
fixture owns one configfs device and two transient services per measurement.
Weights are set by this fixture, not ResMan; ratios have no product verdict.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time


def require(value, message):
    if not value:
        raise RuntimeError(message)


def read(path):
    return Path(path).read_text().strip()


def counters(text, device):
    rows = [line.split() for line in text.splitlines() if line.split()[0] == device]
    require(len(rows) == 1, "missing or duplicate dedicated-device accounting")
    pairs = [word.split("=", 1) for word in rows[0][1:]]
    require(len(dict(pairs)) == len(pairs), "duplicate I/O counter")
    result = {key: int(value) for key, value in pairs}
    require(all(result.get(key, -1) >= 0 for key in ("rbytes", "rios")), "missing read counters")
    return result


def summarize(before, after, device):
    require(after["time_ns"] > before["time_ns"], "nonpositive interval")
    require(len(before["leaves"]) == len(after["leaves"]) == 2, "expected two competing leaves")
    deltas = []
    for first, last in zip(before["leaves"], after["leaves"]):
        require(first["identity"] == last["identity"], "measurement unit identity changed")
        a, b = counters(first["io_stat"], device), counters(last["io_stat"], device)
        delta = {key: b[key] - a[key] for key in ("rbytes", "rios")}
        require(all(value > 0 for value in delta.values()), "inactive or reset I/O workload")
        deltas.append(delta)
    require(len(deltas) == 2, "expected two competing leaves")
    total = sum(delta["rbytes"] for delta in deltas)
    return {"seconds": (after["time_ns"] - before["time_ns"]) / 1e9,
            "deltas": deltas, "read_share_percent": [100 * delta["rbytes"] / total for delta in deltas]}


class Characterization:
    def __init__(self, evidence, run_id, revision="unit-test"):
        require(re.fullmatch(r"r[0-9a-z]{6,30}", run_id), "unsafe run ID")
        self.evidence = evidence
        evidence.mkdir(mode=0o700, parents=False, exist_ok=False)
        self.name = "resmannull" + run_id
        self.config = Path("/sys/kernel/config/nullb") / self.name
        self.block = Path("/sys/block") / self.name
        self.device = Path("/dev") / self.name
        self.parent = self.name + ".slice"
        self.units = []
        self.module_owned = False
        self.device_owned = False
        self.lock = None
        self.system_schedulers = {}
        self.record = {"scope": "kernel-bfq-null_blk-characterization-not-daemon-enforcement",
                       "source_revision": revision,
                       "run_id": run_id, "kernel": os.uname().release,
                       "fixture_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                       "phases": []}

    def save(self):
        (self.evidence / "characterization.json").write_text(json.dumps(self.record, indent=2) + "\n")

    def command(self, *args, check=True):
        result = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
        with (self.evidence / "commands.jsonl").open("a") as log:
            log.write(json.dumps({"argv": args, "exit": result.returncode, "output": result.stdout}) + "\n")
        if check:
            require(result.returncode == 0, "command failed: %s: %s" % (args, result.stdout))
        return result.stdout.strip()

    def prepare(self):
        require(os.geteuid() == 0, "root required")
        self.lock = os.open("/run/resman-nullblk-characterization.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        fcntl.flock(self.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        require(not list(Path("/tmp").glob("resman-final-*")), "another real-kernel bundle exists")
        require(not self.command("pgrep", "-x", "resman", check=False), "ResMan must be quiescent")
        require(not self.config.exists() and not self.block.exists(), "device name already exists")
        require(self.command("systemctl", "show", self.parent, "-p", "LoadState", "--value") == "not-found",
                "fixture parent already exists")
        self.system_schedulers = {str(path): read(path) for path in Path("/sys/block").glob("*/queue/scheduler")}
        self.record["system_schedulers_before"] = self.system_schedulers
        self.record["host_identity"] = read("/etc/machine-id")
        self.record["boot_id"] = read("/proc/sys/kernel/random/boot_id")
        self.record["module_preexisting"] = Path("/sys/module/null_blk").exists()
        if not self.record["module_preexisting"]:
            self.command("modprobe", "null_blk", "nr_devices=0")
            self.module_owned = True
        self.config.mkdir()
        self.device_owned = True
        settings = {"size": 256, "no_sched": 0, "queue_mode": 2, "submit_queues": 1,
                    "hw_queue_depth": 1, "irqmode": 2, "completion_nsec": 1000000,
                    "memory_backed": 0}
        for key, value in settings.items():
            (self.config / key).write_text(str(value))
            require(read(self.config / key) == str(value), "device setting readback failed: " + key)
        (self.config / "power").write_text("1")
        self.command("udevadm", "settle", "--timeout=10")
        require(self.device.is_block_device(), "dedicated block device absent")
        self.dev = read(self.block / "dev")
        require("%d:%d" % (os.major(self.device.stat().st_rdev), os.minor(self.device.stat().st_rdev)) == self.dev,
                "device node identity differs")
        require(not self.command("lsblk", "-nr", "-o", "MOUNTPOINTS", str(self.device)), "device is mounted")
        (self.block / "queue/scheduler").write_text("bfq")
        (self.block / "queue/iosched/low_latency").write_text("0")
        self.record["device"] = {"name": self.name, "major_minor": self.dev, "settings": settings,
                                 "scheduler": read(self.block / "queue/scheduler"),
                                 "low_latency": read(self.block / "queue/iosched/low_latency"),
                                 "scope": "synthetic completion, no physical media performance"}
        self.save()

    def snapshot(self, identities, weights):
        require("[bfq]" in read(self.block / "queue/scheduler"), "BFQ was changed during measurement")
        require(read(self.block / "queue/iosched/low_latency") == "0", "BFQ heuristics changed")
        leaves = []
        for unit, original, weight in zip(self.units, identities, weights):
            require(self.command("systemctl", "is-active", unit) == "active", "I/O service exited")
            identity = self.command("systemctl", "show", unit, "-p", "InvocationID", "--value")
            require(identity == original, "service was replaced")
            group = self.command("systemctl", "show", unit, "-p", "ControlGroup", "--value")
            require(group == "/" + self.parent + "/" + unit, "service left its dedicated parent")
            path = Path("/sys/fs/cgroup" + group)
            require(read(path / "io.bfq.weight") == "default " + str(weight), "BFQ weight differs")
            leaves.append({"identity": [identity, path.stat().st_ino], "cgroup": group,
                           "weight": weight, "io_stat": read(path / "io.stat")})
        return {"time_ns": time.monotonic_ns(), "leaves": leaves}

    def phase(self, name, requested, effective):
        identities = []
        for index, weight in enumerate(requested):
            unit = self.name + name + str(index) + ".service"
            require(self.command("systemctl", "show", unit, "-p", "LoadState", "--value") == "not-found",
                    "service already exists")
            self.units.append(unit)
            self.command("systemd-run", "--quiet", "--unit=" + unit, "-p", "Slice=" + self.parent,
                         "-p", "IOWeight=" + str(weight), "-p", "RuntimeMaxSec=120", "-p", "TimeoutStopSec=10",
                         "/bin/sh", "-c", "while :; do dd if=%s of=/dev/null bs=64K count=4096 iflag=direct status=none || exit 1; done" % self.device)
            identity = self.command("systemctl", "show", unit, "-p", "InvocationID", "--value")
            require(re.fullmatch(r"[0-9a-f]{32}", identity), "missing service identity")
            identities.append(identity)
        # This is a declared warmup, not a readiness assertion or product tolerance.
        time.sleep(5)
        phase = {"name": name, "requested_systemd_weights": requested, "effective_bfq_weights": effective,
                 "samples": [], "intervals": []}
        self.record["phases"].append(phase)
        for index in range(13):
            if index:
                time.sleep(5)
            sample = self.snapshot(identities, effective)
            phase["samples"].append(sample)
            if index and index % 4 == 0:
                phase["intervals"].append(summarize(phase["samples"][index - 4], sample, self.dev))
            self.save()
        print(name, [row["read_share_percent"] for row in phase["intervals"]], flush=True)
        self.stop_units()

    def stop_units(self):
        for unit in list(self.units):
            self.command("systemctl", "stop", unit)
            require(self.command("systemctl", "show", unit, "-p", "ActiveState", "--value") == "inactive",
                    "service survived cleanup")
            self.units.remove(unit)

    def cleanup(self):
        self.stop_units()
        if self.device_owned:
            self.command("systemctl", "stop", self.parent)
            (self.config / "power").write_text("0")
            self.config.rmdir()
            self.command("udevadm", "settle", "--timeout=10")
            require(not self.block.exists() and not self.device.exists(), "device survived cleanup")
            self.device_owned = False
        # A concurrent owner may have created another configfs device. Never
        # unload that owner's device merely because we loaded the module first.
        foreign_devices = list(Path("/sys/block").glob("nullb*"))
        foreign_configs = []
        config_root = Path("/sys/kernel/config/nullb")
        if config_root.exists():
            foreign_configs = [path for path in config_root.iterdir() if path.is_dir()]
        if self.module_owned and not foreign_devices and not foreign_configs:
            self.command("modprobe", "-r", "null_blk")
            require(not Path("/sys/module/null_blk").exists(), "module survived cleanup")
        observed = {path: read(path) for path in self.system_schedulers}
        require(observed == self.system_schedulers, "an existing device scheduler changed")
        self.record["cleanup"] = {"result": "PASS", "system_schedulers_after": observed,
                                  "module_present": Path("/sys/module/null_blk").exists(),
                                  "foreign_configfs_devices_preserved": [str(path) for path in foreign_configs]}

    def run(self):
        try:
            self.prepare()
            self.phase("equal", [100, 100], [100, 100])
            self.phase("weighted", [100, 2300], [100, 300])
            self.record["result"] = "CHARACTERIZATION"
        except Exception as error:
            self.record["result"] = "FAIL"
            self.record["error"] = str(error)
        finally:
            try:
                self.cleanup()
            except Exception as error:
                self.record["result"] = "FAIL"
                self.record["cleanup"] = {"result": "FAIL", "error": str(error)}
            self.save()
            if self.lock is not None:
                os.close(self.lock)
        print(self.record["result"], flush=True)
        return 0 if self.record["result"] == "CHARACTERIZATION" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--source-revision", required=True)
    args = parser.parse_args()
    def interrupted(_signal, _frame):
        raise RuntimeError("characterization interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    require(re.fullmatch(r"[0-9a-f]{40}", args.source_revision), "full source revision required")
    raise SystemExit(Characterization(args.evidence, args.run_id, args.source_revision).run())
