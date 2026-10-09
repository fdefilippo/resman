#!/usr/bin/env python3
"""Negative tests for the independent cpu-delegation evidence consumer."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


path = Path(__file__).with_name("validate_evidence.py")
spec = importlib.util.spec_from_file_location("validate_evidence", path)
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)

QUALIFICATION = "b" * 40
SOURCE = "a" * 40
PACKAGE_IDENTITY = "resman-1.38.0-10.el8.x86_64"
KERNEL = "4.18.0-553.56.1.el8_10.x86_64"
FIXTURE_UNIT = "resman-cpudel-fixture"
INITIAL_SUBTREE = ["memory", "pids"]


def shown(*values):
    return {"command": ["systemctl", "show"], "status": 0, "output": list(values)}


def delegation(accepted, errno=None, errno_name=None, outside_root=(), revert=True):
    after = INITIAL_SUBTREE + ["cpu"] if accepted else list(INITIAL_SUBTREE)
    attempt = {"written": "+cpu", "accepted": accepted, "errno": errno,
               "errno_name": errno_name, "message": None, "subtree_control": after}
    reverted = None
    if accepted and revert:
        reverted = {"written": "-cpu", "accepted": True, "errno": None, "errno_name": None,
                    "message": None, "subtree_control": list(INITIAL_SUBTREE)}
    tasks = list(outside_root)
    return {"label": "probe", "subtree_control_before": list(INITIAL_SUBTREE),
            "attempt": attempt, "revert": reverted,
            "subtree_control_after": list(INITIAL_SUBTREE) if not accepted or revert
            else after,
            "realtime": {"all": tasks, "outside_root": tasks}}


FIXTURE_TASK = {"tid": 4242, "policy": "FF", "priority": "50",
                "cgroup": "0::/system.slice/" + FIXTURE_UNIT + ".service"}


def declared_run(mode, reason):
    """One start made under an explicit operator declaration."""
    published = ("level=INFO event=startup enforcement_mode=observation_only reason=" + reason)
    return {
        "label": "declared-" + mode,
        "declaration": {"requested": mode, "replaced_existing_line": True,
                        "effective": ["ENFORCEMENT_MODE=" + mode]},
        "start": {"command": ["systemctl", "start", "resman"], "status": 0, "output": []},
        "state": shown("ActiveState=active", "SubState=running", "Result=success",
                       "ExecMainStatus=0", "ExecMainCode=0", "NRestarts=0"),
        "log": [published],
        "capability_errors": [],
        "enforcement_mode": [published],
        "user_slice_cpu_max": None,
        "subtree_control": list(INITIAL_SUBTREE),
        "probe_slices": {"units": shown(), "cgroups": []},
    }


def archive_records():
    capability = ("level=ERROR event=startup_failed reason=required_capability_unavailable "
                  "detail=\"cpu.max is unavailable for user-resmancapprobe0.slice\"")
    return {
        "platform": {
            "os_release": ['ID="ol"', 'VERSION_ID="8.10"'],
            "kernel": KERNEL,
            "kernel_config_present": True,
            "rt_group_sched": "CONFIG_RT_GROUP_SCHED=y",
            "command_line": "systemd.unified_cgroup_hierarchy=1 psi=1",
            "cgroup_filesystem": {"command": ["stat"], "status": 0, "output": ["cgroup2fs"]},
            "controllers": ["cpuset", "cpu", "io", "memory", "hugetlb", "pids", "rdma"],
            "subtree_control": list(INITIAL_SUBTREE),
            "systemd_version": shown("systemd 239 (239-82.el8_10.1)"),
            "systemd_package": shown("systemd-239-82.el8_10.1.x86_64"),
            "kernel_package": shown("kernel-core-" + KERNEL),
            "resman_package": shown(PACKAGE_IDENTITY),
            "sched_rt_runtime_us": "950000",
        },
        "baseline-delegation": delegation(True),
        "realtime-fixture": {
            "unit": FIXTURE_UNIT, "requested_priority": 50,
            "start": {"command": ["systemd-run"], "status": 0, "output": []},
            "main_pid": FIXTURE_TASK["tid"],
            "policy": {"command": ["chrt"], "status": 0, "output": [
                "pid 4242's current scheduling policy: SCHED_FIFO",
                "pid 4242's current scheduling priority: 50"]},
            "cgroup": FIXTURE_TASK["cgroup"],
            "state": shown("ActiveState=active", "SubState=running"),
            "realtime": {"all": [FIXTURE_TASK], "outside_root": [FIXTURE_TASK]},
        },
        "delegation-under-realtime": delegation(False, errno=22, errno_name="EINVAL",
                                                outside_root=[FIXTURE_TASK]),
        "daemon-under-realtime": {
            "label": "under-realtime",
            "start": {"command": ["systemctl", "start", "resman"], "status": 1, "output": []},
            "state": shown("ActiveState=failed", "SubState=failed", "Result=exit-code",
                           "ExecMainStatus=78", "ExecMainCode=1", "NRestarts=0"),
            "log": [capability], "capability_errors": [capability],
            "enforcement_mode": [], "user_slice_cpu_max": None,
            "subtree_control": list(INITIAL_SUBTREE),
            "probe_slices": {"units": shown(), "cgroups": []},
        },
        "control-without-realtime": {
            "realtime_after_removal": {"all": [], "outside_root": []},
            "delegation": delegation(True),
            "daemon": {
                "label": "without-realtime",
                "start": {"command": ["systemctl", "start", "resman"], "status": 0,
                          "output": []},
                "state": shown("ActiveState=active", "SubState=running", "Result=success",
                               "ExecMainStatus=0", "ExecMainCode=0", "NRestarts=0"),
                "log": ["level=INFO event=startup enforcement_mode=systemd_native"],
                "capability_errors": [],
                "enforcement_mode": ["level=INFO event=startup enforcement_mode=systemd_native"],
                "user_slice_cpu_max": "max 100000\n",
                "subtree_control": INITIAL_SUBTREE + ["cpu"],
                "probe_slices": {"units": shown(), "cgroups": []},
            },
        },
        "declared-observation": declared_run("observation_only",
                                            "operator_requested_observation"),
        "declared-auto": declared_run("auto", "mandatory_capability_unavailable"),
        "restored-declaration": {"requested": "systemd_native",
                                 "replaced_existing_line": True,
                                 "effective": ["ENFORCEMENT_MODE=systemd_native"]},
        "residue": {
            "daemon": shown("ActiveState=inactive", "SubState=dead", "Result=success"),
            "fixture": shown("ActiveState=inactive", "SubState=dead"),
            "subtree_control": list(INITIAL_SUBTREE),
            "initial_subtree_control": list(INITIAL_SUBTREE),
            "leases_present": False,
            "system_control": [],
            "probe_slices": {"units": shown(), "cgroups": []},
            "realtime": {"all": [], "outside_root": []},
        },
        "result": {"result": "MEASURED", "run_id": "r20261009120000-1",
                   "source_revision": SOURCE, "unit": FIXTURE_UNIT, "priority": 50},
    }


class ConsumerTests(unittest.TestCase):
    def build(self, directory, records=None, package=b"package", files=None):
        root = Path(directory) / "remote"
        (root / "guest").mkdir(parents=True)
        (root / "initial-boot").mkdir()
        (root / "qualified-boot").mkdir()
        package_path = Path(directory) / "package.rpm"
        package_path.write_bytes(package)
        digest = hashlib.sha256(package).hexdigest()
        build_manifest = Path(directory) / "build-manifest.txt"
        manifest_body = ("build_kind=el8-rootful-podman\nsource_revision=" + SOURCE
                         + "\npackage_identity=" + PACKAGE_IDENTITY
                         + "\npackage_sha256=" + digest + "\n")
        build_manifest.write_text(manifest_body)
        (root / "build-manifest.txt").write_text(manifest_body)
        (root / "environment.txt").write_text(
            "run_id=r20261009120000-1\nqualification_revision=" + QUALIFICATION
            + "\nsource_revision=" + SOURCE
            + "\nbase_sha256=" + validator.BASE_SHA256
            + "\nkernel_family=rhck\npackage_identity=" + PACKAGE_IDENTITY
            + "\npackage_sha256=" + digest + "\n")
        (root / "initial-boot" / "cgroup-filesystem.txt").write_text("tmpfs\n")
        (root / "initial-boot" / "boot-id.txt").write_text("boot-one\n")
        (root / "qualified-boot" / "boot-id.txt").write_text("boot-two\n")
        (root / "qualified-boot" / "systemd-version.txt").write_text(
            "systemd 239 (239-82.el8_10.1)\n")
        (root / "qualified-boot" / "cgroup-filesystem.txt").write_text("cgroup2fs\n")
        (root / "qualified-boot" / "os-release.txt").write_text(
            'ID="ol"\nVERSION_ID="8.10"\n')
        (root / "qualified-boot" / "kernel.txt").write_text(KERNEL + "\n")
        (root / "qualified-boot" / "running-kernel-package.txt").write_text(
            "kernel-core-" + KERNEL + "\n")
        (root / "qualified-boot" / "cmdline.txt").write_text(
            "BOOT_IMAGE=/vmlinuz systemd.unified_cgroup_hierarchy=1 psi=1\n")
        for name, payload in (records if records is not None else archive_records()).items():
            (root / "guest" / (name + ".json")).write_text(json.dumps(payload) + "\n")
        for name, body in (files or {}).items():
            (root / name).write_text(body)
        lines = []
        for item in sorted(root.rglob("*")):
            if item.is_file():
                lines.append(hashlib.sha256(item.read_bytes()).hexdigest() + "  ./"
                             + str(item.relative_to(root)) + "\n")
        (root / "SHA256SUMS").write_text("".join(lines))
        return root, package_path, build_manifest

    def reseal(self, root):
        """Rewrite the manifest after a deliberate archive edit."""
        (root / "SHA256SUMS").write_text("".join(
            hashlib.sha256(item.read_bytes()).hexdigest() + "  ./"
            + str(item.relative_to(root)) + "\n"
            for item in sorted(root.rglob("*"))
            if item.is_file() and item.name != "SHA256SUMS"))

    def validate(self, records=None, expect="REPRODUCED", expect_remedy="REMEDIED", **keywords):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory, records, **keywords)
            return validator.validate(root, QUALIFICATION, package, manifest, expect,
                                      expect_remedy)

    def test_complete_archive_is_reproduced_and_remedied(self):
        summary = self.validate()
        self.assertEqual(summary["verdict"], "REPRODUCED")
        self.assertEqual(summary["remedy"], "REMEDIED")
        self.assertEqual(summary["outcomes"]["declared_observation"], "OBSERVING")
        self.assertEqual(summary["outcomes"]["declared_auto"], "OBSERVING")
        self.assertEqual(summary["outcomes"], {
            "declared_observation": "OBSERVING",
            "declared_auto": "OBSERVING",
            "delegation_baseline": "DELEGABLE",
            "realtime_fixture": "ESTABLISHED",
            "delegation_under_realtime": "NOT_DELEGABLE",
            "daemon_under_realtime": "REFUSED_CONFIGURATION",
            "delegation_without_realtime": "DELEGABLE",
            "daemon_without_realtime": "STARTED",
        })

    def mutate(self, mutation):
        records = archive_records()
        mutation(records)
        return records

    def reject(self, mutation, fragment, expect="REPRODUCED"):
        with self.assertRaises(AssertionError) as caught:
            self.validate(self.mutate(mutation), expect=expect)
        self.assertIn(fragment, str(caught.exception))

    def test_baseline_refusal_is_not_a_reproduction(self):
        def mutation(records):
            records["baseline-delegation"] = delegation(False, errno=22, errno_name="EINVAL")
        self.reject(mutation, "computed verdict NOT_REPRODUCED")

    def test_accepted_delegation_under_realtime_is_not_a_reproduction(self):
        def mutation(records):
            records["delegation-under-realtime"] = delegation(
                True, outside_root=[FIXTURE_TASK])
        self.reject(mutation, "computed verdict NOT_REPRODUCED")

    def test_unexpected_refusal_reason_is_refused(self):
        def mutation(records):
            records["delegation-under-realtime"] = delegation(
                False, errno=1, errno_name="EPERM", outside_root=[FIXTURE_TASK])
        self.reject(mutation, "unexpected reason")

    def test_refusal_without_errno_is_refused(self):
        def mutation(records):
            records["delegation-under-realtime"]["attempt"]["errno"] = None
        self.reject(mutation, "recorded no errno")

    def test_refusal_measured_without_realtime_task_is_refused(self):
        def mutation(records):
            records["delegation-under-realtime"]["realtime"] = {"all": [], "outside_root": []}
        self.reject(mutation, "without any realtime task outside the root")

    def test_baseline_measured_with_realtime_task_is_refused(self):
        def mutation(records):
            records["baseline-delegation"]["realtime"]["outside_root"] = [FIXTURE_TASK]
        self.reject(mutation, "realtime tasks already lived outside the root")

    def test_unreverted_baseline_is_refused(self):
        def mutation(records):
            records["baseline-delegation"]["revert"] = None
        self.reject(mutation, "was not reverted")

    def test_fixture_without_realtime_policy_is_refused(self):
        def mutation(records):
            records["realtime-fixture"]["policy"]["output"] = [
                "pid 4242's current scheduling policy: SCHED_OTHER"]
        self.reject(mutation, "does not run under SCHED_FIFO")

    def test_fixture_with_wrong_priority_is_refused(self):
        def mutation(records):
            records["realtime-fixture"]["policy"]["output"] = [
                "pid 4242's current scheduling policy: SCHED_FIFO",
                "pid 4242's current scheduling priority: 1"]
        self.reject(mutation, "does not hold the requested realtime priority")

    def test_fixture_in_root_cgroup_is_refused(self):
        def mutation(records):
            records["realtime-fixture"]["cgroup"] = "0::/"
        self.reject(mutation, "not inside its own non-root cgroup")

    def test_fixture_absent_from_inventory_is_refused(self):
        def mutation(records):
            records["realtime-fixture"]["realtime"]["outside_root"] = []
        self.reject(mutation, "absent from the realtime inventory")

    def test_inactive_fixture_is_refused(self):
        def mutation(records):
            records["realtime-fixture"]["state"] = shown("ActiveState=inactive")
        self.reject(mutation, "fixture unit is not active")

    def test_daemon_refusal_with_other_status_is_refused(self):
        def mutation(records):
            records["daemon-under-realtime"]["state"] = shown(
                "ActiveState=failed", "SubState=failed", "Result=exit-code",
                "ExecMainStatus=1")
        self.reject(mutation, "permanent configuration status")

    def test_daemon_refusal_without_typed_error_is_refused(self):
        def mutation(records):
            records["daemon-under-realtime"]["capability_errors"] = []
        self.reject(mutation, "typed cpu capability error")

    def test_daemon_refusal_naming_another_interface_is_refused(self):
        def mutation(records):
            records["daemon-under-realtime"]["capability_errors"] = [
                "reason=required_capability_unavailable detail=\"io.weight is unavailable\""]
        self.reject(mutation, "typed cpu capability error")

    def test_started_daemon_under_realtime_is_not_a_reproduction(self):
        def mutation(records):
            records["daemon-under-realtime"]["state"] = shown(
                "ActiveState=active", "SubState=running", "Result=success",
                "ExecMainStatus=0")
            records["daemon-under-realtime"]["capability_errors"] = []
        self.reject(mutation, "did not refuse to start")

    def test_control_daemon_refusal_is_refused(self):
        def mutation(records):
            records["control-without-realtime"]["daemon"]["state"] = shown(
                "ActiveState=failed", "SubState=failed", "Result=exit-code",
                "ExecMainStatus=78")
        self.reject(mutation, "did not activate")

    def test_control_daemon_without_published_mode_is_refused(self):
        def mutation(records):
            records["control-without-realtime"]["daemon"]["enforcement_mode"] = []
        self.reject(mutation, "without publishing its enforcement mode")

    def test_control_daemon_with_capability_error_is_refused(self):
        def mutation(records):
            records["control-without-realtime"]["daemon"]["capability_errors"] = [
                "reason=required_capability_unavailable detail=\"cpu.max is unavailable\""]
        self.reject(mutation, "although it activated")

    def test_control_with_remaining_realtime_task_is_refused(self):
        def mutation(records):
            records["control-without-realtime"]["realtime_after_removal"] = {
                "all": [FIXTURE_TASK], "outside_root": [FIXTURE_TASK]}
        self.reject(mutation, "still lived outside the root")

    def test_control_refusing_delegation_is_refused(self):
        def mutation(records):
            records["control-without-realtime"]["delegation"] = delegation(
                False, errno=22, errno_name="EINVAL")
        self.reject(mutation, "did not restore cpu delegation")

    def test_control_leaving_manual_delegation_is_refused(self):
        def mutation(records):
            records["control-without-realtime"]["delegation"]["subtree_control_after"] = \
                INITIAL_SUBTREE + ["cpu"]
        self.reject(mutation, "delegated by hand")

    def test_running_service_in_residue_is_refused(self):
        def mutation(records):
            records["residue"]["daemon"] = shown("ActiveState=active")
        self.reject(mutation, "left running")

    def test_active_fixture_in_residue_is_refused(self):
        def mutation(records):
            records["residue"]["fixture"] = shown("ActiveState=active")
        self.reject(mutation, "fixture was left active")

    def test_changed_delegation_in_residue_is_refused(self):
        def mutation(records):
            records["residue"]["subtree_control"] = INITIAL_SUBTREE + ["cpu"]
        self.reject(mutation, "not restored to its initial value")

    def test_remaining_lease_is_refused(self):
        def mutation(records):
            records["residue"]["leases_present"] = True
        self.reject(mutation, "durable property lease was left behind")

    def test_remaining_drop_in_is_refused(self):
        def mutation(records):
            records["residue"]["system_control"] = [
                "/run/systemd/system.control/user.slice.d"]
        self.reject(mutation, "drop-in was left behind")

    def test_remaining_probe_slice_is_refused(self):
        def mutation(records):
            records["residue"]["probe_slices"]["cgroups"] = [
                "/sys/fs/cgroup/user-resmancapprobe0.slice"]
        self.reject(mutation, "probe slice was left behind")

    def test_kernel_without_rt_group_sched_is_refused(self):
        def mutation(records):
            records["platform"]["rt_group_sched"] = "# CONFIG_RT_GROUP_SCHED is not set"
        self.reject(mutation, "CONFIG_RT_GROUP_SCHED=y")

    def test_predelegated_root_is_refused(self):
        def mutation(records):
            records["platform"]["subtree_control"] = INITIAL_SUBTREE + ["cpu"]
        self.reject(mutation, "already delegated cpu")

    def test_absent_package_is_refused(self):
        def mutation(records):
            records["platform"]["resman_package"]["status"] = 1
        self.reject(mutation, "not installed in the guest")

    def test_disagreeing_kernel_is_refused(self):
        def mutation(records):
            records["platform"]["kernel"] = "4.18.0-other.el8_10.x86_64"
        self.reject(mutation, "disagree on the running kernel")

    def test_blocked_measurement_is_refused(self):
        def mutation(records):
            records["result"] = {"result": "BLOCKED", "detail": "fixture never started"}
        self.reject(mutation, "did not complete the measurement")

    def test_falsifying_run_is_accepted_only_when_expected(self):
        records = self.mutate(
            lambda values: values.update({"baseline-delegation": delegation(
                False, errno=22, errno_name="EINVAL")}))
        summary = self.validate(copy.deepcopy(records), expect="NOT_REPRODUCED")
        self.assertEqual(summary["verdict"], "NOT_REPRODUCED")

    def test_uek_kernel_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            (root / "qualified-boot" / "kernel.txt").write_text("5.15.0-uek.el8uek.x86_64\n")
            self.reseal(root)
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("does not run an RHCK kernel", str(caught.exception))

    def test_kernel_not_owned_by_kernel_core_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            (root / "qualified-boot" / "running-kernel-package.txt").write_text(
                "kernel-uek-core-5.15.0\n")
            self.reseal(root)
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("not owned by kernel-core", str(caught.exception))

    def test_initial_boot_already_unified_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            (root / "initial-boot" / "cgroup-filesystem.txt").write_text("cgroup2fs\n")
            self.reseal(root)
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("distribution default", str(caught.exception))

    def test_single_boot_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            (root / "qualified-boot" / "boot-id.txt").write_text("boot-one\n")
            self.reseal(root)
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("distinct boot", str(caught.exception))

    def test_tampered_evidence_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            (root / "qualified-boot" / "kernel.txt").write_text(KERNEL + " tampered\n")
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("SHA256SUMS differ", str(caught.exception))

    def test_foreign_package_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            package.write_bytes(b"another package")
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("does not identify the supplied package", str(caught.exception))

    def test_foreign_qualification_revision_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, "c" * 40, package, manifest, "REPRODUCED")
        self.assertIn("qualification revision differs", str(caught.exception))

    def test_absent_manifest_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            (root / "SHA256SUMS").unlink()
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("manifest is absent", str(caught.exception))

    def test_retained_build_manifest_must_match(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            manifest.write_text(manifest.read_text() + "extra=1\n")
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("retained build manifest differs", str(caught.exception))

    def test_non_el8_package_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            body = (root / "environment.txt").read_text().replace(
                PACKAGE_IDENTITY, "resman-1.38.0-10.el9.x86_64")
            (root / "environment.txt").write_text(body)
            self.reseal(root)
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("not an EL8 package", str(caught.exception))

    # The declarations that rescue an affected host are recomputed too.
    def test_a_refused_declared_start_is_not_a_remedy(self):
        def mutation(records):
            records["declared-auto"]["state"] = shown(
                "ActiveState=failed", "SubState=failed", "Result=exit-code",
                "ExecMainStatus=78")
        self.reject(mutation, "did not activate")

    def test_a_declaration_without_a_published_reason_is_refused(self):
        def mutation(records):
            records["declared-auto"]["enforcement_mode"] = [
                "level=INFO event=startup enforcement_mode=observation_only"]
        self.reject(mutation, "mandatory_capability_unavailable")

    def test_declared_observation_must_not_borrow_the_capability_reason(self):
        def mutation(records):
            records["declared-observation"] = declared_run(
                "observation_only", "mandatory_capability_unavailable")
        self.reject(mutation, "operator_requested_observation")

    def test_a_declaration_measured_under_another_mode_is_refused(self):
        def mutation(records):
            records["declared-auto"]["declaration"]["effective"] = [
                "ENFORCEMENT_MODE=systemd_native"]
        self.reject(mutation, "not the one measured")

    def test_a_probe_slice_left_by_a_declared_mode_is_refused(self):
        def mutation(records):
            records["declared-observation"]["probe_slices"]["cgroups"] = [
                "/sys/fs/cgroup/user-resmancapprobe0.slice"]
        self.reject(mutation, "capability probe slice behind")

    def test_a_probe_unit_created_under_declared_observation_is_refused(self):
        def mutation(records):
            records["declared-observation"]["probe_slices"]["units"] = shown(
                "user-resmancapprobe0.slice loaded active active")
        self.reject(mutation, "created a capability probe unit")

    def test_a_capability_error_during_a_declared_mode_is_refused(self):
        def mutation(records):
            records["declared-auto"]["capability_errors"] = [
                "reason=required_capability_unavailable detail=\"cpu.max is unavailable\""]
        self.reject(mutation, "published a capability error as a failure")

    def test_a_measurement_that_does_not_restore_the_default_is_refused(self):
        def mutation(records):
            records["restored-declaration"]["effective"] = ["ENFORCEMENT_MODE=auto"]
        self.reject(mutation, "did not restore the default declaration")

    def test_half_a_remedy_is_refused(self):
        def mutation(records):
            del records["declared-auto"]
        self.reject(mutation, "only one of the two observing declarations")

    def test_an_archive_without_the_remedy_is_only_accepted_explicitly(self):
        records = archive_records()
        for name in ("declared-observation", "declared-auto", "restored-declaration"):
            del records[name]
        with self.assertRaises(AssertionError):
            self.validate(copy.deepcopy(records))
        summary = self.validate(records, expect="REPRODUCED", expect_remedy="NOT_MEASURED")
        self.assertEqual(summary["remedy"], "NOT_MEASURED")

    def test_absent_guest_record_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root, package, manifest = self.build(directory)
            (root / "guest" / "residue.json").unlink()
            self.reseal(root)
            with self.assertRaises(AssertionError) as caught:
                validator.validate(root, QUALIFICATION, package, manifest, "REPRODUCED")
        self.assertIn("guest record is absent: residue", str(caught.exception))


if __name__ == "__main__":
    unittest.main(verbosity=1)
