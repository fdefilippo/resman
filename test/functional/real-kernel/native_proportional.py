#!/usr/bin/env python3
"""Simultaneous native/reference/stale-control measurements on a disposable host."""
import json
import math
import os
from pathlib import Path
import pwd
import re
import signal
import subprocess
import sys
import time
import traceback

from native_gate import Blocked, NativeGate, eventually, field, require, sha, wait_for_json
from native_placement import SCOPE, observe


REQUIRED_CHECKS = frozenset({
    "full-budget-rejection", "pam-sessions", "native-plan", "full-contention",
    "stale-control", "work-conserving-lending", "root-progress", "unchanged-membership",
    "unchanged-weights", "graceful-stop",
})
LEAVES = ("a", "b", "root", "best")
WEIGHTS = {"a": 5000, "b": 5000, "root": 10000, "best": 10000}
SAMPLED_NODES = ("parent",) + LEAVES
PROGRAMMED_PARENT_CAPACITY_USEC_PER_SECOND = 1200000
MAXIMUM_FRAME_SKEW_SECONDS = 0.2
PRIMARY_CONSERVATION_FRACTION = 0.01
# Five integer-microsecond counters at both endpoints can contribute at most
# one microsecond of truncation apiece to the parent-versus-leaf difference.
COUNTER_QUANTIZATION_ALLOWANCE_USEC = 2 * len(SAMPLED_NODES)


def checks_pass(checks):
    return set(checks) == REQUIRED_CHECKS and all(value == "PASS" for value in checks.values())


def _sampling_spans(frame):
    require(set(frame["sampling"]) == set(frame["nodes"]), "sampling metadata differs from measurement groups")
    require(0 <= frame["skew"] <= MAXIMUM_FRAME_SKEW_SECONDS, "counter-read skew exceeds 200 ms")
    frame_end = frame["time"] + frame["skew"]
    ordered = []
    spans = {}
    for group, nodes in frame["nodes"].items():
        require(set(nodes) == set(SAMPLED_NODES), "measurement nodes changed")
        sampling = frame["sampling"][group]
        require(sampling == {"capacity_usec_per_second": PROGRAMMED_PARENT_CAPACITY_USEC_PER_SECOND},
                "wrong or ambiguous programmed parent capacity")
        group_reads = []
        for node in SAMPLED_NODES:
            read = nodes[node].get("read")
            require(isinstance(read, dict) and set(read) == {"started", "finished"},
                    "missing per-node counter-read timing")
            started, finished = read["started"], read["finished"]
            require(isinstance(started, (int, float)) and isinstance(finished, (int, float)),
                    "counter-read timing is not numeric")
            require(frame["time"] <= started <= finished <= frame_end,
                    "counter-read timing lies outside its frame")
            group_reads.append((started, finished, group, node))
            ordered.append((started, finished, group, node))
        for previous, current in zip(group_reads, group_reads[1:]):
            require(previous[1] <= current[0], "parent and leaf counters were not read in declared order")
        spans[group] = group_reads[-1][1] - group_reads[0][0]
    ordered.sort()
    for previous, current in zip(ordered, ordered[1:]):
        require(previous[1] <= current[0], "counter reads overlap")
    observed_groups = [group for _, _, group, _ in ordered]
    for group in frame["nodes"]:
        positions = [index for index, observed in enumerate(observed_groups) if observed == group]
        require(positions == list(range(positions[0], positions[0] + len(SAMPLED_NODES))),
                "a parent/leaf read block was interleaved with another group")
    return spans


def _counter_deltas(before, after, minimum_seconds):
    require(after["time"] > before["time"], "measurement interval is not positive")
    require(after["time"] - before["time"] >= minimum_seconds, "measurement window too short")
    before_spans, after_spans = _sampling_spans(before), _sampling_spans(after)
    require(set(before["nodes"]) == set(after["nodes"]), "measurement groups changed")
    require(bool(before["nodes"]), "no measurement groups")
    result = {}
    for group in before["nodes"]:
        deltas = {}
        for node in SAMPLED_NODES:
            old, new = before["nodes"][group][node], after["nodes"][group][node]
            require(old["identity"] == new["identity"], "cgroup recreated during measurement")
            delta = new["stat"]["usage_usec"] - old["stat"]["usage_usec"]
            require(delta >= 0, "counter decreased during measurement")
            deltas[node] = delta
        require(deltas["parent"] > 0, "no measured parent bandwidth")
        result[group] = {"deltas": deltas, "before_span": before_spans[group],
                         "after_span": after_spans[group]}
    return result


