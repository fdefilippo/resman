# Lock-boundary inventory

This inventory is the review record required by Rule 15. State mutexes protect only
in-memory fields. Slow operations that must remain ordered use
`internal/operationgate.Gate`, a channel-based coordinator whose wait does not block
unrelated state access. A gate may span I/O; a `sync.Mutex` or `sync.RWMutex` may not.

| Component | Synchronization-bearing types | State synchronization | Ordered-operation gate | Reviewed boundary |
| --- | --- | --- | --- | --- |
| `config.Config` | `config.Config`<br>`config.EditorConfigCandidate` | `mu`, `regexCache` | shared `saveGate` | Config snapshots are copied under `mu`; legacy setters and revision-bound editor writes share `saveGate`, and source confirmation plus atomic persistence run while it is held. |
| `config.Watcher` | `config.Watcher` | `mu`, wait groups | `reloadGate` | Lifecycle/digest state is copied under `mu`; file reads and the apply callback run after unlock. |
| `internal/configepoch.Barrier` | `internal/configepoch.Barrier` | `mu` and `sync.Cond` | epoch admission itself | The barrier unlocks before returning control to component callbacks. |
| `internal/app.App` | `internal/app.App`<br>`internal/app.shutdownDeadlineState` | `cfgMu`, `psiMu`, shutdown deadline mutex and wait group | component gates | Component start/stop and filesystem probes run after snapshotting pointers. The shutdown mutex protects only deadline ownership and the current stage; timer waits, logging and forced exit run after unlock. |
| `internal/systemdunit.Adapter` | `internal/systemdunit.Adapter` | operation-owned lease and recovery maps only | `opGate` | Discovery and exact topology confirmation, durable lease-journal replacement, runtime-only D-Bus mutation, readback, cgroup verification, the exact `IODeviceWeight` keyed-reset completion, identity-stable accounting, CPU coverage scans and compare-before-restore are serialized by the gate; no state mutex is held across external I/O. |
| `cgroup.Manager` | `cgroup.Manager` | `cfgMu` | none | The manager verifies the read-only cgroup observation boundary and publishes immutable host-authority status. It never owns a hierarchy or changes PID membership. |
| `cgroup.PSIWatcher` | `cgroup.PSIWatcher` | `mu`, wait group | `opGate` | Monitor lists and descriptors are snapshotted under `mu`; open, write, close, wake and poll run after unlock. |
| `database.DatabaseManager` | `database.DatabaseManager` | immutable `db` and `dbPath` | `dbGate` | SQLite's one-connection lifecycle is serialized without a state mutex; `Path` remains available during database I/O. |
| `logging.Logger` | `logging.loggerState` | `state.mu` for level; `healthMu` for bounded sink health | `writeGate` | Sink writes, close, rotation and direct stderr fallback are ordered by the gate; health publication performs no I/O, and runtime level/health reads never wait for the sink. |
| `mcp.Server` | `mcp.Server` | `mu`, wait group, atomics | `lifecycleGate` | Lifecycle intent is published under `mu`; listener creation, serving and shutdown run after unlock. |
| `metrics.Collector` | `metrics.Collector`<br>`metrics.procCache`<br>`metrics.emaCache`<br>`metrics.hostCPUSamplingState` | `mu`, cache/EMA/process/username and per-stream host CPU mutexes | `userMetricsScan` | Cache and sampling state mutations are local; `/proc/stat` counters are read before the decision or observation baseline is locked, and procfs scans and database writes run after state unlock. |
| `metrics.DBWriter` | `metrics.DBWriter` | `mu` | database manager gate | The mutex protects enablement and timestamps only; database calls use a prior snapshot. |
| `metrics.PrometheusExporter` | `metrics.PrometheusExporter` | `mu` for lifecycle state | `metricsGate`, per-run channels | Metric deltas/label cleanup are serialized without the lifecycle mutex; HTTP listen/shutdown and Prometheus collector calls do not hold `mu`. |
| `state.Manager` | `state.Manager`<br>`state.ThresholdTracker`<br>`state.UserStabilityTracker`<br>`state.controlHistory` | `mu`, `hookMu`, history/tracker mutexes, epoch barrier | `opGate`, hook wait group | Control/enforcement and policy-reload operations remain ordered. Systemd-native reconciliation snapshots state under `mu`, releases it, then enters adapter operations; it reconfirms topology and authority before publishing under `mu`. Collectors, cgroups, D-Bus, kernel reads, database, exporter, logger and hooks run without state mutexes. |
| `state.PatternDetector` | `state.PatternDetector` | component mutexes | state-manager gate | Pattern classification is observational; configuration is resolved before locking and logging runs after unlock. |

The `Synchronization-bearing types` column is machine-read by `make verify-contracts`.
Each production named struct that directly declares `sync.Mutex`, `sync.RWMutex`, or
`operationgate.Gate` must occur exactly once. Test-only and generated Go files are
excluded explicitly; a justified exceptional omission must use the stale-checked
`scripts/verify-contracts/lock-boundary-inventory.allowlist` instead of changing the
scanner.

When adding a mutex or gate, update this table in the same change. Review must trace
every call made between lock and unlock, including logger calls, injected test hooks,
standard-library filesystem/database/network calls and third-party metric methods.

When both epoch admission and an ordered manager operation are used, the
systemd-native lock order is `configepoch.Barrier`, then `state.Manager.opGate`, then
one operation at a time through `internal/systemdunit.Adapter.opGate`. Direct policy
reconciliation enters the manager gate inside the reload epoch. `state.Manager.mu`
may be acquired before or after those operations only to copy or publish in-memory
state; it must never be held while entering the adapter gate. The adapter does not
call back into `state.Manager`.
