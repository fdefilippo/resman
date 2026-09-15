# Grafana Dashboard Guide

## Overview

The ResMan Grafana dashboard shows kernel pressure, the limits ResMan currently applies, memory and I/O of the monitored users, and host load against the available cores.

**Dashboard File:** `docs/dashboard-grafana-operations.json`

**Compatibility:** Grafana 8.x+

---

## Installation

### 1. Import Dashboard

```bash
# Via Grafana UI
1. Open Grafana
2. Go to Dashboards → Import
3. Upload `docs/dashboard-grafana-operations.json`
4. Select Prometheus datasource
5. Click Import
```

### 2. Via Grafana CLI

```bash
grafana-cli --pluginUrl https://github.com/fdefilippo/resman/raw/main/docs/dashboard-grafana-operations.json dashboards install resman
```

---

## Dashboard Panels

The dashboard is organized in eight rows. Every query is filtered by the `cluster`,
`environment`, `server_role` and `hostname` variables; per-user series also honor
`username`. The previous layout is archived as
`docs/archive/dashboard-grafana-operations-2026-09-06.json`.

### Row 1: Pressure (PSI)

| Panel | Metric | Description |
|-------|--------|-------------|
| **CPU pressure** | `rate(resman_psi_events_total{type="cpu", scope="system"}) * 60` | Kernel PSI events per minute; shows `PSI inactive` when `PSI_EVENT_DRIVEN` is off or no event was ever recorded |
| **I/O pressure** | `rate(resman_psi_events_total{type="io", scope="system"}) * 60` | Same for the I/O stall monitor |
| **Memory pressure** | `rate(resman_user_memory_high_breaches_total) * 60` | ResMan exports no memory PSI value; this gauge counts `memory.high` breaches per minute in authoritative user slices and shows `no RAM enforcement` when no counter exists |

### Row 2: Limits

| Panel | Metric | Description |
|-------|--------|-------------|
| **CPU limits** | `resman_cpu_actively_limited_users_count`, `resman_cpu_eligible_users_count` | Users currently limited against users eligible under the CPU policy |
| **RAM limits** | `resman_ram_eligible_users_count`, `resman_resource_limits_active`, `increase(resman_user_memory_high_breaches_total)` | RAM-eligible users, whether RAM or I/O limits are active (0/1) and soft-limit breaches |
| **I/O limits** | `resman_io_eligible_users_count`, `resman_resource_limits_active` | I/O-eligible users and the RAM/I/O active state |

Read this row together with `resman_enforcement_mode`: in `observation_only` the
daemon records intent only and no user is ever counted as limited. Use
`resman_enforcement_action_state` to distinguish the requested intent from the
acknowledged action and its bounded block reason.

### Row 3: Memory and I/O

| Panel | Metric | Description |
|-------|--------|-------------|
| **RAM of monitored processes** | `resman_all_users_memory_usage_bytes`, `resman_ram_eligible_users_memory_usage_bytes`, `sum(resman_cgroup_memory_usage_bytes)` | Process-derived memory of all monitored users, of RAM-eligible users, and `memory.current` of authoritative user slices |
| **I/O by user** | `rate(resman_user_io_read_bytes_total) + rate(resman_user_io_write_bytes_total)` | Block-device throughput per user, top 10 |
| **RAM by user** | `resman_user_memory_usage_bytes` | Process-derived memory per user, top 10 |

### Row 4: Load

| Panel | Metric | Description |
|-------|--------|-------------|
| **Load average vs online cores** | `resman_system_load_average`, `resman_cpu_total_cores` | One-minute load average with the core count drawn as a dashed threshold line |
| **Load per core** | `resman_system_load_average / resman_cpu_total_cores` | Gauge; yellow above 0.7, red above 1.0 |

### Row 5: Enforcement and coverage

| Panel | Metric | Description |
|-------|--------|-------------|
| **Enforcement mode** | `resman_enforcement_mode{mode} == 1` | `observation_only` or `systemd_native`; the key to reading the Limits row |
| **Requested versus applied action** | `resman_enforcement_action_state == 1` | Latest bounded tuple of mode, requested intent, acknowledged action and block reason |
| **CPU Points denominator** | `resman_cpu_points_denominator_state{state} == 1` | `complete` means every active sibling slice was confirmed this cycle |
| **CPU Points delivery** | `resman_cpu_points_delivery_state{state} == 1` | `available`, `throttled_parent` or `unavailable` |
| **CPU / RAM / I/O coverage by user** | `resman_user_cpu_points_process_coverage`, `resman_user_ram_cgroup_coverage`, `resman_user_io_coverage` (`coverage` label) | Per-user authority coverage: `complete`, `partial`, `refused`, `unavailable` |

### Row 6: CPU Points delivery

