#!/usr/bin/env python3
"""Run packaged-daemon weighted-I/O contention qualification in one QEMU guest."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import signal
import sqlite3
import subprocess
import time
import urllib.request


PROVENANCE = "resman-nq6.40.5-ol9-rhck-20260915"
EXPECTED_KERNEL = "5.14.0-687.46.1.el9_8.x86_64"
EXPECTED_MANAGER = "252-67.0.1.el9_8.2"
EXPECTED_PACKAGE = "resman-1.38.0-8.el9.x86_64"
COMPLETED_STAGES = ("preflight", "workloads", "io_cost", "bfq", "authority", "public",
                    "composition", "lifecycle", "release")
PROFILES = {
    "smoke": {"interval_count": 1, "interval_seconds": 1,
              "scope": "packaged-daemon-controlled-contention-smoke"},
    "qualification": {"interval_count": 3, "interval_seconds": 10,
                      "scope": "packaged-daemon-controlled-contention"},
}


class Blocked(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha256(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def selected_scheduler(text):
    selected = re.findall(r"\[([^\]\s]+)\]", text)
    require(len(selected) == 1, "scheduler has no unique selected value")
    return selected[0]


def device_row(text, device):
    rows = [line for line in (text or "").splitlines()
            if line.split() and line.split()[0] == device]
    require(len(rows) <= 1, "device appears more than once in a controller file")
    return rows[0] if rows else None


def io_stat_read_bytes(text, device):
    row = device_row(text, device)
    require(row is not None, "io.stat lacks selected device " + device)
    fields = dict(token.split("=", 1) for token in row.split()[1:] if "=" in token)
    return int(fields.get("rbytes", "0"))


def parse_metric(text, name, labels=None):
    labels = labels or {}
    for line in text.splitlines():
        if line.startswith("#"):
            continue
        match = re.fullmatch(r"([^\s{]+)(?:\{([^}]*)\})?\s+([^\s]+)", line)
        if not match or match.group(1) != name:
            continue
        found = {}
        for key, value in re.findall(r'(\w+)="((?:[^"\\]|\\.)*)"', match.group(2) or ""):
            found[key] = bytes(value, "utf-8").decode("unicode_escape")
        if all(found.get(key) == value for key, value in labels.items()):
            return float(match.group(3))
    raise KeyError("metric not found: " + name + " " + repr(labels))


def active_metric_label(text, name, label):
    """Return the label value of the unique active sample in a gauge vector."""
    active = []
    for line in text.splitlines():
        if line.startswith("#"):
            continue
        match = re.fullmatch(r"([^\s{]+)(?:\{([^}]*)\})?\s+([^\s]+)", line)
        if not match or match.group(1) != name or float(match.group(3)) != 1:
            continue
        labels = dict(re.findall(r'(\w+)="((?:[^"\\]|\\.)*)"', match.group(2) or ""))
        if label in labels:
            active.append(labels[label])
    require(len(active) == 1, "metric has no unique active label: " + name)
    return active[0]


def device_evidence(devices):
    """Project internal device state into the retained JSON contract."""
    result = []
    for device in devices:
        projected = {key: value for key, value in device.items() if key != "block"}
        projected["sysfs_path"] = str(device["block"])
        result.append(projected)
    return result


def eventually(check, message, timeout=75, interval=1):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            value = check()
            if value:
                return value
        except Exception as error:
            last = error
        time.sleep(interval)
    suffix = "" if last is None else ": " + str(last)
    raise RuntimeError(message + suffix)


class Campaign:
    def __init__(self, bundle, evidence, run_id, revision, source_tree,
                 qualification_revision, qualification_tree, package, devices,
                 profile="qualification"):
        self.bundle = bundle
        self.evidence = evidence
        self.run_id = run_id
        self.revision = revision
        self.source_tree = source_tree
        self.qualification_revision = qualification_revision
        self.qualification_tree = qualification_tree
        self.package = package
        self.device_paths = devices
        require(profile in PROFILES, "unknown campaign profile")
        self.profile = profile
        self.profile_config = PROFILES[profile]
        self.config = Path("/etc/resman/resman.conf")
        self.weight_map = Path("/etc/resman/io-weights.map")
        self.database = Path("/var/lib/resman/metrics.db")
        self.journal = Path("/var/lib/resman/systemd-property-leases.json")
        self.log = Path("/var/log/resman.log")
        self.users = [pwd.getpwnam("resman-t1"), pwd.getpwnam("resman-t2")]
        self.slices = ["user-%d.slice" % user.pw_uid for user in self.users]
        self.devices = []
        self.raw_files = {}
        self.workloads = []
        self.original_config = None
        self.original_map = None
        self.result = "FAIL"
        self.summary = {}
        self.completed_stages = []

    def command(self, *args, check=True, timeout=90, input_text=None):
        result = subprocess.run(args, input=input_text, text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, timeout=timeout)
        if check and result.returncode != 0:
            raise RuntimeError("command failed (%s): %s" % (" ".join(args), result.stdout))
        return result

    def read(self, path):
        return Path(path).read_text()

    def record(self, name, value):
        path = self.evidence / name
        path.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n")
        self.raw_files[name] = sha256(path)
        return value

    def checkpoint(self, stage):
        require(stage in COMPLETED_STAGES, "unknown campaign checkpoint")
        require(stage not in self.completed_stages, "campaign checkpoint repeated")
        self.completed_stages.append(stage)
        self.record("checkpoint-%02d-%s.json" % (len(self.completed_stages), stage), {
            "profile": self.profile, "stage": stage, "status": "PASS",
            "completed_stages": list(self.completed_stages), "time_ns": time.time_ns(),
        })

    def preflight(self):
        if os.geteuid() != 0 or not Path("/run/systemd/system").is_dir():
            raise Blocked("root in a disposable systemd QEMU guest is required")
        require(os.uname().release == EXPECTED_KERNEL, "guest did not boot exact qualified RHCK")
        for tool in ("systemctl", "busctl", "rpm", "rpm2cpio", "cpio", "curl", "dd"):
            require(self.command("sh", "-c", "command -v " + tool, check=False).returncode == 0,
                    "missing guest tool: " + tool)
        require(self.package.is_file() and not self.package.is_symlink(), "exact RPM is unavailable")
        package_identity = self.command("rpm", "-qp", "--qf", "%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}",
                                        str(self.package)).stdout
        require(package_identity == EXPECTED_PACKAGE, "unexpected package identity: " + package_identity)
        installed_identity = self.command("rpm", "-q", "--qf", "%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}",
                                          "resman").stdout
        require(installed_identity == package_identity, "installed package differs")
        archive = subprocess.Popen(["rpm2cpio", str(self.package)], stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE)
        extracted = subprocess.run(["cpio", "--extract", "--to-stdout", "./usr/bin/resman"],
                                   stdin=archive.stdout, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, timeout=30)
        if archive.stdout is not None:
            archive.stdout.close()
        archive_stderr = archive.stderr.read().decode(errors="replace") if archive.stderr is not None else ""
        archive_status = archive.wait(timeout=30)
        require(archive_status == 0 and extracted.returncode == 0 and extracted.stdout,
                "package payload extraction failed: " + archive_stderr + extracted.stderr.decode(errors="replace"))
        verify = self.command("rpm", "-V", "resman", check=False).stdout.strip()
        require(not verify, "installed package files fail rpm verification: " + verify)
        package_digest = sha256(self.package)
        binary_digest = sha256("/usr/bin/resman")
        payload_digest = hashlib.sha256(extracted.stdout).hexdigest()
        require(binary_digest == payload_digest, "installed binary differs from supplied RPM payload")
        self.package_info = {"identity": package_identity, "sha256": package_digest,
                             "installed_binary_sha256": binary_digest,
                             "payload_binary_sha256": payload_digest,
                             "installed_binary_matches_payload": True,
                             "verification": "rpm -V returned no differences"}
        os_release = {}
        for line in self.read("/etc/os-release").splitlines():
            if "=" in line:
                key, value = line.split("=", 1)
                os_release[key] = value.strip().strip('"')
        manager_raw = self.command("busctl", "get-property", "org.freedesktop.systemd1",
                                   "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
                                   "Version").stdout.strip()
        manager = shlex.split(manager_raw)[1]
        require(manager == EXPECTED_MANAGER,
                "running systemd Manager differs from retained representative: " + repr(manager))
        kernel_owner = self.command("rpm", "-q", "--whatprovides",
                                    "/boot/vmlinuz-" + os.uname().release).stdout.strip()
        require(kernel_owner.startswith("kernel-core-"), "running kernel is not RHCK")
        self.platform = {"id": os_release.get("ID"), "version_id": os_release.get("VERSION_ID"),
                         "manager_version": manager, "kernel_release": os.uname().release,
                         "kernel_package_owner": kernel_owner}
        require(self.platform["id"] == "ol" and self.platform["version_id"] == "9.8",
                "guest is not Oracle Linux 9.8")
        require(len(self.device_paths) == 2, "two disposable devices are required")
        for path in self.device_paths:
            stat = path.stat()
            require(path.is_block_device(), "not a block device: " + str(path))
            major_minor = "%d:%d" % (os.major(stat.st_rdev), os.minor(stat.st_rdev))
            name = path.resolve().name
            block = Path("/sys/block") / name
            require(block.is_dir() and self.read(block / "dev").strip() == major_minor,
                    "device identity is not a whole request queue")
            self.devices.append({"path": "/dev/" + name, "supplied_path": str(path),
                                 "name": name, "major_minor": major_minor,
                                 "block": block, "initial_scheduler": self.read(block / "queue/scheduler").strip(),
                                 "initial_mode": stat.st_mode & 0o777,
                                 "initial_qos": self.read("/sys/fs/cgroup/io.cost.qos")
                                                if Path("/sys/fs/cgroup/io.cost.qos").exists() else None,
                                 "owned_disposable": True, "identity_stable": True})
            path.chmod(0o666)
        self.original_config = self.config.read_bytes()
        self.original_map = self.weight_map.read_bytes() if self.weight_map.exists() else None
        self.command("systemctl", "disable", "--now", "resman", check=False)
        require(not self.journal.exists(), "pre-existing ResMan ownership journal")
        self.install_workload()

    def install_workload(self):
        target = Path("/usr/local/libexec/resman-iow-effect-workload.py")
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("""#!/usr/bin/env python3
import signal,subprocess,sys,time
stop=False
def done(*_):
 global stop; stop=True
