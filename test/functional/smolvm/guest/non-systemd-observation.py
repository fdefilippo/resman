#!/usr/bin/env python3
"""Prove fail-closed observation in a private non-systemd process view.

The outer SmolVM remains systemd-booted. This negative scenario removes the
systemd runtime marker and gives the daemon a non-systemd PID 1, then proves
that ResMan observes load without creating a hierarchy or moving a process.
"""
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
import urllib.request


CHECKS = frozenset({"non-systemd-namespace", "observation-only-mode",
                    "no-pid-relocation", "no-managed-hierarchy"})


class Blocked(RuntimeError):
    pass


def require(value, message):
    if not value:
        raise AssertionError(message)


def eventually(predicate, message, seconds=45):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.5)
    raise AssertionError(message)


def field(path):
    return Path(path).read_text().strip()


def birth(pid):
    return field("/proc/%d/stat" % pid).rsplit(")", 1)[1].split()[19]


def stop_owned_workload(workload, original_birth):
    """Stop only the fixture's recorded process lifetime and process group."""
    forced = False
    if workload.poll() is None:
        require(birth(workload.pid) == original_birth,
                "workload identity changed before cleanup")
        require(os.getpgid(workload.pid) == workload.pid,
                "workload process group changed before cleanup")
        os.killpg(workload.pid, signal.SIGTERM)
        try:
            workload.wait(timeout=10)
        except subprocess.TimeoutExpired:
            forced = True
            require(birth(workload.pid) == original_birth,
                    "workload identity changed before forced cleanup")
            os.killpg(workload.pid, signal.SIGKILL)
            workload.wait(timeout=10)
    return {"forced_termination": forced, "returncode": workload.returncode}


def save(directory, name, value):
    (directory / (name + ".json")).write_text(json.dumps(value, indent=2) + "\n")


def render_config(example, work):
    """Render the exact guest candidate for production validation and execution."""
    settings = {
        "CGROUP_ROOT": "/sys/fs/cgroup",
        "CPU_POINTS_FILE": str(work / "cpu-points.map"),
        "USER_INCLUDE_LIST": "^resman-cpu$", "USER_EXCLUDE_LIST": "root",
        "CPU_THRESHOLD": "2", "CPU_RELEASE_THRESHOLD": "1",
        "CPU_THRESHOLD_DURATION": "0", "MIN_ACTIVE_TIME": "5",
        "PROCESS_MIN_AGE_SECONDS": "0", "POLLING_INTERVAL": "5",
        "METRICS_CACHE_TTL": "1", "METRICS_REFRESH_INTERVAL": "5",
        "IGNORE_SYSTEM_LOAD": "true", "PSI_EVENT_DRIVEN": "false", "BLACKOUT": "",
        "RAM_LIMIT_ENABLED": "false", "IO_LIMIT_ENABLED": "false",
        "MCP_ENABLED": "false", "METRICS_DB_ENABLED": "false",
        "LOG_FILE": str(work / "resman.log"), "USE_SYSLOG": "false",
        "ENABLE_PROMETHEUS": "true", "PROMETHEUS_METRICS_BIND_PORT": "19100",
    }
    body = example.strip() + "\n"
    for key, value in settings.items():
        body, count = re.subn(r"(?m)^" + key + r"=.*$", key + "=" + value, body)
        require(count == 1, "missing or duplicated fixture key: " + key)
    return body


def write_guest_outcome(evidence, status, detail, cleanup):
    (evidence / "result").write_text(status + "\n")
    with (evidence / "environment.txt").open("a") as stream:
        stream.write("scenario=non-systemd-observation\nkernel=%s\nguest_cleanup=%s\nresult=%s\ndetail=%s\n" %
                     (os.uname().release, cleanup, status, detail.replace("\n", " ")))


