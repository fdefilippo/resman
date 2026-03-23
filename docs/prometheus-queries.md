# Prometheus Query Examples for CPU Manager Go

This document provides example PromQL queries for monitoring CPU Manager Go metrics.

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
cpu_manager_cpu_total_usage_percent
```

### CPU Usage Trend (1 hour)
```promql
# Total CPU usage over time
cpu_manager_cpu_total_usage_percent[1h]
```

### User CPU Usage vs Total
```promql
# Compare total vs user CPU usage
cpu_manager_cpu_total_usage_percent
cpu_manager_cpu_user_usage_percent
```

### System Load Average
```promql
# Current system load (1 minute average)
cpu_manager_system_load_average
```

### Load per CPU Core
```promql
# Normalized load per core
cpu_manager_system_load_average / cpu_manager_cpu_total_cores
```

---

## Per-User Metrics

### Top 5 Users by CPU Usage
```promql
topk(5, cpu_manager_user_cpu_usage_percent)
```

### Top 10 Users by Memory Usage
```promql
topk(10, cpu_manager_user_memory_usage_bytes)
```

### CPU Usage for Specific User
```promql
# By username
cpu_manager_user_cpu_usage_percent{username="francesco"}

# By UID
cpu_manager_user_cpu_usage_percent{uid="1000"}
```

### Memory Usage for Specific User
```promql
cpu_manager_user_memory_usage_bytes{username="francesco"}
```

### All Users Sorted by CPU (Descending)
```promql
sort_desc(cpu_manager_user_cpu_usage_percent)
```

### Users with CPU > 50%
```promql
cpu_manager_user_cpu_usage_percent > 50
```

### CPU Usage Rate of Change (5 min)
```promql
# Positive = increasing, Negative = decreasing
rate(cpu_manager_user_cpu_usage_percent[5m])
```

### CPU Usage Average (1 hour) per User
```promql
avg_over_time(cpu_manager_user_cpu_usage_percent[1h])
```

---

## Memory Analysis

### Total Memory Used by All Users
```promql
sum(cpu_manager_user_memory_usage_bytes)
```

### Total Memory in GB
```promql
sum(cpu_manager_user_memory_usage_bytes) / 1024 / 1024 / 1024
```

### Memory Distribution Among Users
```promql
# Percentage of total user memory per user
cpu_manager_user_memory_usage_bytes 
/ on() group_left() sum(cpu_manager_user_memory_usage_bytes) * 100
```

### Memory Growth Rate (per minute)
```promql
# Positive = growing, Negative = shrinking
irate(cpu_manager_user_memory_usage_bytes[5m]) * 60
```

### Users Using More Than 1GB Memory
```promql
cpu_manager_user_memory_usage_bytes > 1073741824
```

### Users Using More Than 2GB Memory
```promql
cpu_manager_user_memory_usage_bytes > 2147483648
```

### Memory per User (Human Readable)
```promql
# In MB
cpu_manager_user_memory_usage_bytes / 1024 / 1024
```

---

## Process Analysis

### Total Processes Across All Users
```promql
sum(cpu_manager_user_process_count)
```

### Processes per User
```promql
cpu_manager_user_process_count
```

### Users with Most Processes
```promql
sort_desc(cpu_manager_user_process_count)
```

### Users with More Than 100 Processes
```promql
cpu_manager_user_process_count > 100
```

### Average Processes per Active User
```promql
avg(cpu_manager_user_process_count)
```

### Process Count Trend (1 hour)
```promql
# Change in process count over time
delta(cpu_manager_user_process_count[1h])
```

---

## Limit Status

### Users Currently Limited
```promql
# Returns 1 for users with active limits
cpu_manager_user_cpu_limited == 1
```

### Count of Limited Users
```promql
count(cpu_manager_user_cpu_limited == 1)
```

### Limited Users with High CPU
```promql
cpu_manager_user_cpu_usage_percent > 50 and cpu_manager_user_cpu_limited == 1
```

### Limits Activation Status
```promql
# 1 = limits active globally, 0 = inactive
cpu_manager_limits_active
```

### Total Limit Activations (Last Hour)
```promql
increase(cpu_manager_limits_activated_total[1h])
```

### Total Limit Deactivations (Last Hour)
```promql
increase(cpu_manager_limits_deactivated_total[1h])
```

### Limit Activation Rate
```promql
# Activations per minute
rate(cpu_manager_limits_activated_total[5m]) * 60
```

---

## Performance Metrics

### Control Cycle Duration (Average)
```promql
# Average duration of control cycles
rate(cpu_manager_control_cycle_duration_seconds_sum[5m]) 
/ rate(cpu_manager_control_cycle_duration_seconds_count[5m])
```

### Control Cycle Duration (95th Percentile)
```promql
histogram_quantile(0.95, rate(cpu_manager_control_cycle_duration_seconds_bucket[5m]))
```

### Control Cycles per Minute
```promql
rate(cpu_manager_control_cycle_duration_seconds_count[5m]) * 60
```

### Active Users Count
```promql
cpu_manager_active_users_count
```

### Limited Users Count
```promql
cpu_manager_limited_users_count
```

### System Memory Usage
```promql
cpu_manager_memory_usage_megabytes
```

---

## Error Tracking

### Error Rate by Component
```promql
sum by (component) (rate(cpu_manager_errors_total[5m]))
```

### Total Errors (Last Hour)
```promql
increase(cpu_manager_errors_total[1h])
```

### Errors by Type
```promql
sum by (error_type) (rate(cpu_manager_errors_total[1h]))
```

### Error Rate Trend
```promql
# Compare current vs previous hour
sum(rate(cpu_manager_errors_total[1h])) 
- sum(rate(cpu_manager_errors_total[1h] offset 1h))
```

---

## Alerting Queries

### High CPU Usage Alert
```promql
# User CPU > 90% for 5 minutes
cpu_manager_user_cpu_usage_percent > 90
```

### High Memory Usage Alert
```promql
# User memory > 4GB for 10 minutes
cpu_manager_user_memory_usage_bytes > 4294967296
```

### Too Many Processes Alert
```promql
# User has > 500 processes for 5 minutes
cpu_manager_user_process_count > 500
```

### System Overload Alert
```promql
# Load per core > 2 for 5 minutes
(cpu_manager_system_load_average / cpu_manager_cpu_total_cores) > 2
```

### Limits Not Activating Alert
```promql
# High CPU but limits not active
cpu_manager_cpu_user_usage_percent > 80 and cpu_manager_limits_active == 0
```

### Frequent Limit Toggling Alert
```promql
# More than 5 activations in 10 minutes
increase(cpu_manager_limits_activated_total[10m]) > 5
```

### Control Cycle Too Slow Alert
```promql
# Average cycle > 10 seconds
(rate(cpu_manager_control_cycle_duration_seconds_sum[5m]) 
 / rate(cpu_manager_control_cycle_duration_seconds_count[5m])) > 10
