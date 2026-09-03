#!/usr/bin/env bash
set -Eeuo pipefail

# Remote non-interactive root shells may omit administrative directories even
# though the packaged tools are installed there (runuser is /usr/sbin on EL9).
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

scenario=${1:?scenario is required}
run_id=${2:?run id is required}
source_revision=${3:?source revision is required}
bundle_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$bundle_dir/evidence
binary=$bundle_dir/resman
test_user=${RESMAN_REAL_KERNEL_USER:-resman-t1}
test_uid=
test_gid=
work_root=/tmp/resman-final-work-$run_id
helper_root=/tmp/resman-final-helper-$run_id
pid_root=/tmp/resman-final-pids-$run_id
cgroup_name=resman-yom-$run_id
cgroup_root=/sys/fs/cgroup/$cgroup_name
config_file=$work_root/resman.conf
map_file=$work_root/cpu-points.map
log_file=$work_root/resman.log
port=$((21000 + $$ % 8000))
resman_pid=
installed_service_active=
service_quiesced=0
had_crontab=0
session_id=
user_unit=resman-yom-user-$run_id.service
transient_unit=resman-yom-transient-$run_id.service
system_unit=resman-yom-system-$run_id.service
result=FAIL
detail="systemd ownership scenario did not complete"

case "$scenario" in
	systemd-ownership-preservation) ;;
	*) echo "invalid scenario: $scenario" >&2; exit 2 ;;
esac
case "$run_id" in
	*[!a-z0-9-]*|'') echo "invalid run id: $run_id" >&2; exit 2 ;;
esac

mkdir -p "$evidence_dir" "$work_root" "$helper_root"
chmod 0700 "$evidence_dir" "$work_root"
chmod 0755 "$helper_root"
exec > >(tee -a "$evidence_dir/runner.log") 2>&1

fail() {
	detail=$1
	echo "FAIL: $detail" >&2
	exit 1
}

user_systemctl() {
	runuser -u "$test_user" -- env \
		XDG_RUNTIME_DIR="/run/user/$test_uid" \
		DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$test_uid/bus" \
		systemctl --user "$@"
}

restore_crontab() {
	[[ -n $test_uid ]] || return 0
	if [[ $had_crontab -eq 1 ]]; then
		crontab -u "$test_user" "$work_root/original.crontab"
	else
		crontab -u "$test_user" -r >/dev/null 2>&1 || true
	fi
}

