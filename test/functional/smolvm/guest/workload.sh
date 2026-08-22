#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
    echo "usage: $0 {cpu|memory|io} [duration]" >&2
    exit 2
}

scenario=${1:-}
duration=${2:-15s}

case "$scenario" in
    cpu)
        exec runuser -u resman-cpu -- resman-functional-workload cpu "$duration"
        ;;
    memory)
        exec runuser -u resman-memory -- resman-functional-workload memory "$duration"
        ;;
    io)
        workdir=/var/lib/resman-functional/workloads/resman-io
        install -d -o resman-io -g resman-io -m 0700 "$workdir"
        cd "$workdir"
        exec runuser -u resman-io -- resman-functional-workload io "$duration"
        ;;
    *)
        usage
        ;;
esac
