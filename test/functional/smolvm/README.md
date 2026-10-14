# SmolVM functional harness

This harness runs resman in a disposable microVM with systemd as PID 1 and a
writable cgroup v2 hierarchy. It covers the boundaries that a microVM proves
without a real distribution kernel: fail-closed startup on a missing interface,
the MCP filter reload contract, PSI refresh neutrality, and observation on a
host whose PID 1 is not systemd.

Every scenario must be named explicitly with `SMOLVM_SCENARIO`; there is no
default scenario.

## Scenarios

The SmolVM 1.9.0 kernel lists the I/O controller but does not expose `io.max`.
Use that environment to verify that an enabled I/O feature fails closed during
startup and names the feature, controller, and interface:

```bash
make test-functional-smolvm-missing-io-startup
```

The `missing-io-startup` scenario is `BLOCKED` rather than passed when `io.max`
is available or when another required interface is also missing, because either
condition would not reproduce the intended boundary unambiguously. Its startup
rejection is the only expected daemon error and is declared by a
scenario-specific pattern naming I/O limiting, the `io` controller, and `io.max`.

The MCP filter reload boundary uses stdio and only the CPU controller, so it does
not require guest networking, TLS material, `memory.max`, or `io.max`:

```bash
make test-functional-smolvm-mcp-filter-reload
```

It runs an observed CPU workload without applying limits, calls the real
`set_user_include_list` tool using MCP 2026-07-28, requires a response that
confirms both persistence and runtime application, verifies the file, and proves
that the removed `reload` input is rejected without side effects. The stdio
tool, resource, and prompt must distinguish observed users from actively limited
users and omit the removed metric and status aliases.

The PSI contract scenario enables event-driven mode, establishes a decision
sample, and then requires two observation refreshes to change the system
snapshot without advancing the per-user decision CPU or EMA:

```bash
make test-functional-smolvm-psi-refresh
```

It returns `BLOCKED`/77 when the guest kernel lacks PSI. The final matrix may
satisfy that same required contract with current-revision evidence from an
explicit disposable real-kernel host, but retains the blocked SmolVM attempt as
separate evidence.

The `non-systemd-observation` scenario is run by the final gate through
`SMOLVM_SCENARIO=non-systemd-observation test/functional/smolvm/run.sh run`.

## Retired scenarios

The `resource-only`, `memory-only`, `process-membership`, `cpu-without-cpuset`,
`container-runtime`, `block-iops`, `limit-hook-executor` and
`host-cpu-sampling-cadence` scenarios asserted the retired PID-relocation
hierarchy: a ResMan-owned `resman-functional-<run id>` cgroup root, a `limited/`
subtree and process-membership writes. No production code creates that
hierarchy since systemd-native enforcement replaced relocation, so they could
only fail. Real-kernel campaigns on disposable QEMU guests replace them,
notably `test/functional/early-boot` for systemd-native enforcement at boot on
EL8, EL9 and EL10. `host_test.sh` rejects any return of the retired hierarchy or
of those scenario names.

## Running the harness

The targets build the guest image with `sudo podman` and invoke every
KVM-dependent SmolVM command through `sg kvm -c`. Starting a new login shell is
not required after adding the user to the `kvm` group.

The guest is derived from `docker.io/amd64/oraclelinux:9`. The derived image
adds systemd, SQLite/curl diagnostics, fixture users, and the `stress` workload
tool from `ol9_developer_EPEL`. The repository definition comes from Oracle
Linux's `oracle-epel-release-el9` package. This package-and-user fixture is a
persistent local image keyed by the hash of `Containerfile.base`; ordinary runs
reuse it and rebuild only the cached Go builder plus the thin layer containing
the new CGO-enabled resman binary, scripts, and config. Both the upstream base
digest, fixture image ID/reuse status, and per-run image ID are recorded in the
evidence. The per-run image is removed during cleanup, while fixture images are
retained until their definition changes or the operator removes them.

