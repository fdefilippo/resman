#!/usr/bin/env bash
# Verify the contract of the EL8 cpu-delegation reproduction harness.
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

for script in "$script_dir/qemu-host.sh" "$script_dir/remote-qemu.sh"; do
	bash -n "$script"
done
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/guest_probe_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence_test.py"

# The reproduction must run on the reviewed Oracle Linux 8.10 image.
grep -q 'OL8U10_x86_64-kvm-b287.qcow2' "$script_dir/qemu-host.sh"
grep -q 'cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c' "$script_dir/qemu-host.sh"
grep -q 'cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c' \
	"$script_dir/validate_evidence.py"
grep -q -- '--boot uefi' "$script_dir/qemu-host.sh"
grep -q 'systemd.unified_cgroup_hierarchy=1 psi=1' "$script_dir/qemu-host.sh"

# The field nodes run the distribution kernel, which is the only family that
# carries the realtime group scheduling this reproduction depends on.
grep -q 'dnf install -y --setopt=install_weak_deps=False kernel' "$script_dir/qemu-host.sh"
grep -q 'selected_image != \*uek\*' "$script_dir/qemu-host.sh"
grep -q "grep -q '\^kernel-core-'" "$script_dir/qemu-host.sh"
grep -q "grep -q '\^CONFIG_RT_GROUP_SCHED=y\\\$'" "$script_dir/qemu-host.sh"
grep -q 'CONFIG_RT_GROUP_SCHED=y' "$script_dir/validate_evidence.py"

# The experiment must measure both directions, not only the refusal.
grep -q 'baseline-delegation' "$script_dir/guest_probe.py"
grep -q 'delegation-under-realtime' "$script_dir/guest_probe.py"
grep -q 'control-without-realtime' "$script_dir/guest_probe.py"
grep -q 'daemon-under-realtime' "$script_dir/guest_probe.py"
grep -q 'residue' "$script_dir/guest_probe.py"
grep -q 'chrt' "$script_dir/guest_probe.py"
grep -q 'expect_started=True' "$script_dir/validate_evidence.py"
grep -q 'expect_started=False' "$script_dir/validate_evidence.py"
grep -q 'without any realtime task outside the root' "$script_dir/validate_evidence.py"

# The realtime inventory must come from the kernel, not from a ps column: ps
# renders a kernel thread's cgroup as a dash, which once made every realtime
# kernel thread look like a task outside the root cgroup.
grep -q 'parse_task_policy' "$script_dir/guest_probe.py"
grep -q 'ROOT_CGROUPS' "$script_dir/guest_probe.py"
grep -q 'read_threads' "$script_dir/guest_probe.py"
grep -q 'Path("/proc").glob' "$script_dir/guest_probe.py"
if grep -q 'ps -eLo' "$script_dir/guest_probe.py"; then
	echo "the realtime inventory must not depend on the ps cgroup column" >&2
	exit 1
fi

# A blocked precondition must retain the evidence that justified the block.
grep -q 'save(evidence, "preconditions"' "$script_dir/guest_probe.py"

# The guest records measurements; the verdict belongs to the validator alone.
if grep -qE '"(REPRODUCED|NOT_REPRODUCED)"' "$script_dir/guest_probe.py"; then
	echo "the guest probe must not publish a campaign verdict" >&2
	exit 1
fi
grep -q 'CONFIGURATION_STATUS = 78' "$script_dir/validate_evidence.py"
grep -q 'EINVAL = 22' "$script_dir/validate_evidence.py"

# Provenance must bind the archive to one package and one revision.
grep -q 'merge-base --is-ancestor.*manifest_revision.*qualification_revision' \
	"$script_dir/remote-qemu.sh"
grep -q 'manifest_tree.*rev-parse.*manifest_revision.*tree' "$script_dir/remote-qemu.sh"
grep -q 'test/functional/cpu-delegation/\*)' "$script_dir/remote-qemu.sh"
grep -q 'package input changed after the recorded build' "$script_dir/remote-qemu.sh"
grep -q 'manifest_package_sha.*sha256sum' "$script_dir/remote-qemu.sh"
grep -q 'validate_evidence.py' "$script_dir/remote-qemu.sh"
grep -q 'tar --no-same-owner' "$script_dir/remote-qemu.sh"
grep -q 'resman-1.39.0-1.el8.x86_64' "$script_dir/qemu-host.sh"

# The remedy must be measured against the very condition that broke the host:
# all three declarations, on one guest, with one realtime task.
grep -q 'declared-observation' "$script_dir/guest_probe.py"
grep -q 'declared-auto' "$script_dir/guest_probe.py"
grep -q 'restored-declaration' "$script_dir/guest_probe.py"
grep -q 'operator_requested_observation' "$script_dir/validate_evidence.py"
grep -q 'mandatory_capability_unavailable' "$script_dir/validate_evidence.py"
grep -q 'created a capability probe unit' "$script_dir/validate_evidence.py"
grep -q -- '--expect-remedy' "$script_dir/remote-qemu.sh"

# The guest must be a distinct kernel, never a container sharing this one.
if grep -Eq '\b(podman|docker)\b' "$script_dir/qemu-host.sh" "$script_dir/remote-qemu.sh"; then
	echo "a cpu delegation reproduction must not use a shared-host-kernel container" >&2
	exit 1
fi

# The retained archive must stay intact and keep the verdict it was reviewed
# with. Recomputing the typed outcomes needs the package as well, and the
# README records that command; this gate covers integrity and provenance.
archive=$(find "$script_dir/evidence" -mindepth 1 -maxdepth 1 -type d | sort | head -n 1)
[[ -n $archive ]] || { echo "no retained cpu-delegation archive" >&2; exit 1; }
(cd "$archive" && sha256sum --quiet --check SHA256SUMS)
(cd "$archive/remote" && sha256sum --quiet --check SHA256SUMS)
grep -qx PASS "$archive/result"
grep -q '"verdict": "REPRODUCED"' "$archive/verdict.json"
grep -q '"rt_group_sched": "CONFIG_RT_GROUP_SCHED=y"' "$archive/verdict.json"
grep -q '"package_identity": "resman-1.38.0-10.el8.x86_64"' "$archive/verdict.json"
grep -q 'kernel-core-' "$archive/remote/qualified-boot/running-kernel-package.txt"

printf 'PASS: EL8 cpu delegation reproduction harness contract\n'
