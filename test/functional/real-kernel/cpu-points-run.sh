#!/usr/bin/env bash
# Real-kernel CPU Points contract for the source revision under review.
set -Eeuo pipefail

PATH=/usr/sbin:/sbin:/usr/bin:/bin
export PATH

scenario=${1:?scenario is required}
run_id=${2:?run id is required}
source_revision=${3:?source revision is required}
bundle_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$bundle_dir/evidence
binary=$bundle_dir/resman
work_root=/tmp/resman-cpu-points-$run_id
managed_root=/sys/fs/cgroup/resman-cpu-points-$run_id
correct_root=/sys/fs/cgroup/resman-cpu-points-correct-$run_id
stale_root=/sys/fs/cgroup/resman-cpu-points-stale-$run_id
config_file=$work_root/resman.conf
map_file=$work_root/cpu-points.map
created_file=$work_root/created-cgroups.txt
origin_file=${created_file%.txt}-origins.json
daemon_log=$work_root/resman.log
daemon_pid=
initial_service_active=
service_quiesced=0
temporary_user=
result=FAIL
detail="CPU Points real-kernel scenario did not complete"
cleanup_status=PASS
load_pids=()
load_users=()
raw_pids=()
last_started_pid=
container_name=

case "$scenario" in
	cpu-points-proportional) ;;
	*) echo "invalid CPU Points scenario: $scenario" >&2; exit 2 ;;
esac
case "$run_id" in
	*[!a-z0-9-]*|'') echo "invalid run id: $run_id" >&2; exit 2 ;;
esac

mkdir -p "$evidence_dir"
chmod 0700 "$evidence_dir"
exec > >(tee -a "$evidence_dir/runner.log") 2>&1

fail() {
	detail=$1
	echo "FAIL: $detail" >&2
	exit 1
}

blocked() {
	detail=$1
	result=BLOCKED
	echo "BLOCKED: $detail" >&2
	exit 77
}

stop_daemon() {
	if [[ -n $daemon_pid ]]; then
		kill -TERM "$daemon_pid" 2>/dev/null || true
		wait "$daemon_pid" 2>/dev/null || true
		daemon_pid=
	fi
}

stop_loads() {
	local wanted=${1:-} index pid user
	for index in "${!load_pids[@]}"; do
		pid=${load_pids[$index]}
		user=${load_users[$index]}
		[[ -n $pid ]] || continue
		[[ -z $wanted || $user == "$wanted" ]] || continue
		kill -TERM "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		load_pids[index]=
	done
}

stop_raw_loads() {
	local pid
	for pid in "${raw_pids[@]:-}"; do
		[[ -n $pid ]] || continue
		kill -TERM "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	raw_pids=()
}

remove_cgroup_tree() {
	local root=$1 path deadline
	[[ -e $root ]] || return 0
	deadline=$((SECONDS + 20))
	while (( SECONDS < deadline )); do
		while IFS= read -r path; do
			rmdir "$path" 2>/dev/null || true
		done < <(find "$root" -depth -type d 2>/dev/null)
		[[ -e $root ]] || return 0
		sleep 1
	done
	return 1
}

finish() {
	local status=$? unexpected=
	trap - EXIT HUP INT TERM
	set +e
	stop_daemon
	stop_loads
	stop_raw_loads
	if [[ -n $container_name ]]; then
		podman rm -f "$container_name" >>"$evidence_dir/cleanup.log" 2>&1 \
			|| cleanup_status=FAIL-container-cleanup
		container_name=
	fi
	for root in "$managed_root" "$correct_root" "$stale_root"; do
		remove_cgroup_tree "$root" || cleanup_status=FAIL-cgroup-cleanup
	done
	if [[ -f $daemon_log ]]; then
		grep ' \[ERROR\] ' "$daemon_log" \
			| grep -v 'CPU Points policy changes the active allocation class' \
			>"$evidence_dir/unexpected-daemon-errors.txt" || true
		[[ ! -s $evidence_dir/unexpected-daemon-errors.txt ]] \
			|| unexpected="unexpected daemon error diagnostics"
	fi
	if [[ -n $temporary_user ]]; then
		userdel "$temporary_user" >>"$evidence_dir/cleanup.log" 2>&1 \
			|| cleanup_status=FAIL-user-cleanup
	fi
	case "$work_root" in
		/tmp/resman-cpu-points-r*) rm -rf -- "$work_root" ;;
		*) cleanup_status=FAIL-work-root ;;
	esac
	if [[ $service_quiesced -eq 1 && $initial_service_active == active ]]; then
		systemctl start resman >>"$evidence_dir/cleanup.log" 2>&1 \
			|| cleanup_status=FAIL-service-restore
	fi
	if [[ -n $unexpected ]]; then
		status=1
		detail=$unexpected
	fi
	if [[ $cleanup_status != PASS ]]; then
		status=1
		detail="CPU Points scenario cleanup failed"
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
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for_log() {
	local pattern=$1 timeout=$2
	local deadline=$((SECONDS + timeout))
	while (( SECONDS < deadline )); do
		grep -Fq "$pattern" "$daemon_log" 2>/dev/null && return 0
		[[ -z $daemon_pid ]] || kill -0 "$daemon_pid" 2>/dev/null \
			|| fail "source daemon exited while waiting for $pattern"
		sleep 1
	done
	return 1
}

