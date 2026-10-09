#!/usr/bin/env python3
"""Measure cpu-controller delegation and resman startup on one disposable guest.

The probe records measurements only. Every typed outcome is derived later by
validate_evidence.py from the records written here, so that no verdict depends
on the guest that produced the raw data.
"""

import argparse
import errno
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


ROOT_SUBTREE_CONTROL = "/sys/fs/cgroup/cgroup.subtree_control"
ROOT_CONTROLLERS = "/sys/fs/cgroup/cgroup.controllers"
USER_SLICE_CPU_MAX = "/sys/fs/cgroup/user.slice/cpu.max"
SYSTEM_CONTROL = "/run/systemd/system.control"
LEASES = "/var/lib/resman/systemd-property-leases.json"
DEFAULT_LOG = "/var/log/resman.log"
CONFIG = "/etc/resman/resman.conf"
REALTIME_CLASSES = frozenset({"FF", "RR"})
CAPABILITY_MARKER = "required_capability_unavailable"


class Blocked(RuntimeError):
    """Blocked means the guest cannot provide valid delegation evidence."""


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(*args, **keywords):
    """Run one command, never raising for a non-zero status."""
    expected = keywords.pop("expected", None)
    require(not keywords, "unsupported run() keyword")
    process = subprocess.run(list(args), universal_newlines=True,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    record = {"command": list(args), "status": process.returncode,
              "output": process.stdout.splitlines()}
    if expected is not None and process.returncode != expected:
        raise Blocked("command {0} exited {1}, expected {2}: {3}".format(
            " ".join(args), process.returncode, expected, process.stdout.strip()))
    return record


def text(path, fallback=None):
    path = Path(path)
    if not path.is_file():
        return fallback
    return path.read_text()


def write_subtree_control(value):
    """Write one controller token and report the exact kernel answer."""
    try:
        handle = os.open(ROOT_SUBTREE_CONTROL, os.O_WRONLY | os.O_CLOEXEC)
    except OSError as error:
        raise Blocked("cannot open the root subtree control: " + str(error))
    try:
        os.write(handle, value.encode())
    except OSError as error:
        return {"written": value, "accepted": False, "errno": error.errno,
                "errno_name": errno.errorcode.get(error.errno, "UNKNOWN"),
                "message": error.strerror,
                "subtree_control": text(ROOT_SUBTREE_CONTROL, "").split()}
    finally:
        os.close(handle)
    return {"written": value, "accepted": True, "errno": None, "errno_name": None,
            "message": None, "subtree_control": text(ROOT_SUBTREE_CONTROL, "").split()}


def parse_realtime_tasks(output):
    """Select the realtime threads from one ps inventory."""
    tasks = []
    for line in output.splitlines():
        words = line.split()
        if len(words) < 4 or not words[0].isdigit() or words[1] not in REALTIME_CLASSES:
            continue
        tasks.append({"tid": int(words[0]), "policy": words[1],
                      "priority": words[2], "cgroup": words[3]})
    return {"all": tasks, "outside_root": [task for task in tasks if task["cgroup"] != "0::/"]}


def realtime_tasks():
    """Inventory realtime threads and the cgroup each one lives in."""
    process = subprocess.run(["ps", "-eLo", "tid,cls,rtprio,cgroup", "--no-headers"],
                             universal_newlines=True, stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT)
    require(process.returncode == 0, "ps could not inventory realtime threads")
    return parse_realtime_tasks(process.stdout)


def delegation_attempt(label):
    """Enable cpu at the root, record the answer, and restore the initial set."""
    before = text(ROOT_SUBTREE_CONTROL, "").split()
    attempt = write_subtree_control("+cpu")
    reverted = None
    if attempt["accepted"] and "cpu" not in before:
        reverted = write_subtree_control("-cpu")
    return {"label": label, "subtree_control_before": before, "attempt": attempt,
            "revert": reverted, "subtree_control_after": text(ROOT_SUBTREE_CONTROL, "").split(),
            "realtime": realtime_tasks()}


def configured_log_path(body):
    """Resolve the log the installed configuration publishes to."""
    match = re.search(r"(?m)^LOG_FILE=(.+)$", body)
    if match is None:
        return DEFAULT_LOG
    value = match.group(1).strip().strip('"')
    return value if value.startswith("/") else DEFAULT_LOG


def log_path():
    return configured_log_path(text(CONFIG, ""))


def log_offset():
    path = Path(log_path())
    return path.stat().st_size if path.is_file() else 0


def log_tail(offset):
    path = Path(log_path())
    if not path.is_file():
        return []
    with path.open("rb") as handle:
        handle.seek(min(offset, path.stat().st_size))
        return handle.read().decode("utf-8", "replace").splitlines()


def daemon_state():
    return run("systemctl", "show", "resman", "-p", "ActiveState", "-p", "SubState",
               "-p", "Result", "-p", "ExecMainStatus", "-p", "ExecMainCode",
               "-p", "NRestarts", "-p", "InvocationID")


def probe_slices():
    units = run("systemctl", "list-units", "--all", "--no-legend", "--no-pager",
                "user-resmancapprobe*.slice")
    residue = sorted(str(path) for path in Path("/sys/fs/cgroup").glob("user-resmancapprobe*"))
    return {"units": units, "cgroups": residue}


def start_daemon(label):
    """Start the installed service once and record everything it published."""
    offset = log_offset()
    start = run("systemctl", "start", "resman")
    deadline = time.time() + 60
    state = daemon_state()
    while time.time() < deadline:
        state = daemon_state()
        active = re.search(r"(?m)^ActiveState=(\S+)$", "\n".join(state["output"]))
        if active is not None and active.group(1) not in ("activating", "deactivating"):
            break
        time.sleep(1)
    published = log_tail(offset)
    return {"label": label, "start": start, "state": state, "log": published,
            "capability_errors": [line for line in published if CAPABILITY_MARKER in line],
            "enforcement_mode": [line for line in published if "enforcement_mode=" in line],
            "user_slice_cpu_max": text(USER_SLICE_CPU_MAX),
            "subtree_control": text(ROOT_SUBTREE_CONTROL, "").split(),
            "probe_slices": probe_slices()}


def platform_record():
    release = os.uname().release
    config = text("/boot/config-" + release, "")
    rt_group_sched = None
    for line in config.splitlines():
        if line.startswith("CONFIG_RT_GROUP_SCHED"):
            rt_group_sched = line.strip()
    return {
        "os_release": text("/etc/os-release", "").splitlines(),
        "kernel": release,
        "kernel_config_present": bool(config),
        "rt_group_sched": rt_group_sched,
        "command_line": text("/proc/cmdline", "").strip(),
        "cgroup_filesystem": run("stat", "-fc", "%T", "/sys/fs/cgroup", expected=0),
        "controllers": text(ROOT_CONTROLLERS, "").split(),
        "subtree_control": text(ROOT_SUBTREE_CONTROL, "").split(),
        "systemd_version": run("systemctl", "--version", expected=0),
        "systemd_package": run("rpm", "-q", "systemd", expected=0),
        "kernel_package": run("rpm", "-q", "--whatprovides", "/boot/vmlinuz-" + release),
        "resman_package": run("rpm", "-q", "resman", expected=0),
        "sched_rt_runtime_us": text("/proc/sys/kernel/sched_rt_runtime_us", "").strip(),
    }


def establish_realtime_fixture(unit, priority, seconds):
    """Place one owned SCHED_FIFO task in a non-root cgroup, as vcs.service does."""
    start = run("systemd-run", "--unit=" + unit, "--slice=system.slice",
                "--description=resman cpu delegation fixture",
                "/usr/bin/chrt", "-f", str(priority), "/bin/sleep", str(seconds),
                expected=0)
    main_pid = None
    deadline = time.time() + 30
    while time.time() < deadline:
        shown = run("systemctl", "show", unit, "-p", "MainPID", "--value")
        candidate = "\n".join(shown["output"]).strip()
        if candidate.isdigit() and int(candidate) > 0:
            main_pid = int(candidate)
            break
        time.sleep(1)
    if main_pid is None:
        raise Blocked("the realtime fixture never published a main PID")
    policy = run("chrt", "-p", str(main_pid))
    cgroup = text("/proc/{0}/cgroup".format(main_pid), "").strip()
    return {"unit": unit, "requested_priority": priority, "start": start,
            "main_pid": main_pid, "policy": policy, "cgroup": cgroup,
            "state": run("systemctl", "show", unit, "-p", "ActiveState", "-p", "SubState"),
            "realtime": realtime_tasks()}


def residue_record(unit):
    return {
        "daemon": daemon_state(),
        "fixture": run("systemctl", "show", unit, "-p", "ActiveState", "-p", "SubState"),
        "subtree_control": text(ROOT_SUBTREE_CONTROL, "").split(),
        "leases_present": Path(LEASES).exists(),
        "system_control": sorted(str(path) for path in Path(SYSTEM_CONTROL).glob("*"))
        if Path(SYSTEM_CONTROL).is_dir() else [],
        "probe_slices": probe_slices(),
        "realtime": realtime_tasks(),
    }


def save(evidence, name, payload):
    path = evidence / (name + ".json")
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
    path.chmod(0o600)


def measure(options, evidence):
    initial_subtree = text(ROOT_SUBTREE_CONTROL, "").split()
    platform = platform_record()
    save(evidence, "platform", platform)
    require(platform["cgroup_filesystem"]["output"] == ["cgroup2fs"],
            "the guest does not use the unified cgroup v2 filesystem")
    require("cpu" in platform["controllers"],
            "the guest root cgroup does not even publish the cpu controller")
    if realtime_tasks()["outside_root"]:
        raise Blocked("the guest already has realtime tasks outside the root cgroup")

    save(evidence, "baseline-delegation", delegation_attempt("baseline"))
    fixture = establish_realtime_fixture(options.unit, options.priority, options.seconds)
    save(evidence, "realtime-fixture", fixture)
    save(evidence, "delegation-under-realtime", delegation_attempt("under-realtime"))
    save(evidence, "daemon-under-realtime", start_daemon("under-realtime"))

    run("systemctl", "stop", "resman")
    run("systemctl", "reset-failed", "resman")
    run("systemctl", "stop", options.unit, expected=0)
    run("systemctl", "reset-failed", options.unit)
    settled = realtime_tasks()
    if settled["outside_root"]:
        raise Blocked("the realtime fixture did not leave the non-root cgroup")
    control = {"realtime_after_removal": settled,
               "delegation": delegation_attempt("without-realtime")}
    control["daemon"] = start_daemon("without-realtime")
    save(evidence, "control-without-realtime", control)

    run("systemctl", "stop", "resman")
    run("systemctl", "reset-failed", "resman")
    if "cpu" in text(ROOT_SUBTREE_CONTROL, "").split() and "cpu" not in initial_subtree:
        write_subtree_control("-cpu")
    residue = residue_record(options.unit)
    residue["initial_subtree_control"] = initial_subtree
    save(evidence, "residue", residue)


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--source-revision", required=True)
    parser.add_argument("--unit", default="resman-cpudel-fixture")
    parser.add_argument("--priority", type=int, default=50)
    parser.add_argument("--seconds", type=int, default=900)
    options = parser.parse_args(argv)
    require(re.fullmatch(r"r[0-9]{14}-[0-9]+", options.run_id) is not None,
            "a canonical run id is required")
    require(re.fullmatch(r"[0-9a-f]{40}", options.source_revision) is not None,
            "a full source revision is required")
    require(re.fullmatch(r"[A-Za-z0-9@_.\-]+", options.unit) is not None,
            "an unambiguous fixture unit name is required")
    require(1 <= options.priority <= 99, "the fixture priority is outside SCHED_FIFO range")
    evidence = Path(options.evidence)
    evidence.mkdir(mode=0o700, parents=True, exist_ok=True)
    result = {"result": "MEASURED", "run_id": options.run_id,
              "source_revision": options.source_revision, "unit": options.unit,
              "priority": options.priority}
    try:
        measure(options, evidence)
    except Blocked as blocked:
        result.update({"result": "BLOCKED", "detail": str(blocked)})
        save(evidence, "result", result)
        print("BLOCKED: " + str(blocked), file=sys.stderr)
        return 77
    save(evidence, "result", result)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
