# Metrics Database Guide

## Overview

ResMan can persist system and per-user observations in SQLite and expose the retained
history through MCP. Persistence is disabled by default so an operator must opt in.
Database write failures are observable and retried on a later control cycle, but they
do not disable resource enforcement.

During blackout, host Prometheus observations still refresh at
`METRICS_REFRESH_INTERVAL` in polling and PSI modes. SQLite history and per-user
Prometheus series remain decision-owned: no new decision interval is sampled or
persisted during blackout. A gap in history is not a measured zero-usage interval.

## Configuration

The authoritative defaults and comments are in
[`config/resman.conf.example`](../config/resman.conf.example).

```ini
METRICS_DB_ENABLED=false
METRICS_DB_PATH=/var/lib/resman/metrics.db
METRICS_DB_RETENTION_DAYS=30
METRICS_DB_WRITE_INTERVAL=30
```

- `METRICS_DB_ENABLED` enables persistence and the database-backed MCP tools.
- `METRICS_DB_PATH` selects the SQLite file. Use `:memory:` only for tests.
- `METRICS_DB_RETENTION_DAYS` controls automatic retention cleanup and is dynamic.
- `METRICS_DB_WRITE_INTERVAL` is measured in seconds and must be at least 5.

Changing `METRICS_DB_ENABLED`, `METRICS_DB_PATH`, or
`METRICS_DB_WRITE_INTERVAL` requires a restart. Reload reports that requirement
explicitly rather than publishing values that the running database does not use.

### On-disk security contract

For a file-backed database, the immediate parent directory must be owned by the UID
running ResMan and have mode `0700`. ResMan creates a missing parent with that mode.
The ancestor hierarchy must be stable: symbolic-link ancestors and directories whose
owner, group, or other users can replace the verified path are rejected.

An existing database and its optional `-wal` and `-shm` sidecars must be regular,
non-symlink files owned by the ResMan UID with mode `0600`. ResMan validates these
properties before SQLite opens the store and normalizes newly created files to
`0600`. It never relies on the caller's umask to protect per-user history.

An invalid persistence path disables database features while the resource manager
continues protecting the host. Startup reports the exact path, expected ownership,
and required mode. Correct the filesystem state and restart ResMan; do not weaken the
permissions. The `:memory:` database is exempt because it creates no files.

## Examples

### Standard persistent database

```ini
METRICS_DB_ENABLED=true
METRICS_DB_PATH=/var/lib/resman/metrics.db
METRICS_DB_RETENTION_DAYS=30
METRICS_DB_WRITE_INTERVAL=30
```

### In-memory test database

```ini
METRICS_DB_ENABLED=true
METRICS_DB_PATH=:memory:
```

All data in an in-memory database is lost when the service stops.

### Extended retention with fewer writes

```ini
METRICS_DB_ENABLED=true
METRICS_DB_RETENTION_DAYS=90
METRICS_DB_WRITE_INTERVAL=300
```

## Schema and compatibility

The current store uses schema version 12 and contains `user_metrics`,
`system_metrics`, `control_history`, `process_activity` and `metric_rollups` tables.
Each raw observation transaction has one `sample_epoch_id` and common
`interval_start`/`interval_end` boundary. A nullable start identifies the first
baseline after daemon startup. User and system records in one transaction therefore
describe the same decision-sample interval; history consumers must not combine rows
from different epoch identifiers.

Schema 11 upgrades transactionally to schema 12 by adding process summaries and
hourly rollups; existing raw rows and terminal history remain intact. Supported
schemas 7 through 10 first pass through their existing versioned migrations.
No historical process activity is inferred from user aggregates. See
[hourly observation rollups](METRICS-ROLLUPS.md) for atomic compaction, effective
retention and coverage semantics. Raw-history and summary tools continue to query
raw rows only; `get_metrics_rollups` queries compacted buckets separately.

`process_activity` retains sampled boot/PID/kernel-start identities and covered
CPU/RAM summaries, not claims of uninterrupted activity. Its rows expire under
the raw retention window. Live queries work with persistence disabled and report
the optional store status explicitly. Persisted summaries do not bridge daemon
downtime or create a new process sample. Their writes follow the completed decision
scan; MCP queries never initiate collection.

User records separate process-derived CPU and memory observation; total and
enforceable process counts; configured CPU class and nullable mapped guarantee;
lifecycle outcome from applied class and weight; raw `cpu.max`/`cpu.weight`
diagnostics; nullable slice usage deltas; and cgroup
RAM usage, charge coverage, limits, swap policy and distinct high/max/OOM/kill event
deltas. Lifecycle is one of `ineligible`, `eligible_inactive`, `applied`,
`failed`, or `released`. Process-origin, ownership-rejection, and recovery lifecycle
columns were removed with schema 7 because ResMan no longer relocates PIDs.

