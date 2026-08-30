#!/usr/bin/env bash
# Remote runner for the packaged-service scenario family.
#
# Unlike run.sh, which builds and launches a binary from the source revision,
# this runner exercises the installed package: the shipped unit, the packaged
# paths and the systemd lifecycle. Evidence therefore records artifact=
# installed-package so a PASS cannot be mistaken for source-revision coverage.
set -Eeuo pipefail

# Non-interactive SSH sessions do not inherit the administrative PATH.
PATH=/usr/sbin:/sbin:/usr/bin:/bin
export PATH

service_quiesced=0

# quiesce_installed_service prevents a scenario from editing restart-required
# configuration while the instance whose state it saved is still running.
quiesce_installed_service() {
	local state
	systemctl stop resman || return 1
	state=$(systemctl is-active resman 2>&1 || true)
	case "$state" in
		inactive|failed)
			service_quiesced=1
			return 0
			;;
		*)
			return 1
			;;
	esac
}

# cgroup_path_labels prints the cgroup_path label of every resman_cgroup_* sample
# published for one UID. The three cgroup gauges are the series that must follow
# a user across placement changes instead of accumulating one entry per path.
cgroup_path_labels() {
	local scrape=$1 uid=$2
	# awk rather than a grep pipeline: publishing no cgroup series is a state
	# this scenario asserts twice, and under pipefail an empty grep would abort
	# the run before the assertion that expects it.
	awk -v uid="uid=\"$uid\"" '
		/^resman_cgroup_(cpu_quota_microseconds|cpu_period_microseconds|memory_usage_bytes)\{/ \
			&& index($0, uid) \
			&& match($0, /cgroup_path="[^"]*"/) {
			print substr($0, RSTART + 13, RLENGTH - 14)
		}
	' "$scrape" | sort -u
}

if [[ ${RESMAN_REAL_KERNEL_LIBRARY_ONLY:-0} == 1 ]]; then
	if [[ ${BASH_SOURCE[0]} != "$0" ]]; then
		return 0
	fi
	exit 0
fi

scenario=${1:?scenario is required}
run_id=${2:?run id is required}
source_revision=${3:?source revision is required}

bundle_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$bundle_dir/evidence
config_path=/etc/resman/resman.conf
config_backup=$bundle_dir/resman.conf.original
config_saved=0
tls_generated=0
hook_installed=0
load_pids=()
container_names=()
tls_snapshot=
managed_cgroup_root=/sys/fs/cgroup/resman
initial_active=
initial_enabled=
result=FAIL
detail=
cleanup_status=PASS

# Accounts that no scenario may ever move into a ResMan cgroup.
protected_users=(root dbacro1 dbacro2 crm francesco)
test_users=(resman-t1 resman-t2 resman-t3 resman-t4 pippo pluto)

mkdir -p "$evidence_dir"
chmod 0700 "$evidence_dir"
exec > >(tee -a "$evidence_dir/runner.log") 2>&1

# assert_no_protected_process proves the safety contract of this suite: an
# excluded identity must never appear inside a managed cgroup, whatever the
# scenario did.
assert_no_protected_process() {
	local pids pid uid owner root
	root=$managed_cgroup_root
	[[ -d $root ]] || return 0
	while IFS= read -r pids; do
		# cgroupfs reports zero size even for populated files, so -s would make
		# this safety check inert. Read the contents instead.
		[[ -r $pids ]] || continue
		while IFS= read -r pid; do
			[[ -n $pid ]] || continue
			uid=$(awk '/^Uid:/ {print $2; exit}' "/proc/$pid/status" 2>/dev/null || true)
			[[ -n $uid ]] || continue
			for owner in "${protected_users[@]}"; do
				if [[ $uid == "$(id -u "$owner" 2>/dev/null || echo -1)" ]]; then
					printf 'protected uid %s (%s) found in %s\n' "$uid" "$owner" "$pids" \
						>>"$evidence_dir/protected-user-violations.txt"
					return 1
				fi
			done
		done <"$pids"
	done < <(find "$root" -name cgroup.procs 2>/dev/null)
	return 0
}

# finish is invoked by the EXIT trap registered below; ShellCheck does not
# resolve that indirection here.
# shellcheck disable=SC2329
finish() {
	local status=$?
	local final_active final_enabled mode owner
	trap - EXIT INT TERM
	set +e

	# An early preflight exit can run this trap before the workload helpers have
	# been defined. In that case no workload could have been started.
	if declare -F stop_user_load >/dev/null; then
		stop_user_load
	fi
	local container
	for container in "${container_names[@]:-}"; do
		[[ -n $container ]] || continue
		podman rm -f "$container" >>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL-container-cleanup
	done
	if [[ $hook_installed -eq 1 ]]; then
		rm -f /usr/local/bin/resman-functional-hook /var/lib/resman/functional-hook.env \
			>>"$evidence_dir/cleanup.log" 2>&1
	fi

	# Restore the unit to the state observed before the scenario ran.
	systemctl stop resman >>"$evidence_dir/cleanup.log" 2>&1
	if [[ $initial_enabled == enabled ]]; then
		systemctl enable resman >>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL-unit-enable
	else
		systemctl disable resman >>"$evidence_dir/cleanup.log" 2>&1
	fi

# Restore the packaged configuration and prove its permissions came back.
	if [[ $config_saved -eq 1 ]]; then
		install -m 0600 -o root -g root "$config_backup" "$config_path" \
			>>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL-config-restore
		mode=$(stat -c '%a' "$config_path" 2>/dev/null)
		owner=$(stat -c '%U:%G' "$config_path" 2>/dev/null)
		[[ $mode == 600 && $owner == root:root ]] || cleanup_status=FAIL-config-mode
	fi

if [[ $initial_active == active ]]; then
		systemctl start resman >>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL-unit-start
	fi

	# systemctl can return while the unit is still deactivating, so settle first.
	local settle=$((SECONDS + 60))
	while (( SECONDS < settle )); do
		final_active=$(systemctl is-active resman 2>&1 || true)
		[[ $final_active == activating || $final_active == deactivating ]] || break
		sleep 2
	done
	final_enabled=$(systemctl is-enabled resman 2>&1 || true)
	[[ $final_active == "$initial_active" && $final_enabled == "$initial_enabled" ]] \
		|| cleanup_status=FAIL-unit-state

	if [[ -d $managed_cgroup_root ]]; then
		find "$managed_cgroup_root" -mindepth 1 -maxdepth 2 -type d \
			>"$evidence_dir/residual-cgroups.txt" 2>&1
		[[ -s $evidence_dir/residual-cgroups.txt ]] && cleanup_status=FAIL-residual-cgroups
	fi

	# Generated TLS material is a scenario artifact. Remove whatever appeared
	# after the pre-generation snapshot rather than a fixed list of names, so a
	# change in what the generator emits cannot leave residue behind.
	if [[ $tls_generated -eq 1 && -f $tls_snapshot ]]; then
		find /etc/resman/tls -mindepth 1 | sort >"$evidence_dir/tls-after.txt" 2>&1
		comm -13 "$tls_snapshot" "$evidence_dir/tls-after.txt" \
			| while IFS= read -r artifact; do
				[[ -n $artifact ]] && rm -rf -- "$artifact"
			done
		find /etc/resman/tls -mindepth 1 | sort >"$evidence_dir/tls-final.txt" 2>&1
		comm -13 "$tls_snapshot" "$evidence_dir/tls-final.txt" \
			>"$evidence_dir/residual-tls.txt" 2>&1
		[[ -s $evidence_dir/residual-tls.txt ]] && cleanup_status=FAIL-residual-tls
	fi

	assert_no_protected_process || cleanup_status=FAIL-protected-user

	if [[ $cleanup_status != PASS ]]; then
		status=1
		[[ $result == PASS ]] && result=FAIL
	fi
	if [[ $result == FAIL && $status -eq 0 ]]; then
		status=1
	fi

	printf '%s\n' "$result" >"$evidence_dir/result"
	{
		printf 'cleanup=%s\n' "$cleanup_status"
		printf 'result=%s\n' "$result"
		printf 'detail=%s\n' "$detail"
		printf 'final_active=%s\n' "$final_active"
		printf 'final_enabled=%s\n' "$final_enabled"
		printf 'exit_code=%d\n' "$status"
	} >>"$evidence_dir/environment.txt"
	exit "$status"
}
trap finish EXIT
# A dying control session delivers SIGHUP; without trapping it the exit trap
# never runs and the host keeps the scenario's modifications.
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

