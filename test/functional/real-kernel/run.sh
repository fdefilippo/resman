#!/usr/bin/env bash
set -Eeuo pipefail

scenario=${1:?scenario is required}
run_id=${2:?run id is required}
source_revision=${3:?source revision is required}
bundle_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$bundle_dir/evidence
binary=$bundle_dir/resman
test_user=${RESMAN_REAL_KERNEL_USER:-pippo}
test_uid=
test_gid=
resman_pid=
workload_pid=
workload_process_group=0
anchor_pid=
active_cgroup_base=
active_log_file=
active_work_root=
selected_io_device=
result=FAIL
detail="real-kernel scenario did not complete"

case "$run_id" in
	*[!a-z0-9-]*|'') echo "invalid run id: $run_id" >&2; exit 2 ;;
esac
case "$scenario" in
	psi-refresh-neutrality|block-io-all-dimensions) ;;
	*) echo "invalid scenario: $scenario" >&2; exit 2 ;;
esac

mkdir -p "$evidence_dir"
chmod 0700 "$evidence_dir"
exec > >(tee -a "$evidence_dir/runner.log") 2>&1

stop_children() {
	if [[ -n $workload_pid ]]; then
		if [[ $workload_process_group -eq 1 ]]; then
			kill -CONT -- "-$workload_pid" 2>/dev/null || true
			kill -TERM -- "-$workload_pid" 2>/dev/null || true
		else
			kill -CONT "$workload_pid" 2>/dev/null || true
			kill -TERM "$workload_pid" 2>/dev/null || true
		fi
		wait "$workload_pid" 2>/dev/null || true
		workload_pid=
		workload_process_group=0
	fi
	if [[ -n $anchor_pid ]]; then
		kill -TERM "$anchor_pid" 2>/dev/null || true
		wait "$anchor_pid" 2>/dev/null || true
		anchor_pid=
	fi
	if [[ -n $resman_pid ]]; then
		kill -TERM "$resman_pid" 2>/dev/null || true
		wait "$resman_pid" 2>/dev/null || true
		resman_pid=
	fi
}

finish() {
	local status=$?
	local cleanup_status=PASS
	trap - EXIT INT TERM
	set +e
	stop_children
	if [[ -n $active_log_file && -f $active_log_file ]]; then
		grep ' \[ERROR\] ' "$active_log_file" >>"$evidence_dir/daemon-errors.txt" || true
	fi
	if [[ -n $active_cgroup_base && -e $active_cgroup_base ]]; then
		cleanup_status=FAIL
		find "$active_cgroup_base" -maxdepth 4 -type f -name cgroup.procs \
			-exec sh -c 'printf "%s:" "$1"; tr "\n" "," <"$1"; echo' _ {} \; \
			>"$evidence_dir/remaining-cgroup-processes.txt" 2>&1 || true
	fi
	if [[ -n $active_work_root ]]; then
		case "$active_work_root" in
			/tmp/resman-final-work-"$run_id") rm -rf -- "$active_work_root" ;;
			*) cleanup_status=FAIL ;;
		esac
	fi
	if [[ -s $evidence_dir/daemon-errors.txt ]]; then
		status=1
		detail="resman emitted unexpected error-level diagnostics"
	fi
	if [[ $cleanup_status != PASS ]]; then
		status=1
		detail="dedicated cgroup cleanup failed"
	fi
	if [[ $result == PASS && $status -ne 0 ]]; then
		result=FAIL
	fi
	if [[ $result == FAIL && $status -eq 0 ]]; then
		status=1
	fi
	printf '%s\n' "$result" >"$evidence_dir/result"
	printf 'cleanup=%s\nresult=%s\ndetail=%s\nexit_code=%d\n' \
		"$cleanup_status" "$result" "$detail" "$status" >>"$evidence_dir/environment.txt"
	exit "$status"
}
trap finish EXIT
# A dying control session delivers SIGHUP; without trapping it the exit trap
# never runs and the host keeps the scenario's modifications.
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
	detail=$1
	echo "FAIL: $detail" >&2
	exit 1
}

metric_value() {
	local metric=$1
	local metrics_file=$2
	awk -v metric="$metric" '
		index($1, metric) == 1 && (length($1) == length(metric) || substr($1, length(metric) + 1, 1) == "{") {
			print $2
			exit
		}
	' "$metrics_file"
}

