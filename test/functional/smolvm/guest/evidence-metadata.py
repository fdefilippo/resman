#!/usr/bin/env python3
"""Bind disposable-guest evidence to the actual source binary and kernel."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sys


def update_fields(directory, values):
    path = Path(directory) / "environment.txt"
    current = {}
    for line in path.read_text().splitlines() if path.exists() else ():
        key, separator, value = line.partition("=")
        if not separator or key in current:
            raise ValueError("malformed or duplicate evidence field: " + key)
        current[key] = value
    current.update(values)
    path.write_text("".join("%s=%s\n" % (key, value) for key, value in current.items()))


def finalize(directory, cleanup, exit_code):
    """The host alone publishes final cleanup/result after guest teardown."""
    directory = Path(directory)
    if cleanup not in {"PASS", "FAIL"}:
        raise ValueError("unknown host cleanup result")
    exit_code = int(exit_code)
    guest = (directory / "result").read_text().strip() if (directory / "result").exists() else "missing"
    result = "FAIL" if cleanup == "FAIL" or guest == "FAIL" else "BLOCKED" if exit_code == 77 else "FAIL" if exit_code else guest
    if result not in {"PASS", "BLOCKED", "FAIL"}:
        result = "FAIL"
    values = {"guest_result": guest, "cleanup": cleanup, "result": result, "exit_code": str(exit_code)}
    path = directory / "environment.txt"
    current = dict(line.split("=", 1) for line in path.read_text().splitlines()) if path.exists() else {}
    if "scenario" not in current and "requested_scenario" in current:
        values["scenario"] = current["requested_scenario"]
    update_fields(directory, values)
    (directory / "result").write_text(result + "\n")


def write_metadata(directory, run_id, revision, scenario):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("a full source revision is required")
    directory = Path(directory)
    artifacts = directory / "artifacts"
    artifacts.mkdir(mode=0o700, exist_ok=True)
    binary = artifacts / "resman"
    shutil.copyfile("/usr/bin/resman", binary)
    binary.chmod(0o700)
    metadata = {"source_revision": revision, "kernel": os.uname().release,
                "host_identity": Path("/etc/machine-id").read_text().strip(),
                "boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                "run_id": run_id, "scenario": scenario,
                "tested_artifact": "source-binary, not the installed package",
                "tested_binary_path": "artifacts/resman",
                "artifact_path": "artifacts/resman",
                "tested_binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest()}
    (directory / "environment.json").write_text(json.dumps(metadata, indent=2) + "\n")
    update_fields(directory, {"source_revision": revision})


if __name__ == "__main__":
    if sys.argv[1:2] == ["finalize"]:
        finalize(*sys.argv[2:])
    else:
        write_metadata(*sys.argv[1:])
