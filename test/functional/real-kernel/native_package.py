#!/usr/bin/env python3
"""Exercise the installed RPM binary, proved identical to a retained RPM payload."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import sqlite3
import subprocess
import sys
import traceback

from native_gate import (Blocked, CURRENT_SCHEMA_VERSION, NativeGate, eventually,
                         field, require, sha)


REQUIRED_CHECKS = frozenset({"installed-identity", "shipped-defaults", "schema-reset", "upgrade-750-rejected", "graceful-stop"})
PREVIOUS_SCHEMA_VERSION = 6


def connect_database(path, **kwargs):
    """Open SQLite through the string-path boundary supported by EL8 Python."""
    return sqlite3.connect(str(path), **kwargs)


def matching_package(installed_identity, package_identity, installed_binary, payload):
    require(installed_identity == package_identity, "installed RPM identity differs from supplied artifact")
    require(hashlib.sha256(installed_binary).digest() == hashlib.sha256(payload).digest(),
            "installed binary differs from supplied RPM payload")


def extract_payload(package, member):
    # Extract only named members to stdout, never archive paths onto the host.
    archive = subprocess.run(["rpm2cpio", str(package)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)
    extracted = subprocess.run(["cpio", "--extract", "--to-stdout", "." + member], input=archive.stdout,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)
    require(bool(extracted.stdout), "required RPM member missing: " + member)
    return extracted.stdout


class PackageGate(NativeGate):
    def __init__(self, *args):
        super().__init__(*args)
        self.binary = Path("/usr/bin/resman")
        self.package_checks = {}

    def package_passed(self, name, proof):
        self.save(name, proof)
        self.package_checks[name] = "PASS"

    def preflight(self):
        package = self.bundle / "package.rpm"
        if not package.is_file() or package.is_symlink():
            raise Blocked("supply the exact installed RPM as REAL_KERNEL_PACKAGE; no package is built or installed implicitly")
        for tool in ("rpm", "rpm2cpio", "cpio"):
            if not shutil.which(tool):
                raise Blocked("missing package inspection tool: " + tool)
        if not self.binary.is_file() or self.binary.is_symlink():
            raise Blocked("a regular installed /usr/bin/resman is required")
        query = "%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}"
        package_identity = self.command("rpm", "-qp", "--qf", query, package).stdout.strip()
        installed_identity = self.command("rpm", "-q", "--qf", query, "resman").stdout.strip()
        payload = extract_payload(package, "/usr/bin/resman")
        matching_package(installed_identity, package_identity, self.binary.read_bytes(), payload)
        config = extract_payload(package, "/etc/resman/resman.conf").decode()
        point_map = extract_payload(package, "/etc/resman/cpu-points.map").decode()
        for key, expected in (("CPU_RESERVE_POINTS", "100"), ("CPU_ROOT_POINTS", "100"),
                              ("CPU_BEST_EFFORT_POINTS", "100")):
            matches = re.findall(r"(?m)^" + key + r"=(.*)$", config)
            require(matches == [expected], "unexpected shipped default for " + key)
        authored = [line for line in point_map.splitlines() if line and not line.startswith("#")]
        require(authored == ["[resman-cpu-points-map-v1]"], "shipped map grants unexpected user entitlements")
        # The common lifecycle adjusts these exact shipped defaults, not a source-tree copy.
        (self.bundle / "resman.conf.example").write_text(config)
        super().preflight()
        artifacts = self.evidence / "artifacts"
        artifacts.mkdir(mode=0o700, exist_ok=True)
        shutil.copyfile(package, artifacts / "package.rpm")
        (artifacts / "package.rpm").chmod(0o600)
        metadata = json.loads((self.evidence / "environment.json").read_text())
        metadata.update(tested_artifact="rpm", package_identity=package_identity, package_sha256=sha(package),
                        package_artifact_path="artifacts/package.rpm")
        self.save("environment", metadata)
        self.package_passed("installed-identity", {"package_identity": package_identity, "package_sha256": sha(package),
                            "installed_binary_sha256": sha(self.binary), "payload_binary_sha256": hashlib.sha256(payload).hexdigest()})
        self.package_passed("shipped-defaults", {"reserve_points": 100, "root_points": 100, "best_effort_points": 100,
                            "empty_map": True, "config_sha256": hashlib.sha256(config.encode()).hexdigest()})

    def validate(self):
        super().validate()
        self.package_passed("upgrade-750-rejected", json.loads((self.evidence / "rejected-config.json").read_text()))
        old_database = self.work / "schema-six.db"
        with connect_database(old_database) as database:
            database.execute("PRAGMA user_version=%d" % PREVIOUS_SCHEMA_VERSION)
        old_database.chmod(0o600)
        old_config = self.work / "schema-six.conf"
        old_log = self.work / "schema-six.log"
        body = self.config.read_text().replace("METRICS_DB_PATH=" + str(self.db), "METRICS_DB_PATH=" + str(old_database))
        body = body.replace("LOG_FILE=" + str(self.log), "LOG_FILE=" + str(old_log))
        old_config.write_text(body)
        old_config.chmod(0o600)
        with (self.evidence / "schema-six-startup.log").open("w") as log:
            process = subprocess.Popen([str(self.binary), "-config", str(old_config)], stdout=log, stderr=log)
            try:
                eventually(lambda: "schema version %d" % PREVIOUS_SCHEMA_VERSION in field(old_log, "")
                           and "delete or move" in field(old_log, ""),
                           "package did not report explicit incompatible-schema reset", 20)
            finally:
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=75)
        with connect_database("file:%s?mode=ro" % old_database, uri=True) as database:
            require(database.execute("PRAGMA user_version").fetchone()[0] == PREVIOUS_SCHEMA_VERSION,
                    "old schema was silently migrated")
        self.save("schema-six-preserved", {"old_schema": PREVIOUS_SCHEMA_VERSION,
                                             "operator_reset_required": True})

    def run(self):
        code = super().run()
        status = (self.evidence / "result").read_text().strip()
        try:
            if status == "PASS":
                with connect_database("file:%s?mode=ro" % self.db, uri=True) as database:
                    version = database.execute("PRAGMA user_version").fetchone()[0]
                    rows = database.execute("SELECT count(*) FROM user_metrics").fetchone()[0]
                require(version == CURRENT_SCHEMA_VERSION and rows > 0,
                        "package produced no schema-seven runtime history")
                self.package_passed("schema-reset", {"old_schema_preserved": PREVIOUS_SCHEMA_VERSION,
                                                      "new_schema": version, "measured_rows": rows})
                self.package_passed("graceful-stop", json.loads((self.evidence / "graceful-stop.json").read_text()))
                require(set(self.package_checks) == REQUIRED_CHECKS and all(v == "PASS" for v in self.package_checks.values()),
                        "package acceptance is incomplete")
        except Exception:
            status, code = "FAIL", 1
            traceback.print_exc()
        for key in REQUIRED_CHECKS:
            self.package_checks.setdefault(key, "BLOCKED")
        # Keep all original lifecycle results alongside the package-specific projection.
        self.save("package-lifecycle-checks", self.checks)
        self.save("checks", self.package_checks)
        environment = (self.evidence / "environment.txt").read_text()
        environment = environment.replace("scenario=systemd-native-lifecycle", "scenario=native-package-acceptance")
        environment = re.sub(r"(?m)^result=.*$", "result=" + status, environment)
        (self.evidence / "environment.txt").write_text(environment)
        (self.evidence / "result").write_text(status + "\n")
        return code


if __name__ == "__main__":
    require(sys.argv[1] == "native-package-acceptance", "unsupported package scenario")
    gate = PackageGate(Path(__file__).parent, sys.argv[2], sys.argv[3])
    def interrupted(_signal, _frame):
        raise RuntimeError("package campaign interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    sys.exit(gate.run())
