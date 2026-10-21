#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
scratch=$(mktemp -d)
cleanup() {
	find "$scratch" -depth -delete
}
trap cleanup EXIT INT TERM

config_file="$scratch/resman.conf"
cat >"$config_file" <<'EOF'
CPU_THRESHOLD=50 # activation
CPU_RELEASE_THRESHOLD=25 # release
EOF

plain_log="$scratch/resman.log"
cat >"$plain_log" <<'EOF'
[2026-10-20 23:59:59] [INFO] Control cycle completed cycle_id=1 requested_policy_intent=maintain applied_enforcement_action=maintain total_cpu_usage=10 cpu_eligible_users_cpu_usage=40 cpu_eligible_users_host_cpu_percent=10
[2026-10-21 00:00:01] [INFO] unrelated message
[2026-10-21 00:00:02] [INFO] Control cycle completed cycle_id=2 requested_policy_intent=maintain applied_enforcement_action=maintain total_cpu_usage=70 cpu_eligible_users_cpu_usage=160 cpu_eligible_users_host_cpu_percent=40
EOF

compressed_log="$scratch/resman.log.1.gz"
cat >"$scratch/compressed-source.log" <<'EOF'
[2026-10-21 00:00:03] [INFO] Control cycle completed cycle_id=3 requested_policy_intent=activate applied_enforcement_action=activate total_cpu_usage=90 cpu_eligible_users_cpu_usage=240 cpu_eligible_users_host_cpu_percent=60
EOF
gzip -c "$scratch/compressed-source.log" >"$compressed_log"

output=$(
	"$script_dir/analyze-control-cycles.sh" \
		--config "$config_file" \
		--date 2026-10-21 \
		"$plain_log" "$compressed_log"
)

expected_lines=(
	"Period: 2026-10-21 00:00:02 to 2026-10-21 00:00:03"
	"Online CPUs: 4"
	"Control cycles: 2"
	"Activation threshold                            50.00         2.00           200.00"
	"Maximum eligible-user CPU observed              60.00         2.40           240.00"
	"Release threshold                               25.00         1.00           100.00"
	"Maximum total host CPU: 90.00% at 2026-10-21 00:00:03"
	"Samples at or above activation threshold: 1"
	"Requested activation cycles: 1"
	"Applied activation cycles: 1"
)
for expected in "${expected_lines[@]}"; do
	grep -Fqx -- "$expected" <<<"$output" || {
		printf 'missing expected output line: %s\n' "$expected" >&2
		printf '%s\n' "$output" >&2
		exit 1
	}
done

override_output=$(
	"$script_dir/analyze-control-cycles.sh" \
		--config "$config_file" \
		--cpu-threshold 60 \
		--release-threshold 30 \
		--date 2026-10-21 \
		"$plain_log" "$compressed_log"
)
grep -Fq 'Activation threshold                            60.00' <<<"$override_output"
grep -Fq 'Release threshold                               30.00' <<<"$override_output"

zero_log="$scratch/zero.log"
cat >"$zero_log" <<'EOF'
[2026-10-21 00:00:04] [INFO] Control cycle completed cycle_id=4 requested_policy_intent=maintain applied_enforcement_action=maintain total_cpu_usage=0 cpu_eligible_users_cpu_usage=0 cpu_eligible_users_host_cpu_percent=0
EOF
zero_output=$(
	"$script_dir/analyze-control-cycles.sh" \
		--cpu-threshold 50 \
		--release-threshold 25 \
		--cpus 8 \
		"$zero_log"
)
grep -Fqx 'Online CPUs: 8' <<<"$zero_output"

malformed_log="$scratch/malformed.log"
cat >"$malformed_log" <<'EOF'
[2026-10-21 00:00:05] [INFO] Control cycle completed cycle_id=5 requested_policy_intent=maintain applied_enforcement_action=maintain total_cpu_usage=80 cpu_eligible_users_host_cpu_percent=20
EOF
if "$script_dir/analyze-control-cycles.sh" \
	--cpu-threshold 50 \
	--release-threshold 25 \
	--cpus 4 \
	"$malformed_log" >"$scratch/malformed.out" 2>&1; then
	printf 'malformed completed cycle was accepted\n' >&2
	exit 1
fi
grep -Fq 'invalid or missing cpu_eligible_users_cpu_usage' "$scratch/malformed.out"

empty_log="$scratch/empty.log"
printf '%s\n' '[2026-10-21 00:00:06] [INFO] unrelated message' >"$empty_log"
if "$script_dir/analyze-control-cycles.sh" \
	--cpu-threshold 50 \
	--release-threshold 25 \
	--cpus 4 \
	"$empty_log" >"$scratch/empty.out" 2>&1; then
	printf 'input without completed cycles was accepted\n' >&2
	exit 1
fi
grep -Fq 'no completed control cycles matched the input' "$scratch/empty.out"

corrupt_log="$scratch/corrupt.log.gz"
printf '%s\n' 'not gzip data' >"$corrupt_log"
if "$script_dir/analyze-control-cycles.sh" \
	--cpu-threshold 50 \
	--release-threshold 25 \
	--cpus 4 \
	"$corrupt_log" >"$scratch/corrupt.out" 2>&1; then
	printf 'corrupt gzip log was accepted\n' >&2
	exit 1
fi
grep -Fq 'gzip-compressed log is invalid' "$scratch/corrupt.out"

printf 'control-cycle log summary tests passed\n'
