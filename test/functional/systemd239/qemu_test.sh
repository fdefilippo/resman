#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

for script in "$script_dir/qemu-host.sh" "$script_dir/remote-qemu.sh"; do
	bash -n "$script"
done
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/el8_package_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence_test.py"

grep -q 'OL8U10_x86_64-kvm-b287.qcow2' "$script_dir/qemu-host.sh"
grep -q 'cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c' "$script_dir/qemu-host.sh"
grep -q 'qemu-img create.*-b.*base_image' "$script_dir/qemu-host.sh"
grep -q 'bus=sata' "$script_dir/qemu-host.sh"
grep -q -- '--boot uefi' "$script_dir/qemu-host.sh"
grep -q 'systemd.unified_cgroup_hierarchy=1 psi=1' "$script_dir/qemu-host.sh"
grep -q 'bootloader-before/grubby-info.txt' "$script_dir/qemu-host.sh"
grep -q 'bootloader-after/grubenv.txt' "$script_dir/qemu-host.sh"
grep -q 'if grep -q.*ControlGroupId=' "$script_dir/qemu-host.sh"
grep -q "if grep -Eq '\^\[\[:space:\]\]\*ControlGroupId" "$script_dir/qemu-host.sh"
grep -q 'ControlGroupId' "$script_dir/el8_package.py"
grep -q 'negative-identity' "$script_dir/el8_package.py"
grep -q 'systemd_native_absent' "$script_dir/el8_package.py"
grep -q 'tar --no-same-owner' "$script_dir/remote-qemu.sh"
grep -q "manifest_tree.*rev-parse 'HEAD\^{tree\}'" "$script_dir/remote-qemu.sh"
grep -q 'manifest_package_sha.*sha256sum' "$script_dir/remote-qemu.sh"
grep -q 'validate_evidence.py' "$script_dir/remote-qemu.sh"

if grep -Eq '\b(podman|docker)\b' "$script_dir/qemu-host.sh" "$script_dir/remote-qemu.sh"; then
	echo "EL8 runtime qualification must not use a shared-host-kernel container" >&2
	exit 1
fi

printf 'PASS: EL8 systemd 239 QEMU harness contract\n'
