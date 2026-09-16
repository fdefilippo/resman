#!/usr/bin/env bash
set -Eeuo pipefail

remote_host=${1:?usage: remote-qemu.sh ROOT_AT_QEMU_HOST}
scenario=${2:-io-device-weight-matrix}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
evidence_root=${IODEVICEWEIGHT_EVIDENCE_ROOT:-$repo_root/build/functional/io-device-weight}
if [[ $scenario == io-device-weight-ol8-uek-attribution ]]; then
	evidence_root=${IODEVICEWEIGHT_EVIDENCE_ROOT:-$repo_root/build/functional/io-device-weight-attribution}
fi
run_id=r$(date -u +%Y%m%d%H%M%S)-$$
source_revision=$(git -C "$repo_root" rev-parse HEAD)
remote_root=/tmp/resman-iow-$run_id
evidence_dir=$evidence_root/$run_id
scratch_dir=
remote_started=0
remote_execution_started=0
remote_ssh_pid=

case "$remote_host" in root@*[A-Za-z0-9.-]) ;; *) echo "a root SSH target is required" >&2; exit 2 ;; esac
[[ $scenario == io-device-weight-matrix || $scenario == io-device-weight-ol8-uek-attribution ]] \
	|| { echo "invalid QEMU scenario: $scenario" >&2; exit 2; }
[[ $source_revision =~ ^[0-9a-f]{40}$ ]] || { echo "full source revision required" >&2; exit 1; }
[[ -z $(git -C "$repo_root" status --porcelain --untracked-files=no) ]] \
	|| { echo "the tracked worktree must be clean" >&2; exit 1; }
[[ $remote_root == /tmp/resman-iow-r*-* ]] \
	|| { echo "unsafe remote root: $remote_root" >&2; exit 2; }

# Invoked by the EXIT/INT/TERM traps below.
# shellcheck disable=SC2329
cleanup() {
	local status=$? cleanup_status=PASS final_result=FAIL
	local stop_ok=1
	trap - EXIT INT TERM
	set +e
	if [[ $remote_started -eq 1 ]]; then
		if [[ $remote_execution_started -eq 1 ]]; then
			stop_remote >>"$evidence_dir/collect.log" 2>&1 || stop_ok=0
		fi
		if [[ $stop_ok -eq 1 ]]; then
			mkdir -p "$evidence_dir/remote"
			ssh -q -o BatchMode=yes "$remote_host" \
				"test ! -d '$remote_root/evidence' || tar -C '$remote_root' -cf - evidence" \
				| tar --no-same-owner -C "$evidence_dir/remote" --strip-components=1 -xf - \
				>>"$evidence_dir/collect.log" 2>&1 || { status=1; cleanup_status=FAIL; }
			ssh -q -o BatchMode=yes "$remote_host" "rm -rf -- '$remote_root'" \
				>>"$evidence_dir/collect.log" 2>&1 || { status=1; cleanup_status=FAIL; }
		else
			printf 'remote matrix could not be stopped; preserving %s\n' "$remote_root" \
				>>"$evidence_dir/collect.log"
			status=1
			cleanup_status=FAIL
		fi
	fi
	if [[ -n $remote_ssh_pid ]]; then
		kill -TERM "$remote_ssh_pid" 2>/dev/null || true
		wait "$remote_ssh_pid" 2>/dev/null || true
		remote_ssh_pid=
	fi
	if [[ -n $scratch_dir ]]; then
		case "$scratch_dir" in
			"${TMPDIR:-/tmp}"/resman-iow.*) rm -rf -- "$scratch_dir" ;;
			*) status=1; cleanup_status=FAIL ;;
		esac
	fi
	if [[ $status -eq 0 ]]; then
		validator=(python3 "$script_dir/validate_evidence.py" \
			"$evidence_dir/remote" "$source_revision" --table "$evidence_dir/capability-table.md")
		if [[ $scenario == io-device-weight-ol8-uek-attribution ]]; then
			validator=(python3 "$script_dir/validate_attribution.py" \
				"$evidence_dir/remote" "$source_revision")
		fi
		if PYTHONDONTWRITEBYTECODE=1 "${validator[@]}" >>"$evidence_dir/collect.log" 2>&1; then
			final_result=PASS
		else
			status=1
		fi
	fi
	printf 'cleanup=%s\nresult=%s\nexit_code=%d\n' "$cleanup_status" "$final_result" "$status" \
		>>"$evidence_dir/request.txt"
	printf '%s\n' "$final_result" >"$evidence_dir/result"
	(
		cd "$evidence_dir"
		manifest_tmp=$evidence_dir/../io-device-weight-SHA256SUMS.tmp
		find . -type f ! -name SHA256SUMS -print0 | sort -z \
			| xargs -0 sha256sum -- >"$manifest_tmp"
		mv "$manifest_tmp" SHA256SUMS
	)
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

control() {
	local action=$1
	ssh -q -o BatchMode=yes "$remote_host" \
		"RESMAN_REAL_KERNEL_LOCK_FILE=/run/lock/resman-iow-remote.lock RESMAN_REAL_KERNEL_STATE_FILE=/run/lock/resman-iow-remote.state RESMAN_REAL_KERNEL_STOP_TIMEOUT=120 '$remote_root/control.sh' '$action' '$run_id' '$scenario' '$source_revision'"
}

# Invoked from cleanup after an interrupted remote execution.
# shellcheck disable=SC2329
stop_remote() {
	control stop
}

run_remote() {
	control start &
	remote_ssh_pid=$!
	wait "$remote_ssh_pid"
	local status=$?
	remote_ssh_pid=
	return "$status"
}

scratch_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-iow.XXXXXX")
mkdir -p "$evidence_dir" "$scratch_dir/bundle"
chmod 0700 "$evidence_dir" "$scratch_dir/bundle"
install -m 0755 "$script_dir/qemu-host.sh" "$scratch_dir/bundle/qemu-host.sh"
install -m 0755 "$script_dir/qemu-platform.sh" "$scratch_dir/bundle/qemu-platform.sh"
install -m 0755 "$script_dir/qemu-run.sh" "$scratch_dir/bundle/run.sh"
install -m 0755 "$script_dir/../real-kernel/remote-control.sh" "$scratch_dir/bundle/control.sh"
install -m 0644 "$script_dir/guest_probe.py" "$scratch_dir/bundle/guest_probe.py"
install -m 0644 "$script_dir/guest_attribution.py" "$scratch_dir/bundle/guest_attribution.py"

{
	printf 'run_id=%s\nsource_revision=%s\nremote_host=%s\nscenario=%s\n' \
		"$run_id" "$source_revision" "$remote_host" "$scenario"
} >"$evidence_dir/request.txt"

tar -C "$scratch_dir/bundle" -cf - . \
	| ssh -q -o BatchMode=yes "$remote_host" \
		"test ! -e '$remote_root' && install -d -m 0700 '$remote_root' && tar --no-same-owner -C '$remote_root' -xf -"
remote_started=1
set +e
remote_execution_started=1
run_remote
remote_status=$?
remote_execution_started=0
set -e
exit "$remote_status"
