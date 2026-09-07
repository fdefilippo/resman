#!/usr/bin/env python3
"""Bind PSI evidence to its artifact and recompute assertions from actual scrapes."""
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import sys


def require(condition, message):
    if not condition:
        raise ValueError(message)


def metadata(directory, binary, revision, run_id):
    directory, binary = Path(directory), Path(binary)
    artifacts = directory / "artifacts"
    artifacts.mkdir(mode=0o700, exist_ok=True)
    retained = artifacts / "resman"
    shutil.copyfile(binary, retained)
    retained.chmod(0o700)
    result = {"source_revision": revision, "kernel": os.uname().release,
              "host_identity": Path("/etc/machine-id").read_text().strip(),
              "boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip(), "run_id": run_id,
              "tested_artifact": "source-binary, not the installed package", "artifact_path": "artifacts/resman",
              "tested_binary_sha256": hashlib.sha256(retained.read_bytes()).hexdigest()}
    (directory / "environment.json").write_text(json.dumps(result, indent=2) + "\n")


def metric(scrape, name, username=None):
    found = []
    for line in scrape.splitlines():
        if not re.match(re.escape(name) + r"(?:\{|\s)", line):
            continue
        if username is not None and ('username="' + username + '"') not in line:
            continue
        value = float(line.split()[-1])
        require(math.isfinite(value), "nonfinite metric")
        found.append(value)
    require(len(found) == 1, "missing or ambiguous metric: " + name)
    return found[0]


def validate(directory):
    directory = Path(directory)
    proof = {}
    for line in (directory / "psi-refresh-neutrality.txt").read_text().splitlines():
        key, separator, value = line.partition("=")
        require(separator and key not in proof, "invalid PSI proof field")
        proof[key] = value
    environment = dict(line.split("=", 1) for line in (directory / "environment.txt").read_text().splitlines())
    user = environment["test_user"]
    require(proof.get("psi_available") == "true" and proof.get("psi_event_driven_active") == "true", "missing PSI activation proof")
    values = {}
    for suffix, filename in (("before", "psi-baseline.prom"), ("after", "psi-after-refresh.prom")):
        scrape = (directory / filename).read_text()
        require(metric(scrape, "resman_observation_host_cpu_sample_available") == 1,
                "host observation has no available sample: " + suffix)
        values["control_cycles_" + suffix] = metric(scrape, "resman_control_cycles_total")
        values["collections_" + suffix] = metric(scrape, "resman_metrics_collection_duration_seconds_count")
        values["decision_cpu_" + suffix] = metric(scrape, "resman_user_cpu_usage_percent", user)
        values["decision_ema_" + suffix] = metric(scrape, "resman_user_cpu_usage_ema_percent", user)
        values["system_observation_" + suffix] = metric(scrape, "resman_cpu_total_usage_percent")
    require(all(key in proof and float(proof[key]) == value for key, value in values.items()), "summary differs from retained scrapes")
    require(values["control_cycles_before"] >= 2 and values["control_cycles_before"] == values["control_cycles_after"], "control cycle advanced")
    require(values["collections_after"] >= values["collections_before"] + 2, "two observations were not measured")
    require(values["decision_cpu_before"] == values["decision_cpu_after"] and
            values["decision_ema_before"] == values["decision_ema_after"], "decision state changed")
    require(values["system_observation_before"] != values["system_observation_after"], "host observation did not change")
    log = (directory / "psi-resman.log").read_text()
    require("PSI event-driven mode enabled" in log and "PSI event received" not in log, "not a refresh-only PSI window")
    return values


if __name__ == "__main__":
    if sys.argv[1] == "metadata":
        metadata(*sys.argv[2:])
    elif sys.argv[1] == "validate":
        evidence = Path(sys.argv[2])
        proof = validate(evidence)
        (evidence / "psi-refresh-neutrality.json").write_text(json.dumps(proof, indent=2) + "\n")
        (evidence / "checks.json").write_text(json.dumps({"psi-refresh-neutrality": "PASS"}) + "\n")
    else:
        raise ValueError("unsupported PSI evidence action")
