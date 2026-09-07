# Native lifecycle pre-gate

`systemd-native-lifecycle` exercises the native adapter with a source binary on an
explicitly disposable systemd host. It is **not** the complete `resman-nq6.9`
release matrix, and a PASS here cannot close that issue or the parent epic.

```sh
GO_BIN=/usr/local/go/bin/go \
  test/functional/real-kernel/remote.sh systemd-native-lifecycle root@terra
```

The existing remote controller owns cancellation, exclusive host access and evidence
retrieval. A dirty source requires explicit `REAL_KERNEL_ALLOW_DIRTY=1` and is labeled
as a worktree digest rather than a release revision. The environment also records the
tested binary hash and the installed package separately: testing a source binary does
not prove an RPM or DEB. Package acceptance remains a separate part of nq6.9.

## Preconditions and footprint

The service must already be stopped; no ResMan process, ownership journal or runtime
user-slice override may exist. Accounts `resman-t1`, `resman-t2` and `resman-t3` must
exist and have no running processes. The current fixture selects device `8:0`, as on
terra; do not infer coverage for a different storage topology.

The runner creates one uniquely named file under `/etc/cron.d`, a public root-owned
helper directory with private per-user output, and transient daemon/split units. Cron
creates genuine PAM/logind sessions; the PID's own `/proc/PID/cgroup` must match the
session advertised by logind. Six CPU workers and 64 MiB of resident memory run per
user. The workload has a bounded lifetime. Neither an existing user's crontab nor the
package configuration is replaced. Cleanup targets recorded sessions, not every PID
of a UID, and never performs an unguarded `systemctl revert` or deletes the journal.

The default policy uses reserve/root/best-effort 100, two mapped users at 300 each,
and one excluded sibling. Before workloads start, a real foreground daemon start
validates the fixture in blackout. The previously valid 750-point upgrade map must
exit 78 with explicit overcommit and without creating a lease.

## Assertions and evidence

The ten independently required entries in `checks.json` cover upgrade rejection,
real sessions, blackout intent suppression, blackout observation, native quota and
weights, memory/I/O property values, resource-specific authority loss and release,
crash reclaim, graceful stop, and release when only root remains. A missing result,
FAIL or BLOCKED can never produce an overall PASS. Property checks are **not** I/O
throughput or CPU proportional-delivery measurements.

Graceful stop is proved twice with sessions still alive: first after ordinary
application, then after crash/reclaim. The first must finish before the crash phase
starts. Both require the journal and mutable drop-ins to disappear, with exact
baseline values in the kernel; a late cleanup after logout cannot substitute.

Config, map, SQLite history, logs, scrapes, PID identities, command arguments, kernel
readbacks and result files are retained under the local evidence directory printed by
the remote runner. On failed release, the runner records the journal and both D-Bus
and kernel state **before** terminating its sessions. A later manual recovery cannot
retroactively turn a cleanup failure into PASS.

Unprivileged tests run through `make test-functional-real-kernel-unit`, including
missing/non-PASS evidence, blocked preflight, failed cleanup and helper permissions
under the controller's private umask. The separate Go test
`TestSystemdNativeBlackoutSuppressesIntentWhileObservationRemainsIndependent` proves
the state boundary and positive control; manually invoking a refresh there does not
prove that the application loop schedules it during blackout.

The release matrix still needs complete current-revision nq6.9 acceptance. The
controlled proportional method and its earlier campaign have independent PASS;
that does not extend to the new producers described below or replace a complete
current-revision final matrix. Coverage, recovery, reconciliation, package and
non-systemd producers are implemented and locally tested, but their field acceptance
has not yet been established by this harness change. Hard I/O delivery remains
part of the daemon coverage row. Weighted I/O is exercised by the implemented
adapter-only row described below, not by a daemon policy or a delivery guarantee;
the future policy is deferred to `resman-nq6.40`. There is no overall nq6.9 PASS
yet. Track completion and findings in Beads, not by reinterpreting a supported
subset as the final gate.

## Additional native rows: implementation is not field acceptance

The following commands collect separate revision-bound rows, sequentially, without
changing the installed service configuration or installing a package:

```bash
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-coverage root@terra
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-recovery root@terra
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-reconciliation root@terra
```

