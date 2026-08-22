#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-smolvm-host-test.XXXXXX")
trap 'rm -rf -- "$tmp_dir"' EXIT

for script in "$script_dir/run.sh" "$script_dir/host_test.sh" \
    "$script_dir/guest/run-functional.sh" "$script_dir/guest/wait-systemd.sh" \
    "$script_dir/guest/workload.sh"; do
    bash -n "$script"
done

cat >"$tmp_dir/sg-ok" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$SG_TEST_LOG"
EOF
cat >"$tmp_dir/sg-fail" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod 0755 "$tmp_dir/sg-ok" "$tmp_dir/sg-fail"

export RESMAN_SMOLVM_LIBRARY_ONLY=1
export SMOLVM_BIN=/opt/test/smolvm
export SG_BIN=$tmp_dir/sg-ok
export SG_TEST_LOG=$tmp_dir/sg.log
# shellcheck disable=SC1091
source "$script_dir/run.sh"

run_kvm machine status --name contract-test
mapfile -t sg_args <"$SG_TEST_LOG"
[[ ${sg_args[0]} == kvm ]]
[[ ${sg_args[1]} == -c ]]
[[ ${sg_args[2]} == *'/opt/test/smolvm machine status --name contract-test'* ]]

SG_BIN=$tmp_dir/sg-fail
if (kvm_device=$tmp_dir/missing-kvm; [[ -n $kvm_device ]]; require_host_capabilities) >/dev/null 2>&1; then
    echo "missing KVM capability was reported as success" >&2
    exit 1
else
    status=$?
    [[ $status -eq 77 ]] || {
        echo "missing KVM capability returned $status, want 77" >&2
        exit 1
    }
fi

signal_evidence=$tmp_dir/signal-evidence
mkdir "$signal_evidence"
printf 'PASS\n' >"$signal_evidence/result"
set +e
(
    # shellcheck disable=SC2034 # Read by cleanup through the EXIT trap.
    evidence_dir=$signal_evidence
    # shellcheck disable=SC2329 # Invoked indirectly by cleanup through the EXIT trap.
    cleanup_resources() { return 0; }
    trap cleanup EXIT
    interrupt 143
)
signal_status=$?
set -e
[[ $signal_status -eq 143 ]] || {
    echo "interruption returned $signal_status, want 143" >&2
    exit 1
}
[[ $(< "$signal_evidence/result") == FAIL ]]
grep -q '^cleanup=PASS$' "$signal_evidence/environment.txt"
grep -q '^exit_code=143$' "$signal_evidence/environment.txt"

grep -q '^CGROUP_BASE=resman-functional-@RUN_ID@$' "$script_dir/fixtures/resman.conf"
grep -q '^METRICS_DB_PATH=.*/@RUN_ID@/metrics.db$' "$script_dir/fixtures/resman.conf"
grep -q '^PROMETHEUS_METRICS_BIND_PORT=19100$' "$script_dir/fixtures/resman.conf"
grep -q '^MCP_HTTP_PORT=19101$' "$script_dir/fixtures/resman.conf"
grep -q '^POLLING_INTERVAL=5$' "$script_dir/fixtures/resman.conf"
grep -q '^METRICS_REFRESH_INTERVAL=5$' "$script_dir/fixtures/resman.conf"
grep -q '^PROCESS_EXCLUDE_LIST=' "$script_dir/fixtures/resman.conf"
grep -q '^FROM docker.io/amd64/oraclelinux:9$' "$script_dir/Containerfile.base"
# shellcheck disable=SC2016 # Match the literal Containerfile build argument.
grep -q '^FROM \${FUNCTIONAL_BASE_IMAGE}$' "$script_dir/Containerfile"
grep -q '^CMD \["/sbin/init"\]$' "$script_dir/Containerfile.base"
grep -q 'fixture_image_reused' "$script_dir/run.sh"
grep -q 'SMOLVM_REQUIRE_PSI' "$script_dir/run.sh"
grep -q 'SMOLVM_SCENARIO' "$script_dir/run.sh"
grep -q 'pressure_summary cpu' "$script_dir/guest/run-functional.sh"
grep -q 'process-membership' "$script_dir/guest/run-functional.sh"
grep -q 'missing-io-startup' "$script_dir/guest/run-functional.sh"
grep -q 'controller-startup-rejection.txt' "$script_dir/guest/run-functional.sh"
grep -q '^EnvironmentFile=/run/resman-functional/%i/environment$' \
    "$script_dir/guest/resman-functional@.service"

echo "PASS: SmolVM host harness contract"
