#!/usr/bin/env python3
"""Unit tests for the EL8 systemd 239 package boundary."""
import ast
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


directory = Path(__file__).parent
sys.path.insert(0, str(directory))
sys.path.insert(0, str(directory.parent / "real-kernel"))
spec = importlib.util.spec_from_file_location("el8_package", directory / "el8_package.py")
el8 = importlib.util.module_from_spec(spec)
spec.loader.exec_module(el8)
import native_package
from native_gate import Blocked, NativeGate


class EL8PackageContractTests(unittest.TestCase):
    def test_sqlite_paths_cross_the_el8_boundary_as_strings(self):
        expected = object()
        database_path = Path("/tmp/resman-schema-six.db")
        with patch.object(native_package.sqlite3, "connect", return_value=expected) as connect:
            observed = native_package.connect_database(database_path, uri=True)
        self.assertIs(observed, expected)
        connect.assert_called_once_with(str(database_path), uri=True)

    def test_guest_uses_the_systemd_239_kill_option(self):
        tree = ast.parse((directory.parent / "real-kernel" / "native_gate.py").read_text())
        commands = []
        for node in ast.walk(tree):
            if not (isinstance(node, ast.Call)
                    and isinstance(node.func, ast.Attribute)
                    and node.func.attr == "command"):
                continue
            arguments = [argument.value for argument in node.args
                         if isinstance(argument, ast.Constant) and isinstance(argument.value, str)]
            if arguments[:2] == ["systemctl", "kill"]:
                commands.append(arguments)
        self.assertEqual(commands, [
            ["systemctl", "kill", "--kill-who=main", "--signal=HUP"],
            ["systemctl", "kill", "--kill-who=main", "--signal=KILL"],
        ])

    def test_systemd_contract_requires_exact_major_version(self):
        self.assertEqual(el8.systemd_major_version("systemd 239 (239-82.el8)\n"), 239)
        self.assertEqual(el8.systemd_major_version("systemd 252 (252.1)\n"), 252)
        with self.assertRaisesRegex(AssertionError, "malformed"):
            el8.systemd_major_version("version unavailable")

    def test_control_group_id_absence_comes_from_interface_members(self):
        old = "ControlGroup property s emits-change\nCPUWeight property t emits-change\n"
        modern = old + "ControlGroupId property t emits-change\n"
        self.assertFalse(el8.slice_interface_has_control_group_id(old))
        self.assertTrue(el8.slice_interface_has_control_group_id(modern))
        self.assertFalse(el8.slice_interface_has_control_group_id(
            "Description property s ControlGroupId-is-not-a-member\n"))

    def test_required_boot_contract_is_explicit(self):
        self.assertEqual(el8.REQUIRED_CONTROLLERS, {"cpu", "io", "memory"})
        self.assertEqual(el8.REQUIRED_KERNEL_ARGUMENTS,
                         {"systemd.unified_cgroup_hierarchy=1", "psi=1"})

    def test_guest_programs_remain_compatible_with_el8_python(self):
        programs = (
            directory / "el8_package.py",
            directory.parent / "real-kernel" / "native_gate.py",
            directory.parent / "real-kernel" / "native_package.py",
            directory.parent / "real-kernel" / "native-workload.py",
        )
        for program in programs:
            with self.subTest(program=program.name):
                tree = ast.parse(program.read_text(), filename=str(program), feature_version=(3, 6))
                for node in ast.walk(tree):
                    if (isinstance(node, ast.Call)
                            and isinstance(node.func, ast.Attribute)
                            and isinstance(node.func.value, ast.Name)
                            and node.func.value.id == "subprocess"):
                        keywords = {keyword.arg for keyword in node.keywords}
                        self.assertNotIn("text", keywords)
                        self.assertNotIn("capture_output", keywords)
                    if (isinstance(node, ast.Attribute)
                            and isinstance(node.value, ast.Name)
                            and node.value.id == "time"):
                        self.assertNotEqual(node.attr, "time_ns")

    def test_parent_cpu_baseline_accepts_an_unmaterialized_unlimited_controller(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            gate = NativeGate(temporary_directory, "runit", "revision")
            gate.parent = Path(temporary_directory) / "user.slice"
            gate.parent.mkdir()
            response = SimpleNamespace(returncode=0, stdout="CPUQuotaPerSecUSec=infinity\n")
            with patch.object(gate, "command", return_value=response):
                gate.validate_parent_cpu_baseline()
            self.assertEqual(json.loads((gate.evidence / "parent-cpu-baseline.json").read_text()), {
                "kernel_cpu_max": "unavailable",
                "systemd_cpu_quota_per_sec_usec": "infinity",
            })

    def test_parent_cpu_baseline_rejects_a_finite_kernel_or_systemd_quota(self):
        cases = (
            ("80000 100000\n", "infinity", "finite kernel CPU quota"),
            (None, "80000", "explicit unlimited systemd CPU quota"),
            ("max 100000\n", "", "explicit unlimited systemd CPU quota"),
        )
        for kernel_quota, configured_quota, message in cases:
            with self.subTest(kernel_quota=kernel_quota, configured_quota=configured_quota), \
                    tempfile.TemporaryDirectory() as temporary_directory:
                gate = NativeGate(temporary_directory, "runit", "revision")
                gate.parent = Path(temporary_directory) / "user.slice"
                gate.parent.mkdir()
                if kernel_quota is not None:
                    (gate.parent / "cpu.max").write_text(kernel_quota)
                response = SimpleNamespace(returncode=0, stdout="CPUQuotaPerSecUSec=" + configured_quota + "\n")
                with patch.object(gate, "command", return_value=response), \
                        self.assertRaisesRegex(Blocked, message):
                    gate.validate_parent_cpu_baseline()

    def test_parent_cpu_baseline_rejects_an_unreadable_systemd_value(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            gate = NativeGate(temporary_directory, "runit", "revision")
            gate.parent = Path(temporary_directory) / "user.slice"
            gate.parent.mkdir()
            response = SimpleNamespace(returncode=1, stdout="systemctl failed\n")
            with patch.object(gate, "command", return_value=response), \
                    self.assertRaisesRegex(Blocked, "explicit unlimited systemd CPU quota"):
                gate.validate_parent_cpu_baseline()


if __name__ == "__main__":
    unittest.main()
