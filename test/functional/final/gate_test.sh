#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-final-gate-test.XXXXXX")
trap 'rm -rf -- "$tmp_dir"' EXIT

for script in "$script_dir/run.sh" "$script_dir/gate_test.sh" \
	"$repo_root/test/functional/real-kernel/run.sh" \
	"$repo_root/test/functional/real-kernel/remote.sh"; do
	bash -n "$script"
done

cat >"$tmp_dir/fake-go" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat >"$tmp_dir/fake-smolvm" <<'EOF'
#!/usr/bin/env bash
set -eu
dir=$SMOLVM_EVIDENCE_ROOT/fake
mkdir -p "$dir"
if [[ ${FAKE_FAIL_SCENARIO:-} == "$SMOLVM_SCENARIO" ]]; then
	printf 'FAIL\n' >"$dir/result"
	printf 'detail=simulated scenario failure\n' >"$dir/environment.txt"
	exit 1
fi
case "$SMOLVM_SCENARIO" in
	block-iops|psi-refresh-neutrality)
		printf 'BLOCKED\n' >"$dir/result"
		printf 'detail=simulated missing guest capability\n' >"$dir/environment.txt"
		exit 77
		;;
	*)
		printf 'PASS\n' >"$dir/result"
		printf 'detail=simulated functional proof\n' >"$dir/environment.txt"
		;;
esac
EOF

cat >"$tmp_dir/fake-real-kernel" <<EOF
#!/usr/bin/env bash
set -eu
scenario=\$1
revision=\$(git -C "$repo_root" rev-parse HEAD)
dir=\$REAL_KERNEL_EVIDENCE_ROOT/fake-\$scenario
mkdir -p "\$dir"
printf 'PASS\n' >"\$dir/result"
printf 'scenario=%s\nsource_revision=%s\ncleanup=PASS\n' "\$scenario" "\$revision" >"\$dir/environment.txt"
if [[ \$scenario == psi-refresh-neutrality ]]; then
	cat >"\$dir/psi-refresh-neutrality.txt" <<'PROOF'
psi_available=true
psi_event_driven_active=true
control_cycles_before=2
control_cycles_after=2
decision_cpu_before=10
decision_cpu_after=10
decision_ema_before=5
decision_ema_after=5
system_observation_before=20
system_observation_after=1
PROOF
else
	cat >"\$dir/block-io-summary.txt" <<'PROOF'
io_max_available=true
cached_and_socket_false_activation=PASS
read_bps=PASS
write_bps=PASS
read_iops=PASS
write_iops=PASS
PROOF
fi
EOF
chmod 0755 "$tmp_dir/fake-go" "$tmp_dir/fake-smolvm" "$tmp_dir/fake-real-kernel"

run_gate() {
	local root=$1
	shift
	env FINAL_GATE_ALLOW_DIRTY=1 \
		GO_BIN="$tmp_dir/fake-go" \
		FINAL_GATE_SMOLVM_RUNNER="$tmp_dir/fake-smolvm" \
		FINAL_GATE_REAL_KERNEL_RUNNER="$tmp_dir/fake-real-kernel" \
		FINAL_GATE_EVIDENCE_ROOT="$root" "$@" "$script_dir/run.sh"
}

pass_root=$tmp_dir/pass
run_gate "$pass_root" env RESMAN_REAL_KERNEL_HOST=fake.example >/dev/null
pass_dir=$(find "$pass_root" -mindepth 1 -maxdepth 1 -type d | head -n 1)
[[ $(< "$pass_dir/result") == PASS ]]
[[ $(wc -l <"$pass_dir/matrix.tsv") -eq 14 ]]
grep -q $'^block-io\tPASS\tremote-real-kernel\t' "$pass_dir/matrix.tsv"
grep -q $'^psi-refresh-neutrality\tPASS\tremote-real-kernel\t' "$pass_dir/matrix.tsv"
grep -q $'^block-io-smolvm\tBLOCKED\t' "$pass_dir/attempts.tsv"
grep -q $'^psi-refresh-neutrality-smolvm\tBLOCKED\t' "$pass_dir/attempts.tsv"

blocked_root=$tmp_dir/blocked
set +e
run_gate "$blocked_root" env -u RESMAN_REAL_KERNEL_HOST >/dev/null
blocked_status=$?
set -e
[[ $blocked_status -eq 77 ]]
blocked_dir=$(find "$blocked_root" -mindepth 1 -maxdepth 1 -type d | head -n 1)
[[ $(< "$blocked_dir/result") == BLOCKED ]]
grep -q '^blocked_rows=2$' "$blocked_dir/environment.txt"
[[ $(wc -l <"$blocked_dir/matrix.tsv") -eq 14 ]]

failed_root=$tmp_dir/failed
set +e
run_gate "$failed_root" env RESMAN_REAL_KERNEL_HOST=fake.example \
	FAKE_FAIL_SCENARIO=process-membership >/dev/null
failed_status=$?
set -e
[[ $failed_status -eq 1 ]]
failed_dir=$(find "$failed_root" -mindepth 1 -maxdepth 1 -type d | head -n 1)
[[ $(< "$failed_dir/result") == FAIL ]]
grep -q $'^process-membership\tFAIL\t' "$failed_dir/matrix.tsv"

echo "PASS: final semantic gate contract"
