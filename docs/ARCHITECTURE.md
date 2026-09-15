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
| **Activation** | `CPUWeight` on systemd user slices | `MemoryMax` + `MemoryHigh` on systemd user slices | systemd per-device I/O maxima |
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
dimension is configured, the decision engine reads `rios`/`wios` from the
authoritative user slice and preserves a logical cumulative counter across slice
lifetimes.

To show only limited users in dashboards, filter by `resman_user_cpu_limit_active{uid, username} == 1`.

CPU Points observations use one synchronized decision interval for parent and slice
CPU deltas and resource coverage. See [CPU Points observability](CPU-POINTS-OBSERVABILITY.md)
for the CPU Points contract and operator measurement procedure in schema 9.

## Cgroup Hierarchy

Current metrics schema: 9.

On systemd hosts the authoritative topology is flat:

```text
user.slice                 finite parent pool
  user-0.slice             dedicated root entitlement
  user-1000.slice          mapped guarantee
  user-1001.slice          best effort, including excluded users
```

All runnable siblings can borrow unused capacity. Processes retain their original
session/service membership. CPU covers descendants of each user slice, including
rootless containers; a UID split across unrelated units has partial coverage.
RAM and I/O use independent authority checks and are refused when ownership is
incomplete. Without an authoritative systemd adapter ResMan reports
`observation_only` and applies no limits. Measurements use a 60-second window and
effective parent delivery.

## Placement transitions and memory charges

The CPU Points map is a separate strict text contract. Its first physical line is
`[resman-cpu-points-map-v1]`; subsequent assignments are exact
`username=points` records. The entire username is passed to NSS without escaping or
normalization, so dotted identities such as `john.smith` are direct keys. The package
installs `/etc/resman/cpu-points.map` as a root-owned regular mode-`0600` file below
the private mode-`0700` configuration directory. Unsafe files or ancestors reject the
complete composite configuration epoch.

Active class changes reconcile weights in place without moving processes or changing
the accounting identity. Reserve, root and best effort default to 100 each,
leaving 700 named points. Root receives CPU_ROOT_POINTS=100 without a leaf CPUQuota;
the reserve protects system.slice, not an unbounded root login shell.

Native memory accounting includes existing slice charges; incomplete authority refuses
RAM and I/O without abandoning CPU scheduling. No resource transition changes process
membership or the accounting identity.

With the normal `memory.high < memory.max` composition and no swap or reclaimable
pages, a process can remain alive but make negligible progress at high indefinitely.
The observable signature is a plateau below max with rising high events and zero max,
OOM, OOM-kill and OOM-group-kill events. This is expected high throttling, not proof
that the max boundary failed. Raising or disabling high, providing reclaimable
capacity or swap, or releasing RAM enforcement are the operator remedies. An explicit
`memory.high = memory.max` control is a separate max/OOM experiment.

## Error Handling

| Context | Behavior |
|---------|----------|
| Cgroup creation failure | `Error` level, propagated, stops processing for that user |
| Limit application failure | `Warn` level, not propagated, best-effort |
| Limit removal failure | `Warn` level, not propagated, best-effort |
