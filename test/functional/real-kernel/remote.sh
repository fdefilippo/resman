#!/usr/bin/env bash
set -Eeuo pipefail

scenario=${1:?scenario is required}
remote_host=${2:-${RESMAN_REAL_KERNEL_HOST:-}}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
go_bin=${GO_BIN:-go}
evidence_root=${REAL_KERNEL_EVIDENCE_ROOT:-$repo_root/build/functional/real-kernel}
scratch_dir=
remote_root=
evidence_dir=
remote_cleanup_done=0

# Source-revision scenarios ship a binary built from the working tree. Packaged
# scenarios exercise the installed unit instead and must not ship one, so that a
# PASS can never be read as coverage of the wrong artifact.
scenario_family=source
case "$scenario" in
	psi-refresh-neutrality|block-io-all-dimensions) ;;
	service-start-stop|service-reload-lifecycle|service-fatal-config) scenario_family=package ;;
	prometheus-scrape|mcp-https-endtoend) scenario_family=package ;;
	blackout-timeframe|metrics-database-lifecycle) scenario_family=package ;;
	multi-user-enforcement|shutdown-restoration-under-load) scenario_family=package ;;
	limit-hook-delivery) scenario_family=package ;;
	*) echo "invalid scenario: $scenario" >&2; exit 2 ;;
esac
case "$remote_host" in
	''|*[!A-Za-z0-9_.@-]*)
		echo "a simple user@host real-kernel target is required" >&2
		exit 2
		;;
esac

safe_remove_scratch() {
	[[ -n $scratch_dir ]] || return 0
	case "$scratch_dir" in
		"${TMPDIR:-/tmp}"/resman-real-kernel.*) rm -rf -- "$scratch_dir" ;;
		*) echo "refusing to remove unexpected scratch path: $scratch_dir" >&2; return 1 ;;
	esac
}

cleanup() {
	local status=$?
	local cleanup_log=/dev/null
	trap - EXIT INT TERM
	set +e
	[[ -z $evidence_dir ]] || cleanup_log=$evidence_dir/remote-cleanup.log
	if [[ $remote_cleanup_done -eq 0 && -n $remote_root ]]; then
		ssh -q -o BatchMode=yes "$remote_host" "rm -rf -- '$remote_root'" \
			>>"$cleanup_log" 2>&1 || status=1
	fi
	safe_remove_scratch || status=1
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

tracked_changes=$(git -C "$repo_root" status --porcelain --untracked-files=no)
if [[ ${REAL_KERNEL_ALLOW_DIRTY:-0} != 1 && -n $tracked_changes ]]; then
	echo "the tracked worktree must be clean before collecting revision-bound evidence" >&2
	exit 1
fi

run_id=r$(date -u +%Y%m%d%H%M%S)-$$
source_revision=$(git -C "$repo_root" rev-parse HEAD)
if [[ -n $tracked_changes ]]; then
	worktree_digest=$(git -C "$repo_root" diff HEAD | sha256sum)
	worktree_digest=${worktree_digest%% *}
	source_revision=worktree-$worktree_digest
fi
scratch_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-real-kernel.XXXXXX")
bundle_dir=$scratch_dir/bundle
mkdir -p "$bundle_dir" "$evidence_root"
chmod 0700 "$scratch_dir" "$bundle_dir"
if [[ $scenario_family == package ]]; then
	install -m 0755 "$script_dir/service-run.sh" "$bundle_dir/run.sh"
	install -m 0755 "$repo_root/docs/generate-tls-certs.sh" "$bundle_dir/generate-tls-certs.sh"
else
	install -m 0755 "$script_dir/run.sh" "$bundle_dir/run.sh"
	install -m 0755 "$script_dir/workload.py" "$bundle_dir/workload.py"
	(
		cd "$repo_root"
		CGO_ENABLED=1 "$go_bin" build -trimpath -o "$bundle_dir/resman" ./main.go
	)
fi

evidence_dir=$evidence_root/$run_id-$scenario
mkdir "$evidence_dir"
chmod 0700 "$evidence_dir"
remote_root=/tmp/resman-final-$run_id
commands_log=$evidence_dir/commands.log
{
	printf 'tar -C %q -cf - . | ssh -q -o BatchMode=yes %q %q\n' \
		"$bundle_dir" "$remote_host" "install -d -m 0700 '$remote_root' && tar -C '$remote_root' -xf -"
	printf 'ssh -q -o BatchMode=yes %q %q\n' "$remote_host" \
		"'$remote_root/run.sh' '$scenario' '$run_id' '$source_revision'"
	printf 'ssh -q -o BatchMode=yes %q %q | tar -C %q --strip-components=1 -xf -\n' \
		"$remote_host" "tar -C '$remote_root' -cf - evidence" "$evidence_dir"
} >"$commands_log"

tar -C "$bundle_dir" -cf - . \
	| ssh -q -o BatchMode=yes "$remote_host" \
		"install -d -m 0700 '$remote_root' && tar -C '$remote_root' -xf -"

set +e
ssh -q -o BatchMode=yes "$remote_host" \
	"'$remote_root/run.sh' '$scenario' '$run_id' '$source_revision'"
remote_status=$?
set -e

ssh -q -o BatchMode=yes "$remote_host" "tar -C '$remote_root' -cf - evidence" \
	| tar -C "$evidence_dir" --strip-components=1 -xf -
{
	printf 'requested_host=%s\n' "$remote_host"
	printf 'local_source_revision=%s\n' "$source_revision"
	printf 'remote_exit_code=%d\n' "$remote_status"
} >>"$evidence_dir/environment.txt"

ssh -q -o BatchMode=yes "$remote_host" "rm -rf -- '$remote_root'"
remote_cleanup_done=1
safe_remove_scratch
scratch_dir=
trap - EXIT INT TERM

if [[ $remote_status -ne 0 || $(< "$evidence_dir/result") != PASS ]]; then
	echo "FAIL: real-kernel scenario failed; evidence: $evidence_dir" >&2
	exit "${remote_status:-1}"
fi
grep -q '^cleanup=PASS$' "$evidence_dir/environment.txt" \
	|| { echo "FAIL: remote cleanup was not verified; evidence: $evidence_dir" >&2; exit 1; }

echo "PASS: real-kernel $scenario"
echo "Evidence: $evidence_dir"
