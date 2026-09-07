#!/usr/bin/env python3
"""Fail-closed checks for the native coverage fixture, without host mutations."""
import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from native_coverage import (CoverageGate, REQUIRED_CHECKS, pam_identity, result_for,
                             validate_container_observation, io_values, verify_io_interval,
                             IO_DIMENSIONS, io_measurement, verify_io_dimension)
from native_gate import Blocked


class CoverageTests(unittest.TestCase):
    def test_sudoers_collision_never_becomes_owned_or_deleted(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            path = Path(directory) / "operator-sudoers"
            path.write_text("operator rule\n")
            with self.assertRaises(FileExistsError):
                gate.create_sudoers(path, "fixture rule\n")
            self.assertIsNone(gate.sudoers)
            with patch.object(gate, "cleanup_nested"), patch("native_gate.NativeGate.cleanup"):
                gate.cleanup()
            self.assertEqual(path.read_text(), "operator rule\n")

    def test_sudoers_cleanup_checks_both_inode_and_content(self):
        for mutation in ("none", "content", "identity", "symlink"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                gate = CoverageGate(directory, "runit", "revision")
                path = Path(directory) / "sudoers"
                gate.create_sudoers(path, "fixture rule\n")
                if mutation == "content":
                    path.chmod(0o600)
                    path.write_text("operator rule\n")
                elif mutation == "identity":
                    replacement = Path(directory) / "replacement"
                    replacement.write_text("fixture rule\n")
                    replacement.replace(path)
                elif mutation == "symlink":
                    target = Path(directory) / "target"
                    target.write_text("fixture rule\n")
                    path.unlink()
                    path.symlink_to(target)
                if mutation == "none":
                    gate.remove_sudoers()
                    self.assertFalse(path.exists())
                    gate.remove_sudoers()
                else:
                    with self.assertRaises(AssertionError):
                        gate.remove_sudoers()
                    self.assertTrue(path.exists())

    def test_each_io_dimension_disables_every_competing_cap(self):
        for dimension in IO_DIMENSIONS:
            with self.subTest(dimension=dimension), tempfile.TemporaryDirectory() as directory:
                gate = CoverageGate(directory, "runit", "revision")
                gate.config.write_text("IO_READ_BPS=100M\nIO_WRITE_BPS=50M\nIO_READ_IOPS=1000\nIO_WRITE_IOPS=500\n")
                with patch.object(gate, "write_config"):
                    expected = gate.configure_io_dimension(dimension)
                values = dict(line.split("=", 1) for line in gate.config.read_text().splitlines())
                for candidate, (key, cap) in IO_DIMENSIONS.items():
                    self.assertEqual(values[key], str(cap) if candidate == dimension else "0")
                    self.assertEqual(expected[candidate], str(cap) if candidate == dimension else "max")

    def test_io_rates_use_the_measured_device_operation_counter_not_dd_count(self):
        before = "8:0 rbytes=1000 wbytes=1000 rios=10 wios=10"
        after = "8:0 rbytes=2024 wbytes=2024 rios=18 wios=14"
        for dimension, expected in (("rbps", 512), ("wbps", 512), ("riops", 4), ("wiops", 2)):
            with self.subTest(dimension=dimension):
                self.assertEqual(io_measurement(before, after, 1024, 2, dimension)["rate"], expected)
        for dimension in IO_DIMENSIONS:
            with self.subTest(dimension=dimension):
                with self.assertRaises(AssertionError):
                    io_measurement(after, before, 1024, 2, dimension)
                with self.assertRaises(Blocked):
                    io_measurement(before, "253:0 rbytes=2024 rios=18", 1024, 2, dimension)

    def test_every_isolated_cap_needs_discriminating_control_and_bounded_delivery(self):
        for dimension, (_key, cap) in IO_DIMENSIONS.items():
            with self.subTest(dimension=dimension):
                verify_io_dimension(dimension, {"uncapped": {"rate": 2 * cap}, "capped": {"rate": cap}})
                with self.assertRaises(Blocked):
                    verify_io_dimension(dimension, {"uncapped": {"rate": cap}, "capped": {"rate": cap}})
                with self.assertRaises(AssertionError):
                    verify_io_dimension(dimension, {"uncapped": {"rate": 2 * cap}, "capped": {"rate": 2 * cap}})

    def test_hard_io_runs_both_phases_for_all_four_dimensions_before_pass(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
            gate = CoverageGate(directory, "runit", "revision")
            gate.helper = Path(directory) / "helper"
            user = SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid())
            gate.accounts = [user]
            (gate.helper / str(user.pw_uid)).mkdir(parents=True)
            active, measured = {"dimension": "rbps", "phase": "uncapped"}, []
            def command(*args, **_kwargs):
                if args[0] == "dd":
                    (gate.helper / str(user.pw_uid) / "direct-io.bin").touch()
                if args[0] == "systemd-run":
                    unit = next(arg for arg in args if str(arg).startswith("--unit="))
                    active["phase"] = "uncapped" if "-uncapped.service" in unit else "capped"
                return SimpleNamespace(returncode=0, stdout="")
            def configure(dimension):
                active["dimension"] = dimension
                return {key: str(cap) if key == dimension else "max" for key, (_, cap) in IO_DIMENSIONS.items()}
            def field_value(*_args):
                return "8:0 " + " ".join(key + "=" + (str(cap) if key == active["dimension"] else "max")
                                           for key, (_, cap) in IO_DIMENSIONS.items())
            def measured_interval(_before, _after, _size, _seconds, dimension):
                measured.append((dimension, active["phase"]))
                return {"rate": IO_DIMENSIONS[dimension][1] * (2 if active["phase"] == "uncapped" else 1)}
            for method in ("stop_daemon", "assert_released", "write_config", "start_daemon", "assert_applied", "resources"):
                stack.enter_context(patch.object(gate, method))
            stack.enter_context(patch.object(gate, "command", side_effect=command))
            stack.enter_context(patch.object(gate, "backing_devices", return_value={"8:0"}))
            stack.enter_context(patch.object(gate, "configure_io_dimension", side_effect=configure))
            stack.enter_context(patch("native_coverage.field", side_effect=field_value))
            stack.enter_context(patch("native_coverage.io_measurement", side_effect=measured_interval))
            gate.hard_io()
            self.assertEqual(measured, [(dimension, phase) for dimension in IO_DIMENSIONS for phase in ("uncapped", "capped")])
            proof = json.loads((gate.evidence / "hard-io-delivery.json").read_text())
            self.assertEqual(set(proof["dimensions"]), {"rbps", "wbps", "riops", "wiops"})
            self.assertEqual(gate.checks["hard-io-delivery"], "PASS")

    def test_payload_snapshot_has_real_identity_and_unavailable_not_zero(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            root = Path(directory) / "cgroup"
            node = root / "payload"
            node.mkdir(parents=True)
            (node / "cpu.weight").write_text("100\n")
            (node / "io.max").write_text("")
            with patch("native_coverage.Path", side_effect=lambda value: root if value == "/sys/fs/cgroup" else Path(value)):
                snapshot = gate.payload_limits(["0::/payload"])["0::/payload"]
            self.assertEqual(snapshot["identity"], [node.stat().st_dev, node.stat().st_ino])
            self.assertEqual(snapshot["limits"]["cpu.weight"], {"available": True, "value": "100"})
            self.assertEqual(snapshot["limits"]["io.max"], {"available": True, "value": ""})
            self.assertEqual(snapshot["limits"]["memory.max"], {"available": False})

    def test_payload_changes_during_restore_or_reapply_are_rejected(self):
        baseline = {"0::/payload": {"identity": [4, 20], "limits": {"cpu.weight": {"available": True, "value": "100"}}}}
        for phase in (1, 2):
            for change in ("identity", "value", "availability"):
                with self.subTest(phase=phase, change=change), tempfile.TemporaryDirectory() as directory:
                    gate = CoverageGate(directory, "runit", "revision")
                    changed = json.loads(json.dumps(baseline))
                    if change == "identity":
                        changed["0::/payload"]["identity"] = [4, 21]
                    elif change == "value":
                        changed["0::/payload"]["limits"]["cpu.weight"]["value"] = "200"
                    else:
                        changed["0::/payload"]["limits"]["cpu.weight"] = {"available": False}
                    sequence = [baseline, baseline, baseline]
                    sequence[phase] = changed
                    with contextlib.ExitStack() as stack:
                        for method in ("stop_daemon", "assert_released", "start_daemon", "assert_applied", "refused"):
                            stack.enter_context(patch.object(gate, method))
                        stack.enter_context(patch("native_coverage.field", return_value="log"))
                        stack.enter_context(patch.object(gate, "payload_limits", side_effect=sequence))
                        with self.assertRaisesRegex(AssertionError, "payload limits or identity changed"):
                            gate.verify_payload_restoration_cycle(["0::/payload"], 1006, "authority_split", "test")

    def test_every_required_check_is_independently_required(self):
        complete = dict.fromkeys(REQUIRED_CHECKS, "PASS")
        self.assertEqual(result_for(complete), "PASS")
        for name in REQUIRED_CHECKS:
            for status in ("FAIL", "BLOCKED"):
                with self.subTest(check=name, status=status):
                    self.assertEqual(result_for(dict(complete, **{name: status})), status)
            missing = complete.copy()
            del missing[name]
            with self.assertRaises(AssertionError):
                result_for(missing)
        with self.assertRaises(AssertionError):
            result_for(dict(complete, decorative="PASS"))
        with self.assertRaises(AssertionError):
            result_for(dict.fromkeys(REQUIRED_CHECKS, "SKIP"))

    def test_ssh_needs_correct_uid_scope_and_logind_owner(self):
        original = {"uid": 1006, "cgroup": "0::/user.slice/user-1006.slice/session-42.scope", "session_user": "1006"}
        self.assertEqual(pam_identity(original, 1006), "42")
        for key, value in (("uid", 0), ("session_user", "0"), ("cgroup", "0::/system.slice/sshd.service"),
                           ("cgroup", "0::/user.slice/user-10060.slice/session-42.scope")):
            with self.subTest(key=key, value=value), self.assertRaises(AssertionError):
                pam_identity(dict(original, **{key: value}), 1006)

    def test_machine_only_observation_requires_presence_null_fields_and_unavailable_coverage(self):
        row = {"uid": 1008, "cpu_authority_coverage": "unavailable", "ram_coverage": "unavailable",
               "io_coverage": "unavailable", "cpu_weight": None, "ram_cgroup_usage_bytes": None, "cgroup_path": None}
        validate_container_observation([row], 1008, "unavailable", True)
        for key in ("cpu_weight", "ram_cgroup_usage_bytes", "cgroup_path"):
            with self.subTest(key=key), self.assertRaises(AssertionError):
                validate_container_observation([dict(row, **{key: 0})], 1008, "unavailable", True)
        for key in ("cpu_authority_coverage", "ram_coverage", "io_coverage"):
            with self.subTest(key=key), self.assertRaises(AssertionError):
                validate_container_observation([dict(row, **{key: "complete"})], 1008, "unavailable", True)
        with self.assertRaises(AssertionError):
            validate_container_observation([], 1008, "unavailable", True)

    def test_capability_refusal_is_not_a_decorative_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            def unavailable():
                raise Blocked("no dedicated device")
            gate.attempt(["weighted-io-delivery"], unavailable)
            self.assertEqual(gate.checks["weighted-io-delivery"], "BLOCKED")
            evidence = json.loads((gate.evidence / "weighted-io-delivery.json").read_text())
            self.assertEqual(evidence["capability_failure"], "no dedicated device")

    def test_mid_campaign_failure_cannot_be_downgraded_to_blocked(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
            gate = CoverageGate(directory, "runit", "revision")
            for method in ("preflight", "validate", "write_config", "start_daemon", "start_sessions",
                           "assert_applied", "resources", "cleanup"):
                stack.enter_context(patch.object(gate, method))
            stack.enter_context(patch.object(gate, "ssh_sessions", side_effect=AssertionError("wrong PAM owner")))
            stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            self.assertEqual(gate.run(), 1)
            self.assertEqual((gate.evidence / "result").read_text().strip(), "FAIL")

    def test_shared_disk_scheduler_is_never_mutated(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            with patch("native_coverage.field", return_value="none [mq-deadline] bfq"), patch.object(gate, "command") as command:
                with self.assertRaises(Blocked):
                    gate.weighted_io()
                command.assert_not_called()

    def test_retained_credentials_are_redacted_without_changing_the_source_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            gate.config.write_text("MCP_AUTH_TOKEN=" + gate.mcp_token + "\n")
            from native_gate import sha
            original = sha(gate.config)
            gate.redact_credentials()
            self.assertNotIn(gate.mcp_token, gate.config.read_text())
            self.assertEqual(json.loads((gate.evidence / "config-source.json").read_text())["sha256"], original)

    def test_cleanup_reaches_daemon_even_when_each_fixture_teardown_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            gate.container_names = [("user", "container", "podman")]
            gate.podman_stores = [("user", "podman")]
            gate.machine_units = [("machine", "machine.service")]
            gate.child_units = ["child.service"]
            with patch.object(gate, "cleanup_nested", side_effect=RuntimeError("nested failure")), \
                 patch.object(gate, "ssh", side_effect=RuntimeError("container failure")), \
                 patch.object(gate, "stop_machine", side_effect=RuntimeError("machine failure")), \
                 patch.object(gate, "stop_owned_unit", side_effect=RuntimeError("child failure")), \
                 patch.object(gate, "command"), patch("native_gate.NativeGate.cleanup") as parent:
                with self.assertRaisesRegex(AssertionError, "nested failure.*container failure.*machine failure.*child failure"):
                    gate.cleanup()
                parent.assert_called_once()

    def test_machine_cleanup_is_idempotent_only_for_verified_missing_unit(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            gate.machine_units = [("machine", "machine.service")]
            def missing(*args, **_kwargs):
                return SimpleNamespace(returncode=5, stdout="not-found\n" if args[:2] == ("systemctl", "show") else "")
            with patch.object(gate, "command", side_effect=missing):
                gate.stop_machine("machine", "machine.service")
            self.assertEqual(gate.machine_units, [])
            for state in ("loaded", "error", ""):
                with patch.object(gate, "command", return_value=SimpleNamespace(returncode=1, stdout=state)):
                    with self.assertRaises(AssertionError):
                        gate.stop_owned_unit("machine.service")

    def test_resource_refusal_checks_every_original_limit(self):
        baseline = {"memory": dict.fromkeys(("memory.high", "memory.max", "memory.swap.max"), "max"),
                    "io": io_values("")}
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            gate.resource_baselines[1006] = baseline
            def once(check, message, *_args):
                if not check():
                    raise AssertionError(message)
            with patch("native_coverage.eventually", side_effect=once), \
                 patch("native_coverage.field", side_effect=lambda path, *_: "9900" if str(path).endswith("cpu.weight") else "authority_split"):
                with patch.object(gate, "resource_values", return_value=baseline):
                    gate.refused(1006, "authority_split", 0)
                for category in baseline:
                    for key in baseline[category]:
                        changed = {name: values.copy() for name, values in baseline.items()}
                        changed[category][key] = "12345"
                        with self.subTest(key=key), patch.object(gate, "resource_values", return_value=changed):
                            with self.assertRaises(AssertionError):
                                gate.refused(1006, "authority_split", 0)

    def test_shifted_absence_requires_two_fresh_database_epochs(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            def once(check, message, *_args):
                if not check():
                    raise AssertionError(message)
            with patch("native_coverage.eventually", side_effect=once):
                for epochs in ([None], [10, 10], [10, 11, 11, 11]):
                    with self.subTest(epochs=epochs), patch.object(gate, "epoch", side_effect=epochs):
                        with self.assertRaises(AssertionError):
                            gate.wait_for_fresh_epochs()
                with patch.object(gate, "epoch", side_effect=[10, 11, 11, 12, 12]):
                    self.assertEqual(gate.wait_for_fresh_epochs(), 12)

    def test_io_requires_new_device_writes_not_a_cumulative_device_name(self):
        self.assertEqual(verify_io_interval("8:0 wbytes=1000", "8:0 wbytes=2000", 1000), 1000)
        for after in ("8:0 wbytes=1000", "8:0 wbytes=999", "253:0 wbytes=2000", "8:0 wbytes=3000"):
            with self.subTest(after=after), self.assertRaises(AssertionError):
                verify_io_interval("8:0 wbytes=1000", after, 1000)

    def test_nested_identity_is_owned_before_placement_validation(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            proc = Path(directory) / "123"
            proc.mkdir()
            (proc / "stat").write_text("123 (systemd-nspawn) " + " ".join(["S"] + ["0"] * 18 + ["456"]))
            (proc / "cgroup").write_text("0::/system.slice/unexpected.service")
            with self.assertRaisesRegex(AssertionError, "genuine PAM"):
                gate.record_nested_supervisor(proc, "run-machine", 1006)
            self.assertEqual(gate.nested_processes[123]["birth"], "456")
            self.assertEqual(gate.nested_processes[123]["machine"], "run-machine")

    def test_nested_cleanup_discovers_supervisor_after_failed_readiness_and_checks_birth(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            gate.accounts = [SimpleNamespace(pw_uid=1006)]
            gate.nested_machines.add("run-machine")
            procroot = Path(directory) / "proc"
            procroot.mkdir()
            proc = procroot / "123"
            proc.mkdir()
            (proc / "stat").write_text("123 (systemd-nspawn) " + " ".join(["S"] + ["0"] * 18 + ["456"]))
            (proc / "cgroup").write_text("0::/system.slice/unexpected.service")
            (proc / "cmdline").write_bytes(b"/usr/bin/systemd-nspawn\0--machine=run-machine\0")
            def path(value):
                return procroot / str(value)[6:] if str(value).startswith("/proc/") else procroot if str(value) == "/proc" else Path(value)
            with patch("native_coverage.Path", side_effect=path), patch("native_coverage.os.kill") as kill, \
                 patch("native_coverage.eventually") as wait:
                gate.cleanup_nested()
                kill.assert_called_once_with(123, __import__("signal").SIGTERM)
                wait.assert_called_once()
                self.assertFalse(gate.nested_processes)
                gate.nested_processes[123] = {"pid": 123, "birth": "old", "machine": "run-machine"}
                kill.reset_mock()
                gate.cleanup_nested()
                kill.assert_not_called()

    def test_nested_payload_requires_live_foreign_namespace_and_exact_scope(self):
        with tempfile.TemporaryDirectory() as directory:
            gate = CoverageGate(directory, "runit", "revision")
            gate.accounts = [SimpleNamespace(pw_uid=os.getuid())]
            procroot = Path(directory) / "proc"
            proc = procroot / "123"
            (proc / "ns").mkdir(parents=True)
            (procroot / "1" / "ns").mkdir(parents=True)
            (procroot / "1" / "ns" / "pid").touch()
            (proc / "ns" / "pid").touch()
            (proc / "stat").write_text("123 (worker) " + " ".join(["R"] + ["0"] * 18 + ["456"]))
            scope = "0::/user.slice/user-1006.slice/session-42.scope"
            (proc / "cgroup").write_text(scope + "/payload/work.service")
            supervisor = {"cgroup": scope + "/supervisor"}
            real_stat = os.stat
            def stat(value, *args, **kwargs):
                if str(value) == "/proc/1/ns/pid":
                    value = procroot / "1" / "ns" / "pid"
                return real_stat(value, *args, **kwargs)
            with patch("native_coverage.Path", side_effect=lambda value: procroot if value == "/proc" else Path(value)), \
                 patch("native_coverage.os.stat", side_effect=stat):
                self.assertEqual(gate.nested_payloads(supervisor)[0]["pid"], 123)
                (proc / "cgroup").write_text(scope + "0/payload/work.service")
                self.assertEqual(gate.nested_payloads(supervisor), [])
                (proc / "cgroup").write_text(scope + "/payload/work.service")
                (proc / "stat").write_text("123 (worker) " + " ".join(["Z"] + ["0"] * 18 + ["456"]))
                with self.assertRaises(AssertionError):
                    gate.nested_payloads(supervisor)
                (proc / "stat").write_text("123 (worker) " + " ".join(["R"] + ["0"] * 18 + ["456"]))
                (proc / "ns" / "pid").unlink()
                os.link(procroot / "1" / "ns" / "pid", proc / "ns" / "pid")
                with self.assertRaises(AssertionError):
                    gate.nested_payloads(supervisor)

    def test_real_shell_dispatch_retains_cleanup_and_result_on_sigterm(self):
        source = Path(__file__).resolve().parent
        with tempfile.TemporaryDirectory() as directory:
            bundle = Path(directory)
            for name in ("native-run.sh", "native_coverage.py"):
                shutil.copyfile(source / name, bundle / name)
            # Only this subprocess gets inert preflight/cleanup replacements.
            # The real shell dispatcher, constructor, signal handler and finally
            # block still execute; no host operation is authorized by this test.
            (bundle / "sitecustomize.py").write_text(
                "import os,time\nfrom pathlib import Path\nfrom native_gate import NativeGate\n"
                "def preflight(self):\n"
                " self.save('entered', {'bundle': str(self.bundle), 'run_id': self.run_id})\n"
                " while True: time.sleep(0.05)\n"
                "def cleanup(self): self.save('cleanup-called', {'called': True})\n"
                "NativeGate.preflight=preflight\nNativeGate.cleanup=cleanup\n")
            env = dict(os.environ, PYTHONPATH=str(bundle) + os.pathsep + str(source), PYTHONDONTWRITEBYTECODE="1")
            process = subprocess.Popen(["bash", str(bundle / "native-run.sh"), "systemd-native-coverage", "runit", "test-revision"],
                                       cwd=directory, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                deadline = time.monotonic() + 5
                while not (bundle / "evidence" / "entered.json").exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(0.02)
                self.assertTrue((bundle / "evidence" / "entered.json").exists(), "real CLI did not enter bundle preflight")
                process.send_signal(signal.SIGTERM)
                stdout, stderr = process.communicate(timeout=5)
                self.assertEqual(process.returncode, 1, stdout + stderr)
                self.assertEqual((bundle / "evidence" / "result").read_text().strip(), "FAIL")
                self.assertTrue((bundle / "evidence" / "cleanup-called.json").exists())
                self.assertIn("source_revision=test-revision", (bundle / "evidence" / "environment.txt").read_text())
            finally:
                if process.poll() is None:
                    process.kill()
                    process.communicate(timeout=5)


if __name__ == "__main__":
    unittest.main()
