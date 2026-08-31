#!/usr/bin/env bash
set -Eeuo pipefail

run_id=${1:?run id is required}
allocated_cpus=${2:?allocated CPU count is required}
allocated_memory_mib=${3:?allocated memory is required}
require_psi=${4:?PSI requirement flag is required}
scenario=${5:?scenario is required}
artifact_dir=/mnt/resman-artifacts
runtime_dir=/run/resman-functional/$run_id
state_dir=/var/lib/resman-functional/$run_id
config_file=$runtime_dir/resman.conf
service=resman-functional@${run_id}.service
result_file=$artifact_dir/result
mcp_pid=
mcp_workload_pid=
cpu_workload_pid=
memory_workload_pid=
io_workload_pid=
io_anchor_pid=
container_workload_pid=
container_name=
container_log_dir=
functional_cgroup_root=/sys/fs/cgroup
expected_daemon_error_patterns=()

# SmolVM does not expose /dev/fuse to the guest. Keep nested Podman state
# isolated to this run and use vfs so the container scenario does not depend on
# a host device that is unrelated to resman's runtime contract.
container_podman() {
	sudo podman --root "$state_dir/podman-storage" \
		--runroot "$runtime_dir/podman-run" --storage-driver=vfs "$@"
}

case "$run_id" in
    *[!a-z0-9-]*|'')
        echo "invalid run id: $run_id" >&2
        exit 2
        ;;
esac
case "$scenario" in
	resource-only|memory-only|process-membership|cpu-without-cpuset|missing-io-startup|mcp-filter-reload|container-runtime|block-iops|psi-refresh-neutrality) ;;
	*)
		echo "invalid scenario: $scenario" >&2
		exit 2
		;;
esac
if [[ $scenario == missing-io-startup ]]; then
	expected_daemon_error_patterns+=(
		'Failed to initialize cgroup manager.*I/O limiting.*controller "io".*interface "io.max"'
	)
fi
if [[ $scenario == process-membership ]]; then
	expected_daemon_error_patterns+=(
		'Error in control cycle.*recorded origin is unavailable'
	)
fi

mkdir -p "$artifact_dir" "$runtime_dir" "$state_dir"
chmod 0700 "$runtime_dir" "$state_dir"
exec > >(tee -a "$artifact_dir/guest.log") 2>&1

result=FAIL
detail="guest harness did not complete"

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
	if [[ -n $memory_workload_pid ]]; then
		kill -TERM "$memory_workload_pid" 2>/dev/null
		wait "$memory_workload_pid" 2>/dev/null
		memory_workload_pid=
	fi
	if [[ -n $io_workload_pid ]]; then
		kill -TERM "$io_workload_pid" 2>/dev/null
		wait "$io_workload_pid" 2>/dev/null
		io_workload_pid=
	fi
	if [[ -n $io_anchor_pid ]]; then
		kill -TERM "$io_anchor_pid" 2>/dev/null
		wait "$io_anchor_pid" 2>/dev/null
		io_anchor_pid=
	fi
	if [[ -n $container_name ]]; then
		container_podman logs "$container_name" >"$artifact_dir/container-stdout.log" 2>"$artifact_dir/container-stderr.log"
		container_podman rm --force "$container_name" >/dev/null 2>&1
		container_name=
	fi
	if [[ -n $container_workload_pid ]]; then
		pkill -TERM -u resman-cpu -x stress >/dev/null 2>&1
		wait "$container_workload_pid" 2>/dev/null
		container_workload_pid=
	fi
    ps -eo pid,ppid,uid,user,comm,args >"$artifact_dir/processes.txt" 2>&1
    find "$functional_cgroup_root/resman-functional-$run_id" -maxdepth 3 -type d -print \
        >"$artifact_dir/cgroup-tree.txt" 2>&1
    curl --fail --silent --show-error --max-time 2 \
        http://127.0.0.1:19100/metrics >"$artifact_dir/prometheus-metrics-final.txt" 2>&1
    systemctl stop "$service" >/dev/null 2>&1
	if [[ -n $container_log_dir ]]; then
		cp "$container_log_dir/resman.log" "$artifact_dir/resman.log" 2>/dev/null
	else
		cp "$state_dir/resman.log" "$artifact_dir/resman.log" 2>/dev/null
	fi
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

process_cgroup_path() {
	local pid=$1
	awk -F '::' '$1 == "0" { print $2 }' "/proc/$pid/cgroup"
}

process_start_time() {
	local pid=$1
	awk '{ print $22 }' "/proc/$pid/stat"
}

[[ $(ps -p 1 -o comm= | tr -d ' ') == systemd ]] || blocked "systemd is not PID 1"
[[ $(findmnt -n -o FSTYPE /sys/fs/cgroup) == cgroup2 ]] || blocked "cgroup v2 is not mounted"
[[ -r /sys/fs/cgroup/cgroup.controllers ]] || blocked "cgroup.controllers is not readable"
[[ -w /sys/fs/cgroup/cgroup.subtree_control ]] || blocked "cgroup root is not writable"

controllers=$(< /sys/fs/cgroup/cgroup.controllers)
required_controllers=(cpu)
if [[ $scenario == resource-only || $scenario == missing-io-startup ]]; then
	required_controllers+=(memory io)
elif [[ $scenario == memory-only ]]; then
	required_controllers+=(memory)
