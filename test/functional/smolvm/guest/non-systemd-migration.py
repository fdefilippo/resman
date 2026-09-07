#!/usr/bin/env python3
"""Exercise legacy migration in a disposable guest's private PID/mount namespace.

This is actual daemon/cgroup execution, not a non-systemd host-boot claim. The
outer SmolVM remains systemd-booted; only this private process view lacks systemd.
"""
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
import urllib.request


CHECKS = frozenset({"non-systemd-namespace", "legacy-cpu-ingress",
                    "legacy-live-restoration", "legacy-cleanup"})


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


def assert_restored(pid, original_birth, original_cgroup):
    require(birth(pid) == original_birth, "workload identity changed during restoration")
    require(field("/proc/%d/cgroup" % pid) == original_cgroup,
            "live workload did not return to its exact origin")


def stop_owned_workload(workload, original_birth, root, identity):
    """Bound cleanup to the fixture's recorded process and private cgroup tree."""
    def confirm_tree():
        current = root.stat()
        require((current.st_dev, current.st_ino) == identity,
                "fixture cgroup identity changed before cleanup")

    confirm_tree()
    forced = False
    if workload.poll() is None:
        require(original_birth is not None and birth(workload.pid) == original_birth,
                "workload identity changed before cleanup")
        require(os.getpgid(workload.pid) == workload.pid, "workload process group changed")
        os.killpg(workload.pid, signal.SIGTERM)
        try:
            workload.wait(timeout=10)
        except subprocess.TimeoutExpired:
            forced = True
    # The supervisor may exit before its workers. Kill only the recorded,
    # run-private tree, never a potentially reused process group identifier.
    confirm_tree()
    if "populated 1" in field(root / "cgroup.events").splitlines():
        forced = True
        (root / "cgroup.kill").write_text("1")
    workload.wait(timeout=10)
    eventually(lambda: "populated 0" in field(root / "cgroup.events").splitlines(),
               "fixture workload tree remained populated after cleanup", 10)
    return {"forced_tree_termination": forced, "returncode": workload.returncode,
            "tree_identity": list(identity), "populated": False}


def save(directory, name, value):
    (directory / (name + ".json")).write_text(json.dumps(value, indent=2) + "\n")


def render_config(example, work, root):
    """Render the exact guest candidate for production validation and execution."""
    settings = {"CGROUP_ROOT": "/sys/fs/cgroup", "CGROUP_BASE": root.name + "/managed",
                "CREATED_CGROUPS_FILE": str(work / "cgroups.txt"),
                "CPU_POINTS_FILE": str(work / "cpu-points.map"), "USER_INCLUDE_LIST": "^resman-cpu$",
                "USER_EXCLUDE_LIST": "root", "CPU_THRESHOLD": "2", "CPU_RELEASE_THRESHOLD": "1",
                "CPU_THRESHOLD_DURATION": "0", "MIN_ACTIVE_TIME": "5", "PROCESS_MIN_AGE_SECONDS": "0",
                "POLLING_INTERVAL": "5", "METRICS_CACHE_TTL": "1", "METRICS_REFRESH_INTERVAL": "5",
                "IGNORE_SYSTEM_LOAD": "true", "PSI_EVENT_DRIVEN": "false", "BLACKOUT": "",
                "RAM_LIMIT_ENABLED": "false", "IO_LIMIT_ENABLED": "false", "MCP_ENABLED": "false",
                "METRICS_DB_ENABLED": "false", "LOG_FILE": str(work / "resman.log"), "USE_SYSLOG": "false",
                "ENABLE_PROMETHEUS": "true", "PROMETHEUS_METRICS_BIND_PORT": "19100"}
    body = example.strip() + "\n"
    for key, value in settings.items():
        body, count = re.subn(r"(?m)^" + key + "=.*$", key + "=" + value, body)
        require(count == 1, "missing or duplicated fixture key: " + key)
    return body