blocked() {
	result=BLOCKED
	detail=$1
	exit 77
}

fail() {
	result=FAIL
	detail=$1
	exit 1
}

[[ $EUID -eq 0 ]] || fail "the packaged-service family requires root on the test host"
command -v systemctl >/dev/null 2>&1 || blocked "systemd is unavailable on this host"
rpm -q resman >/dev/null 2>&1 || blocked "the resman package is not installed on this host"

installed_version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' resman)
# systemctl reports inactive and disabled through a non-zero exit status,
# which set -e would treat as a scenario failure.
initial_active=$(systemctl is-active resman 2>&1 || true)
initial_enabled=$(systemctl is-enabled resman 2>&1 || true)

{
	printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf 'scenario=%s\n' "$scenario"
	printf 'run_id=%s\n' "$run_id"
	printf 'source_revision=%s\n' "$source_revision"
	printf 'artifact=installed-package\n'
	printf 'installed_version=%s\n' "$installed_version"
	printf 'host=%s\n' "$(hostname -f 2>/dev/null || hostname)"
	printf 'os=%s\n' "$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '\"')"
	printf 'kernel=%s\n' "$(uname -r)"
	printf 'initial_active=%s\n' "$initial_active"
	printf 'initial_enabled=%s\n' "$initial_enabled"
	printf 'protected_users=%s\n' "${protected_users[*]}"
	printf 'eligible_users=%s\n' "${test_users[*]}"
} >"$evidence_dir/environment.txt"

# The backup is taken from the live file, so a modification surviving an earlier
# run would be captured and preserved forever. rpm tracks the digest of this
# %config(noreplace) file, so a polluted host is detectable and must block rather
# than silently propagate into every later scenario.
rpm -V resman >"$evidence_dir/package-verification.txt" 2>&1 || true
if grep -qE '^..5.* /etc/resman/resman.conf$' "$evidence_dir/package-verification.txt"; then
	blocked "the packaged configuration differs from the package; restore it before running scenarios"
fi

install -m 0600 "$config_path" "$config_backup"
config_saved=1
quiesce_installed_service \
	|| fail "the installed service did not become quiescent before configuration mutation"
printf 'pre_scenario_service_quiesced=true\n' >>"$evidence_dir/environment.txt"

# write_scenario_configuration installs a configuration whose eligibility is
# restricted to the dedicated accounts. Excluded identities are named in both
# lists so a regression in either direction is visible.
write_scenario_configuration() {
	local include_list exclude_list
	[[ $service_quiesced -eq 1 ]] \
		|| fail "scenario configuration mutation attempted while the installed service was not quiescent"
	include_list=$(printf '^%s$,' "${test_users[@]}")
	include_list=${include_list%,}
	exclude_list=$(printf '^%s$,' "${protected_users[@]}")
	exclude_list=${exclude_list%,}

	sed \
		-e "s|^USER_INCLUDE_LIST=.*|USER_INCLUDE_LIST=$include_list|" \
		-e "s|^USER_EXCLUDE_LIST=.*|USER_EXCLUDE_LIST=$exclude_list|" \
		-e 's|^LOG_LEVEL=.*|LOG_LEVEL=INFO|' \
		-e 's|^METRICS_DB_ENABLED=.*|METRICS_DB_ENABLED=false|' \
		-e 's|^MCP_ENABLED=.*|MCP_ENABLED=false|' \
		"$config_backup" >"$bundle_dir/scenario.conf"
	install -m 0600 -o root -g root "$bundle_dir/scenario.conf" "$config_path"
}

# daemon_log_path resolves the sink resman actually writes to. The packaged unit
# sets StandardOutput=journal, but the daemon logs to its configured file, so the
# journal carries only systemd's own lifecycle messages. Asserting on the journal
# alone would therefore be vacuous.
daemon_log_path() {
	sed -n 's/^LOG_FILE=//p' "$config_path" | tail -1
}

daemon_log_since() {
	local marker=$1 log
	log=$(daemon_log_path)
	[[ -n $log && -f $log ]] || return 0
	awk -v start="$marker" 'NR>start' "$log"
}

daemon_log_lines() {
	local log
	log=$(daemon_log_path)
	[[ -n $log && -f $log ]] || { printf '0'; return 0; }
	wc -l <"$log" | tr -d ' '
}

# wait_for_daemon_log blocks until the daemon sink contains at least count
# occurrences of a pattern after marker, or fails when the deadline passes.
# Scenarios therefore observe real control-cycle progress rather than guessing a
# duration, and they run for as long as the configured cadence actually needs.
wait_for_daemon_log() {
	local pattern=$1 count=$2 timeout=$3 marker=$4
	local deadline=$((SECONDS + timeout)) seen=0
	while (( SECONDS < deadline )); do
		seen=$(daemon_log_since "$marker" | grep -c "$pattern" || true)
		if (( seen >= count )); then
			printf '%s\n' "$seen"
			return 0
		fi
		sleep 5
	done
	printf '%s\n' "$seen"
	return 1
}

# wait_for_unit_active tolerates systemd's transient reloading state after
# ExecReload while still failing promptly if the daemon actually stops.
wait_for_unit_active() {
	local timeout=$1 state
	local deadline=$((SECONDS + timeout))
	while (( SECONDS < deadline )); do
		state=$(systemctl is-active resman 2>&1 || true)
		case "$state" in
			active) return 0 ;;
			activating|reloading) sleep 1 ;;
			*) return 1 ;;
		esac
	done
	return 1
}

# observed_polling_interval reports the cadence the scenario configuration asks
# for, so waits scale with the contract instead of a hard-coded constant.
observed_polling_interval() {
	local value
	value=$(sed -n 's/^POLLING_INTERVAL=//p' "$config_path" | tail -1)
	[[ $value =~ ^[0-9]+$ && $value -gt 0 ]] || value=30
	printf '%s\n' "$value"
}

# mcp_call issues one stateless MCP request. resman serves no initialize
# handshake (resman-4pw.18), so every request must carry the protocol revision in
# the SDK _meta envelope as well as the header.
mcp_call() {
	local port=$1 token=$2 revision=$3 method=$4 extra=$5 outfile=$6 target=${7:-}
	local body name_header=()
	[[ -n $target ]] && name_header=(-H "Mcp-Name: $target")
	body=$(printf '{"jsonrpc":"2.0","id":1,"method":"%s","params":{%s"_meta":{"io.modelcontextprotocol/protocolVersion":"%s","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"resman-functional-suite","version":"1"}}}}' \
		"$method" "$extra" "$revision")
	curl -sk -o "$outfile" -w '%{http_code}' --max-time 25 \
		-X POST "https://127.0.0.1:$port/mcp" \
		-H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' \
		-H "Mcp-Protocol-Version: $revision" \
		-H "Mcp-Method: $method" \
		"${name_header[@]}" \
		-H "Authorization: Bearer $token" \
		--data-binary "$body" || true
}

# start_user_load runs one CPU burner as an eligible test account. Scenarios keep
# every timing interval at its shipped default and lower only the threshold, so
# the activation and release semantics stay intact while a four-core host is not
# saturated at the expense of the services that legitimately run on it.
start_user_load() {
	local user=$1
	runuser -u "$user" -- sh -c 'while :; do :; done' >/dev/null 2>&1 &
	load_pids+=($!)
}

stop_user_load() {
	local pid
	for pid in "${load_pids[@]:-}"; do
		[[ -n $pid ]] || continue
		kill -TERM "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	load_pids=()
}

# limited_cgroup_for reports the managed leaf of one user, if enforcement created it.
limited_cgroup_for() {
	local uid=$1
	printf '%s/limited/user_%s' "$managed_cgroup_root" "$uid"
}

