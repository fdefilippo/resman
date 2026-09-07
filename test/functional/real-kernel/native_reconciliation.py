#!/usr/bin/env python3
"""CPU-only native reconciliation with live PAM, bounded topology and opt-in hotplug."""
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import signal
import sqlite3
import sys
import threading
import time
import traceback

from native_gate import Blocked, NativeGate, eventually, field, require, wait_for_json


REQUIRED_CHECKS = frozenset({"policy-reload", "concurrent-reconciliation", "topology-turnover", "cardinality-refusal", "online-cpu-change"})


def online_set(text):
    result = set()
    for part in text.strip().split(","):
        start, separator, end = part.partition("-")
        first, last = int(start), int(end) if separator else int(start)
        require(0 <= first <= last, "invalid online CPU interval")
        result.update(range(first, last + 1))
    require(bool(result), "no online CPU")
    return result


def atomic_write(path, text):
    temporary = path.with_name(path.name + ".candidate")
    with temporary.open("x") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(text)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def expected_weights(mapped, root_points, best_effort, active_uids):
    scale = 10000 // max([root_points, best_effort] + list(mapped.values()))
    unmapped = sorted(uid for uid in active_uids if uid and uid not in mapped)
    aggregate = best_effort * scale
    require(len(unmapped) <= aggregate, "unrepresentable expected topology")
    weights = {uid: points * scale for uid, points in mapped.items() if uid in active_uids}
    if 0 in active_uids:
        weights[0] = root_points * scale
    for index, uid in enumerate(unmapped):
        weights[uid] = aggregate // len(unmapped) + (index < aggregate % len(unmapped))
    return weights


def truthful_row(row):
    state = row["denominator_state"]
    require(state in {"complete", "incomplete", "inactive", "unavailable"}, "unknown denominator state")
    if state == "complete":
        require(row["programmed_sibling_weight_sum"] is not None and row["observed_sibling_weight_sum"] is not None,
                "complete row omitted its denominator")
        require(row["programmed_sibling_weight_sum"] == row["observed_sibling_weight_sum"],
                "complete row acknowledges a divergent denominator")


def journal_advanced(before, after):
    previous, current = before["journal"]["generation"], after["journal"]["generation"]
    require(isinstance(previous, int) and isinstance(current, int) and current > previous,
            "changed plan did not advance its durable journal generation")


def unused_slice(info):
    """Accept synthetic inactive slices, never operator-configured units."""
    values = {}
    for line in info.splitlines():
        key, separator, value = line.partition("=")
        if not separator or key in values:
            return False
        values[key] = value
    return (set(values) == {"LoadState", "ActiveState", "ControlGroup", "FragmentPath", "DropInPaths"}
            and values["LoadState"] in {"not-found", "loaded"}
            and values["ActiveState"] == "inactive"
            and not values["ControlGroup"] and not values["FragmentPath"]
            and values["DropInPaths"] in {"", "/usr/lib/systemd/system/user-.slice.d/10-defaults.conf"})