stop_workloads() {
	set +e
	if [[ -z $test_uid ]]; then
		set -e
		return 0
	fi
	crontab -u "$test_user" -r >/dev/null 2>&1 || true
	[[ -z $session_id ]] || loginctl terminate-session "$session_id" >/dev/null 2>&1 || true
	user_systemctl stop "$user_unit" >/dev/null 2>&1 || true
	user_systemctl disable "$user_unit" >/dev/null 2>&1 || true
	rm -f -- "/run/user/$test_uid/systemd/user/$user_unit"
	user_systemctl daemon-reload >/dev/null 2>&1 || true
	systemctl stop "$transient_unit" "$system_unit" >/dev/null 2>&1 || true
	rm -f -- "/run/systemd/system/$system_unit"
	systemctl daemon-reload >/dev/null 2>&1 || true
	for pid_file in "$pid_root"/*.pid; do
		[[ -r $pid_file ]] || continue
		pid=$(< "$pid_file")
		case "$pid" in *[!0-9]*|'') continue ;; esac
		kill -TERM "$pid" >/dev/null 2>&1 || true
	done
	set -e
}

finish() {
	local status=$? cleanup_status=PASS
	trap - EXIT INT TERM
	set +e
	for diagnostic in stdout stderr resman.log; do
		if [[ -f $work_root/$diagnostic ]]; then
			cp -- "$work_root/$diagnostic" "$evidence_dir/$diagnostic"
		fi
	done
	[[ -z $resman_pid ]] || kill -TERM "$resman_pid" >/dev/null 2>&1
	[[ -z $resman_pid ]] || wait "$resman_pid" >/dev/null 2>&1
	resman_pid=
	stop_workloads
	restore_crontab >/dev/null 2>&1 || cleanup_status=FAIL
	if [[ -n $test_uid ]]; then
		for path in "$cgroup_root/recovery/user_$test_uid" "$cgroup_root/recovery" "$cgroup_root"; do
			rmdir "$path" >/dev/null 2>&1 || true
		done
	fi
	[[ ! -e $cgroup_root ]] || cleanup_status=FAIL
	if [[ $service_quiesced -eq 1 && $installed_service_active == active ]]; then
		systemctl start resman >>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL
	fi
	case "$work_root" in
		/tmp/resman-final-work-"$run_id") rm -rf -- "$work_root" ;;
		*) cleanup_status=FAIL ;;
	esac
	case "$helper_root" in
		/tmp/resman-final-helper-"$run_id") rm -rf -- "$helper_root" ;;
		*) cleanup_status=FAIL ;;
	esac
	case "$pid_root" in
		/tmp/resman-final-pids-"$run_id") rm -rf -- "$pid_root" ;;
		*) cleanup_status=FAIL ;;
	esac
	if [[ $result == PASS && $cleanup_status != PASS ]]; then
		result=FAIL
		detail="ownership proof passed but cleanup failed"
		status=1
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

metric_value() {
	local metric=$1 file=$2
	awk -v metric="$metric" '
		index($1, metric) == 1 && (length($1) == length(metric) || substr($1, length(metric) + 1, 1) == "{") {
			print $2
			exit
		}
	' "$file"
}

pid_cgroup() {
	local pid=$1
	cut -d: -f3 "/proc/$pid/cgroup"
}

wait_pid_file() {
	local file=$1
	for _ in $(seq 1 90); do
		if [[ -s $file ]]; then
			pid=$(< "$file")
			[[ -d /proc/$pid ]] && return 0
		fi
		sleep 1
	done
	return 1
}

preflight() {
	[[ $(id -u) -eq 0 ]] || fail "scenario must run as root"
	[[ -x $binary ]] || fail "staged resman binary is missing"
	[[ -d /run/systemd/system ]] || fail "systemd runtime is unavailable"
	[[ $(stat -fc %T /sys/fs/cgroup) == cgroup2fs ]] || fail "cgroup v2 is unavailable"
	for command in crontab curl flock loginctl runuser setpriv systemctl; do
		command -v "$command" >/dev/null || fail "$command is required"
	done
	id "$test_user" >/dev/null 2>&1 || fail "fixture user $test_user is unavailable"
	test_uid=$(id -u "$test_user")
	test_gid=$(id -g "$test_user")
	install -d -o "$test_uid" -g "$test_gid" -m 0700 "$pid_root"
	[[ $test_uid -ge 1000 ]] || fail "fixture user must be non-system"
	[[ ! -e $cgroup_root ]] || fail "dedicated cgroup already exists"
	installed_service_active=$(systemctl is-active resman 2>&1 || true)
	if pgrep -x resman >/dev/null 2>&1; then
		[[ $installed_service_active == active ]] || fail "an unowned resman process is active"
		systemctl stop resman
		service_quiesced=1
	fi
	if crontab -u "$test_user" -l >"$work_root/original.crontab" 2>/dev/null; then
		had_crontab=1
	fi
	{
		printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		printf 'scenario=%s\nsource_revision=%s\n' "$scenario" "$source_revision"
		printf 'host=%s\nos=%s\nkernel=%s\n' \
			"$(hostname -f 2>/dev/null || hostname)" \
			"$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"')" \
			"$(uname -r)"
		printf 'test_user=%s\ntest_uid=%s\ninitial_service_active=%s\n' \
			"$test_user" "$test_uid" "$installed_service_active"
	} >"$evidence_dir/environment.txt"
}

write_helpers() {
	cat >"$helper_root/busy.sh" <<'EOF'
#!/usr/bin/env bash
set -eu
pid_file=$1
printf '%s\n' "$$" >"$pid_file"
exec sh -c 'while :; do :; done'
EOF
	cat >"$helper_root/pam-launch.sh" <<EOF
#!/usr/bin/env bash
set -eu
exec 9>"$pid_root/pam.lock"
flock -n 9 || exit 0
"$helper_root/busy.sh" "$pid_root/pam.pid"
EOF
	chmod 0755 "$helper_root/busy.sh" "$helper_root/pam-launch.sh"
}

start_pam_session() {
	local cron_file=$work_root/test.crontab
	if [[ $had_crontab -eq 1 ]]; then
		cp "$work_root/original.crontab" "$cron_file"
	else
		: >"$cron_file"
	fi
	printf '* * * * * %s\n' "$helper_root/pam-launch.sh" >>"$cron_file"
	crontab -u "$test_user" "$cron_file"
	wait_pid_file "$pid_root/pam.pid" || fail "cron/PAM session workload did not start"
	pam_pid=$(< "$pid_root/pam.pid")
	pam_cgroup=$(pid_cgroup "$pam_pid")
	case "$pam_cgroup" in
		/user.slice/user-"$test_uid".slice/session-*.scope) ;;
		*) fail "cron did not create a genuine PAM/logind session" ;;
	esac
	scope=${pam_cgroup##*/}
	session_id=${scope#session-}
	session_id=${session_id%.scope}
	[[ $(loginctl show-session "$session_id" -p Scope --value) == "$scope" ]] \
		|| fail "logind does not identify the workload session"
	printf 'pam_session=%s\npam_cgroup_before=%s\n' "$session_id" "$pam_cgroup" \
		>"$evidence_dir/ownership-identities.txt"
}

