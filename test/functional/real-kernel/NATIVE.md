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
