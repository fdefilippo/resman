# Prometheus Query Examples for ResMan

This document provides example PromQL queries for monitoring ResMan metrics.

## Table of Contents

- [System Overview](#system-overview)
- [Per-User Metrics](#per-user-metrics)
- [Memory Analysis](#memory-analysis)
- [Process Analysis](#process-analysis)
- [Limit Status](#limit-status)
- [Performance Metrics](#performance-metrics)
- [Error Tracking](#error-tracking)
- [Alerting Queries](#alerting-queries)

---

## System Overview

### Total CPU Usage
```promql
# Current total CPU usage percentage
resman_cpu_total_usage_percent
```

### CPU Usage Trend (1 hour)
```promql
# Total CPU usage over time
resman_cpu_total_usage_percent[1h]
```

The total CPU gauge is the observation stream. An unavailable refresh preserves its
last valid value; it is never overwritten with a fabricated zero. Check the
observation-owned availability before treating the value as fresh:

```promql
# Latest observation refresh produced a comparable host CPU sample
resman_observation_host_cpu_sample_available

# Bounded causes of unavailable observation samples
sum by (reason) (increase(resman_observation_host_cpu_sample_unavailable_total[1h]))
```

Use the separate decision-owned availability series when diagnosing system-load
attribution:

```promql
# Latest control cycle could compare the host CPU jiffy sample
resman_control_cycle_host_cpu_sample_available

# Bounded causes of unavailable decision samples
sum by (reason) (increase(resman_control_cycle_host_cpu_sample_unavailable_total[1h]))
```

### User CPU Usage vs Total
```promql
# Compare total vs user CPU usage
resman_cpu_total_usage_percent
resman_all_users_cpu_usage_percent
```

### System Load Average
```promql
# Current system load (1 minute average)
resman_system_load_average
```

### Load per CPU Core
```promql
# Normalized load per core
resman_system_load_average / resman_cpu_total_cores
```

---

## Per-User Metrics

### Top 5 Users by CPU Usage
```promql
topk(5, resman_user_cpu_usage_percent)
```

### Top 10 Users by Memory Usage
```promql
topk(10, resman_user_memory_usage_bytes)
```

### CPU Usage for Specific User
```promql
# By username
resman_user_cpu_usage_percent{username="francesco"}

# By UID
resman_user_cpu_usage_percent{uid="1000"}
```

### Memory Usage for Specific User
```promql
resman_user_memory_usage_bytes{username="francesco"}
```

### All Users Sorted by CPU (Descending)
```promql
sort_desc(resman_user_cpu_usage_percent)
```

### Users with CPU > 50%
```promql
resman_user_cpu_usage_percent > 50
```

### CPU Usage Rate of Change (5 min)
```promql
# Positive = increasing, Negative = decreasing
deriv(resman_user_cpu_usage_percent[5m])
```

### CPU Usage Average (1 hour) per User
```promql
avg_over_time(resman_user_cpu_usage_percent[1h])
```

---

## Memory Analysis

### Total Memory Used by All Users
```promql
sum(resman_user_memory_usage_bytes)
```

### Total Memory in GB
```promql
sum(resman_user_memory_usage_bytes) / 1024 / 1024 / 1024
```

### Memory Distribution Among Users
```promql
# Percentage of total user memory per user
resman_user_memory_usage_bytes
/ on() group_left() sum(resman_user_memory_usage_bytes) * 100
```

### Memory Growth Rate (per minute)
```promql
# Positive = growing, Negative = shrinking
deriv(resman_user_memory_usage_bytes[5m]) * 60
```

### Users Using More Than 1GB Memory
```promql
resman_user_memory_usage_bytes > 1073741824
```

### Users Using More Than 2GB Memory
```promql
resman_user_memory_usage_bytes > 2147483648
```

### Memory per User (Human Readable)
```promql
# In MB
resman_user_memory_usage_bytes / 1024 / 1024
```

---

## Process Analysis

### Total Processes Across All Users
```promql
sum(resman_user_process_count)
```

### Processes per User
```promql
resman_user_process_count
```

### Users with Most Processes
```promql
sort_desc(resman_user_process_count)
```

### Users with More Than 100 Processes
```promql
resman_user_process_count > 100
```

### Average Processes per Active User
```promql
avg(resman_user_process_count)
```

### Process Count Trend (1 hour)
```promql
# Change in process count over time
delta(resman_user_process_count[1h])
```

---

## Limit Status

### Users with CPU Limits Currently Active
```promql
# Returns 1 for users with active limits
resman_user_cpu_limit_active == 1
```

### Count of CPU-Limited Users
```promql
count(resman_user_cpu_limit_active == 1)
```

### CPU-Limited Users with High CPU
```promql
resman_user_cpu_usage_percent > 50 and resman_user_cpu_limit_active == 1
```

### CPU Limits Activation Status
```promql
# 1 = CPU limits active, 0 = inactive
resman_cpu_limits_active
```

### Confirmed Limit Activations (Last Hour)
```promql
increase(resman_cpu_limits_activated_total[1h])
```

### Confirmed Limit Deactivations (Last Hour)
```promql
increase(resman_cpu_limits_deactivated_total[1h])
```

### Limit Activation Rate
```promql
# Activations per minute
rate(resman_cpu_limits_activated_total[5m]) * 60
```

---

## Performance Metrics

### Control Cycle Duration (Average)
```promql
# Average duration of control cycles
rate(resman_control_cycle_duration_seconds_sum[5m])
/ rate(resman_control_cycle_duration_seconds_count[5m])
```

### Control Cycle Duration (95th Percentile)
```promql
histogram_quantile(0.95, rate(resman_control_cycle_duration_seconds_bucket[5m]))
```

### Control Cycles per Minute
```promql
rate(resman_control_cycle_duration_seconds_count[5m]) * 60
```

### Active Users Count
```promql
resman_all_users_count
```

### CPU-Limited Users Count
```promql
resman_cpu_actively_limited_users_count
```

### System Memory Usage
```promql
resman_memory_usage_megabytes
```

---

## Error Tracking

### Processes Missing Required Procfs Access
```promql
resman_procfs_unavailable_processes > 0
```

The bounded `access` label is `executable_identity` or `io_decision`. A non-zero
value means process policy or I/O decisions are operating conservatively because
the daemon cannot read a required foreign-process input.

### Error Rate by Component
```promql
sum by (component) (rate(resman_errors_total[5m]))
```

### Total Errors (Last Hour)
```promql
sum(increase(resman_errors_total[1h]))
```

### Errors by Type
```promql
sum by (error_type) (rate(resman_errors_total[1h]))
```

### Error Rate Trend
```promql
# Compare current vs previous hour
sum(rate(resman_errors_total[1h]))
- sum(rate(resman_errors_total[1h] offset 1h))
```

---

## Alerting Queries

### High CPU Usage Alert
```promql
# User CPU > 90% for 5 minutes
resman_user_cpu_usage_percent > 90
```

### High Memory Usage Alert
```promql
# User memory > 4GB for 10 minutes
resman_user_memory_usage_bytes > 4294967296
```

### Too Many Processes Alert
```promql
# User has > 500 processes for 5 minutes
resman_user_process_count > 500
```

### System Overload Alert
```promql
# Load per core > 2 for 5 minutes
(resman_system_load_average / resman_cpu_total_cores) > 2
```

### CPU Limits Not Activating Alert
```promql
# High CPU but limits not active
resman_all_users_cpu_usage_percent > 80 and resman_cpu_limits_active == 0
```

### Frequent CPU Limit Toggling Alert
```promql
# More than 5 confirmed state changes in 10 minutes
(increase(resman_cpu_limits_activated_total[10m])
 + increase(resman_cpu_limits_deactivated_total[10m])) > 5
```

### Control Cycle Too Slow Alert
```promql
# Average cycle > 10 seconds
(rate(resman_control_cycle_duration_seconds_sum[5m])
 / rate(resman_control_cycle_duration_seconds_count[5m])) > 10
```

### High Error Rate Alert
```promql
# More than 10 errors in 5 minutes
sum(increase(resman_errors_total[5m])) > 10
```

---

## Grafana Panel Examples

### CPU Usage by User (Time Series)
```promql
resman_user_cpu_usage_percent
```
- **Visualization**: Time series
- **Legend**: `{{username}} (UID: {{uid}})`
- **Unit**: Percent (0-100)

### Memory Usage by User (Time Series)
```promql
resman_user_memory_usage_bytes
```
- **Visualization**: Time series
- **Legend**: `{{username}}`
- **Unit**: Bytes

### User Resource Table
```promql
# Current CPU
resman_user_cpu_usage_percent

# Current Memory
resman_user_memory_usage_bytes

# Current Processes
resman_user_process_count
```
- **Visualization**: Table
- **Columns**: username, CPU%, Memory (MB), Processes

### Limits Status Panel
```promql
resman_user_cpu_limit_active
```
- **Visualization**: Stat
- **Color mode**: Value
- **Thresholds**: 0=green, 1=red

---

## Useful Combinations

### CPU Efficiency (CPU per Process)
```promql
# CPU usage divided by process count
resman_user_cpu_usage_percent
/ on(uid, username) group_left() resman_user_process_count
```

### Memory per Process
```promql
# Average memory per process
resman_user_memory_usage_bytes
/ on(uid, username) group_left() resman_user_process_count
```

### Users with High CPU and Memory
```promql
# Both CPU > 50% AND Memory > 1GB
(resman_user_cpu_usage_percent > 50)
and
(resman_user_memory_usage_bytes > 1073741824)
```

### Resource Score (CPU + Normalized Memory)
```promql
# Combined score: CPU% + (Memory/1GB * 10)
resman_user_cpu_usage_percent
+ (resman_user_memory_usage_bytes / 1024 / 1024 / 1024 * 10)
```

---

## Recording Rules (Optional)

For better query performance, consider adding recording rules:

```yaml
groups:
- name: resman_recording
  interval: 30s
  rules:
  - record: job:resman_user_cpu_usage:avg1h
    expr: avg_over_time(resman_user_cpu_usage_percent[1h])

  - record: job:resman_user_memory:avg1h
    expr: avg_over_time(resman_user_memory_usage_bytes[1h])

  - record: job:resman_limits:activation_rate
    expr: rate(resman_cpu_limits_activated_total[5m])
```

---

## See Also

- [Prometheus Documentation](https://prometheus.io/docs/prometheus/latest/querying/basics/)
- [Grafana Documentation](https://grafana.com/docs/)
- [ResMan Man Page](resman.8)
- [Alerting Rules](alerting-rules.yml)
