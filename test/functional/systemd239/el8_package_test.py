#!/usr/bin/env python3
"""Unit tests for the EL8 systemd 239 package boundary."""
import ast
import importlib.util
from pathlib import Path
import sys
import unittest


directory = Path(__file__).parent
sys.path.insert(0, str(directory))
sys.path.insert(0, str(directory.parent / "real-kernel"))
spec = importlib.util.spec_from_file_location("el8_package", directory / "el8_package.py")
el8 = importlib.util.module_from_spec(spec)
spec.loader.exec_module(el8)


class EL8PackageContractTests(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
