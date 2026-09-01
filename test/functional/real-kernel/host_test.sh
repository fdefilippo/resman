#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

bash -n "$script_dir/cpu-points-run.sh"

# shellcheck disable=SC1091
RESMAN_REAL_KERNEL_LIBRARY_ONLY=1 source "$script_dir/service-run.sh"

calls=()
stop_status=0
reported_state=inactive

systemctl() {
	calls+=("$*")
	case "$1" in
		stop) return "$stop_status" ;;
		is-active) printf '%s\n' "$reported_state"; return 0 ;;
		*) return 2 ;;
	esac
}

assert_quiesce_case() {
	local name=$1 state=$2 stop_rc=$3 want_rc=$4
	calls=()
	service_quiesced=0
	reported_state=$state
	stop_status=$stop_rc
	local got_rc=0
	quiesce_installed_service || got_rc=$?
	[[ $got_rc -eq $want_rc ]] \
		|| { printf '%s: exit %d, want %d\n' "$name" "$got_rc" "$want_rc" >&2; exit 1; }
	[[ ${calls[0]:-} == "stop resman" ]] \
		|| { printf '%s: first call was %q\n' "$name" "${calls[0]:-}" >&2; exit 1; }
	if [[ $want_rc -eq 0 ]]; then
		[[ $service_quiesced -eq 1 ]] \
			|| { printf '%s: success did not publish quiescence\n' "$name" >&2; exit 1; }
	else
		[[ $service_quiesced -eq 0 ]] \
			|| { printf '%s: failure published false quiescence\n' "$name" >&2; exit 1; }
	fi
}

assert_quiesce_case "inactive after stop" inactive 0 0
assert_quiesce_case "failed but process-free after stop" failed 0 0
assert_quiesce_case "stop failed" active 1 1
assert_quiesce_case "unit remained active" active 0 1

# cgroup_path_labels is what stands behind the series-lifecycle assertions, so a
# blind extractor would make all three of them pass vacuously. These cases pin
# the two states the scenario asserts (no series, one series) and the regression
# it exists to catch (a stale path surviving alongside the enforcing one).
scrape_fixture=$(mktemp -d)
trap 'rm -rf "$scrape_fixture"' EXIT

assert_path_labels() {
	local name=$1 uid=$2 body=$3 want=$4 got rc=0
	printf '%s' "$body" >"$scrape_fixture/scrape.prom"
	got=$(cgroup_path_labels "$scrape_fixture/scrape.prom" "$uid" | tr '\n' ' ') || rc=$?
	[[ $rc -eq 0 ]] \
		|| { printf '%s: extractor exited %d, want 0\n' "$name" "$rc" >&2; exit 1; }
	[[ ${got% } == "$want" ]] \
		|| { printf '%s: paths %q, want %q\n' "$name" "${got% }" "$want" >&2; exit 1; }
}

idle_body='# HELP resman_control_cycles_total Control cycles
resman_control_cycles_total 12
resman_actively_limited_users_count 0
'
limited_body='resman_cgroup_cpu_period_microseconds{uid="1006",cgroup_path="/sys/fs/cgroup/resman/limited/best_effort/user_1006"} 100000
resman_cgroup_memory_usage_bytes{uid="1006",cgroup_path="/sys/fs/cgroup/resman/limited/best_effort/user_1006"} 4096
'
stale_body='resman_cgroup_cpu_period_microseconds{uid="1006",cgroup_path="/sys/fs/cgroup/resman/limited/best_effort/user_1006"} 100000
resman_cgroup_cpu_period_microseconds{uid="1006",cgroup_path="/sys/fs/cgroup/resman/user_1006"} 100000
'
neighbour_body='resman_cgroup_cpu_period_microseconds{uid="10061",cgroup_path="/sys/fs/cgroup/resman/limited/best_effort/user_10061"} 100000
'

# An idle daemon publishes no cgroup series at all. This must not abort the run:
# the scenario asserts this state twice, before load and after release.
assert_path_labels "idle publishes nothing" 1006 "$idle_body" ""
assert_path_labels "enforced publishes one path" 1006 "$limited_body" \
	"/sys/fs/cgroup/resman/limited/best_effort/user_1006"
assert_path_labels "a stale path is reported alongside the enforcing one" 1006 "$stale_body" \
	"/sys/fs/cgroup/resman/limited/best_effort/user_1006 /sys/fs/cgroup/resman/user_1006"
assert_path_labels "a longer UID is not mistaken for this one" 1006 "$neighbour_body" ""

"$script_dir/remote-control_test.sh"

printf 'PASS: real-kernel packaged-service host contract\n'
