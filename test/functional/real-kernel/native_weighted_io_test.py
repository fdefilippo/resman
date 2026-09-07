#!/usr/bin/env python3
"""Adapter scope, evidence and bounded cleanup regressions without host mutation."""
import copy
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import native_weighted_io as weighted


def fixture(directory, metadata):
    def put(name, value):
        (directory / name).write_text(json.dumps(value))
    uid_list = [1006, 1007]
    put("weighted-io-scope.json", {"scope": weighted.SCOPE, "daemon_exercised": False,
        "adapter_exercised": True, "source_revision": metadata["source_revision"],
        "run_id": metadata["run_id"], "probe_sha256": metadata["tested_binary_sha256"]})
    put("owned-null-block-device.json", {"name": "resmanweight" + weighted.re.sub("[^a-z0-9]", "", metadata["run_id"]),
        "major_minor": "250:1", "created_by_run": True})
    put("weighted-io-pam-sessions.json", {str(uid): {"cgroup": "0::/user.slice/user-%d.slice/session-42.scope" % uid}
                                         for uid in uid_list})
    phases = []
    def raw(uid, operation, weight):
        return {"scope": weighted.PROBE_SCOPE, "operation": operation, "requested_io_weight": weight,
            "after_io_weight": weight, "expected_bfq_weight": 100 if weight == 100 else 300,
            "identity": {"Name": "user-%d.slice" % uid, "ControlGroupID": uid, "InvocationID": [1] * 16},
            "after_kernel": {"io.weight": None, "io.bfq.weight": "default " + str(100 if weight == 100 else 300) + "\n"},
            "mutable_paths": ["/run/systemd/system.control/user-%d.slice.d/50-IOWeight.conf" % uid],
            "journal": {"units": [{"unit": "user-%d.slice" % uid, "phase": "applied", "properties": [
                {"property": "IOWeight", "last_applied": weight, "uncertain": False}]}]}}
    for index, name in enumerate(("bfq-equal", "bfq-weighted", "non-bfq")):
        scheduler = "none [bfq]" if index < 2 else "[none] bfq"
        phase = {"name": name, "device": "250:1", "scheduler_before": scheduler, "scheduler_after": scheduler,
                 "effective_weight_capability": index < 2, "io_cost_qos_before": None,
                 "io_cost_qos_after": None, "operations": []}
        for uid, weight in zip(uid_list, [100, 100] if index == 0 else [100, 2300]):
            name = "probe-%d-apply.json" % (index * 2 + uid)
            put(name, raw(uid, "apply", weight))
            phase["operations"].append({"uid": uid, "file": name})
        phases.append(phase)
    put("adapter-weight-roundtrip.json", {"phases": phases})
    put("bfq-device-capability.json", phases[1])
    put("non-bfq-negative-capability.json", phases[2])
    restores = {}
    for uid in uid_list:
        value = raw(uid, "restore", 100)
        value.update(mutable_paths=[], journal={"units": []})
        name = "probe-%d-restore.json" % uid
        put(name, value)
        baseline_file = "probe-%d-recover.json" % uid
        value["operation"] = "recover"
        put(baseline_file, value)
        restores[str(uid)] = {"uid": uid, "file": name, "baseline_file": baseline_file,
                              "baseline": 100, "journal_absent": True}
    put("exact-weight-restoration.json", restores)
    put("owned-device-cleanup.json", {"device_absent": True, "config_absent": True,
        "system_schedulers_before": {"/sys/block/nullb0/queue/scheduler": "[none]"},
        "system_schedulers_after": {"/sys/block/nullb0/queue/scheduler": "[none]"},
        "module_preexisting": True, "module_present": True})
    for name in ("equal", "weighted"):
        before = {"time_ns": 1, "leaves": [{"identity": uid, "io_stat": "250:1 rbytes=100 rios=1"} for uid in uid_list]}
        after = {"time_ns": 20000000001, "leaves": [{"identity": uid, "io_stat": "250:1 rbytes=200 rios=2"} for uid in uid_list]}
        put("traffic-" + name + ".json", {"result": "CHARACTERIZATION", "device": "250:1", "before": before,
            "after": after, "interval": weighted.summarize(before, after, "250:1")})