scrape_metrics() {
	local output=$1 status
	status=$(curl -sS -o "$output" -w '%{http_code}' --max-time 20 \
		http://127.0.0.1:1976/metrics || true)
	[[ $status == 200 && -s $output ]] \
		|| fail "Prometheus scrape returned HTTP $status"
}

write_map() {
	local first=$1 second=$2 third=$3
	cat >"$map_file" <<EOF
[resman-cpu-points-map-v1]
resman-t1=$first
resman-t2=$second
resman-t3=$third
EOF
	chmod 0600 "$map_file"
}

write_config() {
	local map_path=${1:-$map_file} min_active=${2:-180}
	cat >"$config_file" <<EOF
CGROUP_ROOT=/sys/fs/cgroup
CGROUP_BASE=$(basename "$managed_root")
CREATED_CGROUPS_FILE=$created_file
LOG_FILE=$daemon_log
LOG_LEVEL=DEBUG
USE_SYSLOG=false
POLLING_INTERVAL=5
MIN_ACTIVE_TIME=$min_active
METRICS_CACHE_TTL=1
METRICS_REFRESH_INTERVAL=5
PROCESS_MIN_AGE_SECONDS=0
IGNORE_SYSTEM_LOAD=true
CPU_THRESHOLD=2
CPU_RELEASE_THRESHOLD=1
CPU_THRESHOLD_DURATION=0
CPU_RESERVE_POINTS=100
CPU_BEST_EFFORT_POINTS=100
CPU_POINTS_FILE=$map_path
USER_INCLUDE_LIST=^resman-t1$,^resman-t2$,^resman-t3$,^resman-t4$,^pippo$,^pluto$
USER_EXCLUDE_LIST=root
PROCESS_EXCLUDE_LIST=^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$,^resman-skip$
RAM_LIMIT_ENABLED=false
RAM_USER_INCLUDE_LIST=
RAM_USER_EXCLUDE_LIST=root
IO_LIMIT_ENABLED=false
IO_USER_INCLUDE_LIST=
IO_USER_EXCLUDE_LIST=root
ENABLE_PROMETHEUS=true
PROMETHEUS_METRICS_BIND_HOST=127.0.0.1
PROMETHEUS_METRICS_BIND_PORT=1976
PROMETHEUS_AUTH_TYPE=none
PROMETHEUS_TLS_ENABLED=false
MCP_ENABLED=false
METRICS_DB_ENABLED=false
PSI_EVENT_DRIVEN=false
EOF
	chmod 0600 "$config_file"
}

start_daemon() {
	: >"$daemon_log"
	"$binary" --config "$config_file" >"$evidence_dir/daemon.stdout" \
		2>"$evidence_dir/daemon.stderr" &
	daemon_pid=$!
	wait_for_log 'Entering main control loop' 45 \
		|| fail "source daemon did not enter its control loop"
}

start_user_cpu() {
	local user=$1 count=${2:-1} uid gid i pid
	uid=$(id -u "$user")
	gid=$(id -g "$user")
	for ((i = 0; i < count; i++)); do
		setpriv --reuid="$uid" --regid="$gid" --clear-groups \
			sh -c 'exec sh -c "while :; do :; done"' >/dev/null 2>&1 &
		pid=$!
		load_pids+=("$pid")
		load_users+=("$user")
	done
}

start_user_memory() {
	local user=$1 uid gid pid
	uid=$(id -u "$user")
	gid=$(id -g "$user")
	setpriv --reuid="$uid" --regid="$gid" --clear-groups \
		python3 -c 'import sys,time
needle=sys.argv[1]
deadline=time.time()+45
while time.time()<deadline:
    with open("/proc/self/cgroup", encoding="ascii") as handle:
        if needle in handle.read():
            break
    time.sleep(0.1)
else:
    raise SystemExit("process was not acquired before allocation")
x=bytearray(96*1024*1024)
x[::4096]=b"x"*(len(x)//4096)
time.sleep(3600)' "$(basename "$managed_root")" \
		>/dev/null 2>&1 &
	pid=$!
	load_pids+=("$pid")
	load_users+=("$user")
	last_started_pid=$pid
}

leaf_path() {
	local class=$1 user=$2
	printf '%s/limited/%s/user_%s' "$managed_root" "$class" "$(id -u "$user")"
}

wait_for_leaf() {
	local class=$1 user=$2 timeout=$3 path deadline
	path=$(leaf_path "$class" "$user")
	deadline=$((SECONDS + timeout))
	while (( SECONDS < deadline )); do
		if [[ -r $path/cgroup.procs && -n $(< "$path/cgroup.procs") ]]; then
			return 0
		fi
		sleep 1
	done
	return 1
}

wait_for_absent_leaf() {
	local class=$1 user=$2 timeout=$3 path deadline
	path=$(leaf_path "$class" "$user")
	deadline=$((SECONDS + timeout))
	while (( SECONDS < deadline )); do
		[[ ! -d $path ]] && return 0
		sleep 1
	done
	return 1
}

cpu_stat_value() {
	local path=$1 key=$2
	awk -v key="$key" '$1 == key { print $2; found=1 } END { if (!found) print 0 }' \
		"$path/cpu.stat"
}

snapshot_nodes() {
	local output=$1 item name path start end usage periods throttled throttled_usec
	shift
	: >"$output"
	for item in "$@"; do
		name=${item%%=*}
		path=${item#*=}
		start=$(date +%s%N)
		usage=$(cpu_stat_value "$path" usage_usec)
		periods=$(cpu_stat_value "$path" nr_periods)
		throttled=$(cpu_stat_value "$path" nr_throttled)
		throttled_usec=$(cpu_stat_value "$path" throttled_usec)
		end=$(date +%s%N)
		printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
			"$name" "$start" "$end" "$usage" "$periods" "$throttled" "$throttled_usec" \
			>>"$output"
	done
}

snapshot_field() {
	local file=$1 node=$2 field=$3
	awk -F '\t' -v node="$node" -v field="$field" '$1 == node { print $field; exit }' "$file"
}

measurement_value() {
	local file=$1 key=$2
	sed -n "s/^${key}=//p" "$file" | tail -1
}

origin_record() {
	local file=$1 pid=$2
	python3 - "$file" "$pid" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    state = json.load(handle)
pid = int(sys.argv[2])
for origin in state.get("origins", []):
    if origin.get("pid") == pid:
        print(json.dumps(origin, sort_keys=True, separators=(",", ":")))
        break
else:
    raise SystemExit(1)
PY
}

measure_nodes() {
	local name=$1 seconds=$2
	shift 2
	local before=$evidence_dir/$name-before.tsv after=$evidence_dir/$name-after.tsv
	local summary=$evidence_dir/$name.txt item node before_usage after_usage delta
	local min_start max_end skew parent guaranteed best_effort parent_path cpu_max quota period nominal
	snapshot_nodes "$before" "$@"
	sleep "$seconds"
	snapshot_nodes "$after" "$@"
	min_start=$(awk -F '\t' 'NR == 1 || $2 < min { min=$2 } END { print min }' "$before")
	max_end=$(awk -F '\t' 'NR == 1 || $3 > max { max=$3 } END { print max }' "$after")
	skew=$(awk -F '\t' '
		FNR == NR { if (NR == 1 || $2 < min1) min1=$2; if (NR == 1 || $3 > max1) max1=$3; next }
		{ if (FNR == 1 || $2 < min2) min2=$2; if (FNR == 1 || $3 > max2) max2=$3 }
		END { a=max1-min1; b=max2-min2; if (b>a) a=b; printf "%.3f", a/1000000 }
	' "$before" "$after")
	{
		printf 'window_seconds=%s\nread_window_start_ns=%s\nread_window_end_ns=%s\nmax_sampling_skew_ms=%s\n' \
			"$seconds" "$min_start" "$max_end" "$skew"
		for item in "$@"; do
			node=${item%%=*}
			before_usage=$(snapshot_field "$before" "$node" 4)
			after_usage=$(snapshot_field "$after" "$node" 4)
			delta=$((after_usage - before_usage))
			(( delta >= 0 )) || fail "$name observed a decreasing usage counter for $node"
			printf '%s_usage_usec=%s\n' "$node" "$delta"
		done
		parent=$(snapshot_field "$after" parent 4)
		before_usage=$(snapshot_field "$before" parent 4)
		parent=$((parent - before_usage))
		guaranteed=$(snapshot_field "$after" guaranteed 4)
		before_usage=$(snapshot_field "$before" guaranteed 4)
		guaranteed=$((guaranteed - before_usage))
		best_effort=$(snapshot_field "$after" best_effort 4)
		before_usage=$(snapshot_field "$before" best_effort 4)
		best_effort=$((best_effort - before_usage))
		awk -v p="$parent" -v g="$guaranteed" -v b="$best_effort" 'BEGIN {
			if (p <= 0 || g < 0 || b < 0) exit 1
			printf "guaranteed_parent_share=%.6f\n", 100*g/p
			printf "best_effort_parent_share=%.6f\n", 100*b/p
		}' || fail "$name did not deliver measurable parent/domain CPU"
		parent_path=
		for item in "$@"; do
			[[ ${item%%=*} == parent ]] && parent_path=${item#*=}
		done
		cpu_max=$(< "$parent_path/cpu.max")
		read -r quota period <<<"$cpu_max"
		[[ $quota =~ ^[0-9]+$ && $period =~ ^[0-9]+$ ]] \
			|| fail "$name parent cpu.max is not finite: $cpu_max"
		nominal=$((quota * seconds * 1000000 / period))
		printf 'online_cpus=%s\nprogrammed_cpu_max=%s\nparent_nominal_usec=%s\n' \
			"$(getconf _NPROCESSORS_ONLN)" "$cpu_max" "$nominal"
		awk -v p="$parent" -v n="$nominal" 'BEGIN {
			if (n <= 0) exit 1
			printf "parent_effective_nominal_ratio=%.6f\n", p/n
		}'
		printf 'parent_nr_periods_delta=%s\n' "$(( $(snapshot_field "$after" parent 5) - $(snapshot_field "$before" parent 5) ))"
		printf 'parent_nr_throttled_delta=%s\n' "$(( $(snapshot_field "$after" parent 6) - $(snapshot_field "$before" parent 6) ))"
		printf 'parent_throttled_usec_delta=%s\n' "$(( $(snapshot_field "$after" parent 7) - $(snapshot_field "$before" parent 7) ))"
		for node in g1 g2 g3; do
			if grep -q "^${node}[[:space:]]" "$before"; then
				delta=$(($(snapshot_field "$after" "$node" 4) - $(snapshot_field "$before" "$node" 4)))
				if (( guaranteed > 0 )); then
					awk -v node="$node" -v d="$delta" -v g="$guaranteed" 'BEGIN {
						printf "%s_guaranteed_share=%.6f\n", node, 100*d/g
					}'
				fi
			fi
		done
		for node in be1 be2; do
			if grep -q "^${node}[[:space:]]" "$before"; then
				delta=$(($(snapshot_field "$after" "$node" 4) - $(snapshot_field "$before" "$node" 4)))
				awk -v node="$node" -v d="$delta" -v b="$best_effort" 'BEGIN {
					if (b <= 0) exit 1
					printf "%s_best_effort_share=%.6f\n", node, 100*d/b
				}'
			fi
		done
	} >"$summary"
}

assert_within() {
	local actual=$1 expected=$2 tolerance=$3 message=$4
	awk -v a="$actual" -v e="$expected" -v t="$tolerance" 'BEGIN {
		d=a-e; if (d<0) d=-d; exit !(d<=t)
	}' || fail "$message: actual=$actual expected=$expected tolerance=$tolerance"
}