start_owned_units() {
	local user_dir=/run/user/$test_uid/systemd/user
	install -d -o "$test_uid" -g "$test_gid" -m 0700 "$user_dir"
	cat >"$user_dir/$user_unit" <<EOF
[Service]
Type=exec
ExecStart=$helper_root/busy.sh $pid_root/user-service.pid
EOF
	chown "$test_uid:$test_gid" "$user_dir/$user_unit"
	user_systemctl daemon-reload
	user_systemctl start "$user_unit"
	wait_pid_file "$pid_root/user-service.pid" || fail "user service did not start"

	systemd-run --quiet --unit="$transient_unit" --service-type=exec \
		--uid="$test_uid" --gid="$test_gid" \
		"$helper_root/busy.sh" "$pid_root/transient.pid"
	wait_pid_file "$pid_root/transient.pid" || fail "transient unit did not start"

	cat >"/run/systemd/system/$system_unit" <<EOF
[Unit]
Description=ResMan ownership containment test
[Service]
Type=exec
User=$test_user
Group=$test_gid
ExecStart=$helper_root/busy.sh $pid_root/system-service.pid
EOF
	systemctl daemon-reload
	systemctl start "$system_unit"
	wait_pid_file "$pid_root/system-service.pid" || fail "system service did not start"

	for kind in user-service transient system-service; do
		pid=$(< "$pid_root/$kind.pid")
		printf '%s_pid=%s\n%s_cgroup_before=%s\n' \
			"$kind" "$pid" "$kind" "$(pid_cgroup "$pid")" \
			>>"$evidence_dir/ownership-identities.txt"
	done
}

seed_recovery_occupant() {
	mkdir -p "$cgroup_root/recovery/user_$test_uid"
	setpriv --reuid="$test_uid" --regid="$test_gid" --init-groups \
		"$helper_root/busy.sh" "$pid_root/recovery.pid" &
	wait_pid_file "$pid_root/recovery.pid" || fail "recovery fixture did not start"
	recovery_pid=$(< "$pid_root/recovery.pid")
	printf '%s' "$recovery_pid" >"$cgroup_root/recovery/user_$test_uid/cgroup.procs"
	recovery_cgroup=$(pid_cgroup "$recovery_pid")
	[[ $recovery_cgroup == "/$cgroup_name/recovery/user_$test_uid" ]] \
		|| fail "recovery fixture did not enter its historical leaf"
	printf 'recovery_pid=%s\nrecovery_cgroup_before=%s\n' \
		"$recovery_pid" "$recovery_cgroup" >>"$evidence_dir/ownership-identities.txt"
}