`systemd-native-coverage` requires root and non-root localhost SSH authentication
and trusted host keys to work already. It probes both PTY modes, child units,
runtime-owned descendants and kernel-backed I/O. It never installs SSH credentials,
edits PAM, downloads an image or changes the block-device scheduler. The nspawn root
must already exist at `/var/lib/machines/nq6c`; runs use volatile overlays. The
rootless test requires the preloaded rootful `docker.io/library/oraclelinux:9`
image, transferred into isolated run-owned rootless storage, rather than pulled
from a registry. Unavailable prerequisites remain explicit BLOCKED results.

The daemon coverage row does not claim weighted-I/O policy. The approved
`resman-nq6.39` scope is a separate `systemd-native-weighted-io-adapter` row:

```bash
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-weighted-io-adapter root@terra
```

It uses a run-owned configfs null_blk device and two real cron/PAM user slices,
without non-root SSH credentials. Only the real adapter writes IOWeight; its
retained probe proves 100/2300 programming, BFQ 100/300 readback and exact release.
The fixture independently checks BFQ on its own device and a non-BFQ negative
phase (also excluding enabled per-device io.cost). Programmed values can remain
readable while the negative device has no effective weight capability. This is
fixture capability logic, not a new production scheduler detector. Pure-read
traffic remains CHARACTERIZATION without a delivery verdict. No daemon policy,
public knob or physical-disk guarantee is added; `resman-nq6.40` owns that deferred
design. See [NULL-BLOCK.md](NULL-BLOCK.md) for ownership and cleanup boundaries.

`systemd-native-recovery` executes a retained opt-in adapter test binary. It checks
real PAM ownership, crash between revert and reload, automatic recovery, persistent
and runtime operator conflicts, recreated units and exact cleanup. Its evidence
class is an adapter probe, not the daemon or installed package.

`systemd-native-reconciliation` uses CPU-only policy with genuine PAM sessions.
It verifies composite reload and successful acknowledgement against live weights,
SQLite intervals and durable journal generations; real logout/login; concurrent
reload and sibling admission; and bounded cardinality refusal. At most sixteen
transient topology-probe slices are created under previously unused UID names;
no user accounts are created. Existing services and nonempty unowned slices are
not eligible for cleanup.

CPU hotplug is opt-in, not an implied consequence of running reconciliation:

```bash
# Run only after explicit approval for CPU hotplug on this disposable host.
REAL_KERNEL_ALLOW_CPU_HOTPLUG=1 GO_BIN=/usr/local/go/bin/go \
  test/functional/real-kernel/remote.sh systemd-native-reconciliation root@terra
```

Without that flag, the other proofs remain retained and `online-cpu-change` is
BLOCKED. Only values `0` and `1` are accepted. The fixture records the requested
intent, changes only the highest online nonzero CPU, and confirms restoration of
the original online set on success or failure. This is a laboratory topology probe,
not a new ResMan affinity or placement feature.

## Package and non-systemd rows

The package row runs the installed `/usr/bin/resman` only after its identity and
bytes match the supplied RPM payload:

```bash
REAL_KERNEL_PACKAGE=/absolute/path/to/resman-1.33.0-6.el9.x86_64.rpm \
GO_BIN=/usr/local/go/bin/go \
  test/functional/real-kernel/remote.sh native-package-acceptance root@terra
```

The example uses the next identity after the produced `1.33.0-5`. Increment RELEASE
before any later build and install it only through an explicitly approved operation.
The row neither builds nor installs packages. It uses shipped RPM defaults, proves the 750-point
rejection, preserves rejected schema 5, observes schema 6 with real rows, and runs
the native lifecycle using isolated configuration. A source-binary PASS cannot
satisfy this package row.

```bash
SMOLVM_SCENARIO=non-systemd-migration test/functional/smolvm/run.sh run
```

This required row proves legacy ingress and exact live restoration under a private
PID/mount namespace with non-systemd PID 1 inside disposable SmolVM. It does not
claim a physical non-systemd boot. Missing guest capabilities produce BLOCKED.
The final catalog also retains separate SmolVM `missing-io-startup` and
`mcp-filter-reload` rows and real-host `psi-refresh-neutrality` evidence.

## Simultaneous flat proportional scenario

`remote.sh systemd-native-proportional root@terra` adds a separate source-binary row.
It uses the same exclusive remote ownership and cleanup boundary, with genuine cron
sessions for both mapped users and an excluded best-effort user, plus an authenticated
localhost SSH/PAM session for root (OL9 cron does not register root with logind).
Root localhost authentication and a known host key must already work: the runner
never changes SSH, PAM, authorized keys or host-key verification. Four online
CPUs with unrestricted runner affinity are required. The fixture is deliberately not
the shipped default: reserve 700, root 100, best effort 100, mapped 50 + 50. Its
300-point native parent runs beside independent correct and stale-control parents
with the same quota, so their combined ceilings total 900 points on the host.

