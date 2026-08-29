#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

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

printf 'PASS: real-kernel packaged-service host contract\n'
