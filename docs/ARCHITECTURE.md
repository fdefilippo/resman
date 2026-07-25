# ResMan Architecture

## Control Flow

ResMan uses a single control cycle that runs every `POLLING_INTERVAL` seconds:

```
1. collectSystemMetrics()
   ├─ Scan /proc for CPU, RAM, process counts
   ├─ Read cgroup stats (memory.events, io.stat)
   └─ Compute aggregates for ALL USERS and LIMITED USERS

2. makeDecision()
   ├─ cpuExceeded  = LimitedUsersCPUUsage >= CPUThreshold
   ├─ ramExceeded  = RAM% of limited users >= RAMThreshold
   ├─ ioExceeded   = IO% of limited users >= IOThreshold
   │
   ├─ ACTIVATE if:  cpuExceeded OR ramExceeded OR ioExceeded
   ├─ DEACTIVATE if: cpuBelow AND ramBelow AND ioBelow
   └─ MAINTAIN otherwise

3. executeDecision()
   ├─ ACTIVATE_LIMITS   → activateLimits()
   └─ DEACTIVATE_LIMITS → deactivateLimits()
```

## Controller Behavior

| Aspect | CPU | RAM | IO |
|--------|-----|-----|-----|
| **Enable flag** | Always on | `RAM_LIMIT_ENABLED` | `IO_LIMIT_ENABLED` |
| **Activation** | `cpu.weight` in shared cgroup | `memory.max` + `memory.high` in per-user cgroup | `io.max` in per-user cgroup |
| **Throttling** | Reduces CPU share proportionally | Kernel reclaim + throttle | Kernel IO throttle |
| **Hard limit** | Quota in `cpu.max` | `memory.max` (OOM killer) | `io.max` (bandwidth/IOPS cap) |
| **Soft limit** | — | `memory.high` (throttle, no kill) | — |
| **User filters** | `USER_INCLUDE_LIST` / `USER_EXCLUDE_LIST` | `RAM_USER_INCLUDE_LIST` / `RAM_USER_EXCLUDE_LIST` | `IO_USER_INCLUDE_LIST` / `IO_USER_EXCLUDE_LIST` |
| **Threshold duration** | `CPU_THRESHOLD_DURATION` (anti-spike) | Immediate | Immediate |

## User Filtering

Each controller has independent user filter lists:

```
CPU:  USER_INCLUDE_LIST      / USER_EXCLUDE_LIST      → IsUserWhitelisted()
RAM:  RAM_USER_INCLUDE_LIST  / RAM_USER_EXCLUDE_LIST  → IsUserWhitelistedForRAM()
IO:   IO_USER_INCLUDE_LIST   / IO_USER_EXCLUDE_LIST   → IsUserWhitelistedForIO()
```

If a controller's include list is empty/nil, all users are included.
If a controller's exclude list is empty/nil, no users are excluded.

## Prometheus Metrics

All per-user metrics use 2 labels: `uid`, `username`.

```prometheus
# Gauges (current state)
resman_user_cpu_usage_percent{uid, username}
resman_user_memory_usage_bytes{uid, username}
resman_user_process_count{uid, username}
resman_user_cpu_limited{uid, username}          # 0 or 1

# Counters (cumulative)
resman_user_memory_high_breaches_total{uid, username}
resman_user_io_read_bytes_total{uid, username}
resman_user_io_write_bytes_total{uid, username}
resman_user_io_read_ops_total{uid, username}
resman_user_io_write_ops_total{uid, username}
```

The `*_bytes_total` series report block-device traffic. For compatibility, the
`*_ops_total` series retain their historical names but expose `/proc/PID/io`
`syscr`/`syscw`: read/write-family syscall counts, not block-device IOPS.

To show only limited users in dashboards, filter by `resman_user_cpu_limited{uid, username} == 1`.

## Cgroup Hierarchy

```
/sys/fs/cgroup/                     ← root (controllers: cpu, cpuset, io)
  └── resman/                       ← base cgroup
        ├── limited/                ← shared cgroup (CPU only)
        │     ├── user_1000/        ← per-user sub-cgroup
        │     └── user_1001/
        ├── user_1000/              ← per-user cgroup (RAM, IO)
        └── user_1001/
```

- **CPU**: Uses shared cgroup `limited/` with proportional `cpu.weight`
- **RAM**: Applied directly to per-user cgroup (`memory.max`, `memory.high`)
- **IO**: Applied directly to per-user cgroup (`io.max`)

## Error Handling

| Context | Behavior |
|---------|----------|
| Cgroup creation failure | `Error` level, propagated, stops processing for that user |
| Limit application failure | `Warn` level, not propagated, best-effort |
| Limit removal failure | `Warn` level, not propagated, best-effort |