elif [[ $scenario == block-iops ]]; then
	required_controllers+=(io)
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
if [[ $scenario == resource-only || $scenario == memory-only ]]; then
	[[ $memory_max_available == true ]] || blocked "memory controller is listed but memory.max is unavailable"
fi
if [[ $scenario == resource-only ]]; then
	[[ $io_max_available == true ]] || blocked "io controller is listed but io.max is unavailable"
fi
if [[ $scenario == block-iops ]]; then
	[[ $io_max_available == true ]] || blocked "io controller is listed but io.max is unavailable for the block IOPS scenario"
fi
if [[ $scenario == missing-io-startup ]]; then
	[[ $memory_max_available == true ]] || blocked "memory.max is also unavailable; missing-io startup attribution would be ambiguous"
	[[ $io_max_available == false ]] || blocked "io.max is available; the guest cannot exercise missing-interface startup rejection"
fi

if [[ $scenario == cpu-without-cpuset ]]; then
	cpu_only_parent=/sys/fs/cgroup/resman-functional-cpu-parent-$run_id
	functional_cgroup_root=$cpu_only_parent/root
	mkdir "$cpu_only_parent" || blocked "cannot create the CPU-only delegation parent"
	printf '+cpu\n' >"$cpu_only_parent/cgroup.subtree_control" \
		|| blocked "cannot delegate the mandatory cpu controller"
	mkdir "$functional_cgroup_root" || blocked "cannot create the CPU-only delegated root"
	cpu_only_controllers=$(< "$functional_cgroup_root/cgroup.controllers")
	[[ " $cpu_only_controllers " == *" cpu "* ]] \
		|| blocked "the delegated root does not expose the mandatory cpu controller"
	[[ " $cpu_only_controllers " != *" cpuset "* ]] \
		|| blocked "the delegated root still exposes cpuset and cannot reproduce the scenario"
fi

sed "s/@RUN_ID@/$run_id/g" /opt/resman-functional/fixtures/resman.conf >"$config_file"
printf '%s\n' '[resman-cpu-points-map-v1]' >"$runtime_dir/cpu-points.map"
chmod 0600 "$runtime_dir/cpu-points.map"
if [[ $scenario == block-iops ]]; then
	sed -i \
		-e 's/^RAM_LIMIT_ENABLED=.*/RAM_LIMIT_ENABLED=false/' \
		-e 's/^IO_READ_BPS=.*/IO_READ_BPS=max/' \
		-e 's/^IO_WRITE_BPS=.*/IO_WRITE_BPS=max/' \
		-e 's/^IO_THRESHOLD=.*/IO_THRESHOLD=10/' \
		-e 's/^IO_RELEASE_THRESHOLD=.*/IO_RELEASE_THRESHOLD=5/' \
		"$config_file"
fi
if [[ $scenario == memory-only ]]; then
	sed -i \
		-e 's/^IO_LIMIT_ENABLED=.*/IO_LIMIT_ENABLED=false/' \
		"$config_file"
fi
if [[ $scenario == process-membership || $scenario == cpu-without-cpuset \
	|| $scenario == container-runtime ]]; then
	sed -i \
		-e 's/^CPU_THRESHOLD=.*/CPU_THRESHOLD=10/' \
		-e 's/^USER_INCLUDE_LIST=.*/USER_INCLUDE_LIST=^resman-cpu$/' \
		-e 's/^RAM_LIMIT_ENABLED=.*/RAM_LIMIT_ENABLED=false/' \
		-e 's/^IO_LIMIT_ENABLED=.*/IO_LIMIT_ENABLED=false/' \
		"$config_file"
fi
if [[ $scenario == container-runtime ]]; then
	container_log_dir=$state_dir/container-log
	container_state_dir=$state_dir/container-state
	mkdir -p "$container_log_dir" "$container_state_dir"
	chmod 0700 "$container_log_dir" "$container_state_dir"
	sed -i \
		-e "s|^CGROUP_BASE=.*|CGROUP_BASE=resman-container-$run_id|" \
		-e "s|^CREATED_CGROUPS_FILE=.*|CREATED_CGROUPS_FILE=/run/resman-cgroups.txt|" \
		-e 's|^CPU_POINTS_FILE=.*|CPU_POINTS_FILE=/etc/resman/cpu-points.map|' \
		-e 's/^METRICS_DB_ENABLED=.*/METRICS_DB_ENABLED=false/' \
		-e 's|^LOG_FILE=.*|LOG_FILE=/var/log/resman/resman.log|' \
		"$config_file"
fi
if [[ $scenario == cpu-without-cpuset ]]; then
	sed -i "s|^CGROUP_ROOT=.*|CGROUP_ROOT=$functional_cgroup_root|" "$config_file"
