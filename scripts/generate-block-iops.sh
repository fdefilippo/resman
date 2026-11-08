#!/bin/sh
# Rebuild the embedded kernel program and its source/object integrity manifest.
set -eu
cd "$(dirname "$0")/../internal/blockiops"
"${CLANG:-clang}" -O2 -g -target bpf -mcpu=v3 -fdebug-compilation-dir=. -c bpf/collector.c -o bpf/collector_bpfel.o
sha256sum bpf/collector.c bpf/collector_bpfel.o > bpf/SHA256SUMS
