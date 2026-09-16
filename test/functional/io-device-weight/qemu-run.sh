#!/usr/bin/env bash
set -Eeuo pipefail

scenario=${1:?scenario is required}
run_id=${2:?run ID is required}
source_revision=${3:?source revision is required}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

case "$scenario" in
	io-device-weight-matrix)
		exec "$script_dir/qemu-host.sh" "$run_id" "$source_revision" matrix
		;;
	io-device-weight-ol8-uek-attribution)
		exec "$script_dir/qemu-host.sh" "$run_id" "$source_revision" ol8-uek-attribution
		;;
	*)
		echo "invalid QEMU scenario: $scenario" >&2
		exit 2
		;;
esac