user_metric_value() {
	local metric=$1
	local username=$2
	local metrics_file=$3
	awk -v metric="$metric" -v username="$username" '
		index($1, metric "{") == 1 && index($1, "username=\"" username "\"") > 0 {
			print $2
			exit
		}
	' "$metrics_file"
}

write_common_config() {
	local config_file=$1
	local cgroup_base=$2
	local log_file=$3
	local port=$4
	cat >"$config_file" <<EOF
CGROUP_ROOT=/sys/fs/cgroup
CGROUP_BASE=$(basename "$cgroup_base")
CREATED_CGROUPS_FILE=${config_file%.conf}.cgroups.txt
LOG_FILE=$log_file
LOG_LEVEL=DEBUG
USE_SYSLOG=false
POLLING_INTERVAL=5
MIN_ACTIVE_TIME=1
METRICS_CACHE_TTL=1
METRICS_REFRESH_INTERVAL=5
PROCESS_MIN_AGE_SECONDS=0
MIN_SYSTEM_CORES=1
IGNORE_SYSTEM_LOAD=true
CPU_THRESHOLD=100
CPU_RELEASE_THRESHOLD=99
CPU_THRESHOLD_DURATION=0
CPU_QUOTA_NORMAL="max 100000"
USER_INCLUDE_LIST=
USER_EXCLUDE_LIST=root
PROCESS_EXCLUDE_LIST=^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$
RAM_LIMIT_ENABLED=false
RAM_USER_INCLUDE_LIST=
RAM_USER_EXCLUDE_LIST=root
IO_LIMIT_ENABLED=false
IO_THRESHOLD=2
IO_RELEASE_THRESHOLD=1
IO_THRESHOLD_DURATION=0
IO_READ_BPS=max
IO_WRITE_BPS=max
IO_READ_IOPS=0
IO_WRITE_IOPS=0
IO_DEVICE_FILTER=all
IO_USER_INCLUDE_LIST=^$test_user$
IO_USER_EXCLUDE_LIST=root
ENABLE_PROMETHEUS=true
PROMETHEUS_METRICS_BIND_HOST=127.0.0.1
PROMETHEUS_METRICS_BIND_PORT=$port
PROMETHEUS_AUTH_TYPE=none
PROMETHEUS_TLS_ENABLED=false
MCP_ENABLED=false
METRICS_DB_ENABLED=false
PSI_EVENT_DRIVEN=false
EOF
	chmod 0600 "$config_file"
}

start_resman() {
	local config_file=$1
	local log_file=$2
	active_log_file=$log_file
	"$binary" --config "$config_file" >"${log_file%.log}.stdout" \
		2>"${log_file%.log}.stderr" &
	resman_pid=$!
	for _ in $(seq 1 30); do
		kill -0 "$resman_pid" 2>/dev/null || fail "resman exited during startup"
		grep -q 'Entering main control loop' "$log_file" 2>/dev/null && return 0
		sleep 1
	done
	fail "resman did not enter its control loop"
}

preflight() {
	local os_name
	[[ $(id -u) -eq 0 ]] || fail "real-kernel scenarios must run as root"
	[[ -x $binary ]] || fail "the staged resman binary is missing or not executable"
	[[ $(stat -fc %T /sys/fs/cgroup) == cgroup2fs ]] || fail "cgroup v2 is not mounted"
	[[ -w /sys/fs/cgroup/cgroup.subtree_control ]] || fail "the cgroup root is not writable"
	command -v curl >/dev/null || fail "curl is required"
	command -v python3 >/dev/null || fail "python3 is required"
	command -v setpriv >/dev/null || fail "setpriv is required"
	id "$test_user" >/dev/null 2>&1 || fail "fixture user $test_user is unavailable"
	if pgrep -x resman >/dev/null 2>&1; then
		fail "another resman process is active; refusing to mutate a shared host"
	fi
	test_uid=$(id -u "$test_user")
	test_gid=$(id -g "$test_user")
	os_name=$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"')
	{
		printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		printf 'scenario=%s\n' "$scenario"
		printf 'source_revision=%s\n' "$source_revision"
		printf 'host=%s\n' "$(hostname -f 2>/dev/null || hostname)"
		printf 'os=%s\n' "$os_name"
		printf 'kernel=%s\n' "$(uname -r)"
		printf 'cgroup_mount=%s\n' "$(findmnt -n -o SOURCE,FSTYPE,OPTIONS /sys/fs/cgroup)"
		printf 'controllers=%s\n' "$(< /sys/fs/cgroup/cgroup.controllers)"
		printf 'test_user=%s\n' "$test_user"
		printf 'test_uid=%s\n' "$test_uid"
	} >"$evidence_dir/environment.txt"
}

