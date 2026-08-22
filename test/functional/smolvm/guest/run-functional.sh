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

case "$run_id" in
    *[!a-z0-9-]*|'')
        echo "invalid run id: $run_id" >&2
        exit 2
        ;;
esac
case "$scenario" in
	resource-only|process-membership) ;;
	*)
		echo "invalid scenario: $scenario" >&2
		exit 2
		;;
esac

mkdir -p "$artifact_dir" "$runtime_dir" "$state_dir"
chmod 0700 "$runtime_dir" "$state_dir"
exec > >(tee -a "$artifact_dir/guest.log") 2>&1

result=FAIL
detail="guest harness did not complete"

finish() {
    local status=$?
    set +e
    cp "$state_dir/resman.log" "$artifact_dir/resman.log" 2>/dev/null
    ps -eo pid,ppid,uid,user,comm,args >"$artifact_dir/processes.txt" 2>&1
    find "/sys/fs/cgroup/resman-functional-$run_id" -maxdepth 3 -type d -print \
        >"$artifact_dir/cgroup-tree.txt" 2>&1
    curl --fail --silent --show-error --max-time 2 \
        http://127.0.0.1:19100/metrics >"$artifact_dir/prometheus-metrics-final.txt" 2>&1
    systemctl stop "$service" >/dev/null 2>&1
    journalctl -u "$service" --no-pager >"$artifact_dir/resman-journal.log" 2>&1
    printf '%s\n' "$result" >"$result_file"
    printf 'result=%s\ndetail=%s\nexit_code=%d\n' "$result" "$detail" "$status" \
        >>"$artifact_dir/environment.txt"
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
if [[ $scenario == resource-only ]]; then
	required_controllers+=(memory io)
fi
for controller in "${required_controllers[@]}"; do
    [[ " $controllers " == *" $controller "* ]] || blocked "required $controller controller is unavailable"
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

probe=/sys/fs/cgroup/resman-functional-probe-$run_id
mkdir "$probe" || blocked "cannot create a guest cgroup"
rmdir "$probe" || fail "cannot remove the guest cgroup preflight probe"

sed "s/@RUN_ID@/$run_id/g" /opt/resman-functional/fixtures/resman.conf >"$config_file"
if [[ $scenario == process-membership ]]; then
	sed -i \
		-e 's/^CPU_THRESHOLD=.*/CPU_THRESHOLD=10/' \
		-e 's/^USER_INCLUDE_LIST=.*/USER_INCLUDE_LIST=^resman-cpu$/' \
		-e 's/^MIN_SYSTEM_CORES=.*/MIN_SYSTEM_CORES=1/' \
		-e 's/^RAM_LIMIT_ENABLED=.*/RAM_LIMIT_ENABLED=false/' \
		-e 's/^IO_LIMIT_ENABLED=.*/IO_LIMIT_ENABLED=false/' \
		"$config_file"
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
    printf 'cgroup_path=%s\n' "/sys/fs/cgroup/resman-functional-$run_id"
    printf 'prometheus_endpoint=%s\n' 'http://127.0.0.1:19100/metrics'
    printf 'mcp_endpoint=%s\n' 'http://127.0.0.1:19101/mcp'
} >>"$artifact_dir/environment.txt"

systemctl daemon-reload
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
base_cgroup=/sys/fs/cgroup/resman-functional-$run_id
[[ -d $base_cgroup ]] || fail "isolated cgroup root was not created"

# A controller name alone does not prove that the kernel exposes the files
# needed by the configured policy. Probe a real child below resman's base after
# the manager has enabled its subtree controllers.
controller_probe=$base_cgroup/controller-probe
mkdir "$controller_probe" || blocked "cannot create the controller-interface probe cgroup"
cpu_max_available=false
memory_max_available=false
io_max_available=false
[[ -e $controller_probe/cpu.max ]] && cpu_max_available=true
[[ -e $controller_probe/memory.max ]] && memory_max_available=true
[[ -e $controller_probe/io.max ]] && io_max_available=true
{
    printf 'cpu_max_available=%s\n' "$cpu_max_available"
    printf 'memory_max_available=%s\n' "$memory_max_available"
    printf 'io_max_available=%s\n' "$io_max_available"
} >>"$artifact_dir/environment.txt"
rmdir "$controller_probe" || fail "cannot remove the controller-interface probe cgroup"
[[ $cpu_max_available == true ]] || blocked "cpu controller is listed but cpu.max is unavailable"
if [[ $scenario == resource-only ]]; then
	[[ $memory_max_available == true ]] || blocked "memory controller is listed but memory.max is unavailable"
	[[ $io_max_available == true ]] || blocked "io controller is listed but io.max is unavailable"
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

# Exercise sustained-active membership reconciliation without requiring memory
# or I/O controller interfaces. Both workloads begin in an explicit cgroup
# outside resman's subtree so origin restoration can be verified exactly.
cpu_uid=$(id -u resman-cpu)
origin_cgroup=/sys/fs/cgroup/resman-functional-origin-$run_id
limited_cgroup=$base_cgroup/limited/user_$cpu_uid
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

process_cgroup_path() {
	local pid=$1
	awk -F '::' '$1 == "0" { print $2 }' "/proc/$pid/cgroup"
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

sed -i 's/^PROCESS_EXCLUDE_LIST=.*/PROCESS_EXCLUDE_LIST=^stress$/' "$config_file"
chmod 0600 "$config_file"
excluded_restored=false
for _ in $(seq 1 20); do
	if ! cgroup_has_pid "$limited_cgroup" "$initial_pid" \
		&& ! cgroup_has_pid "$limited_cgroup" "$new_pid" \
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

sed -i 's/^PROCESS_EXCLUDE_LIST=.*/PROCESS_EXCLUDE_LIST=^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$/' "$config_file"
chmod 0600 "$config_file"
reincluded=false
for _ in $(seq 1 20); do
	if cgroup_has_pid "$limited_cgroup" "$initial_pid" \
		&& cgroup_has_pid "$limited_cgroup" "$new_pid"; then
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
	printf 'reincluded=%s\n' "$reincluded"
} >"$artifact_dir/process-membership.txt"

pkill -TERM -u resman-cpu -x stress >/dev/null 2>&1 || true
pkill -TERM -u resman-cpu -x yes >/dev/null 2>&1 || true
kill "$initial_launcher" "$new_launcher" "$anchor_launcher" >/dev/null 2>&1 || true
wait "$initial_launcher" "$new_launcher" "$anchor_launcher" >/dev/null 2>&1 || true
systemctl status "$service" --no-pager >"$artifact_dir/resman-status.txt"
result=PASS
detail="active process membership reconciled new, excluded, and re-included processes"
echo "PASS: $detail"