start_resman() {
	printf '[resman-cpu-points-map-v1]\n%s=300\n' "$test_user" >"$map_file"
	chmod 0600 "$map_file"
	cat >"$config_file" <<EOF
CGROUP_ROOT=/sys/fs/cgroup
CGROUP_BASE=$cgroup_name
CREATED_CGROUPS_FILE=$work_root/cgroups.txt
LOG_FILE=$log_file
LOG_LEVEL=DEBUG
USE_SYSLOG=false
POLLING_INTERVAL=3
MIN_ACTIVE_TIME=1
METRICS_CACHE_TTL=1
METRICS_REFRESH_INTERVAL=3
PROCESS_MIN_AGE_SECONDS=0
IGNORE_SYSTEM_LOAD=true
CPU_THRESHOLD=1
CPU_RELEASE_THRESHOLD=0
CPU_THRESHOLD_DURATION=0
CPU_RESERVE_POINTS=100
CPU_BEST_EFFORT_POINTS=100
CPU_POINTS_FILE=$map_file
USER_INCLUDE_LIST=^$test_user\$
USER_EXCLUDE_LIST=root
RAM_LIMIT_ENABLED=false
IO_LIMIT_ENABLED=false
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
	"$binary" --config "$config_file" >"$work_root/stdout" 2>"$work_root/stderr" &
	resman_pid=$!
	for _ in $(seq 1 30); do
		kill -0 "$resman_pid" 2>/dev/null || fail "source resman exited during startup"
		grep -q 'Entering main control loop' "$log_file" 2>/dev/null && return 0
		sleep 1
	done
	fail "source resman did not enter its control loop"
}

prove_containment() {
	local metrics=$evidence_dir/prometheus.prom ready=0
	for _ in $(seq 1 40); do
		curl --fail --silent --show-error --max-time 2 \
			"http://127.0.0.1:$port/metrics" >"$metrics" || true
		if grep -q 'resman_enforcement_mode.*mode="observation_only_systemd".* 1$' "$metrics" \
			&& grep -q 'resman_cgroup_ingress_skipped_total.*reason="systemd_ownership_preserved"' "$metrics"; then
			ready=1
			break
		fi
		sleep 1
	done
	[[ $ready -eq 1 ]] || fail "typed systemd ownership refusal was not observed"

	for kind in pam user-service transient system-service recovery; do
		pid=$(< "$pid_root/$kind.pid")
		before=$(sed -n "s/^${kind}_cgroup_before=//p" "$evidence_dir/ownership-identities.txt")
		after=$(pid_cgroup "$pid")
		printf '%s_cgroup_after=%s\n' "$kind" "$after" >>"$evidence_dir/ownership-identities.txt"
		[[ $after == "$before" ]] || fail "$kind workload changed cgroup membership"
	done

	active=$(metric_value resman_actively_limited_users_count "$metrics")
	cpu_active=$(metric_value resman_cpu_actively_limited_users_count "$metrics")
	stranded=$(metric_value resman_recovery_stranded_processes "$metrics")
	observed=$(metric_value resman_cpu_eligible_users_cpu_usage_percent "$metrics")
	[[ ${active:-missing} == 0 && ${cpu_active:-missing} == 0 ]] \
		|| fail "observation-only mode reported an active limit"
	awk -v value="${stranded:-0}" 'BEGIN { exit !(value >= 1) }' \
		|| fail "live recovery occupant was not reported as stranded"
	awk -v value="${observed:-0}" 'BEGIN { exit !(value > 0) }' \
		|| fail "user observation did not continue"
	if find "$cgroup_root" -path '*/limited/*' -o -path '*/user_*' \
		| grep -v "/recovery/user_$test_uid$" | grep -q .; then
		fail "new enforcement membership was created"
	fi

	crontab -u "$test_user" -r >/dev/null 2>&1 || true
	loginctl terminate-session "$session_id"
	for _ in $(seq 1 20); do
		[[ ! -d /proc/$(< "$pid_root/pam.pid") ]] && break
		sleep 1
	done
	[[ ! -d /proc/$(< "$pid_root/pam.pid") ]] \
		|| fail "loginctl terminate-session did not reach the workload"

	cat >"$evidence_dir/systemd-ownership-summary.txt" <<'EOF'
pam_session=PASS
user_service=PASS
transient_unit=PASS
system_service=PASS
unchanged_membership=PASS
terminate_session=PASS
observation_continues=PASS
zero_active_limits=PASS
recovery_upgrade=PASS
enforcement_mode=observation_only_systemd
EOF
	result=PASS
	detail="systemd ownership remained authoritative and inherited recovery stayed stranded"
}

preflight
write_helpers
start_pam_session
start_owned_units
seed_recovery_occupant
start_resman
prove_containment
