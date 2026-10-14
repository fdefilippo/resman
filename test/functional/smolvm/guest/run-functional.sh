#!/usr/bin/env bash
set -Eeuo pipefail

run_id=${1:?run id is required}
allocated_cpus=${2:?allocated CPU count is required}
allocated_memory_mib=${3:?allocated memory is required}
require_psi=${4:?PSI requirement flag is required}
scenario=${5:?scenario is required}
source_revision=${6:?source revision is required}
artifact_dir=/mnt/resman-artifacts
runtime_dir=/run/resman-functional/$run_id
state_dir=/var/lib/resman-functional/$run_id
config_file=$runtime_dir/resman.conf
service=resman-functional@${run_id}.service
result_file=$artifact_dir/result
mcp_pid=
mcp_workload_pid=
cpu_workload_pid=
expected_daemon_error_patterns=()

case "$run_id" in
    *[!a-z0-9-]*|'')
        echo "invalid run id: $run_id" >&2
        exit 2
        ;;
esac
case "$scenario" in
	missing-io-startup|mcp-filter-reload|psi-refresh-neutrality|non-systemd-observation) ;;
	*)
		echo "invalid scenario: $scenario" >&2
		exit 2
		;;
esac
if [[ $scenario == missing-io-startup ]]; then
	expected_daemon_error_patterns+=(
		'Failed to initialize systemd-native enforcement.*I/O limiting.*controller "io".*interface "io.max"'
	)
fi

mkdir -p "$artifact_dir" "$runtime_dir" "$state_dir"
chmod 0700 "$runtime_dir" "$state_dir"
exec > >(tee -a "$artifact_dir/guest.log") 2>&1
python3 /opt/resman-functional/evidence-metadata.py "$artifact_dir" "$run_id" "$source_revision" "$scenario"
if [[ $scenario == non-systemd-observation ]]; then
    exec python3 /opt/resman-functional/non-systemd-observation.py "$run_id" "$source_revision"
fi

result=FAIL
detail="guest harness did not complete"

# Invoked by the EXIT trap below.
# shellcheck disable=SC2329
finish() {
    local status=$?
	local daemon_error_assertion=PASS
	trap - EXIT
    set +e
	if [[ -n $mcp_pid ]]; then
		kill -TERM "$mcp_pid" 2>/dev/null
		wait "$mcp_pid" 2>/dev/null
		mcp_pid=
	fi
	if [[ -n $mcp_workload_pid ]]; then
		kill -TERM "$mcp_workload_pid" 2>/dev/null
		wait "$mcp_workload_pid" 2>/dev/null
		mcp_workload_pid=
	fi
	if [[ -n $cpu_workload_pid ]]; then
		kill -TERM "$cpu_workload_pid" 2>/dev/null
		wait "$cpu_workload_pid" 2>/dev/null
		cpu_workload_pid=
	fi
    ps -eo pid,ppid,uid,user,comm,args >"$artifact_dir/processes.txt" 2>&1
    curl --fail --silent --show-error --max-time 2 \
        http://127.0.0.1:19100/metrics >"$artifact_dir/prometheus-metrics-final.txt" 2>&1
    systemctl stop "$service" >/dev/null 2>&1
	cp "$state_dir/resman.log" "$artifact_dir/resman.log" 2>/dev/null
    journalctl -u "$service" --no-pager >"$artifact_dir/resman-journal.log" 2>&1
	if ! /opt/resman-functional/assert-daemon-errors.sh "$scenario" "$artifact_dir" \
		"${expected_daemon_error_patterns[@]}"; then
		daemon_error_assertion=FAIL
		if [[ $result == PASS ]]; then
			detail="unexpected or missing expected daemon error; inspect daemon error evidence"
		fi
		result=FAIL
		status=1
	fi
	if [[ $result == PASS && $status -ne 0 ]]; then
		result=FAIL
		detail="scenario reported PASS after a non-zero exit"
	fi
	if [[ $result == FAIL && $status -eq 0 ]]; then
		status=1
	fi
    printf '%s\n' "$result" >"$result_file"
	if [[ $result == PASS ]]; then
		# A per-scenario key is published only after the existing scenario and
		# daemon-error assertions have both succeeded, never as an inventory stub.
		printf '{"%s":"PASS"}\n' "$scenario" >"$artifact_dir/checks.json"
		printf '{"scenario":"%s","daemon_error_assertion":"PASS","detail":"see guest.log and scenario evidence"}\n' \
			"$scenario" >"$artifact_dir/$scenario.json"
	fi
	printf 'daemon_error_assertion=%s\nresult=%s\ndetail=%s\nexit_code=%d\n' \
		"$daemon_error_assertion" "$result" "$detail" "$status" \
        >>"$artifact_dir/environment.txt"
	exit "$status"
}
trap finish EXIT

