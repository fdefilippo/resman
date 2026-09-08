#!/usr/bin/env python3
"""Native ownership coverage, with explicit capability failures and retained proof.

This fixture never installs SSH credentials, downloads an image, changes the disk
scheduler or reverts an operator unit. The prepared nspawn root is booted through
a volatile overlay. Missing test capabilities remain BLOCKED.
"""
import json
import hashlib
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import signal
import subprocess
import sqlite3
import ssl
import sys
import time
import traceback
import urllib.request

from native_gate import Blocked, NativeGate, eventually, field, require


REQUIRED_CHECKS = frozenset({
    "ssh-pam-root-no-pty", "ssh-pam-root-pty", "ssh-pam-user-no-pty", "ssh-pam-user-pty",
    "child-units", "resource-properties", "rootless-envelope", "nspawn-machine-split",
    "nspawn-machine-only", "nspawn-keep-unit", "nspawn-shifted-bounded",
    "hard-io-delivery",
})


def result_for(checks):
    require(set(checks) == REQUIRED_CHECKS, "coverage check inventory differs")
    require(set(checks.values()) <= {"PASS", "FAIL", "BLOCKED"}, "unknown coverage result")
    return "FAIL" if "FAIL" in checks.values() else "BLOCKED" if "BLOCKED" in checks.values() else "PASS"


def pam_identity(payload, uid):
    require(payload["uid"] == uid, "SSH did not execute as the requested UID")
    match = re.fullmatch(r"0::/user\.slice/user-%d\.slice/session-([a-z0-9]+)\.scope" % uid,
                         payload["cgroup"])
    require(match is not None, "SSH workload is outside a genuine PAM/logind scope")
    require(payload["session_user"] == str(uid), "logind session UID differs")
    return match[1]


def validate_container_observation(rows, uid, coverage, machine_only=False):
    row = next((item for item in rows if item["uid"] == uid), None)
    require(row is not None, "observed machine UID disappeared from current SQLite epoch")
    require(row["cpu_authority_coverage"] == coverage, "CPU coverage differs from authority")
    if machine_only:
        for key in ("cpu_weight", "ram_cgroup_usage_bytes", "cgroup_path"):
            require(row[key] is None, "machine-only UID fabricated a slice field: " + key)
        require(row["ram_coverage"] == "unavailable" and row["io_coverage"] == "unavailable",
                "machine-only UID claims resource coverage")
    elif coverage == "partial":
        require(row["ram_coverage"] == "partial" and row["io_coverage"] == "partial",
                "authority-split UID mixes CPU coverage with an earlier resource cycle")
    return row


def io_values(text, device="8:0"):
    values = dict.fromkeys(("rbps", "wbps", "riops", "wiops"), "max")
    for line in text.splitlines():
        words = line.split()
        if words and words[0] == device:
            values.update(item.split("=", 1) for item in words[1:])
    return values


def io_written(text, device="8:0"):
    for line in text.splitlines():
        words = line.split()
        if words and words[0] == device:
            values = dict(item.split("=", 1) for item in words[1:])
            return int(values["wbytes"])
    return 0


def verify_io_interval(before, after, size):
    delta = io_written(after) - io_written(before)
    require(size <= delta <= size * 1.25,
            "direct I/O is not attributable to the declared device interval")
    return delta


IO_DIMENSIONS = {"rbps": ("IO_READ_BPS", 100 << 20), "wbps": ("IO_WRITE_BPS", 50 << 20),
                 "riops": ("IO_READ_IOPS", 1000), "wiops": ("IO_WRITE_IOPS", 500)}


def io_sample(path):
    sample = {"time": time.monotonic(), "identity": None, "identity_after": None, "io_stat": None}
    try:
        first = path.stat()
        sample["identity"] = [first.st_dev, first.st_ino]
        sample["io_stat"] = (path / "io.stat").read_text()
        last = path.stat()
        sample["identity_after"] = [last.st_dev, last.st_ino]
    except OSError as error:
        sample["error"] = str(error)
    return sample


def io_counters(text):
    if text is None:
        raise Blocked("I/O accounting file is unavailable")
    for line in text.splitlines():
        words = line.split()
        if words and words[0] == "8:0":
            return {key: int(value) for key, value in (word.split("=", 1) for word in words[1:])}
    raise Blocked("device 8:0 has no I/O accounting for this workload")


def validate_io_sample(sample):
    if sample.get("error") or sample["identity"] is None or sample["io_stat"] is None:
        raise Blocked("I/O accounting sample is unavailable")
    require(sample["identity"] == sample["identity_after"], "I/O cgroup changed during observation")
    counters = io_counters(sample["io_stat"])
    if not {"rbytes", "wbytes", "rios", "wios"} <= counters.keys():
        raise Blocked("device accounting omits bytes or completed operations")


def io_measurement(before, after, size, seconds, dimension):
    require(seconds > 0, "nonpositive I/O interval")
    first, last = io_counters(before), io_counters(after)
    prefix = dimension[0]
    keys = (prefix + "bytes", prefix + "ios")
    if not all(key in first and key in last for key in keys):
        raise Blocked("device accounting omits bytes or completed operations")
    deltas = {key: last[key] - first[key] for key in keys}
    require(size <= deltas[keys[0]] <= size * 1.25 and deltas[keys[1]] > 0,
            "device I/O interval is missing, reset or contaminated")
    counter = keys[1] if dimension.endswith("iops") else keys[0]
    return {"seconds": seconds, "bytes_requested": size, "deltas": deltas,
            "rate": deltas[counter] / seconds, "rate_counter": counter,
            "io_stat_before": before, "io_stat_after": after}


def verify_io_dimension(dimension, measurements):
    cap = IO_DIMENSIONS[dimension][1]
    if measurements["uncapped"]["rate"] < 1.5 * cap:
        raise Blocked("uncapped device cannot discriminate the " + dimension + " cap")
    require(0 < measurements["capped"]["rate"] <= 1.15 * cap, "observed " + dimension + " exceeds its isolated cap")


