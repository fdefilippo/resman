#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

for script in "$script_dir/qemu-host.sh" "$script_dir/qemu-run.sh" "$script_dir/remote-qemu.sh" \
	"$script_dir/cleanup-owned.sh" "$script_dir/cleanup_owned_test.sh" \
	"$script_dir/prepare-base.sh" "$script_dir/prepare_base_test.sh"; do
	bash -n "$script"
done
"$script_dir/cleanup_owned_test.sh"
"$script_dir/prepare_base_test.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/guest_effect_test.py"

grep -q 'packaged-daemon-controlled-contention' "$script_dir/validate_evidence.py"
grep -q 'daemon_mutated_scheduler_or_iocost' "$script_dir/validate_evidence.py"
grep -q 'reversed' "$script_dir/validate_evidence.py"
grep -q 'restart_recovery' "$script_dir/validate_evidence.py"
grep -q 'compare_before_restore' "$script_dir/validate_evidence.py"
grep -Fq 'path = first["path"]' "$script_dir/guest_effect.py"
grep -q 'self.signal_workloads("STOP")' "$script_dir/guest_effect.py"
grep -q 'mechanisms\["io_cost"\].*mechanism_campaign' "$script_dir/guest_effect.py"
grep -q 'kernel_release=.*5.14.0-687.46.1.el9_8' "$script_dir/prepare-base.sh"
grep -q 'bus=virtio,serial=' "$script_dir/qemu-host.sh"
grep -q 'tar --no-same-owner' "$script_dir/remote-qemu.sh"
grep -q 'remote-control.sh.*control.sh' "$script_dir/remote-qemu.sh"
grep -q 'stop_remote' "$script_dir/remote-qemu.sh"
grep -q 'cleanup_remote_owned_run' "$script_dir/remote-qemu.sh"
grep -q 'prepare-base.sh' "$script_dir/qemu-host.sh"
grep -q 'LIBGUESTFS_BACKEND=direct' "$script_dir/prepare-base.sh"
grep -q 'LIBGUESTFS_BACKEND=direct' "$script_dir/qemu-host.sh"
grep -q 'restorecon -RF /root/.ssh' "$script_dir/qemu-host.sh"

if grep -Eq '\b(podman|docker)\b' "$script_dir/qemu-host.sh" "$script_dir/remote-qemu.sh"; then
	echo "effect qualification must use the QEMU guest kernel" >&2
	exit 1
fi

for evidence_dir in "$script_dir"/evidence/*; do
	[[ -d $evidence_dir ]] || continue
	revision=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["source"]["revision"])' "$evidence_dir/summary.json")
	package_sha=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["package"]["sha256"])' "$evidence_dir/summary.json")
	PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence.py" "$evidence_dir" \
		--revision "$revision" --package-sha "$package_sha"
done

printf 'PASS: weighted-I/O effect qualification harness contract\n'
