#!/usr/bin/env bash
set -Eeuo pipefail
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
if [[ ${1:-} == systemd-native-weighted-io-adapter ]]; then
	exec python3 "$script_dir/native_weighted_io.py" "$@"
fi
if [[ ${1:-} == systemd-native-reconciliation ]]; then
	exec python3 "$script_dir/native_reconciliation.py" "$@"
fi
if [[ ${1:-} == native-package-acceptance ]]; then
	exec python3 "$script_dir/native_package.py" "$@"
fi
if [[ ${1:-} == systemd-native-coverage ]]; then
	exec python3 "$script_dir/native_coverage.py" "$@"
fi
if [[ ${1:-} == systemd-native-recovery ]]; then
	exec python3 "$script_dir/native_recovery.py" "$@"
fi
if [[ ${1:-} == systemd-native-reference || ${1:-} == systemd-native-reference-pinned || ${1:-} == systemd-native-reference-pinned-six ]]; then
	exec python3 "$script_dir/native_reference.py" "$@"
fi
if [[ ${1:-} == systemd-native-proportional ]]; then
	exec python3 "$script_dir/native_proportional.py" "$@"
fi
exec python3 "$script_dir/native_gate.py" "$@"