```

### High Error Rate Alert
```promql
# More than 10 errors in 5 minutes
increase(cpu_manager_errors_total[5m]) > 10
```

---

## Grafana Panel Examples

### CPU Usage by User (Time Series)
```promql
cpu_manager_user_cpu_usage_percent
```
- **Visualization**: Time series
- **Legend**: `{{username}} (UID: {{uid}})`
- **Unit**: Percent (0-100)

### Memory Usage by User (Time Series)
```promql
cpu_manager_user_memory_usage_bytes
```
- **Visualization**: Time series
- **Legend**: `{{username}}`
- **Unit**: Bytes

### User Resource Table
```promql
# Current CPU
cpu_manager_user_cpu_usage_percent

# Current Memory
cpu_manager_user_memory_usage_bytes

# Current Processes
cpu_manager_user_process_count
```
- **Visualization**: Table
- **Columns**: username, CPU%, Memory (MB), Processes

### Limits Status Panel
```promql
cpu_manager_user_cpu_limited
```
- **Visualization**: Stat
- **Color mode**: Value
- **Thresholds**: 0=green, 1=red

---

## Useful Combinations

### CPU Efficiency (CPU per Process)
```promql
# CPU usage divided by process count
cpu_manager_user_cpu_usage_percent 
/ on(uid, username) group_left() cpu_manager_user_process_count
```

### Memory per Process
```promql
# Average memory per process
cpu_manager_user_memory_usage_bytes 
/ on(uid, username) group_left() cpu_manager_user_process_count
```

### Users with High CPU and Memory
```promql
# Both CPU > 50% AND Memory > 1GB
(cpu_manager_user_cpu_usage_percent > 50) 
and 
(cpu_manager_user_memory_usage_bytes > 1073741824)
```

### Resource Score (CPU + Normalized Memory)
```promql
# Combined score: CPU% + (Memory/1GB * 10)
cpu_manager_user_cpu_usage_percent 
+ (cpu_manager_user_memory_usage_bytes / 1024 / 1024 / 1024 * 10)
```

---

## Recording Rules (Optional)

For better query performance, consider adding recording rules:

```yaml
groups:
- name: cpu_manager_recording
  interval: 30s
  rules:
  - record: job:cpu_manager_user_cpu_usage:avg1h
    expr: avg_over_time(cpu_manager_user_cpu_usage_percent[1h])
    
  - record: job:cpu_manager_user_memory:avg1h
    expr: avg_over_time(cpu_manager_user_memory_usage_bytes[1h])
    
  - record: job:cpu_manager_limits:activation_rate
    expr: rate(cpu_manager_limits_activated_total[5m])
```

---

## See Also

- [Prometheus Documentation](https://prometheus.io/docs/prometheus/latest/querying/basics/)
- [Grafana Documentation](https://grafana.com/docs/)
- [CPU Manager Man Page](resman.8)
- [Alerting Rules](alerting-rules.yml)
