#!/usr/bin/env python3
"""Characterize systemd IODeviceWeight on one disposable virtual disk."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import signal
import stat
import subprocess
import sys
import time


WEIGHT = 333
OUTCOMES = frozenset({"SUPPORTED", "UNSUPPORTED"})


class Blocked(RuntimeError):
    """Blocked means that the guest cannot provide valid platform evidence."""


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def optional_text(path):
    path = Path(path)
    return path.read_text().strip() if path.is_file() else None


def scheduler_values(text):
    values = text.replace("[", "").replace("]", "").split()
    selected = re.findall(r"\[([^\]\s]+)\]", text)
    require(len(selected) == 1, "scheduler must expose exactly one selected value")
    return values, selected[0]


def keyed_line(text, key):
    if text is None:
        return None
    rows = [line.strip() for line in text.splitlines()
            if line.split() and line.split()[0] == key]
    require(len(rows) <= 1, "duplicate entry for " + key)
    return rows[0] if rows else None


def keyed_weight(text, key):
    row = keyed_line(text, key)
    if row is None:
        return None
    words = row.split()
    require(len(words) == 2 and words[0] == key and words[1].isdigit(),
            "malformed weight entry for " + key)
    return int(words[1])


def bfq_weight(weight):
    require(1 <= weight <= 10000, "systemd weight is outside the supported range")
    if weight <= 100:
        return 100 - (100 - weight) * 99 // 99
    return 100 + (weight - 100) * 900 // 9900


class Probe:
    """Own one guest-only service, slice, and virtual device policy."""

    def __init__(self, evidence, platform, run_id, revision, device):
        require(re.fullmatch(r"el(?:8|9|10)", platform), "invalid platform label")
        require(re.fullmatch(r"r[0-9]{14}-[0-9]+", run_id), "unsafe run identifier")
        require(re.fullmatch(r"[0-9a-f]{40}", revision), "full source revision required")
        self.evidence = evidence
        self.platform = platform
        self.run_id = run_id
        self.revision = revision
        self.device_argument = device
        suffix = hashlib.sha256((run_id + platform).encode("ascii")).hexdigest()[:12]
        self.slice = "user-resman_iow_" + suffix + ".slice"
        self.service = "resman-iow-" + suffix + ".service"
        self.unit_started = False
        self.system_schedulers = {}
        self.initial_scheduler = None
        self.initial_low_latency = None
        self.iocost_touched = False
        self.device = None
        self.block = None
        self.dev = None
        self.cgroup = None
        self.object_path = None
        self.record = {
            "schema_version": 1,
            "scope": "test-only-systemd-iodeviceweight-platform-characterization",
            "platform": platform,
            "run_id": run_id,
            "source_revision": revision,
            "probe_sha256": sha256(__file__),
            "requested_weight": WEIGHT,
            "rows": {},
        }

    def save(self):
        (self.evidence / "result.json").write_text(
            json.dumps(self.record, indent=2, sort_keys=True) + "\n")

    def command(self, *args, check=True, timeout=45):
        result = subprocess.run(args, universal_newlines=True, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, timeout=timeout, check=False)
        with (self.evidence / "commands.jsonl").open("a") as log:
            log.write(json.dumps({"argv": list(args), "exit_code": result.returncode,
                                  "output": result.stdout}, sort_keys=True) + "\n")
        if check:
            require(result.returncode == 0,
                    "command failed (%d): %s: %s" % (result.returncode, args, result.stdout))
        return result

    def write_control(self, path, value):
        path = Path(path)
        before = optional_text(path)
        error = None
        try:
            descriptor = os.open(path, os.O_WRONLY | os.O_CLOEXEC)
            try:
                os.write(descriptor, value.encode("ascii"))
            finally:
                os.close(descriptor)
        except OSError as caught:
            error = "%s: %s" % (caught.__class__.__name__, caught)
        after = optional_text(path)
        with (self.evidence / "control-writes.jsonl").open("a") as log:
            log.write(json.dumps({"path": str(path), "request": value,
                                  "before": before, "after": after,
                                  "error": error}, sort_keys=True) + "\n")
        if error:
            raise RuntimeError("control write failed for %s: %s" % (path, error))
        return {"path": str(path), "request": value,
                "before": before, "after": after}

    def preflight(self):
        if os.geteuid() != 0 or not Path("/run/systemd/system").is_dir():
            raise Blocked("root in a systemd guest is required")
        for command in ("busctl", "findmnt", "lsblk", "modprobe", "systemctl", "systemd-run"):
            if not shutil_which(command):
                raise Blocked("missing guest command: " + command)
        if self.command("stat", "-fc", "%T", "/sys/fs/cgroup").stdout.strip() != "cgroup2fs":
            raise Blocked("the representative guest is not using cgroup v2")
        resolved = Path(self.device_argument).resolve(strict=True)
        mode = resolved.stat().st_mode
        if not stat.S_ISBLK(mode):
            raise Blocked("the dedicated probe path is not a block device")
        if self.command("findmnt", "--noheadings", "--source", str(resolved), check=False).stdout.strip():
            raise Blocked("the dedicated probe device is mounted")
        names = self.command("lsblk", "-nrpo", "NAME", str(resolved)).stdout.splitlines()
        if names != [str(resolved)]:
            raise Blocked("the dedicated probe device has dependent block devices")
        if self.command("lsblk", "-nrpo", "MOUNTPOINT", str(resolved)).stdout.strip():
            raise Blocked("the dedicated probe device or a dependent device is mounted")
        self.device = resolved
        self.block = Path("/sys/class/block") / resolved.name
        self.dev = optional_text(self.block / "dev")
        require(re.fullmatch(r"[0-9]+:[0-9]+", self.dev or "") is not None,
                "dedicated device identity is unavailable")
        device_number = resolved.stat().st_rdev
        require(self.dev == "%d:%d" % (os.major(device_number), os.minor(device_number)),
                "device node and sysfs identities differ")
        optional_module = self.command("modprobe", "bfq", check=False)
        self.system_schedulers = {
            str(path): optional_text(path)
            for path in Path("/sys/block").glob("*/queue/scheduler")
        }
        scheduler = optional_text(self.block / "queue/scheduler")
        if scheduler is None:
            raise Blocked("the dedicated device has no scheduler interface")
        self.initial_scheduler = scheduler_values(scheduler)[1]
        self.initial_low_latency = optional_text(self.block / "queue/iosched/low_latency")
        self.record["environment"] = {
            "machine_id": optional_text("/etc/machine-id"),
            "boot_id": optional_text("/proc/sys/kernel/random/boot_id"),
            "kernel": os.uname().release,
            "os_release": optional_text("/etc/os-release"),
            "systemd_version": self.command("systemctl", "--version").stdout,
            "systemd_package": self.command("rpm", "-q", "systemd", check=False).stdout.strip(),
            "cgroup_controllers": optional_text("/sys/fs/cgroup/cgroup.controllers"),
            "bfq_module_probe": {"exit_code": optional_module.returncode,
                                 "output": optional_module.stdout},
            "device": str(self.device),
            "major_minor": self.dev,
            "device_size_bytes": int(optional_text(self.block / "size")) * 512,
            "scheduler_before": scheduler,
            "all_schedulers_before": self.system_schedulers,
        }
        self.save()

    def start_unit(self):
        existing = self.command("systemctl", "list-units", "--all", "--plain", "--no-legend",
                                self.slice).stdout.strip()
        require(not existing, "owned slice name already exists")
        self.command("systemd-run", "--quiet", "--unit=" + self.service,
                     "--slice=" + self.slice, "--property=Type=simple",
                     "--property=RuntimeMaxSec=240", "--property=TimeoutStopSec=10",
                     "/usr/bin/sleep", "240")
        self.unit_started = True
        require(self.command("systemctl", "is-active", self.service).stdout.strip() == "active",
                "probe service did not become active")
        cgroup = self.command("systemctl", "show", self.slice,
                              "--property=ControlGroup", "--value").stdout.strip()
        require(cgroup == "/user.slice/" + self.slice,
                "slice is not directly below user.slice: " + cgroup)
        self.cgroup = Path("/sys/fs/cgroup" + cgroup)
        response = self.command("busctl", "call", "org.freedesktop.systemd1",
                                "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
                                "GetUnit", "s", self.slice).stdout.strip()
        values = shlex.split(response)
        require(len(values) == 2 and values[0] == "o" and values[1].startswith("/org/freedesktop/systemd1/unit/"),
                "GetUnit returned an invalid object path: " + response)
        self.object_path = values[1]
        introspection = self.command(
            "busctl", "introspect", "--no-pager", "--no-legend",
            "org.freedesktop.systemd1", self.object_path,
            "org.freedesktop.systemd1.Slice", check=False)
        signature = None
        if introspection.returncode == 0:
            for line in introspection.stdout.splitlines():
                words = line.split()
                if len(words) >= 4 and words[0].endswith(".IODeviceWeight") and words[1] == "property":
                    signature = words[2]
                    break
        self.record["dbus"] = {
            "object_path": self.object_path,
            "interface": "org.freedesktop.systemd1.Slice",
            "property": "IODeviceWeight",
            "property_signature": signature,
            "introspection_exit_code": introspection.returncode,
            "introspection": introspection.stdout,
            "request": {
                "destination": "org.freedesktop.systemd1",
                "object_path": "/org/freedesktop/systemd1",
                "interface": "org.freedesktop.systemd1.Manager",
                "method": "SetUnitProperties",
                "signature": "sba(sv)",
                "unit": self.slice,
                "runtime": True,
                "property": "IODeviceWeight",
                "variant_signature": "a(st)",
                "device": str(self.device),
                "weight": WEIGHT,
            },
        }
        self.record["unit"] = {
            "slice": self.slice,
            "service": self.service,
            "control_group": cgroup,
            "control_group_inode": self.cgroup.stat().st_ino,
        }
        self.save()

    def property_readback(self):
        if self.record.get("dbus", {}).get("property_signature") != "a(st)":
            return None
        result = self.command("busctl", "get-property", "org.freedesktop.systemd1",
                              self.object_path, "org.freedesktop.systemd1.Slice",
                              "IODeviceWeight", check=False)
        return {"exit_code": result.returncode, "value": result.stdout.strip()}

    def set_weights(self, entries):
        args = ["busctl", "call", "org.freedesktop.systemd1",
                "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
                "SetUnitProperties", "sba(sv)", self.slice, "true", "1",
                "IODeviceWeight", "a(st)", str(len(entries))]
        for device, weight in entries:
            args.extend((str(device), str(weight)))
        return self.command(*args, check=False)

    def controller_chain(self):
        result = []
        for path in (Path("/sys/fs/cgroup"), Path("/sys/fs/cgroup/user.slice"), self.cgroup):
            result.append({
                "path": str(path),
                "inode": path.stat().st_ino if path.is_dir() else None,
                "controllers": optional_text(path / "cgroup.controllers"),
                "subtree_control": optional_text(path / "cgroup.subtree_control"),
            })
        return result

    def snapshot(self):
        return {
            "property": self.property_readback(),
            "controller_chain": self.controller_chain(),
            "kernel": {
                "io.weight": optional_text(self.cgroup / "io.weight"),
                "io.bfq.weight": optional_text(self.cgroup / "io.bfq.weight"),
                "root.io.cost.qos": optional_text("/sys/fs/cgroup/io.cost.qos"),
                "root.io.cost.model": optional_text("/sys/fs/cgroup/io.cost.model"),
            },
            "scheduler": optional_text(self.block / "queue/scheduler"),
            "low_latency": optional_text(self.block / "queue/iosched/low_latency"),
        }

    def property_phase(self, name):
        before = self.snapshot()
        request = self.set_weights(((self.device, WEIGHT),))
        during = self.snapshot()
        reset = self.set_weights(())
        after = self.snapshot()
        if request.returncode == 0:
            readback = during["property"]
            require(readback is not None and readback["exit_code"] == 0 and
                    str(self.device) in readback["value"] and str(WEIGHT) in readback["value"],
                    "systemd did not read back the requested IODeviceWeight")
        if reset.returncode == 0:
            readback = after["property"]
            require(readback is not None and readback["exit_code"] == 0 and
                    readback["value"].split()[-1:] == ["0"],
                    "systemd did not read back an empty IODeviceWeight array")
            for filename in ("io.weight", "io.bfq.weight"):
                require(keyed_line(after["kernel"][filename], self.dev) ==
                        keyed_line(before["kernel"][filename], self.dev),
                        filename + " did not return to its pre-property device entry")
        return {
            "name": name,
            "request_exit_code": request.returncode,
            "request_output": request.stdout,
            "reset_exit_code": reset.returncode,
            "reset_output": reset.stdout,
            "before": before,
            "during": during,
            "after": after,
        }

    def transport_probe(self):
        if self.record["dbus"]["property_signature"] != "a(st)":
            self.record["transport"] = {
                "outcome": "UNSUPPORTED",
                "reason": "systemd does not expose IODeviceWeight with signature a(st)",
            }
            return
        phase = self.property_phase("transport")
        if phase["request_exit_code"] != 0:
            phase.update(outcome="UNSUPPORTED",
                         reason="systemd rejected the IODeviceWeight D-Bus request")
        elif phase["reset_exit_code"] != 0:
            raise RuntimeError("systemd accepted IODeviceWeight but rejected its exact reset")
        else:
            phase.update(outcome="SUPPORTED", reason="D-Bus request and empty-array reset succeeded")
        self.record["transport"] = phase
        self.save()

    def select_scheduler(self, name):
        scheduler_path = self.block / "queue/scheduler"
        values, selected = scheduler_values(optional_text(scheduler_path))
        if name not in values:
            return False
        if selected != name:
            self.write_control(scheduler_path, name + "\n")
        require(scheduler_values(optional_text(scheduler_path))[1] == name,
                "scheduler selection was not acknowledged")
        low_latency = self.block / "queue/iosched/low_latency"
        if name == "bfq" and low_latency.is_file():
            self.write_control(low_latency, "0\n")
            require(optional_text(low_latency) == "0", "BFQ low_latency reset was not acknowledged")
        return True

    def prepare_iocost(self):
        qos = Path("/sys/fs/cgroup/io.cost.qos")
        model = Path("/sys/fs/cgroup/io.cost.model")
        if not qos.is_file() or not model.is_file():
            return False, "kernel does not expose root io.cost.qos and io.cost.model"
        initial = keyed_line(optional_text(qos), self.dev)
        if initial is not None and "enable=1" in initial.split()[1:]:
            raise Blocked("owned device unexpectedly has a pre-enabled io.cost policy")
        try:
            write = self.write_control(qos, self.dev + " enable=1 ctrl=auto\n")
        except RuntimeError as error:
            return False, str(error)
        self.iocost_touched = True
        active = keyed_line(optional_text(qos), self.dev)
        if active is None or "enable=1" not in active.split()[1:]:
            return False, "kernel did not acknowledge io.cost enable=1"
        self.record.setdefault("io_cost_writes", []).append(write)
        return True, "root io.cost policy enabled on the owned device"

    def disable_iocost(self):
        if not self.iocost_touched:
            return
        qos = Path("/sys/fs/cgroup/io.cost.qos")
        write = self.write_control(qos, self.dev + " enable=0\n")
        row = keyed_line(optional_text(qos), self.dev)
        require(row is not None and "enable=0" in row.split()[1:],
                "io.cost did not acknowledge disable")
        self.record.setdefault("io_cost_writes", []).append(write)
        self.iocost_touched = False

    def row(self, mechanism, ready, reason, phase=None):
        row = {"mechanism": mechanism, "outcome": "UNSUPPORTED", "reason": reason}
        if phase is not None:
            row["phase"] = phase
        if not ready:
            self.record["rows"][mechanism] = row
            return row
        if phase["request_exit_code"] != 0:
            row["reason"] = "systemd rejected IODeviceWeight while the mechanism was active"
        elif phase["reset_exit_code"] != 0:
            raise RuntimeError("systemd could not reset IODeviceWeight for " + mechanism)
        elif mechanism == "bfq":
            weight = keyed_weight(phase["during"]["kernel"]["io.bfq.weight"], self.dev)
            if weight == bfq_weight(WEIGHT):
                row.update(outcome="SUPPORTED", reason="active BFQ read the systemd per-device weight",
                           observed_weight=weight, expected_weight=bfq_weight(WEIGHT))
            else:
                row["reason"] = "active BFQ did not expose the expected per-device weight"
        elif mechanism == "iocost":
            weight = keyed_weight(phase["during"]["kernel"]["io.weight"], self.dev)
            qos = keyed_line(phase["during"]["kernel"]["root.io.cost.qos"], self.dev)
            if weight == WEIGHT and qos and "enable=1" in qos.split()[1:]:
                row.update(outcome="SUPPORTED", reason="active io.cost read the systemd per-device weight",
                           observed_weight=weight, expected_weight=WEIGHT)
            else:
                row["reason"] = "active io.cost did not expose the expected per-device weight"
        else:
            row.update(outcome="SUPPORTED",
                       reason="BFQ and io.cost were active for the same owned device",
                       policy_status="mechanism_ambiguous")
        self.record["rows"][mechanism] = row
        return row

    def characterize(self):
        self.transport_probe()
        if self.record["transport"]["outcome"] != "SUPPORTED":
            reason = self.record["transport"]["reason"]
            for mechanism in ("bfq", "iocost", "simultaneous"):
                self.row(mechanism, False, reason)
            return

        bfq_ready = self.select_scheduler("bfq")
        bfq_phase = self.property_phase("bfq") if bfq_ready else None
        bfq_row = self.row("bfq", bfq_ready, "BFQ is unavailable on the owned virtual device", bfq_phase)
        self.select_scheduler(self.initial_scheduler)

        iocost_ready, iocost_reason = self.prepare_iocost()
        iocost_phase = self.property_phase("iocost") if iocost_ready else None
        iocost_row = self.row("iocost", iocost_ready, iocost_reason, iocost_phase)
        self.disable_iocost()

        both_individually_supported = (
            bfq_row["outcome"] == "SUPPORTED" and iocost_row["outcome"] == "SUPPORTED")
        if both_individually_supported:
            self.select_scheduler("bfq")
        simultaneous_iocost, simultaneous_reason = self.prepare_iocost() if both_individually_supported else (
            False, "BFQ and io.cost did not both pass their individual systemd-to-kernel paths")
        simultaneous_ready = both_individually_supported and simultaneous_iocost
        simultaneous_phase = self.property_phase("simultaneous") if simultaneous_ready else None
        self.row("simultaneous", simultaneous_ready, simultaneous_reason, simultaneous_phase)
        self.disable_iocost()
        self.select_scheduler(self.initial_scheduler)
        self.save()

    def cleanup(self):
        errors = []
        try:
            if self.object_path and self.record.get("dbus", {}).get("property_signature") == "a(st)":
                reset = self.set_weights(())
                require(reset.returncode == 0, "final empty-array reset failed")
            self.disable_iocost()
        except Exception as error:
            errors.append(str(error))
        try:
            if self.block and self.initial_scheduler:
                self.select_scheduler(self.initial_scheduler)
                low_latency = self.block / "queue/iosched/low_latency"
                if self.initial_low_latency is not None and low_latency.is_file():
                    self.write_control(low_latency, self.initial_low_latency + "\n")
        except Exception as error:
            errors.append(str(error))
        if self.unit_started:
            self.command("systemctl", "stop", self.service, check=False)
            active = self.command("systemctl", "show", self.service,
                                  "--property=ActiveState", "--value", check=False).stdout.strip()
            if active not in {"", "inactive", "failed"}:
                errors.append("owned service remained active: " + active)
            reverted = self.command("systemctl", "revert", self.slice, check=False)
            if reverted.returncode != 0:
                errors.append("systemctl revert failed: " + reverted.stdout)
            self.command("systemctl", "reset-failed", self.service, check=False)
            self.command("systemctl", "stop", self.slice, check=False)
            self.command("systemctl", "reset-failed", self.slice, check=False)
        dropins = []
        if self.slice:
            for root in ("/run/systemd/system.control", "/run/systemd/system"):
                dropins.extend(str(path) for path in Path(root).glob(self.slice + ".d/*"))
        observed = ({str(path): optional_text(path)
                     for path in self.system_schedulers} if self.system_schedulers else {})
        if observed != self.system_schedulers:
            errors.append("an existing guest device scheduler changed")
        self.record["cleanup"] = {
            "result": "FAIL" if errors else "PASS",
            "errors": errors,
            "property_reset": True if self.object_path else None,
            "io_cost_disabled": not self.iocost_touched,
            "scheduler_after": optional_text(self.block / "queue/scheduler") if self.block else None,
            "all_schedulers_after": observed,
            "owned_drop_ins": dropins,
            "service_active_state": self.command(
                "systemctl", "show", self.service, "--property=ActiveState", "--value",
                check=False).stdout.strip() if self.unit_started else None,
        }
        if errors:
            raise RuntimeError("; ".join(errors))

    def run(self):
        exit_code = 1
        try:
            self.preflight()
            self.start_unit()
            self.characterize()
            require(set(self.record["rows"]) == {"bfq", "iocost", "simultaneous"},
                    "mechanism matrix is incomplete")
            require(all(row["outcome"] in OUTCOMES for row in self.record["rows"].values()),
                    "invalid mechanism outcome")
            self.record["result"] = "CHARACTERIZED"
            exit_code = 0
        except Blocked as error:
            self.record["result"] = "BLOCKED"
            self.record["error"] = str(error)
            exit_code = 77
        except Exception as error:
            self.record["result"] = "FAIL"
            self.record["error"] = str(error)
            exit_code = 1
        finally:
            try:
                self.cleanup()
            except Exception as error:
                self.record["result"] = "FAIL"
                self.record["cleanup_error"] = str(error)
                exit_code = 1
            self.save()
        print(self.record["result"], flush=True)
        return exit_code


def shutil_which(command):
    for directory in os.environ.get("PATH", "").split(os.pathsep):
        candidate = Path(directory) / command
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return str(candidate)
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--platform", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--source-revision", required=True)
    parser.add_argument("--device", required=True)
    args = parser.parse_args()
    args.evidence.mkdir(mode=0o700, parents=False, exist_ok=False)
    probe = Probe(args.evidence, args.platform, args.run_id,
                  args.source_revision, args.device)

    def interrupted(_signal, _frame):
        raise RuntimeError("characterization interrupted")

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(probe.run())


if __name__ == "__main__":
    main()