class CoverageGate(NativeGate):
    def __init__(self, *args):
        super().__init__(*args)
        self.child_units = []
        self.machine_units = []
        self.container_names = []
        self.podman_stores = []
        self.coverage_crons = []
        self.sudoers = None
        self.sudoers_identity = None
        self.nested_machines = set()
        self.nested_processes = {}
        self.resource_baselines = {}
        self.root = Path("/var/lib/machines/nq6c")
        self.mcp_token = secrets.token_hex(32)
        self.mcp_port = self.port + 5000
        self.cert = self.work / "server.crt"
        self.key = self.work / "server.key"

    def preflight(self):
        super().preflight()
        if not shutil.which("openssl"):
            raise Blocked("OpenSSL is required for the isolated authenticated MCP observation")
        self.command("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                     "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1",
                     "-keyout", self.key, "-out", self.cert)
        self.key.chmod(0o600)

    def write_config(self, blackout=True, overcommit=False):
        super().write_config(blackout, overcommit)
        settings = {"MCP_ENABLED": "true", "MCP_TRANSPORT": "http", "MCP_HTTP_HOST": "127.0.0.1",
                    "MCP_HTTP_PORT": str(self.mcp_port), "MCP_TLS_ENABLED": "true",
                    "MCP_TLS_CERT_FILE": str(self.cert), "MCP_TLS_KEY_FILE": str(self.key),
                    "MCP_AUTH_TOKEN": self.mcp_token, "MCP_ALLOW_WRITE_OPS": "false"}
        body = self.config.read_text()
        for key, value in settings.items():
            body, count = re.subn(r"(?m)^" + key + r"=.*$", lambda _: key + "=" + value, body)
            require(count == 1, "MCP fixture key missing or duplicated: " + key)
        self.config.write_text(body)

    def mcp_users(self, uid):
        revision = "2026-07-28"
        payload = {"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {
            "name": "get_user_metrics", "arguments": {"uids": [uid]}, "_meta": {
                "io.modelcontextprotocol/protocolVersion": revision,
                "io.modelcontextprotocol/clientCapabilities": {},
                "io.modelcontextprotocol/clientInfo": {"name": "resman-native-coverage", "version": "1"}}}}
        request = urllib.request.Request("https://127.0.0.1:%d/mcp" % self.mcp_port,
            data=json.dumps(payload).encode(), headers={"Content-Type": "application/json",
                "Accept": "application/json, text/event-stream", "Mcp-Protocol-Version": revision,
                "Mcp-Method": "tools/call", "Mcp-Name": "get_user_metrics",
                "Authorization": "Bearer " + self.mcp_token})
        with urllib.request.urlopen(request, context=ssl.create_default_context(cafile=str(self.cert)), timeout=15) as response:
            result = json.loads(response.read())
        require("error" not in result and not result.get("result", {}).get("isError"), "MCP user observation failed")
        return result["result"]["structuredContent"]["users"]

    def cron_accounts(self):
        return self.accounts[:2]

    def session_accounts(self):
        return self.accounts[:2]

    def attempt(self, names, operation):
        try:
            operation()
        except Blocked as err:
            for name in names:
                self.checks.setdefault(name, "BLOCKED")
                self.save(name, {"capability_failure": str(err)})
        except Exception as err:
            for name in names:
                self.checks[name] = "FAIL"
                self.save(name, {"failure": str(err)})
            raise

    def assert_applied(self):
        online = len(os.sched_getaffinity(0))
        eventually(lambda: field(self.parent / "cpu.max", "") == "%d 100000" % (online * 90000),
                   "native parent did not activate")
        for account in self.accounts[:2]:
            eventually(lambda a=account: field(self.slice(a.pw_uid) / "cpu.weight", "") == "9900",
                       "mapped slice weight not applied")
        require(field(self.slice(0) / "cpu.weight") == "3300", "root entitlement differs")
        require(field(self.slice(0) / "cpu.max").split()[0] == "max", "root leaf quota is finite")
        self.assert_membership()

    def ssh(self, user, script, pty=False, timeout=30):
        command = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=yes",
                   "-tt" if pty else "-T", user + "@localhost", script]
        result = self.command(*command, check=False, timeout=timeout)
        if result.returncode == 255:
            raise Blocked("existing localhost SSH authorization/host trust unavailable for " + user)
        require(result.returncode == 0, "SSH command failed: " + result.stdout)
        return result.stdout

    def ssh_sessions(self):
        # The identity and logind query execute before the SSH session terminates.
        code = ("import json,os,pathlib,re,subprocess; "
                "c=pathlib.Path('/proc/self/cgroup').read_text().strip(); "
                "m=re.search(r'/session-([a-z0-9]+)\\.scope$',c); "
                "u=subprocess.check_output(['loginctl','show-session',m[1],'-p','User','--value'],text=True).strip() if m else ''; "
                "print('RESMAN_IDENTITY='+json.dumps(dict(uid=os.getuid(),cgroup=c,session_user=u)))")
        for user, uid, label in (("root", 0, "root"), (self.accounts[0].pw_name, self.accounts[0].pw_uid, "user")):
            for pty in (False, True):
                name = "ssh-pam-%s-%s" % (label, "pty" if pty else "no-pty")
                def exercise():
                    output = self.ssh(user, "python3 -c " + shlex.quote(code), pty)
                    lines = [line.strip() for line in output.splitlines() if line.startswith("RESMAN_IDENTITY=")]
                    require(len(lines) == 1, "SSH identity response missing or duplicated")
                    identity = json.loads(lines[0].partition("=")[2])
                    pam_identity(identity, uid)
                    self.passed(name, {"identity": identity, "pty": pty})
                self.attempt([name], exercise)

    def start_child(self, name, uid, *command):
        unit = "resman-coverage-%s-%s.service" % (self.run_id, name)
        self.command("systemctl", "reset-failed", unit, check=False)
        self.child_units.append(unit)
        self.command("systemd-run", "--quiet", "--unit=" + unit, "--uid=" + str(uid),
                     "-p", "Slice=user-%d.slice" % uid, "-p", "RuntimeMaxSec=180", *command)
        eventually(lambda: self.command("systemctl", "show", unit, "-p", "MainPID", "--value").stdout.strip() != "0",
                   "child unit has no live main PID")
        pid = int(self.command("systemctl", "show", unit, "-p", "MainPID", "--value").stdout.strip())
        return unit, pid

    def children(self):
        identities = []
        try:
            for index in range(2):
                unit, pid = self.start_child("child%d" % index, self.accounts[0].pw_uid, "/usr/bin/sleep", "120")
                path = field("/proc/%d/cgroup" % pid)
                require(path.startswith("0::/user.slice/user-%d.slice/" % self.accounts[0].pw_uid),
                        "child unit is outside authoritative UID slice")
                identities.append({"unit": unit, "pid": pid, "cgroup": path})
            self.resources()
            for identity in identities:
                require(field("/proc/%d/cgroup" % identity["pid"]) == identity["cgroup"], "child workload migrated")
            self.passed("child-units", {"children": identities, "pam_sessions": self.sessions})
        finally:
            for identity in identities:
                self.command("systemctl", "stop", identity["unit"])

    def rows(self):
        with sqlite3.connect("file:%s?mode=ro" % self.db, uri=True) as db:
            db.row_factory = sqlite3.Row
            return [dict(row) for row in db.execute(
                "SELECT uid,cpu_authority_coverage,ram_coverage,io_coverage,cpu_weight,"
                "ram_cgroup_usage_bytes,cgroup_path FROM user_metrics WHERE "
                "sample_epoch_id=(SELECT MAX(sample_epoch_id) FROM user_metrics)")]

    def epoch(self):
        with sqlite3.connect("file:%s?mode=ro" % self.db, uri=True) as db:
            return db.execute("SELECT MAX(sample_epoch_id) FROM user_metrics").fetchone()[0]

    def wait_for_fresh_epochs(self):
        previous = self.epoch()
        require(previous is not None, "shifted-UID check has no observation baseline")
        for _ in range(2):
            eventually(lambda: self.epoch() not in (None, previous), "shifted-UID observation did not advance")
            previous = self.epoch()
        return previous

    def resource_values(self, uid):
        directory = self.slice(uid)
        return {"memory": {name: field(directory / name, "max")
                            for name in ("memory.high", "memory.max", "memory.swap.max")},
                "io": io_values(field(directory / "io.max", ""))}

    def capture_resource_baselines(self):
        require(not self.started, "resource baselines must precede daemon enforcement")
        self.resource_baselines = {a.pw_uid: self.resource_values(a.pw_uid) for a in self.accounts[:2]}
        self.save("resource-baselines", self.resource_baselines)

    def refused(self, uid, reason, since):
        directory = self.slice(uid)
        baseline = self.resource_baselines[uid]
        eventually(lambda: self.resource_values(uid) == baseline,
                   "refusal did not restore every RAM and per-device I/O baseline")
        require(field(directory / "cpu.weight") == "9900", "resource refusal removed CPU weight")
        eventually(lambda: reason in field(self.log)[since:], "missing fresh typed refusal: " + reason)

    def assert_owned_units(self):
        data = json.loads(self.journal.read_text())
        units = [item["unit"] for item in data["units"]]
        require(all(re.fullmatch(r"user(?:-[0-9]+)?\.slice", unit) for unit in units),
                "journal acquired a machine or nested unit")
        return units

    def create_sudoers(self, path, body):
        # A collision never becomes run-owned, even when finally invokes cleanup.
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o440)
        with os.fdopen(descriptor, "w") as stream:
            stream.write(body)
            stream.flush()
            info = os.fstat(stream.fileno())
            self.sudoers = path
            self.sudoers_identity = (info.st_dev, info.st_ino, hashlib.sha256(body.encode()).hexdigest())

    def remove_sudoers(self):
        if self.sudoers is None:
            return
        info = self.sudoers.lstat()
        require(not self.sudoers.is_symlink(), "run sudoers was replaced with a symlink")
        current = (info.st_dev, info.st_ino, hashlib.sha256(self.sudoers.read_bytes()).hexdigest())
        require(current == self.sudoers_identity, "run sudoers identity or content changed; preserved for inspection")
        self.sudoers.unlink()
        self.sudoers = self.sudoers_identity = None

    def payload_limits(self, cgroups):
        result = {}
        for path in sorted(set(cgroups)):
            require(path.startswith("0::/") and ".." not in Path(path[3:]).parts,
                    "invalid payload cgroup path")
            directory = Path("/sys/fs/cgroup") / path[4:]
            before = directory.stat()
            values = {}
            for name in ("cpu.max", "cpu.weight", "memory.high", "memory.max", "memory.swap.max", "io.max", "io.weight"):
                try:
                    values[name] = {"available": True, "value": field(directory / name)}
                except FileNotFoundError:
                    values[name] = {"available": False}
            after = directory.stat()
            require((before.st_dev, before.st_ino) == (after.st_dev, after.st_ino), "payload cgroup changed while reading")
            result[path] = {"identity": [before.st_dev, before.st_ino], "limits": values}
        require(result, "no payload cgroup to verify")
        return result

    def verify_payload_restoration_cycle(self, cgroups, uid, reason, label):
        before = self.payload_limits(cgroups)
        self.stop_daemon()
        self.assert_released()
        restored = self.payload_limits(cgroups)
        require(restored == before, "payload limits or identity changed during ancestor restoration")
        since = len(field(self.log))
        self.start_daemon()
        self.assert_applied()
        self.refused(uid, reason, since)
        reapplied = self.payload_limits(cgroups)
        require(reapplied == before, "payload limits or identity changed during ancestor enforcement")
        proof = {"before": before, "ancestor_restored": restored, "ancestor_reapplied": reapplied,
                 "scope": "sampled payload kernel properties and cgroup identities, not a syscall trace"}
        self.save(label + "-payload-limits", proof)
        return proof

    def nspawn_start(self, label, shifted=False):
        if not shutil.which("systemd-nspawn") or not self.root.is_dir():
            raise Blocked("prepared OL9 nspawn root /var/lib/machines/nq6c is unavailable")
        machine = "resman-" + self.run_id + "-" + label
        unit = machine + ".service"
        self.machine_units.append((machine, unit))
        self.command("systemctl", "reset-failed", unit, check=False)
        args = ["systemd-run", "--quiet", "--unit=" + unit, "-p", "Slice=machine.slice", "-p", "Delegate=yes",
                "/usr/bin/systemd-nspawn", "--quiet", "--boot", "--volatile=overlay", "--directory=" + str(self.root),
                "--machine=" + machine]
        if shifted:
            args.append("--private-users=pick")
        self.command(*args)
        eventually(lambda: self.command("machinectl", "show", machine, "-p", "Leader", "--value", check=False).returncode == 0,
                   "nspawn machine did not register", 60)
        return machine, unit

    def stop_machine(self, machine, unit):
        self.command("machinectl", "terminate", machine, check=False)
        self.stop_owned_unit(unit)
        if (machine, unit) in self.machine_units:
            self.machine_units.remove((machine, unit))

    def stop_owned_unit(self, unit):
        result = self.command("systemctl", "stop", unit, check=False)
        if result.returncode:
            state = self.command("systemctl", "show", unit, "-p", "LoadState", "--value", check=False)
            require(state.stdout.strip() == "not-found", "failed to stop run-owned unit: " + unit)

    def machine_processes(self, machine):
        scope = self.command("machinectl", "show", machine, "-p", "Unit", "--value").stdout.strip()
        control = self.command("systemctl", "show", scope, "-p", "ControlGroup", "--value").stdout.strip()
        require(control.startswith("/machine.slice/"), "registered machine is not owned by machine.slice")
        result = []
        for proc in Path("/proc").iterdir():
            if not proc.name.isdecimal():
                continue
            try:
                cgroup = field(proc / "cgroup")
                if cgroup == "0::" + control or cgroup.startswith("0::" + control + "/"):
                    result.append({"pid": int(proc.name), "uid": proc.stat().st_uid, "cgroup": cgroup})
            except FileNotFoundError:
                continue
        return result

    def nspawn_machine(self):
        since = len(field(self.log))
        machine, unit = self.nspawn_start("machine")
        try:
            a, _, only = self.accounts
            eventually(lambda: {a.pw_uid, only.pw_uid} <= {p["uid"] for p in self.machine_processes(machine)},
                       "prepared nspawn root must run both host-UID fixture workloads", 60)
            processes = self.machine_processes(machine)
            self.refused(a.pw_uid, "authority_split", since)
            def observed():
                rows = self.rows()
                try:
                    validate_container_observation(rows, a.pw_uid, "partial")
                    validate_container_observation(rows, only.pw_uid, "unavailable", True)
                    return True
                except AssertionError:
                    return False
            eventually(observed, "machine split/machine-only observations did not converge")
            rows = self.rows()
            scrape = self.scrape()
            require('uid="%d"' % only.pw_uid in scrape, "machine-only UID is missing from Prometheus")
            require(not self.slice(only.pw_uid).exists(), "machine-only UID has an unexpected host slice")
            units = self.assert_owned_units()
            users = self.mcp_users(only.pw_uid)
            require(len(users) == 1 and users[0]["uid"] == only.pw_uid, "machine-only UID disappeared from MCP")
            require(not users[0]["cpu_limit_active"] and not users[0]["ram_limit_active"] and not users[0]["io_limit_active"],
                    "MCP claims enforcement on a machine-only UID")
            limits = self.verify_payload_restoration_cycle([p["cgroup"] for p in processes if p["uid"] in (a.pw_uid, only.pw_uid)], a.pw_uid,
                                                           "authority_split", "nspawn-machine")
            self.passed("nspawn-machine-split", {"processes": processes, "rows": rows, "owned_units": units,
                                                   "payload_limits": limits})
            self.passed("nspawn-machine-only", {"uid": only.pw_uid, "rows": rows, "prometheus_uid_present": True,
                                                 "mcp_users": users})
        finally:
            self.stop_machine(machine, unit)
        self.resources()

    def nspawn_shifted(self):
        machine, unit = self.nspawn_start("shifted", True)
        try:
            eventually(lambda: len(self.machine_processes(machine)) >= 3, "shifted machine has no workload")
            processes = self.machine_processes(machine)
            shifted = {p["uid"] for p in processes if p["uid"] > 65535}
            require(shifted, "private-users did not shift any observed UID")
            start = len(field(self.log))
            epoch = self.command("date", "+%s").stdout.strip()
            observed_epoch = self.wait_for_fresh_epochs()
            require(shifted <= {p["uid"] for p in self.machine_processes(machine)},
                    "shifted workload disappeared before fresh observation")
            rows, scrape = self.rows(), self.scrape()
            require(not shifted.intersection({row["uid"] for row in rows}), "shifted UID leaked into persisted users")
            require(all('uid="%d"' % uid not in scrape for uid in shifted), "shifted UID leaked into user series")
            require(all(not re.search(r"\buid=%d\b" % uid, field(self.log)[start:]) for uid in shifted),
                    "shifted UID produced an unbounded per-user diagnostic")
            self.passed("nspawn-shifted-bounded", {"shifted_uids": sorted(shifted), "processes": processes,
                                                    "disposition": "omitted, not attributed to a named user",
                                                    "start_epoch": epoch, "observed_sample_epoch_id": observed_epoch})
        finally:
            self.stop_machine(machine, unit)

    def rootless(self):
        if not shutil.which("podman"):
            raise Blocked("Podman is unavailable")
        user = self.accounts[0]
        image = "docker.io/library/oraclelinux:9"
        if self.command("podman", "image", "exists", image, check=False).returncode:
            raise Blocked("preloaded rootful oraclelinux:9 image is absent; no network pull is permitted")
        self.ssh(user.pw_name, "true")
        archive = self.helper / "oraclelinux.tar"
        self.command("podman", "save", "--output", archive, image, timeout=120)
        archive.chmod(0o644)
        storage = self.helper / str(user.pw_uid)
        prefix = "podman --root %s --runroot %s" % (shlex.quote(str(storage / "podman-root")),
                                                   shlex.quote(str(storage / "podman-run")))
        self.ssh(user.pw_name, prefix + " load --input " + shlex.quote(str(archive)), timeout=120)
        self.podman_stores.append((user.pw_name, prefix))
        name = "resman-" + self.run_id
        self.container_names.append((user.pw_name, name, prefix))
        since = len(field(self.log))
        self.ssh(user.pw_name, prefix + " run --detach --pull=never --name %s %s /bin/sh -c 'while :; do :; done'" % (name, image))
        try:
            inspect = json.loads(self.ssh(user.pw_name, prefix + " inspect " + name))[0]
            pid = inspect["State"]["Pid"]
            original = field("/proc/%d/cgroup" % pid)
            require(original.startswith("0::/user.slice/user-%d.slice/" % user.pw_uid),
                    "rootless container is not inside the UID envelope")
            require(os.stat("/proc/%d/ns/pid" % pid).st_ino != os.stat("/proc/1/ns/pid").st_ino,
                    "rootless fixture does not isolate its PID namespace")
            self.refused(user.pw_uid, "runtime_owned_descendant", since)
            limits = self.verify_payload_restoration_cycle([original], user.pw_uid,
                                                           "runtime_owned_descendant", "rootless")
            require(field("/proc/%d/cgroup" % pid) == original, "container PID migrated")
            self.passed("rootless-envelope", {"pid": pid, "cgroup": original, "inspect_id": inspect["Id"],
                                              "parent_quota": field(self.parent / "cpu.max"),
                                              "uid_weight": field(self.slice(user.pw_uid) / "cpu.weight"),
                                              "owned_units": self.assert_owned_units(), "payload_limits": limits})
        finally:
            self.ssh(user.pw_name, prefix + " rm --force " + name)
            self.container_names.remove((user.pw_name, name, prefix))
        self.resources()

    def keep_unit(self):
        # This requires a root-owned exact-command helper authorized in sudoers,
        # not broad permission to invoke systemd-nspawn with arbitrary arguments.
        if not self.root.is_dir() or not shutil.which("visudo") or not shutil.which("sudo"):
            raise Blocked("keep-unit requires prepared nspawn root, sudo and visudo")
        user = self.accounts[0]
        wrapper = self.helper / "keep-unit"
        machine = "resman-" + self.run_id + "-nested"
        self.nested_machines.add(machine)
        command = ["/usr/bin/systemd-nspawn", "--quiet", "--boot", "--register=no", "--keep-unit",
                   "--volatile=overlay", "--directory=" + str(self.root), "--machine=" + machine]
        wrapper.write_text("#!/bin/sh\nexec " + shlex.join(command) + "\n")
        wrapper.chmod(0o755)
        self.create_sudoers(Path("/etc/sudoers.d/resman-" + self.run_id),
                            user.pw_name + " ALL=(root) NOPASSWD: " + str(wrapper) + " \"\"\n")
        self.command("visudo", "-cf", self.sudoers)
        cron = Path("/etc/cron.d/resman-nested-" + self.run_id)
        require(not cron.exists(), "nested cron path already exists")
        self.coverage_crons.append(cron)
        cron.write_text("SHELL=/bin/bash\nPATH=/usr/sbin:/usr/bin:/sbin:/bin\n* * * * * " + user.pw_name +
                        " /usr/bin/flock -n " + str(self.helper / str(user.pw_uid) / "nested.lock") +
                        " sudo -n " + str(wrapper) + "\n")
        cron.chmod(0o600)
        since = len(field(self.log))
        identity = None
        try:
            def locate():
                nonlocal identity
                for proc in Path("/proc").iterdir():
                    if not proc.name.isdecimal():
                        continue
                    try:
                        argv = (proc / "cmdline").read_bytes().split(b"\0")
                        if argv[0] == b"/usr/bin/systemd-nspawn" and os.fsencode("--machine=" + machine) in argv:
                            identity = self.record_nested_supervisor(proc, machine, user.pw_uid)
                            return True
                    except FileNotFoundError:
                        continue
                return False
            eventually(locate, "cron did not create the nested nspawn PAM fixture", 90)
            cron.unlink()
            self.refused(user.pw_uid, "runtime_owned_descendant", since)
            payloads = self.nested_payloads(identity)
            require(payloads, "nested nspawn has no live foreign-namespace payload")
            def complete():
                return any(row["uid"] == user.pw_uid and row["cpu_authority_coverage"] == "complete"
                           for row in self.rows())
            eventually(complete, "nested workload CPU envelope is not complete")
            self.confirm_nested_supervisor(Path("/proc/%d" % identity["pid"]), identity, "post-envelope")
            require(self.nested_payloads(identity) == payloads, "nested payload identity or placement changed")
            limits = self.verify_payload_restoration_cycle([p["cgroup"] for p in payloads] + [identity["cgroup"]],
                                                           user.pw_uid, "runtime_owned_descendant", "nspawn-nested")
            self.passed("nspawn-keep-unit", {"supervisor": identity, "owned_units": self.assert_owned_units(),
                                            "payloads": payloads, "cpu_authority_coverage": "complete",
                                            "cpu_weight": field(self.slice(user.pw_uid) / "cpu.weight"), "payload_limits": limits})
        finally:
            if cron.exists():
                cron.unlink()
            self.cleanup_nested()
            self.remove_sudoers()
        self.resources()

    def record_nested_supervisor(self, proc, machine, uid):
        identity = {"pid": int(proc.name), "cgroup": field(proc / "cgroup"),
                    "birth": field(proc / "stat").rsplit(")", 1)[1].split()[19], "machine": machine}
        self.nested_processes[int(proc.name)] = identity
        match = re.match(r"0::/user\.slice/user-%d\.slice/session-([a-z0-9]+)\.scope(?:/|$)" % uid,
                         identity["cgroup"])
        require(match is not None, "nested nspawn is not inside a genuine PAM scope")
        identity["session"] = match[1]
        identity["session_identity"] = self.capture_nested_session(match[1], uid)
        self.confirm_nested_supervisor(proc, identity, "discovery")
        return identity

    def confirm_nested_supervisor(self, proc, identity, phase):
        final_birth = field(proc / "stat").rsplit(")", 1)[1].split()[19]
        final_cgroup = field(proc / "cgroup")
        scope = "0::" + identity["session_identity"]["snapshot"]["control_group"]
        self.save("nested-supervisor-" + str(identity["pid"]) + "-" + phase, {
            "before": identity, "after_birth": final_birth, "after_cgroup": final_cgroup})
        # nspawn may initialize its supervisor subgroup during discovery. That
        # does not change the authoritative session which this fixture owns.
        require(final_birth == identity["birth"] and
                (final_cgroup == scope or final_cgroup.startswith(scope + "/")),
                "nested supervisor changed identity or left the owned session")
        identity["cgroup"] = final_cgroup

    def nested_scope_snapshot(self, scope):
        result = self.command("systemctl", "show", scope, "-p", "LoadState", "-p", "ActiveState",
                              "-p", "InvocationID", "-p", "ControlGroup", check=False)
        values = dict(line.split("=", 1) for line in result.stdout.splitlines() if "=" in line)
        if values.get("LoadState") == "not-found":
            return None
        require(result.returncode == 0 and values.get("LoadState") == "loaded", "cannot inspect owned nested scope")
        control = values.get("ControlGroup", "")
        if not control and values.get("ActiveState") in ("inactive", "failed"):
            return None
        require(control.startswith("/user.slice/user-") and ".." not in Path(control).parts,
                "nested scope has no exact user cgroup")
        path = Path("/sys/fs/cgroup") / control.lstrip("/")
        try:
            first = path.stat()
        except FileNotFoundError:
            return None  # The original cgroup has drained and disappeared.
        try:
            events = dict(line.split() for line in field(path / "cgroup.events").splitlines())
            last = path.stat()
        except FileNotFoundError:
            require(not path.exists(), "nested scope accounting disappeared before cleanup")
            return None
        require((first.st_dev, first.st_ino) == (last.st_dev, last.st_ino), "nested scope changed during inspection")
        return {"scope": scope, "control_group": control, "invocation_id": values.get("InvocationID", ""),
                "cgroup_identity": [first.st_dev, first.st_ino], "active_state": values.get("ActiveState"),
                "populated": events.get("populated")}

    def capture_nested_session(self, session, uid):
        properties = self.command("loginctl", "show-session", session, "-p", "User", "-p", "Scope").stdout
        values = dict(line.split("=", 1) for line in properties.splitlines() if "=" in line)
        scope = "session-" + session + ".scope"
        require(values == {"User": str(uid), "Scope": scope}, "nested session owner or scope differs")
        snapshot = self.nested_scope_snapshot(scope)
        require(snapshot is not None and snapshot["control_group"] == "/user.slice/user-%d.slice/%s" % (uid, scope)
                and snapshot["invocation_id"], "nested session lacks a stable authoritative scope")
        return {"session": session, "uid": uid, "snapshot": snapshot}

    def nested_cgroup_drained(self, original):
        path = Path("/sys/fs/cgroup") / original["control_group"].lstrip("/")
        try:
            first = path.stat()
            require([first.st_dev, first.st_ino] == original["cgroup_identity"],
                    "nested cgroup identity changed after unit disappearance")
            events = dict(line.split() for line in (path / "cgroup.events").read_text().splitlines())
            last = path.stat()
            require((first.st_dev, first.st_ino) == (last.st_dev, last.st_ino),
                    "nested cgroup changed during drain confirmation")
            return events.get("populated") == "0"
        except FileNotFoundError:
            require(not path.exists(), "nested cgroup lost accounting before drain confirmation")
            return True

    def drain_nested_session(self, owned):
        original = owned["snapshot"]
        scope = original["scope"]
        require(scope == "session-" + owned["session"] + ".scope" and
                original["control_group"] == "/user.slice/user-%d.slice/%s" % (owned["uid"], scope),
                "nested session cleanup identity is inconsistent")
        proof = {"owned": owned}
        name = "nested-session-drain-" + owned["session"]
        def inspect():
            current = self.nested_scope_snapshot(scope)
            proof["last_snapshot"] = current
            self.save(name, proof)
            if current is not None:
                for key in ("scope", "control_group", "invocation_id", "cgroup_identity"):
                    require(current[key] == original[key], "nested session identity changed before cleanup: " + key)
            else:
                require(self.nested_cgroup_drained(original),
                        "nested scope disappeared while its payload remains populated")
            return current
        current = inspect()
        if current is None:
            proof["drained"] = True
            self.save(name, proof)
            return
        proof["terminate_returncode"] = self.command("loginctl", "terminate-session", owned["session"], check=False).returncode
        current = inspect()
        if current is not None and current["populated"] == "1":
            # Termination can acknowledge before container PID 1 exits. Only the
            # original, revalidated run-owned PAM scope may receive this signal.
            killed = self.command("systemctl", "kill", "--kill-who=all", "--signal=SIGKILL", scope, check=False)
            proof["kill_returncode"] = killed.returncode
            current = inspect()
            require(killed.returncode == 0 or current is None, "failed to kill owned nested payload")
        def drained():
            snapshot = inspect()
            return snapshot is None or (snapshot["populated"] == "0" and snapshot["active_state"] in ("inactive", "failed"))
        eventually(drained, "owned nested session payload survived cleanup", 20)
        proof["drained"] = True
        self.save(name, proof)

    def io_transfer(self, user, unit, args):
        self.child_units.append(unit)
        self.command("systemctl", "reset-failed", unit, check=False)
        command = ["systemd-run", "--quiet", "--wait", "--pipe", "--unit=" + unit,
                   "--uid=" + str(user.pw_uid), "-p", "Slice=user-%d.slice" % user.pw_uid,
                   "-p", "RuntimeMaxSec=90", "/usr/bin/dd", *args, "status=none"]
        result = {"command": command, "returncode": None}
        try:
            completed = self.command(*command, check=False, timeout=100)
            result.update(returncode=completed.returncode, output=completed.stdout)
        except (OSError, subprocess.TimeoutExpired) as error:
            result["error"] = str(error)
        return result

    def measure_io_phase(self, user, target, dimension, phase):
        name = "hard-io-%s-%s" % (dimension, phase)
        unit = "resman-coverage-%s-%s-%s" % (self.run_id, dimension, phase)
        # A successful transfer creates a real sparse device-counter baseline.
        # It is outside both the requested byte count and measured interval.
        warmup = self.io_transfer(user, unit + "-warmup.service", ["if=" + str(target), "of=/dev/null",
                                  "iflag=direct", "bs=1M", "count=1"])
        before = io_sample(self.slice(user.pw_uid))
        proof = {"warmup": warmup, "before": before, "after": None,
                 "scope": "daemon hard-limit delivery; fixture accounting and unmeasured warmup only"}
        self.save(name + "-raw", proof)
        require(warmup["returncode"] == 0, "I/O baseline warmup failed")
        validate_io_sample(before)
        is_read, is_iops = dimension.startswith("r"), dimension.endswith("iops")
        size = (32 if is_iops else 512) << 20
        args = (["if=" + str(target), "of=/dev/null", "iflag=direct"] if is_read else
                ["if=/dev/zero", "of=" + str(target), "oflag=direct", "conv=notrunc,fdatasync"])
        args += ["bs=4K", "count=8192"] if is_iops else ["bs=1M", "count=512"]
        start = time.monotonic()
        measured = self.io_transfer(user, unit + ".service", args)
        elapsed = time.monotonic() - start
        after = io_sample(self.slice(user.pw_uid))
        proof.update(after=after, command_result=measured, start=start, end=start + elapsed,
                     bytes_requested=size)
        # Save the unsuccessful interval too, before any capability or value check.
        self.save(name + "-raw", proof)
        require(measured["returncode"] == 0, "measured I/O command failed")
        validate_io_sample(after)
        require(before["identity"] == after["identity"], "I/O cgroup changed across measurement")
        return io_measurement(before["io_stat"], after["io_stat"], size, elapsed, dimension)

    def hard_io(self):
        # A slow disk cannot prove a working cap: require an independently measured
        # uncapped control on the same run-owned file and device before comparing.
        user = self.accounts[0]
        target = self.helper / str(user.pw_uid) / "direct-io.bin"
        dev = target.parent.stat().st_dev
        backing = self.backing_devices("%d:%d" % (os.major(dev), os.minor(dev)))
        if "8:0" not in backing:
            raise Blocked("run-owned direct-I/O file is not backed by device 8:0: " + str(sorted(backing)))
        require(not target.exists() and not target.is_symlink(), "direct-I/O target already exists")
        measurements = {}
        blocked = []
        try:
            # Keep the accounting controller available while the daemon is stopped
            # for the uncapped control. This owned child sets no resource limit.
            anchor = "resman-coverage-%s-io-accounting.service" % self.run_id
            self.child_units.append(anchor)
            self.command("systemctl", "reset-failed", anchor, check=False)
            self.command("systemd-run", "--quiet", "--unit=" + anchor, "--uid=" + str(user.pw_uid),
                         "-p", "Slice=user-%d.slice" % user.pw_uid, "-p", "IOAccounting=yes",
                         "-p", "RuntimeMaxSec=1800", "/usr/bin/sleep", "1800")
            for dimension in IO_DIMENSIONS:
                self.stop_daemon()
                self.assert_released()
                # A real written extent makes uncached reads measurable; never use
                # sparse files or a pre-existing operator file as the read control.
                self.command("dd", "if=/dev/zero", "of=" + str(target), "bs=1M", "count=512",
                             "oflag=direct", "conv=fdatasync", "status=none", timeout=120)
                os.chown(target, user.pw_uid, user.pw_gid)
                per_dimension = {}
                for phase in ("uncapped", "capped"):
                    if phase == "capped":
                        expected = self.configure_io_dimension(dimension)
                        self.start_daemon()
                        self.assert_applied()
                        eventually(lambda: io_values(field(self.slice(user.pw_uid) / "io.max", "")) == expected,
                                   "I/O measurement has another finite dimension or the wrong target cap")
                    per_dimension[phase] = self.measure_io_phase(user, target, dimension, phase)
                measurements[dimension] = per_dimension
                self.save("hard-io-measurements", {"device": "8:0", "backing_devices": sorted(backing), "dimensions": measurements})
                try:
                    verify_io_dimension(dimension, per_dimension)
                except Blocked as err:
                    blocked.append(str(err))
            if blocked:
                raise Blocked("; ".join(blocked))
            require(set(measurements) == set(IO_DIMENSIONS), "not every hard I/O dimension was measured")
            self.passed("hard-io-delivery", {"device": "8:0", "dimensions": measurements,
                                              "scope": "four independent single-dimension direct-I/O controls"})
        finally:
            self.stop_daemon()
            self.assert_released()
            self.write_config(blackout=False)
            self.start_daemon()
            self.assert_applied()
            self.resources()

    def configure_io_dimension(self, dimension):
        require(dimension in IO_DIMENSIONS, "unknown I/O measurement dimension")
        self.write_config(blackout=False)
        body = self.config.read_text()
        expected = dict.fromkeys(IO_DIMENSIONS, "max")
        for candidate, (key, cap) in IO_DIMENSIONS.items():
            value = str(cap) if candidate == dimension else "0"
            body, count = re.subn(r"(?m)^" + key + r"=.*$", key + "=" + value, body)
            require(count == 1, "missing isolated I/O fixture key: " + key)
            if candidate == dimension:
                expected[candidate] = value
        self.config.write_text(body)
        return expected

    def backing_devices(self, device, visited=None):
        visited = set() if visited is None else visited
        if device in visited:
            return visited
        visited.add(device)
        node = Path("/sys/dev/block") / device
        if (node / "partition").exists():
            self.backing_devices(field(node.resolve().parent / "dev"), visited)
        slaves = node / "slaves"
        if slaves.is_dir():
            for slave in slaves.iterdir():
                self.backing_devices(field(slave / "dev"), visited)
        return visited

    def nested_payloads(self, supervisor):
        scope = re.match(r"(0::/user\.slice/user-[0-9]+\.slice/session-[a-z0-9]+\.scope)(?:/|$)",
                         supervisor["cgroup"])
        require(scope is not None, "nested supervisor has no PAM scope")
        result = []
        host_namespace = os.stat("/proc/1/ns/pid").st_ino
        for proc in Path("/proc").iterdir():
            if not proc.name.isdecimal():
                continue
            try:
                cgroup = field(proc / "cgroup")
                if not cgroup.startswith(scope[1] + "/payload/") and cgroup != scope[1] + "/payload":
                    continue
                if proc.stat().st_uid not in {a.pw_uid for a in self.accounts}:
                    continue
                stat = field(proc / "stat").rsplit(")", 1)[1].split()
                namespace = os.stat(proc / "ns/pid").st_ino
                require(stat[0] != "Z" and namespace != host_namespace, "nested payload is not a live isolated process")
                result.append({"pid": int(proc.name), "birth": stat[19], "cgroup": cgroup, "pid_namespace": namespace})
            except FileNotFoundError:
                continue
        return sorted(result, key=lambda item: item["pid"])

    def cleanup_nested(self):
        # Discovery also runs after readiness failure: a launched supervisor is
        # owned by its exact run identifier before placement has been accepted.
        for proc in Path("/proc").iterdir():
            if not proc.name.isdecimal():
                continue
            try:
                argv = (proc / "cmdline").read_bytes().split(b"\0")
                for machine in self.nested_machines:
                    if os.fsencode("--machine=" + machine) in argv and argv[0] == b"/usr/bin/systemd-nspawn":
                        self.nested_processes.setdefault(int(proc.name), {
                            "pid": int(proc.name), "birth": field(proc / "stat").rsplit(")", 1)[1].split()[19],
                            "machine": machine})
            except FileNotFoundError:
                continue
        for pid, identity in list(self.nested_processes.items()):
            proc = Path("/proc/%d" % pid)
            try:
                if "session_identity" in identity:
                    self.drain_nested_session(identity["session_identity"])
                    del self.nested_processes[pid]
                    continue
                stat = field(proc / "stat").rsplit(")", 1)[1].split()
                if stat[19] != identity["birth"]:
                    del self.nested_processes[pid]
                    continue
                argv = (proc / "cmdline").read_bytes().split(b"\0")
                require(argv[0] == b"/usr/bin/systemd-nspawn" and
                        os.fsencode("--machine=" + identity["machine"]) in argv,
                        "nested supervisor identity changed before cleanup")
                match = re.fullmatch(r"0::/user\.slice/user-%d\.slice/session-([a-z0-9]+)\.scope(?:/.*)?" %
                                     self.accounts[0].pw_uid, field(proc / "cgroup"))
                if match:
                    identity["session_identity"] = self.capture_nested_session(match[1], self.accounts[0].pw_uid)
                    require(field(proc / "stat").rsplit(")", 1)[1].split()[19] == identity["birth"],
                            "nested supervisor changed before cleanup")
                    self.drain_nested_session(identity["session_identity"])
                else:
                    os.kill(pid, signal.SIGTERM)
                eventually(lambda p=proc: not p.exists(), "nested supervisor survived bounded cleanup", 20)
                del self.nested_processes[pid]
            except FileNotFoundError:
                del self.nested_processes[pid]

    def cleanup(self):
        errors = []
        def attempt(operation):
            try:
                operation()
            except Exception as err:
                errors.append(str(err))
        for cron in self.coverage_crons:
            attempt(lambda p=cron: p.unlink(missing_ok=True))
        if self.sudoers:
            attempt(self.remove_sudoers)
        attempt(self.cleanup_nested)
        for user, name, prefix in self.container_names:
            attempt(lambda u=user, p=prefix, n=name: self.ssh(u, p + " rm --force " + n))
        for user, prefix in self.podman_stores:
            attempt(lambda u=user, p=prefix: self.ssh(u, p + " unmount --all"))
        for machine, unit in list(self.machine_units):
            attempt(lambda m=machine, u=unit: self.stop_machine(m, u))
            attempt(lambda u=unit: self.command("systemctl", "reset-failed", u, check=False))
        for unit in self.child_units:
            attempt(lambda u=unit: self.stop_owned_unit(u))
            attempt(lambda u=unit: self.command("systemctl", "reset-failed", u, check=False))
        attempt(super().cleanup)
        require(not errors, "cleanup failures: " + "; ".join(errors))

    def redact_credentials(self):
        # The fixture credential is ephemeral and never goes into command argv.
        # Preserve the exact authored digest, but do not distribute a usable token.
        from native_gate import sha
        if self.config.exists():
            self.save("config-source", {"sha256": sha(self.config), "credential_redacted_in_retained_copy": True})
        for path in self.evidence.rglob("*"):
            if not path.is_file() or path.is_symlink():
                continue
            try:
                body = path.read_text()
            except UnicodeDecodeError:
                continue
            if self.mcp_token in body:
                path.write_text(body.replace(self.mcp_token, "<redacted-ephemeral-fixture-credential>"))

    def run(self):
        status, detail, cleanup = "FAIL", "coverage incomplete", "PASS"
        failed = False
        try:
            self.preflight()
            self.validate()
            self.write_config(blackout=False)
            self.start_sessions()
            self.capture_resource_baselines()
            self.start_daemon()
            self.assert_applied()
            self.resources()
            self.ssh_sessions()
            for names, action in ((["child-units"], self.children),
                                  (["rootless-envelope"], self.rootless),
                                  (["nspawn-machine-split", "nspawn-machine-only"], self.nspawn_machine),
                                  (["nspawn-keep-unit"], self.keep_unit),
                                  (["nspawn-shifted-bounded"], self.nspawn_shifted),
                                  (["hard-io-delivery"], self.hard_io)):
                self.attempt(names, action)
            detail = "native ownership coverage completed; capability refusals remain BLOCKED"
        except Blocked as err:
            status, detail = "BLOCKED", str(err)
        except Exception as err:
            failed = True
            detail = str(err)
            traceback.print_exc()
            self.checks.setdefault("resource-properties", "FAIL")
        finally:
            try:
                self.cleanup()
            except Exception as err:
                cleanup, status = "FAIL", "FAIL"
                detail += "; cleanup: " + str(err)
                traceback.print_exc()
            self.redact_credentials()
            self.checks = {name: self.checks.get(name, "BLOCKED") for name in REQUIRED_CHECKS}
            if cleanup == "PASS" and not failed:
                status = result_for(self.checks)
            self.save("checks", self.checks)
            (self.evidence / "result").write_text(status + "\n")
            (self.evidence / "environment.txt").write_text(
                "scenario=systemd-native-coverage\nsource_revision=%s\nkernel=%s\ncleanup=%s\nresult=%s\ndetail=%s\n" %
                (self.revision, os.uname().release, cleanup, status, detail.replace("\n", " ")))
        print(status + ": " + detail, flush=True)
        return {"PASS": 0, "BLOCKED": 77, "FAIL": 1}[status]


if __name__ == "__main__":
    require(sys.argv[1] == "systemd-native-coverage", "unsupported native coverage scenario")
    gate = CoverageGate(Path(__file__).parent, sys.argv[2], sys.argv[3])
    def interrupted(_signal, _frame):
        raise RuntimeError("native coverage interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    sys.exit(gate.run())