assert_at_least() {
	local actual=$1 minimum=$2 message=$3
	awk -v a="$actual" -v m="$minimum" 'BEGIN { exit !(a>=m) }' \
		|| fail "$message: actual=$actual minimum=$minimum"
}

create_reference_hierarchy() {
	local root=$1 guaranteed_weight=$2 online=$3
	local quota=$((online * 900 * 100))
	mkdir "$root"
	printf '%s 100000' "$quota" >"$root/cpu.max"
	printf '+cpu' >"$root/cgroup.subtree_control"
	mkdir "$root/guaranteed" "$root/best_effort"
	printf '%s' "$guaranteed_weight" >"$root/guaranteed/cpu.weight"
	printf '100' >"$root/best_effort/cpu.weight"
	printf '+cpu' >"$root/guaranteed/cgroup.subtree_control"
	printf '+cpu' >"$root/best_effort/cgroup.subtree_control"
	local leaf weight
	for leaf in g1 g2 g3; do
		case "$leaf" in g1|g2) weight=300 ;; g3) weight=200 ;; esac
		mkdir "$root/guaranteed/$leaf"
		printf '%s' "$weight" >"$root/guaranteed/$leaf/cpu.weight"
	done
	for leaf in be1 be2; do
		mkdir "$root/best_effort/$leaf"
		printf '100' >"$root/best_effort/$leaf/cpu.weight"
	done
	[[ ! -s $root/cgroup.procs && ! -s $root/guaranteed/cgroup.procs \
		&& ! -s $root/best_effort/cgroup.procs ]] \
		|| fail "reference hierarchy contains processes in an internal node"
	for leaf in "$root/guaranteed/g1" "$root/guaranteed/g2" "$root/guaranteed/g3" \
		"$root/best_effort/be1" "$root/best_effort/be2"; do
		[[ -w $leaf/cpu.weight && -w $leaf/cpu.max ]] \
			|| fail "reference leaf lacks CPU controller interfaces: $leaf"
	done
}