# Enforcement means the managed cgroup holds the user's processes, not merely
# that the directory exists: the leaf is created a moment before the processes
# are moved into it, and reading cgroup.procs in that window returns nothing.
wait_for_limited_cgroup() {
	local uid=$1 timeout=$2
	local deadline=$((SECONDS + timeout)) path
	path=$(limited_cgroup_for "$uid")
	while (( SECONDS < deadline )); do
		if [[ -r $path/cgroup.procs && -n $(cat "$path/cgroup.procs" 2>/dev/null) ]]; then
			return 0
		fi
		sleep 5
	done
	return 1
}

wait_for_released_cgroup() {
	local uid=$1 timeout=$2
	local deadline=$((SECONDS + timeout)) path
	path=$(limited_cgroup_for "$uid")
	while (( SECONDS < deadline )); do
		[[ -d $path ]] || return 0
		sleep 5
	done
	return 1
}

# configure_enforcement keeps every shipped default. terra is a laboratory host,
# so scenarios saturate it for real rather than relaxing thresholds.
configure_enforcement() {
	sed -i \
		-e 's|^LOG_LEVEL=.*|LOG_LEVEL=DEBUG|' \
		-e 's|^IGNORE_SYSTEM_LOAD=.*|IGNORE_SYSTEM_LOAD=false|' \
		"$config_path"
	grep -q '^IGNORE_SYSTEM_LOAD=false$' "$config_path" \
		|| printf 'IGNORE_SYSTEM_LOAD=false\n' >>"$config_path"
}

# saturate_host starts enough burners per account to drive the host past the
# shipped CPU threshold and the system-load heuristic.
saturate_host() {
	local user=$1 count=$2 i
	for (( i = 0; i < count; i++ )); do
		start_user_load "$user"
	done
}

journal_cursor() {
	journalctl -u resman -n 1 --show-cursor -o cat 2>/dev/null \
		| sed -n 's/^-- cursor: //p' | tail -1
}

journal_errors_since() {
	local cursor=$1
	if [[ -n $cursor ]]; then
		journalctl -u resman --after-cursor="$cursor" -p err --no-pager -o cat 2>/dev/null
	else
		journalctl -u resman -b -p err --no-pager -o cat 2>/dev/null
	fi
}

scenario_service_start_stop() {
	write_scenario_configuration
	local cursor log_marker
	cursor=$(journal_cursor)
	log_marker=$(daemon_log_lines)

	local interval cycles
	interval=$(observed_polling_interval)
	systemctl start resman || fail "systemctl start failed for the packaged unit"
	[[ $(systemctl is-active resman 2>&1 || true) == active ]] \
		|| fail "the packaged unit did not stay active after start"

	# Three control cycles at the configured cadence prove the daemon is running
	# its loop, not merely that the process survived a few seconds.
	cycles=$(wait_for_daemon_log 'Control cycle completed' 3 $(( interval * 5 + 60 )) "$log_marker") \
		|| fail "the daemon completed only $cycles control cycles within the configured cadence"
	printf 'observed_control_cycles=%s\n' "$cycles" >>"$evidence_dir/environment.txt"

	systemctl show resman -p MainPID -p ExecMainStatus --no-pager \
		>"$evidence_dir/unit-runtime.txt"

	journal_errors_since "$cursor" >"$evidence_dir/journal-errors.txt"
	[[ -s $evidence_dir/journal-errors.txt ]] \
		&& fail "the packaged unit logged priority-error journal entries during startup"

	daemon_log_since "$log_marker" >"$evidence_dir/daemon-log.txt"
	grep ' \[ERROR\] ' "$evidence_dir/daemon-log.txt" >"$evidence_dir/daemon-errors.txt" || true
	[[ -s $evidence_dir/daemon-errors.txt ]] \
		&& fail "the daemon logged error-level diagnostics during a clean start and stop"
	[[ -s $evidence_dir/daemon-log.txt ]] \
		|| fail "the daemon produced no log output, so the error assertion would be vacuous"

	systemctl stop resman || fail "systemctl stop failed for the packaged unit"
	sleep 2
	[[ $(systemctl is-active resman 2>&1 || true) != active ]] \
		|| fail "the packaged unit remained active after stop"

	local exec_status
	exec_status=$(systemctl show resman -p ExecMainStatus --value --no-pager)
	[[ $exec_status == 0 ]] \
		|| fail "the packaged unit exited with status $exec_status instead of a clean shutdown"

	assert_no_protected_process \
		|| fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="the packaged unit started, logged no error, stopped cleanly and left no protected identity managed"
}


# scenario_service_reload_lifecycle proves the reload half of the configuration
# contract on a live unit: a dynamic key is applied without restart, while a
# restart-required key is rejected by name and the active value survives.
scenario_service_reload_lifecycle() {
	write_scenario_configuration
	local cursor log_marker
	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed for the packaged unit"
	local interval
	interval=$(observed_polling_interval)
	wait_for_daemon_log 'Control cycle completed' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon did not reach steady state before the reload probe"
	[[ $(systemctl is-active resman 2>&1 || true) == active ]] \
		|| fail "the packaged unit did not stay active after start"

	# A dynamic key must take effect without a restart.
	log_marker=$(daemon_log_lines)
	sed -i 's|^LOG_LEVEL=.*|LOG_LEVEL=DEBUG|' "$config_path"
	systemctl reload resman || fail "systemctl reload failed for a dynamic change"
	wait_for_daemon_log 'Log level updated' 1 90 "$log_marker" >/dev/null \
		|| fail "the dynamic LOG_LEVEL change was not applied within the reload deadline"
	# A logged intent is not an effect: DEBUG output must actually start appearing
	# on subsequent cycles of the already-running daemon.
	wait_for_daemon_log '\[DEBUG\]' 1 $(( interval * 3 + 60 )) "$log_marker" >/dev/null \
		|| fail "the running daemon never emitted DEBUG output after the level change"
	daemon_log_since "$log_marker" >"$evidence_dir/journal-dynamic-reload.txt"
	grep -q 'Log level updated' "$evidence_dir/journal-dynamic-reload.txt" \
		|| fail "the dynamic LOG_LEVEL change was not applied by reload"
	grep -q 'new_level=DEBUG' "$evidence_dir/journal-dynamic-reload.txt" \
		|| fail "the applied log level did not report the requested value"

	# A restart-required key must be rejected by name, leaving the unit running.
	log_marker=$(daemon_log_lines)
	sed -i 's|^SERVER_ROLE=.*|SERVER_ROLE=resman-functional-probe|' "$config_path"
	systemctl reload resman || true
	wait_for_daemon_log 'Configuration change rejected until restart' 1 90 "$log_marker" >/dev/null \
		|| fail "the restart-required change was not rejected within the reload deadline"
	daemon_log_since "$log_marker" >"$evidence_dir/journal-restart-required.txt"
	local restart_warning_count
	restart_warning_count=$(grep -c ' \[WARN\] Configuration change rejected until restart ' \
		"$evidence_dir/journal-restart-required.txt" || true)
	[[ $restart_warning_count == 1 ]] \
		|| fail "the restart-required change produced $restart_warning_count terminal warnings instead of one"
	grep -q 'rejected_fields=SERVER_ROLE' "$evidence_dir/journal-restart-required.txt" \
		|| fail "the reload rejection did not name the restart-required field"
	grep -q 'source=forced' "$evidence_dir/journal-restart-required.txt" \
		|| fail "the reload rejection did not identify the forced reload source"
	grep -q 'processed=true' "$evidence_dir/journal-restart-required.txt" \
		|| fail "the reload rejection did not confirm that the digest was recorded"
	! grep -q 'Configuration file marked as processed after partial apply' \
		"$evidence_dir/journal-restart-required.txt" \
		|| fail "the reload emitted the obsolete second partial-apply warning"
	wait_for_unit_active 30 \
		|| fail "the unit did not return to active after rejecting a restart-required change"
	# The rejection must leave a working daemon, not a wedged one: it has to keep
	# completing control cycles afterwards.
	wait_for_daemon_log 'Control cycle completed' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon stopped completing control cycles after a rejected reload"
	daemon_log_since "$log_marker" | grep ' \[ERROR\] ' >"$evidence_dir/reload-errors.txt" || true
	[[ -s $evidence_dir/reload-errors.txt ]] \
		&& fail "the daemon logged an error for the expected restart-required rejection"

	systemctl stop resman || fail "systemctl stop failed after the reload scenario"
	assert_no_protected_process \
		|| fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="reload applied a dynamic key and rejected a restart-required key by name while staying active"
}

