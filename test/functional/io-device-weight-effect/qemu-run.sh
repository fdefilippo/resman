#!/usr/bin/env bash
set -Eeuo pipefail

scenario=${1:?scenario is required}
run_id=${2:?run ID is required}
qualification_revision=${3:?qualification revision is required}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

[[ $scenario == io-device-weight-effect ]] \
	|| { echo "invalid QEMU effect scenario: $scenario" >&2; exit 2; }
exec "$script_dir/qemu-host.sh" "$run_id" "$qualification_revision"
