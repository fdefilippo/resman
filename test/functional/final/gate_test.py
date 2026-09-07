#!/usr/bin/env python3
"""Mutation-shaped tests for the authoritative final matrix, without a real host."""
import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import gate
from catalog import ROWS, validate_catalog
from native_reference_test import samples
from native_psi_evidence_test import fixture as psi_fixture


def put(path, data):
    path.write_text(json.dumps(data))


def placement(groups, paused=False):
    return {group + "/" + leaf: {str(i): {"birth": "90", "state": "T" if paused and leaf == "a" else "R",
                                          "affinity": [cpu]} for i, cpu in enumerate(gate.LAYOUT)}
            for group in groups for leaf in gate.LEAVES}


def fixture(root):
    binary_hash = gate.hashlib.sha256(b"fixture-binary").hexdigest()
    package_hash = gate.hashlib.sha256(b"fixture-package").hexdigest()
    manifest = {"schema": 1, "source_revision": "revision", "evidence": {}, "artifacts": {
        "source-binary": {"source_revision": "revision", "binary_sha256": binary_hash},
        "adapter-probe": {"source_revision": "revision", "binary_sha256": binary_hash},
        "package": {"source_revision": "revision", "binary_sha256": binary_hash, "sha256": package_hash,
                    "kind": "rpm", "identity": "resman-1.33.0-2.el9"}}}
    for row in ROWS:
        paths = []
        for replica in range(3 if row.scenario == "systemd-native-reference-pinned-six" else 1):
            directory = root / (row.scenario + str(replica))
            directory.mkdir()
            (directory / "binary").write_bytes(b"fixture-binary")
            paths.append(str(directory))
            status = "CHARACTERIZATION" if row.scenario == "systemd-native-reference" else "PASS"
            (directory / "result").write_text(status)
            (directory / "environment.txt").write_text("scenario=%s\nsource_revision=revision\nkernel=6.12-test\ncleanup=PASS\nresult=%s\n" % (row.scenario, status))
            meta = {"source_revision": "revision", "kernel": "6.12-test", "host_identity": "host", "boot_id": "boot", "run_id": directory.name,
                    "tested_binary_sha256": binary_hash, "artifact_path": "binary",
                    "tested_artifact": "source-binary, not the installed package"}
            if row.artifact == "package":
                (directory / "package.rpm").write_bytes(b"fixture-package")
                meta.update(tested_artifact="rpm", package_identity="resman-1.33.0-2.el9", package_sha256=package_hash,
                            package_artifact_path="package.rpm")
            if row.artifact == "adapter-probe":
                meta["tested_artifact"] = "adapter-probe, not the daemon or installed package"
            put(directory / "environment.json", meta)
            if row.checks:
                put(directory / "checks.json", dict.fromkeys(row.checks, "PASS"))
                for check in row.checks:
                    put(directory / (check + ".json"), {"observation": 1})
            if row.scenario == "psi-refresh-neutrality":
                psi_fixture(directory)
            if row.scenario.startswith("systemd-native-reference"):
                raw = samples()
                for frame in raw:
                    frame["time"] += replica * 400
                    frame["placement"] = placement(("referencea", "referenceb", "stale"))
                put(directory / "reference-raw.json", raw)
                put(directory / "measurement-scope.json", {
                    "scope": gate.SCOPE if status == "PASS" else "unbound-characterization; no acceptance verdict",
                    "daemon_exercised": False, "source_revision": "revision", "cpu_assignment_multiset": gate.LAYOUT,
                    "run_id": directory.name})
            if row.scenario == "systemd-native-proportional":
                for window in range(6):
                    raw = copy.deepcopy(samples()[window * 12:window * 12 + 13])
                    for frame in raw:
                        frame["nodes"]["native"] = frame["nodes"].pop("referencea")
                        frame["nodes"]["oracle"] = frame["nodes"].pop("referenceb")
                        frame["placement"] = placement(("native", "oracle", "stale"))
                    put(directory / ("full-contention-%d-raw.json" % window), raw)
                idle = copy.deepcopy(raw)
                final = copy.deepcopy(raw[-1])
                for step, frame in enumerate(idle):
                    frame["time"] = final["time"] + step * 5
                    for group, nodes in frame["nodes"].items():
                        for name, node in nodes.items():
                            for key in node["stat"]:
                                node["stat"][key] = final["nodes"][group][name]["stat"][key] + step * 50
                        nodes["parent"]["stat"]["usage_usec"] = final["nodes"][group]["parent"]["stat"]["usage_usec"] + 6000000 * step
                        for name, per_sample in {"a": 0, "b": 1200000, "root": 2400000, "best": 2400000}.items():
                            nodes[name]["stat"]["usage_usec"] = final["nodes"][group][name]["stat"]["usage_usec"] + per_sample * step
                    frame["placement"] = placement(("native", "oracle", "stale"), paused=True)
                put(directory / "mapped-idle-raw.json", idle)
                put(directory / "root-progress.json", {"full_response_seconds": [1, 1, 1], "idle_response_seconds": [1, 1, 1]})
                put(directory / "unchanged-weights.json", {"journal_unchanged": True})
                put(directory / "measurement-scope.json", {"scope": gate.SCOPE, "daemon_exercised": True,
                    "source_revision": "revision", "run_id": directory.name, "cpu_assignment_multiset": gate.LAYOUT, "full_contention_windows": 6, "window_seconds": 60})
        manifest["evidence"][row.scenario] = paths
    return manifest


