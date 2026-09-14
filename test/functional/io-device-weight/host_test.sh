#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

for script in "$script_dir/qemu-host.sh" "$script_dir/qemu-platform.sh" \
	"$script_dir/qemu-run.sh" "$script_dir/remote-qemu.sh"; do
	bash -n "$script"
done
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/guest_probe_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence_test.py"
for evidence_dir in "$script_dir"/evidence/*; do
	[[ -d $evidence_dir ]] || continue
	revision=$(awk -F= '$1 == "source_revision" {print $2}' "$evidence_dir/matrix.txt")
	PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence.py" \
		"$evidence_dir" "$revision"
done

grep -q 'IODeviceWeight.*a(st)' "$script_dir/guest_probe.py"
grep -q 'SetUnitProperties.*sba(sv)' "$script_dir/validate_evidence.py"
grep -q 'for platform in el8 el9 el10' "$script_dir/qemu-host.sh"
grep -q 'qemu-platform.sh.*rhck' "$script_dir/qemu-host.sh"
grep -q 'dnf install -y --setopt=install_weak_deps=False kernel' "$script_dir/qemu-platform.sh"
grep -q 'rpm -q --whatprovides.*vmlinuz' "$script_dir/qemu-platform.sh"
grep -q 'running kernel is not owned by kernel-core' "$script_dir/qemu-platform.sh"
grep -q 'stable -ge 3' "$script_dir/qemu-platform.sh"
grep -q 'bus=virtio,serial=' "$script_dir/qemu-platform.sh"
grep -q 'systemd.unified_cgroup_hierarchy=1' "$script_dir/qemu-platform.sh"
grep -q 'tar --no-same-owner' "$script_dir/remote-qemu.sh"
grep -q 'remote-control.sh.*control.sh' "$script_dir/remote-qemu.sh"
grep -q 'stop_remote' "$script_dir/remote-qemu.sh"
grep -q 'mechanism_ambiguous' "$script_dir/validate_evidence.py"
grep -q 'UNSUPPORTED.*valid platform result' "$script_dir/validate_evidence.py"
grep -q 'unsupported outcome lacks a typed reason' "$script_dir/validate_evidence.py"
grep -q 'NOT_APPLICABLE' "$script_dir/validate_evidence.py"
grep -q 'OL8/UEK' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'RHCK combinations' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'remain uncharacterized' "$script_dir/CAPABILITY-MATRIX.md"

if grep -Eq '\b(podman|docker)\b' "$script_dir/qemu-host.sh" \
	"$script_dir/qemu-platform.sh" "$script_dir/remote-qemu.sh"; then
	echo "platform characterization must not use a shared-host-kernel container" >&2
	exit 1
fi

printf 'PASS: IODeviceWeight QEMU characterization harness contract\n'
