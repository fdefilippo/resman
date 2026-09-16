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

for script in "$script_dir/qemu-host.sh" "$script_dir/qemu-run.sh" "$script_dir/remote-qemu.sh" \
	"$script_dir/cleanup-owned.sh" "$script_dir/cleanup_owned_test.sh" \
	"$script_dir/prepare-base.sh" "$script_dir/prepare_base_test.sh"; do
	bash -n "$script"
done
"$script_dir/cleanup_owned_test.sh"
"$script_dir/prepare_base_test.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence_test.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/guest_effect_test.py"

if grep -Fq '"outcome": "EFFECT_QUALIFIED"' "$script_dir/guest_effect.py"; then
	echo "raw campaign output must not pre-claim effect qualification" >&2
	exit 1
fi

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
grep -q 'RESMAN_IO_EFFECT_MECHANISMS' "$script_dir/remote-qemu.sh"
grep -q 'mechanism_args' "$script_dir/qemu-host.sh"
grep -q 'prepare-base.sh' "$script_dir/qemu-host.sh"
grep -q 'LIBGUESTFS_BACKEND=direct' "$script_dir/prepare-base.sh"
grep -q 'LIBGUESTFS_BACKEND=direct' "$script_dir/qemu-host.sh"
grep -q 'restorecon -RF /root/.ssh' "$script_dir/qemu-host.sh"
grep -q '^reset_guest_after_smoke()' "$script_dir/qemu-host.sh"
grep -Fq '/var/lib/resman/metrics.db-wal' "$script_dir/qemu-host.sh"
grep -Fq '/var/log/resman.log' "$script_dir/qemu-host.sh"
grep -Fq "reset_guest_after_smoke >\"\$evidence_dir/profile-reset.log\"" "$script_dir/qemu-host.sh"
grep -Fq 'CONFIG_MTIME_VERIFY_ROWS' "$script_dir/guest_effect.py"

if grep -Eq '\b(podman|docker)\b' "$script_dir/qemu-host.sh" "$script_dir/remote-qemu.sh"; then
	echo "effect qualification must use the QEMU guest kernel" >&2
	exit 1
fi

for archive_dir in "$script_dir"/evidence/*; do
	[[ -d $archive_dir ]] || continue
	assert_manifest_members_tracked "$archive_dir/SHA256SUMS"
	(
		cd "$archive_dir"
		sha256sum --check --strict SHA256SUMS >/dev/null
	)
	grep -qx 'PASS' "$archive_dir/result"
	grep -qx 'cleanup=PASS' "$archive_dir/request.txt"
	grep -qx 'PASS' "$archive_dir/remote/result"
	grep -qx 'cleanup=PASS' "$archive_dir/remote/environment.txt"
	[[ $(grep -c '^mechanisms=' "$archive_dir/request.txt") -eq 1 ]]
	mechanisms=$(awk -F= '$1 == "mechanisms" {print $2}' "$archive_dir/request.txt")
	case ",$mechanisms," in
		,bfq,|,io_cost,|,bfq,io_cost,|,io_cost,bfq,) ;;
		*) echo "retained archive has an invalid mechanism selection: $mechanisms" >&2; exit 1 ;;
	esac
	evidence_dir=$archive_dir/remote/guest
	revision=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["source"]["revision"])' "$evidence_dir/summary.json")
	package_sha=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["package"]["sha256"])' "$evidence_dir/summary.json")
	mechanism_args=()
	IFS=, read -r -a selected_mechanisms <<<"$mechanisms"
	for mechanism in "${selected_mechanisms[@]}"; do
		mechanism_args+=(--mechanism "$mechanism")
	done
	PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/validate_evidence.py" "$evidence_dir" \
		--revision "$revision" --package-sha "$package_sha" "${mechanism_args[@]}"
done

printf 'PASS: weighted-I/O effect qualification harness contract\n'
