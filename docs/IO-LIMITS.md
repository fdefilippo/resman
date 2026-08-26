# ResMan v1.20.0 - I/O Limits

## Overview

ResMan v1.20.0 introduced block-I/O limits for cgroups v2 through the `io`
controller. ResMan can control disk bandwidth and block operations per user.

---

## What is the `io` controller?

The cgroups v2 `io` controller can:

- Limit read and write bandwidth in bytes per second.
- Limit read and write block-I/O operations per second (IOPS).
- Report per-device I/O statistics.

### How CPU, RAM, and I/O limits differ

| Limit | Behaviour when exceeded |
|-------|-------------------------|
| CPU (`cpu.max`) | Throttling until the next period |
| RAM (`memory.max`) | OOM kill |
| RAM (`memory.high`) | Throttling and aggressive reclaim |
| **I/O (`io.max`)** | **Block-I/O throttling** |

---

## Configuration

### Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `IO_LIMIT_ENABLED` | `false` | Enables or disables I/O limits |
| `IO_THRESHOLD` | `75` | Percentage at which any configured dimension activates limits |
| `IO_RELEASE_THRESHOLD` | `40` | Percentage below which every configured dimension must fall before release |
| `IO_READ_BPS` | `100M` | Per-user read-bandwidth limit |
| `IO_WRITE_BPS` | `50M` | Per-user write-bandwidth limit |
| `IO_READ_IOPS` | `1000` | Per-user read-IOPS limit |
| `IO_WRITE_IOPS` | `500` | Per-user write-IOPS limit |
| `IO_DEVICE_FILTER` | `all` | Enumerates whole devices in `/sys/block`, or selects one `major:minor` device |

### Bandwidth format

Bandwidth values support the `K`, `M`, `G`, and `T` suffixes, using base 1024.

```bash
IO_READ_BPS=104857600   # 100 MiB/s in bytes
IO_READ_BPS=100M        # 100 MiB/s with a suffix
IO_READ_BPS=max         # No read-bandwidth limit
```

`max` or an empty string disables only the corresponding bandwidth dimension.
`0` disables only the corresponding IOPS dimension. Other dimensions remain
enabled and continue to participate in decisions.

### Configuration examples

```bash
# I/O limits disabled (default)
IO_LIMIT_ENABLED=false

# Moderate limits
IO_LIMIT_ENABLED=true
IO_THRESHOLD=75
IO_RELEASE_THRESHOLD=40
IO_READ_BPS=100M
IO_WRITE_BPS=50M
IO_READ_IOPS=1000
IO_WRITE_IOPS=500

# Stricter limits for a database server
IO_LIMIT_ENABLED=true
IO_THRESHOLD=80
IO_RELEASE_THRESHOLD=50
IO_READ_BPS=50M
IO_WRITE_BPS=25M
IO_READ_IOPS=500
IO_WRITE_IOPS=250

# Bandwidth limits only
IO_LIMIT_ENABLED=true
IO_READ_BPS=200M
IO_WRITE_BPS=100M
IO_READ_IOPS=0
IO_WRITE_IOPS=0
```

---

## Monitoring

### Prometheus metrics

```text
resman_user_io_read_bytes_total{uid, username}
resman_user_io_write_bytes_total{uid, username}
resman_user_io_read_ops_total{uid, username}
resman_user_io_write_ops_total{uid, username}
```

The Prometheus `*_bytes_total` metrics measure bytes transferred to block
devices. The historical `*_ops_total` metrics continue to derive from the
`syscr` and `syscw` counters in `/proc/PID/io`: they observe read-family and
write-family syscalls and are not the IOPS inputs used by the decision engine.
IOPS decisions instead use `rios` and `wios` from `io.stat` in per-user
observation cgroups.

### Useful queries

```promql
# Top five users by read bandwidth
topk(5, rate(resman_user_io_read_bytes_total[5m]))

# Total write bandwidth by hostname
sum by (hostname) (rate(resman_user_io_write_bytes_total[5m]))

# Read/write-family syscall rate by user, not block-device IOPS
sum by (username) (rate(resman_user_io_read_ops_total[5m]) + rate(resman_user_io_write_ops_total[5m]))
```

