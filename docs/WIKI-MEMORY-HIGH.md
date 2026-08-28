# `memory.high` Soft Limits

## Overview

ResMan can apply both `memory.high` and `memory.max` to RAM-eligible user cgroups.
The soft boundary provides proportional reclaim and throttling before the hard limit
can invoke the OOM killer.

| Property | `memory.high` | `memory.max` |
|---|---|---|
| Boundary | Soft | Hard |
| Kernel response | Throttling and reclaim | OOM kill when reclaim cannot satisfy the limit |
| Processes killed directly | No | Possible |
| ResMan setting | `RAM_HIGH_RATIO` of the hard quota | `RAM_QUOTA_PER_USER` |

For a 512 MiB hard quota and the default ratio of `0.8`, ResMan writes approximately
410 MiB to `memory.high` and 512 MiB to `memory.max`.

## Configuration

```ini
# Balanced production default: memory.high is 80% of memory.max.
RAM_HIGH_RATIO=0.8

# Earlier reclaim at 70%.
# RAM_HIGH_RATIO=0.7

# Later reclaim at 90%.
# RAM_HIGH_RATIO=0.9

# Disable the soft limit and use only memory.max.
# RAM_HIGH_RATIO=0.0
```

`RAM_HIGH_RATIO` accepts values from `0.0` through `1.0`. Configuration validation
rejects values outside that range. A value of zero disables `memory.high`.

The RAM feature must also be enabled and the user must satisfy the RAM-specific
include and exclude policy. See [`IO-LIMITS.md`](IO-LIMITS.md) and
[`TECHNICAL-SPECIFICATION.md`](TECHNICAL-SPECIFICATION.md) for the shared
eligibility, intent, and observed-enforcement model.

## Monitoring

ResMan exports:

```promql
resman_user_memory_high_events_total
```

The counter is derived from the `high` field in `memory.events` and identifies how
often a user cgroup crossed the soft boundary. Useful queries include:

```promql
# Users with the most soft-limit events in the last five minutes.
topk(10, increase(resman_user_memory_high_events_total[5m]))

# Events grouped by host and user.
sum by (hostname, uid, username) (
  increase(resman_user_memory_high_events_total[1h])
)
```

The shipped Grafana dashboard includes memory usage and memory-pressure panels. A
small number of soft-limit events can be normal during bursts. Sustained growth means
the workload is repeatedly reclaiming memory and should be investigated before it
reaches `memory.max`.

## Alerting example

```yaml
groups:
  - name: resman-memory-pressure
    rules:
      - alert: ResManUserMemoryPressure
        expr: increase(resman_user_memory_high_events_total[10m]) > 10
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "User {{ $labels.username }} is under sustained memory pressure"

      - alert: ResManUserMemoryPressureCritical
        expr: increase(resman_user_memory_high_events_total[5m]) > 50
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "User {{ $labels.username }} is approaching its hard RAM boundary"
```

Tune thresholds from observed workload behavior. A soft-limit event is not itself an
OOM event.

## Verification

For a limited UID, inspect the actual cgroup files:

```bash
uid=1001
base=/sys/fs/cgroup/resman/user_${uid}

cat "$base/memory.high"
cat "$base/memory.max"
cat "$base/memory.events"
```

The exact cgroup path may differ when `CGROUP_ROOT` or `CGROUP_BASE` is customized.
Use the MCP cgroup information surface or ResMan logs to find the managed path.

## Troubleshooting

### Soft-limit events occur without an OOM kill

This is expected: `memory.high` asks the kernel to throttle and reclaim, while
`memory.max` is the hard boundary. Check whether events are sustained and whether
application latency is affected.

### `memory.high` equals `memory.max`

Set `RAM_HIGH_RATIO` below `1.0` and reload or restart according to the configuration
lifecycle reported by ResMan.

### `memory.high` is absent

At startup, ResMan probes a real child cgroup for the interfaces required by enabled
features. If RAM limiting is enabled but `memory.high` or another required memory
interface is unavailable, startup fails closed and names the missing interface.

## Operational guidance

- Start with `RAM_HIGH_RATIO=0.8` and adjust from measurements.
- Alert on sustained event growth rather than a single burst.
- Correlate events with memory usage, application latency, and OOM records.
- Size `RAM_QUOTA_PER_USER` from workload requirements rather than suppressing alerts.
- Keep the memory controller enabled in the delegated cgroup hierarchy.

## References

- [Linux cgroup v2 memory controller](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html#memory)
- [PSI documentation](https://www.kernel.org/doc/html/latest/accounting/psi.html)
- [`docs/TECHNICAL-SPECIFICATION.md`](TECHNICAL-SPECIFICATION.md)
