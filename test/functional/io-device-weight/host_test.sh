#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

for script in "$script_dir/qemu-host.sh" "$script_dir/qemu-platform.sh" \
	"$script_dir/remote-qemu.sh"; do
	bash -n "$script"
done
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/guest_probe_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence_test.py"

grep -q 'IODeviceWeight.*a(st)' "$script_dir/guest_probe.py"
grep -q 'SetUnitProperties.*sba(sv)' "$script_dir/validate_evidence.py"
grep -q 'for platform in el8 el9 el10' "$script_dir/qemu-host.sh"
grep -q 'bus=virtio,serial=' "$script_dir/qemu-platform.sh"
grep -q 'systemd.unified_cgroup_hierarchy=1' "$script_dir/qemu-platform.sh"
grep -q 'tar --no-same-owner' "$script_dir/remote-qemu.sh"
grep -q 'mechanism_ambiguous' "$script_dir/validate_evidence.py"
grep -q 'UNSUPPORTED.*valid platform result' "$script_dir/validate_evidence.py"

if grep -Eq '\b(podman|docker)\b' "$script_dir/qemu-host.sh" \
	"$script_dir/qemu-platform.sh" "$script_dir/remote-qemu.sh"; then
	echo "platform characterization must not use a shared-host-kernel container" >&2
	exit 1
fi

printf 'PASS: IODeviceWeight QEMU characterization harness contract\n'