# scenario_service_fatal_config proves that a permanently invalid configuration
# reaches the failed state once, with the configuration exit status, instead of
# retrying forever under Restart=always.
scenario_service_fatal_config() {
	write_scenario_configuration
	printf '\nRESMAN_FUNCTIONAL_UNKNOWN_KEY=1\n' >>"$config_path"

	systemctl reset-failed resman >/dev/null 2>&1 || true
	if systemctl start resman >"$evidence_dir/fatal-start.txt" 2>&1; then
		fail "systemctl start reported success for a permanently invalid configuration"
	fi
	# Outlast StartLimitIntervalSec so a restart loop would have shown itself.
	sleep 75

	local exec_status active_state restarts
	exec_status=$(systemctl show resman -p ExecMainStatus --value --no-pager)
	active_state=$(systemctl is-active resman 2>&1 || true)
	restarts=$(systemctl show resman -p NRestarts --value --no-pager)
	{
		printf 'exec_main_status=%s\n' "$exec_status"
		printf 'active_state=%s\n' "$active_state"
		printf 'n_restarts=%s\n' "$restarts"
	} >"$evidence_dir/fatal-config-state.txt"

	[[ $exec_status == 78 ]] \
		|| fail "invalid configuration exited with status $exec_status instead of 78"
	[[ $active_state == failed ]] \
		|| fail "the unit settled in state $active_state instead of failed"
	[[ $restarts == 0 ]] \
		|| fail "the unit restarted $restarts times despite a permanent configuration rejection"

	systemctl reset-failed resman >/dev/null 2>&1 || true
	result=PASS
	detail="an invalid configuration exited 78 once and the unit stayed failed without restarting"
}


# scenario_prometheus_scrape proves the exporter answers a real scrape over the
# network with the series the shipped dashboards and alerts consume, after the
# decision cycle has actually populated them.

# scrape_metrics stores one scrape and fails the scenario if the endpoint does
# not answer, so a later assertion can never pass against a stale or empty file.
scrape_metrics() {
	local port=$1 outfile=$2 status
	rm -f "$outfile"
	status=$(curl -s -o "$outfile" -w '%{http_code}' --max-time 20 "http://127.0.0.1:$port/metrics" || true)
	[[ $status == 200 ]] || fail "the metrics endpoint answered HTTP $status instead of 200"
	[[ -s $outfile ]] || fail "the metrics endpoint returned an empty body"
}

# start_nested_namespace_load runs a CPU-burning workload for an eligible host
# UID inside a rootful Podman container. The process is indistinguishable from a
# host workload by UID alone, which is precisely the condition resman-54d
# guards: it is a candidate for the same UID in a foreign PID namespace.
start_nested_namespace_load() {
	local user=$1 name=$2 uid image
	uid=$(id -u "$user")
	image=$(podman images --format '{{.Repository}}:{{.Tag}}' | grep -v '^<none>' | head -1)
	[[ -n $image ]] || return 1
	local workers=${3:-1}
	container_names+=("$name")
	podman run -d --rm --name "$name" --user "$uid" "$image" \
		sh -c "i=0; while [ \$i -lt $workers ]; do (while :; do :; done) & i=\$((i+1)); done; wait" \
		>/dev/null 2>&1 || return 1
}

# nested_load_host_pid reports the host PID of a nested workload, which is what
# ResMan sees through host procfs.
nested_load_host_pid() {
	podman inspect "$1" --format '{{.State.Pid}}' 2>/dev/null
}

# pid_namespace_identity prints the typed device:inode pair ResMan compares.
pid_namespace_identity() {
	stat -L -c '%d:%i' "/proc/$1/ns/pid" 2>/dev/null
}

# cgroup_of_pid prints the unified cgroup path of one PID.
cgroup_of_pid() {
	cut -d: -f3 "/proc/$1/cgroup" 2>/dev/null
}

