#!/usr/bin/env python3
"""Negative tests for the independent EL8 QEMU evidence consumer."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


path = Path(__file__).with_name("validate_evidence.py")
spec = importlib.util.spec_from_file_location("validate_evidence", path)
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)


class EvidenceConsumerTests(unittest.TestCase):
    qualification_revision = "b" * 40
    source_revision = "a" * 40

    def write_fields(self, path, values):
        path.write_text("".join(f"{key}={value}\n" for key, value in values.items()))

    def write_manifest(self, root):
        manifest = root / "SHA256SUMS"
        entries = [
            ("./" + str(path.relative_to(root)), validator.digest(path))
            for path in root.rglob("*")
            if path.is_file() and path != manifest
        ]
        manifest.write_text("".join(
            f"{value}  {name}\n" for name, value in sorted(entries)))

    def write_complete_fixture(self, directory):
        base = Path(directory)
        root = base / "evidence"
        root.mkdir()
        package = base / "package.rpm"
        package.write_bytes(b"synthetic EL8 RPM payload\n")
        package_sha256 = validator.digest(package)
        build = {
            "build_kind": "el8-rootful-podman",
            "source_revision": self.source_revision,
            "source_tree": "c" * 40,
            "source_archive_sha256": "d" * 64,
            "builder_image": "registry.example.invalid/resman-el8-builder",
            "builder_image_digest": "sha256:" + "e" * 64,
            "go_version": "go1.27.1",
            "package_identity": validator.PACKAGE_IDENTITY,
            "package_sha256": package_sha256,
        }
        build_manifest = base / "build-manifest.txt"
        self.write_fields(build_manifest, build)
        self.write_fields(root / "build-manifest.txt", build)
        self.write_fields(root / "environment.txt", {
            "qualification_revision": self.qualification_revision,
            "source_revision": self.source_revision,
            "result": "PASS",
            "cleanup": "PASS",
            "base_sha256": validator.BASE_SHA256,
            "package_identity": validator.PACKAGE_IDENTITY,
            "package_sha256": package_sha256,
        })
        self.write_boot_fixture(root)

        guest = root / "guest"
        guest.mkdir()
        (guest / "result").write_text("PASS\n")
        checks = {
            "el8-checks.json": {
                "el8-runtime-contract": "PASS",
                "negative-identity": "PASS",
                "package-lifecycle": "PASS",
            },
            "checks.json": {
                "installed-identity": "PASS",
                "shipped-defaults": "PASS",
                "schema-reset": "PASS",
                "upgrade-750-rejected": "PASS",
                "graceful-stop": "PASS",
            },
            "package-lifecycle-checks.json": {
                "full-budget-rejection": "PASS",
                "pam-sessions": "PASS",
                "blackout-suppression": "PASS",
                "blackout-observation": "PASS",
                "native-plan": "PASS",
                "resource-properties": "PASS",
                "authority-split": "PASS",
                "crash-reclaim": "PASS",
                "graceful-stop": "PASS",
                "root-only-release": "PASS",
            },
        }
        for name, values in checks.items():
            (guest / name).write_text(json.dumps(values) + "\n")
        (guest / "installed-identity.json").write_text(json.dumps({
            "package_identity": validator.PACKAGE_IDENTITY,
            "package_sha256": package_sha256,
        }) + "\n")
        (guest / "negative-identity.json").write_text(json.dumps({
            "exit_code": 78,
            "systemd_native_absent": True,
            "journal_absent": True,
            "probe_drop_ins_absent": True,
        }) + "\n")
        (root / "post-run.txt").write_text(
            "Result=success\nActiveState=inactive\nSubState=dead\n"
            "max 100000\nCPUQuotaPerSecUSec=infinity\n")
        self.write_manifest(root)
        return root, package, build_manifest

    def validate_fixture(self, root, package, build_manifest):
        validator.validate(
            root, self.qualification_revision, package, build_manifest)

    def replace_field(self, path, key, value):
        values = validator.fields(path)
        values[key] = value
        self.write_fields(path, values)

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

    def test_complete_evidence_accepts_package_and_guest_results(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, build_manifest = self.write_complete_fixture(directory)
            self.validate_fixture(root, package, build_manifest)

    def test_manifest_rejects_absent_malformed_unsafe_and_duplicate_entries(self):
        mutations = {
            "absent": lambda root: (root / "SHA256SUMS").unlink(),
            "malformed": lambda root: (root / "SHA256SUMS").write_text(
                "not-a-sha256  ./retained.txt\n"),
            "unsafe": lambda root: (root / "SHA256SUMS").write_text(
                validator.digest(root / "retained.txt") + "  retained.txt\n"),
            "duplicate": lambda root: (root / "SHA256SUMS").write_text(
                2 * (validator.digest(root / "retained.txt") + "  ./retained.txt\n")),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                (root / "retained.txt").write_text("retained\n")
                self.write_manifest(root)
                mutate(root)
                with self.assertRaises(AssertionError):
                    validator.verify_manifest(root)

    def test_manifest_rejects_an_invalid_digest_even_if_files_match(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "retained.txt").write_text("retained\n")
            invalid_digest = "g" * 64
            (root / "SHA256SUMS").write_text(
                invalid_digest + "  ./retained.txt\n")
            with mock.patch.object(validator, "digest", return_value=invalid_digest):
                with self.assertRaisesRegex(AssertionError, "malformed"):
                    validator.verify_manifest(root)

    def test_command_line_rejects_an_incomplete_validation_request(self):
        result = subprocess.run(
            [sys.executable, str(path)], capture_output=True, text=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("usage: validate_evidence.py", result.stdout + result.stderr)

    def test_validate_rejects_divergent_build_and_package_provenance(self):
        mutations = {
            "retained build manifest": (
                "retained build manifest",
                lambda root, _package, _manifest: self.replace_field(
                    root / "build-manifest.txt", "builder_image", "other-builder")),
            "host result": (
                "host run or cleanup",
                lambda root, _package, _manifest: self.replace_field(
                    root / "environment.txt", "result", "FAIL")),
            "host cleanup": (
                "host run or cleanup",
                lambda root, _package, _manifest: self.replace_field(
                    root / "environment.txt", "cleanup", "FAIL")),
            "qualification revision": (
                "qualification revision",
                lambda root, _package, _manifest: self.replace_field(
                    root / "environment.txt", "qualification_revision", "f" * 40)),
            "package source revision": (
                "package source revision",
                lambda root, _package, _manifest: self.replace_field(
                    root / "environment.txt", "source_revision", "f" * 40)),
            "build kind": (
                "EL8 container path",
                lambda root, _package, _manifest: self.replace_field(
                    root / "build-manifest.txt", "build_kind", "local")),
            "required build field": (
                "build manifest lacks go_version",
                lambda root, _package, _manifest: self.replace_field(
                    root / "build-manifest.txt", "go_version", "")),
            "build package identity": (
                "build manifest identifies another RPM",
                lambda root, _package, _manifest: self.replace_field(
                    root / "build-manifest.txt", "package_identity", "resman-other")),
            "base image digest": (
                "unreviewed EL8 base image",
                lambda root, _package, _manifest: self.replace_field(
                    root / "environment.txt", "base_sha256", "0" * 64)),
            "request package identity": (
                "unexpected RPM identity",
                lambda root, _package, _manifest: self.replace_field(
                    root / "environment.txt", "package_identity", "resman-other")),
            "request package digest": (
                "RPM digest differs",
                lambda root, _package, _manifest: self.replace_field(
                    root / "environment.txt", "package_sha256", "0" * 64)),
            "build package digest": (
                "RPM digest differs",
                lambda root, _package, _manifest: self.replace_field(
                    root / "build-manifest.txt", "package_sha256", "0" * 64)),
        }
        for name, (message, mutate) in mutations.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root, package, build_manifest = self.write_complete_fixture(directory)
                mutate(root, package, build_manifest)
                if name in {"build kind", "required build field", "build package identity",
                            "build package digest"}:
                    (build_manifest).write_text((root / "build-manifest.txt").read_text())
                self.write_manifest(root)
                with self.assertRaisesRegex(AssertionError, message):
                    self.validate_fixture(root, package, build_manifest)

    def test_validate_rejects_incomplete_or_divergent_guest_results(self):
        inventory_files = (
            "el8-checks.json",
            "checks.json",
            "package-lifecycle-checks.json",
        )
        for name in inventory_files:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root, package, build_manifest = self.write_complete_fixture(directory)
                path = root / "guest" / name
                values = json.loads(path.read_text())
                values.pop(next(iter(values)))
                path.write_text(json.dumps(values) + "\n")
                self.write_manifest(root)
                with self.assertRaisesRegex(AssertionError, "check inventory"):
                    self.validate_fixture(root, package, build_manifest)

        mutations = {
            "guest gate": (
                "guest package gate failed",
                lambda root, _package: (root / "guest" / "result").write_text("FAIL\n")),
            "installed package identity": (
                "guest installed another package identity",
                lambda root, _package: (root / "guest" / "installed-identity.json").write_text(
                    json.dumps({"package_identity": "resman-other",
                                "package_sha256": validator.digest(_package)}) + "\n")),
            "installed package digest": (
                "guest tested another package payload",
                lambda root, _package: (root / "guest" / "installed-identity.json").write_text(
                    json.dumps({"package_identity": validator.PACKAGE_IDENTITY,
                                "package_sha256": "0" * 64}) + "\n")),
            "negative identity proof": (
                "negative identity proof is incomplete",
                lambda root, _package: (root / "guest" / "negative-identity.json").write_text(
                    json.dumps({"exit_code": 78, "systemd_native_absent": True,
                                "journal_absent": True}) + "\n")),
            "post-run baseline": (
                "post-run service or parent baseline is wrong",
                lambda root, _package: (root / "post-run.txt").write_text(
                    "Result=success\nActiveState=active\nSubState=running\n"
                    "90000 100000\nCPUQuotaPerSecUSec=900ms\n")),
        }
        for name, (message, mutate) in mutations.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root, package, build_manifest = self.write_complete_fixture(directory)
                mutate(root, package)
                self.write_manifest(root)
                with self.assertRaisesRegex(AssertionError, message):
                    self.validate_fixture(root, package, build_manifest)

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
