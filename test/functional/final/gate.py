#!/usr/bin/env python3
"""Collect and validate the complete revision-bound nq6 matrix without fallback PASS."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

from catalog import ROWS, validate_catalog

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "test/functional/real-kernel"))
from native_placement import LAYOUT, SCOPE, assert_equal_layout
from native_proportional import LEAVES, compare_reference, ratios, validate_sample_interval
from native_weighted_io import validate_proof as validate_weighted_io
from native_reference import analyze
from native_replicas import summarize
from native_psi_evidence import validate as validate_psi


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read_json(path):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "duplicate JSON key: " + key)
            result[key] = value
        return result
    return json.loads(Path(path).read_text(), object_pairs_hook=unique)


def fields(path):
    result = {}
    for line in Path(path).read_text().splitlines():
        key, separator, value = line.partition("=")
        require(separator and key not in result, "malformed or duplicate environment field")
        result[key] = value
    return result


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def retained_artifact(directory, relative, expected):
    relative = Path(relative)
    require(not relative.is_absolute() and relative.parts and ".." not in relative.parts, "artifact path escapes evidence")
    current = directory
    for component in relative.parts:
        current = current / component
        require(not current.is_symlink(), "artifact path contains a symlink")
    require(current.is_file(), "tested artifact was not retained")
    require(digest(current) == expected, "retained artifact bytes differ from measured identity")


def check_placement(frames, groups, paused=()):
    require(len(frames) == 13, "a window must retain all thirteen synchronized frames")
    previous = None
    for index, frame in enumerate(frames):
        layout = frame["placement"]
        require(set(layout) == {group + "/" + leaf for group in groups for leaf in LEAVES}, "missing compared leaf")
        births = {}
        for name, workers in layout.items():
            require(len(workers) == 6, "wrong worker count")
            require(all(w["state"] == ("T" if name in paused else "R") and len(w["affinity"]) == 1
                        for w in workers.values()), "worker not runnable or singleton-affined")
            births[name] = {pid: w["birth"] for pid, w in workers.items()}
        assert_equal_layout({name: workers for name, workers in layout.items() if name not in paused})
        require(previous is None or previous == births, "worker changed during interval")
        previous = births
        if index:
            validate_sample_interval(frames[index - 1], frame)


def proportional(directory, revision):
    scope = read_json(directory / "measurement-scope.json")
    require(scope["scope"] == SCOPE and scope["daemon_exercised"] is True, "wrong proportional scope")
    require(scope["source_revision"] == revision and scope["cpu_assignment_multiset"] == LAYOUT,
            "wrong proportional revision or placement")
    require(scope["full_contention_windows"] == 6 and scope["window_seconds"] == 60, "missing full windows")
    measured = []
    last = None
    for window in range(6):
        frames = read_json(directory / ("full-contention-%d-raw.json" % window))
        require(last is None or frames[0]["time"] >= last["time"], "full contention windows overlap or repeat")
        if last is not None:
            consecutive(last, frames[0])
        check_placement(frames, ("native", "oracle", "stale"))
        values = ratios(frames[0], frames[-1])
        compare_reference(values)
        require(min(abs(values[g]["mapped"] - values["stale"]["mapped"]) for g in ("native", "oracle")) >= 2,
                "stale control is not discriminating")
        for group in values:
            require(values[group]["parent_usec"] >= (frames[-1]["time"] - frames[0]["time"]) * 1.2e6 * 0.8,
                    "parent bandwidth was not saturated")
            require(values[group]["throttling"]["nr_throttled"] > 0, "parent was not throttled")
        measured.append(values)
        last = frames[-1]
    idle_frames = read_json(directory / "mapped-idle-raw.json")
    require(idle_frames[0]["time"] >= last["time"], "lending precedes full contention")
    consecutive(last, idle_frames[0])
    check_placement(idle_frames, ("native", "oracle", "stale"), {g + "/a" for g in ("native", "oracle", "stale")})
    idle = ratios(idle_frames[0], idle_frames[-1])
    compare_reference(idle)
    require(idle["native"]["a"] < 0.1, "paused leaf consumed CPU")
    require(all(idle["native"][leaf] > measured[-1]["native"][leaf] + 1 for leaf in ("b", "root", "best")),
            "idle capacity was not lent")
    root = read_json(directory / "root-progress.json")
    for phase in ("full_response_seconds", "idle_response_seconds"):
        require(len(root[phase]) == 3 and all(0 <= value <= 3 for value in root[phase]), "root response proof failed")
    require(read_json(directory / "unchanged-weights.json")["journal_unchanged"] is True,
            "lending changed the journal")
    return {"scope": SCOPE, "windows": 6,
            "worst_aggregate_gap_pp": max(abs(v["native"]["mapped"] - v["oracle"]["mapped"]) for v in measured),
            "worst_leaf_gap_pp": max(abs(v["native"][leaf] - v["oracle"][leaf]) for v in measured for leaf in LEAVES),
            "minimum_control_separation_pp": min(abs(v[g]["mapped"] - v["stale"]["mapped"])
                                                  for v in measured for g in ("native", "oracle"))}


def consecutive(previous, current):
    for group in previous["nodes"]:
        for node in ("parent",) + LEAVES:
            old, new = previous["nodes"][group][node], current["nodes"][group][node]
            require(old["identity"] == new["identity"], "unit changed between measured windows")
            require(all(new["stat"][key] >= old["stat"][key] for key in old["stat"]), "counter reset between windows")
    old_births = {name: {pid: worker["birth"] for pid, worker in workers.items()} for name, workers in previous["placement"].items()}
    new_births = {name: {pid: worker["birth"] for pid, worker in workers.items()} for name, workers in current["placement"].items()}
    require(old_births == new_births, "workload changed between measured windows")


def inspect_evidence(row, directory, revision, artifact):
    directory = Path(directory)
    status = (directory / "result").read_text().strip()
    require(status in {"PASS", "FAIL", "BLOCKED", "CHARACTERIZATION"}, "unknown evidence status")
    env = fields(directory / "environment.txt")
    require(env.get("scenario") == row.scenario, "wrong scenario")
    require(env.get("source_revision") == revision, "stale evidence revision")
    require(env.get("result") == status, "inconsistent result")
    if status in {"FAIL", "BLOCKED"}:
        require(env.get("cleanup") != "FAIL" or status == "FAIL", "failed cleanup cannot be BLOCKED")
        return status, {"detail": env.get("detail", status)}
    expected = "CHARACTERIZATION" if row.scenario == "systemd-native-reference" else "PASS"
    require(status == expected, "characterization is not delivery acceptance")
    require(env.get("cleanup") == "PASS" and env.get("kernel"), "missing verified cleanup or kernel")
    meta = read_json(directory / "environment.json")
    require(meta["source_revision"] == revision and meta["kernel"] == env["kernel"], "metadata identity differs")
    require(meta.get("host_identity") and meta.get("boot_id") and meta.get("run_id"), "missing host, boot or run identity")
    require(re.fullmatch(r"[0-9a-f]{64}", meta["tested_binary_sha256"]) is not None, "invalid binary identity")
    retained_artifact(directory, meta["artifact_path"], meta["tested_binary_sha256"])
    expected_binary = artifact.get("binaries", {}).get(str(directory.resolve()), artifact.get("binary_sha256"))
    require(expected_binary is not None and meta["tested_binary_sha256"] == expected_binary, "wrong tested binary")
    if row.artifact == "package":
        require(meta["tested_artifact"] == artifact["kind"] and artifact["kind"] in {"rpm", "deb"},
                "source-binary evidence is not package acceptance")
        require(meta["package_identity"] == artifact["identity"] and meta["package_sha256"] == artifact["sha256"],
                "wrong tested package identity")
        retained_artifact(directory, meta["package_artifact_path"], meta["package_sha256"])
    elif row.artifact == "adapter-probe":
        require(meta["tested_artifact"] == "adapter-probe, not the daemon or installed package", "wrong adapter artifact class")
    else:
        require(meta["tested_artifact"] == "source-binary, not the installed package", "wrong artifact class")
    if row.checks:
        checks = read_json(directory / "checks.json")
        require(set(checks) == set(row.checks), "missing or unknown required check")
        require(all(value == "PASS" for value in checks.values()), "required check is not PASS")
        for name in row.checks:
            require(bool(read_json(directory / (name + ".json"))), "check has no retained observation: " + name)
    extra = {}
    if row.scenario in {"systemd-native-proportional", "systemd-native-reference-pinned-six", "systemd-native-reference"}:
        require(read_json(directory / "measurement-scope.json")["run_id"] == meta["run_id"], "scope and environment run identities differ")
    if row.scenario == "systemd-native-weighted-io-adapter":
        extra = validate_weighted_io(lambda name: read_json(directory / name), meta)
    elif row.scenario == "systemd-native-proportional":
        extra = proportional(directory, revision)
    elif row.scenario == "psi-refresh-neutrality":
        extra = {"recomputed": validate_psi(directory)}
    elif row.scenario == "systemd-native-reference":
        scope = read_json(directory / "measurement-scope.json")
        require(scope["scope"] == "unbound-characterization; no acceptance verdict" and
                scope["daemon_exercised"] is False and scope["source_revision"] == revision,
                "wrong characterization scope")
        raw = read_json(directory / "reference-raw.json")
        rows = analyze(raw)
        extra = {"scope": scope["scope"], "windows": len([r for r in rows if r["window_seconds"] == 60]),
                 "worst_aggregate_dispersion_pp": max(abs(r["differences_pp"]["mapped"]) for r in rows if r["window_seconds"] == 60)}
    return status, {"host_identity": meta["host_identity"], "boot_id": meta["boot_id"], "kernel": meta["kernel"], "run_id": meta["run_id"], **extra}


def run_local(output, command):
    """Whole-package JSON output proves that tests ran, not a matching exit code alone."""
    with (output / "local-tests.jsonl").open("w") as log:
        result = subprocess.run(command, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
    require(result.returncode == 0, "local package tests failed")
    events = []
    for line in (output / "local-tests.jsonl").read_text().splitlines():
        try:
            events.append(json.loads(line))
        except ValueError:
            continue
    require(any(event.get("Action") == "pass" and event.get("Test") for event in events), "local command executed zero tests")
    require(not any(event.get("Action") == "fail" for event in events), "local test failure in event stream")


def collect(row, root, host, runner, log):
    smolvm = row.scenario in {"non-systemd-migration", "missing-io-startup", "mcp-filter-reload"}
    if (not host or not row.remote) and not smolvm:
        return []
    expected = 3 if row.scenario == "systemd-native-reference-pinned-six" else 1
    directories = []
    for replica in range(expected):
        target = root / (row.scenario + "-%d" % replica)
        target.mkdir()
        env = dict(os.environ, REAL_KERNEL_EVIDENCE_ROOT=str(target))
        command = [str(runner), row.scenario, host]
        if smolvm:
            env.update(SMOLVM_EVIDENCE_ROOT=str(target), SMOLVM_SCENARIO=row.scenario)
            command = [os.environ.get("FINAL_GATE_SMOLVM_RUNNER", str(ROOT / "test/functional/smolvm/run.sh")), "run"]
        with log.open("a") as output:
            output.write(json.dumps(command) + "\n")
            output.flush()
            result = subprocess.run(command, env=env, stdout=output, stderr=subprocess.STDOUT)
        found = sorted({path.parent for path in target.glob("*/result")})
        require(len(found) == 1, "runner retained zero or multiple result directories (exit %d)" % result.returncode)
        status = (found[0] / "result").read_text().strip()
        require((result.returncode, status) in {(0, "PASS"), (0, "CHARACTERIZATION"), (77, "BLOCKED"), (1, "FAIL")},
                "runner exit and result disagree")
        directories.extend(found)
    return directories


def matrix(manifest, revision, output, host="", runner=None, local=True):
    validate_catalog()
    require(manifest.get("schema") == 1 and manifest.get("source_revision") == revision, "wrong manifest revision or schema")
    supplied = manifest.get("evidence", {})
    require(set(supplied) <= {row.scenario for row in ROWS}, "unknown supplied scenario")
    require(set(manifest.get("artifacts", {})) <= {"source-binary", "adapter-probe", "package"}, "unknown artifact class")
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    results = []
    if local:
        try:
            run_local(output, [os.environ.get("GO_BIN", "go"), "test", "-json", "-count=1", "./..."])
            results.append({"scenario": "local-package-contracts", "status": "PASS", "criteria": [], "evidence": [str(output / "local-tests.jsonl")]})
        except (ValueError, OSError) as err:
            results.append({"scenario": "local-package-contracts", "status": "FAIL", "detail": str(err), "criteria": [], "evidence": [str(output / "local-tests.jsonl")]})
    metadata = {}
    for row in ROWS:
        directories = [Path(value).resolve() for value in supplied.get(row.scenario, [])]
        record = {"scenario": row.scenario, "criteria": list(row.criteria), "evidence": [str(d) for d in directories]}
        try:
            artifact = manifest.get("artifacts", {}).get(row.artifact)
            if artifact is None and not directories:
                record.update(status="BLOCKED", detail="expected artifact identity must be supplied before collection")
                results.append(record)
                continue
            if not directories:
                directories = collect(row, output, host, runner, output / "commands.log")
                record["evidence"] = [str(d) for d in directories]
            if not directories:
                record.update(status="BLOCKED", detail="required evidence or capability is unavailable; no substitute acceptance")
            else:
                require(artifact is not None, "missing expected artifact identity")
                require(artifact.get("source_revision") == revision, "artifact is from another revision")
                expected = 3 if row.scenario == "systemd-native-reference-pinned-six" else 1
                # A failed or blocked early replica remains visible, never discarded.
                require(len(directories) == expected, "wrong number of evidence directories")
                inspected = [inspect_evidence(row, d, revision, artifact) for d in directories]
                statuses = [status for status, _ in inspected]
                status = "FAIL" if "FAIL" in statuses else "BLOCKED" if "BLOCKED" in statuses else statuses[0]
                record.update(status=status, observations=[details for _, details in inspected])
                if status == "PASS" and expected == 3:
                    require(len({(m["host_identity"], m["boot_id"], m["kernel"]) for _, m in inspected}) == 1, "replica host/boot/kernel mismatch")
                    spans = sorted((frames[0]["time"], frames[-1]["time"])
                                   for frames in (read_json(d / "reference-raw.json") for d in directories))
                    require(all(spans[i][1] < spans[i + 1][0] for i in range(2)), "reference replicas overlap or repeat")
                    record["recomputed"] = summarize(directories, revision)
                if status in {"PASS", "CHARACTERIZATION"}:
                    metadata[row.scenario] = inspected[0][1]
                record["sha256"] = {str(d): {str(p.relative_to(d)): digest(p) for p in sorted(d.rglob("*")) if p.is_file()}
                                    for d in directories}
        except (AssertionError, ValueError, KeyError, OSError, TypeError) as err:
            record.update(status="FAIL", detail=str(err))
        results.append(record)
    # Calibration from another machine/kernel cannot authorize daemon measurements.
    names = ("systemd-native-proportional", "systemd-native-reference-pinned-six", "systemd-native-reference")
    present = [metadata[name] for name in names if name in metadata]
    if len(present) == 3 and len({(m["host_identity"], m["boot_id"], m["kernel"]) for m in present}) != 1:
        results.append({"scenario": "calibration-identity", "status": "FAIL", "criteria": [5], "evidence": [], "detail": "daemon and calibration host/kernel differ"})
    overall = "FAIL" if any(r["status"] == "FAIL" for r in results) else "BLOCKED" if any(r["status"] == "BLOCKED" for r in results) else "PASS"
    payload = {"source_revision": revision, "result": overall, "independent_review": "pending", "measurement_scope": SCOPE, "rows": results}
    (output / "matrix.json").write_text(json.dumps(payload, indent=2) + "\n")
    (output / "result").write_text(overall + "\n")
    (output / "summary.md").write_text("# nq6 final gate\n\nResult: **%s**\n\nRevision: `%s`\n\nScope: %s. Independent review remains required.\n\n| Scenario | Status | Criteria |\n|---|---|---|\n%s\n" %
        (overall, revision, SCOPE, "\n".join("| %s | %s | %s |" % (r["scenario"], r["status"], ", ".join(map(str, r["criteria"]))) for r in results)))
    return payload


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", default=os.environ.get("FINAL_GATE_MANIFEST"))
    args = parser.parse_args()
    revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=no"], cwd=ROOT, text=True).strip()
    require(not dirty, "tracked worktree must be clean; freeze source throughout the campaign")
    manifest = read_json(args.manifest) if args.manifest else {"schema": 1, "source_revision": revision, "evidence": {}, "artifacts": {}}
    output = Path(os.environ.get("FINAL_GATE_EVIDENCE_ROOT", ROOT / "build/functional/final")) / ("r%s-%d" % (time.strftime("%Y%m%d%H%M%S", time.gmtime()), os.getpid()))
    result = matrix(manifest, revision, output, os.environ.get("RESMAN_REAL_KERNEL_HOST", ""),
                    Path(os.environ.get("FINAL_GATE_REAL_KERNEL_RUNNER", ROOT / "test/functional/real-kernel/remote.sh")))
    print(result["result"] + ": " + str(output))
    return {"PASS": 0, "FAIL": 1, "BLOCKED": 77}[result["result"]]


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, KeyError) as error:
        print("FAIL: " + str(error), file=sys.stderr)
        sys.exit(1)