| Panel | Metric | Description |
|-------|--------|-------------|
| **Parent: programmed vs delivered cores** | `parent_quota / parent_period`, `parent_usage_delta / interval`, `parent_throttled_delta / interval` | Nominal parent capacity against measured delivery and CFS throttling |
| **Delivered cores by user slice** | `resman_user_cpu_points_leaf_usage_microseconds_delta / interval` | Measured CPU time per slice over the same interval as the parent, top 10 |
| **Programmed weights** | `resman_cpu_points_programmed_*_weight*`, `resman_cpu_points_observed_sibling_weight_sum` | Programmed against observed sibling weights |
| **Points budget** | `resman_cpu_points_reserve`, `..._nominal_parent_pool`, `..._root_entitlement`, `..._best_effort_entitlement`, `..._applied_guarantee_total` | The configured budget and the guarantees in the last published plan |

### Row 7: Authority and daemon health

| Panel | Metric | Description |
|-------|--------|-------------|
| **Observation-only state** | `resman_enforcement_mode{mode="observation_only"} == 1` | Hosts where no authoritative enforcement adapter is available |
| **Errors by component** | `increase(resman_errors_total{component, error_type})` | Error increments per window |
| **Limit hooks** | `resman_limit_hook_executions_total{hook_type, outcome}`, `..._queue_depth`, `..._queue_capacity`, `..._in_flight` | Hook delivery health |
| **Control cycle duration p95** | `histogram_quantile(0.95, rate(resman_control_cycle_duration_seconds_bucket))` | Daemon latency |
| **Control cycles by trigger** | `increase(resman_control_cycle_triggers_total{trigger})` | Polling against PSI-triggered cycles |

Rows 5 and 6 depend on the CPU Points telemetry retained in schema 9; on an
earlier release those panels show no data.

### Row 8: Weighted I/O policy

| Panel | Metric | Description |
|-------|--------|-------------|
| **Weighted I/O lifecycle** | `resman_io_device_weight_state{state} == 1` | Distinguishes automatic `refused_observation` re-evaluation from `refused_intervention`, which requires host correction followed by reload or restart |
| **Weighted I/O proof and application** | `resman_io_device_weight_functionally_accepted`, `..._programmed`, `..._read_back`, `..._effect_qualified` | Keeps functional proof, complete production application and retained contention qualification separate |
| **Weighted I/O attempts and partial coverage** | `increase(resman_io_device_weight_classification_attempts_total)`, `increase(..._probe_attempts_total)`, `resman_io_device_weight_partial_users` | Shows read-only post-READY classifier calls separately from mutating transient probes and counts applied slices with partial UID workload coverage; the full API also exports mechanism, verification states, complete/unavailable coverage, denominator, exact value path and retry timing |

When the feature is disabled, the lifecycle panel reports `disabled` and the other
weighted-I/O gauges remain zero. A functionally accepted probe does not mean a
production plan is programmed, and `effect_qualified` never authorizes the feature.

## User Policy Configuration Impact

`USER_INCLUDE_LIST` and `USER_EXCLUDE_LIST` control CPU-limit eligibility only.
They do not filter observation or remove users from Prometheus metrics. Empty CPU
include lists make nobody eligible for CPU limiting; use `USER_INCLUDE_LIST=.*`
to make every non-excluded user eligible.

RAM and I/O use their own include and exclude lists. Empty RAM or I/O include
lists select every non-excluded user for that resource. See
[`CONFIGURATION.md`](CONFIGURATION.md) for the generated lifecycle and
empty-value contract.

The dashboard continues to show observed users in the configured UID range.
Policy changes affect eligibility and actively-limited status panels, not whether
an active user is observable.

---

## Variables

The dashboard includes the following template variables:

| Variable | Label | Query | Multi-Select |
|----------|-------|-------|--------------|
| `DS_PROMETHEUS` | Datasource | Prometheus datasource selector | No |
| `cluster` | Cluster | `label_values(resman_cpu_total_usage_percent, cluster)` | Yes |
| `environment` | Environment | `label_values(resman_cpu_total_usage_percent{cluster=~"$cluster"}, environment)` | Yes |
| `server_role` | Server role | `label_values(resman_cpu_total_usage_percent{cluster=~"$cluster", environment=~"$environment"}, server_role)` | Yes |
| `hostname` | Hostname | `label_values(resman_cpu_total_usage_percent{cluster=~"$cluster", environment=~"$environment", server_role=~"$server_role"}, hostname)` | Yes |
| `username` | Username | `label_values(resman_user_cpu_usage_percent{cluster=~"$cluster", environment=~"$environment", server_role=~"$server_role", hostname=~"$hostname"}, username)` | Yes |

### Using Variables

