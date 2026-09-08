#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
assert_script=$script_dir/guest/assert-daemon-errors.sh
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-daemon-errors-test.XXXXXX")
trap 'rm -rf -- "$tmp_dir"' EXIT

new_case() {
	local name=$1
	local path=$tmp_dir/$name
	mkdir "$path"
	printf '%s\n' "$path"
}

clean_case=$(new_case clean)
bash "$assert_script" clean "$clean_case"
grep -q '^observed_error_count=0$' "$clean_case/daemon-error-summary.txt"

injected_case=$(new_case injected)
printf '[2026-08-24 00:00:00] [ERROR] deliberately injected daemon failure\n' \
	>"$injected_case/resman.log"
if bash "$assert_script" injected "$injected_case"; then
	echo "an injected daemon error did not fail the assertion" >&2
	exit 1
fi
grep -Fq 'deliberately injected daemon failure' \
	"$injected_case/unexpected-daemon-errors.txt"

expected_case=$(new_case expected)
printf '%s\n' \
	'Failed to initialize systemd-native enforcement: enabled feature I/O limiting requires controller "io" interface "io.max"' \
	>"$expected_case/controller-startup-rejection.txt"
printf '%s\n' \
	'[2026-08-24 00:00:00] [ERROR] Failed to initialize systemd-native enforcement error=enabled feature I/O limiting requires controller \"io\" interface \"io.max\"' \
	>"$expected_case/resman.log"
expected_pattern='Failed to initialize systemd-native enforcement.*I/O limiting.*controller "io".*interface "io.max"'
bash "$assert_script" missing-io-startup "$expected_case" "$expected_pattern"
grep -Fq "expected_pattern=$expected_pattern" \
	"$expected_case/daemon-error-expectations.txt"
[[ ! -s $expected_case/unexpected-daemon-errors.txt ]]
grep -q '^observed_error_count=2$' "$expected_case/daemon-error-summary.txt"

missing_case=$(new_case missing)
if bash "$assert_script" missing-io-startup "$missing_case" "$expected_pattern"; then
	echo "a missing declared daemon error did not fail the assertion" >&2
	exit 1
fi
grep -Fq "$expected_pattern" "$missing_case/missing-daemon-error-expectations.txt"

journal_case=$(new_case journal)
printf '%s\n' \
	'Aug 24 00:00:00 guest resman[42]: Warning: Error during cleanup: device or resource busy' \
	>"$journal_case/resman-journal.log"
if bash "$assert_script" journal "$journal_case"; then
	echo "an unexpected daemon journal error did not fail the assertion" >&2
	exit 1
fi
grep -Fq 'Error during cleanup' "$journal_case/unexpected-daemon-errors.txt"

systemd_case=$(new_case systemd-only)
printf '%s\n' \
	"Aug 24 00:00:00 guest systemd[1]: service: Failed with result 'exit-code'." \
	>"$systemd_case/resman-journal.log"
bash "$assert_script" systemd-only "$systemd_case"
grep -q '^observed_error_count=0$' "$systemd_case/daemon-error-summary.txt"

echo "PASS: daemon error assertion contract"
