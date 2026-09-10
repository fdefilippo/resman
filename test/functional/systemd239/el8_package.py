#!/usr/bin/env python3
"""Qualify the exact installed EL8 RPM on the systemd 239 contract."""
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time

from native_gate import field, require
from native_package import PackageGate


REQUIRED_CONTROLLERS = frozenset({"cpu", "io", "memory"})
REQUIRED_KERNEL_ARGUMENTS = frozenset({"systemd.unified_cgroup_hierarchy=1", "psi=1"})


def systemd_major_version(output):
    match = re.match(r"systemd ([0-9]+)(?:\s|$)", output)
    require(match is not None, "systemd version output is malformed")
    return int(match.group(1))


def slice_interface_has_control_group_id(output):
    return any(line.split()[:1] == ["ControlGroupId"] for line in output.splitlines())


class EL8PackageGate(PackageGate):
    def preflight(self):
        systemd = self.command("systemctl", "--version").stdout
        os_release = field("/etc/os-release")
        command_line = field("/proc/cmdline")
        filesystem = self.command("stat", "-fc", "%T", "/sys/fs/cgroup").stdout.strip()
        controllers = set(field("/sys/fs/cgroup/cgroup.controllers").split())
        interface = self.command(
            "busctl", "introspect", "--no-pager", "--no-legend",
            "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/user_2eslice",
            "org.freedesktop.systemd1.Slice").stdout
        missing_psi = [name for name in ("cpu", "io", "memory")
                       if not Path("/proc/pressure", name).is_file()]

        require('VERSION_ID="8.10"' in os_release and 'ID="ol"' in os_release,
                "guest is not Oracle Linux 8.10")
        require(systemd_major_version(systemd) == 239, "guest does not run systemd 239")
        require(filesystem == "cgroup2fs", "guest does not use the unified cgroup v2 filesystem")
        require(REQUIRED_CONTROLLERS <= controllers, "guest lacks a required cgroup v2 controller")
        require(REQUIRED_KERNEL_ARGUMENTS <= set(command_line.split()),
                "guest did not boot with the documented cgroup v2 and PSI arguments")
        require(not missing_psi, "guest lacks required PSI files: " + ",".join(missing_psi))
        require(not slice_interface_has_control_group_id(interface),
                "systemd 239 fixture unexpectedly exposes ControlGroupId")

        self.save("el8-runtime-contract", {
            "os_release": os_release.splitlines(),
            "systemd": systemd.splitlines(),
            "kernel": os.uname().release,
            "command_line": command_line,
            "cgroup_filesystem": filesystem,
            "controllers": sorted(controllers),
            "psi": {name: True for name in ("cpu", "io", "memory")},
            "control_group_id_absent": True,
            "slice_interface": interface.splitlines(),
        })
        super().preflight()

    def validate(self):
        super().validate()
        self.validate_fail_closed_identity()

    def validate_fail_closed_identity(self):
        negative_log = self.work / "negative-identity.log"
        negative_config = self.work / "negative-identity.conf"
        config = self.config.read_text()
        config = config.replace("LOG_FILE=" + str(self.log), "LOG_FILE=" + str(negative_log))
        config = re.sub(r"(?m)^ENABLE_PROMETHEUS=.*$", "ENABLE_PROMETHEUS=false", config)
        config = re.sub(r"(?m)^METRICS_DB_ENABLED=.*$", "METRICS_DB_ENABLED=false", config)
        negative_config.write_text(config)
        negative_config.chmod(0o600)

        command = [
            "unshare", "--mount", "--propagation", "private", "/bin/sh", "-ceu",
            'mount -t tmpfs -o mode=0755 resman-negative "$1"; exec "$2" -config "$3"',
            "resman-negative", "/sys/fs/cgroup", str(self.binary), str(negative_config),
        ]
        with (self.evidence / "negative-identity.stdout").open("w") as output:
            process = subprocess.Popen(command, stdout=output, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 20
            while process.poll() is None and time.monotonic() < deadline:
                if "Systemd-native CPU enforcement unavailable" in field(negative_log, ""):
                    break
                time.sleep(0.25)
            if process.poll() is None:
                process.terminate()
            process.wait(timeout=75)

        text = field(negative_log, "") + "\n" + field(self.evidence / "negative-identity.stdout", "")
        require("Systemd-native CPU enforcement selected" not in text,
                "unverifiable cgroup identity selected systemd_native")
        require("enforcement_mode=systemd_native" not in text,
                "unverifiable cgroup identity reported systemd_native success")
        require(process.returncode != 0 or "remaining observation-only" in text,
                "negative identity mutation produced neither refusal nor observation-only fallback")
        require(not self.journal.exists(), "negative identity mutation left a property lease")
        require(not list(Path("/run/systemd/system.control").glob("user-resmancapprobe*.slice.d")),
                "negative identity mutation left capability-probe drop-ins")
        self.save("negative-identity", {
            "exit_code": process.returncode,
            "systemd_native_absent": True,
            "journal_absent": True,
            "probe_drop_ins_absent": True,
        })

    def run(self):
        code = super().run()
        status = (self.evidence / "result").read_text().strip()
        checks = {
            "el8-runtime-contract": "PASS" if (self.evidence / "el8-runtime-contract.json").exists() else "FAIL",
            "negative-identity": "PASS" if (self.evidence / "negative-identity.json").exists() else "FAIL",
            "package-lifecycle": status,
        }
        self.save("el8-checks", checks)
        if set(checks.values()) != {"PASS"}:
            (self.evidence / "result").write_text("FAIL\n")
            return 1
        return code


if __name__ == "__main__":
    require(len(sys.argv) == 3, "usage: el8_package.py RUN_ID SOURCE_REVISION")
    gate = EL8PackageGate(Path(__file__).parent, sys.argv[1], sys.argv[2])

    def interrupted(_signal, _frame):
        raise RuntimeError("EL8 package campaign interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    sys.exit(gate.run())