run_psi_refresh_neutrality() {
	local cgroup_base=/sys/fs/cgroup/resman-final-psi-$run_id
	local config_file=$bundle_dir/psi.conf
	local log_file=$evidence_dir/psi-resman.log
	local port=$((20000 + $$ % 10000))
	local endpoint=http://127.0.0.1:$port/metrics
	local baseline=$evidence_dir/psi-baseline.prom
	local after=$evidence_dir/psi-after-refresh.prom
	local cycles_before cycles_after collection_before collection_after
	local user_cpu_before user_cpu_after user_ema_before user_ema_after
	local system_before system_after refresh_ready=false

	for pressure in cpu memory io; do
		[[ -r /proc/pressure/$pressure ]] || fail "PSI pressure file is unavailable: $pressure"
	done
	active_cgroup_base=$cgroup_base
	[[ ! -e $cgroup_base ]] || fail "dedicated PSI cgroup already exists"
	write_common_config "$config_file" "$cgroup_base" "$log_file" "$port"
	sed -i \
		-e 's/^POLLING_INTERVAL=.*/POLLING_INTERVAL=120/' \
		-e 's/^METRICS_REFRESH_INTERVAL=.*/METRICS_REFRESH_INTERVAL=5/' \
		-e 's/^PSI_EVENT_DRIVEN=.*/PSI_EVENT_DRIVEN=true/' \
		"$config_file"
	cat >>"$config_file" <<'EOF'
PSI_CPU_STALL_THRESHOLD=999999
PSI_IO_STALL_THRESHOLD=999999
PSI_FALLBACK_INTERVAL=30
EOF
	setpriv --reuid="$test_uid" --regid="$test_gid" --init-groups \
		sha256sum /dev/zero >/dev/null 2>"$evidence_dir/psi-workload.stderr" &
	workload_pid=$!
	workload_process_group=0
	start_resman "$config_file" "$log_file"
	for _ in $(seq 1 45); do
		curl --fail --silent --show-error --max-time 2 "$endpoint" >"$baseline" || true
		cycles_before=$(metric_value resman_control_cycles_total "$baseline")
		user_cpu_before=$(user_metric_value resman_user_cpu_usage_percent "$test_user" "$baseline")
		if [[ ${cycles_before:-0} -ge 2 && -n ${user_cpu_before:-} ]]; then
			break
		fi
		sleep 1
	done
	[[ ${cycles_before:-0} -ge 2 && -n ${user_cpu_before:-} ]] \
		|| fail "PSI fallback did not establish a user decision sample"
	user_ema_before=$(user_metric_value resman_user_cpu_usage_ema_percent "$test_user" "$baseline")
	collection_before=$(metric_value resman_metrics_collection_duration_seconds_count "$baseline")
	system_before=$(metric_value resman_cpu_total_usage_percent "$baseline")
	[[ -n ${user_ema_before:-} && -n ${collection_before:-} && -n ${system_before:-} ]] \
		|| fail "PSI baseline metrics are incomplete"

	kill -TERM "$workload_pid" 2>/dev/null || true
	wait "$workload_pid" 2>/dev/null || true
	workload_pid=
	for _ in $(seq 1 15); do
		sleep 1
		curl --fail --silent --show-error --max-time 2 "$endpoint" >"$after" || true
		cycles_after=$(metric_value resman_control_cycles_total "$after")
		collection_after=$(metric_value resman_metrics_collection_duration_seconds_count "$after")
		if [[ $cycles_after == "$cycles_before" \
			&& ${collection_after:-0} -ge $((collection_before + 2)) ]]; then
			refresh_ready=true
			break
		fi
	done
	[[ $refresh_ready == true ]] \
		|| fail "two observation refreshes did not finish before another control cycle"
	user_cpu_after=$(user_metric_value resman_user_cpu_usage_percent "$test_user" "$after")
	user_ema_after=$(user_metric_value resman_user_cpu_usage_ema_percent "$test_user" "$after")
	system_after=$(metric_value resman_cpu_total_usage_percent "$after")
	[[ $user_cpu_after == "$user_cpu_before" ]] || fail "observation changed decision-owned user CPU"
	[[ $user_ema_after == "$user_ema_before" ]] || fail "observation changed decision-owned user EMA"
	[[ $system_after != "$system_before" ]] || fail "the observation snapshot did not change"
	grep -Fq 'PSI event-driven mode enabled' "$log_file" || fail "PSI event-driven mode did not activate"
	if grep -Fq 'PSI event received' "$log_file"; then
		fail "a PSI event invalidated the refresh-only assertion window"
	fi
	{
		printf 'psi_available=true\n'
		printf 'psi_event_driven_active=true\n'
		printf 'control_cycles_before=%s\n' "$cycles_before"
		printf 'control_cycles_after=%s\n' "$cycles_after"
		printf 'collections_before=%s\n' "$collection_before"
		printf 'collections_after=%s\n' "$collection_after"
		printf 'decision_cpu_before=%s\n' "$user_cpu_before"
		printf 'decision_cpu_after=%s\n' "$user_cpu_after"
		printf 'decision_ema_before=%s\n' "$user_ema_before"
		printf 'decision_ema_after=%s\n' "$user_ema_after"
		printf 'system_observation_before=%s\n' "$system_before"
		printf 'system_observation_after=%s\n' "$system_after"
	} >"$evidence_dir/psi-refresh-neutrality.txt"
	stop_children
	[[ ! -e $cgroup_base ]] || fail "PSI scenario left its dedicated cgroup behind"
	active_cgroup_base=
	result=PASS
	detail="PSI refreshes advanced observation without changing decision CPU or EMA"
}