class ReconciliationGate(NativeGate):
    def __init__(self, *args):
        super().__init__(*args)
        self.probes = {}
        self.candidate_uids = []
        self.hotplug_original = None
        self.hotplug_cpu = None
        self.reconcile_checks = {}
        self.points = (300, 300)
        self.best_effort = 100

    def record(self, name, proof):
        self.save(name, proof)
        self.reconcile_checks[name] = "PASS"

    def preflight(self):
        super().preflight()
        self.hotplug_original = online_set(field("/sys/devices/system/cpu/online"))
        require(os.sched_getaffinity(0) == self.hotplug_original, "runner affinity is restricted")
        if 0 not in self.active_uids():
            raise Blocked("a genuine active root user slice is required")
        # No user accounts are created. A bounded pool of absent unit names and
        # NSS-unused identities is reserved for transient topology-only probes.
        for uid in range(60000, 60032):
            try:
                pwd.getpwuid(uid)
                continue
            except KeyError:
                pass
            if self.command("pgrep", "-u", str(uid), check=False).returncode == 0:
                continue
            info = self.command("systemctl", "show", "user-%d.slice" % uid,
                                "-p", "LoadState", "-p", "ActiveState", "-p", "ControlGroup",
                                "-p", "FragmentPath", "-p", "DropInPaths", check=False).stdout
            if not unused_slice(info) or self.slice(uid).exists():
                continue
            self.save("candidate-slice-%d" % uid, {"uid": uid, "unit_properties": info,
                      "scope": "unused synthetic UID; inherited distribution default is preserved"})
            self.candidate_uids.append(uid)
            if len(self.candidate_uids) == 17:
                break
        if len(self.candidate_uids) != 17:
            raise Blocked("seventeen unused, unconfigured UID slice names are unavailable; no accounts or existing units will be changed")

    def write_config(self, blackout=True, overcommit=False):
        super().write_config(blackout, overcommit)
        body = self.config.read_text()
        for key in ("RAM_LIMIT_ENABLED", "IO_LIMIT_ENABLED"):
            body = re.sub(r"(?m)^" + key + r"=.*$", key + "=false", body)
        self.config.write_text(body)

    def active_uids(self):
        text = self.command("systemctl", "list-units", "--all", "--no-legend", "--plain", "--state=active", "user-*.slice").stdout
        return {int(match.group(1)) for match in re.finditer(r"(?m)^\s*user-([0-9]+)\.slice\s", text)}

    def history(self, after=0):
        with sqlite3.connect("file:%s?mode=ro" % self.db, uri=True) as database:
            database.row_factory = sqlite3.Row
            return [dict(row) for row in database.execute(
                "SELECT sample_epoch_id, denominator_state, cpu_points_degraded, online_cpus, "
                "programmed_parent_quota_usec, programmed_parent_period_usec, programmed_sibling_weight_sum, "
                "observed_sibling_weight_sum FROM system_metrics WHERE sample_epoch_id>? ORDER BY sample_epoch_id", (after,))]

    def latest(self):
        rows = self.history()
        require(bool(rows), "no persisted control interval")
        for row in rows:
            truthful_row(row)
        return rows[-1]

    def kernel(self):
        uids = self.active_uids()
        return {"online": sorted(online_set(field("/sys/devices/system/cpu/online"))),
                "parent_quota": field(self.parent / "cpu.max"),
                "weights": {str(uid): field(self.slice(uid) / "cpu.weight", "unavailable") for uid in sorted(uids)},
                "identities": {str(uid): [self.slice(uid).stat().st_dev, self.slice(uid).stat().st_ino] for uid in sorted(uids)}}

    def snapshot(self):
        journal_before = self.journal.read_bytes()
        kernel = self.kernel()
        row = self.latest()
        journal_after = self.journal.read_bytes()
        require(journal_before == journal_after, "journal changed during stable snapshot")
        return {"kernel": kernel, "history": row, "journal": json.loads(journal_after),
                "journal_sha256": hashlib.sha256(journal_after).hexdigest(),
                "main_sha256": hashlib.sha256(self.config.read_bytes()).hexdigest(),
                "map_sha256": hashlib.sha256(self.map.read_bytes()).hexdigest(), "monotonic": time.monotonic()}

    def stable(self, after=0, expected_uids=None):
        observed = None
        def check():
            nonlocal observed
            try:
                observed = self.snapshot()
                kernel, row = observed["kernel"], observed["history"]
                actual_uids = {int(uid) for uid in kernel["weights"]}
                if expected_uids is not None and actual_uids != set(expected_uids):
                    return False
                mapped = dict(zip((self.accounts[0].pw_uid, self.accounts[1].pw_uid), self.points))
                weights = expected_weights(mapped, 100, self.best_effort, actual_uids)
                expected_quota = len(kernel["online"]) * 90000
                return (row["sample_epoch_id"] > after and row["denominator_state"] == "complete" and
                        not row["cpu_points_degraded"] and row["online_cpus"] == len(kernel["online"]) and
                        row["programmed_parent_quota_usec"] == expected_quota and row["programmed_parent_period_usec"] == 100000 and
                        row["programmed_sibling_weight_sum"] == sum(weights.values()) and
                        kernel["parent_quota"] == "%d 100000" % expected_quota and
                        kernel["weights"] == {str(uid): str(weight) for uid, weight in weights.items()})
            except (AssertionError, OSError, sqlite3.Error):
                return False
        eventually(check, "complete history and exact live kernel plan did not converge", 60)
        self.assert_membership()
        return observed

    def reload_time(self):
        match = re.search(r"(?m)^resman_config_reload_last_success_timestamp_seconds(?:\{[^\n]*\})? ([0-9.eE+-]+)$", self.scrape())
        return float(match.group(1)) if match else 0

    def publish_policy(self, points, best_effort):
        a, b = self.accounts[:2]
        body = re.sub(r"(?m)^CPU_BEST_EFFORT_POINTS=.*$", "CPU_BEST_EFFORT_POINTS=%d" % best_effort, self.config.read_text())
        atomic_write(self.map, "[resman-cpu-points-map-v1]\n%s=%d\n%s=%d\n" % (a.pw_name, points[0], b.pw_name, points[1]))
        atomic_write(self.config, body)
        self.points, self.best_effort = points, best_effort
        self.command("systemctl", "kill", "--kill-whom=main", "--signal=HUP", self.unit)

    def create_probe(self, uid):
        require(uid in self.candidate_uids and uid not in self.probes, "UID is not a fresh reserved topology probe")
        unit = "user-%d.slice" % uid
        require(uid not in self.active_uids(), "probe slice became active before creation")
        info = self.command("systemctl", "show", unit, "-p", "LoadState", "-p", "ActiveState",
                            "-p", "ControlGroup", "-p", "FragmentPath", "-p", "DropInPaths", check=False).stdout
        require(unused_slice(info) and not self.slice(uid).exists(), "probe slice acquired an external configuration or workload")
        service = "resman-native-topology-%s-%d.service" % (self.run_id, uid)
        before = self.command("systemctl", "show", service, "-p", "LoadState", "-p", "FragmentPath", "-p", "DropInPaths", check=False).stdout
        require("LoadState=not-found" in before, "probe service name already exists")
        self.probes[uid] = {"service": service, "invocation": None}
        self.command("systemd-run", "--quiet", "--unit=" + service, "--slice=" + unit,
                     "-p", "RuntimeMaxSec=600", "-p", "TimeoutStopSec=10", "/usr/bin/sleep", "600")
        invocation = self.command("systemctl", "show", service, "--value", "-p", "InvocationID").stdout.strip()
        require(bool(invocation), "probe service has no stable invocation identity")
        self.probes[uid]["invocation"] = invocation
        eventually(lambda: uid in self.active_uids(), "probe slice was not activated")

    def remove_probe(self, uid):
        service = self.probes[uid]["service"]
        recorded = self.probes[uid]["invocation"]
        current = self.command("systemctl", "show", service, "--value", "-p", "InvocationID", check=False).stdout.strip()
        require(not current or (recorded is not None and current == recorded), "probe service was replaced or not confirmed; refusing to stop another owner")
        self.command("systemctl", "stop", service)
        self.command("systemctl", "reset-failed", service, check=False)
        if self.slice(uid).exists():
            require(not any(field(path) for path in self.slice(uid).rglob("cgroup.procs")),
                    "unowned workload entered the probe slice; refusing to stop it")
        self.command("systemctl", "stop", "user-%d.slice" % uid)
        eventually(lambda: uid not in self.active_uids(), "run-owned topology slice did not depart")
        del self.probes[uid]

    def pam_turnover(self, before, expected_initial):
        account = self.accounts[2]
        uid = account.pw_uid
        previous = self.sessions[uid]
        self.command("loginctl", "terminate-session", previous["session"])
        eventually(lambda: not Path("/proc/%d" % previous["pid"]).exists(), "PAM logout did not terminate its workload", 30)
        del self.sessions[uid]
        departed = self.stable(before["history"]["sample_epoch_id"], expected_initial - {uid})
        directory = self.helper / str(uid)
        (directory / "identity.json").unlink()
        require(not self.cron.exists(), "fixture cron path is occupied")
        cron = ("SHELL=/bin/bash\nPATH=/usr/sbin:/usr/bin:/sbin:/bin\n"
                "* * * * * %s /usr/bin/flock -n %s/run.lock /usr/bin/python3 %s/workload.py %s\n" %
                (account.pw_name, directory, self.helper, directory))
        with self.cron.open("x") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(cron)
            stream.flush()
            os.fsync(stream.fileno())
        try:
            identity = wait_for_json(directory / "identity.json",
                                     "new cron/PAM session did not publish complete identity", 90)
            match = re.fullmatch(r"0::/user.slice/user-%d.slice/session-([a-z0-9]+)\.scope" % uid, identity["cgroup"])
            require(match is not None, "new workload is not governed by a genuine PAM scope")
            identity["session"] = match[1]
            require(identity["uid"] == uid and identity["session"] != previous["session"], "PAM session was not recreated")
            self.sessions[uid] = identity
            require(self.command("loginctl", "show-session", identity["session"], "-p", "User", "--value").stdout.strip() == str(uid),
                    "logind session owner does not match the workload")
        finally:
            self.cron.unlink(missing_ok=True)
        rejoined = self.stable(departed["history"]["sample_epoch_id"], expected_initial)
        return {"before": before, "logout": departed, "login": rejoined, "old_session": previous, "new_session": self.sessions[uid]}

    def reload_and_turnover(self):
        initial = self.stable()
        initial_uids = {int(uid) for uid in initial["kernel"]["weights"]}
        expected_initial = {0, *(account.pw_uid for account in self.accounts)}
        if initial_uids != expected_initial:
            raise Blocked("unowned user slices are active; bounded cardinality experiment cannot isolate its denominator")
        acknowledged = self.reload_time()
        self.publish_policy((400, 200), 80)
        reloaded = self.stable(initial["history"]["sample_epoch_id"], expected_initial)
        journal_advanced(initial, reloaded)
        require(initial["main_sha256"] != reloaded["main_sha256"] and initial["map_sha256"] != reloaded["map_sha256"],
                "reload did not exercise both composite sources")
        eventually(lambda: self.reload_time() > acknowledged, "composite reload had no new successful acknowledgement")
        self.record("policy-reload", {"before": initial, "after": reloaded, "success_timestamp": self.reload_time()})
        pam = self.pam_turnover(reloaded, expected_initial)
        reloaded = pam["login"]
        uid = self.candidate_uids[0]
        self.create_probe(uid)
        added = self.stable(reloaded["history"]["sample_epoch_id"], expected_initial | {uid})
        journal_advanced(reloaded, added)
        self.remove_probe(uid)
        removed = self.stable(added["history"]["sample_epoch_id"], expected_initial)
        self.record("topology-turnover", {"pam": pam, "before": reloaded, "joined": added, "departed": removed})
        barrier = threading.Barrier(2)
        def reload():
            barrier.wait(timeout=10)
            self.publish_policy((350, 250), 90)
        def turnover():
            barrier.wait(timeout=10)
            self.create_probe(uid)
        start_epoch = removed["history"]["sample_epoch_id"]
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as workers:
            operations = [workers.submit(reload), workers.submit(turnover)]
            for operation in operations:
                operation.result(timeout=50)
        joined = self.stable(start_epoch, expected_initial | {uid})
        journal_advanced(removed, joined)
        interval = self.history(start_epoch)
        require(bool(interval), "concurrent reconciliation produced no recorded interval")
        for row in interval:
            truthful_row(row)
        self.remove_probe(uid)
        settled = self.stable(joined["history"]["sample_epoch_id"], expected_initial)
        self.record("concurrent-reconciliation", {"intervals": interval, "joined": joined, "settled": settled,
                    "allowed_intermediate_states": ["complete", "incomplete", "inactive", "unavailable"]})

    def cardinality(self):
        before = self.latest()
        self.publish_policy((600, 100), 1)
        baseline = self.stable(before["sample_epoch_id"])
        expected = {int(uid) for uid in baseline["kernel"]["weights"]}
        # 10000//600 = 16; existing excluded slice consumes one of sixteen
        # minimum best-effort weights. Fifteen probes fit; the sixteenth does not.
        for uid in self.candidate_uids[:15]:
            self.create_probe(uid)
            expected.add(uid)
        maximum = self.stable(baseline["history"]["sample_epoch_id"], expected)
        journal = self.journal.read_bytes()
        log_offset = len(field(self.log))
        offender = self.candidate_uids[15]
        self.create_probe(offender)
        eventually(lambda: "best_effort_cardinality" in field(self.log)[log_offset:], "unrepresentable topology did not report typed refusal")
        eventually(lambda: self.latest()["sample_epoch_id"] > maximum["history"]["sample_epoch_id"] and
                   self.latest()["cpu_points_degraded"], "cardinality refusal did not publish degraded state")
        observed = self.kernel()
        require(observed["parent_quota"] == maximum["kernel"]["parent_quota"], "refused plan changed parent quota")
        require(all(observed["weights"][uid] == weight for uid, weight in maximum["kernel"]["weights"].items()),
                "refused plan changed an existing weight")
        require(self.journal.read_bytes() == journal, "refused cardinality plan mutated its ownership journal")
        refusal = self.latest()
        require(refusal["denominator_state"] != "complete", "unrepresented slice was acknowledged as a complete denominator")
        for uid in reversed(list(self.probes)):
            self.remove_probe(uid)
        self.publish_policy((300, 300), 100)
        recovered = self.stable(refusal["sample_epoch_id"], {0, *(account.pw_uid for account in self.accounts)})
        self.record("cardinality-refusal", {"maximum_representable": maximum, "refused": refusal,
                    "kernel_during_refusal": observed, "recovered": recovered, "best_effort_slices": 17,
                    "aggregate_weight": 16, "maximum_scale": 16, "accounts_created": 0})

    def hotplug(self):
        if field(self.bundle / "allow-cpu-hotplug", "no") != "yes":
            raise Blocked("online CPU change requires explicit REAL_KERNEL_ALLOW_CPU_HOTPLUG=1; no CPU was changed")
        current = online_set(field("/sys/devices/system/cpu/online"))
        require(current == self.hotplug_original and len(current) >= 2, "online topology changed outside the fixture")
        cpu = max(current)
        path = Path("/sys/devices/system/cpu/cpu%d/online" % cpu)
        if cpu == 0 or not path.is_file() or not os.access(path, os.W_OK):
            raise Blocked("highest nonzero online CPU is not hotpluggable")
        before = self.stable()
        self.hotplug_cpu = cpu
        path.write_text("0\n")
        eventually(lambda: online_set(field("/sys/devices/system/cpu/online")) == current - {cpu}, "CPU offline did not complete", 15)
        offline = self.stable(before["history"]["sample_epoch_id"])
        require(offline["history"]["online_cpus"] == len(current) - 1, "capacity cache hid the online change")
        path.write_text("1\n")
        eventually(lambda: online_set(field("/sys/devices/system/cpu/online")) == current, "CPU restoration did not complete", 15)
        self.hotplug_cpu = None
        restored = self.stable(offline["history"]["sample_epoch_id"])
        self.record("online-cpu-change", {"cpu": cpu, "before": before, "offline": offline, "restored": restored})

    def cleanup(self):
        errors = []
        if self.hotplug_cpu is not None:
            try:
                Path("/sys/devices/system/cpu/cpu%d/online" % self.hotplug_cpu).write_text("1\n")
                eventually(lambda: online_set(field("/sys/devices/system/cpu/online")) == self.hotplug_original,
                           "cleanup did not restore original CPU topology", 15)
                self.hotplug_cpu = None
            except Exception as error:
                errors.append(str(error))
        for uid in reversed(list(self.probes)):
            try:
                self.remove_probe(uid)
            except Exception as error:
                errors.append(str(error))
        try:
            super().cleanup()
        except Exception as error:
            errors.append(str(error))
        if errors:
            raise RuntimeError("; ".join(errors))

    def run(self):
        status, cleanup, detail = "FAIL", "PASS", "reconciliation incomplete"
        hotplug_intent = field(self.bundle / "hotplug-intent", "0")
        try:
            self.preflight()
            self.validate()
            self.write_config(blackout=False)
            self.start_sessions()
            self.start_daemon()
            self.reload_and_turnover()
            self.cardinality()
            self.hotplug()
            require(set(self.reconcile_checks) == REQUIRED_CHECKS and all(value == "PASS" for value in self.reconcile_checks.values()), "missing reconciliation proof")
            status, detail = "PASS", "native reconciliation verified against live kernel, journal generations and synchronized history"
        except Blocked as error:
            status, detail = "BLOCKED", str(error)
        except Exception as error:
            detail = str(error)
            traceback.print_exc()
        finally:
            try:
                self.cleanup()
            except Exception as error:
                status, cleanup = "FAIL", "FAIL"
                detail += "; cleanup: " + str(error)
                traceback.print_exc()
            for name in REQUIRED_CHECKS:
                self.reconcile_checks.setdefault(name, "BLOCKED")
            self.save("checks", self.reconcile_checks)
            (self.evidence / "result").write_text(status + "\n")
            (self.evidence / "environment.txt").write_text(
                "scenario=systemd-native-reconciliation\nsource_revision=%s\nkernel=%s\ncleanup=%s\nresult=%s\nhotplug_requested=%s\ndetail=%s\n" %
                (self.revision, os.uname().release, cleanup, status, hotplug_intent, detail.replace("\n", " ")))
        return {"PASS": 0, "BLOCKED": 77, "FAIL": 1}[status]


if __name__ == "__main__":
    require(sys.argv[1] == "systemd-native-reconciliation", "unsupported reconciliation scenario")
    gate = ReconciliationGate(Path(__file__).parent, sys.argv[2], sys.argv[3])
    def interrupted(_signal, _frame):
        raise RuntimeError("native reconciliation interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    sys.exit(gate.run())