**Filter by specific users:**
1. Click on the `Username` dropdown
2. Select one or more users
3. All per-user panels will update to show only selected users

**Change the monitored scope:**
1. Select the cluster and environment
2. Narrow the view by server role and hostname when needed
3. Use Grafana's global time picker to change the time range

---

## Prometheus Queries

### Example Queries

**Top 5 users by CPU:**
```promql
topk(5, resman_user_cpu_usage_percent)
```

**Total memory used by all users:**
```promql
sum(resman_user_memory_usage_bytes)
```

**Alert: User memory exceeds 2GB:**
```promql
resman_user_memory_usage_bytes > 2147483648
```

**Processes for specific user:**
```promql
resman_user_process_count{username="francesco"}
```

**Users with active limits:**
```promql
resman_user_cpu_limit_active == 1
```

**Average CPU usage in last hour:**
```promql
avg_over_time(resman_all_users_cpu_usage_percent[1h])
```

### Alerting Rules

Example alerting rules are available in `docs/alerting-rules.yml`.

**Example: High CPU Usage Alert**
```yaml
groups:
  - name: resman
    rules:
      - alert: HighUserCPUUsage
        expr: resman_user_cpu_usage_percent > 80
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "High CPU usage for user {{ $labels.username }}"
          description: "User {{ $labels.username }} has CPU usage above 80% (current: {{ $value }}%)"
```

---

## Troubleshooting

### User Not Appearing in Dashboard

**Problem:** A user is not showing up in the dashboard metrics.

**Possible Causes:**
1. User has no active processes
2. User UID is below `SYSTEM_UID_MIN` or above the runtime `SYSTEM_UID_MAX`
3. Prometheus has not scraped the current decision sample yet

**Solution:**
```bash
# Check the observed user and UID range
grep -E "USER_(INCLUDE|EXCLUDE)_LIST" /etc/resman/resman.conf

# Check if user has processes
ps -u username

# Check user UID
id username
```

### Metrics Not Updating

**Problem:** Dashboard metrics are stale.

**Possible Causes:**
1. Prometheus scrape interval too long
2. ResMan not running
3. Prometheus exporter disabled

**Solution:**
```bash
# Check ResMan status
systemctl status resman

# Check Prometheus exporter
curl http://localhost:1974/metrics

# Check Prometheus scrape config
# prometheus.yml should have:
# - job_name: 'resman'
#   static_configs:
#     - targets: ['localhost:1974']
#   scrape_interval: 15s
```

### Empty Dashboard

**Problem:** All panels show "No data".

**Possible Causes:**
1. Wrong Prometheus datasource selected
2. No active users on system
3. Metrics not being exported

**Solution:**
```bash
# Verify Prometheus connection
# In Grafana: Configuration → Data Sources → Test

# Check if metrics exist
curl http://localhost:1974/metrics | grep resman

# Check active users
ps aux | awk '{print $1}' | sort | uniq
```

---

## Customization

### Adding New Panels

1. Click "Edit" on dashboard
2. Click "Add panel"
3. Enter Prometheus query
4. Configure visualization type
5. Save panel

### Modifying Existing Panels

1. Click on panel title
2. Click "Edit"
3. Modify query or visualization settings
4. Click "Apply"
5. Save dashboard

### Exporting Dashboard

```bash
# Via Grafana UI
1. Open dashboard
2. Click dashboard settings (gear icon)
3. Click "JSON Model"
4. Copy JSON
5. Save to file

# Via API
curl -H "Authorization: Bearer <token>" \
  http://grafana/api/dashboards/uid/<uid> \
  > dashboard-export.json
```

---

## Best Practices

### 1. Use Template Variables

Use the `cluster`, `environment`, `server_role`, `hostname`, and `username` variables to narrow the same dashboard from fleet level to a single user.

### 2. Set Appropriate Thresholds

Adjust alert thresholds based on your system's capacity and requirements.

### 3. Monitor Limit Activations

Keep an eye on `resman_cpu_limits_activated_total` to understand how often limits enter the active state successfully.

### 4. Track Error Rates

Monitor `resman_errors_total` to catch configuration or runtime issues early.

### 5. Use Time Comparisons

Compare current metrics with historical data using `offset` in queries:

```promql
# Current CPU usage
resman_all_users_cpu_usage_percent

# CPU usage 24 hours ago
resman_all_users_cpu_usage_percent offset 24h
```

---

## See Also

- [Prometheus Queries Documentation](prometheus-queries.md)
- [Alerting Rules](alerting-rules.yml)
- [MCP Server Documentation](MCP-README.md)
- [Technical Specification](TECHNICAL-SPECIFICATION.md)

---

**Dashboard Version:** 1.0 (Compatible with ResMan v1.3.0+)
**Last Updated:** March 2026
