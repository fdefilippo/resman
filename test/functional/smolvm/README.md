# SmolVM functional harness

This harness runs resman in a disposable microVM with systemd as PID 1 and a
writable cgroup v2 hierarchy. It is the integration boundary for CPU, memory,
I/O, MCP, Prometheus, database, reload, and cgroup-membership scenarios.

## Run it

```bash
make test-functional-smolvm
```

The default `resource-only` scenario requires CPU, memory, and I/O interfaces.
The process-membership boundary can be run independently on guests that expose
only the CPU interface:

```bash
make test-functional-smolvm-process-membership
```

The CPU capability boundary builds a nested delegated cgroup root that exposes
`cpu` but not `cpuset`, then proves that resman starts, emits the optional
degradation diagnostic, moves the workload, and applies a finite `cpu.max`. It
stops resman while the limited workload is still alive, verifies that the PID
start time did not change, and requires the process to finish in a valid
recovery leaf with a successful service result:

```bash
make test-functional-smolvm-cpu-without-cpuset
```

The SmolVM 1.9.0 kernel lists the I/O controller but does not expose `io.max`.
Use that environment to verify that an enabled I/O feature fails closed during
startup and names the feature, controller, and interface:

```bash
make test-functional-smolvm-missing-io-startup
```

The `missing-io-startup` scenario is `BLOCKED` rather than passed when `io.max`
is available or when another required interface is also missing, because either
condition would not reproduce the intended boundary unambiguously. Its startup
rejection is the only currently expected daemon error and is declared by a
scenario-specific pattern naming I/O limiting, the `io` controller, and `io.max`.

The MCP filter reload boundary uses stdio and only the CPU controller, so it does
not require guest networking, TLS material, `memory.max`, or `io.max`:

```bash
make test-functional-smolvm-mcp-filter-reload
```

It calls the real `set_user_include_list` tool using MCP 2026-07-28, requires a
response that confirms both persistence and runtime application, verifies the
file, and proves that the removed `reload` input is rejected without side effects.

The container-runtime boundary builds the shipped image with CGO/NSS enabled,
loads it into rootful Podman inside the guest, and runs the documented host-wide
contract. It proves local host-user resolution, trustworthy `/proc/PID/exe`
access for a foreign UID, finite `cpu.max` enforcement, and clean release on
container shutdown:

```bash
make test-functional-smolvm-container-runtime
```

The scenario uses the same 2-vCPU/2-GiB guest defaults. The VM network remains
disabled: the shipped image archive is mounted from the host scratch directory,
so a registry pull inside the guest cannot be mistaken for runtime evidence.

The target builds the guest image with `sudo podman` and invokes every
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
`BLOCKED`. PSI is required only by a scenario that enables it, matching the
feature-capability contract:

```bash
SMOLVM_REQUIRE_PSI=1 make test-functional-smolvm
```

If PSI is unavailable, that command is `BLOCKED`/77. The default fixture has
`PSI_EVENT_DRIVEN=false`, so it records unavailable PSI without misclassifying
the unrelated systemd/cgroup/Prometheus/database smoke test.

## Isolation

Each run gets a unique ID. The ID scopes all of the following:

- the SmolVM machine and locally built image tag;
- the guest configuration and systemd instance;
- the cgroup root and cgroup tracking file;
- the SQLite database, logs, and runtime state;
- the evidence directory.

The VM has no network and publishes no host ports by default. Prometheus uses
guest-local port `19100`; the MCP fixture reserves guest-local port `19101` and
is disabled until an MCP scenario explicitly enables it. Tests reach both
through `smolvm machine exec`, so concurrent guests cannot collide on host
ports. Every cgroup mutation occurs in the guest.

CPU and RAM allocations can be overridden explicitly:

```bash
SMOLVM_CPUS=4 SMOLVM_MEMORY_MIB=4096 make test-functional-smolvm
```

The `container-runtime` scenario keeps the standard two CPU and 2 GiB memory
allocation, but defaults the writable overlay to 8 GiB. Nested Podman uses its
`vfs` storage driver because SmolVM does not expose `/dev/fuse`; the larger
overlay accommodates the driver's full layer copies. Set
`SMOLVM_OVERLAY_GIB` explicitly to override this storage-only default.
The host runner also retains `localhost/resman-container-cache:latest` after a
successful product-image build. Per-run tags are still removed, while the stable
cache tag keeps the Oracle Linux package and Go build layers available to later
runs. Override its name with `RESMAN_CONTAINER_IMAGE_CACHE_REF` when needed.

The image provides three fixture users (`resman-cpu`, `resman-memory`, and
`resman-io`), a small wrapper around `stress` for CPU/RAM/I/O workloads,
`curl`, and `sqlite3`. Workloads are launched inside the guest with:

```bash
/opt/resman-functional/workload.sh cpu 15s
/opt/resman-functional/workload.sh memory 15s
/opt/resman-functional/workload.sh io 15s
```

The default semantic scenario leaves CPU eligibility empty, runs CPU, memory,
and I/O workloads, and requires the RAM/I/O users to appear in standalone
cgroups with `cpu.max=max 100000`. It also rejects a run that creates the
finite shared CPU cgroup. The exact controller values are exported as
`resource-only-cgroups.txt`.

The `mcp-filter-reload` scenario runs an observed CPU workload without applying
limits, then verifies that the stdio tool, resource, and prompt distinguish
observed users from actively limited users and omit the removed metric/status
aliases. It also verifies acknowledged filter persistence/runtime application
and explicit rejection of the removed `reload` input.

The `process-membership` scenario activates CPU enforcement for `resman-cpu`,
starts a second process after activation, and requires the next control cycles
to move it into the existing user cgroup. It then reloads
`PROCESS_EXCLUDE_LIST`, verifies that both `stress` processes return to their
captured origin while a differently named CPU workload keeps the user actively
limited, and places a third `stress` process directly in the limited cgroup
without a recorded origin. The persistent fail-closed error must leave only that
process constrained, restore its valid peers, complete the later cycle stages,
and export the distinct `origin_unavailable` error signal. The scenario then
removes the exclusion and verifies that all three processes are in the limited
cgroup. Evidence is exported as `process-membership.txt` and
`process-membership-metrics.txt`.

The guest probes the actual `cpu.max`, `memory.max`, and `io.max` interfaces in
a disposable child cgroup. A controller that is merely listed in
`cgroup.controllers` is insufficient. If the SmolVM kernel lacks a required
interface, the run is `BLOCKED`/77 and cannot be cited as passing evidence.

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
  output, cgroup tree, and SQLite schema;
- standalone RAM/IO cgroup paths and their CPU, memory, and I/O controller values;
- sustained-active process-membership origin and reconciliation results when that
  scenario is selected;
- the CPU-only delegated hierarchy, optional cpuset diagnostic, applied quota,
  and limited process membership for the `cpu-without-cpuset` scenario;
- MCP stdio request/response evidence for typed observation/runtime status,
  acknowledged filter persistence, and runtime application when that scenario
  is selected;
- the shipped image identity, dynamic libc linkage, root runtime user, exact
  Podman inspection, host NSS result, foreign-user executable, finite CPU quota,
  and post-shutdown cgroup for the container-runtime scenario;
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
