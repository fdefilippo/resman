#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
go_bin=${GO_BIN:-go}
evidence_root=${REAL_KERNEL_EVIDENCE_ROOT:-$repo_root/build/functional/real-kernel}
scenario=
remote_host=
scratch_dir=
remote_root=
evidence_dir=
run_id=
remote_cleanup_done=0
remote_execution_started=0
remote_ssh_pid=

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
	local stop_ok=1
	trap - EXIT INT TERM
	set +e
	[[ -z $evidence_dir ]] || cleanup_log=$evidence_dir/remote-cleanup.log
	if [[ $remote_cleanup_done -eq 0 && -n $remote_root ]]; then
		if [[ $remote_execution_started -eq 1 ]]; then
			stop_remote_scenario >>"$cleanup_log" 2>&1 || stop_ok=0
		fi
		if [[ $stop_ok -eq 1 ]]; then
			collect_remote_evidence >>"$cleanup_log" 2>&1 || true
			ssh -q -o BatchMode=yes "$remote_host" "rm -rf -- '$remote_root'" \
				>>"$cleanup_log" 2>&1 || status=1
		else
			echo "remote scenario could not be stopped; preserving $remote_root" \
				>>"$cleanup_log"
			status=1
		fi
	fi
	if [[ -n $remote_ssh_pid ]]; then
		kill -TERM "$remote_ssh_pid" 2>/dev/null || true
		wait "$remote_ssh_pid" 2>/dev/null || true
		remote_ssh_pid=
	fi
	safe_remove_scratch || status=1
	exit "$status"
}

stop_remote_scenario() {
	ssh -q -o BatchMode=yes "$remote_host" \
		"'$remote_root/control.sh' stop '$run_id'"
}

collect_remote_evidence() {
	[[ -n $evidence_dir && -d $evidence_dir ]] || return 0
	ssh -q -o BatchMode=yes "$remote_host" \
		"test ! -d '$remote_root/evidence' || tar -C '$remote_root' -cf - evidence" \
		| tar -C "$evidence_dir" --strip-components=1 -xf -
}

remote_blocked_kind() {
	case "$1" in
		75) printf 'exclusion-lock\n' ;;
		77) printf 'scenario-preflight\n' ;;
		*) return 1 ;;
	esac
}

run_remote_scenario() {
	ssh -q -o BatchMode=yes "$remote_host" \
		"'$remote_root/control.sh' start '$run_id' '$scenario' '$source_revision'" &
	remote_ssh_pid=$!
	wait "$remote_ssh_pid"
	local status=$?
	remote_ssh_pid=
	return "$status"
}

write_hotplug_opt_in() {
	local directory=$1 intent=$2
	case "$intent" in
		0) ;;
		1) printf 'yes\n' >"$directory/allow-cpu-hotplug" ;;
		*) echo "REAL_KERNEL_ALLOW_CPU_HOTPLUG must be exactly 0 or 1" >&2; return 2 ;;
	esac
	printf '%s\n' "$intent" >"$directory/hotplug-intent"
}

if [[ ${RESMAN_REAL_KERNEL_REMOTE_LIBRARY_ONLY:-0} == 1 ]]; then
	if [[ ${BASH_SOURCE[0]} != "$0" ]]; then
		return 0
	fi
	exit 0
fi

scenario=${1:?scenario is required}
remote_host=${2:-${RESMAN_REAL_KERNEL_HOST:-}}

# Source-revision scenarios ship a binary built from the working tree. Packaged
# scenarios exercise the installed unit instead and must not ship one, so that a
# PASS can never be read as coverage of the wrong artifact.
scenario_family=source
case "$scenario" in
	psi-refresh-neutrality|block-io-all-dimensions|cpu-points-proportional|systemd-ownership-preservation|systemd-native-lifecycle) ;;
	systemd-native-proportional|systemd-native-reference|systemd-native-reference-pinned|systemd-native-reference-pinned-six) ;;
	systemd-native-coverage|systemd-native-recovery|systemd-native-reconciliation|systemd-native-weighted-io-adapter) ;;
	native-package-acceptance) ;;
	service-start-stop|service-reload-lifecycle|service-fatal-config) scenario_family=package ;;
	prometheus-scrape|mcp-https-endtoend) scenario_family=package ;;
	prometheus-user-series-lifecycle) scenario_family=package ;;
	pid-namespace-mixed-ingress|pid-namespace-container-only) scenario_family=package ;;
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
install -m 0644 "$script_dir/../final/systemd-containment-dispositions.tsv" \
	"$bundle_dir/systemd-containment-dispositions.tsv"
