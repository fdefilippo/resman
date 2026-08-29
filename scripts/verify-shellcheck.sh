#!/usr/bin/env bash
set -Eeuo pipefail

shellcheck_bin=${SHELLCHECK:-shellcheck}
require_shellcheck=${REQUIRE_SHELLCHECK:-0}

case "$require_shellcheck" in
	0 | 1) ;;
	*)
		printf 'REQUIRE_SHELLCHECK must be 0 or 1, got %q\n' "$require_shellcheck" >&2
		exit 2
		;;
esac

if ! command -v "$shellcheck_bin" >/dev/null 2>&1; then
	if [[ $require_shellcheck == 1 ]]; then
		printf 'ShellCheck is required by ci-quality in CI but was not found: %s\n' "$shellcheck_bin" >&2
		exit 1
	fi
	printf 'WARNING: ShellCheck is not installed; tracked shell scripts were not checked\n' >&2
	exit 0
fi

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf 'ShellCheck gate requires a Git worktree to enumerate shipped scripts\n' >&2
	exit 1
fi

scripts=()
while IFS= read -r -d '' path; do
	[[ -f $path ]] || continue
	first_line=
	IFS= read -r first_line <"$path" || true
	if [[ $path == *.sh || $first_line =~ ^#!.*[/[:space:]](ba|z|k)?sh([[:space:]]|$) ]]; then
		scripts+=("$path")
	fi
done < <(git ls-files -z)

if (( ${#scripts[@]} == 0 )); then
	printf 'ShellCheck gate found no tracked shell scripts\n' >&2
	exit 1
fi

"$shellcheck_bin" "${scripts[@]}"
