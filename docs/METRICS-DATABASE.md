# Metrics Database Guide

## Overview

ResMan can persist system and per-user observations in SQLite and expose the retained
history through MCP. Persistence is disabled by default so an operator must opt in.
Database write failures are observable and retried on a later control cycle, but they
do not disable resource enforcement.

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

The current store contains `user_metrics` and `system_metrics` tables. User records
include observed CPU, memory, process count, cgroup path, per-resource eligibility,
requested limits, and observed active enforcement state. System records contain the
host observation and the distinct CPU, resource, and combined enforcement states.

The schema is versioned with SQLite `PRAGMA user_version`. ResMan intentionally does
not migrate an incompatible database. If the on-disk version differs from the current
schema, startup refuses to open it and tells the operator to move or delete the store
before restarting. This prevents old columns from being silently reinterpreted under
new semantics.

Useful indexes cover timestamps, user IDs, and enforcement-state queries. Timestamp
values are stored in UTC and API responses use RFC 3339.

## MCP access

The database-backed tools are registered even when persistence is disabled so the
public tool inventory is stable. Invocation fails explicitly until
`METRICS_DB_ENABLED=true` and a database manager is available.

- `get_user_history` returns observations for a UID or username over a requested range.
- `get_system_history` returns host observations over a requested range.
- `get_user_summary` returns aggregate statistics for one user.
- `get_metrics_database_info` reports the path, size, retained counts, range, and users.

The authoritative inventory, registration conditions, and invocation requirements are
in the **MCP tools** section of [`resman.8`](resman.8).

Supported time selectors include RFC 3339 timestamps, date-only values, relative
expressions such as `now-24h`, and predefined ranges such as `today`, `yesterday`,
`last_24_hours`, `last_7_days`, and `last_30_days`.

## Direct inspection

Stop ResMan before maintenance that modifies the database. Read-only inspection can
use SQLite directly:

```bash
sqlite3 /var/lib/resman/metrics.db \
  'SELECT timestamp, uid, username, cpu_usage_percent FROM user_metrics ORDER BY timestamp DESC LIMIT 10;'

sqlite3 /var/lib/resman/metrics.db \
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
