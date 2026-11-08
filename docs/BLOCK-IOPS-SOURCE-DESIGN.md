# Certified block-IOPS decision source

Issue: `resman-82p`. Owner decisions: retain the IOPS dimensions and preserve
`PROCESS_EXCLUDE_LIST`; use certified lower and upper bounds for operations whose
origin cannot be proved. No process relocation is permitted.

## Accounting and selection

The embedded CO-RE programs count read/write BIOs at `blk_cgroup_bio_start`, the
accounting boundary that increments `io.stat` operation counters. Device identity
comes from `bi_bdev->bd_disk` (major and first minor), not a partition's raw device
number. Ancestor lookup attributes a descendant operation to its registered
systemd user slice. `IOAccounting=true` is acquired through the existing durable
property lease before registering that unit; no finite quota is needed to count.

Accounting and provenance are separate. The source captures the executable
basename from the current task's `mm->exe_file` only inside a synchronous
`iomap_dio_submit_bio` frame. Checked executable-name reads reject truncation,
dynamic dentry renderers and unproved name normalization. Kernel and io_uring
worker contexts do not provide an issuer identity. Synchronous clones made by
`bio_alloc_clone` inherit provenance only from the same task and unique live
frame cookie. Evicted provenance, other submission paths, buffered writeback,
missing identity, and unproved splits are unresolved. Neither `comm`, `argv[0]`
nor the executable of a writeback worker supplies policy identity.

Each unit/device/direction has cumulative selected, excluded and unresolved
counts. With no process exclusions every operation is selected. Otherwise an
unresolved operation contributes zero to the certainly selected minimum and one
to the possibly selected maximum. This includes a page written by both selected
and excluded processes. The source does not claim to reconstruct its exact
executable composition. Exclusions change decision inputs, not membership in an
enforced user slice.

## Certified intervals

Samples bracket counter reads with the monotonic clock. Source generation,
frozen executable policy, watch registration, UID, systemd invocation, cgroup ID and configured sysfs
device lifetime must match. A new eligible user, recreated slice/device, reload
of the process policy, reset or missing baseline warms up as unavailable.

For previous read `[a,b]` and current read `[c,d]`, `b<c`:

- Activation minimum = selected counter growth / `(d-a)`.
- Release maximum = (selected + unresolved growth) / `(c-b)`.

The outer and inner windows deliberately differ: counter reads are not atomic
clock instants. Across users, counts are added only after choosing a common outer
window and common inner window. Integer growth and aggregation check overflow;
rational-to-float conversion rounds outwards. Quota comparisons use exact integer
ratios; floating-point percentages appear only in diagnostics. Devices are compared independently,
so the same traffic through LVM and its backing disk is never added together.
The denominator remains the configured per-user quota times all eligible users.
Activation is any-of across enabled directions/devices/bandwidth; release is
all-of from complete upper bounds. An unresolved origin is compatible with a
certified below-threshold upper bound; a missing source or user baseline is not.

Systemd's hard-I/O device lookup can map a logical volume or partition to its
originating whole disk. The source and hard-limit verifier use that same boundary
(single originating layer, whole-disk normalization); aliases of one device are
deduplicated. Different values for aliases of the same effective device are
rejected. The weighted-I/O device contract is independent.

Decision collection owns baselines. Prometheus/blackout observation refreshes
project cached decision evidence and cannot advance them. Releasing all owned
properties can disable I/O accounting, so the following interval warms up again.
An unrelated configuration reload reuses the source. A process-policy/device
change prepares a new source before publication; the prior source remains alive
until commit or rollback finishes.

## Integrity and lifecycle

Programs, links and maps are private, unpinned descriptors owned by the daemon.
Health checks bracket reads, including program recursion misses and link state.
Map insertion loss, checked accounting-read failure, ancestor-depth overflow
(64 levels), counter overflow or a detached program invalidates the source
permanently. Operation counters use a bounded non-evicting hash (32,768 rows);
provenance caches may evict only towards unresolved attribution. Watches are
bounded (4,096) and retired when authority/eligibility ends. Cumulative rows are
retained for the source lifetime, so exhaustion is an explicit failure requiring
a fresh source, not an implicit counter reset.

