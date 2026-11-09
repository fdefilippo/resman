# Process activity: superseded proposal

The March 2026 proposal is superseded by the implemented [sampled process activity contract](PROCESS-ACTIVITY.md). Use `get_process_activity` over either MCP HTTP or stdio to query actual boot/PID/start-tick identities, lifetimes, covered CPU intervals and fresh PSS/RSS observations.

The implementation separates process lifetime from measured activity duration, exposes missing coverage and optional persistence failures, bounds runtime/history queries, and reuses the existing authoritative process scan. Per-user summary rows do not identify individual processes or prove continuous activity.

The proposal's additional monitoring timeouts, prolonged-activity configuration keys, per-process Prometheus labels and automatic alerts are not implemented interfaces. Existing metrics database configuration controls optional durable process summaries; see [PROCESS-ACTIVITY.md](PROCESS-ACTIVITY.md) for supported filters, retention, polling limitations and measured collection costs.