The reference has weights 5000/5000/10000/10000. The deliberately stale control
underweights best effort at 5000. Six full-contention 60-second windows and one
lending window record raw parent/leaf counters, identities, per-node monotonic read
intervals, online capacity and throttling. Each group parent and its four leaves are
read as one adjacent block. Every five-second interval checks conservation in both
directions against an error bound derived from the two measured block spans, the
programmed 1.2-CPU capacity and the ten-microsecond integer-counter quantization
allowance. The complete 60-second window retains the fixed bilateral one-percent
conservation bound. The
mapped aggregate must match the same-window reference within 0.5 percentage points;
each leaf within 1.0; the stale control must remain at least 2.0 points away at the
aggregate. There is no stored calibration constant or comparison of delivered CPU
against a nominal guarantee. A parent delivering less than 80% of its nominal quota,
no throttling, changed weights/identity, missing per-node timing, reordered or
interleaved group reads, missing counters or frame skew above 200 ms fails the
measurement rather than silently relaxing the delivery tolerances.

In the lending window one mapped workload is stopped with SIGSTOP, not migrated or
reclassified; its session stays present. Every other native sibling, including the
excluded best-effort user, must consume more of the delivered parent bandwidth.
The journal and programmed weights must remain unchanged. Root must make measured
CPU progress and answer three file probes from its own PAM session within three
seconds in each phase. These probes are not SSH latency measurements. Cleanup resumes
only recorded same-start-time children, restores native properties through ResMan,
and removes only the run-owned reference units and their overrides.

Ten mandatory result keys and independent unprivileged measurement tests guard this
row. Neither this row nor the lifecycle pre-gate alone proves the complete `.9`
acceptance: package identity, SSH, multiple child services, containers, real I/O and
the remaining recovery/conflict/topology cases retain their separate obligations.

## Reference-only diagnostic

The [2026-09-07 investigation](REFERENCE-EVIDENCE.md) records the failed unbound
replications, fixed-affinity controls, immutable evidence identities and the
historical measurement proposal. The accepted controlled method and its scope are
specified in [PLACEMENT.md](PLACEMENT.md). All compared active leaves must have the
same observed CPU assignment multiset; dead or blocked workers invalidate the sample.
Three independent fresh-tree replicas are required, not three windows in one run.

`remote.sh systemd-native-reference root@terra` investigates nq6.37 without starting
ResMan, creating login sessions, or changing `user.slice`. Two independent identical
reference trees and one deliberately stale control each have a 1.2-CPU parent and
the same workloads used above. Six consecutive, non-overlapping 60-second windows
retain all raw samples and scheduling topology. Every five-second sample must have
valid identities, counters and skew. The unbound row now reports dispersion with
`CHARACTERIZATION`, without a delivery verdict. The pinned controls require each
primary window to meet the existing 0.5/1.0-point comparison bounds and distinguish
the stale control by at least 2.0 points. No bad window is discarded.
Descriptive 180/360-second aggregates are also
saved, but cannot substitute for failed primary windows or authorize changed bounds.

The existing exclusive host controller and exact reference cleanup apply. A pinned PASS
means only that these reference trees were comparable in this run, never that the
daemon, lending contract, package or release passed. A FAIL establishes that this
measurement method cannot yet attribute that discrepancy to ResMan. Invalid
unbound collection or cleanup still fails; valid characterization never substitutes
for a required PASS. The bundled
source binary is only queried for its version, never started as a daemon;
`diagnostic-contract.json` explicitly records that scope. Independent fresh runs are
required to assess reproducibility.

`systemd-native-reference-pinned` is a separate diagnostic control: each leaf has
four CPU workers, one explicitly bound to each CPU 0-3, instead of six unbound
workers. The helper process remains unbound. It keeps the same three parents,
weights, measurement windows and thresholds. It can investigate sensitivity to
worker layout, but changes both worker count and affinity: it does not isolate one
of those factors by itself. Even a PASS cannot replace the unbound workload evidence
or authorize a change to the release gate without review.

`systemd-native-reference-pinned-six` retains the original six workers per leaf,
binding them to CPUs 0,1,2,3,0,1 in every reference. It changes affinity without
changing worker count. Worker count, actual affinity, owner and birth identities
are verified at every sample in all diagnostic variants. The helper remains unbound;
neither diagnostic establishes how the production workload behaves without affinity
restrictions or changes ResMan's policy.