def validate_sample_interval(before, after):
    """Validate a short sample using only its measured counter-read uncertainty."""
    report = {}
    for group, measurement in _counter_deltas(before, after, 0).items():
        deltas = measurement["deltas"]
        leaf_total = sum(deltas[node] for node in LEAVES)
        measured_error = leaf_total - deltas["parent"]
        timing_allowance = math.ceil(
            (measurement["before_span"] + measurement["after_span"])
            * PROGRAMMED_PARENT_CAPACITY_USEC_PER_SECOND
        )
        permitted_error = timing_allowance + COUNTER_QUANTIZATION_ALLOWANCE_USEC
        require(abs(measured_error) <= permitted_error,
                "parent/leaf sample difference exceeds measured read-timing uncertainty")
        report[group] = {
            "parent_usec": deltas["parent"], "leaf_usec": leaf_total,
            "error_usec": measured_error, "permitted_error_usec": permitted_error,
            "before_read_span_seconds": measurement["before_span"],
            "after_read_span_seconds": measurement["after_span"],
        }
    return report


def ratios(before, after, minimum_seconds=60):
    """Calculate full-window shares after fixed bilateral conservation validation."""
    result = {}
    for group, measurement in _counter_deltas(before, after, minimum_seconds).items():
        deltas = measurement["deltas"]
        leaf_total = sum(deltas[node] for node in LEAVES)
        measured_error = leaf_total - deltas["parent"]
        permitted_error = deltas["parent"] * PRIMARY_CONSERVATION_FRACTION
        require(abs(measured_error) <= permitted_error,
                "parent/leaf full-window difference exceeds one percent")
        result[group] = {node: 100 * deltas[node] / deltas["parent"] for node in LEAVES}
        result[group]["mapped"] = result[group]["a"] + result[group]["b"]
        result[group]["parent_usec"] = deltas["parent"]
        result[group]["conservation"] = {
            "error_usec": measured_error, "permitted_error_usec": permitted_error,
            "contract": "bilateral-fixed-one-percent",
        }
        result[group]["throttling"] = {}
        for key in ("nr_periods", "nr_throttled", "throttled_usec"):
            value = after["nodes"][group]["parent"]["stat"][key] - before["nodes"][group]["parent"]["stat"][key]
            require(value >= 0, "parent throttling counter decreased")
            result[group]["throttling"][key] = value
    return result


def compare_reference(measured):
    native, reference = measured["native"], measured["oracle"]
    require(abs(native["mapped"] - reference["mapped"]) <= 0.5,
            "mapped aggregate differs from same-window reference by more than 0.5 pp")
    for leaf in LEAVES:
        require(abs(native[leaf] - reference[leaf]) <= 1.0,
                leaf + " differs from same-window reference by more than 1.0 pp")


