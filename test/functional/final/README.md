# Final semantic regression gate

`make test-functional-final` is the final cross-package gate for the
`resman-4pw` audit. It composes focused boundary tests with disposable SmolVM
scenarios and produces one required scenario matrix under
`build/functional/final/<run-id>/`.

The gate always enumerates all required rows. A failed row produces `FAIL`; a
capability-dependent row without valid evidence produces `BLOCKED` and exit
status 77. Running only the scenarios supported by the local guest therefore
cannot produce a whole-gate PASS.

## Evidence classes

- `local-focused-test`: deterministic cross-package tests for resource state,
  every I/O decision dimension, sampling/cache ownership, reload publication,
  MCP 2026-07-28 stateless HTTP, Prometheus transitions, and database errors;
- `smolvm`: real cgroup membership, RAM and CPU quotas, process reconciliation,
  missing-controller startup, MCP reload, and the shipped rootful Podman
  contract in disposable 2-vCPU/2-GiB guests;
- `remote-real-kernel`: an explicitly selected disposable test host used only
  when the SmolVM kernel lacks PSI or `io.max`.

The last class is not a silent fallback. The SmolVM attempt remains in
`attempts.tsv` as `BLOCKED`, while the required row names the substitute's host,
kernel, source revision, isolation path, exact commands, and cleanup result.
Evidence from another revision is rejected.

## Running the complete gate

The current SmolVM 1.9.0 kernel lacks PSI and `io.max`, so provide a
non-production root test host with both capabilities:

```bash
RESMAN_REAL_KERNEL_HOST=root@terra make test-functional-final
```

The remote runner refuses to start while another `resman` process is active. It
copies the current clean revision's binary and runner into a unique directory
below `/tmp`, creates only uniquely named cgroups, retrieves the evidence, and
removes the remote directory. Each scenario verifies that its cgroup is absent
after shutdown. The configured fixture user defaults to `pippo` and can be
overridden with `RESMAN_REAL_KERNEL_USER`.

Previously collected evidence may be supplied explicitly instead:

```bash
FINAL_GATE_PSI_EVIDENCE=/path/to/current/psi \
FINAL_GATE_BLOCK_IO_EVIDENCE=/path/to/current/block-io \
make test-functional-final
```

Both directories must identify the current Git revision and pass the same
structural checks as newly collected evidence.

Use `make test-functional-final-unit` without KVM to prove that a missing
capability, a missing substitute, or a failed required scenario cannot be
reported as PASS.