read_io_stat() {
	local cgroup_path=$1
	local counter=$2
	awk -v counter="$counter" '
		{ for (i = 2; i <= NF; i++) { split($i, field, "="); if (field[1] == counter) total += field[2] } }
		END { print total + 0 }
	' "$cgroup_path/io.stat"
}

direct_stopped_pid=
start_direct_workload() {
	local mode=$1
	local work_dir=$2
	local pid_file=$work_dir/$mode.pid
	rm -f "$pid_file"
	case "$mode" in
		read_bps|read_iops)
			setsid setpriv --reuid="$test_uid" --regid="$test_gid" --init-groups \
				python3 "$active_work_root/workload.py" direct-read "$pid_file" \
				"$work_dir/direct.bin" &
			;;
		write_bps|write_iops)
			setsid setpriv --reuid="$test_uid" --regid="$test_gid" --init-groups \
				python3 "$active_work_root/workload.py" direct-write "$pid_file" \
				"$work_dir/direct.out" &
			;;
	esac
	workload_pid=$!
	workload_process_group=1
	for _ in $(seq 1 100); do
		[[ -s $pid_file ]] && break
		sleep 0.1
	done
	[[ -s $pid_file ]] || fail "$mode workload did not publish its PID"
	direct_stopped_pid=$(<"$pid_file")
	for _ in $(seq 1 100); do
		grep -q '^State:.*T' "/proc/$direct_stopped_pid/status" 2>/dev/null && break
		sleep 0.1
	done
	grep -q '^State:.*T' "/proc/$direct_stopped_pid/status" \
		|| fail "$mode workload did not stop before placement"
}

