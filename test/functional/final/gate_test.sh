#!/usr/bin/env bash
set -Eeuo pipefail
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
bash -n "$script_dir/run.sh" "$script_dir/gate_test.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$script_dir/gate_test.py"
