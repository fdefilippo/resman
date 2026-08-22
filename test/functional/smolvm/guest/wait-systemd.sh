#!/usr/bin/env bash
set -Eeuo pipefail

deadline=$((SECONDS + 55))
until systemctl show-environment >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
        echo "systemd bus did not become ready within 55 seconds" >&2
        exit 1
    fi
    sleep 0.2
done

state=$(systemctl is-system-running --wait || true)
case "$state" in
    running|degraded)
        ;;
    *)
        echo "systemd reached unexpected state: $state" >&2
        exit 1
        ;;
esac