blocked() {
    result=BLOCKED
    detail=$1
    echo "BLOCKED: $detail" >&2
    exit 77
}

fail() {
    detail=$1
    echo "FAIL: $detail" >&2
    exit 1
}

[[ $(ps -p 1 -o comm= | tr -d ' ') == systemd ]] || blocked "systemd is not PID 1"
[[ $(findmnt -n -o FSTYPE /sys/fs/cgroup) == cgroup2 ]] || blocked "cgroup v2 is not mounted"
[[ -r /sys/fs/cgroup/cgroup.controllers ]] || blocked "cgroup.controllers is not readable"
[[ -w /sys/fs/cgroup/cgroup.subtree_control ]] || blocked "cgroup root is not writable"

controllers=$(< /sys/fs/cgroup/cgroup.controllers)
required_controllers=(cpu)
if [[ $scenario == missing-io-startup ]]; then
	required_controllers+=(memory io)
fi
for controller in "${required_controllers[@]}"; do
    [[ " $controllers " == *" $controller "* ]] || blocked "required $controller controller is unavailable"
	if [[ " $(< /sys/fs/cgroup/cgroup.subtree_control) " != *" $controller "* ]]; then
		printf '+%s\n' "$controller" > /sys/fs/cgroup/cgroup.subtree_control \
			|| blocked "cannot enable required $controller controller for interface probing"
	fi
done
psi_available=true
for pressure_file in cpu memory io; do
    [[ -r /proc/pressure/$pressure_file ]] || psi_available=false
done
if [[ $require_psi == 1 && $psi_available != true ]]; then
    blocked "PSI was required but CPU, memory, or I/O pressure data is unavailable"
fi

pressure_summary() {
    local pressure_file=$1
    if [[ -r /proc/pressure/$pressure_file ]]; then
        tr '\n' ';' <"/proc/pressure/$pressure_file"
    else
        printf 'unavailable'
    fi
}

controller_probe=/sys/fs/cgroup/resman-functional-probe-$run_id
mkdir "$controller_probe" || blocked "cannot create the controller-interface probe cgroup"
cpu_max_available=false
memory_max_available=false
io_max_available=false
[[ -e $controller_probe/cpu.max ]] && cpu_max_available=true
[[ -e $controller_probe/memory.max ]] && memory_max_available=true
[[ -e $controller_probe/io.max ]] && io_max_available=true
rmdir "$controller_probe" || fail "cannot remove the controller-interface probe cgroup"
[[ $cpu_max_available == true ]] || blocked "cpu controller is listed but cpu.max is unavailable"
if [[ $scenario == missing-io-startup ]]; then
	[[ $memory_max_available == true ]] || blocked "memory.max is also unavailable; missing-io startup attribution would be ambiguous"
	[[ $io_max_available == false ]] || blocked "io.max is available; the guest cannot exercise missing-interface startup rejection"
fi


sed "s/@RUN_ID@/$run_id/g" /opt/resman-functional/fixtures/resman.conf >"$config_file"
printf '%s\n' '[resman-cpu-points-map-v1]' >"$runtime_dir/cpu-points.map"
chmod 0600 "$runtime_dir/cpu-points.map"
if [[ $scenario == mcp-filter-reload ]]; then
	sed -i \
		-e 's/^RAM_LIMIT_ENABLED=.*/RAM_LIMIT_ENABLED=false/' \
		-e 's/^IO_LIMIT_ENABLED=.*/IO_LIMIT_ENABLED=false/' \
		-e 's/^METRICS_CACHE_TTL=.*/METRICS_CACHE_TTL=1/' \
		-e 's/^MCP_ENABLED=.*/MCP_ENABLED=true/' \
		-e 's/^MCP_TRANSPORT=.*/MCP_TRANSPORT=stdio/' \
		"$config_file"