if [[ $scenario_family == package ]]; then
	install -m 0755 "$script_dir/service-run.sh" "$bundle_dir/run.sh"
	install -m 0755 "$repo_root/docs/generate-tls-certs.sh" "$bundle_dir/generate-tls-certs.sh"
else
	if [[ $scenario == systemd-native-* || $scenario == native-package-acceptance ]]; then
		install -m 0755 "$script_dir/native-run.sh" "$bundle_dir/run.sh"
		install -m 0644 "$script_dir/native_gate.py" "$script_dir/native-workload.py" "$bundle_dir/"
		install -m 0644 "$script_dir/native_proportional.py" "$bundle_dir/"
		install -m 0644 "$script_dir/native_reference.py" "$bundle_dir/"
		install -m 0644 "$script_dir/native_placement.py" "$bundle_dir/"
		install -m 0644 "$script_dir/native_coverage.py" "$bundle_dir/"
		if [[ $scenario == systemd-native-reconciliation ]]; then
			install -m 0644 "$script_dir/native_reconciliation.py" "$bundle_dir/"
			write_hotplug_opt_in "$bundle_dir" "${REAL_KERNEL_ALLOW_CPU_HOTPLUG:-0}"
		fi
		if [[ $scenario == systemd-native-recovery || $scenario == systemd-native-weighted-io-adapter ]]; then
			install -m 0644 "$script_dir/native_recovery.py" "$bundle_dir/"
			(cd "$repo_root" && CGO_ENABLED=1 "$go_bin" test -c -o "$bundle_dir/systemdunit-real.test" ./internal/systemdunit)
		fi
		if [[ $scenario == systemd-native-weighted-io-adapter ]]; then
			install -m 0644 "$script_dir/native_weighted_io.py" "$script_dir/weighted-io-workload.py" \
				"$script_dir/null_blk_characterization.py" "$bundle_dir/"
		fi
		if [[ $scenario == native-package-acceptance ]]; then
			[[ -n ${REAL_KERNEL_PACKAGE:-} && -f $REAL_KERNEL_PACKAGE && ! -L $REAL_KERNEL_PACKAGE ]] || {
				echo "native-package-acceptance requires REAL_KERNEL_PACKAGE naming a regular RPM file" >&2
				exit 77
			}
			install -m 0644 "$script_dir/native_package.py" "$bundle_dir/"
			install -m 0644 "$REAL_KERNEL_PACKAGE" "$bundle_dir/package.rpm"
		fi
		install -m 0600 "$repo_root/config/resman.conf.example" "$bundle_dir/resman.conf.example"
	elif [[ $scenario == cpu-points-proportional ]]; then
		install -m 0755 "$script_dir/cpu-points-run.sh" "$bundle_dir/run.sh"
	elif [[ $scenario == systemd-ownership-preservation ]]; then
		install -m 0755 "$script_dir/ownership-run.sh" "$bundle_dir/run.sh"
	else
		install -m 0755 "$script_dir/run.sh" "$bundle_dir/run.sh"
		install -m 0644 "$script_dir/native_psi_evidence.py" "$bundle_dir/"
		install -m 0755 "$script_dir/workload.py" "$bundle_dir/workload.py"
	fi
	install -m 0755 "$script_dir/service-run.sh" "$bundle_dir/service-run.sh"
	if [[ $scenario != native-package-acceptance ]]; then
		(
			cd "$repo_root"
			CGO_ENABLED=1 "$go_bin" build -trimpath -o "$bundle_dir/resman" ./main.go
		)
	fi
fi
install -m 0755 "$script_dir/remote-control.sh" "$bundle_dir/control.sh"

