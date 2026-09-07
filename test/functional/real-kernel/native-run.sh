#!/usr/bin/env bash
set -Eeuo pipefail
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
if [[ ${1:-} == systemd-native-reference ]]; then
	exec python3 "$script_dir/native_reference.py" "$@"
fi
if [[ ${1:-} == systemd-native-proportional ]]; then
	exec python3 "$script_dir/native_proportional.py" "$@"
fi
exec python3 "$script_dir/native_gate.py" "$@"
