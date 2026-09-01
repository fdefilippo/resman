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
- `remote-real-kernel`: an explicitly selected laboratory host used when the
  SmolVM kernel lacks PSI or `io.max`, and for the mandatory CPU Points
  proportional-allocation proof that needs synchronized 60-second windows.

The last class is not a silent fallback. The SmolVM attempt remains in
`attempts.tsv` as `BLOCKED`, while the required row names the substitute's host,
kernel, source revision, isolation path, exact commands, and cleanup result.
Evidence from another revision is rejected.
The generated summary also names the exact kernel used for the CPU Points
proportional measurement and scopes that scheduler result to the running kernel;
it does not generalize the measurement to untested scheduler families.

## Running the complete gate

The current SmolVM 1.9.0 kernel lacks PSI and `io.max`; CPU Points also needs a
stable real scheduler. Provide a non-production root test host with those
capabilities:

```bash
RESMAN_REAL_KERNEL_HOST=root@terra make test-functional-final
```

The remote runner serializes ownership of the host. General source scenarios
refuse to start while another `resman` process is active; the CPU Points
scenario explicitly quiesces and later restores the installed unit. The runner
copies the current clean revision's binary and script into a unique directory
below `/tmp`, creates only uniquely named cgroups, retrieves the evidence, and
removes the remote directory. Each scenario verifies that its cgroup is absent
after shutdown. The configured fixture user defaults to `pippo` and can be
overridden with `RESMAN_REAL_KERNEL_USER`.

The CPU Points scenario is bound to the source revision rather than the
installed RPM. It temporarily quiesces an active packaged service, restores it
from the cleanup trap, and builds two independent raw-cgroup oracle hierarchies
beside the ResMan-owned hierarchy. Its production-valid equality vector is 300,
300 and 200 mapped points plus the aggregate 100-point best-effort entitlement
inside a 900-point parent. Evidence contains the correct and deliberately
stale-low 60-second samples, cross-cgroup read skew, parent throttling, live
reload preservation, partial process coverage, release and shutdown recovery.
CPU hotplug is reported as `BLOCKED` unless a separate run explicitly opts into
that host mutation; it is never silently inferred from an unchanged topology.

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