start_raw_leaf_load() {
	local leaf=$1 count=${2:-2} i pid
	for ((i = 0; i < count; i++)); do
		sh -c 'exec sh -c "while :; do :; done"' >/dev/null 2>&1 &
		pid=$!
		raw_pids+=("$pid")
		printf '%s' "$pid" >"$leaf/cgroup.procs"
	done
}

load_reference() {
	local root=$1
	start_raw_leaf_load "$root/guaranteed/g1"
	start_raw_leaf_load "$root/guaranteed/g2"
	start_raw_leaf_load "$root/guaranteed/g3"
	start_raw_leaf_load "$root/best_effort/be1"
	start_raw_leaf_load "$root/best_effort/be2"
}

reference_nodes() {
	local root=$1
	printf '%s\n' \
		"parent=$root" \
		"guaranteed=$root/guaranteed" \
		"best_effort=$root/best_effort" \
		"g1=$root/guaranteed/g1" \
		"g2=$root/guaranteed/g2" \
		"g3=$root/guaranteed/g3" \
		"be1=$root/best_effort/be1" \
		"be2=$root/best_effort/be2"
}

preflight() {
	local os_name
	[[ $EUID -eq 0 ]] || fail "CPU Points real-kernel scenario requires root"
	[[ -x $binary ]] || fail "staged source binary is missing"
	[[ $(stat -fc %T /sys/fs/cgroup) == cgroup2fs ]] || blocked "cgroup v2 is unavailable"
	for command in awk curl date find setpriv stat systemctl useradd userdel; do
		command -v "$command" >/dev/null || blocked "$command is unavailable"
	done
	for user in resman-t1 resman-t2 resman-t3 resman-t4 pippo pluto; do
		id "$user" >/dev/null 2>&1 || blocked "fixture user $user is unavailable"
	done
	[[ ! -e $managed_root && ! -e $correct_root && ! -e $stale_root ]] \
		|| blocked "run-scoped CPU Points cgroup already exists"
	initial_service_active=$(systemctl is-active resman 2>&1 || true)
	if [[ $initial_service_active == active ]]; then
		systemctl stop resman || fail "installed resman service could not be quiesced"
		service_quiesced=1
	fi
	pgrep -x resman >/dev/null 2>&1 \
		&& fail "a resman process remains active after service quiescence"
	mkdir -p "$work_root"
	chmod 0700 "$work_root"
	os_name=$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"')
	{
		printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		printf 'scenario=%s\nsource_revision=%s\nartifact=source-revision\n' "$scenario" "$source_revision"
		printf 'host=%s\nos=%s\nkernel=%s\n' "$(hostname -f 2>/dev/null || hostname)" "$os_name" "$(uname -r)"
		printf 'cgroup_mount=%s\ncontrollers=%s\n' \
			"$(findmnt -n -o SOURCE,FSTYPE,OPTIONS /sys/fs/cgroup)" "$(< /sys/fs/cgroup/cgroup.controllers)"
		printf 'online_cpu_list=%s\nonline_cpu_count=%s\n' \
			"$(< /sys/devices/system/cpu/online)" "$(getconf _NPROCESSORS_ONLN)"
		printf 'initial_service_active=%s\n' "$initial_service_active"
	} >"$evidence_dir/environment.txt"
}

probe_kernel_boundaries() {
	local root=/sys/fs/cgroup/resman-cpu-points-probe-$run_id online quota1000
	mkdir "$root"
	printf '+cpu' >"$root/cgroup.subtree_control"
	mkdir "$root/leaf"
	if printf '999 100000' >"$root/leaf/cpu.max" 2>/dev/null; then
		fail "kernel accepted a CPU quota below the measured 1000-usec floor"
	fi
	printf '1000 100000' >"$root/leaf/cpu.max" \
		|| fail "kernel rejected the documented 1000-usec quota floor"
	printf '1' >"$root/leaf/cpu.weight" \
		|| fail "kernel rejected the one-point CPU weight"
	online=$(getconf _NPROCESSORS_ONLN)
	quota1000=$((online * 100000))
	printf '%s 100000' "$quota1000" >"$root/cpu.max" \
		|| fail "kernel rejected the full-capacity parent quota"
	{
		printf 'minimum_quota_rejected=999 100000\nminimum_quota_accepted=%s\n' "$(< "$root/leaf/cpu.max")"
		printf 'minimum_weight_accepted=%s\nfull_pool_cpu_max=%s\nonline_cpus=%s\n' \
			"$(< "$root/leaf/cpu.weight")" "$(< "$root/cpu.max")" "$online"
	} >"$evidence_dir/kernel-boundaries.txt"
	rmdir "$root/leaf" "$root"
}