Use the cheaper checks while developing the harness:

```bash
make test-functional-smolvm-preflight  # no build and no guest
make test-functional-smolvm-unit       # no KVM required
```

Exit status `77` means **blocked by a missing capability**, not pass. In
particular, group membership does not compensate for a missing `/dev/kvm`, and
the preflight requires the device to be a readable and writable character
device from inside `sg kvm`. The guest preflight requires cgroup v2 and records
CPU, memory, and I/O PSI availability.

SmolVM supplies its own kernel and does not boot the OCI image through GRUB.
Running `grubby --update-kernel=ALL
--args="systemd.unified_cgroup_hierarchy=1 psi=1"` inside the Oracle Linux
rootfs would therefore not change the kernel SmolVM actually boots. The harness
checks the effective contract instead and records `/proc/cmdline`, the cgroup v2
mount, controllers, and `/proc/pressure/*`. Missing cgroup v2 is always
`BLOCKED`. PSI is required only by a scenario that enables it
(`SMOLVM_REQUIRE_PSI=1`, set by the PSI target).

The guest probes the actual `cpu.max`, `memory.max`, and `io.max` interfaces in
a disposable child cgroup that the harness creates and removes itself. A
controller that is merely listed in `cgroup.controllers` is insufficient.

## Isolation

Each run gets a unique ID. The ID scopes the SmolVM machine and locally built
image tag, the guest configuration and systemd instance, the SQLite database,
logs and runtime state, and the evidence directory.

The VM has no network and publishes no host ports by default. Prometheus uses
guest-local port `19100`; the MCP fixture reserves guest-local port `19101` and
is disabled until an MCP scenario explicitly enables it. Tests reach both
through `smolvm machine exec`, so concurrent guests cannot collide on host
ports. Every cgroup mutation occurs in the guest.

CPU and RAM allocations can be overridden explicitly:

```bash
SMOLVM_CPUS=4 SMOLVM_MEMORY_MIB=4096 make test-functional-smolvm-mcp-filter-reload
```

The image provides fixture users and a small wrapper around `stress`:

```bash
/opt/resman-functional/workload.sh cpu 15s
```

## Evidence and cleanup

Evidence is written to `build/functional/smolvm/<run-id>/` by default. Set
`SMOLVM_EVIDENCE_ROOT` to export it elsewhere. It includes:

- SmolVM version, upstream image digest, persistent fixture identity/reuse
  status, and per-run image identity;
- exact host commands, including `sg kvm` and `sudo podman`;
- guest kernel, systemd PID 1, cgroup mount and controllers;
- requested and observed CPU/RAM;
- isolated paths and guest-local endpoints;
- systemd journal/status, resman log, process snapshot, initial/final Prometheus
  output, and SQLite schema;
- MCP stdio request/response evidence for typed observation/runtime status,
  acknowledged filter persistence, and runtime application when that scenario
  is selected;
- the complete startup rejection naming feature, controller, and interface for
  the missing-I/O capability scenario;
- declared daemon-error expectations, every observed error-level line,
  unexpected errors, and missing expectations. Any unexpected daemon error or
  declared error that does not occur changes the scenario result to `FAIL`;
- a final `PASS`, `BLOCKED`, or `FAIL` result.

Host-side failures that happen before the guest runner starts are also written
as `FAIL` (or `BLOCKED` for exit status 77); a stale `RUNNING` marker is never a
completed result.

The repository `.containerignore` keeps `.git/`, `.beads/`, `.dolt/`, `build/`,
and `rpmbuild/` out of every root-context `sudo podman build`. Functional evidence
is also ignored by Git and remains available only as a local test artifact.

The EXIT/INT/TERM trap stops and deletes the named VM, removes the unique image
tag, and removes the validated temporary directory. Evidence is intentionally
retained. A successful process exit is rejected unless the guest also exported
an explicit `PASS` result.
