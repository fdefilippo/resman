#!/usr/bin/env bash
set -Eeuo pipefail

remote_host=${1:?usage: remote-qemu.sh ROOT_AT_QEMU_HOST RPM BUILD_MANIFEST}
package=${2:?usage: remote-qemu.sh ROOT_AT_QEMU_HOST RPM BUILD_MANIFEST}
build_manifest=${3:?usage: remote-qemu.sh ROOT_AT_QEMU_HOST RPM BUILD_MANIFEST}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
evidence_root=${IODEVICEWEIGHT_EFFECT_EVIDENCE_ROOT:-$repo_root/build/functional/io-device-weight-effect}
qualification_mechanisms=${RESMAN_IO_EFFECT_MECHANISMS:-bfq,io_cost}
run_id=r$(date -u +%Y%m%d%H%M%S)-$$
qualification_revision=$(git -C "$repo_root" rev-parse HEAD)
qualification_tree=$(git -C "$repo_root" rev-parse 'HEAD^{tree}')
source_revision=$(awk -F= '$1 == "source_revision" {print $2}' "$build_manifest")
source_tree=$(awk -F= '$1 == "source_tree" {print $2}' "$build_manifest")
remote_root=/tmp/resman-iow-effect-$run_id
evidence_dir=$evidence_root/$run_id
scratch_dir=
remote_started=0
remote_execution_started=0
remote_ssh_pid=

case ",$qualification_mechanisms," in
	,bfq,|,io_cost,|,bfq,io_cost,|,io_cost,bfq,) ;;
	*) echo "RESMAN_IO_EFFECT_MECHANISMS must select bfq, io_cost, or both exactly once" >&2; exit 2 ;;
esac

case "$remote_host" in root@*[A-Za-z0-9.-]) ;; *) echo "a root SSH target is required" >&2; exit 2 ;; esac
[[ -f $package && ! -L $package && -f $build_manifest && ! -L $build_manifest ]] \
	|| { echo "regular RPM and build manifest are required" >&2; exit 2; }
[[ -z $(git -C "$repo_root" status --porcelain --untracked-files=no) ]] \
	|| { echo "the worktree must be clean" >&2; exit 1; }
[[ $source_revision =~ ^[0-9a-f]{40}$ && $source_tree =~ ^[0-9a-f]{40}$ ]] \
	|| { echo "build manifest lacks immutable source identity" >&2; exit 1; }
git -C "$repo_root" cat-file -e "$source_revision^{commit}"
[[ $(git -C "$repo_root" rev-parse "$source_revision^{tree}") == "$source_tree" ]] \
	|| { echo "build manifest source tree differs" >&2; exit 1; }
git -C "$repo_root" merge-base --is-ancestor "$source_revision" "$qualification_revision" \
	|| { echo "package source is not an ancestor of qualification source" >&2; exit 1; }
