# Hourly observation rollups

The optional metrics database retains compacted UTC-hour observations after raw
samples expire. Schema 12 adds `metric_rollups` and the independent process-activity
store transactionally. Supported older schemas migrate through their existing steps;
pre-migration observations are preserved, and no past decision, sample, or continuous
coverage is fabricated. Existing raw history and summary APIs keep their raw-only
meaning.

## Compaction and retention

At startup and each existing periodic retention pass, ResMan selects exactly the
raw user/system rows older than `METRICS_DB_RETENTION_DAYS`, adds their sufficient
statistics to hourly buckets, and deletes those same raw rows in one SQLite
transaction. Failure rolls back both compaction and deletion. A repeated pass cannot
recount consumed rows; a late older raw sample contributes once on a later pass.
The procedure does not rebuild an existing bucket from a partially retained subset.
The first and last sample timestamps describe the contributing sample range, not a
claim that every intervening moment was observed.

`METRICS_DB_ROLLUP_RETENTION_DAYS` defaults to 365 and is dynamic, like raw retention.
The effective rollup retention is the larger of configured rollup and raw retention,
so existing long raw-retention configurations remain valid and cannot be silently
shortened. Both startup and periodic cleanup consume the effective value. Cleanup
keeps a partial boundary hour until its entire bucket is outside the rollup window,
which may retain up to one extra hour. Process activity and terminal control history
keep their existing raw-retention window; they are not averaged into trend rows.
The cleanup count includes deleted raw/history/process/expired-rollup rows, not the
number of aggregates created.

These aggregates extend SQLite/MCP trend history. They do not change the retention
of a separate Prometheus server or make a Grafana Prometheus datasource read SQLite.
Database/retention failure remains an explicit optional-sink degradation and never
rolls back applied resource limits.

## Meaning and coverage

Each entry contains `samples`, `known_samples`, `unknown_samples`, `sum`,
`sample_average`, `minimum`, `maximum`, and first/last sample timestamps. Averages
are **sample-weighted**, computed from the accumulated sum and known count; they are
not averages of averages and are not duration-weighted. An all-unknown series has
null sum/average/minimum/maximum; a known idle sample remains a measured zero.
Aggregated numeric values use floating-point statistics for trends, not an exact
integer accounting ledger. The sample counts retain integer identity.

User buckets separate UID and username, preserving user-name changes as different
series. Their metrics are CPU aggregate per-core percentage, selected process memory
bytes, process count, cgroup RAM bytes, unit CPU-use deltas, memory-high/OOM-kill event
deltas, and separate CPU/RAM/I/O applied-limit sample indicators. Nullable raw process
observations remain unknown in the corresponding metric; nullable unit counters are
never converted to zero. The limit indicators summarize the recorded state snapshots,
not continuously verified enforcement over the entire hour.

System buckets separate enforcement mode, denominator state, and total core count.
They retain parent CPU-use/throttled-use deltas, CPU-actively-limited user count, and
separate CPU, RAM/I/O and any-resource applied-state sample indicators. The historical
raw host CPU and load-average columns do not carry source availability; their averages
are deliberately omitted instead of guessing whether old zero values were measured.
Use source-aware current observations and control history for that distinction.
Summed available unit deltas describe only their known sampled intervals, never an
unmeasured complete hour.

## MCP query

`get_metrics_rollups` is always discoverable and requires the metrics database at
invocation. Its typed arguments are:

- `scope`: `user` or `system` (required).
- `uid`: optional nonnegative user UID; invalid for system scope.
- `start_time`, `end_time`: optional RFC3339 timestamps, defaulting to the preceding
  24 hours; the range must increase.
- `limit`: maximum metric rows, default 100 and capped at 1000.

The result contains compacted entries only, explicitly sets `raw_samples_included`
to false, and reports `averaging="sample_weighted"`. It does not merge raw data or
change sampling/decision state. Recent data remains in the raw history tools until
its retention compaction pass. Ordering is newest hour first, with deterministic
identity and metric tie-breaks. A limit bounds metric rows, not whole hour groups.

Queries return whole stored hour buckets overlapping the requested range. A start
or end inside an hour does not filter individual samples within that bucket; inspect
first/last sample timestamps and counts rather than treating a partial-hour request
as a new exact sub-hour aggregate. An unavailable or disabled database returns an
explicit error. Request cancellation and deadlines apply to database gate admission
as well as the SQLite query. Both MCP HTTP and stdio expose the same typed result.