fi
if [[ $scenario == psi-refresh-neutrality ]]; then
	sed -i \
		-e 's/^POLLING_INTERVAL=.*/POLLING_INTERVAL=120/' \
		-e 's/^METRICS_REFRESH_INTERVAL=.*/METRICS_REFRESH_INTERVAL=5/' \
		-e 's/^RAM_LIMIT_ENABLED=.*/RAM_LIMIT_ENABLED=false/' \
		-e 's/^IO_LIMIT_ENABLED=.*/IO_LIMIT_ENABLED=false/' \
		-e 's/^PSI_EVENT_DRIVEN=.*/PSI_EVENT_DRIVEN=true/' \
		"$config_file"
	cat >>"$config_file" <<'EOF'
PSI_CPU_STALL_THRESHOLD=999999
PSI_IO_STALL_THRESHOLD=999999
PSI_FALLBACK_INTERVAL=30
EOF
fi
chmod 0600 "$config_file"
printf 'RESMAN_CONFIG=%s\n' "$config_file" >"$runtime_dir/environment"
chmod 0600 "$runtime_dir/environment"

{
    printf 'kernel=%s\n' "$(uname -r)"
    printf 'uname=%s\n' "$(uname -srvmo)"
    printf 'kernel_cmdline=%s\n' "$(< /proc/cmdline)"
    printf 'pid1=%s\n' "$(ps -p 1 -o comm= | tr -d ' ')"
    printf 'cgroup_mount=%s\n' "$(findmnt -n -o SOURCE,FSTYPE,OPTIONS /sys/fs/cgroup)"
    printf 'controllers_available=%s\n' "$controllers"
	printf 'controllers_enabled=%s\n' "$(< /sys/fs/cgroup/cgroup.subtree_control)"
	printf 'cpu_max_available=%s\n' "$cpu_max_available"
	printf 'memory_max_available=%s\n' "$memory_max_available"
	printf 'io_max_available=%s\n' "$io_max_available"
    printf 'psi_required=%s\n' "$require_psi"
	printf 'psi_available=%s\n' "$psi_available"
	printf 'scenario=%s\n' "$scenario"
    printf 'psi_cpu=%s\n' "$(pressure_summary cpu)"
    printf 'psi_memory=%s\n' "$(pressure_summary memory)"
    printf 'psi_io=%s\n' "$(pressure_summary io)"
    printf 'allocated_cpus=%s\n' "$allocated_cpus"
    printf 'allocated_memory_mib=%s\n' "$allocated_memory_mib"
    printf 'guest_observed_cpus=%s\n' "$(nproc)"
    printf 'guest_observed_memory_kib=%s\n' "$(awk '/MemTotal:/ {print $2}' /proc/meminfo)"
    printf 'stress_version=%s\n' "$(stress --version | head -n 1)"
    printf 'config_path=%s\n' "$config_file"
    printf 'database_path=%s\n' "$state_dir/metrics.db"
    printf 'prometheus_endpoint=%s\n' 'http://127.0.0.1:19100/metrics'
	if [[ $scenario == mcp-filter-reload ]]; then
		printf 'mcp_endpoint=%s\n' 'stdio'
	else
		printf 'mcp_endpoint=%s\n' 'disabled'
	fi
} >>"$artifact_dir/environment.txt"

systemctl daemon-reload
if [[ $scenario == missing-io-startup ]]; then
	set +e
	timeout 15s /usr/bin/resman --config "$config_file" \
		>"$artifact_dir/controller-startup-rejection.txt" 2>&1
	startup_status=$?
	set -e
	if [[ $startup_status -eq 0 ]]; then
		fail "resman started with IO limiting enabled but io.max unavailable"
	fi
	[[ $startup_status -ne 124 ]] || fail "resman did not reject the missing io.max interface within 15 seconds"
	grep -Fq 'I/O limiting' "$artifact_dir/controller-startup-rejection.txt" \
		|| fail "startup rejection did not name the I/O limiting feature"
	grep -Fq 'controller "io"' "$artifact_dir/controller-startup-rejection.txt" \
		|| fail "startup rejection did not name the io controller"
	grep -Fq 'interface "io.max"' "$artifact_dir/controller-startup-rejection.txt" \
		|| fail "startup rejection did not name the io.max interface"
	if systemctl list-units --all --plain --no-legend 'user-resmancapprobe*.slice' | grep -q .; then
		fail "systemd capability probe unit remained loaded after startup rejection"
	fi
	if systemctl list-units --all --plain --no-legend 'resmancapprobe*.service' | grep -q .; then
		fail "systemd capability probe service remained loaded after startup rejection"
	fi
	if find /sys/fs/cgroup -type d -name 'user-resmancapprobe*.slice' -print -quit | grep -q .; then
		fail "systemd capability probe cgroup remained after startup rejection"
	fi
	if find /sys/fs/cgroup -type d -name 'resmancapprobe*.service' -print -quit | grep -q .; then
		fail "systemd capability probe service cgroup remained after startup rejection"
	fi
	result=PASS
	detail="startup rejected IO limiting because user.slice lacked io.max"
	echo "PASS: $detail"
	exit 0
