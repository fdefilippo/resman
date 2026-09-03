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
- `smolvm`: missing-controller startup, MCP reload, and other contracts that do
  not claim migration enforcement on a systemd host, in disposable
  2-vCPU/2-GiB guests;
- `remote-real-kernel`: an explicitly selected laboratory host used when the
  SmolVM kernel lacks PSI and for the mandatory systemd ownership-preservation
  proof using a genuine PAM/logind session and representative unit types.

The last class is not a silent fallback. The SmolVM attempt remains in
`attempts.tsv` as `BLOCKED`, while the required row names the substitute's host,
kernel, source revision, isolation path, exact commands, and cleanup result.
Evidence from another revision is rejected.
The generated summary embeds `systemd-containment-dispositions.tsv`. That inventory
accounts for every former real-kernel or migration-dependent scenario. Assertions
that require systemd-native enforcement are visibly displaced to `resman-nq6`; they
cannot be cited as current containment evidence.

## Running the complete gate

The current SmolVM 1.9.0 kernel lacks PSI. Provide a non-production root test host
with working PAM/logind session scopes:

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

The systemd ownership scenario is bound to the source revision rather than the
installed RPM. It temporarily quiesces an active packaged service and restores it
from the cleanup trap. It creates a genuine cron/PAM login session, a user service,
a transient unit, and a system service, then proves unchanged membership, continued
observation, zero active-limit state, and `loginctl terminate-session` reachability.
It also begins with a live historical recovery occupant and proves that the process
remains stranded and is never silently admitted again.

Previously collected evidence may be supplied explicitly instead:

```bash
FINAL_GATE_PSI_EVIDENCE=/path/to/current/psi \
FINAL_GATE_SYSTEMD_OWNERSHIP_EVIDENCE=/path/to/current/systemd-ownership \
make test-functional-final
```

Both directories must identify the current Git revision and pass the same
structural checks as newly collected evidence.

Use `make test-functional-final-unit` without KVM to prove that a missing
capability, a missing substitute, or a failed required scenario cannot be
reported as PASS.
