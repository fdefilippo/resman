# ResMan configuration reference

This file is generated from `config.Config`, `DefaultConfig`, and the authoritative lifecycle table. Regenerate it with `go run ./scripts/generate-config-reference`; do not edit the table by hand.

`dynamic` keys are applied by hot reload. `restart-required` keys keep their effective value and report a rejected reload until the daemon restarts. An em dash means the key has no special empty or disabled contract beyond its literal value. The copyable, commented configuration is [`config/resman.conf.example`](../config/resman.conf.example).

## Source precedence

The authoritative order is `default < file < environment`: runtime defaults are loaded first, the authored configuration file overrides them, and environment variables override both. Editing a file value while an environment override exists does not change the effective value.

Environment-shadowing remedy: Inspect the running service with `systemctl show resman --property=Environment --property=EnvironmentFiles --property=DropInPaths` and `systemctl cat resman`. Modify the authoritative drop-in with `systemctl edit resman`, or modify the referenced environment file or configuration-management source. Reload the systemd unit definition where required, then restart `resman`; `systemctl reload resman` alone cannot change the environment of the running process.

| Key | Kind | Runtime default | Lifecycle | Sensitive | Editable | Constraint | Empty, disabled, or special value | Remedy |
|---|---|---|---|---|---|---|---|---|
| `AUTODETECT_PATTERNS` | `boolean` | `false` | `dynamic` | `false` | `true` | — | false disables workload-pattern classification and RAM policy selection. | Correct the authored value and submit the complete candidate for validation. |
| `BATCH_NIGHT_RAM_QUOTA` | `string` | `4G` | `dynamic` | `false` | `true` | format byte-quota | — | Correct the authored value and submit the complete candidate for validation. |
| `BLACKOUT` | `string` | `(empty)` | `dynamic` | `false` | `true` | format blackout-timeframes | Empty means no blackout; enforcement is always permitted by schedule. | Correct the authored value and submit the complete candidate for validation. |
| `CGROUP_BASE` | `string` | `resman` | `restart-required` | `false` | `true` | format relative-cgroup-path | — | Persist the value and restart resman for it to become effective. |
| `CGROUP_OPERATION_TIMEOUT` | `integer` | `5` | `dynamic` | `false` | `true` | min 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `CGROUP_ROOT` | `string` | `/sys/fs/cgroup` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `CPU_BEST_EFFORT_POINTS` | `integer` | `100` | `dynamic` | `false` | `true` | min 1; max 1000 | One aggregate entitlement shared by every eligible username absent from CPU_POINTS_FILE. | Correct the authored value and submit the complete candidate for validation. |
| `CPU_POINTS_FILE` | `string` | `/etc/resman/cpu-points.map` | `restart-required` | `false` | `false` | format absolute-clean-path | Absolute restart-required path to the strict direct username guarantee map. | Change the policy-map path in the configuration file and restart resman; MCP map updates target the currently authoritative path only. |
| `CPU_RELEASE_THRESHOLD` | `integer` | `40` | `dynamic` | `false` | `true` | min 1; max 100 | — | Correct the authored value and submit the complete candidate for validation. |
| `CPU_RESERVE_POINTS` | `integer` | `100` | `dynamic` | `false` | `true` | min 0; max 990 | 0 removes nominal headroom outside the finite ResMan CPU parent; it does not create physical isolation. | Correct the authored value and submit the complete candidate for validation. |
| `CPU_THRESHOLD` | `integer` | `75` | `dynamic` | `false` | `true` | min 1; max 100 | — | Correct the authored value and submit the complete candidate for validation. |
| `CPU_THRESHOLD_DURATION` | `integer` | `90` | `dynamic` | `false` | `true` | min 0 | 0 makes CPU threshold activation immediate after a valid sample. | Correct the authored value and submit the complete candidate for validation. |
| `CREATED_CGROUPS_FILE` | `string` | `/run/resman-cgroups.txt` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `DAEMON_SHUTDOWN_TIMEOUT` | `integer` | `60` | `dynamic` | `false` | `true` | min 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `DISABLE_SWAP` | `boolean` | `false` | `dynamic` | `false` | `true` | — | — | Correct the authored value and submit the complete candidate for validation. |
| `ENABLE_PROMETHEUS` | `boolean` | `false` | `restart-required` | `false` | `true` | — | false creates no Prometheus listener. | Persist the value and restart resman for it to become effective. |
| `IGNORE_SYSTEM_LOAD` | `boolean` | `false` | `dynamic` | `false` | `true` | — | — | Correct the authored value and submit the complete candidate for validation. |
| `INTERACTIVE_RAM_QUOTA` | `string` | `1G` | `dynamic` | `false` | `true` | format byte-quota | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_BOOST_DURATION` | `integer` | `600` | `dynamic` | `false` | `true` | min 1 | 0 is rejected while I/O remediation is enabled. | Correct the authored value and submit the complete candidate for validation. |
| `IO_BOOST_MAX_PER_HOUR` | `integer` | `3` | `dynamic` | `false` | `true` | min 0 | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_BOOST_MULTIPLIER` | `number` | `2` | `dynamic` | `false` | `true` | min 0 | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_DEVICE_FILTER` | `string` | `all` | `dynamic` | `false` | `true` | format all-or-device-number | all selects every eligible whole block device. | Correct the authored value and submit the complete candidate for validation. |
| `IO_LIMIT_ENABLED` | `boolean` | `false` | `dynamic` | `false` | `true` | — | false disables I/O enforcement while observation remains available. | Correct the authored value and submit the complete candidate for validation. |
| `IO_PSI_THRESHOLD` | `number` | `50` | `dynamic` | `false` | `true` | min 0; max 100 | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_READ_BPS` | `string` | `100M` | `dynamic` | `false` | `true` | format byte-quota-or-max | max disables the read-bandwidth decision and limit dimension. | Correct the authored value and submit the complete candidate for validation. |
| `IO_READ_IOPS` | `integer` | `1000` | `dynamic` | `false` | `true` | min 0 | 0 disables the read-IOPS decision and limit dimension. | Correct the authored value and submit the complete candidate for validation. |
| `IO_RELEASE_THRESHOLD` | `integer` | `40` | `dynamic` | `false` | `true` | min 1; max 100 | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_REMEDIATION_ENABLED` | `boolean` | `false` | `dynamic` | `false` | `true` | — | false disables starvation remediation. | Correct the authored value and submit the complete candidate for validation. |
| `IO_REVERT_ON_NORMAL` | `boolean` | `true` | `dynamic` | `false` | `true` | — | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_STARVATION_CHECK_INTERVAL` | `integer` | `30` | `dynamic` | `false` | `true` | min 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_STARVATION_THRESHOLD` | `integer` | `300` | `dynamic` | `false` | `true` | min 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_THRESHOLD` | `integer` | `75` | `dynamic` | `false` | `true` | min 1; max 100 | — | Correct the authored value and submit the complete candidate for validation. |
| `IO_THRESHOLD_DURATION` | `integer` | `0` | `dynamic` | `false` | `true` | min 0 | 0 makes I/O threshold activation immediate. | Correct the authored value and submit the complete candidate for validation. |
| `IO_USER_EXCLUDE_LIST` | `string_list` | `(empty)` | `dynamic` | `false` | `true` | format comma-separated-regex-list | Empty excludes nobody from I/O eligibility. | Correct the authored value and submit the complete candidate for validation. |
| `IO_USER_INCLUDE_LIST` | `string_list` | `(empty)` | `dynamic` | `false` | `true` | format comma-separated-regex-list | Empty includes every non-excluded user for I/O eligibility. | Correct the authored value and submit the complete candidate for validation. |
| `IO_WRITE_BPS` | `string` | `50M` | `dynamic` | `false` | `true` | format byte-quota-or-max | max disables the write-bandwidth decision and limit dimension. | Correct the authored value and submit the complete candidate for validation. |
| `IO_WRITE_IOPS` | `integer` | `500` | `dynamic` | `false` | `true` | min 0 | 0 disables the write-IOPS decision and limit dimension. | Correct the authored value and submit the complete candidate for validation. |
| `LIMIT_HOOK_ENABLED` | `boolean` | `false` | `dynamic` | `false` | `true` | — | false disables script and URL hook delivery. | Correct the authored value and submit the complete candidate for validation. |
| `LIMIT_HOOK_MAX_CONCURRENCY` | `integer` | `2` | `restart-required` | `false` | `true` | min 1 | Fixed number of delivery workers; changing it requires restart. | Persist the value and restart resman for it to become effective. |
| `LIMIT_HOOK_QUEUE_CAPACITY` | `integer` | `64` | `restart-required` | `false` | `true` | min 1 | Fixed pending-delivery capacity; saturation is reported and never blocks enforcement. | Persist the value and restart resman for it to become effective. |
| `LIMIT_HOOK_SCRIPT` | `string` | `(empty)` | `dynamic` | `false` | `true` | format safe-executable-path | Empty disables script delivery and requires script user and group to be empty. | Correct the authored value and submit the complete candidate for validation. |
| `LIMIT_HOOK_SCRIPT_GROUP` | `string` | `(empty)` | `restart-required` | `false` | `true` | — | Required non-root NSS group whenever LIMIT_HOOK_SCRIPT is set. | Persist the value and restart resman for it to become effective. |
| `LIMIT_HOOK_SCRIPT_USER` | `string` | `(empty)` | `restart-required` | `false` | `true` | — | Required non-root NSS username whenever LIMIT_HOOK_SCRIPT is set. | Persist the value and restart resman for it to become effective. |
| `LIMIT_HOOK_TIMEOUT` | `integer` | `10` | `dynamic` | `false` | `true` | min 1 | Full timeout in seconds applied independently to every script or HTTP delivery. | Correct the authored value and submit the complete candidate for validation. |
| `LIMIT_HOOK_URL` | `string` | `(redacted)` | `dynamic` | `true` | `true` | format http-or-https-url | Empty disables HTTP delivery; configured requests use a dedicated no-retry client. | Correct the authored value and submit the complete candidate for validation. |
| `LOG_FILE` | `string` | `/var/log/resman.log` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `LOG_LEVEL` | `string` | `INFO` | `dynamic` | `false` | `true` | one of DEBUG, INFO, WARN, ERROR | — | Correct the authored value and submit the complete candidate for validation. |
| `LOG_MAX_SIZE` | `integer` | `10485760` | `restart-required` | `false` | `true` | min 1 | — | Persist the value and restart resman for it to become effective. |
| `MCP_ALLOW_WRITE_OPS` | `boolean` | `false` | `restart-required` | `false` | `true` | — | false omits manual limit tools and rejects configuration writes. | Persist the value and restart resman for it to become effective. |
| `MCP_AUTH_TOKEN` | `string` | `(redacted)` | `restart-required` | `true` | `true` | — | Empty is valid only for stdio; HTTP transport requires a token. | Persist the value and restart resman for it to become effective. |
| `MCP_ENABLED` | `boolean` | `false` | `restart-required` | `false` | `true` | — | false creates no MCP server. | Persist the value and restart resman for it to become effective. |
| `MCP_HTTP_HOST` | `string` | `127.0.0.1` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `MCP_HTTP_PORT` | `integer` | `1969` | `restart-required` | `false` | `true` | min 1; max 65535 | — | Persist the value and restart resman for it to become effective. |
| `MCP_LOG_LEVEL` | `string` | `INFO` | `restart-required` | `false` | `true` | one of DEBUG, INFO, WARN, ERROR | — | Persist the value and restart resman for it to become effective. |
| `MCP_SHUTDOWN_TIMEOUT` | `integer` | `10` | `restart-required` | `false` | `true` | min 1 | — | Persist the value and restart resman for it to become effective. |
| `MCP_TLS_CA_FILE` | `string` | `(empty)` | `restart-required` | `false` | `true` | — | Empty disables client-certificate authentication; the bearer token is still required over HTTP. | Persist the value and restart resman for it to become effective. |
| `MCP_TLS_CERT_FILE` | `string` | `/etc/resman/tls/server.crt` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `MCP_TLS_ENABLED` | `boolean` | `true` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `MCP_TLS_KEY_FILE` | `string` | `/etc/resman/tls/server.key` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `MCP_TLS_MIN_VERSION` | `string` | `1.3` | `restart-required` | `false` | `true` | one of 1.0, 1.1, 1.2, 1.3 | — | Persist the value and restart resman for it to become effective. |
| `MCP_TRANSPORT` | `string` | `stdio` | `restart-required` | `false` | `true` | one of stdio, http | stdio is local and creates no network listener. | Persist the value and restart resman for it to become effective. |
| `METRICS_CACHE_TTL` | `integer` | `15` | `dynamic` | `false` | `true` | min 1 | Controls observation value reuse only; control-cycle host CPU decisions use an independent uncached /proc/stat stream. | Correct the authored value and submit the complete candidate for validation. |
| `METRICS_DB_ENABLED` | `boolean` | `false` | `restart-required` | `false` | `true` | — | false disables metrics persistence and database-backed MCP queries. | Persist the value and restart resman for it to become effective. |
| `METRICS_DB_PATH` | `string` | `/var/lib/resman/metrics.db` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `METRICS_DB_RETENTION_DAYS` | `integer` | `30` | `dynamic` | `false` | `true` | min 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `METRICS_DB_WRITE_INTERVAL` | `integer` | `30` | `restart-required` | `false` | `true` | min 5 | — | Persist the value and restart resman for it to become effective. |
| `METRICS_REFRESH_INTERVAL` | `integer` | `30` | `dynamic` | `false` | `true` | min 5 | — | Correct the authored value and submit the complete candidate for validation. |
| `MIN_ACTIVE_TIME` | `integer` | `60` | `dynamic` | `false` | `true` | — | — | Correct the authored value and submit the complete candidate for validation. |
| `PATTERN_CONFIDENCE_THRESHOLD` | `number` | `0.7` | `dynamic` | `false` | `true` | min 0; max 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `PATTERN_HISTORY_HOURS` | `integer` | `168` | `dynamic` | `false` | `true` | min 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `PATTERN_MIN_SAMPLES` | `integer` | `24` | `dynamic` | `false` | `true` | min 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `POLLING_INTERVAL` | `integer` | `30` | `dynamic` | `false` | `true` | min 5 | — | Correct the authored value and submit the complete candidate for validation. |
| `PROCESS_EXCLUDE_LIST` | `string_list` | `^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$` | `dynamic` | `false` | `true` | format comma-separated-regex-list | Empty excludes no process from enforcement. | Correct the authored value and submit the complete candidate for validation. |
| `PROCESS_MIN_AGE_SECONDS` | `integer` | `60` | `dynamic` | `false` | `true` | min 0 | — | Correct the authored value and submit the complete candidate for validation. |
| `PROMETHEUS_AUTH_PASSWORD_FILE` | `string` | `(empty)` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_AUTH_TYPE` | `string` | `none` | `restart-required` | `false` | `true` | one of none, basic, jwt, both | none disables Prometheus authentication. | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_AUTH_USERNAME` | `string` | `(empty)` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_JWT_AUDIENCE` | `string` | `prometheus` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_JWT_ISSUER` | `string` | `resman` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_JWT_SECRET_FILE` | `string` | `(empty)` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_METRICS_BIND_HOST` | `string` | `127.0.0.1` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_METRICS_BIND_PORT` | `integer` | `1974` | `restart-required` | `false` | `true` | min 1; max 65535 | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_TLS_CA_FILE` | `string` | `(empty)` | `restart-required` | `false` | `true` | — | Empty disables client-certificate authentication. | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_TLS_CERT_FILE` | `string` | `/etc/resman/tls/server.crt` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_TLS_ENABLED` | `boolean` | `false` | `restart-required` | `false` | `true` | — | false serves plain HTTP when the exporter is enabled; keep the default loopback bind unless transport security is configured. | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_TLS_KEY_FILE` | `string` | `/etc/resman/tls/server.key` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
| `PROMETHEUS_TLS_MIN_VERSION` | `string` | `1.2` | `restart-required` | `false` | `true` | one of 1.0, 1.1, 1.2, 1.3 | — | Persist the value and restart resman for it to become effective. |
| `PSI_CPU_STALL_THRESHOLD` | `integer` | `50000` | `dynamic` | `false` | `true` | min 0 | — | Correct the authored value and submit the complete candidate for validation. |
| `PSI_EVENT_DRIVEN` | `boolean` | `false` | `dynamic` | `false` | `true` | — | false uses the polling control loop instead of PSI-triggered cycles. | Correct the authored value and submit the complete candidate for validation. |
| `PSI_FALLBACK_INTERVAL` | `integer` | `300` | `dynamic` | `false` | `true` | min 0 | — | Correct the authored value and submit the complete candidate for validation. |
| `PSI_IO_STALL_THRESHOLD` | `integer` | `50000` | `dynamic` | `false` | `true` | min 0 | — | Correct the authored value and submit the complete candidate for validation. |
| `PSI_WINDOW_US` | `integer` | `1000000` | `dynamic` | `false` | `true` | min 0 | — | Correct the authored value and submit the complete candidate for validation. |
| `RAM_HIGH_RATIO` | `number` | `0.8` | `dynamic` | `false` | `true` | min 0; max 1 | 0 disables memory.high while memory.max remains enforced. | Correct the authored value and submit the complete candidate for validation. |
| `RAM_LIMIT_ENABLED` | `boolean` | `false` | `dynamic` | `false` | `true` | — | false disables RAM enforcement while observation remains available. | Correct the authored value and submit the complete candidate for validation. |
| `RAM_QUOTA_PER_USER` | `string` | `512M` | `dynamic` | `false` | `true` | format byte-quota | — | Correct the authored value and submit the complete candidate for validation. |
| `RAM_RELEASE_THRESHOLD` | `integer` | `40` | `dynamic` | `false` | `true` | min 1; max 100 | — | Correct the authored value and submit the complete candidate for validation. |
| `RAM_THRESHOLD` | `integer` | `75` | `dynamic` | `false` | `true` | min 1; max 100 | — | Correct the authored value and submit the complete candidate for validation. |
| `RAM_USER_EXCLUDE_LIST` | `string_list` | `(empty)` | `dynamic` | `false` | `true` | format comma-separated-regex-list | Empty excludes nobody from RAM eligibility. | Correct the authored value and submit the complete candidate for validation. |
| `RAM_USER_INCLUDE_LIST` | `string_list` | `(empty)` | `dynamic` | `false` | `true` | format comma-separated-regex-list | Empty includes every non-excluded user for RAM eligibility. | Correct the authored value and submit the complete candidate for validation. |
| `SERVER_ROLE` | `string` | `(empty)` | `restart-required` | `false` | `true` | — | Empty omits an operator-defined role value. | Persist the value and restart resman for it to become effective. |
| `SYSTEM_UID_MAX` | `integer` | `host /proc/sys/kernel/pid_max (fallback 60000)` | `dynamic` | `false` | `true` | — | — | Correct the authored value and submit the complete candidate for validation. |
| `SYSTEM_UID_MIN` | `integer` | `1000` | `dynamic` | `false` | `true` | min 0 | — | Correct the authored value and submit the complete candidate for validation. |
| `USERNAME_CACHE_TTL` | `integer` | `60` | `dynamic` | `false` | `true` | min 1 | — | Correct the authored value and submit the complete candidate for validation. |
| `USER_EXCLUDE_LIST` | `string_list` | `(empty)` | `dynamic` | `false` | `true` | format comma-separated-regex-list | Empty excludes nobody from CPU eligibility. | Correct the authored value and submit the complete candidate for validation. |
| `USER_INCLUDE_LIST` | `string_list` | `(empty)` | `dynamic` | `false` | `true` | format comma-separated-regex-list | Empty makes no user eligible for CPU Points enforcement; observation remains active. Use .* for every non-excluded user. | Correct the authored value and submit the complete candidate for validation. |
| `USE_SYSLOG` | `boolean` | `false` | `restart-required` | `false` | `true` | — | — | Persist the value and restart resman for it to become effective. |