signal.signal(signal.SIGTERM,done); signal.signal(signal.SIGINT,done)
while not stop:
 p=subprocess.Popen(['/usr/bin/dd','if='+sys.argv[1],'of=/dev/null','bs=128K','iflag=direct','status=none'])
 while p.poll() is None and not stop: time.sleep(.1)
 if stop and p.poll() is None: p.terminate(); p.wait()
""")
        target.chmod(0o755)
        self.workload_path = target

    def set_config(self, updates, reload_daemon=True):
        lines = self.config.read_text().splitlines()
        values = dict(updates)
        output = []
        seen = set()
        for line in lines:
            match = re.match(r"^([A-Z][A-Z0-9_]*)=", line)
            if match and match.group(1) in values:
                key = match.group(1)
                output.append(key + "=" + str(values[key]))
                seen.add(key)
            else:
                output.append(line)
        require(seen == set(values), "installed config lacks keys: " + repr(set(values) - seen))
        temporary = self.config.with_suffix(".effect.tmp")
        temporary.write_text("\n".join(output) + "\n")
        temporary.chmod(0o600)
        os.replace(temporary, self.config)
        if reload_daemon and self.command("systemctl", "is-active", "--quiet", "resman", check=False).returncode == 0:
            self.command("systemctl", "reload", "resman")

    def set_map(self, first, second):
        temporary = self.weight_map.with_suffix(".effect.tmp")
        temporary.write_text("[resman-io-weights-map-v1]\nresman-t1=%d\nresman-t2=%d\n" % (first, second))
        temporary.chmod(0o600)
        os.replace(temporary, self.weight_map)
        if self.command("systemctl", "is-active", "--quiet", "resman", check=False).returncode == 0:
            self.command("systemctl", "reload", "resman")

    def start_workloads(self):
        for index, user in enumerate(self.users):
            unit = "resman-iow-effect-%d.service" % (index + 1)
            self.command("systemd-run", "--unit", unit, "--uid", user.pw_name,
                         "--slice", self.slices[index], "--property=Type=exec",
                         str(self.workload_path), str(self.device_paths[0]))
            self.workloads.append(unit)
        eventually(lambda: all((Path("/sys/fs/cgroup/user.slice") / unit).is_dir()
                               for unit in self.slices), "user slices were not materialized")

    def metrics(self):
        with urllib.request.urlopen("http://127.0.0.1:1974/metrics", timeout=5) as response:
            return response.read().decode()

    def wait_programmed(self):
        return eventually(lambda: self.metrics() if
                          parse_metric(self.metrics(), "resman_io_device_weight_functionally_accepted") == 1 and
                          parse_metric(self.metrics(), "resman_io_device_weight_programmed") == 1 and
                          parse_metric(self.metrics(), "resman_io_device_weight_read_back") == 1 else None,
                          "weighted-I/O plan was not functionally accepted and programmed", 90)

    def wait_released(self):
        return eventually(lambda: self.metrics() if
                          parse_metric(self.metrics(), "resman_io_device_weight_programmed") == 0 else None,
                          "weighted-I/O plan was not released", 75)

    def kernel_weight(self, uid, mechanism, device):
        name = "io.bfq.weight" if mechanism == "bfq" else "io.weight"
        text = self.read(Path("/sys/fs/cgroup/user.slice") / ("user-%d.slice" % uid) / name)
        row = device_row(text, device)
        require(row is not None and len(row.split()) == 2, "kernel weight entry is absent")
        return int(row.split()[1])

    def systemd_weight_text(self, unit):
        response = shlex.split(self.command("busctl", "call", "org.freedesktop.systemd1",
                                            "/org/freedesktop/systemd1",
                                            "org.freedesktop.systemd1.Manager", "GetUnit", "s",
                                            unit).stdout.strip())
        require(len(response) == 2 and response[0] == "o", "GetUnit returned invalid identity")
        return self.command("busctl", "get-property", "org.freedesktop.systemd1", response[1],
                            "org.freedesktop.systemd1.Slice", "IODeviceWeight").stdout.strip()

    def wait_weights(self, mechanism, expected):
        device = self.devices[0]["major_minor"]
        return eventually(lambda: all(self.kernel_weight(user.pw_uid, mechanism, device) == value
                                      for user, value in zip(self.users, expected)),
                          "kernel did not expose expected sibling weights", 75)

    def snapshot_io(self):
        device = self.devices[0]["major_minor"]
        return {"time_ns": time.monotonic_ns(), "device": device,
                "slices": [{"unit": unit,
                            "invocation_id": self.command("systemctl", "show", unit,
                                                          "--property=InvocationID", "--value").stdout.strip(),
                            "cgroup_inode": (Path("/sys/fs/cgroup/user.slice") / unit).stat().st_ino,
                            "io_stat": self.read(Path("/sys/fs/cgroup/user.slice") / unit / "io.stat")}
                           for unit in self.slices]}

    def run_phase(self, mechanism, name, weights):
        self.set_map(*weights)
        expected = list(weights)
        self.wait_programmed()
        self.wait_weights(mechanism, expected)
        intervals = []
        raw = []
        device = self.devices[0]["major_minor"]
        for index in range(self.profile_config["interval_count"]):
            before = self.snapshot_io()
            time.sleep(self.profile_config["interval_seconds"])
            after = self.snapshot_io()
            delta = [io_stat_read_bytes(after["slices"][item]["io_stat"], device) -
                     io_stat_read_bytes(before["slices"][item]["io_stat"], device) for item in range(2)]
            if self.profile == "qualification" or name == "equal":
                require(all(value > 0 for value in delta),
                        "delivery interval contains no I/O for one sibling")
            else:
                require(all(value >= 0 for value in delta) and sum(delta) > 0,
                        "smoke delivery interval contains no I/O")
            intervals.append({"device": device, "duration_ns": after["time_ns"] - before["time_ns"],
                              "read_bytes_delta": delta})
            raw.append({"index": index, "before": before, "after": after, "delta": delta})
        self.record("%s-%s-intervals.json" % (mechanism, name), raw)
        return {"public_weights": list(weights), "intervals": intervals}

    def aggregate(self, intervals):
        values = [sum(item["read_bytes_delta"][index] for item in intervals) for index in range(2)]
        total = sum(values)
        return {"bytes": values, "shares": [values[0] / total, values[1] / total],
                "duration_ns": sum(item["duration_ns"] for item in intervals)}

    def configure_device_mechanism(self, device, mechanism):
        scheduler_path = device["block"] / "queue/scheduler"
        values = self.read(scheduler_path).replace("[", "").replace("]", "").split()
        qos_path = Path("/sys/fs/cgroup/io.cost.qos")
        if mechanism == "bfq":
            require("bfq" in values, "BFQ is unavailable on the owned device")
            if qos_path.exists():
                qos_path.write_text(device["major_minor"] + " enable=0\n")
            scheduler_path.write_text("bfq\n")
            low_latency = device["block"] / "queue/iosched/low_latency"
            if low_latency.exists():
                low_latency.write_text("0\n")
            require(selected_scheduler(self.read(scheduler_path)) == "bfq", "BFQ selection failed")
            return {"selected_scheduler": "bfq", "io_cost_enabled": False,
                    "owned_disposable_device": True,
                    "daemon_mutated_scheduler_or_iocost": False}
        alternatives = [value for value in ("mq-deadline", "none", "kyber") if value in values]
        require(alternatives, "no non-BFQ scheduler is available")
        scheduler_path.write_text(alternatives[0] + "\n")
        require(qos_path.exists() and Path("/sys/fs/cgroup/io.cost.model").exists(),
                "io.cost interfaces are unavailable")
        qos_path.write_text(device["major_minor"] + " enable=1 ctrl=auto\n")
        row = device_row(self.read(qos_path), device["major_minor"])
        require(row is not None and "enable=1" in row.split(), "io.cost activation failed")
        return {"selected_scheduler": alternatives[0], "io_cost_enabled": True,
                "owned_disposable_device": True,
                "daemon_mutated_scheduler_or_iocost": False}

    def configure_mechanism(self, mechanism):
        return self.configure_device_mechanism(self.devices[0], mechanism)

    def common_config(self, selector):
        self.set_config({"IO_WEIGHT_DEVICES": selector, "IO_ROOT_WEIGHT": 100,
                         "IO_DEFAULT_WEIGHT": 100, "IO_USER_WEIGHT_FILE": str(self.weight_map),
                         "IO_LIMIT_ENABLED": "false", "IO_DEVICE_FILTER": self.devices[0]["major_minor"],
                         "BLACKOUT": "", "ENABLE_PROMETHEUS": "true",
                         "PROMETHEUS_METRICS_BIND_HOST": "127.0.0.1",
                         "PROMETHEUS_METRICS_BIND_PORT": 1974, "PROMETHEUS_TLS_ENABLED": "false",
                         "PROMETHEUS_AUTH_TYPE": "none", "METRICS_DB_ENABLED": "true",
                         "METRICS_DB_PATH": str(self.database), "METRICS_DB_WRITE_INTERVAL": 5,
                         "POLLING_INTERVAL": 5, "PSI_EVENT_DRIVEN": "false",
                         "USE_SYSLOG": "false", "LOG_FILE": str(self.log),
                         "LOG_LEVEL": "DEBUG"}, reload_daemon=False)

    def start_resman(self):
        self.command("systemctl", "reset-failed", "resman", check=False)
        self.command("systemctl", "start", "resman")
        eventually(lambda: self.command("systemctl", "is-active", "--quiet", "resman",
                                        check=False).returncode == 0,
                   "packaged daemon did not become active", 30)

    def stop_resman(self):
        self.command("systemctl", "stop", "resman", check=False, timeout=90)

    def signal_workloads(self, signal_name):
        for unit in self.workloads:
            self.command("systemctl", "kill", "--kill-whom=all", "--signal=" + signal_name,
                         unit)

    def mechanism_campaign(self, mechanism):
        self.stop_resman()
        self.signal_workloads("STOP")
        try:
            self.set_map(100, 100)
            setup = self.configure_mechanism(mechanism)
            self.common_config(self.devices[0]["major_minor"])
            scheduler_before = self.read(self.devices[0]["block"] / "queue/scheduler")
            qos_before = self.read("/sys/fs/cgroup/io.cost.qos") if Path("/sys/fs/cgroup/io.cost.qos").exists() else None
            self.start_resman()
            self.wait_programmed()
        finally:
            self.signal_workloads("CONT")
        require(self.read(self.devices[0]["block"] / "queue/scheduler") == scheduler_before,
                "daemon changed the selected scheduler")
        if qos_before is not None:
            require(self.read("/sys/fs/cgroup/io.cost.qos") == qos_before,
                    "daemon changed io.cost policy")
        phases = {}
        for name, weights in (("equal", (100, 100)), ("unequal", (100, 1000)),
                              ("reversed", (1000, 100))):
            phases[name] = self.run_phase(mechanism, name, weights)
        self.set_map(100, 1000)
        self.wait_weights(mechanism, [100, 1000])
        systemd_values = [100, 10000] if mechanism == "bfq" else [100, 1000]
        dbus_text = [self.systemd_weight_text(unit) for unit in self.slices]
        require(all(str(value) in text for value, text in zip(systemd_values, dbus_text)),
                "systemd did not read back exact per-device weights")
        kernel_values = [self.kernel_weight(user.pw_uid, mechanism, self.devices[0]["major_minor"])
                         for user in self.users]
        require(kernel_values == [100, 1000], "kernel weight readback differs")
        introspection = self.command("busctl", "introspect", "--no-pager", "--no-legend",
                                     "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/" +
                                     self.slices[0].replace("-", "_2d").replace(".", "_2e"),
                                     "org.freedesktop.systemd1.Slice").stdout
        require(re.search(r"IODeviceWeight\s+property\s+a\(st\)", introspection) is not None,
                "D-Bus IODeviceWeight signature differs")
        aggregates = {name: self.aggregate(phase["intervals"]) for name, phase in phases.items()}
        return {"outcome": "EFFECT_QUALIFIED", "device": self.devices[0]["major_minor"],
                "setup": setup,
                "transport": {"dbus_property": "IODeviceWeight", "dbus_signature": "a(st)",
                              "dbus_readback": dbus_text, "exact_readback": True,
                              "exact_kernel_entry": True, "systemd_values": systemd_values,
                              "kernel_values": kernel_values},
                "phases": phases, "reported_aggregates": aggregates}

    def authority_evidence(self):
        complete_metrics = self.wait_programmed()
        complete_users = int(parse_metric(complete_metrics,
                                          "resman_io_device_weight_complete_users"))
        partial_users = int(parse_metric(complete_metrics,
                                         "resman_io_device_weight_partial_users"))
        require(complete_users >= 2, "test users did not start with complete authority")
        complete = {"coverage": "complete", "complete_users": complete_users,
                    "partial_users": partial_users,
                    "aggregate_coverage": active_metric_label(
                        complete_metrics, "resman_io_device_weight_authority_coverage", "coverage")}
        stray = "resman-iow-effect-stray.service"
        self.command("systemd-run", "--unit", stray, "--uid", self.users[0].pw_name,
                     "--slice", "system.slice", "--property=Type=exec",
                     str(self.workload_path), str(self.device_paths[0]))
        try:
            def partial_snapshot():
                metrics = self.metrics()
                if (parse_metric(metrics, "resman_io_device_weight_complete_users") <= complete_users - 1 and
                        parse_metric(metrics, "resman_io_device_weight_partial_users") >= partial_users + 1):
                    return metrics
                return None
            partial_metrics = eventually(partial_snapshot,
                "partial authority was not published", 60)
            partial = {"coverage": "partial", "partial_users": int(parse_metric(
                partial_metrics, "resman_io_device_weight_partial_users")),
                       "complete_users": int(parse_metric(
                           partial_metrics, "resman_io_device_weight_complete_users")),
                       "aggregate_coverage": active_metric_label(
                           partial_metrics, "resman_io_device_weight_authority_coverage", "coverage"),
                       "programmed": parse_metric(partial_metrics,
                                                  "resman_io_device_weight_programmed") == 1}
        finally:
            self.command("systemctl", "stop", stray, check=False)
            self.command("systemctl", "reset-failed", stray, check=False)
        return {"complete": complete, "partial": partial}

    def public_observability(self):
        metrics = self.wait_programmed()
        provenance_value = parse_metric(metrics, "resman_io_device_weight_effect_qualification_info",
                                        {"provenance": PROVENANCE})
        def database_row():
            if not self.database.exists():
                return None
            with sqlite3.connect("file:%s?mode=ro" % self.database, uri=True) as database:
                version = database.execute("PRAGMA user_version").fetchone()[0]
                row = database.execute("SELECT io_device_weight_effect_qualified, "
                    "io_device_weight_effect_qualification_provenance FROM system_metrics "
                    "ORDER BY timestamp DESC LIMIT 1").fetchone()
            return (version, row) if row and row[0] == 1 and row[1] == PROVENANCE else None
        version, row = eventually(database_row, "SQLite did not persist qualification provenance", 30)
        self.record("public-prometheus.json", {"text": metrics})
        self.record("public-sqlite.json", {"schema": version, "row": list(row)})
        return {"prometheus": {"functionally_accepted": int(parse_metric(
                    metrics, "resman_io_device_weight_functionally_accepted")),
                    "effect_qualified": int(parse_metric(metrics,
                                                           "resman_io_device_weight_effect_qualified")),
                    "provenance": PROVENANCE if provenance_value == 1 else "none"},
                "sqlite": {"schema": version, "effect_qualified": row[0], "provenance": row[1]}}

    def property_contains(self, unit, prop, device):
        target = next((item["path"] for item in self.devices if item["major_minor"] == device), device)
        if prop == "IODeviceWeight":
            return target in self.systemd_weight_text(unit)
        return target in self.command("systemctl", "show", unit, "--property=" + prop,
                                      "--value", check=False).stdout

    def composition_evidence(self):
        first, second = [item["major_minor"] for item in self.devices]
        self.configure_device_mechanism(self.devices[1], "bfq")
        self.set_config({"IO_WEIGHT_DEVICES": first + "," + second, "IO_LIMIT_ENABLED": "true",
                         "IO_DEVICE_FILTER": first, "IO_THRESHOLD": 2, "IO_RELEASE_THRESHOLD": 1,
                         "IO_READ_BPS": "32M", "IO_WRITE_BPS": "max", "IO_READ_IOPS": 0,
                         "IO_WRITE_IOPS": 0, "IO_THRESHOLD_DURATION": 0,
                         "CPU_THRESHOLD": 2, "CPU_RELEASE_THRESHOLD": 1,
                         "CPU_THRESHOLD_DURATION": 0, "IGNORE_SYSTEM_LOAD": "true"})
        def first_layout():
            return (self.property_contains(self.slices[0], "IODeviceWeight", first) and
                    self.property_contains(self.slices[0], "IODeviceWeight", second) and
                    self.property_contains(self.slices[0], "IOReadBandwidthMax", first))
        eventually(first_layout, "W intersection H and W minus H were not programmed", 90)
        self.set_config({"IO_WEIGHT_DEVICES": second})
        eventually(lambda: not self.property_contains(self.slices[0], "IODeviceWeight", first) and
                   self.property_contains(self.slices[0], "IODeviceWeight", second) and
                   self.property_contains(self.slices[0], "IOReadBandwidthMax", first),
                   "H minus W did not preserve the independent hard cap", 75)
        self.set_config({"IO_WEIGHT_DEVICES": ""})
        eventually(lambda: not self.property_contains(self.slices[0], "IODeviceWeight", second) and
                   self.property_contains(self.slices[0], "IOReadBandwidthMax", first),
                   "weight release changed the hard cap", 75)
        self.set_config({"IO_WEIGHT_DEVICES": second, "IO_LIMIT_ENABLED": "false"})
        self.wait_programmed()
        eventually(lambda: self.property_contains(self.slices[0], "IODeviceWeight", second) and
                   not self.property_contains(self.slices[0], "IOReadBandwidthMax", first),
                   "hard-cap release changed the weight", 75)
        return {"intersection": {"weight": True, "hard_cap": True},
                "weight_only": {"weight": True, "hard_cap": False},
                "hard_cap_only": {"weight": False, "hard_cap": True},
                "independent_release": True, "separate_leases": True}

    def lifecycle_evidence(self):
        first = self.devices[0]
        self.set_config({"IO_WEIGHT_DEVICES": first["major_minor"], "IO_LIMIT_ENABLED": "false",
                         "BLACKOUT": ""})
        self.set_map(100, 100)
        self.wait_programmed()
        old_pid = int(self.command("systemctl", "show", "resman", "--property=MainPID", "--value").stdout)
        os.kill(old_pid, signal.SIGKILL)
        eventually(lambda: (lambda value: value > 0 and value != old_pid)(int(self.command(
            "systemctl", "show", "resman", "--property=MainPID", "--value", check=False).stdout or "0")),
                   "packaged daemon did not restart", 45)
        self.wait_programmed()
        restart = "PASS"
        self.set_config({"BLACKOUT": "* 00-24"})
        self.wait_released()
        blackout = "PASS"
        self.set_config({"BLACKOUT": ""})
        self.wait_programmed()
        scheduler_path = first["block"] / "queue/scheduler"
        values = self.read(scheduler_path).replace("[", "").replace("]", "").split()
        alternative = next((value for value in ("mq-deadline", "none", "kyber") if value in values), None)
        require(alternative is not None, "no BFQ capability-loss control is available")
        scheduler_path.write_text(alternative + "\n")
        self.wait_released()
        capability_loss = "PASS"
        scheduler_path.write_text("bfq\n")
        self.wait_programmed()
        unit = self.slices[0]
        # Exercise an external write against the same canonical tuple that the
        # classifier hands to the daemon. A by-id alias is a distinct systemd
        # array key even when it resolves to the same block device.
        path = first["path"]
        self.command("systemctl", "set-property", "--runtime", unit,
                     "IODeviceWeight=%s 777" % path)
        self.set_config({"IO_WEIGHT_DEVICES": ""})
        eventually(lambda: "777" in self.systemd_weight_text(unit),
                   "compare-before-restore overwrote an external value", 30)
        self.command("systemctl", "set-property", "--runtime", unit,
                     "IODeviceWeight=%s 100" % path)
        self.stop_resman()
        self.start_resman()
        eventually(lambda: not self.property_contains(unit, "IODeviceWeight", first["major_minor"]),
                   "recovery did not remove the reconciled owned value", 60)
        return {"blackout_release": blackout, "restart_recovery": restart,
                "capability_loss_release": capability_loss, "compare_before_restore": "PASS"}

    def cleanup(self):
        errors = []
        try:
            if self.config.exists() and self.original_config is not None:
                self.config.write_bytes(self.original_config)
                self.config.chmod(0o600)
            if self.original_map is None:
                self.weight_map.unlink(missing_ok=True)
            else:
                self.weight_map.write_bytes(self.original_map)
                self.weight_map.chmod(0o600)
            self.stop_resman()
        except Exception as error:
            errors.append("daemon cleanup: " + str(error))
        for unit in self.workloads + ["resman-iow-effect-stray.service"]:
            self.command("systemctl", "stop", unit, check=False)
            self.command("systemctl", "reset-failed", unit, check=False)
        for item in self.devices:
            try:
                qos = Path("/sys/fs/cgroup/io.cost.qos")
                if qos.exists():
                    qos.write_text(item["major_minor"] + " enable=0\n")
                scheduler = item["block"] / "queue/scheduler"
                scheduler.write_text(selected_scheduler(item["initial_scheduler"]) + "\n")
                Path(item["path"]).chmod(item["initial_mode"])
            except Exception as error:
                errors.append("device cleanup: " + str(error))
        scheduler_restored = all(self.read(item["block"] / "queue/scheduler").strip() == item["initial_scheduler"]
                                 for item in self.devices)
        io_cost_restored = True
        if Path("/sys/fs/cgroup/io.cost.qos").exists():
            current = self.read("/sys/fs/cgroup/io.cost.qos")
            io_cost_restored = all("enable=1" not in (device_row(current, item["major_minor"]) or "")
                                   for item in self.devices)
        units_removed = all(self.command("systemctl", "show", unit, "--property=ActiveState", "--value",
                                         check=False).stdout.strip() in ("", "inactive", "failed")
                            for unit in self.workloads)
        leases_removed = not self.journal.exists()
        return {"result": "PASS" if not errors and scheduler_restored and io_cost_restored and
                units_removed and leases_removed else "FAIL", "errors": errors,
                "scheduler_restored": scheduler_restored, "io_cost_restored": io_cost_restored,
                "units_removed": units_removed, "leases_removed": leases_removed}

    def capture_diagnostics(self):
        diagnostics = {}
        if self.log.exists():
            diagnostics["daemon_log"] = "daemon-log.json"
            self.record("daemon-log.json", {"text": self.log.read_text(errors="replace")})
        journal = self.command("journalctl", "--no-pager", "-u", "resman", check=False).stdout
        diagnostics["systemd_journal"] = "systemd-journal.json"
        self.record("systemd-journal.json", {"text": journal})
        probe_journal = self.command(
            "sh", "-c", "journalctl --no-pager -b | grep -E 'resmancapprobe|resman' || true",
            check=False).stdout
        diagnostics["capability_probe_journal"] = "capability-probe-journal.json"
        self.record("capability-probe-journal.json", {"text": probe_journal})
        try:
            metrics = self.metrics()
        except Exception as error:
            metrics = "unavailable: " + str(error)
        diagnostics["final_prometheus"] = "final-prometheus.json"
        self.record("final-prometheus.json", {"text": metrics})
        if self.journal.exists():
            diagnostics["ownership_journal"] = "ownership-journal.json"
            self.record("ownership-journal.json", {"text": self.journal.read_text(errors="replace")})
        unit_state = {}
        for unit in self.slices:
            unit_state[unit] = self.command(
                "systemctl", "show", unit, "--property=DropInPaths",
                "--property=IODeviceWeight", "--property=IOReadBandwidthMax",
                check=False).stdout
        diagnostics["unit_state"] = "unit-state.json"
        self.record("unit-state.json", unit_state)
        drop_ins = {}
        for unit in self.slices:
            directory = Path("/run/systemd/system.control") / (unit + ".d")
            if directory.is_dir():
                drop_ins[unit] = {path.name: path.read_text(errors="replace")
                                  for path in sorted(directory.glob("*.conf"))}
        diagnostics["unit_drop_ins"] = "unit-drop-ins.json"
        self.record("unit-drop-ins.json", drop_ins)
        return diagnostics

    def run(self):
        exit_code = 1
        cleanup = {"result": "FAIL", "scheduler_restored": False, "io_cost_restored": False,
                   "units_removed": False, "leases_removed": False}
        try:
            self.preflight()
            self.checkpoint("preflight")
            self.start_workloads()
            self.checkpoint("workloads")
            mechanisms = {}
            mechanisms["io_cost"] = self.mechanism_campaign("io_cost")
            self.checkpoint("io_cost")
            mechanisms["bfq"] = self.mechanism_campaign("bfq")
            self.checkpoint("bfq")
            authority = self.authority_evidence()
            self.checkpoint("authority")
            public = self.public_observability()
            self.checkpoint("public")
            composition = self.composition_evidence()
            self.checkpoint("composition")
            lifecycle = self.lifecycle_evidence()
            self.checkpoint("lifecycle")
            self.set_config({"IO_WEIGHT_DEVICES": ""})
            self.wait_released()
            self.checkpoint("release")
            self.result = "PASS"
            exit_code = 0
        except Blocked as error:
            self.result = "BLOCKED"
            self.error = str(error)
            exit_code = 77
        except Exception as error:
            self.result = "FAIL"
            self.error = str(error)
            exit_code = 1
        finally:
            diagnostics = {}
            try:
                diagnostics = self.capture_diagnostics()
            except Exception as error:
                diagnostics = {"capture_error": str(error)}
            try:
                cleanup = self.cleanup()
                if cleanup["result"] != "PASS":
                    self.result, exit_code = "FAIL", 1
            except Exception as error:
                cleanup = {"result": "FAIL", "error": str(error), "scheduler_restored": False,
                           "io_cost_restored": False, "units_removed": False, "leases_removed": False}
                self.result, exit_code = "FAIL", 1
            summary = {"schema": 1, "scope": self.profile_config["scope"],
                       "profile": self.profile,
                       "cadence": {"interval_count": self.profile_config["interval_count"],
                                   "interval_seconds": self.profile_config["interval_seconds"]},
                       "completed_stages": self.completed_stages,
                       "provenance": PROVENANCE,
                       "source": {"revision": self.revision,
                                  "tree": self.source_tree,
                                  "qualification_revision": self.qualification_revision,
                                  "qualification_tree": self.qualification_tree},
                       "result": self.result, "cleanup": cleanup,
                       "raw_files": self.raw_files, "diagnostics": diagnostics}
            for name in ("package_info", "platform"):
                if hasattr(self, name):
                    summary["package" if name == "package_info" else name] = getattr(self, name)
            if self.devices:
                summary["devices"] = device_evidence(self.devices)
            for name in ("mechanisms", "authority", "public", "composition", "lifecycle"):
                if name in locals():
                    summary["public_observability" if name == "public" else name] = locals()[name]
            if hasattr(self, "error"):
                summary["error"] = self.error
            self.summary = summary
            (self.evidence / "summary.json").write_text(json.dumps(summary, sort_keys=True, indent=2) + "\n")
        return exit_code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--source-tree", required=True)
    parser.add_argument("--qualification-revision", required=True)
    parser.add_argument("--qualification-tree", required=True)
    parser.add_argument("--package", type=Path, required=True)
    parser.add_argument("--device", action="append", type=Path, required=True)
    parser.add_argument("--profile", choices=tuple(PROFILES), default="qualification")
    args = parser.parse_args()
    args.evidence.mkdir(mode=0o700, parents=True, exist_ok=False)
    campaign = Campaign(args.bundle, args.evidence, args.run_id, args.revision, args.source_tree,
                        args.qualification_revision, args.qualification_tree, args.package, args.device,
                        args.profile)
    raise SystemExit(campaign.run())


if __name__ == "__main__":
    main()
