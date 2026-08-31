# ResMan configuration reference

This file is generated from `config.Config`, `DefaultConfig`, and the authoritative lifecycle table. Regenerate it with `go run ./scripts/generate-config-reference`; do not edit the table by hand.

`dynamic` keys are applied by hot reload. `restart-required` keys keep their effective value and report a rejected reload until the daemon restarts. An em dash means the key has no special empty or disabled contract beyond its literal value. The copyable, commented configuration is [`config/resman.conf.example`](../config/resman.conf.example).

| Key | Runtime default | Lifecycle | Empty, disabled, or special value |
|---|---|---|---|
| `AUTODETECT_PATTERNS` | `false` | `dynamic` | false disables workload-pattern classification and RAM policy selection. |
| `BATCH_NIGHT_RAM_QUOTA` | `4G` | `dynamic` | — |
| `BLACKOUT` | `(empty)` | `dynamic` | Empty means no blackout; enforcement is always permitted by schedule. |
| `CGROUP_BASE` | `resman` | `restart-required` | — |
| `CGROUP_OPERATION_TIMEOUT` | `5` | `dynamic` | — |
| `CGROUP_ROOT` | `/sys/fs/cgroup` | `restart-required` | — |
| `CPU_BEST_EFFORT_POINTS` | `100` | `restart-required` | One aggregate entitlement shared by every eligible username absent from CPU_POINTS_FILE. |
| `CPU_POINTS_FILE` | `/etc/resman/cpu-points.map` | `restart-required` | Absolute restart-required path to the strict direct username guarantee map. |
| `CPU_RELEASE_THRESHOLD` | `40` | `dynamic` | — |
| `CPU_RESERVE_POINTS` | `100` | `restart-required` | 0 removes nominal headroom outside the finite ResMan CPU parent; it does not create physical isolation. |
| `CPU_THRESHOLD` | `75` | `dynamic` | — |
| `CPU_THRESHOLD_DURATION` | `90` | `dynamic` | 0 makes CPU threshold activation immediate after a valid sample. |
| `CREATED_CGROUPS_FILE` | `/run/resman-cgroups.txt` | `restart-required` | — |
| `DISABLE_SWAP` | `false` | `dynamic` | — |
| `ENABLE_PROMETHEUS` | `false` | `restart-required` | false creates no Prometheus listener. |
| `IGNORE_SYSTEM_LOAD` | `false` | `dynamic` | — |
| `INTERACTIVE_RAM_QUOTA` | `1G` | `dynamic` | — |
| `IO_BOOST_DURATION` | `600` | `dynamic` | 0 is rejected while I/O remediation is enabled. |
| `IO_BOOST_MAX_PER_HOUR` | `3` | `dynamic` | — |
| `IO_BOOST_MULTIPLIER` | `2` | `dynamic` | — |
| `IO_DEVICE_FILTER` | `all` | `dynamic` | all selects every eligible whole block device. |
| `IO_LIMIT_ENABLED` | `false` | `dynamic` | false disables I/O enforcement while observation remains available. |
| `IO_PSI_THRESHOLD` | `50` | `dynamic` | — |
| `IO_READ_BPS` | `100M` | `dynamic` | max disables the read-bandwidth decision and limit dimension. |
| `IO_READ_IOPS` | `1000` | `dynamic` | 0 disables the read-IOPS decision and limit dimension. |
| `IO_RELEASE_THRESHOLD` | `40` | `dynamic` | — |
| `IO_REMEDIATION_ENABLED` | `false` | `dynamic` | false disables starvation remediation. |
| `IO_REVERT_ON_NORMAL` | `true` | `dynamic` | — |
| `IO_STARVATION_CHECK_INTERVAL` | `30` | `dynamic` | — |
| `IO_STARVATION_THRESHOLD` | `300` | `dynamic` | — |
| `IO_THRESHOLD` | `75` | `dynamic` | — |
| `IO_THRESHOLD_DURATION` | `0` | `dynamic` | 0 makes I/O threshold activation immediate. |
| `IO_USER_EXCLUDE_LIST` | `(empty)` | `dynamic` | Empty excludes nobody from I/O eligibility. |
| `IO_USER_INCLUDE_LIST` | `(empty)` | `dynamic` | Empty includes every non-excluded user for I/O eligibility. |
| `IO_WRITE_BPS` | `50M` | `dynamic` | max disables the write-bandwidth decision and limit dimension. |
| `IO_WRITE_IOPS` | `500` | `dynamic` | 0 disables the write-IOPS decision and limit dimension. |
| `LIMIT_HOOK_ENABLED` | `false` | `dynamic` | false disables script and URL hook delivery. |
| `LIMIT_HOOK_SCRIPT` | `(empty)` | `dynamic` | — |
| `LIMIT_HOOK_TIMEOUT` | `10` | `dynamic` | — |
| `LIMIT_HOOK_URL` | `(empty)` | `dynamic` | — |
| `LOG_FILE` | `/var/log/resman.log` | `restart-required` | — |
| `LOG_LEVEL` | `INFO` | `dynamic` | — |
| `LOG_MAX_SIZE` | `10485760` | `restart-required` | — |
| `MCP_ALLOW_WRITE_OPS` | `false` | `restart-required` | false omits manual limit tools and rejects configuration writes. |
| `MCP_AUTH_TOKEN` | `(empty)` | `restart-required` | Empty is valid only for stdio; HTTP transport requires a token. |
| `MCP_ENABLED` | `false` | `restart-required` | false creates no MCP server. |
| `MCP_HTTP_HOST` | `127.0.0.1` | `restart-required` | — |
| `MCP_HTTP_PORT` | `1969` | `restart-required` | — |
| `MCP_LOG_LEVEL` | `INFO` | `restart-required` | — |
| `MCP_SHUTDOWN_TIMEOUT` | `10` | `dynamic` | — |
| `MCP_TLS_CA_FILE` | `(empty)` | `restart-required` | Empty disables client-certificate authentication; the bearer token is still required over HTTP. |
| `MCP_TLS_CERT_FILE` | `/etc/resman/tls/server.crt` | `restart-required` | — |
| `MCP_TLS_ENABLED` | `true` | `restart-required` | — |
| `MCP_TLS_KEY_FILE` | `/etc/resman/tls/server.key` | `restart-required` | — |
| `MCP_TLS_MIN_VERSION` | `1.3` | `restart-required` | — |
| `MCP_TRANSPORT` | `stdio` | `restart-required` | stdio is local and creates no network listener. |
| `METRICS_CACHE_TTL` | `15` | `dynamic` | — |
| `METRICS_DB_ENABLED` | `false` | `restart-required` | false disables metrics persistence and database-backed MCP queries. |
| `METRICS_DB_PATH` | `/var/lib/resman/metrics.db` | `restart-required` | — |
| `METRICS_DB_RETENTION_DAYS` | `30` | `dynamic` | — |
| `METRICS_DB_WRITE_INTERVAL` | `30` | `restart-required` | — |
| `METRICS_REFRESH_INTERVAL` | `30` | `dynamic` | — |
| `MIN_ACTIVE_TIME` | `60` | `dynamic` | — |
| `PATTERN_CONFIDENCE_THRESHOLD` | `0.7` | `dynamic` | — |
| `PATTERN_HISTORY_HOURS` | `168` | `dynamic` | — |
| `PATTERN_MIN_SAMPLES` | `24` | `dynamic` | — |
| `POLLING_INTERVAL` | `30` | `dynamic` | — |
| `PROCESS_EXCLUDE_LIST` | `^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$` | `dynamic` | Empty excludes no process from enforcement. |
| `PROCESS_MIN_AGE_SECONDS` | `60` | `dynamic` | — |
| `PROMETHEUS_AUTH_PASSWORD_FILE` | `(empty)` | `restart-required` | — |
| `PROMETHEUS_AUTH_TYPE` | `none` | `restart-required` | none disables Prometheus authentication. |
| `PROMETHEUS_AUTH_USERNAME` | `(empty)` | `restart-required` | — |
| `PROMETHEUS_JWT_AUDIENCE` | `prometheus` | `restart-required` | — |
| `PROMETHEUS_JWT_ISSUER` | `resman` | `restart-required` | — |
| `PROMETHEUS_JWT_SECRET_FILE` | `(empty)` | `restart-required` | — |
| `PROMETHEUS_METRICS_BIND_HOST` | `127.0.0.1` | `restart-required` | — |
| `PROMETHEUS_METRICS_BIND_PORT` | `1974` | `restart-required` | — |
| `PROMETHEUS_TLS_CA_FILE` | `(empty)` | `restart-required` | Empty disables client-certificate authentication. |
| `PROMETHEUS_TLS_CERT_FILE` | `/etc/resman/tls/server.crt` | `restart-required` | — |
| `PROMETHEUS_TLS_ENABLED` | `false` | `restart-required` | false serves plain HTTP when the exporter is enabled; keep the default loopback bind unless transport security is configured. |
| `PROMETHEUS_TLS_KEY_FILE` | `/etc/resman/tls/server.key` | `restart-required` | — |
| `PROMETHEUS_TLS_MIN_VERSION` | `1.2` | `restart-required` | — |
| `PSI_CPU_STALL_THRESHOLD` | `50000` | `dynamic` | — |
| `PSI_EVENT_DRIVEN` | `false` | `dynamic` | false uses the polling control loop instead of PSI-triggered cycles. |
| `PSI_FALLBACK_INTERVAL` | `300` | `dynamic` | — |
| `PSI_IO_STALL_THRESHOLD` | `50000` | `dynamic` | — |
| `PSI_WINDOW_US` | `1000000` | `dynamic` | — |
| `RAM_HIGH_RATIO` | `0.8` | `dynamic` | 0 disables memory.high while memory.max remains enforced. |
| `RAM_LIMIT_ENABLED` | `false` | `dynamic` | false disables RAM enforcement while observation remains available. |
| `RAM_QUOTA_PER_USER` | `512M` | `dynamic` | — |
| `RAM_RELEASE_THRESHOLD` | `40` | `dynamic` | — |
| `RAM_THRESHOLD` | `75` | `dynamic` | — |
| `RAM_USER_EXCLUDE_LIST` | `(empty)` | `dynamic` | Empty excludes nobody from RAM eligibility. |
| `RAM_USER_INCLUDE_LIST` | `(empty)` | `dynamic` | Empty includes every non-excluded user for RAM eligibility. |
| `SERVER_ROLE` | `(empty)` | `restart-required` | Empty omits an operator-defined role value. |
| `SYSTEM_UID_MAX` | `host /proc/sys/kernel/pid_max (fallback 60000)` | `dynamic` | — |
| `SYSTEM_UID_MIN` | `1000` | `dynamic` | — |
| `USERNAME_CACHE_TTL` | `60` | `dynamic` | — |
| `USER_EXCLUDE_LIST` | `(empty)` | `dynamic` | Empty excludes nobody from CPU eligibility. |
| `USER_INCLUDE_LIST` | `(empty)` | `dynamic` | Empty makes no user eligible for CPU Points enforcement; observation remains active. Use .* for every non-excluded user. |
| `USE_SYSLOG` | `false` | `restart-required` | — |
