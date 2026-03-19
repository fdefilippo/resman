# Grafana Dashboard Guide

## Overview

The CPU Manager Go Grafana dashboard provides real-time visualization of CPU usage, memory consumption, and CPU limits for all monitored users.

**Dashboard File:** `docs/dashboard-grafana.json`

**Compatibility:** Grafana 8.x+

---

## Installation

### 1. Import Dashboard

```bash
# Via Grafana UI
1. Open Grafana
2. Go to Dashboards → Import
3. Upload `docs/dashboard-grafana.json`
4. Select Prometheus datasource
5. Click Import
```

### 2. Via Grafana CLI

```bash
grafana-cli --pluginUrl https://github.com/fdefilippo/cpu-manager-go/raw/main/docs/dashboard-grafana.json dashboards install cpu-manager-go
```

---

## Dashboard Panels

### System Metrics

| Panel | Metric | Description |
|-------|--------|-------------|
| **Total CPU Usage** | `cpu_manager_cpu_total_usage_percent` | Overall system CPU usage |
| **User CPU Usage** | `cpu_manager_cpu_user_usage_percent` | CPU usage by non-system users |
| **Memory Usage** | `cpu_manager_memory_usage_megabytes` | Total system memory usage |
| **Active Users** | `cpu_manager_active_users_count` | Number of active non-system users |

### Per-User Metrics

| Panel | Metric | Description |
|-------|--------|-------------|
| **CPU Usage per User** | `cpu_manager_user_cpu_usage_percent{uid, username}` | CPU usage breakdown by user |
| **Memory per User** | `cpu_manager_user_memory_usage_bytes{uid, username}` | Memory consumption by user |
| **Processes per User** | `cpu_manager_user_process_count{uid, username}` | Number of processes per user |

### Limit Status

| Panel | Metric | Description |
|-------|--------|-------------|
| **Limits Active** | `cpu_manager_limits_active` | Whether CPU limits are currently applied (1=active) |
| **Limited Users** | `cpu_manager_limited_users_count` | Number of users with active CPU limits |
| **User Limit Status** | `cpu_manager_user_cpu_limited{uid, username}` | Per-user limit status (1=limited) |

### Control Cycle Performance

| Panel | Metric | Description |
|-------|--------|-------------|
| **Limits Activated** | `increase(cpu_manager_limits_activated_total[1h])` | Count of limit activations in last hour |
| **Limits Deactivated** | `increase(cpu_manager_limits_deactivated_total[1h])` | Count of limit deactivations in last hour |
| **Avg Cycle Duration** | `rate(cpu_manager_control_cycle_duration_seconds_sum[5m]) / rate(cpu_manager_control_cycle_duration_seconds_count[5m])` | Average control cycle duration |
| **Error Rate** | `sum by (component) (rate(cpu_manager_errors_total[5m]))` | Errors by component |

---

## User Filter Configuration Impact

### USER_INCLUDE_LIST

When `USER_INCLUDE_LIST` is configured, **only users matching the regex patterns** will appear in the dashboard metrics.

**Example:**
```bash
USER_INCLUDE_LIST=^www.*,^app-.*
```

**Result:**
- ✅ `www-data`, `www-run`, `app-prod`, `app-dev` will appear in metrics
- ❌ `francesco`, `mysql`, `nobody` will NOT appear in metrics

**Dashboard Impact:**
- User dropdown will only show matching users
- Per-user panels will only display data for matching users
- Active users count will only count matching users

### USER_EXCLUDE_LIST

When `USER_EXCLUDE_LIST` is configured, **users matching the regex patterns** will be excluded from dashboard metrics.

**Example:**
```bash
USER_EXCLUDE_LIST=^test-.*,^dev-.*,francesco
```

**Result:**
- ❌ `test-user`, `dev-web`, `francesco` will NOT appear in metrics
- ✅ `www-data`, `mysql`, `app-prod` will appear in metrics

**Dashboard Impact:**
- Excluded users will not appear in any panels
- Metrics are calculated excluding filtered users
- User dropdown will not show excluded users

### Combined Configuration

When both lists are configured:
1. **USER_INCLUDE_LIST** filters users to include (whitelist)
2. **USER_EXCLUDE_LIST** removes users from the included set (blacklist)

