#!/usr/bin/env python3
"""Revision-bound native lifecycle gate on an explicitly disposable systemd host.

No broad user kill, unguarded revert, package configuration replacement or evidence
deletion is permitted. A failed cleanup is a failed result, never an automatic reset.
"""
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import signal
import sqlite3
import subprocess
import sys
import time
import traceback
import urllib.request


class Blocked(RuntimeError):
    pass


REQUIRED_CHECKS = frozenset({
    "full-budget-rejection", "pam-sessions", "blackout-suppression", "blackout-observation",
    "native-plan", "resource-properties", "authority-split", "crash-reclaim", "graceful-stop", "root-only-release",
})
CURRENT_SCHEMA_VERSION = 7


def checks_pass(checks):
    return set(checks) == REQUIRED_CHECKS and all(value == "PASS" for value in checks.values())


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def eventually(check, message, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.5)
    raise AssertionError(message)


def wait_for_json(path, message, seconds=60):
    """Wait until a readiness document exists and contains one complete JSON value."""
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            return json.loads(Path(path).read_text())
        except (FileNotFoundError, json.JSONDecodeError):
            time.sleep(0.5)
    raise AssertionError(message)


def field(path, fallback=None):
    try:
        return Path(path).read_text().strip()
    except FileNotFoundError:
        if fallback is not None:
            return fallback
        raise


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def read_current_observations(path):
    with sqlite3.connect("file:%s?mode=ro" % path, uri=True) as db:
        db.row_factory = sqlite3.Row
        require(db.execute("PRAGMA user_version").fetchone()[0] == CURRENT_SCHEMA_VERSION,
                "unexpected schema")
        return [dict(row) for row in db.execute(
            "SELECT uid, cpu_usage_percent, cpu_authority_coverage, ram_coverage, ram_cgroup_usage_bytes, "
            "cpu_weight, cpu_points_lifecycle_state FROM user_metrics "
            "WHERE sample_epoch_id=(SELECT MAX(sample_epoch_id) FROM user_metrics) ORDER BY uid")]