---

## How it works

### Activation

When `IO_LIMIT_ENABLED=true`, ResMan separately calculates utilisation for read
bandwidth, write bandwidth, read operations, and write operations. For each
dimension, the denominator is the per-user limit multiplied by the number of
I/O-eligible users. Any configured dimension that reaches `IO_THRESHOLD`
activates or maintains limits. When `IO_THRESHOLD_DURATION` is greater than
zero, it applies to the highest percentage among dimensions over the threshold.

Bandwidth signals come from `read_bytes` and `write_bytes` in `/proc/PID/io`.
When at least one IOPS dimension is configured, ResMan creates an unlimited
observation cgroup for every I/O-eligible user, reconciles the user's enforceable
processes into it, and calculates rates from `rios` and `wios` in `io.stat`.
Page-cache hits and traffic through pipes or sockets do not increment these
counters; direct I/O to a block device does.

Moving between an observation cgroup and the shared CPU hierarchy preserves a
logical cumulative counter. ResMan absorbs the source's final value before the
move and uses the destination's initial value as the new baseline. Activation
and release therefore do not produce an artificial spike or zero-rate window.
The first sample after observation starts or the process policy changes is
incomplete and cannot release active limits.

For bandwidth signals, ResMan calculates `/proc/PID/io` deltas separately for
each PID and start-time pair before aggregating them per user. A process exit or
counter reset does not erase valid traffic from other processes, PID reuse
establishes a new baseline, and baselines for exited processes are removed after
the scan. IOPS deltas instead follow the stable per-user cgroup identity and its
logical cumulative ledger.

When the activation rule is satisfied:

1. With `IO_DEVICE_FILTER=all`, ResMan enumerates whole devices in `/sys/block`
   and writes one line per `major:minor` device to `<cgroup>/io.max`:

   ```text
   8:0 rbps=104857600 wbps=52428800 riops=1000 wiops=500
   259:0 rbps=104857600 wbps=52428800 riops=1000 wiops=500
   ```

2. The kernel limits the cgroup's block-I/O operations.
3. Excess operations are delayed through throttling.

### Release

Release uses the complementary rule: every configured dimension must be
strictly below `IO_RELEASE_THRESHOLD`. One dimension at or above the threshold
keeps enforcement active. When the release rule is satisfied:

1. ResMan removes the limits for every device present in `<cgroup>/io.max`:

   ```text
   8:0 rbps=max wbps=max riops=max wiops=max
   259:0 rbps=max wbps=max riops=max wiops=max
   ```

2. The kernel stops throttling the cgroup.

### Statistics

ResMan reads statistics from `<cgroup>/io.stat`:

```text
8:0 rios=1234 wios=567 rbytes=104857600 wbytes=52428800
259:0 rios=100 wios=50 rbytes=10485760 wbytes=5242880
```

Values are aggregated across all devices. Block-byte observations are exposed
through Prometheus, while `rios` and `wios` drive IOPS decisions.

---

## Troubleshooting

### Limits are not applied

Verify that:

1. `IO_LIMIT_ENABLED=true`.
2. The kernel exposes the `io` controller:

   ```bash
   grep -w io /sys/fs/cgroup/cgroup.controllers
   ```

3. The parent cgroup enables the controller:

   ```bash
   grep -w io /sys/fs/cgroup/cgroup.subtree_control
   ```

4. A real child cgroup exposes `io.max`. A controller name alone does not prove
   that its interface files are usable in the delegated hierarchy.

### Check active limits

```bash
# Check I/O limits for one user
cat /sys/fs/cgroup/resman/user_1000/io.max

# Check I/O statistics
cat /sys/fs/cgroup/resman/user_1000/io.stat
```

### Find a device's major:minor number

```bash
lsblk -o NAME,MAJ:MIN
# Output: sda      8:0
#         nvme0n1 259:0
```

---

## References

- [Kernel documentation: I/O controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#io)
- [CGROUP-V2-TECHNICAL.md](CGROUP-V2-TECHNICAL.md)

---

**Version:** ResMan v1.20.0