class ProportionalGate(NativeGate):
    def __init__(self, *args):
        super().__init__(*args)
        self.reference_units = []
        self.reference_slices = []
        self.reference_identities = {}
        self.paused = []
        self.paths = {}
        self.root_ssh = None
        self.root_output = None
        self.worker_births = None
        self.paused_leaves = set()

    def workload_args(self):
        return ("--six-pinned-workers",)

    def placement(self):
        identities = {group + "/" + leaf: identity for (group, leaf), identity in self.reference_identities.items()}
        if self.sessions:
            uids = (self.accounts[0].pw_uid, self.accounts[1].pw_uid, 0, self.accounts[2].pw_uid)
            identities.update({"native/" + leaf: self.sessions[uid] for leaf, uid in zip(LEAVES, uids)})
        expected = {group + "/" + leaf for group in self.paths for leaf in LEAVES}
        require(set(identities) == expected, "missing compared workload identity")
        layouts, self.worker_births = observe(identities, self.worker_births, paused=self.paused_leaves)
        return layouts

    def session_accounts(self):
        return self.accounts + [pwd.getpwuid(0)]

    def cron_accounts(self):
        # OL9 cron skips logind for root; SSH is the real root-session boundary.
        return self.accounts

    def preflight(self):
        super().preflight()
        if field("/sys/devices/system/cpu/online") != "0-3" or os.sched_getaffinity(0) != set(range(4)):
            raise Blocked("this simultaneous three-parent fixture requires four unrestricted online CPUs")
        probe = self.command("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
                             "-o", "ConnectTimeout=5", "root@localhost", "cat /proc/self/cgroup", check=False)
        if probe.returncode or not re.fullmatch(r"0::/user.slice/user-0.slice/session-[a-z0-9]+\.scope", probe.stdout.strip()):
            raise Blocked("root localhost SSH must already authenticate and enter a genuine logind session")
        self.save("proportional-environment", {
            "runner_sha256": sha(__file__), "window_seconds": 60, "maximum_skew_seconds": 0.2,
            "sample_conservation": "bilateral-measured-read-span",
            "sample_quantization_allowance_usec": COUNTER_QUANTIZATION_ALLOWANCE_USEC,
            "full_window_conservation": "bilateral-fixed-one-percent",
            "aggregate_tolerance_pp": 0.5, "leaf_tolerance_pp": 1.0,
            "minimum_stale_separation_pp": 2.0, "reserve": 700, "root": 100,
            "best_effort": 100, "mapped": [50, 50], "combined_parent_ceiling_points": 900,
        })

    def start_sessions(self):
        super().start_sessions()
        output = self.helper / "0"
        output.mkdir(mode=0o700)
        command = ["ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=5",
                   "root@localhost", "/usr/bin/python3", str(self.helper / "workload.py"), str(output), *self.workload_args()]
        self.save("root-ssh-command", command)
        self.root_output = (self.evidence / "root-ssh.log").open("w")
        self.root_ssh = subprocess.Popen(command, stdout=self.root_output, stderr=self.root_output)
        identity = wait_for_json(output / "identity.json", "root SSH workload did not publish complete identity", 20)
        match = re.fullmatch(r"0::/user.slice/user-0.slice/session-([a-z0-9]+)\.scope", identity["cgroup"])
        require(match is not None, "root workload is not in its own SSH/PAM session")
        identity["session"] = match[1]
        state = self.command("loginctl", "show-session", match[1], "-p", "User", "-p", "Scope").stdout
        require("User=0" in state and "Scope=session-" + match[1] + ".scope" in state, "logind does not own root workload")
        self.sessions[0] = identity
        self.passed("pam-sessions", self.sessions)

    def write_config(self, blackout=True, overcommit=False):
        super().write_config(blackout, overcommit)
        if not overcommit:
            self.map.write_text("[resman-cpu-points-map-v1]\n" +
                                "".join(a.pw_name + "=50\n" for a in self.accounts[:2]))
        body = self.config.read_text()
        for key, value in {"CPU_RESERVE_POINTS": "700", "RAM_LIMIT_ENABLED": "false", "IO_LIMIT_ENABLED": "false"}.items():
            body, count = re.subn(r"(?m)^" + key + "=.*$", key + "=" + value, body)
            require(count == 1, "missing fixture key " + key)
        self.config.write_text(body)

    def assert_applied(self):
        eventually(lambda: field(self.parent / "cpu.max") == "120000 100000", "native 300-point parent missing")
        mapping = dict(zip(LEAVES, (self.accounts[0].pw_uid, self.accounts[1].pw_uid, 0, self.accounts[2].pw_uid)))
        self.paths["native"] = {"parent": self.parent, **{key: self.slice(uid) for key, uid in mapping.items()}}
        for key, uid in mapping.items():
            eventually(lambda u=uid, k=key: field(self.slice(u) / "cpu.weight", "") == str(WEIGHTS[k]),
                       "native weight missing: " + key)
            require(field(self.slice(uid) / "cpu.max").split()[0] == "max", "native leaf has a quota")
        expected = {"user.slice"} | {"user-%d.slice" % uid for uid in mapping.values()}
        require({item["unit"] for item in json.loads(self.journal.read_text())["units"]} == expected,
                "native denominator differs from the four workload slices")
        self.passed("native-plan", {"quota": "120000 100000", "weights": WEIGHTS, "uids": mapping})

    def reference_workload_args(self):
        return self.workload_args()

    def start_references(self, groups=("oracle", "stale")):
        # Separate, run-owned parents provide an independent same-window oracle.
        # Their total ceiling plus the native ceiling is 90% of this four-CPU host.
        for group in groups:
            prefix = "nq6" + group + self.run_id.replace("-", "")
            parent = prefix + ".slice"
            root = Path("/sys/fs/cgroup") / parent
            require(not root.exists(), "reference cgroup already exists")
            properties = self.command("systemctl", "show", parent, "-p", "ActiveState", "-p", "FragmentPath", "-p", "DropInPaths").stdout
            require(set(properties.splitlines()) == {"ActiveState=inactive", "FragmentPath=", "DropInPaths="},
                    "reference slice has pre-existing configuration or activity")
            self.reference_slices.append(parent)
            self.command("systemctl", "start", parent)
            self.command("systemctl", "set-property", "--runtime", parent, "CPUQuota=120%", "CPUQuotaPeriodSec=100ms")
            self.paths[group] = {"parent": root}
            for leaf in LEAVES:
                unit = prefix + "-" + leaf + ".slice"
                service = prefix + "-" + leaf + ".service"
                self.reference_slices.append(unit)
                self.command("systemctl", "start", unit)
                weight = 5000 if group == "stale" and leaf == "best" else WEIGHTS[leaf]
                self.command("systemctl", "set-property", "--runtime", unit, "CPUWeight=" + str(weight))
                output = self.helper / (group + "-" + leaf)
                output.mkdir(mode=0o700)
                self.command("systemctl", "reset-failed", service, check=False)
                self.reference_units.append(service)
                self.command("systemd-run", "--quiet", "--unit=" + service, "--slice=" + unit,
                             "-p", "RuntimeMaxSec=1200", "-p", "TimeoutStopSec=10s",
                             "/usr/bin/python3", self.helper / "workload.py", output, *self.reference_workload_args())
                identity = wait_for_json(output / "identity.json", "reference workload did not publish complete identity")
                require(identity["cgroup"] == "0::/" + parent + "/" + unit + "/" + service,
                        "reference worker is outside its independent parent")
                self.reference_identities[(group, leaf)] = identity
                self.paths[group][leaf] = root / unit

    def snapshot(self):
        placement = self.placement()
        start = time.monotonic()
        nodes, sampling = {}, {}
        for group, paths in self.paths.items():
            nodes[group] = {}
            sampling[group] = {"capacity_usec_per_second": PROGRAMMED_PARENT_CAPACITY_USEC_PER_SECOND}
            require(tuple(paths) == SAMPLED_NODES, "counter paths are not in the adjacent sampling order")
            require(field(paths["parent"] / "cpu.max") == "120000 100000", "parent quota changed")
            for leaf, path in paths.items():
                read_started = time.monotonic()
                before = path.stat()
                stat = dict((key, int(value)) for key, value in
                            (line.split() for line in field(path / "cpu.stat").splitlines()))
                after = path.stat()
                require((before.st_dev, before.st_ino) == (after.st_dev, after.st_ino), "cgroup replaced during read")
                if leaf != "parent":
                    expected = 5000 if group == "stale" and leaf == "best" else WEIGHTS[leaf]
                    require(field(path / "cpu.weight") == str(expected), "programmed weight changed during measurement")
                read_finished = time.monotonic()
                nodes[group][leaf] = {"identity": [before.st_dev, before.st_ino], "stat": stat,
                                      "read": {"started": read_started, "finished": read_finished}}
        return {"time": start, "skew": time.monotonic() - start, "sampling": sampling,
                "nodes": nodes, "placement": placement}

    def measure(self, phase):
        self.record_topology(phase + "-topology-before")
        before = self.snapshot()
        frames = [before]
        for step in range(1, 13):
            # Sampling duration, not readiness synchronization.
            time.sleep(max(0, before["time"] + step * 5 - time.monotonic()))
            frames.append(self.snapshot())
        self.save(phase + "-raw", frames)
        self.record_topology(phase + "-topology-after")
        sample_validity = [validate_sample_interval(old, new) for old, new in zip(frames, frames[1:])]
        self.save(phase + "-sample-validity", sample_validity)
        measured = ratios(frames[0], frames[-1])
        duration = frames[-1]["time"] - frames[0]["time"]
        for group, values in measured.items():
            values["delivered_over_nominal"] = values["parent_usec"] / (duration * 1200000)
            require(values["delivered_over_nominal"] >= 0.8, group + " parent lacks usable nominal bandwidth")
            require(values["throttling"]["nr_throttled"] > 0, group + " finite parent never throttled")
        self.save(phase + "-measured", measured)
        compare_reference(measured)
        return measured

    def record_topology(self, name):
        result = {}
        for group, paths in self.paths.items():
            result[group] = {}
            for stat in paths["parent"].rglob("cpu.stat"):
                directory = stat.parent
                values = {key: field(directory / key, "unavailable") for key in
                          ("cpu.stat", "cpu.weight", "cpu.weight.nice", "cpu.idle", "cpuset.cpus.effective", "cgroup.procs")}
                processes = {}
                for pid in values["cgroup.procs"].split():
                    if not pid.isdigit():
                        continue
                    proc = Path("/proc") / pid
                    try:
                        fields = field(proc / "stat").rsplit(")", 1)[1].split()
                        status = field(proc / "status")
                        processes[pid] = {"state": fields[0], "ppid": fields[1], "priority": fields[15],
                                          "nice": fields[16], "start_time": fields[19],
                                          "affinity": re.findall(r"(?m)^Cpus_allowed_list:.*$", status)}
                    except FileNotFoundError:
                        processes[pid] = {"state": "disappeared"}
                values["processes"] = processes
                result[group][str(directory.relative_to(paths["parent"]))] = values
        self.save(name, result)

    def pause(self, identity):
        self.assert_membership()
        for pid in identity["children"]:
            stat = field("/proc/%d/stat" % pid).rsplit(")", 1)[1].split()
            require(int(stat[1]) == identity["pid"], "worker is no longer a child of the recorded owner")
            self.paused.append((pid, stat[19]))
            os.kill(pid, signal.SIGSTOP)
            eventually(lambda p=pid: field("/proc/%d/stat" % p).rsplit(")", 1)[1].split()[0] == "T",
                       "worker did not acknowledge pause")

    def resume(self):
        for pid, start_time in self.paused:
            stat = field("/proc/%d/stat" % pid, "")
            if stat and stat.rsplit(")", 1)[1].split()[19] == start_time:
                os.kill(pid, signal.SIGCONT)
        self.paused = []

    def root_response(self):
        output = self.helper / "0"
        latencies = []
        for attempt in range(3):
            token = self.run_id + str(time.monotonic_ns()) + str(attempt)
            start = time.monotonic()
            (output / "probe").write_text(token)
            eventually(lambda: field(output / "response", "") == token, "root session did not respond under contention", 3)
            latencies.append(time.monotonic() - start)
        require(max(latencies) <= 3, "root response exceeded the declared three-second bound")
        return latencies

    def cleanup(self):
        self.resume()
        # Never revert the native user slices here: their durable owner restores them.
        # These reference units were proved absent before this run created them.
        for service in reversed(self.reference_units):
            self.command("systemctl", "stop", service)
            self.command("systemctl", "reset-failed", service, check=False)
        for unit in reversed(self.reference_slices):
            self.command("systemctl", "revert", unit)
            self.command("systemctl", "stop", unit)
            require(not (Path("/run/systemd/system.control") / (unit + ".d")).exists(), "reference drop-in survived")
        try:
            super().cleanup()
        finally:
            if self.root_ssh is not None:
                self.root_ssh.terminate()
                self.root_ssh.wait(timeout=10)
            if self.root_output is not None:
                self.root_output.close()

    def run(self):
        status, detail, cleanup = "FAIL", "proportional gate incomplete", "PASS"
        try:
            self.preflight()
            self.validate()
            self.start_sessions()
            self.write_config(blackout=False)
            self.start_daemon()
            self.assert_applied()
            self.start_references()
            self.assert_membership()
            journal = self.journal.read_bytes()
            full_windows = []
            for window in range(6):
                full = self.measure("full-contention-%d" % window)
                separation = min(abs(full[group]["mapped"] - full["stale"]["mapped"]) for group in ("native", "oracle"))
                require(separation >= 2.0, "stale-plan control is not distinguishable")
                full_windows.append(full)
                print("daemon full contention: %d/6 windows" % (window + 1), flush=True)
            self.passed("full-contention", full_windows)
            root_full = self.root_response()
            separation = abs(full["oracle"]["mapped"] - full["stale"]["mapped"])
            require(separation >= 2.0, "stale-plan control is not distinguishable")
            self.passed("stale-control", {"separation_pp": separation})
            self.pause(self.sessions[self.accounts[0].pw_uid])
            for group in ("oracle", "stale"):
                self.pause(self.reference_identities[(group, "a")])
            self.paused_leaves = {group + "/a" for group in self.paths}
            idle = self.measure("mapped-idle")
            for leaf in ("b", "root", "best"):
                require(idle["native"][leaf] > full["native"][leaf] + 1.0, "idle capacity was not lent to " + leaf)
            require(idle["native"]["a"] < 0.1, "paused leaf still consumes material CPU")
            self.passed("work-conserving-lending", idle)
            require(full["native"]["root"] > 5 and idle["native"]["root"] > 5, "root did not progress under contention")
            self.passed("root-progress", {"full_pp": full["native"]["root"], "idle_pp": idle["native"]["root"],
                                          "full_response_seconds": root_full, "idle_response_seconds": self.root_response()})
            require(self.journal.read_bytes() == journal, "scheduler lending rewrote the native ownership plan")
            self.passed("unchanged-weights", {"journal_unchanged": True})
            self.assert_membership()
            self.passed("unchanged-membership", self.sessions)
            self.resume()
            self.stop_daemon()
            self.assert_released()
            self.assert_membership()
            self.passed("graceful-stop", {"journal_absent": True, "sessions_still_owned": True})
            status = "PASS" if checks_pass(self.checks) else "FAIL"
            detail = "same-window flat proportional and lending checks completed"
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
            self.save("measurement-scope", {"scope": SCOPE, "source_revision": self.revision,
                                           "run_id": self.run_id, "daemon_exercised": True,
                                           "full_contention_windows": 6, "window_seconds": 60,
                                           "cpu_assignment_multiset": [0, 0, 1, 1, 2, 3]})
            (self.evidence / "result").write_text(status + "\n")
            (self.evidence / "environment.txt").write_text(
                "scenario=systemd-native-proportional\nsource_revision=%s\nkernel=%s\ncleanup=%s\nresult=%s\ndetail=%s\n" %
                (self.revision, os.uname().release, cleanup, status, detail.replace("\n", " ")))
            print(status + ": " + detail, flush=True)
        return {"PASS": 0, "BLOCKED": 77, "FAIL": 1}[status]


if __name__ == "__main__":
    require(sys.argv[1] == "systemd-native-proportional", "unsupported proportional scenario")
    gate = ProportionalGate(Path(__file__).parent, sys.argv[2], sys.argv[3])
    def interrupted(_signal, _frame):
        raise RuntimeError("proportional campaign interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    sys.exit(gate.run())
