#!/usr/bin/env bash
set -Eeuo pipefail

run_id=${1:?run ID is required}
source_revision=${2:?source revision is required}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$script_dir/evidence
result=FAIL
detail="matrix did not complete"

[[ $run_id =~ ^r[0-9]{14}-[0-9]+$ ]] || { echo "unsafe run ID: $run_id" >&2; exit 2; }
[[ $source_revision =~ ^[0-9a-f]{40}$ ]] || { echo "full source revision required" >&2; exit 2; }
[[ ! -e $evidence_dir ]] || { echo "evidence directory already exists" >&2; exit 75; }
mkdir -m 0700 "$evidence_dir"

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	if [[ $status -eq 75 || $status -eq 77 ]] && [[ $result == FAIL ]]; then
		result=BLOCKED
		detail="matrix infrastructure was unavailable or invalid"
	fi
	printf 'result=%s\ndetail=%s\nexit_code=%d\n' "$result" "$detail" "$status" \
		>>"$evidence_dir/matrix.txt"
	printf '%s\n' "$result" >"$evidence_dir/result"
	(
		cd "$evidence_dir"
		manifest_tmp=$script_dir/matrix-SHA256SUMS.tmp
		find . -type f ! -name SHA256SUMS -print0 | sort -z \
			| xargs -0 sha256sum -- >"$manifest_tmp"
		mv "$manifest_tmp" SHA256SUMS
	)
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

exec 9>/run/lock/resman-iodeviceweight-qemu.lock
flock -n 9 || { echo "another IODeviceWeight QEMU matrix owns this host" >&2; exit 75; }

{
	printf 'run_id=%s\nsource_revision=%s\nhost=%s\n' "$run_id" "$source_revision" "$(hostname -f)"
	printf 'host_kernel=%s\n' "$(uname -r)"
	printf 'qemu_version=%s\n' "$(qemu-system-x86_64 --version | head -n 1)"
	printf 'libvirt_version=%s\n' "$(virsh version --daemon 2>/dev/null | tr '\n' ' ')"
} >"$evidence_dir/matrix.txt"

blocked=0
failed=0
for platform in el8 el9 el10; do
	set +e
	"$script_dir/qemu-platform.sh" "$platform" "$run_id" "$source_revision"
	status=$?
	set -e
	case "$status" in
		0) ;;
		77) blocked=1 ;;
		*) failed=1 ;;
	esac
done

if [[ $failed -eq 1 ]]; then
	result=FAIL
	detail="one or more platform runs failed"
	exit 1
fi
if [[ $blocked -eq 1 ]]; then
	result=BLOCKED
	detail="one or more platform representatives were unavailable or invalid"
	exit 77
fi
result=PASS
detail="EL8, EL9 and EL10 produced valid IODeviceWeight characterization"
