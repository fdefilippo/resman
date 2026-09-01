# ResMan Architecture

## Control Flow

ResMan uses a single control cycle that runs every `POLLING_INTERVAL` seconds:

```
1. collectSystemMetrics()
   ├─ Scan /proc for CPU, RAM, process counts
   ├─ Read cgroup stats (memory.events, io.stat)
   └─ Compute all-user and independent CPU/RAM/I/O eligibility aggregates

2. makeDecision()
   ├─ cpuExceeded  = CPUEligibleCPUUsage >= CPUThreshold
   ├─ ramExceeded  = RAM% of RAM-eligible users >= RAMThreshold
   ├─ ioExceeded   = IO% of I/O-eligible users >= IOThreshold
   │
   ├─ attribute host load from eligible CPU share when load average is high
   ├─ ACTIVATE if:  cpuExceeded OR ramExceeded OR ioExceeded,
   │                unless external CPU is the measured majority
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

The empty-list contract is resource-specific: an empty CPU include list selects
nobody (fail-safe), while empty RAM and I/O include lists select everybody. An empty
exclude list selects nobody for every resource. Eligibility is evaluated independently;
CPU eligibility never gates RAM or I/O eligibility.

With `IGNORE_SYSTEM_LOAD=false`, high load average suppresses activation only when
CPU-eligible users contribute less than half of measured host CPU activity. Host CPU
is converted from normalized 0-100 percentage to aggregate per-core percentage before
comparison with the process sum. A missing host sample delays activation without
asserting an unmeasured external cause.

## Prometheus Metrics

All per-user metrics use 2 labels: `uid`, `username`.

```prometheus
# Gauges (current state)
resman_user_cpu_usage_percent{uid, username}
resman_user_memory_usage_bytes{uid, username}
resman_user_process_count{uid, username}
resman_user_cpu_limit_active{uid, username}          # 0 or 1

# Counters (cumulative)
resman_user_memory_high_breaches_total{uid, username}
resman_user_io_read_bytes_total{uid, username}
resman_user_io_write_bytes_total{uid, username}
resman_user_io_read_ops_total{uid, username}
resman_user_io_write_ops_total{uid, username}
```

The `*_bytes_total` series report block-device traffic. The Prometheus
`*_ops_total` series expose `/proc/PID/io` `syscr`/`syscw`: read/write-family
syscall counts, not the block-device IOPS used for decisions. When an IOPS
dimension is configured, the decision engine reads `rios`/`wios` from unlimited
per-user observation cgroups and preserves a logical cumulative counter across
enforcement placement changes.

To show only limited users in dashboards, filter by `resman_user_cpu_limit_active{uid, username} == 1`.

CPU Points observability is interval-based. The parent, guaranteed-domain,
best-effort-domain, and per-leaf `*_usage_microseconds_delta` gauges all come from
one control-cycle interval, whose duration is
`resman_cpu_points_observation_interval_seconds`. Allocation ratios use effective
parent usage as the denominator; the nominal pool and programmed `cpu.max` are not
delivered guarantees. Positive parent throttling is expected under a saturated finite
pool, and CFS can deliver less than the nominal quota.

Mapped-user guarantees are optional values. A best-effort UID has no fabricated
per-user guarantee. The applied-class, lifecycle, and process-coverage series separate
a complete UID workload from a partial host-enforceable subset. Lending is
class-prioritized: idle mapped capacity serves runnable guaranteed siblings first;
best effort is reported as borrowing only while the complete guaranteed domain is
inactive.

## Cgroup Hierarchy

```
/sys/fs/cgroup/                     ← root (controllers: cpu, cpuset, io)
  └── resman/                       ← base cgroup
        ├── limited/               ← finite CPU Points parent
        │     ├── guaranteed/       ← aggregate acquired mapped guarantees
        │     │     └── user_1000/  ← exact mapped weight
        │     └── best_effort/      ← one aggregate best-effort entitlement
        │           └── user_1001/  ← equal-share best-effort leaf
        ├── user_1000/              ← IOPS observation; RAM/IO when active
        └── user_1001/
```

- **CPU**: Uses the finite CPU Points parent and two class-priority scheduling domains
- **RAM**: Applied directly to per-user cgroup (`memory.max`, `memory.high`)
- **IO**: Observed and applied directly in the current per-user cgroup (`io.stat`, `io.max`)

## Error Handling

| Context | Behavior |
|---------|----------|
| Cgroup creation failure | `Error` level, propagated, stops processing for that user |
| Limit application failure | `Warn` level, not propagated, best-effort |
| Limit removal failure | `Warn` level, not propagated, best-effort |