# ingress_skipped_total reads the bounded skip counter for one closed reason.
ingress_skipped_total() {
	local scrape=$1 reason=$2
	awk -v want="reason=\"$reason\"" '
		/^resman_cgroup_ingress_skipped_total\{/ && index($0, want) { print $NF }
	' "$scrape" | tail -1
}

scenario_prometheus_scrape() {
	local interval log_marker port=1974 status
	write_scenario_configuration
	sed -i \
		-e 's|^ENABLE_PROMETHEUS=.*|ENABLE_PROMETHEUS=true|' \
		-e 's|^PROMETHEUS_METRICS_BIND_HOST=.*|PROMETHEUS_METRICS_BIND_HOST=127.0.0.1|' \
		-e "s|^PROMETHEUS_METRICS_BIND_PORT=.*|PROMETHEUS_METRICS_BIND_PORT=$port|" \
		"$config_path"

	interval=$(observed_polling_interval)
	log_marker=$(daemon_log_lines)
	grep -q '^ENABLE_PROMETHEUS=true' "$config_path" \
		|| fail "the scenario configuration does not enable the exporter"
	systemctl start resman || fail "systemctl start failed with the exporter enabled"
	wait_for_daemon_log 'Control cycle completed' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon did not complete decision cycles before the scrape"
	# A restart-required key edited while a daemon is running is rejected, so the
	# exporter could be inert while the file says otherwise.
	daemon_log_since "$log_marker" | grep -q 'Prometheus exporter disabled by configuration' \
		&& fail "the daemon started with the exporter disabled despite the scenario configuration"

	status=$(curl -s -o "$evidence_dir/metrics.prom" -w '%{http_code}' \
		--max-time 20 "http://127.0.0.1:$port/metrics" || true)
	[[ $status == 200 ]] || fail "the metrics endpoint answered HTTP $status instead of 200"
	[[ -s $evidence_dir/metrics.prom ]] || fail "the metrics endpoint returned an empty body"

	local missing=()
	for series in resman_control_cycles_total resman_cpu_eligible_users_count \
		resman_actively_limited_users_count resman_any_limits_active \
		resman_cpu_limits_active resman_resource_limits_active; do
		grep -q "^${series}[ {]" "$evidence_dir/metrics.prom" || missing+=("$series")
	done
	if (( ${#missing[@]} > 0 )); then
		printf '%s\n' "${missing[@]}" >"$evidence_dir/missing-series.txt"
		fail "the live scrape omitted ${#missing[@]} contract series"
	fi

	# Every exposed series must carry HELP and TYPE, which is what makes the
	# output consumable by promtool and by Prometheus itself.
	grep -c '^# HELP ' "$evidence_dir/metrics.prom" >"$evidence_dir/help-count.txt"
	[[ $(< "$evidence_dir/help-count.txt") -gt 0 ]] \
		|| fail "the scrape exposed no HELP metadata"

	systemctl stop resman || fail "systemctl stop failed after the scrape scenario"
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="a real scrape returned the decision-owned contract series with metadata after live control cycles"
}

# scenario_prometheus_user_series_lifecycle proves the per-user cgroup gauges
# follow one account through a real placement change: absent while idle, carried
# on the enforcing path while limited, and withdrawn from the old path once the
# account is released. resman-ej0.3 was exactly this defect, and until now only
# a unit test with a synthetic path stood behind it.
scenario_prometheus_user_series_lifecycle() {
	local log_marker port=1974 interval user=resman-t1 uid paths
	uid=$(id -u "$user")
	write_scenario_configuration
	configure_enforcement
	sed -i \
		-e 's|^ENABLE_PROMETHEUS=.*|ENABLE_PROMETHEUS=true|' \
		-e 's|^PROMETHEUS_METRICS_BIND_HOST=.*|PROMETHEUS_METRICS_BIND_HOST=127.0.0.1|' \
		-e "s|^PROMETHEUS_METRICS_BIND_PORT=.*|PROMETHEUS_METRICS_BIND_PORT=$port|" \
		"$config_path"
	grep -q '^ENABLE_PROMETHEUS=true' "$config_path" \
		|| fail "the scenario configuration does not enable the exporter"

	interval=$(observed_polling_interval)
	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed for the series lifecycle scenario"
	wait_for_daemon_log 'Control cycle completed' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon did not complete decision cycles before the first scrape"
	daemon_log_since "$log_marker" | grep -q 'Prometheus exporter disabled by configuration' \
		&& fail "the daemon started with the exporter disabled despite the scenario configuration"

	scrape_metrics "$port" "$evidence_dir/metrics-idle.prom"
	cgroup_path_labels "$evidence_dir/metrics-idle.prom" "$uid" >"$evidence_dir/paths-idle.txt"
	if grep -qF "$(limited_cgroup_for "$uid")" "$evidence_dir/paths-idle.txt"; then
		fail "UID $uid already carried enforcing-path series before any load was applied"
	fi

	saturate_host "$user" 2
	wait_for_limited_cgroup "$uid" 360 \
		|| fail "UID $uid was never moved into a managed cgroup under sustained load"
	# The move and the scrape are independent: let the exporter publish the new
	# placement before reading it, or the assertion races the control cycle.
	log_marker=$(daemon_log_lines)
	wait_for_daemon_log 'Control cycle completed' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon published no control cycle while the account was limited"

	scrape_metrics "$port" "$evidence_dir/metrics-limited.prom"
	cgroup_path_labels "$evidence_dir/metrics-limited.prom" "$uid" >"$evidence_dir/paths-limited.txt"
	grep -qF "$(limited_cgroup_for "$uid")" "$evidence_dir/paths-limited.txt" \
		|| fail "UID $uid was enforced but published no cgroup series for its enforcing path"
	paths=$(grep -vF "$(limited_cgroup_for "$uid")" "$evidence_dir/paths-limited.txt" || true)
	if [[ -n $paths ]]; then
		printf '%s\n' "$paths" >"$evidence_dir/stale-paths-limited.txt"
		fail "UID $uid published cgroup series for a path it no longer occupies"
	fi
	# resman-4pw.12 leaves the leaf at cpu.max=max, so the exporter owes a period
	# sample and no finite quota sample for it.
	grep -E "^resman_cgroup_cpu_period_microseconds\{" "$evidence_dir/metrics-limited.prom" \
		| grep -qF "uid=\"$uid\"" \
		|| fail "UID $uid published no CPU period while it was enforced"
	if grep -E "^resman_cgroup_cpu_quota_microseconds\{" "$evidence_dir/metrics-limited.prom" \
		| grep -qF "uid=\"$uid\""; then
		fail "UID $uid published a finite CPU quota for a leaf whose cpu.max is unlimited"
	fi

	stop_user_load
	wait_for_released_cgroup "$uid" 420 \
		|| fail "UID $uid was still enforced long after its load stopped"
	log_marker=$(daemon_log_lines)
	wait_for_daemon_log 'Control cycle completed' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon published no control cycle after the account was released"

	scrape_metrics "$port" "$evidence_dir/metrics-released.prom"
	cgroup_path_labels "$evidence_dir/metrics-released.prom" "$uid" >"$evidence_dir/paths-released.txt"
	if grep -qF "$(limited_cgroup_for "$uid")" "$evidence_dir/paths-released.txt"; then
		fail "UID $uid still published series for the enforcing path it had already left"
	fi

	systemctl stop resman || fail "systemctl stop failed after the series lifecycle scenario"
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="the per-user cgroup gauges followed one account onto its enforcing path and were withdrawn from it on release"
}

# managed_cgroup_holds_pid reports whether one PID sits anywhere inside a
# ResMan-owned cgroup. It reads cgroup.procs rather than testing the file size,
# because cgroupfs reports every control file as empty.
managed_cgroup_holds_pid() {
	local pid=$1 procs
	while IFS= read -r procs; do
		[[ -r $procs ]] || continue
		grep -qx "$pid" "$procs" && return 0
	done < <(find "$managed_cgroup_root" -name cgroup.procs 2>/dev/null)
	return 1
}

# configure_ingress_observability enables the exporter the two PID-namespace
# scenarios read their evidence from.
configure_ingress_observability() {
	local port=$1
	sed -i \
		-e 's|^ENABLE_PROMETHEUS=.*|ENABLE_PROMETHEUS=true|' \
		-e 's|^PROMETHEUS_METRICS_BIND_HOST=.*|PROMETHEUS_METRICS_BIND_HOST=127.0.0.1|' \
		-e "s|^PROMETHEUS_METRICS_BIND_PORT=.*|PROMETHEUS_METRICS_BIND_PORT=$port|" \
		"$config_path"
	grep -q '^ENABLE_PROMETHEUS=true' "$config_path" \
		|| fail "the scenario configuration does not enable the exporter"
}

# scenario_pid_namespace_mixed_ingress proves resman-54d on real hardware: one
# eligible UID owns a host workload and a workload in a container's PID
# namespace, and only the host workload may enter a ResMan cgroup.
scenario_pid_namespace_mixed_ingress() {
	local log_marker interval port=1974 user=resman-t1 uid
	local container=resman-54d-mixed nested_pid host_ns nested_ns nested_cgroup_before nested_cgroup_after
	uid=$(id -u "$user")
	write_scenario_configuration
	configure_enforcement
	configure_ingress_observability "$port"

	interval=$(observed_polling_interval)
	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed for the mixed-ingress scenario"
	wait_for_daemon_log 'Control cycle completed' 1 120 "$log_marker" >/dev/null \
		|| fail "the daemon did not reach steady state before load was applied"

	start_nested_namespace_load "$user" "$container" \
		|| fail "could not start a nested-namespace workload for $user"
	nested_pid=$(nested_load_host_pid "$container")
	[[ -n $nested_pid && $nested_pid != 0 ]] || fail "the nested workload reported no host PID"
	host_ns=$(pid_namespace_identity "$$")
	nested_ns=$(pid_namespace_identity "$nested_pid")
	nested_cgroup_before=$(cgroup_of_pid "$nested_pid")
	printf 'host_ns=%s\nnested_pid=%s\nnested_ns=%s\nnested_cgroup=%s\nnested_host_uid=%s\n' \
		"$host_ns" "$nested_pid" "$nested_ns" "$nested_cgroup_before" \
		"$(stat -c %u "/proc/$nested_pid" 2>/dev/null)" >"$evidence_dir/namespaces.txt"
	[[ -n $nested_ns && $nested_ns != "$host_ns" ]] \
		|| fail "the nested workload shares the ResMan PID namespace, so it cannot exercise the guard"
	[[ $(stat -c %u "/proc/$nested_pid" 2>/dev/null) == "$uid" ]] \
		|| fail "the nested workload does not run as the eligible host UID"

	saturate_host "$user" 2
	wait_for_limited_cgroup "$uid" 360 \
		|| fail "UID $uid was never moved into a managed cgroup under sustained load"

	# The whole point of the guard: same UID, same eligibility, different PID
	# namespace, and only the host processes may cross into ResMan ownership.
	if managed_cgroup_holds_pid "$nested_pid"; then
		find "$managed_cgroup_root" -name cgroup.procs -exec sh -c 'printf "%s: " "$1"; cat "$1"' _ {} \; \
			>"$evidence_dir/managed-membership.txt" 2>&1
		fail "the nested-namespace process entered a ResMan cgroup"
	fi
	nested_cgroup_after=$(cgroup_of_pid "$nested_pid")
	[[ $nested_cgroup_after == "$nested_cgroup_before" ]] \
		|| fail "the nested workload was moved out of its runtime-owned cgroup"
	[[ -n $(cat "$(limited_cgroup_for "$uid")/cgroup.procs" 2>/dev/null) ]] \
		|| fail "no host process entered the managed cgroup, so the scenario proved nothing"

	daemon_log_since "$log_marker" | grep -F 'Skipped process ingress outside the ResMan PID namespace boundary' \
		>"$evidence_dir/ingress-warning.txt" || true
	[[ -s $evidence_dir/ingress-warning.txt ]] \
		|| fail "the daemon logged no aggregated PID-namespace ingress warning"
	grep -q 'pid_namespace_mismatch_count=[1-9]' "$evidence_dir/ingress-warning.txt" \
		|| fail "the ingress warning reported no PID-namespace mismatch"

	scrape_metrics "$port" "$evidence_dir/metrics-mixed.prom"
	local skipped
	skipped=$(ingress_skipped_total "$evidence_dir/metrics-mixed.prom" pid_namespace_mismatch)
	printf 'pid_namespace_mismatch=%s\n' "${skipped:-<assente>}" >"$evidence_dir/ingress-counter.txt"
	[[ -n $skipped ]] || fail "the exporter published no bounded ingress skip counter"
	awk -v v="$skipped" 'BEGIN { exit !(v + 0 > 0) }' \
		|| fail "the ingress skip counter did not record the refused nested candidate"

	# The nested workload also counts toward this UID's measured usage, so the
	# account cannot fall below the threshold while the container still runs.
	# Releasing is a property of the host processes, so remove both loads.
	podman rm -f "$container" >/dev/null 2>&1 || true
	stop_user_load
	if ! wait_for_released_cgroup "$uid" 420; then
		daemon_log_since "$log_marker" | tail -60 >"$evidence_dir/decision-trail.txt"
		fail "UID $uid was still enforced long after both its host and nested loads stopped"
	fi
	systemctl stop resman || fail "systemctl stop failed after the mixed-ingress scenario"
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="one UID owned a host and a nested-namespace workload; only the host processes entered ResMan ownership and the refusal was reported once with a bounded counter"
}

# scenario_pid_namespace_container_only proves the no-op is honest: when every
# candidate for an eligible UID lives in a foreign PID namespace, ResMan must
# not report enforcement it did not perform.
scenario_pid_namespace_container_only() {
	local log_marker interval port=1974 user=resman-t2 uid
	local container=resman-54d-only nested_pid nested_cgroup_before nested_cgroup_after cycles
	uid=$(id -u "$user")
	write_scenario_configuration
	configure_enforcement
	configure_ingress_observability "$port"

	interval=$(observed_polling_interval)
	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed for the container-only scenario"
	wait_for_daemon_log 'Control cycle completed' 1 120 "$log_marker" >/dev/null \
		|| fail "the daemon did not reach steady state before load was applied"

	start_nested_namespace_load "$user" "$container" 3 \
		|| fail "could not start a nested-namespace workload for $user"
	nested_pid=$(nested_load_host_pid "$container")
	[[ -n $nested_pid && $nested_pid != 0 ]] || fail "the nested workload reported no host PID"
	nested_cgroup_before=$(cgroup_of_pid "$nested_pid")

	# The refusal itself is the precondition of this scenario. Waiting for a
	# number of cycles would let it pass while the account never crossed the
	# threshold, in which case "no active enforcement" would be true because
	# nothing was ever attempted. Wait for the refusal, or fail.
	wait_for_daemon_log 'Skipped process ingress outside the ResMan PID namespace boundary' 1 \
		$(( interval * 8 + 240 )) "$log_marker" >/dev/null \
		|| fail "the daemon never attempted and refused ingress, so the no-op proves nothing"

	cycles=$evidence_dir/control-cycles.txt
	daemon_log_since "$log_marker" | grep -F 'Control cycle completed' >"$cycles" || true
	[[ -s $cycles ]] || fail "no control cycle was recorded"
	daemon_log_since "$log_marker" | grep -F 'Skipped process ingress' >"$evidence_dir/ingress-warning.txt" || true
	grep -q 'pid_namespace_mismatch_count=[1-9]' "$evidence_dir/ingress-warning.txt" \
		|| fail "the refusal reported no PID-namespace mismatch"

	if managed_cgroup_holds_pid "$nested_pid"; then
		fail "the only candidate lived in a foreign PID namespace yet entered a ResMan cgroup"
	fi
	nested_cgroup_after=$(cgroup_of_pid "$nested_pid")
	[[ $nested_cgroup_after == "$nested_cgroup_before" ]] \
		|| fail "the nested workload was moved out of its runtime-owned cgroup"
	grep -q 'active_limited_users=0' "$cycles" \
		|| fail "the daemon reported active limited users while every candidate was refused"

	scrape_metrics "$port" "$evidence_dir/metrics-container-only.prom"
	grep -E '^resman_actively_limited_users_count' "$evidence_dir/metrics-container-only.prom" \
		>"$evidence_dir/active-count.txt" || true
	awk '{ if ($NF + 0 != 0) exit 1 }' "$evidence_dir/active-count.txt" \
		|| fail "the exporter reported active enforcement that never happened"
	local skipped
	skipped=$(ingress_skipped_total "$evidence_dir/metrics-container-only.prom" pid_namespace_mismatch)
	printf 'pid_namespace_mismatch=%s\n' "${skipped:-<assente>}" >"$evidence_dir/ingress-counter.txt"
	awk -v v="${skipped:-0}" 'BEGIN { exit !(v + 0 > 0) }' \
		|| fail "the exporter published no refused-ingress count for a refusal the log recorded"

	podman rm -f "$container" >/dev/null 2>&1 || true
	systemctl stop resman || fail "systemctl stop failed after the container-only scenario"
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="every candidate for the UID lived in a foreign PID namespace; nothing entered ResMan ownership and no active enforcement was reported"
}

# scenario_mcp_https_endtoend proves the MCP endpoint serves only over HTTPS with
# a bearer token, and that an unauthenticated request is refused before any
# protocol detail is disclosed.
scenario_mcp_https_endtoend() {
	local interval log_marker port=1969 token status
	local mcp_revision=2026-07-28
	write_scenario_configuration

	tls_snapshot=$bundle_dir/tls-before.txt
	find /etc/resman/tls -mindepth 1 | sort >"$tls_snapshot" 2>/dev/null || true
	tls_generated=1
	bash "$bundle_dir/generate-tls-certs.sh" /etc/resman/tls \
		>"$evidence_dir/tls-generation.log" 2>&1 \
		|| blocked "TLS certificate generation is unavailable on this host"
	[[ -s /etc/resman/tls/server.crt && -s /etc/resman/tls/server.key ]] \
		|| blocked "the generated TLS material is incomplete"

	token=$(openssl rand -hex 24)
	sed -i \
		-e 's|^MCP_ENABLED=.*|MCP_ENABLED=true|' \
		-e 's|^MCP_TRANSPORT=.*|MCP_TRANSPORT=http|' \
		-e 's|^MCP_HTTP_HOST=.*|MCP_HTTP_HOST=127.0.0.1|' \
		-e "s|^MCP_HTTP_PORT=.*|MCP_HTTP_PORT=$port|" \
		-e 's|^MCP_TLS_ENABLED=.*|MCP_TLS_ENABLED=true|' \
		-e "s|^MCP_AUTH_TOKEN=.*|MCP_AUTH_TOKEN=$token|" \
		-e 's|^MCP_ALLOW_WRITE_OPS=.*|MCP_ALLOW_WRITE_OPS=true|' \
		"$config_path"
	grep -q "^MCP_AUTH_TOKEN=$token$" "$config_path" \
		|| printf 'MCP_AUTH_TOKEN=%s\n' "$token" >>"$config_path"

	interval=$(observed_polling_interval)
	log_marker=$(daemon_log_lines)
	grep -q '^MCP_ENABLED=true' "$config_path" \
		|| fail "the scenario configuration does not enable MCP"
	systemctl start resman || fail "systemctl start failed with MCP over HTTPS enabled"
	wait_for_daemon_log 'Control cycle completed' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon did not reach steady state with MCP enabled"
	daemon_log_since "$log_marker" | grep -q 'MCP server disabled by configuration' \
		&& fail "the daemon started with MCP disabled despite the scenario configuration"

	# Plain HTTP must not be served on the MCP listener.
	status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
		"http://127.0.0.1:$port/mcp" || true)
	[[ $status == 200 ]] && fail "the MCP listener answered plain HTTP with 200"

	# An unauthenticated HTTPS request must be refused without protocol detail.
	status=$(curl -sk -o "$evidence_dir/mcp-unauthenticated.json" -w '%{http_code}' \
		--max-time 15 -X POST "https://127.0.0.1:$port/mcp" \
		-H 'Content-Type: application/json' \
		-d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' || true)
	[[ $status == 401 ]] \
		|| fail "an unauthenticated MCP request answered HTTP $status instead of 401"
	grep -qi 'protocol' "$evidence_dir/mcp-unauthenticated.json" \
		&& fail "the unauthenticated refusal disclosed protocol detail"

	# An authenticated request carrying the only supported revision must list the
	# production tool inventory.
	status=$(mcp_call "$port" "$token" "$mcp_revision" tools/list "" \
		"$evidence_dir/mcp-tools-list.json")
	[[ $status == 200 ]] \
		|| fail "an authenticated tools/list answered HTTP $status instead of 200"
	grep -q 'get_system_status' "$evidence_dir/mcp-tools-list.json" \
		|| fail "the authenticated tool inventory did not include get_system_status"

	# A superseded revision must be refused even with a valid token, which is the
	# latest-only contract rather than an authentication outcome.
	status=$(mcp_call "$port" "$token" 2025-03-26 tools/list "" \
		"$evidence_dir/mcp-legacy-revision.json")
	[[ $status == 200 ]] \
		&& fail "a superseded MCP revision was accepted with a valid token"

	# A read tool must answer over the same authenticated transport.
	status=$(mcp_call "$port" "$token" "$mcp_revision" tools/call \
		'"name":"get_system_status","arguments":{},' \
		"$evidence_dir/mcp-get-system-status.json" get_system_status)
	[[ $status == 200 ]] \
		|| fail "an authenticated get_system_status answered HTTP $status instead of 200"
	grep -q '"observed_users_count"' "$evidence_dir/mcp-get-system-status.json" \
		|| fail "get_system_status did not return the observation contract fields"

	systemctl stop resman || fail "systemctl stop failed after the MCP scenario"
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"

	# The generated key must never be readable beyond its owner.
	local key_mode
	key_mode=$(stat -c '%a' /etc/resman/tls/server.key 2>/dev/null)
	[[ $key_mode == 600 || $key_mode == 400 ]] \
		|| fail "the generated TLS private key is mode $key_mode"

	result=PASS
	detail="MCP served only over HTTPS, refused an unauthenticated request with 401 and no protocol detail, and answered an authenticated tools/list"
}


# scenario_blackout_timeframe proves that a configured blackout window suspends
# the control cycle on a live daemon instead of merely being parsed.
scenario_blackout_timeframe() {
	local interval log_marker window
	write_scenario_configuration
	# A window covering the whole current day, so the scenario cannot race the
	# clock while still exercising the real crontab-like parser.
	window='* 00-23'
	sed -i "s|^BLACKOUT=.*|BLACKOUT=$window|" "$config_path"
	grep -q "^BLACKOUT=$window$" "$config_path" \
		|| printf 'BLACKOUT=%s\n' "$window" >>"$config_path"

	interval=$(observed_polling_interval)
	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed with a blackout window configured"

	wait_for_daemon_log 'blackout timeframe active' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon did not suspend its control cycle inside the blackout window"

	# Suspension must not mean enforcement: no managed cgroup may appear.
	find "$managed_cgroup_root" -mindepth 1 -maxdepth 2 -type d \
		>"$evidence_dir/blackout-cgroups.txt" 2>/dev/null || true
	[[ -s $evidence_dir/blackout-cgroups.txt ]] \
		&& fail "managed cgroups were created while the blackout window was active"

	daemon_log_since "$log_marker" | grep ' \[ERROR\] ' >"$evidence_dir/blackout-errors.txt" || true
	[[ -s $evidence_dir/blackout-errors.txt ]] \
		&& fail "the daemon logged errors while suspended by a blackout window"

	systemctl stop resman || fail "systemctl stop failed after the blackout scenario"
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="a configured blackout window suspended the control cycle without creating managed cgroups or errors"
}

# scenario_metrics_database_lifecycle proves the SQLite history store is created
# under the shipped layout with the confidentiality contract of resman-4pw.66 and
# the schema version of resman-4pw.61, on a live daemon.
scenario_metrics_database_lifecycle() {
	local interval log_marker db=/var/lib/resman/metrics.db mode artifact
	write_scenario_configuration
	rm -f "$db" "$db-wal" "$db-shm"
	sed -i \
		-e 's|^METRICS_DB_ENABLED=.*|METRICS_DB_ENABLED=true|' \
		-e "s|^METRICS_DB_PATH=.*|METRICS_DB_PATH=$db|" \
		"$config_path"

	interval=$(observed_polling_interval)
	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed with the metrics database enabled"
	wait_for_daemon_log 'Metrics database initialized' 1 120 "$log_marker" >/dev/null \
		|| fail "the metrics database was not initialized within the startup deadline"
	wait_for_daemon_log 'Control cycle completed' 2 $(( interval * 4 + 60 )) "$log_marker" >/dev/null \
		|| fail "the daemon did not complete decision cycles with persistence enabled"

	[[ -f $db ]] || fail "the metrics database file was not created at the configured path"

	# resman-4pw.66: database and sidecars must not be readable beyond their owner.
	for artifact in "$db" "$db-wal" "$db-shm"; do
		[[ -e $artifact ]] || continue
		mode=$(stat -c '%a' "$artifact")
		[[ $mode == 600 ]] \
			|| fail "$artifact has mode $mode instead of 600"
	done
	mode=$(stat -c '%a' /var/lib/resman)
	[[ $mode == 700 ]] || fail "the state directory has mode $mode instead of 700"

	# resman-4pw.61: the shipped schema is version 3.
	if command -v sqlite3 >/dev/null 2>&1; then
		sqlite3 "$db" 'PRAGMA user_version;' >"$evidence_dir/schema-version.txt" 2>&1
		[[ $(< "$evidence_dir/schema-version.txt") == 3 ]] \
			|| fail "the metrics database reports schema version $(< "$evidence_dir/schema-version.txt") instead of 3"
	else
		printf 'sqlite3 unavailable; schema version not inspected\n' \
			>"$evidence_dir/schema-version.txt"
	fi

	stat -c '%n %a %U:%G' "$db"* >"$evidence_dir/database-modes.txt" 2>&1
	systemctl stop resman || fail "systemctl stop failed after the database scenario"
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="the metrics database was created under the shipped layout with mode 0600 artifacts and the current schema"
}


# scenario_multi_user_enforcement drives three eligible accounts over the CPU
# threshold at the same time and follows a complete activation and release cycle
# at the shipped cadence.
scenario_multi_user_enforcement() {
	local log_marker user uid uids=() quota weight procs
	write_scenario_configuration
	configure_enforcement

	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed for the enforcement scenario"
	wait_for_daemon_log 'Control cycle completed' 1 120 "$log_marker" >/dev/null \
		|| fail "the daemon did not reach steady state before load was applied"

	for user in resman-t1 resman-t2 resman-t3; do
		uid=$(id -u "$user")
		uids+=("$uid")
		saturate_host "$user" 2
	done

	# CPU_THRESHOLD_DURATION plus a full cadence of margin.
	for uid in "${uids[@]}"; do
		if ! wait_for_limited_cgroup "$uid" 360; then
			daemon_log_since "$log_marker" | tail -80 >"$evidence_dir/decision-trail.txt"
			uptime >"$evidence_dir/host-load.txt" 2>&1
			fail "UID $uid was never moved into a managed cgroup under sustained load"
		fi
	done

	# After resman-4pw.12 removed the per-user limited quota knob, CPU enforcement
	# is the collective ceiling on the shared parent plus per-user weights. The
	# leaf legitimately keeps an unlimited cpu.max unless a workload policy sets
	# one, so the parent is what must carry a finite quota.
	local shared_quota
	shared_quota=$(cat "$managed_cgroup_root/limited/cpu.max" 2>/dev/null)
	printf 'shared cpu.max=%s\n' "$shared_quota" >"$evidence_dir/enforced-quotas.txt"
	[[ $shared_quota == max* ]] \
		&& fail "the shared enforcement cgroup carries no finite CPU quota"

	for uid in "${uids[@]}"; do
		quota=$(cat "$(limited_cgroup_for "$uid")/cpu.max" 2>/dev/null)
		weight=$(cat "$(limited_cgroup_for "$uid")/cpu.weight" 2>/dev/null)
		procs=$(wc -l <"$(limited_cgroup_for "$uid")/cgroup.procs" 2>/dev/null || echo 0)
		printf 'uid=%s cpu.max=%s cpu.weight=%s procs=%s\n' \
			"$uid" "$quota" "$weight" "$procs" >>"$evidence_dir/enforced-quotas.txt"
		[[ -n $weight ]] || fail "UID $uid has no CPU weight inside the shared hierarchy"
		(( procs > 0 )) || fail "UID $uid has a managed cgroup that holds no processes"
	done

	assert_no_protected_process \
		|| fail "a protected identity was moved into a managed cgroup during enforcement"

	# Releasing must survive MIN_ACTIVE_TIME plus the release evaluation.
	stop_user_load
	for uid in "${uids[@]}"; do
		wait_for_released_cgroup "$uid" 420 \
			|| fail "UID $uid was still enforced long after its load stopped"
	done

	systemctl stop resman || fail "systemctl stop failed after the enforcement scenario"
	result=PASS
	detail="three accounts crossed the threshold together under one finite shared ceiling with per-user weights, and all were released after the load stopped"
}

# scenario_shutdown_restoration_under_load stops the daemon while limits are
# active and proves the processes are returned out of managed cgroups.
scenario_shutdown_restoration_under_load() {
	local log_marker uid pid_list
	write_scenario_configuration
	configure_enforcement

	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed for the shutdown scenario"
	wait_for_daemon_log 'Control cycle completed' 1 120 "$log_marker" >/dev/null \
		|| fail "the daemon did not reach steady state before load was applied"

	uid=$(id -u resman-t1)
	saturate_host resman-t1 4
	wait_for_limited_cgroup "$uid" 300 \
		|| fail "UID $uid was never enforced, so shutdown restoration cannot be observed"

	pid_list=$(cat "$(limited_cgroup_for "$uid")/cgroup.procs" 2>/dev/null | tr '\n' ' ')
	printf 'enforced_pids=%s\n' "$pid_list" >"$evidence_dir/pre-shutdown-pids.txt"
	[[ -n ${pid_list// /} ]] || fail "the managed cgroup held no processes before shutdown"

	systemctl stop resman || fail "systemctl stop failed while limits were active"

	# resman-4pw.42: shutdown must return every process out of managed cgroups.
	[[ -d $(limited_cgroup_for "$uid") ]] \
		&& fail "the managed cgroup survived daemon shutdown"
	find "$managed_cgroup_root" -mindepth 1 -maxdepth 2 -type d \
		>"$evidence_dir/post-shutdown-cgroups.txt" 2>/dev/null || true
	[[ -s $evidence_dir/post-shutdown-cgroups.txt ]] \
		&& fail "managed cgroups survived daemon shutdown"

	# The workload must still be alive: restoration moves processes, never kills them.
	local pid alive=0
	for pid in $pid_list; do
		[[ -d /proc/$pid ]] && alive=$((alive + 1))
	done
	printf 'alive_after_shutdown=%s\n' "$alive" >>"$evidence_dir/pre-shutdown-pids.txt"
	(( alive > 0 )) || fail "no enforced process survived daemon shutdown"

	stop_user_load
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"
	result=PASS
	detail="shutdown returned the enforced processes out of managed cgroups and left them running"
}


# scenario_limit_hook_delivery proves the limit hook fires on a real activation
# with the field names of resman-4pw.61 and without leaking secrets.
scenario_limit_hook_delivery() {
	local log_marker uid hook=/usr/local/bin/resman-functional-hook
	local record=/var/lib/resman/functional-hook.env
	write_scenario_configuration
	configure_enforcement

	rm -f "$record"
	cat >"$hook" <<'HOOK'
#!/bin/sh
{
	echo "uid=$RESMAN_LIMIT_UID"
	echo "username=$RESMAN_LIMIT_USERNAME"
	echo "cpu=$RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT"
	echo "eligible=$RESMAN_LIMIT_CPU_ELIGIBLE_USERS_COUNT"
	echo "shared=$RESMAN_LIMIT_SHARED_CGROUP"
	echo "timestamp=$RESMAN_LIMIT_TIMESTAMP"
} >>/var/lib/resman/functional-hook.env
HOOK
	chmod 0700 "$hook"
	hook_installed=1
	# The hook needs both its enable flag and a script path.
	sed -i \
		-e 's|^LIMIT_HOOK_ENABLED=.*|LIMIT_HOOK_ENABLED=true|' \
		-e "s|^LIMIT_HOOK_SCRIPT=.*|LIMIT_HOOK_SCRIPT=$hook|" \
		"$config_path"

	log_marker=$(daemon_log_lines)
	systemctl start resman || fail "systemctl start failed for the hook scenario"
	wait_for_daemon_log 'Control cycle completed' 1 120 "$log_marker" >/dev/null \
		|| fail "the daemon did not reach steady state before load was applied"

	uid=$(id -u resman-t2)
	saturate_host resman-t2 4
	wait_for_limited_cgroup "$uid" 360 \
		|| fail "UID $uid was never enforced, so the hook cannot have fired"

	local deadline=$((SECONDS + 120))
	while (( SECONDS < deadline )); do
		[[ -s $record ]] && break
		sleep 5
	done
	[[ -s $record ]] || fail "the limit hook did not run for a real activation"
	install -m 0600 "$record" "$evidence_dir/hook-environment.txt"

	grep -q "^uid=$uid$" "$record" \
		|| fail "the hook did not receive the enforced UID"
	grep -q '^username=resman-t2$' "$record" \
		|| fail "the hook did not receive the enforced username"
	grep -qE '^cpu=[0-9]+\.[0-9]+$' "$record" \
		|| fail "the hook did not receive a numeric enforceable CPU percentage"
	grep -qE '^eligible=[0-9]+$' "$record" \
		|| fail "the hook did not receive the eligible user count"
	grep -q '^shared=/sys/fs/cgroup/resman/limited$' "$record" \
		|| fail "the hook did not receive the shared cgroup path"

	# resman-4pw.62: the daemon log must not carry hook internals.
	daemon_log_since "$log_marker" | grep -F "$hook" >"$evidence_dir/hook-log-leak.txt" || true
	[[ -s $evidence_dir/hook-log-leak.txt ]] \
		&& fail "the daemon log disclosed the hook script path"

	stop_user_load
	systemctl stop resman || fail "systemctl stop failed after the hook scenario"
	assert_no_protected_process || fail "a protected identity was moved into a managed cgroup"

	result=PASS
	detail="a real activation delivered the documented hook environment without disclosing hook internals in the log"
}

case "$scenario" in
	service-start-stop) scenario_service_start_stop ;;
	service-reload-lifecycle) scenario_service_reload_lifecycle ;;
	service-fatal-config) scenario_service_fatal_config ;;
	prometheus-scrape) scenario_prometheus_scrape ;;
	prometheus-user-series-lifecycle) scenario_prometheus_user_series_lifecycle ;;
	pid-namespace-mixed-ingress) scenario_pid_namespace_mixed_ingress ;;
	pid-namespace-container-only) scenario_pid_namespace_container_only ;;
	mcp-https-endtoend) scenario_mcp_https_endtoend ;;
	blackout-timeframe) scenario_blackout_timeframe ;;
	metrics-database-lifecycle) scenario_metrics_database_lifecycle ;;
	multi-user-enforcement) scenario_multi_user_enforcement ;;
	shutdown-restoration-under-load) scenario_shutdown_restoration_under_load ;;
	limit-hook-delivery) scenario_limit_hook_delivery ;;
	*) fail "unsupported packaged-service scenario: $scenario" ;;
esac

exit 0
