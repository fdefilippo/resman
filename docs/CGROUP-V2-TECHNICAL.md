# Cgroup v2 Technical Reference for ResMan

## Overview

ResMan 1.38.0 uses cgroup v2 through authoritative systemd units. It never creates a
parallel enforcement hierarchy and never changes process membership. systemd and
logind remain the owners of sessions, services, transient units, and rootless
container descendants.

The only enforcing mode is `systemd_native`. If the systemd authority boundary is
missing or cannot be verified, ResMan selects `observation_only`: metrics and
decisions remain available, but no resource property is changed.

## Authoritative topology

```text
/sys/fs/cgroup/
└── user.slice/                 finite CPU pool
    ├── user-0.slice/           explicit lendable root entitlement
    ├── user-1000.slice/        mapped guarantee
    │   ├── session-N.scope/    membership remains owned by logind
    │   └── user@1000.service/  child services and rootless descendants
    └── user-1001.slice/        best-effort participant
```

The native adapter discovers unit identities over D-Bus, applies runtime-only
systemd properties, reads the normalized property back, and verifies the
corresponding kernel interface before publishing success.

### Enterprise Linux 8 and systemd 239

EL8 enforcement requires the unified cgroup v2 hierarchy. The configured cgroup
root must be a real directory; a symbolic-link root fails closed because every
path component is opened without following links.

The verified systemd 239 contract exposes `ControlGroup` and `InvocationID` but
may omit `ControlGroupId`. ResMan resolves the validated `ControlGroup` path below
the cgroup root, uses the kernel cgroup inode as the durable identity, and
reconfirms the complete unit identity around each read. On newer systemd,
`ControlGroupId` remains an additional mandatory cross-check against that kernel
identity. A missing, mismatched, or changing identity is never authoritative.

Before selecting `systemd_native`, ResMan creates a reserved disposable slice and
proves the configured CPU, memory, and I/O property path through application,
kernel readback, and exact restoration. The probe is capability-based and never
selects behavior from a systemd version number.

## CPU control

### Parent capacity

ResMan applies the finite pool to `user.slice` using `CPUQuota` and
`CPUQuotaPeriodSec`. The pool is derived from the verified online-CPU capacity and
`CPU_RESERVE_POINTS`. The reserve protects workloads outside `user.slice`,
including `system.slice`; it does not exempt root login sessions.

`CPU_ROOT_POINTS` gives `user-0.slice` an explicit lendable scheduling
entitlement. ResMan never writes a CPU quota to `user-0.slice`.

### User weights

Mapped guarantees, root, and the aggregate best-effort entitlement are converted to
exact integer `CPUWeight` values. Best-effort slices receive an even integer
partition whose members differ by at most one. Unused capacity remains lendable
among runnable siblings.

CPU Points are scheduling weights, not dedicated cores or physical isolation.
Realized delivery also depends on runnable-thread placement, affinity, and external
workloads. See [CPU Points observability](CPU-POINTS-OBSERVABILITY.md) and
[placement methodology](CPU-POINTS-PLACEMENT.md).

## Memory control

When RAM limiting is enabled and authority is complete, ResMan applies
`MemoryMax` and, when configured, `MemoryHigh` to the authoritative user slice.
The adapter accounts for kernel page-size normalization before confirming the
effective value.

Memory charges remain with the authoritative hierarchy because no PID moves between
cgroups. If an observed UID is split across parents or contains a runtime-owned
descendant that makes authority incomplete, ResMan releases only its owned memory
properties and reports partial or refused coverage.

`memory.high` throttles allocation. On a host without reclaimable pages or swap, a
process can remain alive but make almost no progress before reaching
`memory.max`; a hard-limit OOM kill is not guaranteed merely because both values
are configured.

## I/O control

Strong I/O limits are applied as per-device systemd properties and verified against
`io.max`. The device identity is part of the property lease and exact restoration
contract.

Weighted I/O is a dedicated continuous policy over typed per-device
`IODeviceWeight` assignments; scalar `IOWeight` is not a production policy knob.
Read-only classification selects a candidate from stable device topology and an
active scheduler or io.cost mechanism. An owned transient unit then proves systemd
transport, controller materialization, exact per-device kernel readback, reset, and
cleanup before production mutation. Programmed, read-back, functionally accepted,
effect qualified, partial coverage, and observed delivery remain separate states.

## Property ownership and restoration

Runtime property ownership is recorded in the private journal described by
[SYSTEMD-PROPERTY-LEASES.md](SYSTEMD-PROPERTY-LEASES.md). The adapter:

- writes intent durably before mutation;
- confirms systemd and kernel state before acknowledging application;
- reclaims exact leases after restart or unit recreation;
- compares the current footprint before restoration;
- refuses destructive cleanup when operator state diverges;
- completes interrupted revert/reload sequences idempotently.

An unchanged verified plan produces no D-Bus or journal write. A root-only topology
releases the finite parent pool.

## Observation contract

ResMan reads process and system metrics from `/proc` and cgroup accounting from the
authoritative unit paths. Missing observations are represented as unavailable, never
as fabricated zeroes. Unit recreation, counter decrease, daemon restart, and identity
changes reset delta baselines.

The current SQLite schema is version 10; schema 7 migrates atomically through schemas
8 and 9 to schema 10 with historically disabled weighted-I/O fields and provenance
`none`. Prometheus,
MCP, and SQLite consume the same
typed control-cycle snapshot and the same bounded enforcement modes:

- `systemd_native`
- `observation_only`

## Prohibited architecture

Production code must not:

- write `cgroup.procs`;
- create a ResMan-owned enforcement or recovery hierarchy;
- record process origins or attempt PID restoration;
- expose `migration_enabled` as a mode or compatibility alias;
- acknowledge enforcement without authoritative systemd application and readback.

The architectural source gate enforces the first and fourth prohibitions. The
non-systemd SmolVM scenario proves observation-only behavior, unchanged PID
membership, and absence of a managed ResMan hierarchy.

When event-driven PSI observation is enabled, ResMan writes a poll selector to the
kernel `cpu.pressure` or `io.pressure` interface. That registration does not alter a
controller property, PID membership, scheduling weight, quota, or resource limit and
is outside the prohibited enforcement mutations above.

## References

- [Kernel cgroup v2 documentation](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html)
- [systemd resource control](https://www.freedesktop.org/software/systemd/man/latest/systemd.resource-control.html)
- [Systemd property leases](SYSTEMD-PROPERTY-LEASES.md)
- [CPU Points observability](CPU-POINTS-OBSERVABILITY.md)
- [Architecture](ARCHITECTURE.md)

---

**Document version:** 3.1
**Last updated:** 2026-09-11
**Applies to:** ResMan 1.38.0 and later
