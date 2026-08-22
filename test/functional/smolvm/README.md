# SmolVM functional harness

This harness runs resman in a disposable microVM with systemd as PID 1 and a
writable cgroup v2 hierarchy. It is the integration boundary for CPU, memory,
I/O, MCP, Prometheus, database, reload, and cgroup-membership scenarios.

## Run it

```bash
make test-functional-smolvm
```

The target builds the guest image with `sudo podman` and invokes every
KVM-dependent SmolVM command through `sg kvm -c`. Starting a new login shell is
not required after adding the user to the `kvm` group.

The guest is derived from `docker.io/amd64/oraclelinux:9`. The derived image
adds the CGO-enabled resman binary, systemd, SQLite/curl diagnostics, fixture
users, and the `stress` workload tool from `ol9_developer_EPEL`. The repository
definition comes from Oracle Linux's `oracle-epel-release-el9` package. Both
the base digest and the derived image ID are recorded in the evidence.

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

The guest probes the actual `cpu.max`, `memory.max`, and `io.max` interfaces in
a disposable child cgroup. A controller that is merely listed in
`cgroup.controllers` is insufficient. If the SmolVM kernel lacks a required
interface, the run is `BLOCKED`/77 and cannot be cited as passing evidence.

## Evidence and cleanup

Evidence is written to `build/functional/smolvm/<run-id>/` by default. Set
`SMOLVM_EVIDENCE_ROOT` to export it elsewhere. It includes:

- SmolVM version and image reference/digest;
- exact host commands, including `sg kvm` and `sudo podman`;
- guest kernel, systemd PID 1, cgroup mount and controllers;
- requested and observed CPU/RAM;
- isolated paths and guest-local endpoints;
- systemd journal/status, resman log, process snapshot, initial/final Prometheus
  output, cgroup tree, and SQLite schema;
- standalone RAM/IO cgroup paths and their CPU, memory, and I/O controller values;
- a final `PASS`, `BLOCKED`, or `FAIL` result.

Host-side failures that happen before the guest runner starts are also written
as `FAIL` (or `BLOCKED` for exit status 77); a stale `RUNNING` marker is never a
completed result.

The EXIT/INT/TERM trap stops and deletes the named VM, removes the unique image
tag, and removes the validated temporary directory. Evidence is intentionally
retained. A successful process exit is rejected unless the guest also exported
an explicit `PASS` result.