An absent guarantee never means zero: best-effort users have no synthetic per-user
guarantee. An absent delta means no comparable baseline was available; numeric zero
means two comparable observations produced no increase. Cgroup identity changes,
counter decreases, read failures, and daemon restart create a nullable baseline
instead of a wrapped or multi-lifetime delta. Rising `memory.high` with zero
max/OOM/kill deltas represents throttling or a stall, not a kill.

System records contain the nominal parent pool, live online-CPU capacity,
programmed quota/period, root entitlement, flat programmed/observed sibling weights,
denominator state, enforcement mode and synchronized parent usage/throttling deltas.
They also contain the weighted-I/O lifecycle and bounded reason, configured selector,
separate cumulative classification and probe attempts, programmed and read-back state,
functional acceptance, effect qualification, bounded qualification provenance,
partial-user count, and observed-delivery state. These are independent dimensions: a
programmed value is not proof of delivery, and `functionally_accepted` does not imply
`effect_qualified`. Provenance `none` means no retained campaign matches; a named
provenance is diagnostic and never authorizes runtime mutation.
User rows include independent CPU authority and I/O coverage in addition to RAM.
See [CPU Points observability](CPU-POINTS-OBSERVABILITY.md) and
[weighted block-I/O policy](IO-WEIGHTS.md) for the complete schema-10 contract. A
missing observation never claims measured zero or runnable capacity.

The current Prometheus and MCP projections use the same typed control-cycle snapshot
as these history rows even when the database is disabled. Database cadence controls
only which snapshots are persisted; it does not enable, disable, or advance a second
CPU Points observation stream.

Every delta covers only the decision-sample interval named by its own
`interval_start` and `interval_end`. Baselines advance on every control cycle even
when `METRICS_DB_WRITE_INTERVAL` causes intermediate samples not to be stored. Derive
a rate from a row by dividing its delta by that row's elapsed interval. Do not sum
sparse stored deltas as a total: skipped intervals are not included and cannot be
reconstructed from later rows.

The schema is versioned with SQLite `PRAGMA user_version`. ResMan atomically migrates
schema 7 through schemas 8 and 9 to schema 10 by adding weighted-I/O system columns,
the evidence provenance, and the widened bounded-provenance CHECK. Historical rows are
marked `disabled` with `not_measured` delivery and provenance `none` because the
feature did not exist in schema 7. Each step is transactional; a failed step rolls
back to its input version.
Version 3 and unversioned stores are rejected
by the CPU Points cutover because they cannot express allocation class, guarantee,
common sampling epochs, topology resets, or RAM charge coverage. Version 4 is also
rejected by the ownership-containment release. Version 5 cannot represent the native
flat plan, and version 6 still contains the retired PID-relocation lifecycle. Move or
delete a schema-6-or-older store and restart to create version 12. Development schema-9
stores are rebuilt transactionally without changing row meaning. No alias or dual-read
path exists.

Useful indexes cover timestamps, user IDs, and enforcement-state queries. Timestamp
values are stored in UTC and API responses use RFC 3339.

## Terminal control history

`control_history` stores each completed control-cycle attempt independently of
`METRICS_DB_WRITE_INTERVAL`; observations still obey their existing cadence. The
feature uses the existing database enablement, secure path and retention settings.
Schema 10 migrates transactionally to 11 by adding the empty table and time index.
Existing metrics retain their meaning; no historical decisions are inferred.

Each entry has a random daemon-session identity plus a monotonic per-session cycle
number, start/finish timestamps and a bounded trigger/outcome. It records the policy
decision/reason separately from execution mode, requested intent, acknowledged action
and block reason. CPU request/application and I/O coverage retain their own fields.
A failed collection or a blackout attempt has `sample_available=false`; the legacy
numeric fields must not be read as measured zeros in those entries. Failure records
contain a first failed stage and bounded failure kind, not raw errors or credentials.
Host CPU sample availability and its bounded unavailable reason are stored separately
from the availability of the overall decision sample. A missing host sample is never
a measured zero. Existing policy reasons remain available. Full error diagnostics remain in logs.
`outcome` describes the protective pipeline; history-delivery failure is reported
separately and never implies that enforcement was undone.

