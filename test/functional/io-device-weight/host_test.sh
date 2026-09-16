#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(git -C "$script_dir" rev-parse --show-toplevel)

assert_manifest_members_tracked() {
	local manifest=$1
	local manifest_dir member relative
	manifest_dir=$(dirname -- "$manifest")
	while IFS= read -r line; do
		member=${line#*  }
		if [[ $member != ./* ]]; then
			echo "retained evidence manifest has an invalid member: $manifest: $member" >&2
			return 1
		fi
		relative=${manifest_dir#"$repo_root"/}/${member#./}
		if ! git -C "$repo_root" ls-files --error-unmatch -- "$relative" >/dev/null 2>&1; then
			echo "retained evidence manifest member is not tracked: $relative" >&2
			return 1
		fi
	done <"$manifest"
}

for script in "$script_dir/qemu-host.sh" "$script_dir/qemu-platform.sh" \
	"$script_dir/qemu-run.sh" "$script_dir/remote-qemu.sh"; do
	bash -n "$script"
done
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/guest_probe_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/guest_attribution_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_attribution_test.py"
for evidence_dir in "$script_dir"/evidence/*; do
	[[ -d $evidence_dir ]] || continue
	assert_manifest_members_tracked "$evidence_dir/SHA256SUMS"
	revision=$(awk -F= '$1 == "source_revision" {print $2}' "$evidence_dir/matrix.txt")
	PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence.py" \
		"$evidence_dir" "$revision"
done
for evidence_dir in "$script_dir"/attribution-evidence/*; do
	[[ -d $evidence_dir ]] || continue
	assert_manifest_members_tracked "$evidence_dir/SHA256SUMS"
	revision=$(awk -F= '$1 == "source_revision" {print $2}' "$evidence_dir/matrix.txt")
	PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_attribution.py" \
		"$evidence_dir" "$revision"
done

grep -q 'IODeviceWeight.*a(st)' "$script_dir/guest_probe.py"
grep -q 'SetUnitProperties.*sba(sv)' "$script_dir/validate_evidence.py"
grep -q 'platforms=(el8 el9 el10)' "$script_dir/qemu-host.sh"
grep -q 'kernel_family=rhck' "$script_dir/qemu-host.sh"
grep -q 'kernel_family=uek' "$script_dir/qemu-host.sh"
grep -q 'dnf install -y --setopt=install_weak_deps=False kernel' "$script_dir/qemu-platform.sh"
grep -q 'rpm -q --whatprovides.*vmlinuz' "$script_dir/qemu-platform.sh"
grep -q 'running kernel is not owned by kernel-core' "$script_dir/qemu-platform.sh"
grep -q 'stable -ge 3' "$script_dir/qemu-platform.sh"
grep -q 'bus=virtio,serial=' "$script_dir/qemu-platform.sh"
grep -q 'systemd.unified_cgroup_hierarchy=1' "$script_dir/qemu-platform.sh"
grep -q 'tar --no-same-owner' "$script_dir/remote-qemu.sh"
grep -q 'remote-control.sh.*control.sh' "$script_dir/remote-qemu.sh"
grep -q 'stop_remote' "$script_dir/remote-qemu.sh"
grep -q 'io-device-weight-ol8-uek-attribution' "$script_dir/remote-qemu.sh"
grep -q 'default\\n' "$script_dir/validate_attribution.py"
grep -q 'mechanism_ambiguous' "$script_dir/validate_evidence.py"
grep -q 'UNSUPPORTED.*valid platform result' "$script_dir/validate_evidence.py"
grep -q 'unsupported outcome lacks a typed reason' "$script_dir/validate_evidence.py"
grep -q 'NOT_APPLICABLE' "$script_dir/validate_evidence.py"
grep -q 'OL8/UEK' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'OL8/RHCK' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'UEK is an extension kernel available within the same release' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'are two kernel configurations of Oracle' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'Linux 9 rather than two releases' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'not a runtime allowlist' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'observed capabilities' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'probe_candidate' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'functionally_accepted' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'effect_qualified' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'tables below do not confer' "$script_dir/CAPABILITY-MATRIX.md"
grep -q 'Runtime authorization depends on observed controller' "$script_dir/README.md"
grep -q 'systemd is responsible for materializing' "$script_dir/README.md"
grep -q '252:0 121.*through BFQ' "$script_dir/CAPABILITY-MATRIX.md"
if grep -q '251:0 121.*through BFQ' "$script_dir/CAPABILITY-MATRIX.md"; then
	echo "RHCK matrix uses the UEK campaign device identity" >&2
	exit 1
fi

if grep -Eq '\b(podman|docker)\b' "$script_dir/qemu-host.sh" \
	"$script_dir/qemu-platform.sh" "$script_dir/remote-qemu.sh"; then
	echo "platform characterization must not use a shared-host-kernel container" >&2
	exit 1
fi

printf 'PASS: IODeviceWeight QEMU characterization harness contract\n'
