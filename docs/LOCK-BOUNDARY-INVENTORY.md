# Lock-boundary inventory

This inventory is the review record required by Rule 15. State mutexes protect only
in-memory fields. Slow operations that must remain ordered use
`internal/operationgate.Gate`, a channel-based coordinator whose wait does not block
unrelated state access. A gate may span I/O; a `sync.Mutex` or `sync.RWMutex` may not.

| Component | State synchronization | Ordered-operation gate | Reviewed boundary |
| --- | --- | --- | --- |
| `config.Config` | `mu`, `regexCache` | `saveGate` | Config snapshots are copied under `mu`; atomic file persistence runs only under `saveGate`. |
| `config.Watcher` | `mu`, wait groups | `reloadGate` | Lifecycle/digest state is copied under `mu`; file reads and the apply callback run after unlock. |
| `internal/configepoch.Barrier` | `mu` and `sync.Cond` | epoch admission itself | The barrier unlocks before returning control to component callbacks. |
| `internal/app.App` | `cfgMu`, `psiMu` | component gates | Component start/stop and filesystem probes run after snapshotting pointers. |
| `cgroup.Manager` | `cfgMu`, `mu`, `originMu`, `blockIOMu`, `usernameMu` | `originGate`, `processScanGate` | Cgroup/procfs reads, writes, origin fsync, NSS lookup and process scans run outside state mutexes. Origin persistence publishes only after durable success. |
| `cgroup.PSIWatcher` | `mu`, wait group | `opGate` | Monitor lists and descriptors are snapshotted under `mu`; open, write, close, wake and poll run after unlock. |
| `database.DatabaseManager` | immutable `db` and `dbPath` | `dbGate` | SQLite's one-connection lifecycle is serialized without a state mutex; `Path` remains available during database I/O. |
| `logging.Logger` | `state.mu` for level; `healthMu` for bounded sink health | `writeGate` | Sink writes, close, rotation and direct stderr fallback are ordered by the gate; health publication performs no I/O, and runtime level/health reads never wait for the sink. |
| `mcp.Server` | `mu`, wait group, atomics | `lifecycleGate` | Lifecycle intent is published under `mu`; listener creation, serving and shutdown run after unlock. |
| `metrics.Collector` | `mu`, cache/EMA/process/username mutexes | `userMetricsScan` | Cache and sampling state mutations are local; procfs scans and database writes run after state unlock. |
| `metrics.DBWriter` | `mu` | database manager gate | The mutex protects enablement and timestamps only; database calls use a prior snapshot. |
| `metrics.PrometheusExporter` | `mu` for lifecycle state | `metricsGate`, per-run channels | Metric deltas/label cleanup are serialized without the lifecycle mutex; HTTP listen/shutdown and Prometheus collector calls do not hold `mu`. |
| `state.Manager` | `mu`, `hookMu`, history/tracker mutexes, epoch barrier | `opGate`, hook wait group | Control/enforcement operations remain ordered, while collectors, cgroups, database, exporter, logger and hooks run without state mutexes. |
| `state.IORemediation` | `mu` plus per-user revisions | `opGate` | PSI/cgroup calls use copied state; publication is conditional on the revision observed before I/O. |
| `state.PatternDetector` / `PolicyEngine` | component mutexes | state-manager gate | Configuration is resolved before locking; logging and reconciliation run after unlock. |

When adding a mutex or gate, update this table in the same change. Review must trace
every call made between lock and unlock, including logger calls, injected test hooks,
standard-library filesystem/database/network calls and third-party metric methods.