Memory retains 100 terminal attempts. When persistence is enabled, up to 100 pending
entries are retried idempotently in a single transaction after each attempt. Delivery
uses a separate five-second context budget, also for orderly cancellation; it uses
the existing database operation gate and SQLite busy timeout. It does not create a
background worker or change resource policy. A delivery error is returned to the cycle
owner and degrades writer readiness; the recent history remains available. The first
failure is observable through the cycle error and `resman_errors_total` with component
`control_history`. Retry-window overflow emits a warning and `retry_window_overflow`.

`get_control_history` merges memory with the latest durable rows, deduplicates by
session/cycle, returns chronological order and caps `limit` at 1000 (default 10).
Additive response fields are `persistence_status` (`available`, `unavailable`, or
`disabled`), `pending_entries`, and `dropped_entries`. The dropped count covers this
daemon session only. A readable store whose writer is degraded remains unavailable.
Disabled persistence keeps memory-only history; a failed read returns available
memory with an explicit unavailable status. No client read collects a decision sample.

The terminal-entry duration excludes its own storage delivery; the total-cycle
histogram includes that delivery. The stage histogram measures it as `record_history`.
All retained entries use the existing retention-days cleanup in the same transaction
as user/system observations. An uncommitted attempt interrupted by a crash and pending
memory entries lost on restart cannot be recovered. This is bounded diagnostic history,
not a guaranteed audit log. An abrupt crash can leave no terminal entry for the active
attempt; absence never proves that no action occurred.

## MCP access

Database-backed tools remain registered when persistence is disabled so production
discovery exposes a stable inventory. Invocation fails explicitly until the required
database capability is available. The authoritative tool names, registration
conditions, and invocation requirements are maintained in the **MCP tools** section of
[`resman.8`](resman.8); this guide deliberately does not duplicate that inventory.

Supported time selectors include RFC 3339 timestamps, date-only values, relative
expressions such as `now-24h`, and predefined ranges such as `today`, `yesterday`,
`last_24_hours`, `last_7_days`, and `last_30_days`.
The separate `get_metrics_rollups` range uses RFC3339 timestamps, as documented in
[METRICS-ROLLUPS.md](METRICS-ROLLUPS.md).

`get_user_summary.cpu_limit_active_sample_percent` is the percentage of retained
raw samples whose CPU-limit state is active. The older
`cpu_limit_active_time_percent` field remains as a deprecated alias with the same
value and meaning. Neither is duration-weighted: irregular or skipped samples
prevent interpreting either value as a percentage of elapsed time. New rollups
also state their sample-weighted meaning
explicitly and do not claim uninterrupted enforcement between samples.

## Direct inspection

Current metrics schema: 12.

Stop ResMan before maintenance that modifies the database. For live inspection,
explicitly open SQLite in read-only mode: a `SELECT` alone does not make the
default CLI connection read-only. Never remove or rename the database, WAL, or
shared-memory sidecar while the daemon is running.

```bash
sqlite3 -readonly /var/lib/resman/metrics.db \
  'SELECT timestamp, uid, username, cpu_usage_percent FROM user_metrics ORDER BY timestamp DESC LIMIT 10;'

sqlite3 -readonly /var/lib/resman/metrics.db \
  'SELECT uid, username, AVG(cpu_usage_percent) FROM user_metrics WHERE timestamp > datetime("now", "-24 hours") GROUP BY uid, username;'
```

Prefer the MCP tools for applications because they preserve the typed public contract.

## Retention and performance

Retention cleanup runs at startup and periodically while ResMan is running. File-backed
stores use incremental auto-vacuum; cleanup reclaims a bounded number of pages instead
of running a full blocking `VACUUM` every time. An older store configured with
`auto_vacuum=NONE` is converted once during startup.

Choose the write interval and retention period according to user count and available
storage. Do not set a write interval below five seconds. Monitor database size with
`get_metrics_database_info` and filesystem tooling.

## Troubleshooting

### The database is not created

1. Confirm `METRICS_DB_ENABLED=true`.
2. Check the startup log for a rejected path or incompatible schema.
3. Verify the parent is owned by the ResMan UID with mode `0700`.
4. Verify the database and any WAL/SHM sidecars are regular files owned by that UID
   with mode `0600`.
5. Confirm the ancestor hierarchy contains no symlink or untrusted writable directory.

### Writes are slow

- Increase `METRICS_DB_WRITE_INTERVAL`.
- Check filesystem latency and free space.
- Reduce retention if the store is larger than operationally useful.

### The database is too large

- Reduce `METRICS_DB_RETENTION_DAYS` and allow the next cleanup to run.
- Archive required records outside the live database before reducing retention.
- Inspect the store rather than deleting it while ResMan is running.