def write_guest_outcome(evidence, status, detail, cleanup):
    (evidence / "result").write_text(status + "\n")
    with (evidence / "environment.txt").open("a") as stream:
        stream.write("scenario=non-systemd-migration\nkernel=%s\nguest_cleanup=%s\nresult=%s\ndetail=%s\n" %
                     (os.uname().release, cleanup, status, detail.replace("\n", " ")))


def run(run_id, revision):
    require(re.fullmatch(r"[a-z0-9-]+", run_id), "unsafe run ID")
    require(re.fullmatch(r"[0-9a-f]{40}", revision), "a full source revision is required")
    evidence = Path("/mnt/resman-artifacts")
    checks = {}
    status, detail, cleanup = "FAIL", "legacy migration incomplete", "PASS"
    daemon = workload = None
    original_birth = root_identity = None
    root = Path("/sys/fs/cgroup") / ("resman-legacy-" + run_id)
    owned_root = False
    work = Path("/var/lib/resman-functional") / run_id / "legacy"
    work.mkdir(mode=0o700, parents=True)
    log = work / "resman.log"

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
        parent_namespace = os.environ.get("RESMAN_OUTER_PID_NAMESPACE", "")
        own_namespace = os.readlink("/proc/self/ns/pid")
        require(parent_namespace and parent_namespace != own_namespace, "PID namespace was not isolated")
        passed("non-systemd-namespace", {"outer_pid_namespace": parent_namespace,
                                        "private_pid_namespace": own_namespace,
                                        "pid_one": field("/proc/1/comm"),
                                        "systemd_marker_absent": True,
                                        "scope": "private PID/mount namespace inside disposable SmolVM; not a non-systemd boot"})
        if not shutil.which("stress") or not shutil.which("curl"):
            raise Blocked("stress and curl are required in the disposable guest")
        controllers = field("/sys/fs/cgroup/cgroup.controllers").split()
        if "cpu" not in controllers:
            raise Blocked("writable cgroup v2 CPU controller is required")
        if root.exists():
            raise Blocked("legacy run cgroup already exists")
        # All mutations occur in the disposable guest. The root write enables
        # only available controllers; subsequent writes stay below our own tree.
        available = [name for name in ("cpu", "memory", "io", "cpuset") if name in controllers]
        Path("/sys/fs/cgroup/cgroup.subtree_control").write_text(" ".join("+" + name for name in available))
        root.mkdir()
        owned_root = True
        root_stat = root.stat()
        root_identity = (root_stat.st_dev, root_stat.st_ino)
        (root / "cgroup.subtree_control").write_text(" ".join("+" + name for name in available))
        origin = root / "origin"
        origin.mkdir()
        account = pwd.getpwnam("resman-cpu")
        points = work / "cpu-points.map"
        points.write_text("[resman-cpu-points-map-v1]\n")
        points.chmod(0o600)
        config = work / "resman.conf"
        config.write_text(render_config(field("/opt/resman-functional/resman.conf.example"), work, root))
        config.chmod(0o600)
        stdout = (work / "daemon-stdout.log").open("w")
        daemon = subprocess.Popen(["/usr/bin/resman", "-config", str(config)], stdout=stdout, stderr=stdout)
        eventually(lambda: daemon.poll() is not None or log.exists(), "daemon startup did not respond", 20)
        if daemon.poll() is not None:
            raise Blocked("daemon foreground validation failed: " + field(work / "daemon-stdout.log"))

        def enter_origin():
            (origin / "cgroup.procs").write_text(str(os.getpid()))
            os.setgroups([])
            os.setgid(account.pw_gid)
            os.setuid(account.pw_uid)

        workload = subprocess.Popen(["/usr/bin/stress", "--cpu", "2", "--timeout", "150s"],
                                    preexec_fn=enter_origin, start_new_session=True,
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        pid = workload.pid
        original_birth = birth(pid)
        original_cgroup = field("/proc/%d/cgroup" % pid)
        require(original_cgroup == "0::/" + root.name + "/origin", "workload origin is not our disposable leaf")
        limited = root / "managed" / "limited"
        eventually(lambda: workload.poll() is None and
                   "/managed/limited/" in field("/proc/%d/cgroup" % pid), "daemon never acquired the legacy workload")
        require(field(limited / "cpu.max").split()[0] != "max", "legacy parent has no finite CPU quota")
        with urllib.request.urlopen("http://127.0.0.1:19100/metrics", timeout=5) as response:
            scrape = response.read().decode()
        (evidence / "legacy-active.prom").write_text(scrape)
        require(re.search(r'resman_enforcement_mode\{[^\n]*mode="migration_enabled"[^\n]*\} 1', scrape),
                "the daemon did not publish the explicit migration backend")
        applied = {"pid": pid, "start_time": original_birth, "origin": original_cgroup,
                   "limited_cgroup": field("/proc/%d/cgroup" % pid), "cpu_max": field(limited / "cpu.max")}
        passed("legacy-cpu-ingress", applied)
        daemon.send_signal(signal.SIGTERM)
        require(daemon.wait(timeout=75) == 0, "daemon shutdown failed")
        daemon = None
        assert_restored(pid, original_birth, original_cgroup)
        passed("legacy-live-restoration", {"before": applied,
                                            "restored_cgroup": field("/proc/%d/cgroup" % pid),
                                            "restored_start_time": birth(pid)})
        status, detail = "PASS", "real legacy ingress and exact live restoration in a private guest PID namespace"
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
                     stop_owned_workload(workload, original_birth, root, root_identity))
            if owned_root:
                for directory in sorted([root, *[p for p in root.rglob("*") if p.is_dir()]],
                                        key=lambda path: len(path.parts), reverse=True):
                    require(not field(directory / "cgroup.procs"), "live process survived in " + str(directory))
                    directory.rmdir()
                require(not root.exists(), "legacy cgroup tree survived cleanup")
                passed("legacy-cleanup", {"cgroup_tree_absent": True, "daemon_stopped": True, "workload_stopped": True})
        except Exception as err:
            status, cleanup = "FAIL", "FAIL"
            detail += "; cleanup: " + str(err)
        for name in CHECKS:
            checks.setdefault(name, "BLOCKED")
        shutil.copytree(work, evidence / "legacy-work", dirs_exist_ok=True)
        if status == "PASS" and (set(checks) != CHECKS or any(value != "PASS" for value in checks.values())):
            status, detail = "FAIL", "mandatory legacy check missing"
        save(evidence, "checks", checks)
        write_guest_outcome(evidence, status, detail, cleanup)
    return {"PASS": 0, "BLOCKED": 77, "FAIL": 1}[status]


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "--inside":
        sys.exit(run(sys.argv[2], sys.argv[3]))
    if len(sys.argv) != 3:
        raise SystemExit("usage: non-systemd-migration.py RUN_ID REVISION")
    run_id, revision = sys.argv[1:]
    if not shutil.which("unshare"):
        raise SystemExit(77)
    environment = os.environ.copy()
    environment["RESMAN_OUTER_PID_NAMESPACE"] = os.readlink("/proc/self/ns/pid")
    result = subprocess.run(["unshare", "--mount", "--pid", "--fork", "--mount-proc", "--kill-child",
                             sys.executable, __file__, "--inside", run_id, revision], env=environment)
    evidence = Path("/mnt/resman-artifacts")
    if not (evidence / "result").exists():
        # Namespace creation can fail before the child has any opportunity to
        # publish its own disposition. That is missing infrastructure, not PASS.
        save(evidence, "checks", dict.fromkeys(CHECKS, "BLOCKED"))
        write_guest_outcome(evidence, "BLOCKED", "private namespace runner did not start; exit=%d" % result.returncode, "PASS")
        raise SystemExit(77)
    sys.exit(result.returncode)