prove_policy_edges() {
	local bad_map=$work_root/overcommit.map dotted_map=$work_root/dotted.map dotted_config=$work_root/dotted.conf
	local dotted_user=resman.point
	write_map 300 300 200
	write_config
	# The equality vector is the actual daemon policy exercised below.
	printf 'configured_guarantees=800\nbest_effort=100\nreserve=100\nparent_pool=900\nresult=accepted\n' \
		>"$evidence_dir/policy-equality.txt"
	cat >"$bad_map" <<'EOF'
[resman-cpu-points-map-v1]
resman-t1=301
resman-t2=300
resman-t3=200
EOF
	chmod 0600 "$bad_map"
	write_config "$bad_map"
	: >"$daemon_log"
	set +e
	timeout 15 "$binary" --config "$config_file" >"$evidence_dir/overcommit.stdout" \
		2>"$evidence_dir/overcommit.stderr"
	local bad_status=$?
	set -e
	[[ $bad_status -eq 78 ]] \
		|| fail "one-point CPU Points overcommit exited $bad_status, want permanent-configuration status 78"
	{
		cat "$evidence_dir/overcommit.stderr"
		[[ ! -f $daemon_log ]] || cat "$daemon_log"
	} >"$evidence_dir/overcommit-diagnostic.txt"
	grep -qi 'overcommit' "$evidence_dir/overcommit-diagnostic.txt" \
		|| fail "one-point overcommit failure did not name the cause"

	if ! getent passwd "$dotted_user" >/dev/null 2>&1; then
		useradd --badname --no-create-home --shell /sbin/nologin "$dotted_user"
		temporary_user=$dotted_user
	fi
	cat >"$dotted_map" <<EOF
[resman-cpu-points-map-v1]
$dotted_user=1
EOF
	chmod 0600 "$dotted_map"
	write_config "$dotted_map"
	cp "$config_file" "$dotted_config"
	"$binary" --config "$dotted_config" >"$evidence_dir/dotted.stdout" \
		2>"$evidence_dir/dotted.stderr" &
	daemon_pid=$!
	wait_for_log 'Entering main control loop' 45 \
		|| fail "dotted NSS username did not produce a valid policy"
	stop_daemon
	printf 'username=%s\nuid=%s\nresult=accepted\n' "$dotted_user" "$(id -u "$dotted_user")" \
		>"$evidence_dir/dotted-identity.txt"
	write_map 300 300 200
	write_config
}

run_reference_oracles() {
	local online correct_share stale_share separation
	local nodes=()
	online=$(getconf _NPROCESSORS_ONLN)
	create_reference_hierarchy "$correct_root" 800 "$online"
	mapfile -t nodes < <(reference_nodes "$correct_root")
	load_reference "$correct_root"
	sleep 2
	measure_nodes reference-correct 60 "${nodes[@]}"
	stop_raw_loads
	remove_cgroup_tree "$correct_root" || fail "correct reference hierarchy did not clean up"

	create_reference_hierarchy "$stale_root" 600 "$online"
	mapfile -t nodes < <(reference_nodes "$stale_root")
	load_reference "$stale_root"
	sleep 2
	measure_nodes reference-stale-low 60 "${nodes[@]}"
	stop_raw_loads
	remove_cgroup_tree "$stale_root" || fail "stale reference hierarchy did not clean up"

	correct_share=$(measurement_value "$evidence_dir/reference-correct.txt" guaranteed_parent_share)
	stale_share=$(measurement_value "$evidence_dir/reference-stale-low.txt" guaranteed_parent_share)
	separation=$(awk -v c="$correct_share" -v s="$stale_share" 'BEGIN { d=c-s; if (d<0) d=-d; printf "%.6f", d }')
	printf 'correct_guaranteed_parent_share=%s\nstale_guaranteed_parent_share=%s\nseparation_points=%s\n' \
		"$correct_share" "$stale_share" "$separation" >"$evidence_dir/reference-comparison.txt"
	if ! awk -v d="$separation" 'BEGIN { exit !(d>=2.0) }'; then
		result=BLOCKED
		detail="same-run stale-low oracle separation was $separation points, below 2.0"
		exit 77
	fi
}

actual_nodes() {
	printf '%s\n' \
		"parent=$managed_root/limited" \
		"guaranteed=$managed_root/limited/guaranteed" \
		"best_effort=$managed_root/limited/best_effort" \
		"g1=$(leaf_path guaranteed resman-t1)" \
		"g2=$(leaf_path guaranteed resman-t2)" \
		"g3=$(leaf_path guaranteed resman-t3)" \
		"be1=$(leaf_path best_effort pippo)" \
		"be2=$(leaf_path best_effort pluto)"
}

verify_actual_against_reference() {
	local actual=$evidence_dir/actual-full-contention.txt correct=$evidence_dir/reference-correct.txt
	local stale=$evidence_dir/reference-stale-low.txt key got want
	assert_within "$(measurement_value "$actual" guaranteed_parent_share)" \
		"$(measurement_value "$correct" guaranteed_parent_share)" 0.5 \
		"ResMan guaranteed-domain share diverged from the same-run correct oracle"
	assert_within "$(measurement_value "$actual" best_effort_parent_share)" \
		"$(measurement_value "$correct" best_effort_parent_share)" 0.5 \
		"ResMan best-effort-domain share diverged from the same-run correct oracle"
	for key in g1_guaranteed_share g2_guaranteed_share g3_guaranteed_share; do
		got=$(measurement_value "$actual" "$key")
		want=$(measurement_value "$correct" "$key")
		assert_within "$got" "$want" 1.0 "ResMan leaf ratio $key diverged from the correct oracle"
	done
	local actual_share stale_share
	actual_share=$(measurement_value "$actual" guaranteed_parent_share)
	stale_share=$(measurement_value "$stale" guaranteed_parent_share)
	assert_at_least "$(awk -v a="$actual_share" -v s="$stale_share" 'BEGIN { d=a-s; if(d<0)d=-d; print d }')" \
		2.0 "ResMan result was not distinguishable from stale-low w_G"
}