while IFS= read -r -d '' changed_path; do
	case "$changed_path" in test/functional/io-device-weight-effect/*) ;;
		*) echo "package input changed after build: $changed_path" >&2; exit 1 ;;
	esac
done < <(git -C "$repo_root" diff --name-only -z --no-renames "$source_revision..$qualification_revision")
[[ $(awk -F= '$1 == "package_sha256" {print $2}' "$build_manifest") == "$(sha256sum "$package" | awk '{print $1}')" ]] \
	|| { echo "build manifest does not identify the supplied RPM" >&2; exit 1; }
[[ $remote_root == /tmp/resman-iow-effect-r*-* ]] || { echo "unsafe remote root" >&2; exit 2; }

control() {
	local action=$1
	ssh -q -o BatchMode=yes "$remote_host" \
		"RESMAN_REAL_KERNEL_LOCK_FILE=/run/lock/resman-iodeviceweight-effect-remote.lock RESMAN_REAL_KERNEL_STATE_FILE=/run/lock/resman-iodeviceweight-effect-remote.state RESMAN_REAL_KERNEL_STOP_TIMEOUT=120 '$remote_root/control.sh' '$action' '$run_id' io-device-weight-effect '$qualification_revision'"
}

# Invoked from the cleanup trap after an interrupted remote execution.
# shellcheck disable=SC2329
stop_remote() {
	control stop
}

# Invoked from the cleanup trap after the remote controller has stopped.
# shellcheck disable=SC2329
cleanup_remote_owned_run() {
	ssh -q -o BatchMode=yes "$remote_host" \
		"'$remote_root/cleanup-owned.sh' '$run_id'"
}

run_remote() {
	control start &
	remote_ssh_pid=$!
	wait "$remote_ssh_pid"
	local status=$?
	remote_ssh_pid=
	return "$status"
}

# Invoked by the EXIT, INT, and TERM traps below.
# shellcheck disable=SC2329
cleanup() {
	local status=$? cleanup_status=PASS final_result=FAIL
	trap - EXIT INT TERM
	set +e
	if [[ $remote_started -eq 1 ]]; then
		if [[ $remote_execution_started -eq 1 ]]; then
			stop_remote >>"$evidence_dir/collect.log" 2>&1 || { status=1; cleanup_status=FAIL; }
		fi
		cleanup_remote_owned_run >>"$evidence_dir/collect.log" 2>&1 \
			|| { status=1; cleanup_status=FAIL; }
		mkdir -p "$evidence_dir/remote"
		ssh -q -o BatchMode=yes "$remote_host" \
			"test ! -d '$remote_root/evidence' || tar -C '$remote_root/evidence' -cf - ." \
			| tar --no-same-owner -C "$evidence_dir/remote" -xf - \
			>>"$evidence_dir/collect.log" 2>&1 || { status=1; cleanup_status=FAIL; }
		ssh -q -o BatchMode=yes "$remote_host" "rm -rf -- '$remote_root'" \
			>>"$evidence_dir/collect.log" 2>&1 || { status=1; cleanup_status=FAIL; }
	fi
	if [[ -n $remote_ssh_pid ]]; then
		kill -TERM "$remote_ssh_pid" 2>/dev/null || true
		wait "$remote_ssh_pid" 2>/dev/null || true
		remote_ssh_pid=
	fi
	if [[ -n $scratch_dir ]]; then
		case "$scratch_dir" in
			"${TMPDIR:-/tmp}"/resman-iow-effect.*) rm -rf -- "$scratch_dir" ;;
			*) status=1; cleanup_status=FAIL ;;
		esac
	fi
	if [[ $status -eq 0 ]]; then
		mechanism_args=()
		IFS=, read -r -a selected_mechanisms <<<"$qualification_mechanisms"
		for mechanism in "${selected_mechanisms[@]}"; do
			mechanism_args+=(--mechanism "$mechanism")
		done
		python3 "$script_dir/validate_evidence.py" "$evidence_dir/remote/guest" \
			--profile qualification --revision "$source_revision" \
			--package-sha "$(sha256sum "$package" | awk '{print $1}')" \
			"${mechanism_args[@]}" \
			>>"$evidence_dir/collect.log" 2>&1 && final_result=PASS || status=1
	fi
	printf 'cleanup=%s\nresult=%s\nexit_code=%d\n' "$cleanup_status" "$final_result" "$status" \
		>>"$evidence_dir/request.txt"
	printf '%s\n' "$final_result" >"$evidence_dir/result"
	(
		cd "$evidence_dir"
		manifest_tmp=$evidence_root/effect-SHA256SUMS.tmp
		find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum -- >"$manifest_tmp"
		mv "$manifest_tmp" SHA256SUMS
	)
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

scratch_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-iow-effect.XXXXXX")
mkdir -p "$evidence_dir" "$scratch_dir/bundle"
chmod 0700 "$evidence_dir" "$scratch_dir/bundle"
install -m 0600 "$package" "$scratch_dir/bundle/package.rpm"
install -m 0600 "$build_manifest" "$scratch_dir/bundle/build-manifest.txt"
install -m 0755 "$script_dir/qemu-host.sh" "$scratch_dir/bundle/qemu-host.sh"
install -m 0755 "$script_dir/qemu-run.sh" "$scratch_dir/bundle/run.sh"
install -m 0755 "$script_dir/cleanup-owned.sh" "$scratch_dir/bundle/cleanup-owned.sh"
install -m 0755 "$script_dir/prepare-base.sh" "$scratch_dir/bundle/prepare-base.sh"
install -m 0755 "$script_dir/../real-kernel/remote-control.sh" "$scratch_dir/bundle/control.sh"
install -m 0755 "$script_dir/guest_effect.py" "$scratch_dir/bundle/guest_effect.py"
install -m 0755 "$script_dir/validate_evidence.py" "$scratch_dir/bundle/validate_evidence.py"
printf 'qualification_tree=%s\nmechanisms=%s\n' "$qualification_tree" "$qualification_mechanisms" \
	>"$scratch_dir/bundle/qualification.txt"

{
	printf 'run_id=%s\nsource_revision=%s\nsource_tree=%s\n' "$run_id" "$source_revision" "$source_tree"
	printf 'qualification_revision=%s\nqualification_tree=%s\nremote_host=%s\n' \
		"$qualification_revision" "$qualification_tree" "$remote_host"
	printf 'mechanisms=%s\n' "$qualification_mechanisms"
	printf 'package_sha256=%s\n' "$(sha256sum "$package" | awk '{print $1}')"
} >"$evidence_dir/request.txt"

tar -C "$scratch_dir/bundle" -cf - . | ssh -q -o BatchMode=yes "$remote_host" \
	"test ! -e '$remote_root' && install -d -m 0700 '$remote_root' && tar --no-same-owner -C '$remote_root' -xf -"
remote_started=1
set +e
remote_execution_started=1
run_remote
remote_status=$?
remote_execution_started=0
set -e
exit "$remote_status"
