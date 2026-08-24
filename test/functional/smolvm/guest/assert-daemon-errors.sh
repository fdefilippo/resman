#!/usr/bin/env bash
set -Eeuo pipefail

scenario=${1:?scenario is required}
artifact_dir=${2:?artifact directory is required}
shift 2
expected_patterns=("$@")

errors_file=$artifact_dir/daemon-errors.txt
unexpected_file=$artifact_dir/unexpected-daemon-errors.txt
missing_file=$artifact_dir/missing-daemon-error-expectations.txt
expectations_file=$artifact_dir/daemon-error-expectations.txt
summary_file=$artifact_dir/daemon-error-summary.txt

: >"$errors_file"
: >"$unexpected_file"
: >"$missing_file"
{
	printf 'scenario=%s\n' "$scenario"
	printf 'expected_pattern_count=%d\n' "${#expected_patterns[@]}"
	for pattern in "${expected_patterns[@]}"; do
		printf 'expected_pattern=%s\n' "$pattern"
	done
} >"$expectations_file"

append_structured_log_errors() {
	local source=$1
	local path=$2
	[[ -f $path ]] || return 0
	awk -v source="$source" 'index($0, "] [ERROR] ") { print source ":" NR ":" $0 }' \
		"$path" >>"$errors_file"
}

append_journal_errors() {
	local source=$1
	local path=$2
	[[ -f $path ]] || return 0
	awk -v source="$source" \
		'$0 ~ / resman\[[0-9]+\]: / && $0 ~ /(ERROR|Error|FAILED|Failed|FATAL|Fatal|PANIC|Panic|panic)/ { print source ":" NR ":" $0 }' \
		"$path" >>"$errors_file"
}

append_direct_stderr_errors() {
	local source=$1
	local path=$2
	[[ -f $path ]] || return 0
	awk -v source="$source" \
		'$0 ~ /(ERROR|Error|FAILED|Failed|FATAL|Fatal|PANIC|Panic|panic)/ { print source ":" NR ":" $0 }' \
		"$path" >>"$errors_file"
}

append_structured_log_errors resman.log "$artifact_dir/resman.log"
append_journal_errors resman-journal.log "$artifact_dir/resman-journal.log"
append_direct_stderr_errors controller-startup-rejection.txt \
	"$artifact_dir/controller-startup-rejection.txt"
append_direct_stderr_errors mcp-filter-reload.stderr \
	"$artifact_dir/mcp-filter-reload.stderr"

matched_patterns=()
for _ in "${expected_patterns[@]}"; do
	matched_patterns+=(0)
done

while IFS= read -r line; do
	[[ -n $line ]] || continue
	# Structured log fields JSON-escape embedded quotes, while direct stderr does
	# not. Match both representations against the same scenario declaration.
	match_line=${line//'\"'/'"'}
	matched=false
	for index in "${!expected_patterns[@]}"; do
		if [[ $match_line =~ ${expected_patterns[$index]} ]]; then
			matched_patterns[index]=1
			matched=true
		fi
	done
	if [[ $matched == false ]]; then
		printf '%s\n' "$line" >>"$unexpected_file"
	fi
done <"$errors_file"

for index in "${!expected_patterns[@]}"; do
	if [[ ${matched_patterns[index]} -eq 0 ]]; then
		printf '%s\n' "${expected_patterns[$index]}" >>"$missing_file"
	fi
done

observed_count=$(wc -l <"$errors_file")
unexpected_count=$(wc -l <"$unexpected_file")
missing_count=$(wc -l <"$missing_file")
{
	printf 'scenario=%s\n' "$scenario"
	printf 'observed_error_count=%d\n' "$observed_count"
	printf 'unexpected_error_count=%d\n' "$unexpected_count"
	printf 'missing_expected_error_count=%d\n' "$missing_count"
} >"$summary_file"

if [[ $unexpected_count -ne 0 || $missing_count -ne 0 ]]; then
	exit 1
fi
