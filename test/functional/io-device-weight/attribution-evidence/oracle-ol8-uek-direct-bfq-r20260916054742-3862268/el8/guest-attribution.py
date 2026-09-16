#!/usr/bin/env python3
"""Attribute the OL8 IODeviceWeight BFQ path limit with one direct kernel write."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys


SYSTEMD_WEIGHT = 333
BFQ_WEIGHT = 121


class Blocked(RuntimeError):
    """Blocked means that the guest cannot provide valid attribution evidence."""


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def optional_text(path):
    path = Path(path)
    return path.read_text().strip() if path.is_file() else None


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


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
    require(len(words) == 2 and words[1].isdigit(),
            "malformed weight entry for " + key)
    return int(words[1])


def kernel_option(text, name):
    match = re.search(r"(?m)^" + re.escape(name) + r"=(.+)$", text)
    if match:
        return match.group(1)
    if re.search(r"(?m)^# " + re.escape(name) + r" is not set$", text):
        return "not_set"
    return "absent"


class AttributionProbe:
    """Own one disposable device scheduler and one direct child cgroup."""

    def __init__(self, evidence, run_id, revision, device):
        require(re.fullmatch(r"r[0-9]{14}-[0-9]+", run_id),
                "unsafe run identifier")
        require(re.fullmatch(r"[0-9a-f]{40}", revision),
                "full source revision required")
        self.evidence = evidence
        self.run_id = run_id
        self.revision = revision
        self.device_argument = device
        suffix = hashlib.sha256(run_id.encode("ascii")).hexdigest()[:12]
        self.cgroup = Path("/sys/fs/cgroup/resman-iow-direct-" + suffix)
        self.block = None
        self.device = None
        self.dev = None
        self.initial_scheduler = None
        self.initial_root_subtree = None
        self.root_io_touched = False
        self.cgroup_created = False
        self.record = {
            "schema_version": 1,
            "scope": "test-only-ol8-uek-bfq-direct-write-attribution",
            "run_id": run_id,
            "source_revision": revision,
            "probe_sha256": sha256(__file__),
            "requested_systemd_weight": SYSTEMD_WEIGHT,
            "expected_bfq_weight": BFQ_WEIGHT,
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
                    "command failed (%d): %s: %s" %
                    (result.returncode, args, result.stdout))
        return result

    def write_control(self, path, value, check=True):
        path = Path(path)
        before = optional_text(path)
        error = None
        try:
            descriptor = os.open(path, os.O_WRONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
            try:
                written = os.write(descriptor, value.encode("ascii"))
                require(written == len(value), "short control-file write")
            finally:
                os.close(descriptor)
        except (OSError, RuntimeError) as caught:
            error = "%s: %s" % (caught.__class__.__name__, caught)
        after = optional_text(path)
        entry = {"path": str(path), "request": value, "before": before,
                 "after": after, "error": error}
        with (self.evidence / "control-writes.jsonl").open("a") as log:
            log.write(json.dumps(entry, sort_keys=True) + "\n")
        if check and error:
            raise RuntimeError("control write failed for %s: %s" % (path, error))
        return entry

    def capture_kernel_config(self):
        release = os.uname().release
        candidates = [Path("/boot/config-" + release), Path("/proc/config.gz")]
        source = None
        text = None
        for candidate in candidates:
            if not candidate.is_file():
                continue
            source = str(candidate)
            if candidate.suffix == ".gz":
                with gzip.open(candidate, "rt") as stream:
                    text = stream.read()
            else:
                text = candidate.read_text()
            break
        if source is None:
            return {"status": "unavailable",
                    "checked_sources": [str(candidate) for candidate in candidates]}
        retained = self.evidence / "kernel-config.txt"
        retained.write_text(text)
        return {
            "status": "captured",
            "source": source,
            "retained_file": retained.name,
            "sha256": sha256(retained),
            "options": {
                "CONFIG_BFQ_GROUP_IOSCHED": kernel_option(
                    text, "CONFIG_BFQ_GROUP_IOSCHED"),
                "CONFIG_BLK_CGROUP_IOCOST": kernel_option(
                    text, "CONFIG_BLK_CGROUP_IOCOST"),
            },
        }

    def preflight(self):
        if os.geteuid() != 0 or not Path("/run/systemd/system").is_dir():
            raise Blocked("root in a systemd guest is required")
        if self.command("stat", "-fc", "%T", "/sys/fs/cgroup").stdout.strip() != "cgroup2fs":
            raise Blocked("the representative guest is not using cgroup v2")
        resolved = Path(self.device_argument).resolve(strict=True)
        if not stat.S_ISBLK(resolved.stat().st_mode):
            raise Blocked("the dedicated probe path is not a block device")
        if self.command("findmnt", "--noheadings", "--source", str(resolved),
                        check=False).stdout.strip():
            raise Blocked("the dedicated probe device is mounted")
        names = self.command("lsblk", "-nrpo", "NAME", str(resolved)).stdout.splitlines()
        if names != [str(resolved)]:
            raise Blocked("the dedicated probe device has dependent block devices")
        self.device = resolved
        self.block = Path("/sys/class/block") / resolved.name
        self.dev = optional_text(self.block / "dev")
        require(re.fullmatch(r"[0-9]+:[0-9]+", self.dev or "") is not None,
                "dedicated device identity is unavailable")
        device_number = resolved.stat().st_rdev
        require(self.dev == "%d:%d" % (os.major(device_number), os.minor(device_number)),
                "device node and sysfs identities differ")
        module = self.command("modprobe", "bfq", check=False)
        scheduler_path = self.block / "queue/scheduler"
        scheduler = optional_text(scheduler_path)
        if scheduler is None or "bfq" not in {word.strip("[]") for word in scheduler.split()}:
            raise Blocked("BFQ is unavailable on the owned virtual device")
        selected = re.findall(r"\[([^\]\s]+)\]", scheduler)
        require(len(selected) == 1, "scheduler selection is ambiguous")
        self.initial_scheduler = selected[0]
        self.initial_root_subtree = optional_text("/sys/fs/cgroup/cgroup.subtree_control") or ""
        self.record["environment"] = {
            "kernel": os.uname().release,
            "os_release": optional_text("/etc/os-release"),
            "systemd_version": self.command("systemctl", "--version").stdout,
            "systemd_package": self.command("rpm", "-q", "systemd").stdout.strip(),
            "kernel_package": self.command(
                "rpm", "-q", "--whatprovides", "/boot/vmlinuz-" + os.uname().release,
                check=False).stdout.strip(),
            "device": str(resolved),
            "major_minor": self.dev,
            "scheduler_before": scheduler,
            "root_controllers": optional_text("/sys/fs/cgroup/cgroup.controllers"),
            "root_subtree_before": self.initial_root_subtree,
            "bfq_module_probe": {"exit_code": module.returncode, "output": module.stdout},
            "kernel_config": self.capture_kernel_config(),
        }
        self.save()

    def characterize(self):
        scheduler_path = self.block / "queue/scheduler"
        if self.initial_scheduler != "bfq":
            self.write_control(scheduler_path, "bfq\n")
        require("[bfq]" in optional_text(scheduler_path),
                "BFQ scheduler selection was not acknowledged")
        root_subtree = Path("/sys/fs/cgroup/cgroup.subtree_control")
        if "io" not in self.initial_root_subtree.split():
            self.write_control(root_subtree, "+io\n")
            self.root_io_touched = True
        require("io" in (optional_text(root_subtree) or "").split(),
                "root did not enable the io controller")
        self.cgroup.mkdir(mode=0o700)
        self.cgroup_created = True
        weight_file = self.cgroup / "io.bfq.weight"
        before = optional_text(weight_file)
        write = None
        if before is None:
            outcome = "KERNEL_LIMIT"
            reason = "owned child cgroup does not expose io.bfq.weight"
            during = None
            reset = None
            after = None
        else:
            write = self.write_control(
                weight_file, "%s %d\n" % (self.dev, BFQ_WEIGHT), check=False)
            during = optional_text(weight_file)
            if write["error"] is None and keyed_weight(during, self.dev) == BFQ_WEIGHT:
                outcome = "KERNEL_ACCEPTS"
                reason = "the UEK kernel accepted and exposed the direct BFQ device weight"
            else:
                outcome = "KERNEL_LIMIT"
                reason = "the UEK kernel did not accept the direct BFQ device weight"
            reset = None
            if keyed_line(during, self.dev) is not None:
                reset = self.write_control(weight_file, self.dev + " default\n", check=False)
            after = optional_text(weight_file)
            require(keyed_line(after, self.dev) == keyed_line(before, self.dev),
                    "direct BFQ device weight did not reset exactly")
        self.record["direct_bfq"] = {
            "outcome": outcome,
            "reason": reason,
            "cgroup": str(self.cgroup),
            "weight_file": str(weight_file),
            "major_minor": self.dev,
            "expected_weight": BFQ_WEIGHT,
            "scheduler_during": optional_text(scheduler_path),
            "before": before,
            "write": write,
            "during": during,
            "reset": reset,
            "after": after,
        }
        self.save()

    def cleanup(self):
        errors = []
        if self.cgroup_created:
            try:
                self.cgroup.rmdir()
                self.cgroup_created = False
            except OSError as error:
                errors.append("owned cgroup cleanup failed: " + str(error))
        if self.root_io_touched:
            try:
                self.write_control("/sys/fs/cgroup/cgroup.subtree_control", "-io\n")
                self.root_io_touched = False
            except RuntimeError as error:
                errors.append(str(error))
        if self.block and self.initial_scheduler:
            try:
                self.write_control(self.block / "queue/scheduler",
                                   self.initial_scheduler + "\n")
            except RuntimeError as error:
                errors.append(str(error))
        scheduler_after = optional_text(
            self.block / "queue/scheduler") if self.block else None
        root_after = optional_text("/sys/fs/cgroup/cgroup.subtree_control")
        if self.initial_root_subtree is not None and root_after != self.initial_root_subtree:
            errors.append("root cgroup.subtree_control was not restored")
        self.record["cleanup"] = {
            "result": "FAIL" if errors else "PASS",
            "errors": errors,
            "cgroup_removed": not self.cgroup.exists(),
            "root_subtree_after": root_after,
            "scheduler_after": scheduler_after,
        }
        if errors:
            raise RuntimeError("; ".join(errors))

    def run(self):
        exit_code = 1
        try:
            self.preflight()
            self.characterize()
            self.record["result"] = "ATTRIBUTED"
            exit_code = 0
        except Blocked as error:
            self.record["result"] = "BLOCKED"
            self.record["error"] = str(error)
            exit_code = 77
        except Exception as error:
            self.record["result"] = "FAIL"
            self.record["error"] = str(error)
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--source-revision", required=True)
    parser.add_argument("--device", required=True)
    args = parser.parse_args()
    args.evidence.mkdir(mode=0o700, parents=False, exist_ok=False)
    probe = AttributionProbe(args.evidence, args.run_id,
                             args.source_revision, args.device)

    def interrupted(_signal, _frame):
        raise RuntimeError("attribution interrupted")

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(probe.run())


if __name__ == "__main__":
    main()
