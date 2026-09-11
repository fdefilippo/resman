#!/usr/bin/env python3
"""Negative tests for the independent EL8 QEMU evidence consumer."""
import importlib.util
from pathlib import Path
import tempfile
import unittest


path = Path(__file__).with_name("validate_evidence.py")
spec = importlib.util.spec_from_file_location("validate_evidence", path)
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)


class EvidenceConsumerTests(unittest.TestCase):
    def write_boot_fixture(self, root):
        files = {
            "initial-boot/cgroup-filesystem.txt": "tmpfs\n",
            "initial-boot/firmware.txt": "uefi\n",
            "initial-boot/boot-id.txt": "initial\n",
            "qualified-boot/boot-id.txt": "qualified\n",
            "qualified-boot/systemd-version.txt": "systemd 239 (239-82.el8)\n",
            "qualified-boot/cgroup-filesystem.txt": "cgroup2fs\n",
            "qualified-boot/firmware.txt": "uefi\n",
            "qualified-boot/cmdline.txt": (
                "systemd.unified_cgroup_hierarchy=1 psi=1\n"),
            "qualified-boot/controllers.txt": "cpu io memory\n",
            "qualified-boot/psi-files.txt": "cpu io memory\n",
            "qualified-boot/user-slice.txt": "ControlGroup=/user.slice\n",
            "qualified-boot/slice-interface.txt": ".ControlGroup property s\n",
            "qualified-boot/block-devices.txt": "sda 8:0 disk\n",
            "bootloader-before/grubby-info.txt": "args=quiet\n",
            "bootloader-after/grubby-info.txt": (
                "args=systemd.unified_cgroup_hierarchy=1 psi=1\n"),
            "bootloader-after/grubenv.txt": (
                "kernelopts=systemd.unified_cgroup_hierarchy=1 psi=1\n"),
            "bootloader-after/bls-config.txt": "GRUB_ENABLE_BLSCFG=true\n",
            "bootloader-after/grubenv-link.txt": (
                "/boot/grub2/grubenv -> ../efi/EFI/redhat/grubenv\n"),
            "bootloader-after/entries.txt": "options $kernelopts\n",
        }
        for name, content in files.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)

    def test_el8_boot_contract_accepts_complete_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.write_boot_fixture(root)
            validator.verify_el8_boot_contract(root)

    def test_el8_boot_contract_rejects_each_missing_requirement(self):
        mutations = {
            "initial unified hierarchy": ("initial-boot/cgroup-filesystem.txt", "cgroup2fs\n"),
            "initial UEFI": ("initial-boot/firmware.txt", "bios\n"),
            "distinct reboot": ("qualified-boot/boot-id.txt", "initial\n"),
            "systemd 239": ("qualified-boot/systemd-version.txt", "systemd 252\n"),
            "qualified cgroup v2": ("qualified-boot/cgroup-filesystem.txt", "tmpfs\n"),
            "qualified UEFI": ("qualified-boot/firmware.txt", "bios\n"),
            "new boot arguments": (
                "bootloader-before/grubby-info.txt",
                "args=systemd.unified_cgroup_hierarchy=1 psi=1\n"),
            "grubby arguments": ("bootloader-after/grubby-info.txt", "args=psi=1\n"),
            "grubenv arguments": ("bootloader-after/grubenv.txt", "kernelopts=psi=1\n"),
            "BLS enabled": ("bootloader-after/bls-config.txt", "GRUB_ENABLE_BLSCFG=false\n"),
            "reviewed grubenv link": (
                "bootloader-after/grubenv-link.txt",
                "/boot/grub2/grubenv -> ../grub2/grubenv\n"),
            "BLS kernelopts": ("bootloader-after/entries.txt", "options quiet\n"),
            "qualified command line": ("qualified-boot/cmdline.txt", "psi=1\n"),
            "required controller": ("qualified-boot/controllers.txt", "cpu memory\n"),
            "required PSI file": ("qualified-boot/psi-files.txt", "cpu memory\n"),
            "systemctl ControlGroupId absence": (
                "qualified-boot/user-slice.txt", "ControlGroupId=1646\n"),
            "D-Bus ControlGroupId absence": (
                "qualified-boot/slice-interface.txt", "  ControlGroupId property t\n"),
            "tested block device": ("qualified-boot/block-devices.txt", "vda 253:0 disk\n"),
        }
        for name, (path, content) in mutations.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                self.write_boot_fixture(root)
                (root / path).write_text(content)
                with self.assertRaises(AssertionError):
                    validator.verify_el8_boot_contract(root)

    def test_post_run_accepts_explicit_and_unmaterialized_unlimited_cpu(self):
        prefix = "Result=success\nActiveState=inactive\nSubState=dead\n"
        suffix = "CPUQuotaPerSecUSec=infinity\n"
        for kernel in ("max 100000\n", "cpu.max=unavailable\n"):
            with self.subTest(kernel=kernel.strip()):
                validator.verify_post_run(prefix + kernel + suffix)

    def test_post_run_rejects_incomplete_or_finite_cleanup(self):
        valid = "Result=success\nActiveState=inactive\nSubState=dead\ncpu.max=unavailable\nCPUQuotaPerSecUSec=infinity\n"
        for name, mutated in (
                ("active service", valid.replace("ActiveState=inactive", "ActiveState=active")),
                ("failed service", valid.replace("Result=success", "Result=exit-code")),
                ("finite kernel quota", valid.replace("cpu.max=unavailable", "90000 100000")),
                ("malformed absent marker", valid.replace("cpu.max=unavailable", "cpu.max=")),
                ("finite systemd quota", valid.replace("CPUQuotaPerSecUSec=infinity",
                                                         "CPUQuotaPerSecUSec=900ms"))):
            with self.subTest(name=name), self.assertRaisesRegex(AssertionError, "parent baseline"):
                validator.verify_post_run(mutated)

    def test_fields_reject_duplicate_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Path(directory) / "fields.txt"
            fixture.write_text("result=PASS\nresult=FAIL\n")
            with self.assertRaisesRegex(AssertionError, "duplicate"):
                validator.fields(fixture)

    def test_manifest_rejects_missing_and_extra_files(self):
        for mutation in ("missing", "extra"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                retained = root / "retained.txt"
                retained.write_text("retained\n")
                (root / "SHA256SUMS").write_text(
                    validator.digest(retained) + "  ./retained.txt\n")
                validator.verify_manifest(root)
                if mutation == "missing":
                    retained.unlink()
                else:
                    (root / "extra.txt").write_text("extra\n")
                with self.assertRaisesRegex(AssertionError, "differ"):
                    validator.verify_manifest(root)

    def test_all_pass_requires_exact_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Path(directory) / "checks.json"
            fixture.write_text('{"one":"PASS","two":"PASS"}\n')
            validator.all_pass(fixture, {"one", "two"})
            with self.assertRaisesRegex(AssertionError, "inventory"):
                validator.all_pass(fixture, {"one", "two", "three"})
            fixture.write_text('{"one":"PASS","two":"FAIL"}\n')
            with self.assertRaisesRegex(AssertionError, "non-PASS"):
                validator.all_pass(fixture, {"one", "two"})

    def test_revision_provenance_separates_package_from_qualification(self):
        environment = {
            "qualification_revision": "b" * 40,
            "source_revision": "a" * 40,
        }
        build = {"source_revision": "a" * 40}
        validator.verify_revision_provenance(environment, build, "b" * 40)

        for key, value, message in (
                ("qualification_revision", "c" * 40, "qualification"),
                ("source_revision", "c" * 40, "package source")):
            with self.subTest(key=key):
                mutated = dict(environment)
                mutated[key] = value
                with self.assertRaisesRegex(AssertionError, message):
                    validator.verify_revision_provenance(mutated, build, "b" * 40)


if __name__ == "__main__":
    unittest.main()