run_io_phase() {
	local dimension=$1
	local phase_dir=$evidence_dir/$dimension
	local cgroup_base=/sys/fs/cgroup/resman-final-$dimension-$run_id
	local config_file=$bundle_dir/$dimension.conf
	local log_file=$phase_dir/resman.log
	local port=$((21000 + ($$ + ${#dimension}) % 9000))
	local io_cgroup=$cgroup_base/user_$test_uid
	local work_dir=$active_work_root/$dimension
	local controller_field decision_name before_counter after_counter
	local before after stopped_pid limited=false

	mkdir -p "$phase_dir"
	chmod 0700 "$phase_dir"
	install -d -m 0700 -o "$test_uid" -g "$test_gid" "$work_dir"
	dd if=/dev/zero of="$work_dir/direct.bin" bs=1M count=64 status=none
	chown "$test_uid:$test_gid" "$work_dir/direct.bin"
	active_cgroup_base=$cgroup_base
	[[ ! -e $cgroup_base ]] || fail "dedicated $dimension cgroup already exists"
	write_common_config "$config_file" "$cgroup_base" "$log_file" "$port"
	sed -i \
		-e 's/^IO_LIMIT_ENABLED=.*/IO_LIMIT_ENABLED=true/' \
		-e "s/^IO_DEVICE_FILTER=.*/IO_DEVICE_FILTER=$selected_io_device/" \
		"$config_file"
	case "$dimension" in
		read_bps)
			sed -i 's/^IO_READ_BPS=.*/IO_READ_BPS=1M/' "$config_file"
			controller_field=rbps decision_name=read_bps before_counter=rbytes after_counter=rbytes ;;
		write_bps)
			sed -i 's/^IO_WRITE_BPS=.*/IO_WRITE_BPS=1M/' "$config_file"
			controller_field=wbps decision_name=write_bps before_counter=wbytes after_counter=wbytes ;;
		read_iops)
			sed -i 's/^IO_READ_IOPS=.*/IO_READ_IOPS=100/' "$config_file"
			controller_field=riops decision_name=read_iops before_counter=rios after_counter=rios ;;
		write_iops)
			sed -i 's/^IO_WRITE_IOPS=.*/IO_WRITE_IOPS=100/' "$config_file"
			controller_field=wiops decision_name=write_iops before_counter=wios after_counter=wios ;;
	esac
	start_resman "$config_file" "$log_file"
	setpriv --reuid="$test_uid" --regid="$test_gid" --init-groups sleep 180 &
	anchor_pid=$!
	if [[ $dimension == read_iops || $dimension == write_iops ]]; then
		for _ in $(seq 1 30); do
			if [[ -r $io_cgroup/io.stat && -r $io_cgroup/io.max ]] \
				&& grep -qx "$anchor_pid" "$io_cgroup/cgroup.procs"; then
				break
			fi
			sleep 1
		done
		[[ -r $io_cgroup/io.stat && -r $io_cgroup/io.max ]] \
			|| fail "$dimension observation cgroup was not created"
	fi
	sleep 6

	if [[ $dimension == write_iops ]]; then
		setpriv --reuid="$test_uid" --regid="$test_gid" --init-groups \
			python3 "$active_work_root/workload.py" page-cache "$work_dir/page.pid" "$work_dir/direct.bin" \
			>"$phase_dir/page-cache.stdout" 2>"$phase_dir/page-cache.stderr" &
		workload_pid=$!
		workload_process_group=0
		wait "$workload_pid"
		workload_pid=
		workload_process_group=0
		setpriv --reuid="$test_uid" --regid="$test_gid" --init-groups \
			python3 "$active_work_root/workload.py" socket "$work_dir/socket.pid" "$work_dir/direct.bin" \
			>"$phase_dir/socket.stdout" 2>"$phase_dir/socket.stderr" &
		workload_pid=$!
		workload_process_group=0
		wait "$workload_pid"
		workload_pid=
		workload_process_group=0
		sleep 3
		if grep -Eq 'decision=ACTIVATE_LIMITS reason=.*(read_iops|write_iops)' "$log_file"; then
			fail "page-cache or socket syscalls activated block IOPS enforcement"
		fi
		grep -Eq '^syscr:[[:space:]]+[1-9][0-9]{4,}' "$work_dir/page.pid.io" \
			|| fail "page-cache workload did not prove high syscall activity"
		grep -Eq '^syscw:[[:space:]]+[1-9][0-9]{4,}' "$work_dir/socket.pid.io" \
			|| fail "socket workload did not prove high syscall activity"
		printf 'cached_and_socket_false_activation=PASS\n' >>"$evidence_dir/block-io-summary.txt"
	fi

	before=0
	if [[ $dimension == read_iops || $dimension == write_iops ]]; then
		before=$(read_io_stat "$io_cgroup" "$before_counter")
	fi
	start_direct_workload "$dimension" "$work_dir"
	stopped_pid=$direct_stopped_pid
	if [[ $dimension == read_iops || $dimension == write_iops ]]; then
		printf '%s\n' "$stopped_pid" >"$io_cgroup/cgroup.procs"
	fi
	kill -CONT "$stopped_pid"
	for _ in $(seq 1 30); do
		if [[ -r $io_cgroup/io.max ]] \
			&& grep -Eq "${controller_field}=[0-9]+" "$io_cgroup/io.max" \
			&& grep -Eq "decision=ACTIVATE_LIMITS reason=.*${decision_name}" "$log_file"; then
			limited=true
			break
		fi
		sleep 1
	done
	[[ $limited == true ]] || fail "$dimension did not activate its controller limit"
	for _ in $(seq 1 10); do
		after=$(read_io_stat "$io_cgroup" "$after_counter")
		[[ $after -gt $before ]] && break
		sleep 1
	done
	[[ $after -gt $before ]] || fail "$dimension did not advance its io.stat counter"
	cp "$io_cgroup/io.max" "$phase_dir/io.max.active"
	cp "$io_cgroup/io.stat" "$phase_dir/io.stat.active"
	printf '%s_before=%s\n%s_after=%s\n%s=PASS\n' \
		"$after_counter" "$before" "$after_counter" "$after" "$dimension" \
		>>"$evidence_dir/block-io-summary.txt"
	stop_children
	grep ' \[ERROR\] ' "$log_file" >>"$evidence_dir/daemon-errors.txt" || true
	[[ ! -s $evidence_dir/daemon-errors.txt ]] \
		|| fail "$dimension emitted an unexpected error-level diagnostic"
	[[ ! -e $cgroup_base ]] || fail "$dimension left its dedicated cgroup behind"
	active_cgroup_base=
	active_log_file=
}

