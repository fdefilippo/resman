# Remaining operational improvements

## Primary goal

Operators can inspect real system pressure, understand sustained activity of a specific process, retain longer trends without all raw samples, and distinguish notification attempts from delivery. Collection stays authoritative and bounded; these observations do not silently introduce new enforcement policy.

## Phase 0: Documentation and contract discovery

**How this serves the primary goal:** confirm existing sources and availability before choosing new behavior.

Allowed APIs and source patterns:

- `metrics/collector.go`: `GetAllUserMetricsForDecision`, `scanUserMetrics`, `collectAllUserMetrics`, independent decision/observation sampling state; copy isolation tests from `metrics/collector_test.go`.
- `cgroup/psi.go`, `cgroup/psi_watcher.go`, `internal/app/app_psi.go`: shared PSI parser and owned watcher lifecycle. Kernel source: https://docs.kernel.org/accounting/psi.html. System CPU full pressure is undefined and must not be presented as meaningful measured pressure.
- `state/limit_hook_executor.go`, `state/limit_hooks.go`: bounded queue, owned cancellation, one-shot HTTP primitive. Classify failures before existing sanitization removes unsafe network details.
- `database/control_history.go`, `database/manager.go`: typed DTOs, secure SQLite path, serialized transactional writes, versioned migrations. Copy MCP HTTP/stdio round-trip patterns from `mcp/control_history_test.go`.
- `docs/DEVELOPMENT.md`: preserve eligibility/intent/observation, explicit unknown values, configuration lifecycle, lock boundaries and one authoritative decision sample.

Discovery found duplicate CPU stat reads, not proof that PSS dominates. PSI parser must reject malformed/nonfinite/missing fields and preserve uint64 totals; absent full pressure is not zero. Historical host CPU raw values lack availability, so rollups must not invent it.

## Phase 1: Implement the six contracts

**How this serves the primary goal:** deliver pressure, collection, notification and durable observation improvements through independent owned modules.

Pressure workstream (2.2, 4.3): sample three fixed system PSI resources independently of event-driven mode; explicit per-source availability, freshness and full availability. Keep observation refreshes from overwriting decision pressure. Add opt-in memory wake threshold and preserve RAM/systemd-oomd boundaries. Update lifecycle, readiness, generated config, dashboard and tests.

Collection/activity workstream (3.1, 4.1): measure a representative scan/read path with benchmark/profile evidence, remove demonstrated redundant work and bound any concurrency while retaining one fresh scan bracket. Capture process observations from that same scan; use boot/PID/kernel-start identity. Keep process lifetime distinct from observed CPU/RAM interval coverage, missing samples, reuse and exits. Publish bounded typed MCP queries and durable summaries; never use PID/executable Prometheus labels or infer seven days of continuous CPU activity from process age.

Delivery/storage workstream (3.3, 4.2): retry transient HTTP failures only inside the existing bounded owned job, same idempotency key for all attempts, finite attempts/deadline, visible failures and terminal outcomes. Scripts remain single attempt because replay safety cannot be inferred. Hourly UTC compaction consumes exact expired raw rows atomically into additive counts/sums and deletes those rows in the same transaction; repeated runs and late arrivals cannot double-count. Separate longer rollup retention, bounded typed query and explicit observation/availability counts. Coordinate one schema-12 migration for process activity and rollups.

Anti-pattern guards: no stale PSS under an unchanged fresh-memory field; no extra decision read from MCP; no user aggregate disguised as process identity; no raw sanitized-error unwrapping for retry classification; no exactly-once promise; no average of averages; no filled gaps or fake historical availability; no new memory-PSI activation/release dimension.

## Phase 2: Integrated verification and review

**How this serves the primary goal:** prove the public behavior and failure boundaries before marking work complete.

Run each workstream regression tests, process identity/gap/restart and HTTP/stdio contracts, migration/retention/late-data tests, cancellation/shutdown and repeated race checks. Verify generated config, schema markers, lock inventory, monitoring assets and upgrade notes against implementation. Run `make ci-quality`. Independent review must fix, track or refute every anomaly. Kernel capability tests remain distinct from fixture tests; no live deployment is part of this change.

## Phase 3: Local completion

**How this serves the primary goal:** leave a reviewable source state and an accurate issue trail.

Commit only after verification passes; close the six implementation issues and parent only when all contracts are delivered. Preserve the pending unpublished 1.43.0-1 identity. No push, tag, package build or deployment is authorized by this task.
