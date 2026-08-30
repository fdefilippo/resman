#!/usr/bin/env bash
# The sourced remote-runner functions consume test globals dynamically.
# shellcheck disable=SC2034
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/resman-remote-control-test.XXXXXX")
first_bundle=$test_root/resman-final-rfirst
second_bundle=$test_root/resman-final-rsecond
lock_file=$test_root/runner.lock
state_file=$test_root/runner.state
cleanup_marker=$test_root/cleanup.marker

cleanup_test() {
	local status=$?
	trap - EXIT
	set +e
	if [[ -r $state_file ]]; then
		"$first_bundle/control.sh" stop rfirst >/dev/null 2>&1 || true
	fi
	rm -rf -- "$test_root"
	exit "$status"
}
trap cleanup_test EXIT

mkdir -p "$first_bundle"
install -m 0755 "$script_dir/remote-control.sh" "$first_bundle/control.sh"
cat >"$first_bundle/run.sh" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
marker=${RESMAN_REMOTE_CONTROL_TEST_MARKER:?}
finish() {
	local status=$?
	trap - EXIT INT TERM HUP
	printf 'cleanup\n' >>"$marker"
	exit "$status"
}
trap finish EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
printf 'ready\n' >>"$marker"
while :; do sleep 1; done
EOF
chmod 0755 "$first_bundle/run.sh"

export RESMAN_REAL_KERNEL_LOCK_FILE=$lock_file
export RESMAN_REAL_KERNEL_STATE_FILE=$state_file
export RESMAN_REAL_KERNEL_STOP_TIMEOUT=5
export RESMAN_REAL_KERNEL_BUNDLE_PARENT=$test_root
export RESMAN_REMOTE_CONTROL_TEST_MARKER=$cleanup_marker

"$first_bundle/control.sh" start rfirst service-start-stop revision >"$test_root/first.log" 2>&1 &
first_control_pid=$!
for _ in $(seq 1 50); do
	[[ -r $state_file && -s $cleanup_marker ]] && break
	sleep 0.1
done
[[ -r $state_file ]] || { echo "first run did not publish ownership state" >&2; exit 1; }
IFS=$'\t' read -r active_run wrapper_pid child_pgid <"$state_file"
[[ $active_run == rfirst && $wrapper_pid =~ ^[0-9]+$ && $child_pgid =~ ^[1-9][0-9]*$ ]] \
	|| { echo "invalid active state: $(< "$state_file")" >&2; exit 1; }

mkdir -p "$second_bundle"
install -m 0755 "$script_dir/remote-control.sh" "$second_bundle/control.sh"
install -m 0755 "$first_bundle/run.sh" "$second_bundle/run.sh"

set +e
"$second_bundle/control.sh" start rsecond service-start-stop revision \
	>"$test_root/second.log" 2>&1
second_status=$?
set -e
[[ $second_status -eq 77 ]] \
	|| { echo "overlapping run exited $second_status, want BLOCKED/77" >&2; exit 1; }
grep -q '^BLOCKED:' "$test_root/second.log" \
	|| { echo "overlapping run did not report BLOCKED" >&2; exit 1; }

if ! "$first_bundle/control.sh" stop rfirst; then
	cat "$test_root/first.log" >&2
	exit 1
fi
set +e
wait "$first_control_pid"
first_status=$?
set -e
[[ $first_status -eq 143 ]] \
	|| { echo "stopped run exited $first_status, want 143" >&2; exit 1; }
kill -0 "$child_pgid" 2>/dev/null \
	&& { echo "remote scenario process survived stop" >&2; exit 1; }
[[ ! -e $state_file ]] || { echo "ownership state survived stop" >&2; exit 1; }
[[ $(grep -c '^cleanup$' "$cleanup_marker") -eq 1 ]] \
	|| { echo "remote scenario cleanup did not run exactly once" >&2; exit 1; }

set +e
"$second_bundle/control.sh" start rsecond service-start-stop revision \
	>"$test_root/stale-bundle.log" 2>&1
stale_status=$?
set -e
[[ $stale_status -eq 77 ]] \
	|| { echo "stale overlapping bundle exited $stale_status, want BLOCKED/77" >&2; exit 1; }
grep -q '^BLOCKED: another real-kernel bundle exists:' "$test_root/stale-bundle.log" \
	|| { echo "stale overlapping bundle did not report BLOCKED" >&2; exit 1; }

# Prove that TERM interrupts the local runner while it is waiting for the main
# SSH process, invokes the run-scoped stop command, and only then removes the
# remote bundle. The controller test above proves that stop drains the child and
# runs its cleanup trap.
# shellcheck disable=SC1091
RESMAN_REAL_KERNEL_REMOTE_LIBRARY_ONLY=1 source "$script_dir/remote.sh"
calls_file=$test_root/local-cleanup.calls
start_marker=$test_root/local-start.marker
stop_marker=$test_root/local-stop.marker
ssh() {
	printf '%s\n' "$*" >>"$calls_file"
	case "$*" in
		*"control.sh' start 'rlocal'"*)
			touch "$start_marker"
			while [[ ! -e $stop_marker ]]; do sleep 0.1; done
			return 143
			;;
		*"control.sh' stop 'rlocal'"*) touch "$stop_marker" ;;
	esac
}

(
	remote_host=root@test-host
	remote_root=/tmp/resman-final-rlocal
	run_id=rlocal
	scenario=service-start-stop
	source_revision=revision
	remote_execution_started=1
	remote_cleanup_done=0
	remote_ssh_pid=
	evidence_dir=
	scratch_dir=
	trap cleanup EXIT INT TERM
	run_remote_scenario
) &
local_runner_pid=$!
for _ in $(seq 1 50); do
	[[ -e $start_marker ]] && break
	sleep 0.1
done
[[ -e $start_marker ]] || { echo "local runner did not enter its SSH wait" >&2; exit 1; }
kill -TERM "$local_runner_pid"
set +e
wait "$local_runner_pid"
local_runner_status=$?
set -e
[[ $local_runner_status -eq 143 ]] \
	|| { echo "interrupted local runner exited $local_runner_status, want 143" >&2; exit 1; }
[[ -e $stop_marker ]] || { echo "interrupted local runner did not stop the remote run" >&2; exit 1; }
[[ $(sed -n '2p' "$calls_file") == *"control.sh' stop 'rlocal'"* ]] \
	|| { echo "local cleanup did not stop its remote run after interruption" >&2; exit 1; }
[[ $(sed -n '3p' "$calls_file") == *"rm -rf -- '/tmp/resman-final-rlocal'"* ]] \
	|| { echo "local cleanup did not remove the bundle after stopping" >&2; exit 1; }

echo "PASS: remote scenario ownership and overlap contract"
