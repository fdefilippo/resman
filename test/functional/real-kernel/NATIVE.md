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

The release matrix still needs the remaining nq6.9 acceptance, including SSH,
synchronized proportional measurements and same-run oracle, root responsiveness,
rootless/nspawn placements, actual block-I/O behavior, external conflicts, interrupted
revert/reload recovery, hot topology changes, and the non-systemd backend's own kernel
evidence. Track completion and findings in Beads, not by reinterpreting this pre-gate.

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
underweights best effort at 5000. Two simultaneous 60-second windows record raw
parent/leaf counters, identities, read skew, online capacity and throttling. The
mapped aggregate must match the same-window reference within 0.5 percentage points;
each leaf within 1.0; the stale control must remain at least 2.0 points away at the
aggregate. There is no stored calibration constant or comparison of delivered CPU
against a nominal guarantee. A parent delivering less than 80% of its nominal quota,
no throttling, changed weights/identity, missing counters or skew above 200 ms fails
the measurement rather than silently relaxing the tolerances.

In the second window one mapped workload is stopped with SIGSTOP, not migrated or
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