fi
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
    printf 'kernel=%s\n' "$(uname -srvmo)"
    printf 'kernel_cmdline=%s\n' "$(< /proc/cmdline)"
    printf 'pid1=%s\n' "$(ps -p 1 -o comm= | tr -d ' ')"
    printf 'cgroup_mount=%s\n' "$(findmnt -n -o SOURCE,FSTYPE,OPTIONS /sys/fs/cgroup)"
    printf 'controllers_available=%s\n' "$controllers"
	printf 'controllers_enabled=%s\n' "$(< /sys/fs/cgroup/cgroup.subtree_control)"
	if [[ $scenario == cpu-without-cpuset ]]; then
		printf 'delegated_controllers=%s\n' "$cpu_only_controllers"
	fi
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
	if [[ $scenario == container-runtime ]]; then
		printf 'podman_version=%s\n' "$(container_podman version --format '{{.Client.Version}}')"
	fi
    printf 'config_path=%s\n' "$config_file"
    printf 'database_path=%s\n' "$state_dir/metrics.db"
    printf 'cgroup_path=%s\n' "$functional_cgroup_root/resman-functional-$run_id"
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
	result=PASS
	detail="startup rejected IO limiting because the real child cgroup lacked io.max"
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
if [[ $scenario == container-runtime ]]; then
	container_image=localhost/resman-container:$run_id
	container_archive=/mnt/resman-input/resman-container.tar
	[[ -r $container_archive ]] || fail "shipped container image archive is unavailable in the guest"
	container_podman load --input "$container_archive" >"$artifact_dir/container-load.txt" \
		|| fail "cannot load the shipped container image"
	container_user=$(container_podman image inspect --format '{{.Config.User}}' "$container_image")
	[[ $container_user == 0 ]] || fail "shipped container user is $container_user instead of root"
	container_podman run --rm --entrypoint /usr/bin/ldd "$container_image" /usr/local/bin/resman \
		>"$artifact_dir/container-ldd.txt" \
		|| fail "cannot inspect the shipped resman binary's dynamic dependencies"
	grep -Fq 'libc.so' "$artifact_dir/container-ldd.txt" \
		|| fail "shipped resman binary does not expose a libc dependency"

	/opt/resman-functional/workload.sh cpu 90s &
	container_workload_pid=$!
	container_stress_pid=
	for _ in $(seq 1 50); do
		container_stress_pid=$(pgrep -u resman-cpu -x stress 2>/dev/null | tail -n 1 || true)
		[[ -n $container_stress_pid ]] && break
		sleep 0.1
	done
	[[ -n $container_stress_pid ]] || fail "foreign-user CPU workload did not start"
	container_name=resman-container-$run_id
	container_podman run --detach --name "$container_name" \
		--privileged --pid=host --cgroupns=host --network=host \
		--security-opt label=disable \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		-v "$runtime_dir:/etc/resman:ro" \
		-v /etc/passwd:/etc/passwd:ro \
		-v /etc/group:/etc/group:ro \
		-v /etc/nsswitch.conf:/etc/nsswitch.conf:ro \
		-v "$container_state_dir:/var/lib/resman:rw" \
		-v "$container_log_dir:/var/log/resman:rw" \
		"$container_image" >"$artifact_dir/container-id.txt" \
		|| fail "supported sudo podman invocation did not start"

	curl --fail --silent --show-error --retry 20 --retry-all-errors --retry-delay 1 \
		--max-time 2 http://127.0.0.1:19100/metrics \
		>"$artifact_dir/container-prometheus-metrics.txt" \
		|| fail "container Prometheus endpoint did not become ready"
	for access in executable_identity io_decision; do
		awk -v access="$access" '
			/^resman_procfs_unavailable_processes\{/ && index($0, "access=\"" access "\"") && $NF == 0 { found=1 }
			END { exit !found }
		' "$artifact_dir/container-prometheus-metrics.txt" \
			|| fail "healthy container did not publish zero procfs failures for $access"
	done
	cpu_uid=$(id -u resman-cpu)
	resolved_user=$(container_podman exec "$container_name" getent passwd "$cpu_uid")
	[[ $resolved_user == resman-cpu:* ]] \
		|| fail "container NSS did not resolve the foreign host user"
	resolved_executable=$(container_podman exec "$container_name" readlink "/proc/$container_stress_pid/exe")
	[[ $resolved_executable == */stress ]] \
		|| fail "container could not resolve the foreign-user executable identity"
	container_podman exec "$container_name" cat "/proc/$container_stress_pid/io" \
		>"$artifact_dir/container-foreign-process-io.txt" \
		|| fail "container could not read the foreign-user I/O counters"
	for counter in read_bytes write_bytes syscr syscw; do
		grep -Eq "^${counter}:[[:space:]]+[0-9]+$" "$artifact_dir/container-foreign-process-io.txt" \
			|| fail "foreign-user I/O sample is missing counter $counter"
	done

	container_base_cgroup=/sys/fs/cgroup/resman-container-$run_id
	container_limited_cgroup=$container_base_cgroup/limited/best_effort/user_$cpu_uid
	container_quota_ready=false
	for _ in $(seq 1 45); do
		if [[ -r $container_base_cgroup/limited/cpu.max \
			&& $(< "$container_base_cgroup/limited/cpu.max") != "max 100000" \
			&& -r $container_limited_cgroup/cgroup.procs ]] \
			&& grep -qx "$container_stress_pid" "$container_limited_cgroup/cgroup.procs"; then
			container_quota_ready=true
			break
		fi
		sleep 1
	done
	[[ $container_quota_ready == true ]] \
		|| fail "container did not apply a finite CPU quota to the foreign-user workload"
	container_cpu_max=$(< "$container_base_cgroup/limited/cpu.max")
	container_start_time=$(process_start_time "$container_stress_pid")
	container_podman inspect "$container_name" >"$artifact_dir/container-inspect.json"
	container_podman stop --time 20 "$container_name" >/dev/null \
		|| fail "container did not stop cleanly"
	container_podman logs "$container_name" >"$artifact_dir/container-stdout.log" \
		2>"$artifact_dir/container-stderr.log"
	container_exit_code=$(container_podman inspect --format '{{.State.ExitCode}}' "$container_name")
	[[ $container_exit_code == 0 ]] \
		|| fail "container shutdown returned exit code $container_exit_code"
	container_final_cgroup=$(process_cgroup_path "$container_stress_pid")
	[[ $container_final_cgroup != *"/limited/"* ]] \
		|| fail "foreign-user workload remained under the finite container limit after shutdown"
	container_start_time_after=$(process_start_time "$container_stress_pid")
	[[ $container_start_time_after == "$container_start_time" ]] \
		|| fail "foreign-user PID was reused during container shutdown"
	container_podman rm "$container_name" >/dev/null
	container_name=

	{
		printf 'container_image=%s\n' "$container_image"
		printf 'container_user=%s\n' "$container_user"
		printf 'host_user=%s\n' "$resolved_user"
		printf 'foreign_pid=%s\n' "$container_stress_pid"
		printf 'foreign_executable=%s\n' "$resolved_executable"
		printf 'foreign_io_access=%s\n' available
		printf 'limited_cgroup=%s\n' "$container_limited_cgroup"
		printf 'limited_cpu_max=%s\n' "$container_cpu_max"
		printf 'start_time_before_stop=%s\n' "$container_start_time"
		printf 'start_time_after_stop=%s\n' "$container_start_time_after"
		printf 'final_cgroup=%s\n' "$container_final_cgroup"
		printf 'container_exit_code=%s\n' "$container_exit_code"
	} >"$artifact_dir/container-runtime.txt"
	pkill -TERM -u resman-cpu -x stress >/dev/null 2>&1 || true
	wait "$container_workload_pid" 2>/dev/null || true
	container_workload_pid=
	result=PASS
	detail="rootful Podman resolved a foreign host user and executable, applied a finite CPU quota, and released it on shutdown"
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
base_cgroup=$functional_cgroup_root/resman-functional-$run_id
[[ -d $base_cgroup ]] || fail "isolated cgroup root was not created"

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

if [[ $scenario == block-iops ]]; then
	io_uid=$(id -u resman-io)
	io_cgroup=$base_cgroup/user_$io_uid
	io_workdir=$state_dir/block-iops
	install -d -o resman-io -g resman-io -m 0700 "$io_workdir"
	dd if=/dev/zero of="$io_workdir/page-cache.bin" bs=1M count=256 status=none
	chown resman-io:resman-io "$io_workdir/page-cache.bin"
	cat "$io_workdir/page-cache.bin" >/dev/null

	runuser -u resman-io -- sleep 120s &
	io_anchor_pid=$!
	for _ in $(seq 1 20); do
		[[ -r $io_cgroup/io.stat && -r $io_cgroup/io.max ]] && break
		sleep 1
	done
	[[ -r $io_cgroup/io.stat && -r $io_cgroup/io.max ]] \
		|| fail "unlimited per-user block IOPS observation cgroup was not created"
	# Allow one complete decision interval after the observation baseline.
	sleep 6

	runuser -u resman-io -- dd if="$io_workdir/page-cache.bin" of=/dev/null bs=1 status=none &
	io_workload_pid=$!
	page_cache_pid=
	for _ in $(seq 1 30); do
		page_cache_pid=$(pgrep -u resman-io -x dd | tail -n 1 || true)
		[[ -n $page_cache_pid ]] && break
		sleep 0.1
	done
	[[ -n $page_cache_pid ]] || fail "page-cache syscall workload did not start"
	sleep 6
	cat "/proc/$page_cache_pid/io" >"$artifact_dir/page-cache-proc-io.txt" \
		|| fail "page-cache workload ended before its syscall counters were captured"
	page_cache_syscalls=$(awk '/^syscr:/ {print $2}' "$artifact_dir/page-cache-proc-io.txt")
	[[ ${page_cache_syscalls:-0} -gt 10000 ]] \
		|| fail "page-cache workload did not prove a high read syscall rate"
	if grep -Eq '(^|[[:space:]])(riops|wiops)=[0-9]+' "$io_cgroup/io.max"; then
		fail "page-cache syscall activity triggered block IOPS enforcement"
	fi
	kill -TERM "$io_workload_pid" 2>/dev/null || true
	wait "$io_workload_pid" 2>/dev/null || true
	io_workload_pid=

	read_io_stat() {
		local counter=$1
		awk -v counter="$counter" '
			{ for (i = 2; i <= NF; i++) { split($i, field, "="); if (field[1] == counter) total += field[2] } }
			END { print total + 0 }
		' "$io_cgroup/io.stat"
	}
	write_before=$(read_io_stat wios)
	runuser -u resman-io -- dd if=/dev/zero of="$io_workdir/direct.bin" \
		bs=4096 count=512 oflag=direct conv=fsync status=none
	write_after=$(read_io_stat wios)
	[[ $write_after -gt $write_before ]] || fail "direct write did not increment io.stat wios"
	write_limited=false
	for _ in $(seq 1 20); do
		if grep -Eq 'wiops=[0-9]+' "$io_cgroup/io.max" \
			&& grep -q 'write_iops' "$state_dir/resman.log"; then
			write_limited=true
			break
		fi
		sleep 1
	done
	[[ $write_limited == true ]] || fail "direct block writes did not trigger write IOPS enforcement"

	limits_released=false
	for _ in $(seq 1 40); do
		if ! grep -Eq '(^|[[:space:]])(riops|wiops)=[0-9]+' "$io_cgroup/io.max"; then
			limits_released=true
			break
		fi
		sleep 1
	done
	[[ $limits_released == true ]] || fail "write IOPS enforcement did not release before the read phase"

	read_before=$(read_io_stat rios)
	runuser -u resman-io -- dd if="$io_workdir/direct.bin" of=/dev/null \
		bs=4096 iflag=direct status=none
	read_after=$(read_io_stat rios)
	[[ $read_after -gt $read_before ]] || fail "direct read did not increment io.stat rios"
	read_limited=false
	for _ in $(seq 1 20); do
		if grep -Eq 'riops=[0-9]+' "$io_cgroup/io.max" \
			&& grep -q 'read_iops' "$state_dir/resman.log"; then
			read_limited=true
			break
		fi
		sleep 1
	done
	[[ $read_limited == true ]] || fail "direct block reads did not trigger read IOPS enforcement"

	{
		printf 'io_uid=%s\n' "$io_uid"
		printf 'observation_cgroup=%s\n' "$io_cgroup"
		printf 'page_cache_syscr=%s\n' "$page_cache_syscalls"
		printf 'write_ops_before=%s\n' "$write_before"
		printf 'write_ops_after=%s\n' "$write_after"
		printf 'read_ops_before=%s\n' "$read_before"
		printf 'read_ops_after=%s\n' "$read_after"
		printf 'io_max=%s\n' "$(tr '\n' ';' <"$io_cgroup/io.max")"
	} >"$artifact_dir/block-iops.txt"
	kill -TERM "$io_anchor_pid" 2>/dev/null || true
	wait "$io_anchor_pid" 2>/dev/null || true
	io_anchor_pid=
	result=PASS
	detail="page-cache syscalls stayed observational while direct read and write operations triggered true block IOPS enforcement"
	echo "PASS: $detail"
	exit 0
fi

if [[ $scenario == cpu-without-cpuset ]]; then
	/opt/resman-functional/workload.sh cpu 25s &
	cpu_workload_pid=$!
	cpu_uid=$(id -u resman-cpu)
	limited_cgroup=$base_cgroup/limited/best_effort/user_$cpu_uid
	cpu_quota_ready=false
	for _ in $(seq 1 30); do
		if [[ -r $base_cgroup/limited/cpu.max \
			&& -r $limited_cgroup/cgroup.procs \
			&& $(< "$base_cgroup/limited/cpu.max") != "max 100000" ]] \
			&& grep -q . "$limited_cgroup/cgroup.procs"; then
			cpu_quota_ready=true
			break
		fi
		sleep 1
	done
	[[ $cpu_quota_ready == true ]] \
		|| fail "CPU quota was not enforced in the delegated hierarchy without cpuset"
	grep -Fq 'Optional cpuset controller unavailable; CPU limiting remains enabled' "$state_dir/resman.log" \
		|| fail "the optional cpuset degradation diagnostic was not emitted"
	cpu_stress_pid=$(pgrep -u resman-cpu -x stress | tail -n 1)
	[[ -n $cpu_stress_pid ]] || fail "the limited CPU process exited before shutdown restoration"
	start_time_before_stop=$(process_start_time "$cpu_stress_pid")
	[[ -n $start_time_before_stop ]] || fail "cannot read the limited process start time before shutdown"
	shared_cpu_max_before_stop=$(< "$base_cgroup/limited/cpu.max")

	if ! systemctl stop "$service"; then
		fail "resman service stop failed while restoring a process from a delegated root"
	fi
	[[ -r /proc/$cpu_stress_pid/stat ]] || fail "the limited process exited during shutdown restoration"
	start_time_after_stop=$(process_start_time "$cpu_stress_pid")
	[[ $start_time_after_stop == "$start_time_before_stop" ]] \
		|| fail "the PID was reused during shutdown restoration"
	recovery_cgroup=$base_cgroup/recovery/user_$cpu_uid
	[[ -r $recovery_cgroup/cgroup.procs ]] \
		|| fail "the delegated-root process was not given a recovery leaf"
	grep -qx "$cpu_stress_pid" "$recovery_cgroup/cgroup.procs" \
		|| fail "the delegated-root process was not restored into the recovery leaf"
	if grep -qx "$cpu_stress_pid" "$limited_cgroup/cgroup.procs" 2>/dev/null; then
		fail "the process remained in the limited cgroup after shutdown"
	fi
	service_result=$(systemctl show "$service" --property=Result --value)
	service_exit_status=$(systemctl show "$service" --property=ExecMainStatus --value)
	[[ $service_result == success && $service_exit_status == 0 ]] \
		|| fail "incomplete shutdown was reported as service result=$service_result exit=$service_exit_status"
	final_cgroup=$(process_cgroup_path "$cpu_stress_pid")
	{
		printf 'delegated_root=%s\n' "$functional_cgroup_root"
		printf 'delegated_controllers=%s\n' "$cpu_only_controllers"
		printf 'shared_cpu_max_before_stop=%s\n' "$shared_cpu_max_before_stop"
		printf 'limited_user_cgroup=%s\n' "$limited_cgroup"
		printf 'restored_pid=%s\n' "$cpu_stress_pid"
		printf 'start_time_before_stop=%s\n' "$start_time_before_stop"
		printf 'start_time_after_stop=%s\n' "$start_time_after_stop"
		printf 'recovery_cgroup=%s\n' "$recovery_cgroup"
		printf 'final_cgroup=%s\n' "$final_cgroup"
		printf 'service_result=%s\n' "$service_result"
		printf 'service_exit_status=%s\n' "$service_exit_status"
	} >"$artifact_dir/cpu-without-cpuset.txt"
	pkill -TERM -u resman-cpu -x stress >/dev/null 2>&1 || true
	wait "$cpu_workload_pid" 2>/dev/null || true
	cpu_workload_pid=
	systemctl status "$service" --no-pager >"$artifact_dir/resman-status.txt" 2>&1 || true
	result=PASS
	detail="CPU quota was enforced without cpuset and the live process was restored safely at shutdown"
	echo "PASS: $detail"
	exit 0
fi

# Exercise the resource-only enforcement boundary. CPU eligibility is empty in
# the fixture, while the memory and I/O users are independently eligible.
if [[ $scenario == resource-only ]]; then
/opt/resman-functional/workload.sh cpu 30s &
cpu_workload_pid=$!
/opt/resman-functional/workload.sh memory 30s &
memory_workload_pid=$!
/opt/resman-functional/workload.sh io 30s &
io_workload_pid=$!
cpu_uid=$(id -u resman-cpu)
memory_uid=$(id -u resman-memory)
io_uid=$(id -u resman-io)
memory_cgroup=/sys/fs/cgroup/resman-functional-$run_id/user_$memory_uid
io_cgroup=/sys/fs/cgroup/resman-functional-$run_id/user_$io_uid

resource_limits_ready=false
for _ in $(seq 1 25); do
    if [[ -r $memory_cgroup/memory.max && -r $memory_cgroup/cpu.max \
        && -r $io_cgroup/io.max && -r $io_cgroup/cpu.max ]] \
        && [[ $(< "$memory_cgroup/memory.max") == 134217728 ]] \
        && [[ $(< "$memory_cgroup/cpu.max") == "max 100000" ]] \
        && [[ $(< "$io_cgroup/cpu.max") == "max 100000" ]] \
        && grep -q 'wbps=10485760' "$io_cgroup/io.max"; then
        resource_limits_ready=true
        break
    fi
    sleep 1
done

[[ $resource_limits_ready == true ]] \
    || fail "RAM-only and IO-only limits were not observed in standalone CPU-unlimited cgroups"
[[ ! -d /sys/fs/cgroup/resman-functional-$run_id/limited ]] \
    || fail "resource-only enforcement unexpectedly created the finite shared CPU cgroup"
[[ ! -d /sys/fs/cgroup/resman-functional-$run_id/user_$cpu_uid ]] \
    || fail "CPU-ineligible fixture user unexpectedly received a standalone cgroup"

{
    printf 'cpu_uid=%s\n' "$cpu_uid"
    printf 'memory_uid=%s\n' "$memory_uid"
    printf 'memory_cgroup=%s\n' "$memory_cgroup"
    printf 'memory_cpu_max=%s\n' "$(< "$memory_cgroup/cpu.max")"
    printf 'memory_max=%s\n' "$(< "$memory_cgroup/memory.max")"
    printf 'io_uid=%s\n' "$io_uid"
    printf 'io_cgroup=%s\n' "$io_cgroup"
    printf 'io_cpu_max=%s\n' "$(< "$io_cgroup/cpu.max")"
    printf 'io_max=%s\n' "$(tr '\n' ';' < "$io_cgroup/io.max")"
} >"$artifact_dir/resource-only-cgroups.txt"

wait "$cpu_workload_pid" "$memory_workload_pid" "$io_workload_pid"

systemctl status "$service" --no-pager >"$artifact_dir/resman-status.txt"
result=PASS
detail="RAM-only and IO-only users were enforced in standalone CPU-unlimited cgroups"
echo "PASS: $detail"
exit 0
fi

if [[ $scenario == memory-only ]]; then
	/opt/resman-functional/workload.sh cpu 30s &
	cpu_workload_pid=$!
	/opt/resman-functional/workload.sh memory 30s &
	memory_workload_pid=$!
	cpu_uid=$(id -u resman-cpu)
	memory_uid=$(id -u resman-memory)
	memory_cgroup=$base_cgroup/user_$memory_uid
	memory_limit_ready=false
	for _ in $(seq 1 25); do
		if [[ -r $memory_cgroup/memory.max && -r $memory_cgroup/cpu.max ]] \
			&& [[ $(< "$memory_cgroup/memory.max") == 134217728 ]] \
			&& [[ $(< "$memory_cgroup/cpu.max") == "max 100000" ]]; then
			memory_limit_ready=true
			break
		fi
		sleep 1
	done
	[[ $memory_limit_ready == true ]] \
		|| fail "the RAM-only limit was not observed in a standalone CPU-unlimited cgroup"
	[[ ! -d $base_cgroup/limited ]] \
		|| fail "memory-only enforcement unexpectedly created the finite shared CPU cgroup"
	[[ ! -d $base_cgroup/user_$cpu_uid ]] \
		|| fail "the CPU-ineligible fixture user unexpectedly received a standalone cgroup"
	{
		printf 'cpu_uid=%s\n' "$cpu_uid"
		printf 'memory_uid=%s\n' "$memory_uid"
		printf 'memory_cgroup=%s\n' "$memory_cgroup"
		printf 'memory_cpu_max=%s\n' "$(< "$memory_cgroup/cpu.max")"
		printf 'memory_max=%s\n' "$(< "$memory_cgroup/memory.max")"
	} >"$artifact_dir/memory-only-cgroup.txt"
	wait "$cpu_workload_pid" "$memory_workload_pid"
	cpu_workload_pid=
	memory_workload_pid=
	result=PASS
	detail="the RAM-only user was enforced in a standalone CPU-unlimited cgroup"
	echo "PASS: $detail"
	exit 0
fi

# Exercise sustained-active membership reconciliation without requiring memory
# or I/O controller interfaces. Both workloads begin in an explicit cgroup
# outside resman's subtree so origin restoration can be verified exactly.
cpu_uid=$(id -u resman-cpu)
origin_cgroup=/sys/fs/cgroup/resman-functional-origin-$run_id
limited_cgroup=$base_cgroup/limited/best_effort/user_$cpu_uid
mkdir "$origin_cgroup" || fail "cannot create process-membership origin cgroup"

stress_pid_except() {
	local excluded_pid=${1:-}
	local pid
	for pid in $(pgrep -u resman-cpu -x stress 2>/dev/null); do
		[[ $pid == "$excluded_pid" ]] || printf '%s\n' "$pid"
	done | tail -n 1
}

wait_for_stress_pid() {
	local excluded_pid=${1:-}
	local pid=
	for _ in $(seq 1 50); do
		pid=$(stress_pid_except "$excluded_pid")
		if [[ -n $pid ]]; then
			printf '%s\n' "$pid"
			return 0
		fi
		sleep 0.1
	done
	return 1
}

cgroup_has_pid() {
	local cgroup_path=$1
	local pid=$2
	[[ -r $cgroup_path/cgroup.procs ]] && grep -qx "$pid" "$cgroup_path/cgroup.procs"
}

/opt/resman-functional/workload.sh cpu 90s &
initial_launcher=$!
initial_pid=$(wait_for_stress_pid) || fail "initial CPU process did not start"
printf '%s\n' "$initial_pid" >"$origin_cgroup/cgroup.procs" \
	|| fail "cannot place initial CPU process in the origin cgroup"
initial_origin=$(process_cgroup_path "$initial_pid")

# Keep a differently named process above the release threshold while stress is
# excluded. This makes the reload exercise partial membership reconciliation,
# not the existing full-user release path.
runuser -u resman-cpu -- yes >/dev/null &
anchor_launcher=$!
anchor_pid=
for _ in $(seq 1 50); do
	anchor_pid=$(pgrep -u resman-cpu -x yes 2>/dev/null | tail -n 1 || true)
	[[ -n $anchor_pid ]] && break
	sleep 0.1
done
[[ -n $anchor_pid ]] || fail "non-excluded anchor process did not start"
printf '%s\n' "$anchor_pid" >"$origin_cgroup/cgroup.procs" \
	|| fail "cannot place anchor process in the origin cgroup"
anchor_origin=$(process_cgroup_path "$anchor_pid")

initial_limited=false
for _ in $(seq 1 30); do
	if cgroup_has_pid "$limited_cgroup" "$initial_pid"; then
		initial_limited=true
		break
	fi
	sleep 1
done
[[ $initial_limited == true ]] || fail "initial CPU process was not limited"

/opt/resman-functional/workload.sh cpu 90s &
new_launcher=$!
new_pid=$(wait_for_stress_pid "$initial_pid") || fail "post-activation CPU process did not start"
printf '%s\n' "$new_pid" >"$origin_cgroup/cgroup.procs" \
	|| fail "cannot place post-activation CPU process in the origin cgroup"
new_origin=$(process_cgroup_path "$new_pid")

new_limited=false
for _ in $(seq 1 20); do
	if cgroup_has_pid "$limited_cgroup" "$new_pid"; then
		new_limited=true
		break
	fi
	sleep 1
done
[[ $new_limited == true ]] || fail "post-activation process was not reconciled into the limited cgroup"

# Create a process directly inside the limited cgroup without letting resman
# capture an origin. It is stopped before exec so placement is deterministic.
blocked_pid_file=/run/resman-functional-blocked-$run_id.pid
install -o resman-cpu -g resman-cpu -m 0600 /dev/null "$blocked_pid_file"
# shellcheck disable=SC2016 # Expansion belongs to the nested resman-cpu shell.
runuser -u resman-cpu -- sh -c \
	'printf "%s\n" "$$" >"$1"; kill -STOP "$$"; exec stress --cpu 1 --timeout 90s' \
	sh "$blocked_pid_file" &
blocked_launcher=$!
blocked_pid=
for _ in $(seq 1 50); do
	blocked_pid=$(cat "$blocked_pid_file" 2>/dev/null || true)
	[[ -n $blocked_pid && -r /proc/$blocked_pid/stat ]] && break
	sleep 0.1
done
[[ -n $blocked_pid ]] || fail "originless process did not publish its PID"
rm -f "$blocked_pid_file"
printf '%s\n' "$blocked_pid" >"$limited_cgroup/cgroup.procs" \
	|| fail "cannot place originless process directly in the limited cgroup"
kill -CONT "$blocked_pid"
for _ in $(seq 1 50); do
	[[ $(cat "/proc/$blocked_pid/comm" 2>/dev/null || true) == stress ]] && break
	sleep 0.1
done
[[ $(cat "/proc/$blocked_pid/comm" 2>/dev/null || true) == stress ]] \
	|| fail "originless process did not become the excluded stress workload"
blocked_start_time=$(process_start_time "$blocked_pid")

sed -i 's/^PROCESS_EXCLUDE_LIST=.*/PROCESS_EXCLUDE_LIST=^stress$/' "$config_file"
chmod 0600 "$config_file"
excluded_restored=false
for _ in $(seq 1 20); do
	if ! cgroup_has_pid "$limited_cgroup" "$initial_pid" \
		&& ! cgroup_has_pid "$limited_cgroup" "$new_pid" \
		&& cgroup_has_pid "$limited_cgroup" "$blocked_pid" \
		&& cgroup_has_pid "$limited_cgroup" "$anchor_pid" \
		&& [[ $(process_cgroup_path "$initial_pid") == "$initial_origin" ]] \
		&& [[ $(process_cgroup_path "$new_pid") == "$new_origin" ]]; then
		excluded_restored=true
		break
	fi
	sleep 1
done
[[ $excluded_restored == true ]] \
	|| fail "newly excluded processes were not restored to their captured origins"
blocked_start_time_after_error=$(process_start_time "$blocked_pid")
[[ $blocked_start_time_after_error == "$blocked_start_time" ]] \
	|| fail "originless PID was reused while reconciliation remained fail-closed"

origin_unavailable_signal=false
degraded_cycle_completed=false
for _ in $(seq 1 20); do
	curl --fail --silent --show-error --max-time 2 \
		http://127.0.0.1:19100/metrics >"$artifact_dir/process-membership-metrics.txt" \
		|| fail "cannot scrape process-membership error signal"
	if awk '/^resman_errors_total\{/ && /component="process_membership"/ && /error_type="origin_unavailable"/ && $NF > 0 { found=1 } END { exit !found }' \
		"$artifact_dir/process-membership-metrics.txt"; then
		origin_unavailable_signal=true
	fi
	if grep -q 'Control cycle completed.*outcome=degraded' "$state_dir/resman.log"; then
		degraded_cycle_completed=true
	fi
	[[ $origin_unavailable_signal == true && $degraded_cycle_completed == true ]] && break
	sleep 1
done
[[ $origin_unavailable_signal == true ]] \
	|| fail "persistent origin-unavailable reconciliation was not exported distinctly"
[[ $degraded_cycle_completed == true ]] \
	|| fail "control cycle did not complete after the persistent enforcement error"

sed -i 's/^PROCESS_EXCLUDE_LIST=.*/PROCESS_EXCLUDE_LIST=^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$/' "$config_file"
chmod 0600 "$config_file"
reincluded=false
for _ in $(seq 1 20); do
	if cgroup_has_pid "$limited_cgroup" "$initial_pid" \
		&& cgroup_has_pid "$limited_cgroup" "$new_pid" \
		&& cgroup_has_pid "$limited_cgroup" "$blocked_pid"; then
		reincluded=true
		break
	fi
	sleep 1
done
[[ $reincluded == true ]] || fail "re-included processes were not returned to the limited cgroup"
systemctl is-active --quiet "$service" || fail "resman stopped during membership reconciliation"

{
	printf 'cpu_uid=%s\n' "$cpu_uid"
	printf 'limited_cgroup=%s\n' "$limited_cgroup"
	printf 'initial_pid=%s\n' "$initial_pid"
	printf 'initial_origin=%s\n' "$initial_origin"
	printf 'anchor_pid=%s\n' "$anchor_pid"
	printf 'anchor_origin=%s\n' "$anchor_origin"
	printf 'anchor_remained_limited=%s\n' "$excluded_restored"
	printf 'new_pid=%s\n' "$new_pid"
	printf 'new_origin=%s\n' "$new_origin"
	printf 'post_activation_reconciled=%s\n' "$new_limited"
	printf 'excluded_restored=%s\n' "$excluded_restored"
	printf 'blocked_pid=%s\n' "$blocked_pid"
	printf 'blocked_start_time=%s\n' "$blocked_start_time"
	printf 'blocked_start_time_after_error=%s\n' "$blocked_start_time_after_error"
	printf 'blocked_remained_constrained=true\n'
	printf 'origin_unavailable_signal=%s\n' "$origin_unavailable_signal"
	printf 'degraded_cycle_completed=%s\n' "$degraded_cycle_completed"
	printf 'reincluded=%s\n' "$reincluded"
} >"$artifact_dir/process-membership.txt"

pkill -TERM -u resman-cpu -x stress >/dev/null 2>&1 || true
pkill -TERM -u resman-cpu -x yes >/dev/null 2>&1 || true
membership_launchers=("$initial_launcher" "$new_launcher" "$anchor_launcher" "$blocked_launcher")
kill -TERM "${membership_launchers[@]}" >/dev/null 2>&1 || true
for _ in $(seq 1 50); do
	launchers_alive=false
	for launcher in "${membership_launchers[@]}"; do
		if kill -0 "$launcher" >/dev/null 2>&1; then
			launchers_alive=true
			break
		fi
	done
	[[ $launchers_alive == false ]] && break
	sleep 0.1
done
launcher_cleanup_forced=false
for launcher in "${membership_launchers[@]}"; do
	if kill -0 "$launcher" >/dev/null 2>&1; then
		launcher_cleanup_forced=true
		kill -KILL "$launcher" >/dev/null 2>&1 || true
	fi
	wait "$launcher" >/dev/null 2>&1 || true
done
printf 'launcher_cleanup_forced=%s\n' "$launcher_cleanup_forced" \
	>>"$artifact_dir/process-membership.txt"
systemctl status "$service" --no-pager >"$artifact_dir/resman-status.txt"
result=PASS
detail="membership reconciliation continued past an unavailable origin and restored valid peers"
echo "PASS: $detail"