run_block_io_all_dimensions() {
	local probe=/sys/fs/cgroup/resman-final-probe-$run_id
	local device_file device
	active_work_root=/tmp/resman-final-work-$run_id
	[[ ! -e $active_work_root ]] || fail "dedicated block-I/O work root already exists"
	install -d -m 0700 -o "$test_uid" -g "$test_gid" "$active_work_root"
	install -m 0755 -o "$test_uid" -g "$test_gid" "$bundle_dir/workload.py" \
		"$active_work_root/workload.py"
	[[ " $(< /sys/fs/cgroup/cgroup.controllers) " == *" io "* ]] \
		|| fail "the io controller is unavailable"
	if [[ " $(< /sys/fs/cgroup/cgroup.subtree_control) " != *" io "* ]]; then
		printf '+io\n' >/sys/fs/cgroup/cgroup.subtree_control \
			|| fail "the io controller cannot be enabled"
	fi
	mkdir "$probe"
	[[ -e $probe/io.max && -e $probe/io.stat ]] \
		|| fail "a real child cgroup does not expose io.max and io.stat"
	: >"$evidence_dir/io-device-probes.txt"
	for device_file in /sys/block/*/dev; do
		device=$(<"$device_file")
		if printf '%s rbps=1048576 wbps=1048576 riops=100 wiops=100\n' "$device" \
			>"$probe/io.max" 2>>"$evidence_dir/io-device-probes.txt"; then
			printf '%s rbps=max wbps=max riops=max wiops=max\n' "$device" \
				>"$probe/io.max" \
				|| fail "selected block device cannot be restored to unlimited io.max"
			printf '%s PASS %s\n' "$device" "$device_file" \
				>>"$evidence_dir/io-device-probes.txt"
			selected_io_device=$device
			break
		fi
		printf '%s FAIL %s\n' "$device" "$device_file" \
			>>"$evidence_dir/io-device-probes.txt"
	done
	[[ -n $selected_io_device ]] \
		|| fail "no whole block device accepts finite four-dimensional io.max limits"
	rmdir "$probe"
	printf 'io_max_available=true\nio_device=%s\n' "$selected_io_device" \
		>"$evidence_dir/block-io-summary.txt"
	for dimension in read_bps write_bps read_iops write_iops; do
		run_io_phase "$dimension"
	done
	result=PASS
	detail="all four I/O dimensions activated from real block accounting without syscall false positives"
}

preflight
case "$scenario" in
	psi-refresh-neutrality) run_psi_refresh_neutrality ;;
	block-io-all-dimensions) run_block_io_all_dimensions ;;
esac

echo "PASS: $detail"