class FinalGateTests(unittest.TestCase):
    def test_complete_matrix_and_every_required_check(self):
        validate_catalog()
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            manifest = fixture(root)
            result = gate.matrix(manifest, "revision", root / "success", local=False)
            self.assertEqual(result["result"], "PASS", result)
            self.assertEqual(len(result["rows"]), len(ROWS))
            self.assertEqual(result["independent_review"], "pending")
            count = 0
            for row in ROWS:
                if not row.checks:
                    continue
                path = Path(manifest["evidence"][row.scenario][0]) / "checks.json"
                original = gate.read_json(path)
                for key in row.checks:
                    for bad in (None, "FAIL", "BLOCKED", "UNKNOWN"):
                        with self.subTest(scenario=row.scenario, key=key, bad=bad):
                            changed = dict(original)
                            if bad is None: del changed[key]
                            else: changed[key] = bad
                            put(path, changed)
                            with self.assertRaises(ValueError):
                                gate.inspect_evidence(row, path.parent, "revision", manifest["artifacts"][row.artifact])
                            count += 1
                put(path, original)
                proof = path.parent / (row.checks[0] + ".json")
                saved = proof.read_text()
                proof.unlink()
                with self.assertRaises(OSError):
                    gate.inspect_evidence(row, path.parent, "revision", manifest["artifacts"][row.artifact])
                proof.write_text(saved)
            self.assertGreater(count, 100)

    def test_failures_missing_capabilities_and_evidence_are_retained(self):
        for fault in ("missing-row", "blocked", "fail", "unknown", "wrong-kernel", "wrong-artifact", "stale", "cleanup", "source-is-not-package", "artifact-bytes", "symlink", "escape"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                manifest = fixture(root)
                name = "systemd-native-lifecycle"
                path = Path(manifest["evidence"][name][0])
                if fault == "missing-row": del manifest["evidence"][name]
                if fault in ("blocked", "fail", "unknown"):
                    value = {"blocked": "BLOCKED", "fail": "FAIL", "unknown": "WONDERFUL"}[fault]
                    (path / "result").write_text(value)
                    env = (path / "environment.txt").read_text().replace("result=PASS", "result=" + value)
                    (path / "environment.txt").write_text(env)
                if fault in ("wrong-kernel", "wrong-artifact", "stale"):
                    meta = gate.read_json(path / "environment.json")
                    meta[{"wrong-kernel": "kernel", "wrong-artifact": "tested_binary_sha256", "stale": "source_revision"}[fault]] = "wrong"
                    put(path / "environment.json", meta)
                if fault == "cleanup":
                    (path / "environment.txt").write_text((path / "environment.txt").read_text().replace("cleanup=PASS", "cleanup=FAIL"))
                if fault == "source-is-not-package":
                    path = Path(manifest["evidence"]["native-package-acceptance"][0])
                    meta = gate.read_json(path / "environment.json")
                    meta["tested_artifact"] = "source-binary, not the installed package"
                    put(path / "environment.json", meta)
                if fault == "artifact-bytes": (path / "binary").write_bytes(b"different")
                if fault == "symlink":
                    (path / "binary").unlink()
                    (path / "binary").symlink_to(root / "not-owned")
                if fault == "escape":
                    meta = gate.read_json(path / "environment.json")
                    meta["artifact_path"] = "../binary"
                    put(path / "environment.json", meta)
                result = gate.matrix(manifest, "revision", root / "output", local=False)
                self.assertEqual(result["result"], "BLOCKED" if fault in ("missing-row", "blocked") else "FAIL")
                self.assertTrue(path.exists())
                self.assertTrue((root / "output/matrix.json").exists())

    def test_raw_recomputation_scope_and_replicas_cannot_be_replaced_by_summary(self):
        for fault in ("two-replicas", "duplicate-run", "duplicate-reference", "duplicate-window", "missing-frame", "control", "scope", "host", "daemon-frame", "daemon-control", "sleeping", "affinity", "root-response"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                manifest = fixture(root)
                refs = manifest["evidence"]["systemd-native-reference-pinned-six"]
                path = Path(refs[-1])
                if fault == "two-replicas": refs.pop()
                if fault == "duplicate-reference":
                    (path / "reference-raw.json").write_bytes((Path(refs[0]) / "reference-raw.json").read_bytes())
                if fault in ("duplicate-run", "scope"):
                    scope = gate.read_json(path / "measurement-scope.json")
                    scope["run_id" if fault == "duplicate-run" else "scope"] = Path(refs[0]).name if fault == "duplicate-run" else "unbound"
                    put(path / "measurement-scope.json", scope)
                if fault == "host":
                    meta = gate.read_json(path / "environment.json")
                    meta["host_identity"] = "another-host"
                    put(path / "environment.json", meta)
                if fault in ("missing-frame", "control"):
                    raw = gate.read_json(path / "reference-raw.json")
                    if fault == "missing-frame": raw.pop()
                    else:
                        for frame in raw: frame["nodes"]["stale"] = copy.deepcopy(frame["nodes"]["referencea"])
                    put(path / "reference-raw.json", raw)
                daemon = Path(manifest["evidence"]["systemd-native-proportional"][0])
                if fault == "duplicate-window":
                    (daemon / "full-contention-4-raw.json").write_bytes((daemon / "full-contention-0-raw.json").read_bytes())
                if fault in ("daemon-frame", "daemon-control", "sleeping", "affinity"):
                    raw = gate.read_json(daemon / "full-contention-4-raw.json")
                    if fault == "daemon-frame": raw.pop()
                    elif fault == "daemon-control":
                        for frame in raw: frame["nodes"]["stale"] = copy.deepcopy(frame["nodes"]["native"])
                    elif fault == "sleeping": raw[6]["placement"]["native/a"]["0"]["state"] = "S"
                    else: raw[6]["placement"]["native/a"]["0"]["affinity"] = [0, 1]
                    put(daemon / "full-contention-4-raw.json", raw)
                if fault == "root-response":
                    put(daemon / "root-progress.json", {"full_response_seconds": [1, 4, 1], "idle_response_seconds": [1, 1, 1]})
                result = gate.matrix(manifest, "revision", root / "output", local=False)
                self.assertEqual(result["result"], "FAIL", result)

    def test_manifest_inventory_and_zero_test_command_fail_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            manifest = fixture(root)
            manifest["evidence"]["obsolete-observation-only"] = []
            with self.assertRaises(ValueError): gate.matrix(manifest, "revision", root / "output", local=False)
            with patch("gate.subprocess.run", return_value=subprocess.CompletedProcess([], 0)):
                with self.assertRaisesRegex(ValueError, "zero tests"):
                    gate.run_local(root, ["fake-go"])
            with self.assertRaises(ValueError): gate.read_json(self.write(root / "duplicate.json", '{"a": 1, "a": 2}'))

    @staticmethod
    def write(path, value):
        path.write_text(value)
        return path


if __name__ == "__main__":
    unittest.main()