A mandatory source that cannot load rejects startup before readiness. A failed
reload preflight preserves the running configuration/source. A runtime integrity
failure prevents decisions, invalidates exported IOPS evidence and attempts the
normal compare-before-restore of all owned CPU/RAM/I/O properties. An external
conflict remains explicit with its durable lease; it is never overwritten to
claim successful recovery. Recovery requires a fresh source/restart. A source
failure is not a perpetual unavailable zero beside pinned active limits.

`CAP_BPF` and `CAP_PERFMON` are added to the packaged service. `CAP_SYS_ADMIN` is
not requested. The native component is tested with the service's privilege and
sandbox restrictions. The Go reader serializes kernel I/O with an operation gate,
never a state mutex. No probes are loaded on the development host.

## Qualified profiles and operator compatibility

This backend intentionally accepts only executed kernel builds:

- Oracle Linux 9 RHCK `5.14.0-687.66.1.el9_8.x86_64`.
- Oracle Linux 9 UEK `6.12.0-204.92.4.2.el9uek.x86_64`.

BTF function prototypes/field layouts and the recursion-miss ABI are also checked
at startup. A matching architecture or kernel version family is insufficient:
these are private kernel functions. Other builds, including EL8/EL10, are
explicitly unsupported for enabled IOPS until characterized. CPU/RAM and
bandwidth-only policies do not load this backend. This is a truthful capability
boundary for the previously missing source, not a fallback to syscall counts or
unfiltered slice totals. Operators retaining IOPS must use a qualified build and
the packaged capabilities; do not disable dimensions as an automatic migration.

The prior exact aggregate Prometheus series cannot faithfully represent these
bounds. Nonzero or unavailable exact values are absent. Measured exact idle may
still publish zero; consumers should migrate to per-device bound series and
coverage/uncertainty gauges described in `IO-LIMITS.md`. Configuration keys and
hard limits retain their meaning. This operational/metrics contract change uses
the minor release increment; no database schema change is required.

## Reproduction and evidence

Ordinary tests never attach BPF. Native Go tests require both the `resman_native`
build tag and `RESMAN_NATIVE_DISPOSABLE_GUEST=resman-82p`. Run only inside an
explicitly disposable VM, with systemd, cgroup v2, BTF, fio, Python and coreutils.
The state test expects UID 1000 and the OL9 image's `/dev/dm-0`; device numbers
are discovered inside the guest because they differ between RHCK and UEK.

```
go test -c -tags resman_native ./internal/blockiops
go test -c -tags resman_native ./state
RESMAN_NATIVE_DISPOSABLE_GUEST=resman-82p ./blockiops.test -test.v -test.run TestNative
RESMAN_NATIVE_DISPOSABLE_GUEST=resman-82p ./state.test -test.v -test.run TestNativeBlockIOPSDecision
```

The local native lab uses Oracle's OL9U8 image `OL9U8_x86_64-kvm-b293.qcow2`, SHA256
`b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b`, QEMU/KVM,
and systemd `252-67.0.1.el9_8.2`. SmolVM's `6.12.95` could create BPF maps but
lacked BTF and tracefs (mount returned ENODEV), and cannot substitute for this gate.
Native source/component checks are distinct from a production daemon installation
or a complete distribution qualification matrix.

The checked-in object is rebuilt with `go generate ./internal/blockiops` using
clang 21.1.8 and libbpf development headers 1.5.0 in the native lab. Generation uses
relative source paths and a fixed debug compilation directory. `bpf/SHA256SUMS`
and the ordinary object-integrity test detect stale C/object pairs.

Primary references:

- [Linux cgroup v2 I/O and writeback](https://docs.kernel.org/admin-guide/cgroup-v2.html#io).
- [Linux 6.12 block accounting](https://github.com/torvalds/linux/blob/v6.12/block/blk-cgroup.c).
- [systemd 252 hard-I/O lookup](https://github.com/systemd/systemd/blob/v252/src/core/cgroup.c).
- [systemd 239 originating-device lookup](https://github.com/systemd/systemd/blob/v239/src/basic/blockdev-util.c).