class NativeGate:
    def __init__(self, bundle, run_id, revision):
        require(re.fullmatch(r"r[a-z0-9-]+", run_id), "unsafe run id")
        self.bundle = Path(bundle)
        self.evidence = self.bundle / "evidence"
        self.evidence.mkdir(mode=0o700, exist_ok=True)
        self.work = self.evidence / "work"
        self.work.mkdir(mode=0o700)
        self.helper = Path("/var/tmp/resman-native-" + run_id)
        self.cron = Path("/etc/cron.d/resman-native-" + run_id)
        self.unit = "resman-native-" + run_id + ".service"
        self.split_unit = "resman-native-split-" + run_id + ".service"
        self.journal = Path("/var/lib/resman/systemd-property-leases.json")
        self.parent = Path("/sys/fs/cgroup/user.slice")
        self.config = self.work / "resman.conf"
        self.map = self.work / "cpu-points.map"
        self.log = self.work / "resman.log"
        self.db = self.work / "metrics.db"
        self.binary = self.bundle / "resman"
        self.accounts = []
        self.sessions = {}
        self.checks = {}
        self.started = False
        self.owned_host = False
        self.revision = revision
        self.run_id = run_id
        self.port = 23000 + os.getpid() % 5000
        self.cron_created = False

    def command(self, *args, check=True, timeout=30, input=None):
        with (self.evidence / "commands.jsonl").open("a") as log:
            log.write(json.dumps([str(a) for a in args]) + "\n")
        result = subprocess.run([str(a) for a in args], input=input, universal_newlines=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
        if check and result.returncode:
            raise RuntimeError("command failed (%d): %s\n%s" % (result.returncode, args, result.stdout))
        return result

    def save(self, name, value):
        (self.evidence / (name + ".json")).write_text(json.dumps(value, indent=2) + "\n")

    def passed(self, name, evidence):
        self.save(name, evidence)
        self.checks[name] = "PASS"
        print("PASS:", name, flush=True)

    def parent_cpu_baseline(self):
        kernel_quota = field(self.parent / "cpu.max", "unavailable")
        response = self.command(
            "systemctl", "show", "user.slice", "-p", "CPUQuotaPerSecUSec", check=False,
        )
        properties = response.stdout.strip().splitlines() if response.returncode == 0 else []
        quota_properties = [line.partition("=")[2] for line in properties
                            if line.startswith("CPUQuotaPerSecUSec=")]
        configured_quota = quota_properties[0] if len(quota_properties) == 1 else "unavailable"
        return {
            "kernel_cpu_max": kernel_quota,
            "systemd_cpu_quota_per_sec_usec": configured_quota,
        }

    def parent_cpu_is_unlimited(self):
        kernel_quota = field(self.parent / "cpu.max", "unavailable")
        if kernel_quota != "unavailable":
            return kernel_quota.split()[0] == "max"
        return self.parent_cpu_baseline()["systemd_cpu_quota_per_sec_usec"] == "infinity"

    def validate_parent_cpu_baseline(self):
        baseline = self.parent_cpu_baseline()
        self.save("parent-cpu-baseline", baseline)
        kernel_quota = baseline["kernel_cpu_max"]
        configured_quota = baseline["systemd_cpu_quota_per_sec_usec"]
        if kernel_quota != "unavailable" and kernel_quota.split()[0] != "max":
            raise Blocked("user.slice has a finite kernel CPU quota: " + kernel_quota)
        if configured_quota != "infinity":
            raise Blocked("user.slice does not have an explicit unlimited systemd CPU quota: " + configured_quota)

    def preflight(self):
        if os.geteuid() != 0 or not Path("/run/systemd/system").is_dir():
            raise Blocked("root on a disposable systemd host is required")
        for tool in ("systemctl", "loginctl", "crontab", "flock", "python3", "rpm"):
            if not shutil.which(tool):
                raise Blocked("missing tool: " + tool)
        if self.command("pgrep", "-x", "resman", check=False).returncode == 0:
            raise Blocked("a resman process is active; quiesce it before this campaign")
        if self.command("systemctl", "is-active", "resman", check=False).stdout.strip() == "active":
            raise Blocked("packaged resman service is active")
        if self.journal.exists():
            raise Blocked("an existing ownership journal must be resolved by its owner")
        if list(Path("/run/systemd/system.control").glob("user*.slice.d")):
            raise Blocked("existing user-slice runtime overrides are not disposable")
        # Older systemd may leave the CPU controller disabled below the root until
        # the first CPU property is applied. In that state cpu.max is absent, not
        # finite, so corroborate the kernel view with systemd's configured value.
        self.validate_parent_cpu_baseline()
        for name in ("resman-t1", "resman-t2", "resman-t3"):
            try:
                account = pwd.getpwnam(name)
            except KeyError as err:
                raise Blocked("missing fixture account: " + name) from err
            if self.command("pgrep", "-u", str(account.pw_uid), check=False).returncode == 0:
                raise Blocked(name + " has existing processes; do not disturb analyst work")
            self.accounts.append(account)
        if self.cron.exists() or self.helper.exists():
            raise Blocked("run paths already exist")
        if not Path("/sys/dev/block/8:0").exists():
            raise Blocked("the current lifecycle fixture requires verified block device 8:0")
        if self.command("systemctl", "is-active", "crond", check=False).stdout.strip() != "active":
            raise Blocked("the lifecycle fixture requires the cron/PAM service")
        artifacts = self.evidence / "artifacts"
        artifacts.mkdir(mode=0o700, exist_ok=True)
        shutil.copyfile(self.binary, artifacts / "resman")
        (artifacts / "resman").chmod(0o700)
        self.save("environment", {
            "source_revision": self.revision, "kernel": os.uname().release,
            "host_identity": field("/etc/machine-id"),
            "boot_id": field("/proc/sys/kernel/random/boot_id"),
            "online_cpus": field("/sys/devices/system/cpu/online"),
            "package": self.command("rpm", "-q", "resman").stdout.strip(),
            "tested_binary_sha256": sha(self.binary),
            "artifact_path": "artifacts/resman",
            "tested_artifact": "source-binary, not the installed package",
            "runner_sha256": sha(__file__),
            "workload_sha256": sha(self.bundle / "native-workload.py"),
            "fixture_sha256": sha(self.bundle / "resman.conf.example"),
            "tested_version": self.command(self.binary, "-version").stdout.strip(),
            "swap": field("/proc/swaps"), "run_id": self.run_id,
        })
        self.owned_host = True

    def write_config(self, blackout=True, overcommit=False):
        a, b, excluded = self.accounts
        self.map.write_text("[resman-cpu-points-map-v1]\n%s=%d\n%s=%d\n" %
                            (a.pw_name, 400 if overcommit else 300,
                             b.pw_name, 350 if overcommit else 300))
        self.map.chmod(0o600)
        settings = {
            "USER_INCLUDE_LIST": "^resman-t[12]$", "USER_EXCLUDE_LIST": "^resman-t3$",
            "RAM_USER_INCLUDE_LIST": "^resman-t[12]$", "IO_USER_INCLUDE_LIST": "^resman-t[12]$",
            "POLLING_INTERVAL": "5", "METRICS_CACHE_TTL": "1", "METRICS_REFRESH_INTERVAL": "5",
            "CPU_THRESHOLD": "2", "CPU_RELEASE_THRESHOLD": "1", "CPU_THRESHOLD_DURATION": "0",
            "MIN_ACTIVE_TIME": "5", "IGNORE_SYSTEM_LOAD": "true", "ENABLE_PROMETHEUS": "true",
            "PROMETHEUS_METRICS_BIND_PORT": str(self.port), "LOG_FILE": str(self.log),
            "CPU_POINTS_FILE": str(self.map), "LOG_LEVEL": "INFO", "RAM_LIMIT_ENABLED": "true",
            "RAM_THRESHOLD": "2", "RAM_RELEASE_THRESHOLD": "1", "IO_LIMIT_ENABLED": "true",
            "IO_THRESHOLD": "2", "IO_RELEASE_THRESHOLD": "1", "IO_DEVICE_FILTER": "8:0",
            "METRICS_DB_ENABLED": "true", "METRICS_DB_PATH": str(self.db),
            "METRICS_DB_WRITE_INTERVAL": "5", "BLACKOUT": "* 00-24" if blackout else "",
        }
        body = (self.bundle / "resman.conf.example").read_text()
        for key, value in settings.items():
            body, count = re.subn(r"(?m)^" + re.escape(key) + r"=.*$", key + "=" + value, body)
            require(count == 1, "fixture key missing or duplicated: " + key)
        self.config.write_text(body)
        self.config.chmod(0o600)

    def validate(self):
        # A real foreground start, before creating workloads. Blackout keeps intent
        # suppressed; invalid configuration is BLOCKED, not a product PASS.
        self.write_config(overcommit=True)
        invalid = self.command(self.binary, "-config", self.config, check=False)
        self.save("rejected-config", {"exit": invalid.returncode, "output": invalid.stdout})
        require(invalid.returncode == 78 and "overcommits" in invalid.stdout,
                "750-point upgrade configuration was not rejected explicitly")
        require(not self.journal.exists(), "invalid config mutated ownership")
        self.passed("full-budget-rejection", {"exit": invalid.returncode, "journal_absent": True})
        self.write_config()
        with (self.evidence / "foreground.log").open("w") as log:
            process = subprocess.Popen([str(self.binary), "-config", str(self.config)], stdout=log, stderr=log)
            try:
                eventually(lambda: process.poll() is not None or "Starting main" in field(self.log, "")
                           or "Control cycle" in field(self.log, ""), "foreground startup did not respond", 20)
                if process.poll() is not None:
                    raise Blocked("foreground configuration/startup rejected; see foreground.log")
            finally:
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=75)
        require(not self.journal.exists(), "foreground preflight left ownership state")

    def start_daemon(self):
        self.command("systemctl", "reset-failed", self.unit, check=False)
        self.command("systemd-run", "--quiet", "--unit=" + self.unit,
                     "-p", "Restart=no", "-p", "TimeoutStopSec=75s",
                     self.binary, "-config", self.config)
        self.started = True
        eventually(lambda: self.command("systemctl", "is-active", self.unit, check=False).stdout.strip() == "active",
                   "native daemon did not become active")

    def stop_daemon(self):
        if self.started:
            self.command("systemctl", "stop", self.unit, timeout=85)
            self.started = False

    def workload_args(self):
        return ()

    def session_workload(self):
        return self.bundle / "native-workload.py"

    def start_sessions(self):
        self.helper.mkdir(mode=0o755)
        self.helper.chmod(0o755)
        workload = self.helper / "workload.py"
        shutil.copyfile(self.session_workload(), workload)
        workload.chmod(0o755)
        lines = ["SHELL=/bin/bash", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"]
        for a in self.cron_accounts():
            directory = self.helper / str(a.pw_uid)
            directory.mkdir(mode=0o700)
            os.chown(directory, a.pw_uid, a.pw_gid)
            lines.append("* * * * * %s /usr/bin/flock -n %s/run.lock /usr/bin/python3 %s %s" %
                         (a.pw_name, directory, workload, directory) + "".join(" " + arg for arg in self.workload_args()))
        self.cron.write_text("\n".join(lines) + "\n")
        self.cron.chmod(0o600)
        self.cron_created = True
        for a in self.cron_accounts():
            path = self.helper / str(a.pw_uid) / "identity.json"
            identity = wait_for_json(path, "cron did not publish complete workload identity for " + a.pw_name, 90)
            match = re.fullmatch(r"0::/user.slice/user-%d.slice/session-([a-z0-9]+).scope" % a.pw_uid,
                                 identity["cgroup"])
            require(match is not None, "workload is not in a real PAM session: " + str(identity))
            identity["session"] = match[1]
            self.sessions[a.pw_uid] = identity
            state = self.command("loginctl", "show-session", match[1], "-p", "User", "-p", "Scope").stdout
            require("User=%d" % a.pw_uid in state, "logind does not own workload")
        self.cron.unlink()
        self.cron_created = False
        self.passed("pam-sessions", self.sessions)

    def session_accounts(self):
        return self.accounts

    def cron_accounts(self):
        return self.session_accounts()

    def scrape(self):
        with urllib.request.urlopen("http://127.0.0.1:%d/metrics" % self.port, timeout=5) as response:
            return response.read().decode()

    def blackout(self):
        start = len(field(self.log, ""))
        eventually(lambda: "blackout" in field(self.log, "")[start:].lower(), "blackout was not observed", 25)
        time.sleep(12)  # Measure across multiple decision deadlines, not a readiness delay.
        text = field(self.log, "")[start:]
        require("requested_policy_intent=activate" not in text,
                "blackout emitted activation intent")
        require(self.parent_cpu_is_unlimited(), "blackout applied the pool")
        require(not self.journal.exists(), "blackout created leases")
        scrape = self.scrape()
        (self.evidence / "blackout.prom").write_text(scrape)
        self.passed("blackout-suppression", {"log": text, "no_leases": True})
        observed = bool(re.search(r'resman_enforcement_mode\{[^\n]*mode="systemd_native"[^\n]*\} 1', scrape))
        available = bool(re.search(r'resman_observation_host_cpu_sample_available(?:\{[^\n]*\})? 1', scrape))
        self.checks["blackout-observation"] = "PASS" if observed and available else "FAIL"
        self.save("blackout-observation", {"native_mode_published": observed, "host_sample_available": available})
        print(self.checks["blackout-observation"] + ": blackout-observation", flush=True)
        self.write_config(blackout=False)
        self.command("systemctl", "kill", "--kill-who=main", "--signal=HUP", self.unit)

    def slice(self, uid):
        return self.parent / ("user-%d.slice" % uid)

    def assert_applied(self):
        cpus = len(os.sched_getaffinity(0))
        # The gate refuses a cpuset-restricted runner instead of using its affinity
        # count as the host's online denominator.
        online = sum((int(b) - int(a) + 1) if sep else 1 for item in field("/sys/devices/system/cpu/online").split(",")
                     for a, sep, b in [item.partition("-")])
        require(cpus == online, "runner affinity differs from online capacity")
        expected = "%d 100000" % (online * 90000)
        eventually(lambda: field(self.parent / "cpu.max", "") == expected, "native parent quota not applied")
        for account, weight in zip(self.accounts, (9900, 9900, 3300)):
            eventually(lambda a=account, w=weight: field(self.slice(a.pw_uid) / "cpu.weight", "") == str(w),
                       "native sibling weight not applied")
        require(field(self.slice(0) / "cpu.weight") == "3300", "root weight is not its explicit entitlement")
        require(field(self.slice(0) / "cpu.max").split()[0] == "max", "root has a leaf CPU ceiling")
        self.assert_membership()
        self.passed("native-plan", {"parent": expected, "weights": [9900, 9900, 3300, 3300], "online": online})

    def assert_membership(self):
        for identity in self.sessions.values():
            pid = identity["pid"]
            require(field("/proc/%d/cgroup" % pid) == identity["cgroup"], "PID left its PAM scope")
            require(field("/proc/%d/stat" % pid).rsplit(")", 1)[1].split()[19] == identity["start_time"], "PID identity changed")

    def resources(self):
        for a in self.accounts[:2]:
            directory = self.slice(a.pw_uid)
            eventually(lambda: field(directory / "memory.max", "") == "536870912", "memory.max not applied")
            expected_high = (int(536870912 * 0.8) // os.sysconf("SC_PAGE_SIZE")) * os.sysconf("SC_PAGE_SIZE")
            require(field(directory / "memory.high") == str(expected_high), "shipped ratio/page rounding differs")
            eventually(lambda: "8:0 rbps=104857600 wbps=52428800 riops=1000 wiops=500" in field(directory / "io.max", ""),
                       "per-device I/O limits not verified")
            require(int(field(directory / "memory.current")) >= 64 << 20, "existing memory charges were lost")
        self.passed("resource-properties", {str(a.pw_uid): {
            name: field(self.slice(a.pw_uid) / name) for name in ("memory.high", "memory.max", "memory.current", "io.max")
        } for a in self.accounts[:2]})
        self.observations()

    def observations(self):
        rows = read_current_observations(self.db)
        self.save("observations", rows)
        (self.evidence / "active.prom").write_text(self.scrape())

    def authority_split(self):
        uid = self.accounts[0].pw_uid
        self.command("systemctl", "reset-failed", self.split_unit, check=False)
        self.command("systemd-run", "--quiet", "--unit=" + self.split_unit, "--uid=" + str(uid), "/usr/bin/sleep", "120")
        directory = self.slice(uid)
        try:
            eventually(lambda: field(directory / "memory.max") == "max", "authority loss left memory restriction")
            eventually(lambda: "rbps=104857600" not in field(directory / "io.max"), "authority loss left I/O restriction")
            require(field(directory / "cpu.weight") == "9900", "resource refusal erased CPU scheduling")
            eventually(lambda: "authority_split" in field(self.log), "authority refusal not typed")
            self.passed("authority-split", {"memory": field(directory / "memory.max"), "io": field(directory / "io.max"),
                                             "weight": field(directory / "cpu.weight")})
        finally:
            self.command("systemctl", "stop", self.split_unit)
        self.resources()

    def restart(self):
        before = json.loads(self.journal.read_text())
        self.save("journal-before-crash", before)
        self.command("systemctl", "kill", "--kill-who=main", "--signal=KILL", self.unit)
        eventually(lambda: self.command("systemctl", "is-active", self.unit, check=False).stdout.strip() != "active",
                   "daemon survived kill -9")
        start = len(field(self.log))
        self.started = False
        self.start_daemon()
        eventually(lambda: "lease recovery completed" in field(self.log)[start:], "restart did not report recovery")
        self.assert_applied()
        self.resources()
        self.passed("crash-reclaim", {"before_units": len(before["units"]), "log": field(self.log)[start:]})

    def assert_released(self):
        eventually(self.parent_cpu_is_unlimited, "parent quota survived release")
        try:
            eventually(lambda: not self.journal.exists(), "ownership journal survived release")
        except AssertionError:
            self.save("release-failure-%d" % int(time.time() * 1000000000), {
                "journal": json.loads(self.journal.read_text()) if self.journal.exists() else None,
                "units": {str(a.pw_uid): {
                    "bus": self.command("systemctl", "show", "user-%d.slice" % a.pw_uid,
                                        "-p", "ActiveState", "-p", "DropInPaths", "-p", "IOWriteBandwidthMax",
                                        "-p", "IOReadBandwidthMax", check=False).stdout,
                    "io.max": field(self.slice(a.pw_uid) / "io.max", "absent"),
                    "memory.max": field(self.slice(a.pw_uid) / "memory.max", "absent"),
                } for a in self.accounts},
            })
            raise
        require(not list(Path("/run/systemd/system.control").glob("user*.slice.d")), "native drop-ins survived release")
        require(field(self.slice(0) / "cpu.weight", "100") == "100", "root weight survived release")

    def logout(self):
        evidence = {}
        for uid, identity in list(self.sessions.items()):
            self.command("loginctl", "terminate-session", identity["session"])
            eventually(lambda p=identity["pid"]: not Path("/proc/%d" % p).exists(), "terminate-session did not reach workload")
            evidence[str(uid)] = identity
            del self.sessions[uid]
        self.assert_released()
        self.passed("root-only-release", evidence)

    def cleanup(self):
        if not self.owned_host:
            return
        if self.cron_created:
            self.cron.unlink()
            self.cron_created = False
        self.stop_daemon()
        # Read actual procfs membership, not a user-writable session identifier,
        # including sessions created while a peer failed readiness.
        for account in self.session_accounts():
            identity_file = self.helper / str(account.pw_uid) / "identity.json"
            if not identity_file.exists():
                continue
            identity = json.loads(identity_file.read_text())
            proc = Path("/proc/%d" % int(identity["pid"]))
            if not proc.exists() or proc.stat().st_uid != account.pw_uid:
                continue
            match = re.fullmatch(r"0::/user.slice/user-%d.slice/session-([a-z0-9]+).scope" % account.pw_uid,
                                 field(proc / "cgroup"))
            if match:
                self.sessions[account.pw_uid] = {"session": match[1]}
            else:
                # An invalid fixture placement must not escape cleanup as PASS.
                # Only the exact recorded helper process may be terminated here.
                stat = field(proc / "stat").rsplit(")", 1)[1].split()
                argv = (proc / "cmdline").read_bytes().split(b"\0")[:-1]
                expected = [b"/usr/bin/python3", os.fsencode(self.helper / "workload.py"),
                            os.fsencode(self.helper / str(account.pw_uid))]
                expected.extend(os.fsencode(arg) for arg in self.workload_args())
                require(stat[19] == identity["start_time"] and argv == expected,
                        "unrecognized workload outside PAM; manual cleanup required")
                os.kill(identity["pid"], signal.SIGTERM)
                eventually(lambda p=proc: not p.exists(), "invalid fixture workload survived termination", 15)
                require(not any(Path("/proc/%d" % child).exists() for child in identity["children"]),
                        "invalid fixture children survived termination")
        for identity in self.sessions.values():
            self.command("loginctl", "terminate-session", identity["session"], check=False)
        self.command("systemctl", "stop", self.split_unit, check=False)
        self.command("systemctl", "reset-failed", self.unit, self.split_unit, check=False)
        self.assert_released()
        # Only helper output is disposable. Binary, config, DB, logs and all evidence stay.
        if self.helper.exists():
            shutil.rmtree(self.helper)

    def run(self):
        status, detail, cleanup = "FAIL", "scenario incomplete", "PASS"
        try:
            self.preflight()
            self.validate()
            self.start_daemon()
            self.start_sessions()
            self.blackout()
            self.assert_applied()
            self.resources()
            # A clean stop on live slices is independent of crash recovery.
            self.checks["graceful-stop"] = "FAIL"
            self.stop_daemon()
            self.assert_released()
            self.assert_membership()
            self.save("ordinary-stop", {"sessions_still_owned": True, "journal_absent": True})
            self.start_daemon()
            self.assert_applied()
            self.resources()
            self.authority_split()
            self.restart()
            self.checks["graceful-stop"] = "FAIL"
            self.stop_daemon()
            self.assert_released()
            self.assert_membership()
            self.passed("graceful-stop", {"sessions_still_owned": True, "journal_absent": True})
            self.start_daemon()
            self.assert_applied()
            self.checks["root-only-release"] = "FAIL"
            self.logout()
            status = "PASS" if checks_pass(self.checks) else "FAIL"
            detail = "native lifecycle checks completed; see checks.json for individual outcomes"
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
                "scenario=systemd-native-lifecycle\nsource_revision=%s\nkernel=%s\ncleanup=%s\nresult=%s\ndetail=%s\n" %
                (self.revision, os.uname().release, cleanup, status, detail.replace("\n", " ")))
            print(status + ": " + detail, flush=True)
        return {"PASS": 0, "BLOCKED": 77, "FAIL": 1}[status]


if __name__ == "__main__":
    require(sys.argv[1] == "systemd-native-lifecycle", "unsupported native scenario")
    gate = NativeGate(Path(__file__).parent, sys.argv[2], sys.argv[3])
    def interrupted(_signal, _frame):
        raise RuntimeError("native campaign interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    sys.exit(gate.run())
