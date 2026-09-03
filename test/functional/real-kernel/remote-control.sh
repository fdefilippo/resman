#!/usr/bin/env bash
set -Eeuo pipefail

action=${1:?action is required}
run_id=${2:?run id is required}
shift 2

case "$run_id" in
	*[!a-z0-9-]*|'') echo "invalid run id: $run_id" >&2; exit 2 ;;
esac

bundle_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
lock_file=${RESMAN_REAL_KERNEL_LOCK_FILE:-/run/lock/resman-real-kernel.lock}
state_file=${RESMAN_REAL_KERNEL_STATE_FILE:-/run/lock/resman-real-kernel.state}
stop_timeout=${RESMAN_REAL_KERNEL_STOP_TIMEOUT:-30}
bundle_parent=${RESMAN_REAL_KERNEL_BUNDLE_PARENT:-/tmp}
controlled_child_pgid=0
lock_blocked_exit=75

case "$stop_timeout" in
	''|*[!0-9]*) echo "invalid stop timeout: $stop_timeout" >&2; exit 2 ;;
esac

read_active_state() {
	active_run_id=
	active_wrapper_pid=
	active_child_pgid=
	[[ -r $state_file ]] || return 1
	IFS=$'\t' read -r active_run_id active_wrapper_pid active_child_pgid <"$state_file" || return 1
	[[ -n $active_run_id && $active_wrapper_pid =~ ^[0-9]+$ && $active_child_pgid =~ ^[0-9]+$ ]]
}

lock_is_free() {
	(
		exec 8>"$lock_file"
		flock -n 8
	)
}

publish_active_state() {
	local child_pgid=$1 temporary
	temporary=$(mktemp "${state_file}.XXXXXX")
	chmod 0600 "$temporary"
	printf '%s\t%d\t%d\n' "$run_id" "$$" "$child_pgid" >"$temporary"
	mv -f -- "$temporary" "$state_file"
}

remove_owned_state() {
	if read_active_state \
		&& [[ $active_run_id == "$run_id" && $active_wrapper_pid -eq $$ ]]; then
		rm -f -- "$state_file"
	fi
}

record_preflight_blocked() {
	local scenario=$1 source_revision=$2 message=$3
	local evidence_dir=$bundle_dir/evidence
	mkdir -p "$evidence_dir"
	chmod 0700 "$evidence_dir"
	printf 'BLOCKED\n' >"$evidence_dir/result"
	{
		printf 'scenario=%s\n' "$scenario"
		printf 'run_id=%s\n' "$run_id"
		printf 'source_revision=%s\n' "$source_revision"
		printf 'result=BLOCKED\n'
		printf 'detail=%s\n' "$message"
	} >"$evidence_dir/environment.txt"
}

terminate_child_group() {
	local child_pgid=$1 deadline
	(( child_pgid > 0 )) || return 0
	if ! kill -0 -- "-$child_pgid" 2>/dev/null; then
		return 0
	fi
	kill -TERM -- "-$child_pgid" 2>/dev/null || true
	deadline=$((SECONDS + stop_timeout))
	while (( SECONDS < deadline )); do
		kill -0 -- "-$child_pgid" 2>/dev/null || return 0
		sleep 1
	done
	return 1
}

finish_control() {
	local status=$?
	trap - EXIT HUP INT TERM
	set +e
	if (( controlled_child_pgid > 0 )); then
		terminate_child_group "$controlled_child_pgid" || status=1
	fi
	remove_owned_state
	exit "$status"
}

start_scenario() {
	local scenario=${1:?scenario is required}
	local source_revision=${2:?source revision is required}
	local runner=$bundle_dir/run.sh
	local child_status=1

	if ! command -v flock >/dev/null 2>&1; then
		record_preflight_blocked "$scenario" "$source_revision" \
			"flock is required for real-kernel scenario exclusion"
		echo "BLOCKED: flock is required for real-kernel scenario exclusion" >&2
		return 77
	fi
	if ! command -v setsid >/dev/null 2>&1; then
		record_preflight_blocked "$scenario" "$source_revision" \
			"setsid is required for remote scenario ownership"
		echo "BLOCKED: setsid is required for remote scenario ownership" >&2
		return 77
	fi
	[[ -x $runner ]] || { echo "remote scenario runner is missing: $runner" >&2; return 1; }

	umask 077
	exec 9>"$lock_file"
	if ! flock -n 9; then
		if read_active_state; then
			echo "BLOCKED: real-kernel run $active_run_id is already active" >&2
		else
			echo "BLOCKED: another real-kernel run holds the host lock" >&2
		fi
		return "$lock_blocked_exit"
	fi
	for candidate in "$bundle_parent"/resman-final-r*; do
		[[ -e $candidate || -L $candidate ]] || continue
		[[ $candidate == "$bundle_dir" ]] && continue
		echo "BLOCKED: another real-kernel bundle exists: $(basename "$candidate")" >&2
		return "$lock_blocked_exit"
	done

	trap finish_control EXIT
	trap 'exit 129' HUP
	trap 'exit 130' INT
	trap 'exit 143' TERM

	publish_active_state 0
	setsid "$runner" "$scenario" "$run_id" "$source_revision" &
	controlled_child_pgid=$!
	publish_active_state "$controlled_child_pgid"
	set +e
	wait "$controlled_child_pgid"
	child_status=$?
	set -e
	controlled_child_pgid=0
	return "$child_status"
}

stop_scenario() {
	local deadline
	command -v flock >/dev/null 2>&1 || return 1
	if ! read_active_state; then
		if lock_is_free; then
			return 0
		fi
		echo "active real-kernel run has no readable ownership state" >&2
		return 1
	fi
	if [[ $active_run_id != "$run_id" ]]; then
		return 0
	fi

	if (( active_child_pgid > 0 )); then
		kill -TERM -- "-$active_child_pgid" 2>/dev/null || true
	else
		kill -TERM "$active_wrapper_pid" 2>/dev/null || true
	fi
	deadline=$((SECONDS + stop_timeout))
	while (( SECONDS < deadline )); do
		if ! read_active_state || [[ $active_run_id != "$run_id" ]]; then
			return 0
		fi
		sleep 1
	done
	echo "real-kernel run $run_id did not stop within ${stop_timeout}s" >&2
	return 1
}

case "$action" in
	start) start_scenario "$@" ;;
	stop) stop_scenario ;;
	*) echo "invalid remote control action: $action" >&2; exit 2 ;;
esac