class WeightedIOTests(unittest.TestCase):
    def test_scope_and_every_raw_boundary_are_required(self):
        cases = [
            ("weighted-io-scope.json", lambda x: x.update(daemon_exercised=True)),
            ("weighted-io-scope.json", lambda x: x.update(adapter_exercised=False)),
            ("weighted-io-scope.json", lambda x: x.update(probe_sha256="other")),
            ("owned-null-block-device.json", lambda x: x.update(created_by_run=False)),
            ("owned-null-block-device.json", lambda x: x.update(name="nullb0")),
            ("adapter-weight-roundtrip.json", lambda x: x["phases"].pop()),
            ("adapter-weight-roundtrip.json", lambda x: x["phases"][2].update(effective_weight_capability=True)),
            ("adapter-weight-roundtrip.json", lambda x: x["phases"][2].update(io_cost_qos_after="250:1 enable=1")),
            ("probe-1009-apply.json", lambda x: x.update(after_io_weight=100)),
            ("probe-1009-apply.json", lambda x: x["after_kernel"].update({"io.bfq.weight": "default 100"})),
            ("probe-1009-apply.json", lambda x: x["journal"]["units"][0]["properties"][0].update(uncertain=True)),
            ("probe-1009-apply.json", lambda x: x["identity"].update(ControlGroupID=999)),
            ("probe-1006-restore.json", lambda x: x.update(after_io_weight=2300)),
            ("probe-1006-recover.json", lambda x: x.update(after_io_weight=2300)),
            ("exact-weight-restoration.json", lambda x: x["1006"].update(baseline_file="../outside.json")),
            ("exact-weight-restoration.json", lambda x: x.update({"1007": x["1006"]})),
            ("owned-device-cleanup.json", lambda x: x.update(module_present=False)),
            ("owned-device-cleanup.json", lambda x: x.update(system_schedulers_after={})),
            ("traffic-equal.json", lambda x: x.update(result="PASS")),
            ("traffic-weighted.json", lambda x: x["interval"].update(read_share_percent=[25, 75])),
        ]
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            metadata = {"run_id": "runit", "source_revision": "rev", "tested_binary_sha256": "sha"}
            fixture(directory, metadata)
            read = lambda name: json.loads((directory / name).read_text())
            self.assertFalse(weighted.validate_proof(read, metadata)["daemon_exercised"])
            for filename, change in cases:
                with self.subTest(filename=filename, mutation=change):
                    old = read(filename)
                    new = copy.deepcopy(old)
                    change(new)
                    (directory / filename).write_text(json.dumps(new))
                    with self.assertRaises((ValueError, KeyError)):
                        weighted.validate_proof(read, metadata)
                    (directory / filename).write_text(json.dumps(old))

    def test_negative_capability_is_not_inferred_from_a_file(self):
        self.assertTrue(weighted.iocost_disabled(None, "250:1"))
        self.assertTrue(weighted.iocost_disabled("250:2 enable=1", "250:1"))
        self.assertTrue(weighted.iocost_disabled("250:1 enable=0", "250:1"))
        self.assertFalse(weighted.iocost_disabled("250:1 enable=1", "250:1"))
        self.assertFalse(weighted.iocost_disabled("250:1 model=linear", "250:1"))
        with self.assertRaises(ValueError):
            weighted.selected_scheduler("bfq none")

    def test_cleanup_runs_all_stages_even_after_restore_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            gate = weighted.WeightedIOGate(tmp, "runit", "rev")
            gate.owned_host = True
            with patch.object(gate, "restore_weights", side_effect=RuntimeError("restore failed")), \
                    patch.object(gate, "cleanup_sessions") as sessions, \
                    patch.object(gate, "cleanup_device") as device:
                with self.assertRaisesRegex(AssertionError, "restore failed"):
                    gate.cleanup()
                sessions.assert_called_once()
                device.assert_called_once()

    def test_partial_apply_still_restores_the_previously_recorded_baseline(self):
        with tempfile.TemporaryDirectory() as tmp:
            gate = weighted.WeightedIOGate(tmp, "runit", "rev")
            gate.accounts = [SimpleNamespace(pw_uid=1006)]
            gate.baselines[1006], gate.baseline_files[1006] = 100, "initial.json"
            with patch.object(gate, "operation", return_value=({"file": "restore.json"},
                    {"after_io_weight": 100, "mutable_paths": []})) as operation:
                gate.restore_weights()
                operation.assert_called_once_with(1006, "restore")

    def test_probe_zero_test_selection_is_not_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            gate = weighted.WeightedIOGate(tmp, "runit", "rev")
            gate.dev = "250:1"
            with patch.object(weighted.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout="PASS\n")):
                with self.assertRaisesRegex(AssertionError, "did not execute"):
                    gate.operation(1006, "apply", 100)

    def test_preflight_block_and_interruption_preserve_failure_evidence(self):
        for error, expected in ((weighted.Blocked("capability missing"), 77), (RuntimeError("interrupted"), 1)):
            with tempfile.TemporaryDirectory() as tmp:
                gate = weighted.WeightedIOGate(tmp, "runit", "rev")
                with patch.object(gate, "preflight", side_effect=error), patch.object(gate, "cleanup") as cleanup, \
                        contextlib.redirect_stderr(io.StringIO()):
                    self.assertEqual(gate.run(), expected)
                    cleanup.assert_called_once()
                    self.assertIn(str(error), (gate.evidence / "environment.txt").read_text())

    def test_dispatch_uses_the_dedicated_driver_and_cli_installs_signal_handlers(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            runner = Path(__file__).with_name("native-run.sh")
            (directory / "native-run.sh").write_text(runner.read_text())
            (directory / "native_weighted_io.py").write_text("import sys\nprint('|'.join(sys.argv[1:]))\n")
            result = subprocess.run(["bash", str(directory / "native-run.sh"), weighted.SCENARIO, "runit", "rev"],
                                    text=True, stdout=subprocess.PIPE, check=True)
            self.assertEqual(result.stdout.strip(), weighted.SCENARIO + "|runit|rev")
        with patch.object(weighted, "WeightedIOGate") as cls, patch.object(weighted.signal, "signal") as signal:
            cls.return_value.run.return_value = 77
            self.assertEqual(weighted.main([weighted.SCENARIO, "runit", "rev"]), 77)
            self.assertEqual(signal.call_count, 2)
            with self.assertRaisesRegex(RuntimeError, "interrupted"):
                signal.call_args.args[1](None, None)


if __name__ == "__main__":
    unittest.main()
