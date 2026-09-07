# CPU Points observation under systemd

Current metrics schema: 6.

On a systemd host, `systemd_native` enforcement keeps processes in their existing
units. The finite parent is `user.slice`; its active `user-UID.slice` children
share bandwidth through weights. Root has the dedicated `CPU_ROOT_POINTS`
entitlement. Excluded and unmapped users participate in aggregate best effort.
Unused capacity is available to all runnable siblings. There is no class-priority
borrowing promise. A UID split across unrelated parents has partial CPU coverage;
rootless descendants inside the user slice remain inside its CPU envelope.

The normalized pool is `1000 - CPU_RESERVE_POINTS`. Its programmed quota is
`floor(online_cpus * period * (1000 - reserve) / 1000)` with a minimum quota of
1000 microseconds. CPU Points express relative entitlements to bandwidth actually
delivered by that parent, not absolute CPU floors. Raw weights and configured
quota do not prove delivery.

## One sample, every surface

The decision-cycle accounting snapshot supplies Prometheus, MCP and SQLite schema
6, including when database writes are disabled. CPU and memory readers resolve
paths only through the systemd adapter. They check the unit lifetime before and
after reading, and check the kernel directory identity around the counter reads.
Missing data stays absent. Unit recreation, a decreased counter, restart or a
failed read discards that counter's baseline. The next valid read establishes a
new baseline rather than reporting a multi-interval delta as current activity.

The flat snapshot exposes:

- Configured reserve, nominal parent pool, root and aggregate best-effort points.
- Live online CPUs and programmed parent quota/period; unavailable capacity never
  supplies a current denominator from the collector's core-count cache.
- `denominator_state`: `unavailable`, `inactive`, `incomplete` or `complete`.
  Complete means every active sibling, its lifetime, programmed weight and kernel
  readback were confirmed. It does not assert that every sibling was runnable.
- Programmed weights for each slice, their complete sum, the best-effort aggregate,
  and the observed sibling sum. The latter is absent when confirmation fails.
- Parent and per-slice usage deltas, parent period/throttling deltas, and common
  `interval_start`/`interval_end` boundaries. Delivery is `unavailable` without all
  required counter baselines, even when capacity is known.
- Separate CPU authority, RAM and I/O coverage. CPU authority can be partial while
  the slice is correctly weighted. RAM/I/O can be refused independently of CPU.
  `memory.current` on a native slice includes its existing charges; the legacy
  migration path still has partial post-ingress memory accounting.

RAM high/max/OOM/kill event deltas remain distinct. With unreclaimable pages,
`memory.high < memory.max` can throttle or stall indefinitely without an OOM kill.
An unavailable event delta is never a measured zero.

## Operator measurements

Use the daemon's synchronized values instead of independently timed file reads.
Divide `resman_user_cpu_points_leaf_usage_microseconds_delta` by
`resman_cpu_points_parent_usage_microseconds_delta`. Under complete contention,
compare that ratio with the user's programmed weight divided by
`resman_cpu_points_programmed_sibling_weight_sum`. Sum the best-effort users to
compare the aggregate entitlement; do not treat each rounded best-effort leaf as
having an exact public points guarantee.

Require `resman_cpu_points_denominator_state{state="complete"} == 1`, a positive
parent usage delta, and `resman_cpu_points_observation_interval_seconds` for the
interval. Use several intervals covering at least a 60-second observation window.
Missing deltas and a zero parent denominator do not establish a delivery ratio.

A slice can participate even when its UID is outside the process collector's
selection (root is the usual example). Its slice counters remain observable;
uncollected process usage/count fields are NULL in history and MCP and absent
from Prometheus. Such rows are excluded from process-derived user summaries.
Conversely, an observed UID without a user slice (for example, a service account
running only under `system.slice`) remains in history, MCP and Prometheus with its
process-derived values. Slice weights, counters, limits and history path/quota are
absent/NULL, with CPU/RAM/I/O coverage `unavailable`. Configured CPU eligibility is
preserved: lifecycle is `eligible_inactive` or `ineligible`, never `applied` merely
because processes were observed. These UIDs do not enter the sibling denominator.
If topology discovery itself fails, the process rows still survive but their
lifecycle is `failed`: inability to inspect a slice does not prove its absence.
Jitter and bias depend on both host and workload; an algebraic ratio is a diagnostic
expectation, not a strict per-window pass threshold. Positive parent throttling and
nominal under-delivery can be expected under saturation. The real-kernel functional
gate uses a reference measured in the same execution; its tolerances are not
operator thresholds against the algebraic ratio.

Database retention and `METRICS_DB_WRITE_INTERVAL` select which decision intervals
are stored. Stored rows may therefore have gaps. Normalize a delta by its own
interval duration; summing sparse deltas does not reconstruct elapsed-period totals.

Under `systemd_native`, MCP uses the typed `cpu_points` observation rather than
enriching a sample from a legacy recovery cgroup. `get_limits_status` includes all
observed user slices, including root. The legacy `get_cgroup_info` inspection
refuses this mode and directs callers to that authoritative snapshot.

## Schema and vocabulary break

Schema 6 rejects all prior versioned and unversioned incompatible archives. Stop
ResMan, archive or delete the old metrics database together with its WAL/SHM
sidecars, then restart to create a private schema-6 store. There is no migration
or alias. The three-level domain columns, domain metrics and lending-state field
have been removed. Use flat sibling weights, coverage and measured delivery.

The enforcement-mode vocabulary retains `migration_enabled` for non-systemd hosts
and `observation_only_systemd` when authority is unavailable; native enforcement
uses `systemd_native`. Ownership-refusal lifecycle and bounded ingress counters
remain meaningful for refused acquisition and historical recovery. Native CPU
scheduling never manufactures ownership-refusal events or moves a PID. Requested
intent, successful application, recovery and resource refusal stay separate.
The final harness must select its assertions for the actual enforcement mode;
the systemd-native kernel campaign belongs to `resman-nq6.9`.