run_partial_coverage_contract() {
	local user=resman-t4 uid gid image nested_pid nested_before nested_after excluded_pid excluded_before
	local host_ns nested_ns coverage_line
	uid=$(id -u "$user")
	gid=$(id -g "$user")
	image=$(podman images --format '{{.Repository}}:{{.Tag}}' | grep -v '^<none>' | head -n 1)
	[[ -n $image ]] || blocked "no local Podman image is available for PID-namespace coverage"
	container_name=resman-vcs10-${run_id#r}
	podman run -d --rm --name "$container_name" --user "$uid" "$image" \
		sh -c 'while :; do :; done' >/dev/null \
		|| fail "nested PID-namespace workload could not start"
	nested_pid=$(podman inspect "$container_name" --format '{{.State.Pid}}')
	[[ -n $nested_pid && $nested_pid != 0 ]] || fail "nested workload exposed no host PID"
	host_ns=$(stat -L -c '%d:%i' /proc/1/ns/pid)
	nested_ns=$(stat -L -c '%d:%i' "/proc/$nested_pid/ns/pid")
	[[ $nested_ns != "$host_ns" ]] || fail "Podman workload shares the host PID namespace"
	nested_before=$(cut -d: -f3 "/proc/$nested_pid/cgroup")

	install -m 0755 /usr/bin/sleep "$work_root/resman-skip"
	setpriv --reuid="$uid" --regid="$gid" --clear-groups \
		"$work_root/resman-skip" 3600 >/dev/null 2>&1 &
	excluded_pid=$!
	load_pids+=("$excluded_pid")
	load_users+=("$user")
	excluded_before=$(cut -d: -f3 "/proc/$excluded_pid/cgroup")
	start_user_cpu "$user" 2
	wait_for_leaf best_effort "$user" 90 \
		|| fail "mixed-coverage host processes were not admitted"
	sleep 4
	nested_after=$(cut -d: -f3 "/proc/$nested_pid/cgroup")
	[[ $nested_after == "$nested_before" ]] \
		|| fail "nested PID-namespace process left its runtime-owned cgroup"
	[[ $(cut -d: -f3 "/proc/$excluded_pid/cgroup") == "$excluded_before" ]] \
		|| fail "process-excluded workload entered ResMan ownership"
	[[ $(cut -d: -f3 "/proc/$nested_pid/cgroup") != *"$(basename "$managed_root")"* ]] \
		|| fail "nested process entered the managed CPU Points hierarchy"
	scrape_metrics "$evidence_dir/metrics-partial-coverage.prom"
	coverage_line=$(grep -E "^resman_user_cpu_points_process_coverage\{.*uid=\"$uid\".*coverage=\"partial\".*\} 1$" \
		"$evidence_dir/metrics-partial-coverage.prom" || true)
	[[ -n $coverage_line ]] \
		|| fail "mixed namespace/excluded workload was serialized as a whole-UID guarantee"
	{
		printf 'uid=%s\nhost_pid_namespace=%s\nnested_pid=%s\nnested_pid_namespace=%s\n' \
			"$uid" "$host_ns" "$nested_pid" "$nested_ns"
		printf 'nested_cgroup_before=%s\nnested_cgroup_after=%s\n' "$nested_before" "$nested_after"
		printf 'excluded_pid=%s\nexcluded_cgroup=%s\ncoverage=partial\n' "$excluded_pid" "$excluded_before"
	} >"$evidence_dir/partial-coverage.txt"
	podman rm -f "$container_name" >/dev/null
	container_name=
	stop_loads "$user"
}

run_daemon_contract() {
	local nodes=() user class weight memory_pid before_path after_path before_memory after_memory
	local before_origin after_origin before_inode after_inode correct_share lending_share borrowed_share
	write_map 300 300 200
	write_config "$map_file" 900
	start_daemon

	start_user_cpu resman-t1 2
	start_user_cpu resman-t2 2
	start_user_cpu pippo 2
	start_user_cpu pluto 2
	for user in resman-t1 resman-t2; do
		wait_for_leaf guaranteed "$user" 90 || fail "$user was not admitted as guaranteed"
	done
	for user in pippo pluto; do
		wait_for_leaf best_effort "$user" 90 || fail "$user was not admitted as best effort"
	done
	mapfile -t nodes < <(actual_nodes | grep -v '^g3=')
	measure_nodes actual-before-transition 60 "${nodes[@]}"

	start_user_cpu resman-t3 2
	wait_for_leaf guaranteed resman-t3 90 || fail "third guaranteed user was not admitted"
	weight=$(< "$managed_root/limited/guaranteed/cpu.weight")
	[[ $weight == 800 ]] || fail "third ingress became visible while guaranteed-domain weight was $weight, want 800"
	printf 'observed_domain_weight_at_first_confirmed_third_ingress=%s\nresult=raise-before-admission-confirmed\n' \
		"$weight" >"$evidence_dir/admission-ordering.txt"
	mapfile -t nodes < <(actual_nodes)
	measure_nodes actual-full-contention 60 "${nodes[@]}"
	verify_actual_against_reference

	# An idle guaranteed leaf lends within its own class first: t1 remains
	# runnable, t2 and t3 stay acquired but have no runnable descendants, and
	# best effort remains contended. The guaranteed domain must retain its
	# same-run correct share and the runnable sibling consumes the domain.
	stop_loads resman-t2
	stop_loads resman-t3
	measure_nodes actual-guaranteed-sibling-lending 60 "${nodes[@]}"
	assert_within \
		"$(measurement_value "$evidence_dir/actual-guaranteed-sibling-lending.txt" guaranteed_parent_share)" \
		"$(measurement_value "$evidence_dir/reference-correct.txt" guaranteed_parent_share)" 0.5 \
		"idle mapped leaves leaked entitlement across the class boundary"
	assert_at_least \
		"$(measurement_value "$evidence_dir/actual-guaranteed-sibling-lending.txt" g1_guaranteed_share)" \
		99.0 "the runnable guaranteed sibling did not consume the idle mapped shares"
	[[ $(< "$managed_root/limited/guaranteed/cpu.weight") == 800 ]] \
		|| fail "userspace rewrote w_G from scheduler runnability"

	# Only when the complete guaranteed domain is idle may best effort borrow
	# the finite parent by kernel work conservation. No weight changes here.
	stop_loads resman-t1
	measure_nodes actual-best-effort-borrowing 60 "${nodes[@]}"
	assert_at_least \
		"$(measurement_value "$evidence_dir/actual-best-effort-borrowing.txt" best_effort_parent_share)" \
		95.0 "best effort did not borrow the idle guaranteed domain"
	[[ $(< "$managed_root/limited/guaranteed/cpu.weight") == 800 ]] \
		|| fail "best-effort borrowing was implemented by rewriting w_G"

	# Restore contention before exercising live reload contracts.
	start_user_cpu resman-t1 2
	start_user_cpu resman-t2 2
	start_user_cpu resman-t3 2
	for user in resman-t1 resman-t2 resman-t3; do
		wait_for_leaf guaranteed "$user" 45 || fail "$user did not resume in its guaranteed leaf"
	done

	[[ $(< "$managed_root/limited/cpu.max") == "$(($(getconf _NPROCESSORS_ONLN) * 900 * 100)) 100000" ]] \
		|| fail "ResMan programmed an unexpected parent cpu.max"
	[[ $(< "$managed_root/limited/guaranteed/cpu.weight") == 800 ]] \
		|| fail "ResMan guaranteed-domain weight is not the acquired guarantee sum"
	[[ $(< "$managed_root/limited/best_effort/cpu.weight") == 100 ]] \
		|| fail "ResMan best-effort domain weight is not the aggregate entitlement"
	for user in pippo pluto; do
		[[ $(< "$(leaf_path best_effort "$user")/cpu.weight") == 100 ]] \
			|| fail "$user did not retain the class-local best-effort leaf weight"
	done

	scrape_metrics "$evidence_dir/metrics-full-contention.prom"
	for metric in resman_cpu_points_online_cpus resman_cpu_points_parent_quota_microseconds \
		resman_cpu_points_parent_period_microseconds resman_cpu_points_applied_guarantee_total \
		resman_cpu_points_programmed_guaranteed_domain_weight; do
		grep -q "^${metric}[ {]" "$evidence_dir/metrics-full-contention.prom" \
			|| fail "live scrape omitted $metric"
		done

	run_partial_coverage_contract

	# Same-class reload changes weights without moving any active leaf.
	write_map 250 350 200
	kill -HUP "$daemon_pid"
	local deadline=$((SECONDS + 45))
	while (( SECONDS < deadline )); do
		[[ $(< "$(leaf_path guaranteed resman-t1)/cpu.weight") == 250 \
			&& $(< "$(leaf_path guaranteed resman-t2)/cpu.weight") == 350 \
			&& $(< "$managed_root/limited/guaranteed/cpu.weight") == 800 ]] && break
		sleep 1
	done
	[[ $(< "$(leaf_path guaranteed resman-t1)/cpu.weight") == 250 ]] \
		|| fail "same-class reload did not update the first leaf"
	printf 'domain_weight=%s\nt1_weight=%s\nt2_weight=%s\nt3_weight=%s\n' \
		"$(< "$managed_root/limited/guaranteed/cpu.weight")" \
		"$(< "$(leaf_path guaranteed resman-t1)/cpu.weight")" \
		"$(< "$(leaf_path guaranteed resman-t2)/cpu.weight")" \
		"$(< "$(leaf_path guaranteed resman-t3)/cpu.weight")" \
		>"$evidence_dir/same-class-reload.txt"
	write_map 300 300 200
	kill -HUP "$daemon_pid"
	sleep 4

	# Active class-changing reload is rejected before any migration or old-epoch publication.
	start_user_memory resman-t1
	memory_pid=$last_started_pid
	local t1_leaf
	t1_leaf=$(leaf_path guaranteed resman-t1)
	deadline=$((SECONDS + 45))
	while (( SECONDS < deadline )); do
		grep -qx "$memory_pid" "$t1_leaf/cgroup.procs" 2>/dev/null && break
		sleep 1
	done
	grep -qx "$memory_pid" "$t1_leaf/cgroup.procs" \
		|| fail "resident-memory process did not enter the guaranteed leaf"
	sleep 3
	before_path=$(cut -d: -f3 "/proc/$memory_pid/cgroup")
	before_memory=$(< "$t1_leaf/memory.current")
	before_inode=$(stat -c '%d:%i' "$t1_leaf")
	before_origin=$(origin_record "$origin_file" "$memory_pid") \
		|| fail "resident-memory process has no durable origin record"
	cp "$t1_leaf/io.stat" "$evidence_dir/class-change-before-io.stat"
	scrape_metrics "$evidence_dir/class-change-before.prom"
	grep -E "^resman_user_cpu_points_(configured_guarantee|applied_class|applied_weight)\{.*uid=\"$(id -u resman-t1)\"" \
		"$evidence_dir/class-change-before.prom" >"$evidence_dir/class-change-before-state.prom" || true
	# Removing resman-t1 from the map changes its class while it is acquired.
	cat >"$map_file" <<'EOF'
[resman-cpu-points-map-v1]
resman-t2=300
resman-t3=200
EOF
	chmod 0600 "$map_file"
	local marker
	marker=$(wc -l <"$daemon_log")
	kill -HUP "$daemon_pid"
	deadline=$((SECONDS + 45))
	while (( SECONDS < deadline )); do
		tail -n +$((marker + 1)) "$daemon_log" \
			| grep -q 'CPU Points policy changes the active allocation class' && break
		sleep 1
	done
	tail -n +$((marker + 1)) "$daemon_log" \
		| grep 'CPU Points policy changes the active allocation class' \
		>"$evidence_dir/class-change-rejection.log" \
		|| fail "active class-changing reload was not rejected explicitly"
	after_path=$(cut -d: -f3 "/proc/$memory_pid/cgroup")
	after_memory=$(< "$t1_leaf/memory.current")
	after_inode=$(stat -c '%d:%i' "$t1_leaf")
	after_origin=$(origin_record "$origin_file" "$memory_pid") \
		|| fail "rejected class change lost the resident process origin"
	cp "$t1_leaf/io.stat" "$evidence_dir/class-change-after-io.stat"
	[[ $after_path == "$before_path" && $after_inode == "$before_inode" ]] \
		|| fail "rejected class change migrated or recreated the active leaf"
	(( before_memory > 0 && after_memory > 0 )) \
		|| fail "rejected class change produced a false-zero memory observation"
	[[ $after_origin == "$before_origin" ]] \
		|| fail "rejected class change mutated process-origin state"
	cmp -s "$evidence_dir/class-change-before-io.stat" "$evidence_dir/class-change-after-io.stat" \
		|| fail "rejected class change reset or moved the leaf I/O ledger"
	scrape_metrics "$evidence_dir/class-change-after.prom"
	grep -E "^resman_user_cpu_points_(configured_guarantee|applied_class|applied_weight)\{.*uid=\"$(id -u resman-t1)\"" \
		"$evidence_dir/class-change-after.prom" >"$evidence_dir/class-change-after-state.prom" || true
	cmp -s "$evidence_dir/class-change-before-state.prom" "$evidence_dir/class-change-after-state.prom" \
		|| fail "rejected class change published a different user epoch"
	{
		printf 'pid=%s\nbefore_cgroup=%s\nafter_cgroup=%s\n' "$memory_pid" "$before_path" "$after_path"
		printf 'before_memory_current=%s\nafter_memory_current=%s\n' "$before_memory" "$after_memory"
		printf 'before_leaf_identity=%s\nafter_leaf_identity=%s\n' "$before_inode" "$after_inode"
		printf 'before_origin_digest=%s\nafter_origin_digest=%s\n' "$before_origin" "$after_origin"
	} >"$evidence_dir/class-change-preservation.txt"

	# Restore the old epoch, shorten the hold, release every active leaf, then
	# apply the membership change while inactive and reacquire t1 as best effort.
	write_map 300 300 200
	write_config "$map_file" 1
	kill -HUP "$daemon_pid"
	sleep 4
	stop_loads
	for user in resman-t1 resman-t2 resman-t3; do
		wait_for_absent_leaf guaranteed "$user" 90 || fail "$user did not release"
	done
	for user in pippo pluto; do
		wait_for_absent_leaf best_effort "$user" 90 || fail "$user did not release"
	done
	wait_for_absent_leaf best_effort resman-t4 90 || fail "resman-t4 did not release"
	cat >"$map_file" <<'EOF'
[resman-cpu-points-map-v1]
resman-t2=300
resman-t3=200
EOF
	chmod 0600 "$map_file"
	kill -HUP "$daemon_pid"
	sleep 4
	start_user_cpu resman-t1 2
	wait_for_leaf best_effort resman-t1 90 \
		|| fail "released t1 was not reacquired in its new best-effort class"
	printf 'released_guaranteed_leaf=true\nreacquired_class=best_effort\nreacquired_weight=%s\n' \
		"$(< "$(leaf_path best_effort resman-t1)/cpu.weight")" \
		>"$evidence_dir/release-reacquire.txt"

	# Clean shutdown restores still-live processes rather than killing them.
	local live_pid
	live_pid=$(head -n 1 "$(leaf_path best_effort resman-t1)/cgroup.procs")
	stop_daemon
	[[ -d /proc/$live_pid ]] || fail "shutdown killed the acquired workload"
	[[ $(cut -d: -f3 "/proc/$live_pid/cgroup") != *"$(basename "$managed_root")"* ]] \
		|| fail "shutdown left the workload inside the managed hierarchy"
	printf 'pid=%s\nalive_after_shutdown=true\nrestored_cgroup=%s\n' \
		"$live_pid" "$(cut -d: -f3 "/proc/$live_pid/cgroup")" \
		>"$evidence_dir/shutdown-restoration.txt"
	stop_loads

	# Record the two class-priority lending boundaries. Production ordering and
	# the absence of runnable-derived weight rewrites are also mutation-tested by
	# the focused gate.
	correct_share=$(measurement_value "$evidence_dir/reference-correct.txt" guaranteed_parent_share)
	lending_share=$(measurement_value "$evidence_dir/actual-guaranteed-sibling-lending.txt" guaranteed_parent_share)
	borrowed_share=$(measurement_value "$evidence_dir/actual-best-effort-borrowing.txt" best_effort_parent_share)
	printf 'correct_reference_guaranteed_share=%s\nactual_guaranteed_share=%s\nactual_best_effort_share=%s\nuserspace_runnability_weight_updates=none\n' \
		"$correct_share" "$lending_share" "$borrowed_share" >"$evidence_dir/lending-contract.txt"
}

record_hotplug_disposition() {
	local online_file=
	for candidate in /sys/devices/system/cpu/cpu[1-9]*/online; do
		[[ -e $candidate ]] || continue
		online_file=$candidate
		break
	done
	if [[ ${RESMAN_REAL_KERNEL_ALLOW_CPU_HOTPLUG:-0} != 1 ]]; then
		printf 'status=BLOCKED\nreason=CPU hotplug was not explicitly opted in for this shared laboratory host\ncandidate=%s\n' \
			"${online_file:-none}" >"$evidence_dir/hotplug.txt"
		return
	fi
	[[ -n $online_file && -w $online_file ]] \
		|| { printf 'status=BLOCKED\nreason=no writable non-boot CPU online control\n' >"$evidence_dir/hotplug.txt"; return; }
	printf 'status=BLOCKED\nreason=hotplug opt-in was present but the destructive transition is intentionally owned by a separate run\ncandidate=%s\n' \
		"$online_file" >"$evidence_dir/hotplug.txt"
}

main() {
	preflight
	probe_kernel_boundaries
	prove_policy_edges
	run_reference_oracles
	run_daemon_contract
	record_hotplug_disposition
	cat >"$evidence_dir/cpu-points-summary.txt" <<EOF
policy_equality=PASS
stale_low_separation=PASS
full_contention=PASS
class_priority_lending=PASS
class_change_preservation=PASS
partial_coverage=PASS
shutdown_restoration=PASS
hotplug=$(measurement_value "$evidence_dir/hotplug.txt" status)
EOF
	result=PASS
	detail="production-valid CPU Points equality, independent correct/stale oracles, live admission, reload, release, and shutdown contracts passed"
}

main "$@"