def run(run_id, revision):
    require(re.fullmatch(r"[a-z0-9-]+", run_id), "unsafe run ID")
    require(re.fullmatch(r"[0-9a-f]{40}", revision), "a full source revision is required")
    evidence = Path("/mnt/resman-artifacts")
    checks = {}
    status, detail, cleanup = "FAIL", "non-systemd observation incomplete", "PASS"
    daemon = workload = None
    original_birth = None
    work = Path("/var/lib/resman-functional") / run_id / "non-systemd"
    work.mkdir(mode=0o700, parents=True)
    log = work / "resman.log"
    retired_root = Path("/sys/fs/cgroup/resman")
    retired_tracking = Path("/run/resman-cgroups.txt")

    def command(*args, check=True):
        with (evidence / "commands.jsonl").open("a") as stream:
            stream.write(json.dumps(list(args)) + "\n")
        return subprocess.run(args, check=check, text=True, stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, timeout=30)

    def passed(name, value):
        save(evidence, name, value)
        checks[name] = "PASS"

    try:
        if os.getpid() != 1 or os.geteuid() != 0:
            raise Blocked("private PID namespace must expose this root runner as PID 1")
        command("mount", "--make-rprivate", "/")
        command("mount", "-t", "tmpfs", "-o", "mode=0755", "resman-private-run", "/run")
        require(not Path("/run/systemd/system").exists(), "systemd marker remained visible")
        require(field("/proc/1/comm") != "systemd", "private namespace still sees systemd PID 1")
        outer_namespace = os.environ.get("RESMAN_OUTER_PID_NAMESPACE", "")
        own_namespace = os.readlink("/proc/self/ns/pid")
        require(outer_namespace and outer_namespace != own_namespace,
                "PID namespace was not isolated")
        passed("non-systemd-namespace", {
            "outer_pid_namespace": outer_namespace,
            "private_pid_namespace": own_namespace,
            "pid_one": field("/proc/1/comm"),
            "systemd_marker_absent": True,
            "scope": "private PID/mount namespace inside disposable SmolVM; not a non-systemd boot",
        })
        if not shutil.which("stress"):
            raise Blocked("stress is required in the disposable guest")
        require(not retired_root.exists(), "retired ResMan cgroup root already exists")
        require(not retired_tracking.exists(), "retired ResMan tracking file already exists")

        account = pwd.getpwnam("resman-cpu")
        points = work / "cpu-points.map"
        points.write_text("[resman-cpu-points-map-v1]\n")
        points.chmod(0o600)
        config = work / "resman.conf"
        config.write_text(render_config(field("/opt/resman-functional/resman.conf.example"), work))
        config.chmod(0o600)

        def run_as_account():
            os.setgroups([])
            os.setgid(account.pw_gid)
            os.setuid(account.pw_uid)

        workload = subprocess.Popen(
            ["/usr/bin/stress", "--cpu", "2", "--timeout", "150s"],
            preexec_fn=run_as_account, start_new_session=True,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        original_birth = birth(workload.pid)
        original_cgroup = field("/proc/%d/cgroup" % workload.pid)

        stdout = (work / "daemon-stdout.log").open("w")
        daemon = subprocess.Popen(["/usr/bin/resman", "-config", str(config)],
                                  stdout=stdout, stderr=stdout)
        eventually(lambda: daemon.poll() is not None or log.exists(),
                   "daemon startup did not respond", 20)
        if daemon.poll() is not None:
            raise Blocked("daemon foreground validation failed: " + field(work / "daemon-stdout.log"))
        eventually(lambda: "requested_policy_intent=activate" in field(log),
                   "sustained load never reached activation intent", 45)

        with urllib.request.urlopen("http://127.0.0.1:19100/metrics", timeout=5) as response:
            scrape = response.read().decode()
        (evidence / "observation.prom").write_text(scrape)
        require(re.search(r'resman_enforcement_mode\{[^\n]*mode="observation_only"[^\n]*\} 1', scrape),
                "the daemon did not publish observation_only")
        require(re.search(r"^resman_any_limits_active(?:\{[^\n]*\})? 0$", scrape, re.MULTILINE),
                "observation-only mode acknowledged active limits")
        passed("observation-only-mode", {"mode": "observation_only", "limits_active": False})

        time.sleep(6)
        require(workload.poll() is None, "observed workload exited before the assertion")
        require(birth(workload.pid) == original_birth, "observed workload identity changed")
        require(field("/proc/%d/cgroup" % workload.pid) == original_cgroup,
                "ResMan moved the observed workload")
        passed("no-pid-relocation", {"pid": workload.pid, "start_time": original_birth,
                                      "cgroup_before": original_cgroup,
                                      "cgroup_after": field("/proc/%d/cgroup" % workload.pid)})
        require(not retired_root.exists(), "ResMan recreated its retired cgroup hierarchy")
        require(not retired_tracking.exists(), "ResMan recreated its retired tracking file")
        passed("no-managed-hierarchy", {"cgroup_root_absent": True,
                                         "tracking_file_absent": True})

        daemon.send_signal(signal.SIGTERM)
        require(daemon.wait(timeout=75) == 0, "daemon shutdown failed")
        daemon = None
        require(workload.poll() is None, "daemon shutdown terminated the observed workload")
        require(field("/proc/%d/cgroup" % workload.pid) == original_cgroup,
                "daemon shutdown changed workload membership")
        status, detail = "PASS", "non-systemd authority remained observation-only without PID relocation or managed state"
    except Blocked as err:
        status, detail = "BLOCKED", str(err)
    except Exception as err:
        detail = str(err)
        traceback.print_exc()
    finally:
        try:
            if daemon is not None and daemon.poll() is None:
                daemon.terminate()
                require(daemon.wait(timeout=75) == 0, "cleanup daemon shutdown failed")
            if workload is not None:
                save(evidence, "workload-termination",
                     stop_owned_workload(workload, original_birth))
            require(not retired_root.exists(), "retired cgroup root survived cleanup")
            require(not retired_tracking.exists(), "retired tracking file survived cleanup")
        except Exception as err:
            status, cleanup = "FAIL", "FAIL"
            detail += "; cleanup: " + str(err)
        for name in CHECKS:
            checks.setdefault(name, "BLOCKED")
        shutil.copytree(work, evidence / "non-systemd-work", dirs_exist_ok=True)
        if status == "PASS" and (set(checks) != CHECKS or any(value != "PASS" for value in checks.values())):
            status, detail = "FAIL", "mandatory non-systemd check missing"
        save(evidence, "checks", checks)
        write_guest_outcome(evidence, status, detail, cleanup)
    return {"PASS": 0, "BLOCKED": 77, "FAIL": 1}[status]


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "--inside":
        sys.exit(run(sys.argv[2], sys.argv[3]))
    if len(sys.argv) != 3:
        raise SystemExit("usage: non-systemd-observation.py RUN_ID REVISION")
    run_id, revision = sys.argv[1:]
    if not shutil.which("unshare"):
        raise SystemExit(77)
    environment = os.environ.copy()
    environment["RESMAN_OUTER_PID_NAMESPACE"] = os.readlink("/proc/self/ns/pid")
    result = subprocess.run([
        "unshare", "--mount", "--pid", "--fork", "--mount-proc", "--kill-child",
        sys.executable, __file__, "--inside", run_id, revision,
    ], env=environment)
    evidence = Path("/mnt/resman-artifacts")
    if not (evidence / "result").exists():
        save(evidence, "checks", dict.fromkeys(CHECKS, "BLOCKED"))
        write_guest_outcome(evidence, "BLOCKED",
                            "private namespace runner did not start; exit=%d" % result.returncode,
                            "PASS")
        raise SystemExit(77)
    sys.exit(result.returncode)
