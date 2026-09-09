#!/usr/bin/env sh
set -eu

go_bin=${GO:-go}
project_root=${PROJECT_ROOT:-$(pwd)}

project_root=$(realpath -m -- "$project_root")
for cache_name in GOCACHE GOMODCACHE; do
	cache_path=$("$go_bin" env "$cache_name")
	if [ -z "$cache_path" ]; then
		echo "$cache_name is empty; configure a writable cache outside $project_root" >&2
		exit 2
	fi
	cache_path=$(realpath -m -- "$cache_path")
	case "$cache_path" in
	"$project_root" | "$project_root"/*)
		echo "$cache_name resolves inside the ResMan worktree: $cache_path" >&2
		echo "Set GOCACHE and GOMODCACHE to writable directories outside $project_root" >&2
		exit 2
		;;
	esac
done

boundary_dir=$project_root/build
boundary=$boundary_dir/go.mod
required_go=$(awk '$1 == "go" { print $2; exit }' "$project_root/go.mod")
if [ -z "$required_go" ]; then
	echo "cannot determine the Go version from $project_root/go.mod" >&2
	exit 2
fi

mkdir -p "$boundary_dir"
temporary=$boundary_dir/.resman-go-module-boundary.$$
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
{
	echo "module github.com/fdefilippo/resman/build-artifacts"
	echo
	echo "go $required_go"
} > "$temporary"

if [ -f "$boundary" ] && cmp -s -- "$temporary" "$boundary"; then
	rm -f -- "$temporary"
else
	mv -f -- "$temporary" "$boundary"
fi
trap - EXIT HUP INT TERM
