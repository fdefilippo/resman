#!/usr/bin/env bash
set -Eeuo pipefail

run_id=${1:?run id is required}
allocated_cpus=${2:?allocated CPU count is required}
allocated_memory_mib=${3:?allocated memory is required}
require_psi=${4:?PSI requirement flag is required}
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

mkdir -p "$artifact_dir" "$runtime_dir" "$state_dir"
chmod 0700 "$runtime_dir" "$state_dir"
exec > >(tee -a "$artifact_dir/guest.log") 2>&1

result=FAIL
detail="guest harness did not complete"

finish() {
    local status=$?
    set +e
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
for controller in cpu memory io; do
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
    printf 'psi_cpu=%s\n' "$(pressure_summary cpu)"
    printf 'psi_memory=%s\n' "$(pressure_summary memory)"
    printf 'psi_io=%s\n' "$(pressure_summary io)"
    printf 'allocated_cpus=%s\n' "$allocated_cpus"
    printf 'allocated_memory_mib=%s\n' "$allocated_memory_mib"
    printf 'guest_observed_cpus=%s\n' "$(nproc)"
    printf 'guest_observed_memory_kib=%s\n' "$(awk '/MemTotal:/ {print $2}' /proc/meminfo)"
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
[[ -d /sys/fs/cgroup/resman-functional-$run_id ]] || fail "isolated cgroup root was not created"

systemctl status "$service" --no-pager >"$artifact_dir/resman-status.txt"
result=PASS
detail="systemd, cgroup v2, Prometheus, and SQLite smoke checks passed"
echo "PASS: $detail"
