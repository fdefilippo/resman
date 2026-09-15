#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

for script in "$script_dir/qemu-host.sh" "$script_dir/remote-qemu.sh"; do
	bash -n "$script"
done
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/guest_effect_test.py"

grep -q 'packaged-daemon-controlled-contention' "$script_dir/validate_evidence.py"
grep -q 'daemon_mutated_scheduler_or_iocost' "$script_dir/validate_evidence.py"
grep -q 'reversed' "$script_dir/validate_evidence.py"
grep -q 'restart_recovery' "$script_dir/validate_evidence.py"
grep -q 'compare_before_restore' "$script_dir/validate_evidence.py"
grep -q 'kernel-5.14.0-687.46.1.el9_8' "$script_dir/qemu-host.sh"
grep -q 'bus=virtio,serial=' "$script_dir/qemu-host.sh"
grep -q 'tar --no-same-owner' "$script_dir/remote-qemu.sh"

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
