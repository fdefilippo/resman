#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-smolvm-host-test.XXXXXX")
trap 'rm -rf -- "$tmp_dir"' EXIT

for script in "$script_dir/run.sh" "$script_dir/host_test.sh" \
	"$script_dir/daemon_errors_test.sh" \
	"$script_dir/guest/assert-daemon-errors.sh" \
    "$script_dir/guest/run-functional.sh" "$script_dir/guest/wait-systemd.sh" \
    "$script_dir/guest/workload.sh"; do
    bash -n "$script"
done

"$script_dir/daemon_errors_test.sh"

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

repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
for ignored in .git/ .beads/ .dolt/ build/ rpmbuild/; do
	grep -Fxq "$ignored" "$repo_root/.containerignore" || {
		echo ".containerignore does not exclude $ignored" >&2
		exit 1
	}
done
git -C "$repo_root" check-ignore -q build/functional/smolvm/evidence || {
	echo "functional evidence is not ignored by git" >&2
	exit 1
}

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

preflight_evidence_root=$tmp_dir/preflight-evidence
set +e
(
	evidence_root=$preflight_evidence_root
	# shellcheck disable=SC2329 # Invoked indirectly by run_harness.
	require_host_capabilities() { blocked "simulated missing KVM"; }
	run_harness
) >/dev/null 2>&1
preflight_status=$?
set -e
[[ $preflight_status -eq 77 ]] || {
	echo "failed harness preflight returned $preflight_status, want 77" >&2
	exit 1
}
mapfile -t preflight_results < <(find "$preflight_evidence_root" -mindepth 2 \
	-maxdepth 2 -type f -name result -print)
[[ ${#preflight_results[@]} -eq 1 ]] || {
	echo "failed harness preflight retained ${#preflight_results[@]} results, want 1" >&2
	exit 1
}
[[ $(< "${preflight_results[0]}") == BLOCKED ]]
preflight_environment=${preflight_results[0]%/result}/environment.txt
grep -q '^cleanup=PASS$' "$preflight_environment"
grep -q '^result=BLOCKED$' "$preflight_environment"
grep -q '^exit_code=77$' "$preflight_environment"

failed_pre_guest_root=$tmp_dir/failed-pre-guest-evidence
set +e
(
	set -e
	# shellcheck disable=SC2034 # Read by run_harness from the sourced harness.
	evidence_root=$failed_pre_guest_root
	# shellcheck disable=SC2329 # Invoked indirectly by run_harness.
	require_host_capabilities() { return 42; }
	run_harness
) >/dev/null 2>&1
failed_pre_guest_status=$?
set -e
[[ $failed_pre_guest_status -eq 42 ]] || {
	echo "failed pre-guest setup returned $failed_pre_guest_status, want 42" >&2
	exit 1
}
mapfile -t failed_pre_guest_results < <(find "$failed_pre_guest_root" -mindepth 2 \
	-maxdepth 2 -type f -name result -print)
[[ ${#failed_pre_guest_results[@]} -eq 1 ]]
[[ $(< "${failed_pre_guest_results[0]}") == FAIL ]]
failed_pre_guest_environment=${failed_pre_guest_results[0]%/result}/environment.txt
grep -q '^cleanup=PASS$' "$failed_pre_guest_environment"
grep -q '^result=FAIL$' "$failed_pre_guest_environment"
grep -q '^exit_code=42$' "$failed_pre_guest_environment"

missing_machine_evidence=$tmp_dir/missing-machine-evidence
mkdir "$missing_machine_evidence"
evidence_dir=$missing_machine_evidence
vm_name=never-created
vm_started=1
# shellcheck disable=SC2329 # Invoked indirectly by cleanup_resources.
run_kvm() {
	echo "machine '$vm_name' not found" >&2
	return 1
}
cleanup_resources || {
	echo "an absent VM made cleanup fail" >&2
	exit 1
}
[[ $vm_started -eq 0 ]]
grep -Eq 'machine .* not found' "$missing_machine_evidence/cleanup.log"

failed_cleanup_evidence=$tmp_dir/failed-cleanup-evidence
mkdir "$failed_cleanup_evidence"
evidence_dir=$failed_cleanup_evidence
vm_name=partially-created
vm_started=1
# shellcheck disable=SC2329 # Invoked indirectly by cleanup_resources.
run_kvm() {
	echo "permission denied while deleting $vm_name" >&2
	return 1
}
if cleanup_resources; then
	echo "an ambiguous VM cleanup failure was reported as success" >&2
	exit 1
fi
[[ $vm_started -eq 0 ]]
grep -q 'permission denied' "$failed_cleanup_evidence/cleanup.log"

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
grep -q 'container_image_cache_ref' "$script_dir/run.sh"
grep -q 'podman tag.*container_image_cache_ref' "$script_dir/run.sh"
grep -q 'SMOLVM_REQUIRE_PSI' "$script_dir/run.sh"
grep -q 'SMOLVM_SCENARIO' "$script_dir/run.sh"
grep -q 'pressure_summary cpu' "$script_dir/guest/run-functional.sh"
grep -q 'memory-only' "$script_dir/guest/run-functional.sh"
grep -q 'psi-refresh-neutrality' "$script_dir/guest/run-functional.sh"
grep -q 'limit-hook-executor' "$script_dir/guest/run-functional.sh"
render_line=$(grep -n 'sed "s/@RUN_ID@/' "$script_dir/guest/run-functional.sh" | head -n 1 | cut -d: -f1)
# shellcheck disable=SC2016 # Match the literal shell condition in the guest runner.
memory_config_line=$(grep -n 'if \[\[ \$scenario == memory-only \]\]' \
	"$script_dir/guest/run-functional.sh" | tail -n 1 | cut -d: -f1)
[[ $render_line -lt $memory_config_line ]] || {
	echo "memory-only config mutation precedes fixture rendering" >&2
	exit 1
}
grep -q 'process-membership' "$script_dir/guest/run-functional.sh"
grep -q 'cpu-without-cpuset' "$script_dir/guest/run-functional.sh"
grep -q 'missing-io-startup' "$script_dir/guest/run-functional.sh"
grep -q 'mcp-filter-reload' "$script_dir/guest/run-functional.sh"
grep -q 'block-iops' "$script_dir/guest/run-functional.sh"
grep -q 'io.stat wios' "$script_dir/guest/run-functional.sh"
grep -q 'controller-startup-rejection.txt' "$script_dir/guest/run-functional.sh"
grep -q 'expected_daemon_error_patterns' "$script_dir/guest/run-functional.sh"
grep -q 'assert-daemon-errors.sh' "$script_dir/guest/run-functional.sh"
grep -q '^EnvironmentFile=/run/resman-functional/%i/environment$' \
    "$script_dir/guest/resman-functional@.service"

echo "PASS: SmolVM host harness contract"
