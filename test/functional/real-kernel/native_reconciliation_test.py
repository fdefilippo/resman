#!/usr/bin/env python3
"""Deterministic reconciliation fixture guards; no host mutation is performed."""
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

from native_gate import Blocked
from native_reconciliation import ReconciliationGate, REQUIRED_CHECKS, atomic_write, expected_weights, journal_advanced, online_set, truthful_row, unused_slice


class ReconciliationTests(unittest.TestCase):
    def test_real_concurrent_phase_executes_both_workers_and_propagates_failure(self):
        def snapshot(epoch):
            return {"history": {"sample_epoch_id": epoch}, "journal": {"generation": epoch},
                    "kernel": {"weights": {str(uid): "100" for uid in (0, 1006, 1007, 1008)}},
                    "main_sha256": str(epoch), "map_sha256": str(epoch)}
        for failed_worker in (False, True):
            with self.subTest(failure=failed_worker), tempfile.TemporaryDirectory() as directory:
                gate = ReconciliationGate(directory, "rtest", "revision")
                gate.accounts = [Mock(pw_uid=uid) for uid in (1006, 1007, 1008)]
                gate.candidate_uids = [60000]
                def publish(points, _best_effort):
                    if failed_worker and points == (350, 250):
                        raise RuntimeError("concurrent worker failed")
                with patch.object(gate, "stable", side_effect=[snapshot(n) for n in (1, 2, 4, 5, 6, 7)]), \
                        patch.object(gate, "reload_time", side_effect=[1, 2, 2]), \
                        patch.object(gate, "publish_policy", side_effect=publish) as policies, \
                        patch.object(gate, "pam_turnover", return_value={"login": snapshot(3)}), \
                        patch.object(gate, "create_probe") as create, patch.object(gate, "remove_probe"), \
                        patch.object(gate, "history", return_value=[{"denominator_state": "complete",
                            "programmed_sibling_weight_sum": 100, "observed_sibling_weight_sum": 100}]):
                    if failed_worker:
                        with self.assertRaisesRegex(RuntimeError, "concurrent worker failed"):
                            gate.reload_and_turnover()
                        self.assertNotIn("concurrent-reconciliation", gate.reconcile_checks)
                    else:
                        gate.reload_and_turnover()
                        self.assertEqual(gate.reconcile_checks["concurrent-reconciliation"], "PASS")
                    self.assertEqual(policies.call_count, 2)
                    self.assertEqual(create.call_count, 2)

    def test_probe_reconfirms_synthetic_scope_before_starting_service(self):
        base = "LoadState=loaded\nActiveState=inactive\nControlGroup=\nFragmentPath=\nDropInPaths=/usr/lib/systemd/system/user-.slice.d/10-defaults.conf\n"
        for configured, populated in ((False, False), (True, False), (False, True)):
            with self.subTest(configured=configured, populated=populated), tempfile.TemporaryDirectory() as directory:
                gate = ReconciliationGate(directory, "rtest", "revision")
                gate.candidate_uids = [60000]
                leaf = Path(directory) / "leaf"
                if populated: leaf.mkdir()
                info = base.replace("/usr/lib/systemd/system/user-.slice.d/10-defaults.conf", "/etc/operator.conf") if configured else base
                command = Mock(side_effect=[Mock(stdout=info), Mock(stdout="LoadState=not-found"), Mock(), Mock(stdout="invocation")])
                with patch.object(gate, "active_uids", side_effect=[set(), {60000}]), \
                        patch.object(gate, "slice", return_value=leaf), patch.object(gate, "command", command):
                    if configured or populated:
                        with self.assertRaises(AssertionError): gate.create_probe(60000)
                        self.assertEqual(command.call_count, 1)
                    else:
                        gate.create_probe(60000)
                        self.assertEqual(command.call_args_list[2].args[0], "systemd-run")
                        self.assertEqual(gate.probes[60000]["invocation"], "invocation")

    def test_unused_synthetic_slice_preserves_only_generic_distribution_defaults(self):
        base = {"LoadState": "loaded", "ActiveState": "inactive", "ControlGroup": "",
                "FragmentPath": "", "DropInPaths": "/usr/lib/systemd/system/user-.slice.d/10-defaults.conf"}
        def text(values):
            return "\n".join(key + "=" + value for key, value in values.items())
        self.assertTrue(unused_slice(text(base)))
        self.assertTrue(unused_slice(text(dict(base, LoadState="not-found", DropInPaths=""))))
        for key, value in (("LoadState", "error"), ("ActiveState", "active"), ("ActiveState", "failed"),
                           ("ControlGroup", "/user.slice/user-60000.slice"), ("FragmentPath", "/etc/custom.slice"),
                           ("DropInPaths", "/etc/systemd/system/user-.slice.d/10-defaults.conf"),
                           ("DropInPaths", "/usr/lib/systemd/system/user-60000.slice.d/10-defaults.conf"),
                           ("DropInPaths", base["DropInPaths"] + " /run/systemd/system.control/user-60000.slice.d/50-CPUWeight.conf")):
            with self.subTest(key=key, value=value):
                self.assertFalse(unused_slice(text(dict(base, **{key: value}))))
        for key in base:
            self.assertFalse(unused_slice(text({k: v for k, v in base.items() if k != key})))
        self.assertFalse(unused_slice(text(base) + "\nActiveState=inactive"))

    def test_expected_plan_includes_root_and_partitions_excluded_siblings(self):
        weights = expected_weights({1006: 400, 1007: 200}, 100, 80, {0, 1006, 1007, 1008, 60000})
        self.assertEqual(weights, {0: 2500, 1006: 10000, 1007: 5000, 1008: 1000, 60000: 1000})
        valid = expected_weights({1006: 600, 1007: 100}, 100, 1, {0, 1006, 1007, *range(60000, 60016)})
        self.assertTrue(all(valid[uid] == 1 for uid in range(60000, 60016)))
        with self.assertRaises(AssertionError):
            expected_weights({1006: 600, 1007: 100}, 100, 1, {0, 1006, 1007, *range(60000, 60017)})

    def test_complete_history_requires_the_measured_denominator(self):
        for state, programmed, observed, valid in (
                ("complete", 123, 123, True), ("complete", 123, 122, False),
                ("complete", None, None, False), ("incomplete", 123, None, True),
                ("unavailable", None, None, True), ("made-up", 1, 1, False)):
            row = {"denominator_state": state, "programmed_sibling_weight_sum": programmed, "observed_sibling_weight_sum": observed}
            with self.subTest(row=row):
                if valid: truthful_row(row)
                else:
                    with self.assertRaises(AssertionError): truthful_row(row)
        journal_advanced({"journal": {"generation": 4}}, {"journal": {"generation": 5}})
        for value in (4, 3, None):
            with self.assertRaises(AssertionError):
                journal_advanced({"journal": {"generation": 4}}, {"journal": {"generation": value}})

    def test_atomic_policy_files_are_private_and_replaced(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "map"
            atomic_write(path, "first")
            first = path.stat().st_ino
            atomic_write(path, "second")
            self.assertNotEqual(path.stat().st_ino, first)
            self.assertEqual(path.read_text(), "second")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertFalse(path.with_name("map.candidate").exists())

    def test_hotplug_is_not_implicit_and_online_parser_is_exact(self):
        self.assertEqual(online_set("0-3,6,8-9"), {0, 1, 2, 3, 6, 8, 9})
        with self.assertRaises(AssertionError): online_set("3-1")
        with tempfile.TemporaryDirectory() as directory:
            gate = ReconciliationGate(directory, "rtest", "revision")
            with patch.object(Path, "write_text") as write, self.assertRaisesRegex(Blocked, "explicit"):
                gate.hotplug()
            write.assert_not_called()

    def test_opted_in_hotplug_changes_only_highest_nonzero_cpu_and_restores_after_failure(self):
        for failure in (False, True):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                gate = ReconciliationGate(directory, "rtest", "revision")
                gate.hotplug_original = {0, 1, 2, 3}
                state, writes = {"online": True}, []
                original_write = Path.write_text
                def read(path, fallback=None):
                    if str(path).endswith("allow-cpu-hotplug"): return "yes"
                    if str(path) == "/sys/devices/system/cpu/online": return "0-3" if state["online"] else "0-2"
                    raise AssertionError("unexpected read: " + str(path))
                def write(path, text, *args, **kwargs):
                    if str(path).startswith("/sys/"):
                        self.assertEqual(str(path), "/sys/devices/system/cpu/cpu3/online")
                        writes.append(text)
                        state["online"] = text.strip() == "1"
                        return len(text)
                    return original_write(path, text, *args, **kwargs)
                snapshots = [{"history": {"sample_epoch_id": 1}},
                             RuntimeError("interrupted after offline") if failure else {"history": {"sample_epoch_id": 2, "online_cpus": 3}},
                             {"history": {"sample_epoch_id": 3}}]
                with patch("native_reconciliation.field", read), patch.object(Path, "is_file", return_value=True), \
                        patch("native_reconciliation.os.access", return_value=True), patch.object(Path, "write_text", write), \
                        patch.object(gate, "stable", side_effect=snapshots), patch("native_reconciliation.NativeGate.cleanup"):
                    if failure:
                        with self.assertRaisesRegex(RuntimeError, "interrupted"): gate.hotplug()
                        gate.cleanup()
                    else: gate.hotplug()
                self.assertEqual(writes, ["0\n", "1\n"])
                self.assertTrue(state["online"])
                self.assertIsNone(gate.hotplug_cpu)
                self.assertEqual(gate.reconcile_checks.get("online-cpu-change"), None if failure else "PASS")

    def test_each_missing_proof_failure_and_cancellation_runs_cleanup(self):
        for mutation in ("none", "blocked", "cancelled", "cleanup-failure", *sorted(REQUIRED_CHECKS)):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                gate = ReconciliationGate(directory, "rtest", "revision")
                def phase(*names):
                    def action():
                        for name in names:
                            if name != mutation: gate.record(name, {"measured": True})
                    return action
                hotplug = phase("online-cpu-change")
                if mutation in ("blocked", "cancelled"):
                    def hotplug():
                        if mutation == "blocked": raise Blocked("explicit hotplug approval is absent")
                        raise RuntimeError("cancelled")
                with contextlib.ExitStack() as stack:
                    for name in ("preflight", "validate", "write_config", "start_sessions", "start_daemon"):
                        stack.enter_context(patch.object(gate, name))
                    stack.enter_context(patch.object(gate, "reload_and_turnover", phase("policy-reload", "topology-turnover", "concurrent-reconciliation")))
                    stack.enter_context(patch.object(gate, "cardinality", phase("cardinality-refusal")))
                    stack.enter_context(patch.object(gate, "hotplug", hotplug))
                    cleanup = stack.enter_context(patch.object(gate, "cleanup", side_effect=RuntimeError("cleanup") if mutation == "cleanup-failure" else None))
                    stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
                    stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
                    status = gate.run()
                self.assertEqual(status, 0 if mutation == "none" else 77 if mutation == "blocked" else 1)
                cleanup.assert_called_once()
                recorded = json.loads((gate.evidence / "checks.json").read_text())
                self.assertEqual(set(recorded), REQUIRED_CHECKS)
                self.assertEqual((gate.evidence / "result").read_text().strip(), "PASS" if mutation == "none" else "BLOCKED" if mutation == "blocked" else "FAIL")

    def test_replaced_service_is_not_stopped(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = ReconciliationGate(directory, "rtest", "revision")
            gate.probes[60000] = {"service": "run-owned.service", "invocation": "old"}
            class Result:
                stdout = "new\n"
            with patch.object(gate, "command", return_value=Result()) as command, self.assertRaisesRegex(AssertionError, "replaced"):
                gate.remove_probe(60000)
            self.assertEqual(command.call_count, 1)

    def test_subprocess_dispatch_and_signal_handlers_before_any_host_operation(self):
        script = str(Path(__file__).with_name("native_reconciliation.py"))
        # A trace boundary intercepts the first run() call before its body can
        # inspect or mutate the host. The real __main__ argument and signal
        # wiring is still executed by an independent Python process.
        wrapper = r'''
import json, runpy, signal, sys
def boundary(frame, event, arg):
    if event == "call" and frame.f_code.co_name == "run" and frame.f_globals.get("__name__") == "__main__":
        gate = frame.f_locals["self"]
        handled = []
        for sig in (signal.SIGINT, signal.SIGTERM):
            try: signal.getsignal(sig)(sig, None)
            except RuntimeError: handled.append(sig)
        print(json.dumps({"bundle": str(gate.bundle), "run_id": gate.run_id, "revision": gate.revision, "signals": handled}))
        raise SystemExit(0)
    return boundary
script = sys.argv[1]
sys.argv = [script, "systemd-native-reconciliation", "rdispatch", "revision"]
sys.settrace(boundary)
runpy.run_path(script, run_name="__main__")
'''
        # Copy only the entry point into an isolated bundle so its constructor
        # cannot create a work directory in the repository evidence directory.
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "native_reconciliation.py"
            target.write_bytes(Path(script).read_bytes())
            result = subprocess.run([sys.executable, "-c", "import sys; sys.path.insert(0, %r);\n%s" % (str(Path(script).parent), wrapper), str(target)],
                                    text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            observed = json.loads(result.stdout)
            self.assertEqual(observed["bundle"], directory)
            self.assertEqual(observed["run_id"], "rdispatch")
            self.assertEqual(observed["revision"], "revision")
            self.assertEqual(len(observed["signals"]), 2)

    def test_shell_dispatch_and_explicit_hotplug_marker(self):
        scripts = Path(__file__).parent
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "native-run.sh").write_bytes((scripts / "native-run.sh").read_bytes())
            (root / "native_reconciliation.py").write_text("import json, sys; print(json.dumps(sys.argv))\n")
            dispatched = subprocess.run(["bash", str(root / "native-run.sh"), "systemd-native-reconciliation", "rdispatch", "revision"],
                                        text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
            self.assertEqual(dispatched.returncode, 0, dispatched.stderr)
            self.assertEqual(json.loads(dispatched.stdout)[1:], ["systemd-native-reconciliation", "rdispatch", "revision"])
            for intent in ("0", "1", "true", "2"):
                bundle = root / intent
                bundle.mkdir()
                result = subprocess.run(["bash", "-c", 'source "$1"; write_hotplug_opt_in "$2" "$3"', "test",
                                         str(scripts / "remote.sh"), str(bundle), intent],
                                        env=dict(os.environ, RESMAN_REAL_KERNEL_REMOTE_LIBRARY_ONLY="1"),
                                        text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
                self.assertEqual(result.returncode, 0 if intent in ("0", "1") else 2)
                self.assertEqual((bundle / "allow-cpu-hotplug").exists(), intent == "1")
                if intent in ("0", "1"):
                    self.assertEqual((bundle / "hotplug-intent").read_text().strip(), intent)


if __name__ == "__main__":
    unittest.main()