fi
if [[ $scenario == psi-refresh-neutrality ]]; then
	[[ $psi_available == true ]] || blocked "PSI refresh neutrality requires CPU, memory, and I/O pressure data"
	/opt/resman-functional/workload.sh cpu 90s &
	cpu_workload_pid=$!
fi
if [[ $scenario == mcp-filter-reload ]]; then
	mcp_stdout=$artifact_dir/mcp-filter-reload.jsonl
	mcp_stderr=$artifact_dir/mcp-filter-reload.stderr
	/opt/resman-functional/workload.sh cpu 60s &
	mcp_workload_pid=$!
	coproc RESMAN_MCP { /usr/bin/resman --config "$config_file" 2>"$mcp_stderr"; }
	mcp_pid=$RESMAN_MCP_PID

	request_meta='"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"smolvm-functional","version":"1"}}'
	status_cpu_observed=false
	for request_id in $(seq 1 10); do
		printf '{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{%s,"name":"get_system_status","arguments":{}}}\n' \
			"$request_id" "$request_meta" >&"${RESMAN_MCP[1]}"
		if ! IFS= read -r -t 20 response <&"${RESMAN_MCP[0]}"; then
			kill -TERM "$mcp_pid" 2>/dev/null || true
			fail "MCP system status did not return within 20 seconds"
		fi
		printf '%s\n' "$response" >>"$mcp_stdout"
		if [[ $response =~ \"observed_users_cpu_usage\":([0-9.eE+-]+) ]] \
			&& awk -v value="${BASH_REMATCH[1]}" 'BEGIN { exit !(value > 0) }'; then
			status_cpu_observed=true
			break
		fi
		sleep 1
	done
	[[ $response == *'"observed_users_cpu_usage"'* ]] \
		|| fail "MCP system status omitted observed user CPU usage"
	[[ $status_cpu_observed == true ]] \
		|| fail "MCP system status never reported the live observed CPU workload"
	[[ $response == *'"observed_users_count":'* ]] \
		|| fail "MCP system status omitted the observed-user count"
	[[ $response == *'"actively_limited_users_count":0'* ]] \
		|| fail "MCP system status did not report the observed zero actively-limited users"
	[[ $response != *'total_user_cpu_usage'* && $response != *'"active_users_count"'* \
		&& $response != *'"limits_active"'* && $response != *'"limits_applied_time"'* ]] \
		|| fail "MCP system status exposed a removed metric or runtime alias"

	printf '{"jsonrpc":"2.0","id":20,"method":"resources/read","params":{%s,"uri":"resman://limits/status"}}\n' \
		"$request_meta" >&"${RESMAN_MCP[1]}"
	if ! IFS= read -r -t 20 response <&"${RESMAN_MCP[0]}"; then
		kill -TERM "$mcp_pid" 2>/dev/null || true
		fail "MCP limits status resource did not return within 20 seconds"
	fi
	printf '%s\n' "$response" >>"$mcp_stdout"
	[[ $response == *'actively_limited_users_count'* \
		&& $response == *'cpu_actively_limited_users_count'* \
		&& $response == *'resource_limits_active'* ]] \
		|| fail "MCP limits resource did not expose the explicit runtime contract"
	[[ $response != *'active_users_count'* && $response != *'"limits_active"'* \
		&& $response != *'"limits_applied_time"'* ]] \
		|| fail "MCP limits resource exposed a removed runtime alias"

	printf '{"jsonrpc":"2.0","id":21,"method":"prompts/get","params":{%s,"name":"system-health","arguments":{}}}\n' \
		"$request_meta" >&"${RESMAN_MCP[1]}"
	if ! IFS= read -r -t 20 response <&"${RESMAN_MCP[0]}"; then
		kill -TERM "$mcp_pid" 2>/dev/null || true
		fail "MCP system-health prompt did not return within 20 seconds"
	fi
	printf '%s\n' "$response" >>"$mcp_stdout"
	[[ $response == *'Observed Users'* && $response == *'Actively Limited Users'* \
		&& $response == *'Resource Limits Active'* ]] \
		|| fail "MCP system-health prompt did not use the explicit status semantics"

	printf '{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{%s,"name":"set_user_include_list","arguments":{"patterns":["^resman-cpu$"]}}}\n' \
		"$request_meta" >&"${RESMAN_MCP[1]}"
	if ! IFS= read -r -t 20 response <&"${RESMAN_MCP[0]}"; then
		kill -TERM "$mcp_pid" 2>/dev/null || true
		fail "MCP filter update did not return a confirmed result within 20 seconds"
	fi
	printf '%s\n' "$response" >>"$mcp_stdout"
	[[ $response == *'"persisted":true'* ]] || fail "MCP filter response did not confirm persistence"
	[[ $response == *'"applied":true'* ]] || fail "MCP filter response did not confirm runtime application"
	grep -Fq 'USER_INCLUDE_LIST=^resman-cpu$' "$config_file" \
		|| fail "confirmed MCP filter update is absent from the config file"

	printf '{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{%s,"name":"set_user_include_list","arguments":{"patterns":["^must-not-apply$"],"reload":false}}}\n' \
		"$request_meta" >&"${RESMAN_MCP[1]}"
	if ! IFS= read -r -t 20 response <&"${RESMAN_MCP[0]}"; then
		kill -TERM "$mcp_pid" 2>/dev/null || true
		fail "MCP legacy-parameter rejection did not return within 20 seconds"
	fi
	printf '%s\n' "$response" >>"$mcp_stdout"
	[[ $response == *'was removed'* ]] || fail "MCP did not explicitly reject the removed reload parameter"
	grep -Fq 'USER_INCLUDE_LIST=^resman-cpu$' "$config_file" \
		|| fail "rejected legacy request changed the persisted filter"
	if grep -Fq 'must-not-apply' "$config_file"; then
		fail "rejected legacy request was persisted"
	fi

	kill -TERM "$mcp_pid" 2>/dev/null || true
	wait "$mcp_pid" 2>/dev/null || true
	mcp_pid=
	kill -TERM "$mcp_workload_pid" 2>/dev/null || true
	wait "$mcp_workload_pid" 2>/dev/null || true
	mcp_workload_pid=
	result=PASS
	detail="MCP stdio status surfaces used distinct observation/runtime contracts; filter reload was acknowledged and removed input was rejected"
	echo "PASS: $detail"
	exit 0
fi
if ! systemctl start "$service"; then
    fail "resman systemd service failed to start"
fi
systemctl is-active --quiet "$service" || fail "resman systemd service is not active"

curl --fail --silent --show-error --retry 20 --retry-all-errors --retry-delay 1 \
    --max-time 2 http://127.0.0.1:19100/metrics >"$artifact_dir/prometheus-metrics.txt" \
    || fail "Prometheus endpoint did not become ready"
grep -q '^resman_' "$artifact_dir/prometheus-metrics.txt" \
    || fail "Prometheus endpoint contains no resman metrics"
[[ -f "$state_dir/metrics.db" ]] || fail "SQLite metrics database was not created"
sqlite3 "$state_dir/metrics.db" '.schema' >"$artifact_dir/database-schema.sql" \
    || fail "SQLite metrics database is unreadable"

metric_value() {
	local metric=$1
	local metrics_file=$2
	awk -v metric="$metric" '$1 ~ ("^" metric "({|$)") { print $2; exit }' "$metrics_file"
}

user_metric_value() {
	local metric=$1
	local username=$2
	local metrics_file=$3
	awk -v metric="$metric" -v username="$username" \
		'$1 ~ ("^" metric "{") && $1 ~ ("username=\"" username "\"") { print $2; exit }' \
		"$metrics_file"
}



if [[ $scenario == psi-refresh-neutrality ]]; then
	baseline_metrics=$artifact_dir/psi-baseline.prom
	after_refresh_metrics=$artifact_dir/psi-after-refresh.prom
	cycles_before=0
	for _ in $(seq 1 30); do
		curl --fail --silent --show-error --max-time 2 \
			http://127.0.0.1:19100/metrics >"$baseline_metrics" \
			|| fail "cannot scrape the PSI baseline"
		cycles_before=$(metric_value resman_control_cycles_total "$baseline_metrics")
		user_cpu_before=$(user_metric_value resman_user_cpu_usage_percent resman-cpu "$baseline_metrics")
		if [[ ${cycles_before:-0} -ge 2 && -n ${user_cpu_before:-} ]]; then
			break
		fi
		sleep 1
	done
	[[ ${cycles_before:-0} -ge 2 ]] || fail "the PSI fallback cycle did not establish a decision sample"
	[[ -n ${user_cpu_before:-} ]] || fail "the PSI baseline lacks the decision-owned user CPU series"
	user_ema_before=$(user_metric_value resman_user_cpu_usage_ema_percent resman-cpu "$baseline_metrics")
	collection_before=$(metric_value resman_metrics_collection_duration_seconds_count "$baseline_metrics")
	system_cpu_before=$(metric_value resman_cpu_total_usage_percent "$baseline_metrics")
	[[ -n ${user_ema_before:-} && -n ${collection_before:-} && -n ${system_cpu_before:-} ]] \
		|| fail "the PSI baseline lacks required decision or observation metrics"

	kill -TERM "$cpu_workload_pid" 2>/dev/null || true
	wait "$cpu_workload_pid" 2>/dev/null || true
	cpu_workload_pid=
	refresh_ready=false
	for _ in $(seq 1 15); do
		sleep 1
		curl --fail --silent --show-error --max-time 2 \
			http://127.0.0.1:19100/metrics >"$after_refresh_metrics" \
			|| fail "cannot scrape after PSI observation refreshes"
		cycles_after=$(metric_value resman_control_cycles_total "$after_refresh_metrics")
		collection_after=$(metric_value resman_metrics_collection_duration_seconds_count "$after_refresh_metrics")
		if [[ $cycles_after == "$cycles_before" \
			&& ${collection_after:-0} -ge $((collection_before + 2)) ]]; then
			refresh_ready=true
			break
		fi
	done
	[[ $refresh_ready == true ]] \
		|| fail "two observation refreshes did not complete before another control cycle"
	user_cpu_after=$(user_metric_value resman_user_cpu_usage_percent resman-cpu "$after_refresh_metrics")
	user_ema_after=$(user_metric_value resman_user_cpu_usage_ema_percent resman-cpu "$after_refresh_metrics")
	system_cpu_after=$(metric_value resman_cpu_total_usage_percent "$after_refresh_metrics")
	[[ $user_cpu_after == "$user_cpu_before" ]] \
		|| fail "an observation refresh changed decision-owned user CPU"
	[[ $user_ema_after == "$user_ema_before" ]] \
		|| fail "an observation refresh changed decision-owned user EMA"
	[[ $system_cpu_after != "$system_cpu_before" ]] \
		|| fail "the observation stream did not change after the workload stopped"
	grep -Fq 'PSI event-driven mode enabled' "$state_dir/resman.log" \
		|| fail "PSI was available but event-driven mode did not become active"
	psi_events=$(metric_value resman_psi_events_total "$after_refresh_metrics")
	[[ -z ${psi_events:-} || $psi_events == 0 ]] \
		|| fail "a PSI event invalidated the refresh-only assertion window"
	{
		printf 'control_cycles_before=%s\n' "$cycles_before"
		printf 'control_cycles_after=%s\n' "$cycles_after"
		printf 'collections_before=%s\n' "$collection_before"
		printf 'collections_after=%s\n' "$collection_after"
		printf 'decision_cpu_before=%s\n' "$user_cpu_before"
		printf 'decision_cpu_after=%s\n' "$user_cpu_after"
		printf 'decision_ema_before=%s\n' "$user_ema_before"
		printf 'decision_ema_after=%s\n' "$user_ema_after"
		printf 'system_observation_before=%s\n' "$system_cpu_before"
		printf 'system_observation_after=%s\n' "$system_cpu_after"
	} >"$artifact_dir/psi-refresh-neutrality.txt"
	result=PASS
	detail="PSI observation refreshes changed observation state without advancing decision CPU or EMA"
	echo "PASS: $detail"
	exit 0
fi

fail "scenario $scenario has no measurement in the guest harness"