evidence_dir=$evidence_root/$run_id-$scenario
mkdir "$evidence_dir"
chmod 0700 "$evidence_dir"
remote_root=/tmp/resman-final-$run_id
commands_log=$evidence_dir/commands.log
{
	printf 'tar -C %q -cf - . | ssh -q -o BatchMode=yes %q %q\n' \
		"$bundle_dir" "$remote_host" "install -d -m 0700 '$remote_root' && tar --no-same-owner -C '$remote_root' -xf -"
	printf 'ssh -q -o BatchMode=yes %q %q\n' "$remote_host" \
		"'$remote_root/control.sh' start '$run_id' '$scenario' '$source_revision'"
	printf 'ssh -q -o BatchMode=yes %q %q\n' "$remote_host" \
		"'$remote_root/control.sh' stop '$run_id'"
	printf 'ssh -q -o BatchMode=yes %q %q | tar -C %q --strip-components=1 -xf -\n' \
		"$remote_host" "tar -C '$remote_root' -cf - evidence" "$evidence_dir"
} >"$commands_log"

tar -C "$bundle_dir" -cf - . \
	| ssh -q -o BatchMode=yes "$remote_host" \
		"install -d -m 0700 '$remote_root' && tar --no-same-owner -C '$remote_root' -xf -"

set +e
remote_execution_started=1
run_remote_scenario
remote_status=$?
set -e

if [[ $remote_status -eq 255 ]]; then
	stop_remote_scenario || {
		echo "FAIL: SSH disconnected and remote run $run_id could not be stopped" >&2
		exit 1
	}
fi
remote_execution_started=0

if [[ $(remote_blocked_kind "$remote_status" 2>/dev/null || true) == exclusion-lock ]]; then
	printf 'BLOCKED\n' >"$evidence_dir/result"
	{
		printf 'requested_host=%s\n' "$remote_host"
		printf 'local_source_revision=%s\n' "$source_revision"
		printf 'remote_exit_code=%d\n' "$remote_status"
		printf 'result=BLOCKED\n'
		printf 'detail=another real-kernel scenario or bundle is active\n'
	} >>"$evidence_dir/environment.txt"
	ssh -q -o BatchMode=yes "$remote_host" "rm -rf -- '$remote_root'"
	remote_cleanup_done=1
	safe_remove_scratch
	scratch_dir=
	trap - EXIT INT TERM
	echo "BLOCKED: real-kernel $scenario; evidence: $evidence_dir" >&2
	exit 77
fi

if [[ $(remote_blocked_kind "$remote_status" 2>/dev/null || true) == scenario-preflight ]]; then
	collect_remote_evidence
	[[ -r $evidence_dir/result && $(< "$evidence_dir/result") == BLOCKED ]] \
		|| { printf 'BLOCKED\n' >"$evidence_dir/result"; }
	{
		printf 'requested_host=%s\n' "$remote_host"
		printf 'local_source_revision=%s\n' "$source_revision"
		printf 'remote_exit_code=%d\n' "$remote_status"
	} >>"$evidence_dir/environment.txt"
	remote_detail=$(sed -n 's/^detail=//p' "$evidence_dir/environment.txt" | tail -n 1)
	[[ -n $remote_detail ]] || remote_detail="remote scenario reported BLOCKED without detail"
	ssh -q -o BatchMode=yes "$remote_host" "rm -rf -- '$remote_root'"
	remote_cleanup_done=1
	safe_remove_scratch
	scratch_dir=
	trap - EXIT INT TERM
	echo "BLOCKED: real-kernel $scenario: $remote_detail; evidence: $evidence_dir" >&2
	exit 77
fi

collect_remote_evidence
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

if [[ $remote_status -eq 0 && $scenario == systemd-native-reference && $(< "$evidence_dir/result") == CHARACTERIZATION ]]; then
	grep -q '^cleanup=PASS$' "$evidence_dir/environment.txt" || exit 1
	echo "CHARACTERIZATION (no acceptance verdict): $evidence_dir"
	exit 0
fi
if [[ $remote_status -ne 0 || $(< "$evidence_dir/result") != PASS ]]; then
	echo "FAIL: real-kernel scenario failed; evidence: $evidence_dir" >&2
	if [[ $remote_status -ne 0 ]]; then exit "$remote_status"; else exit 1; fi
fi
grep -q '^cleanup=PASS$' "$evidence_dir/environment.txt" \
	|| { echo "FAIL: remote cleanup was not verified; evidence: $evidence_dir" >&2; exit 1; }

echo "PASS: real-kernel $scenario"
echo "Evidence: $evidence_dir"