**Example:**
```bash
USER_INCLUDE_LIST=^www-.*
USER_EXCLUDE_LIST=^www-test-.*
```

**Result:**
- ✅ `www-prod`, `www-data` (included, not excluded)
- ❌ `www-test-dev` (excluded by exclude list)
- ❌ `francesco` (not in include list)

---

## Variables

The dashboard includes the following template variables:

| Variable | Label | Query | Multi-Select |
|----------|-------|-------|--------------|
| `uid` | User UID | `label_values(cpu_manager_user_cpu_usage_percent, uid)` | ✅ Yes |
| `username` | Username | `label_values(cpu_manager_user_memory_usage_bytes, username)` | ✅ Yes |
| `time_range` | Time Range | Manual: 1h, 6h, 12h, 24h, 7d | ❌ No |

### Using Variables

**Filter by specific users:**
1. Click on the `Username` dropdown
2. Select one or more users
3. All per-user panels will update to show only selected users

**Change time range:**
1. Use the `Time Range` dropdown
2. Or use Grafana's global time picker

---

## Prometheus Queries

### Example Queries

**Top 5 users by CPU:**
```promql
topk(5, cpu_manager_user_cpu_usage_percent)
```

**Total memory used by all users:**
```promql
sum(cpu_manager_user_memory_usage_bytes)
```

**Alert: User memory exceeds 2GB:**
```promql
cpu_manager_user_memory_usage_bytes > 2147483648
```

**Processes for specific user:**
```promql
cpu_manager_user_process_count{username="francesco"}
```

**Users with active limits:**
```promql
cpu_manager_user_cpu_limited == 1
```

**Average CPU usage in last hour:**
```promql
avg_over_time(cpu_manager_cpu_user_usage_percent[1h])
```

### Alerting Rules

Example alerting rules are available in `docs/alerting-rules.yml`.

**Example: High CPU Usage Alert**
```yaml
groups:
  - name: cpu-manager
    rules:
      - alert: HighUserCPUUsage
        expr: cpu_manager_user_cpu_usage_percent > 80
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
1. User is filtered by `USER_INCLUDE_LIST`
2. User is filtered by `USER_EXCLUDE_LIST`
3. User has no active processes
4. User UID is below `SYSTEM_UID_MIN` (default: 1000)

**Solution:**
```bash
# Check current filter configuration
grep -E "USER_(INCLUDE|EXCLUDE)_LIST" /etc/cpu-manager.conf

# Check if user has processes
ps -u username

# Check user UID
id username
```

### Metrics Not Updating

**Problem:** Dashboard metrics are stale.

**Possible Causes:**
1. Prometheus scrape interval too long
2. CPU Manager not running
3. Prometheus exporter disabled

**Solution:**
```bash
# Check CPU Manager status
systemctl status cpu-manager

# Check Prometheus exporter
curl http://localhost:1974/metrics

# Check Prometheus scrape config
# prometheus.yml should have:
# - job_name: 'cpu-manager'
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
curl http://localhost:1974/metrics | grep cpu_manager

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

Leverage the `username` and `uid` variables to create flexible dashboards that work across different environments.

### 2. Set Appropriate Thresholds

Adjust alert thresholds based on your system's capacity and requirements.

### 3. Monitor Limit Activations

Keep an eye on `cpu_manager_limits_activated_total` to understand how often limits are being applied.

### 4. Track Error Rates

Monitor `cpu_manager_errors_total` to catch configuration or runtime issues early.

### 5. Use Time Comparisons

Compare current metrics with historical data using `offset` in queries:

```promql
# Current CPU usage
cpu_manager_cpu_user_usage_percent

# CPU usage 24 hours ago
cpu_manager_cpu_user_usage_percent offset 24h
```

---

## See Also

- [Prometheus Queries Documentation](prometheus-queries.md)
- [Alerting Rules](alerting-rules.yml)
- [MCP Server Documentation](MCP-README.md)
- [Technical Specification](TECHNICAL-SPECIFICATION.md)

---

**Dashboard Version:** 1.0 (Compatible with CPU Manager Go v1.3.0+)  
**Last Updated:** March 2026
