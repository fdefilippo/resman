# Grafana Dashboard Guide

## Overview

The ResMan Grafana dashboard shows kernel pressure, the limits ResMan currently
applies and whether they are in force, memory and I/O of the monitored users, host
load against the available cores, and the health of the daemon. The
[Reading the dashboard](#reading-the-dashboard) section explains every panel.

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

## Reading the dashboard

The dashboard answers four questions in order: is the host under pressure, is ResMan
limiting anyone, is what it applied actually in force, and is the daemon healthy. This
section explains every panel and series, how to read typical shapes, and what to do
about them. Hover the `i` icon on a panel title in Grafana for a one-line reminder.

### Concepts behind every panel

- **Monitored users.** Users with at least one running process whose UID lies between
  `SYSTEM_UID_MIN` (default 1000) and `SYSTEM_UID_MAX`. Root and system accounts are
  not monitored users. A user with only sleeping processes is still monitored; a user
  with no process at all is not.
- **Eligible.** A monitored user whose username matches the resource's include list
  and not its exclude list. Eligibility is configuration, not behaviour: it does not
  depend on how much the user consumes. The CPU lists are `USER_INCLUDE_LIST` and
  `USER_EXCLUDE_LIST`, and an empty CPU include list makes nobody eligible. RAM and
  I/O have their own lists, where an empty include list selects everyone. Eligible
  counts do not depend on `RAM_LIMIT_ENABLED` or `IO_LIMIT_ENABLED`, and the lists
  never hide a monitored user from the per-user panels.
- **Limits episode.** Activation is host-wide. When eligible CPU usage stays above
  `CPU_THRESHOLD` for `CPU_THRESHOLD_DURATION`, or, with RAM or I/O limits enabled,
  eligible usage crosses `RAM_THRESHOLD` or `IO_THRESHOLD` (after
  `IO_THRESHOLD_DURATION`), ResMan starts an episode. It programs the capped `user.slice` and a
  CPU Points weight on every eligible user slice, plus RAM and I/O limits when those
  are enabled. The episode ends together for everyone: eligible CPU must stay below
  `CPU_RELEASE_THRESHOLD` for three `POLLING_INTERVAL` periods, after `MIN_ACTIVE_TIME`.
  During a blackout window no limit is applied.
- **Limited.** A user slice whose CPU Points weight systemd applied and ResMan
  confirmed in the kernel. A limited user is not necessarily a heavy user: during an
  episode every eligible slice is limited, and an idle limited user loses nothing,
  because unused capacity stays available to anyone who needs it.
- **Enforcement mode.** `systemd_native` applies limits through systemd.
  `observation_only` computes the same decisions but applies nothing, so every
  "limited" and "applied" series stays at zero there. Always check the mode first.
- **Per-cycle values.** Series named `..._delta`, delivered cores and memory.high
  events describe the last decision interval only. Prometheus samples whatever the
  last cycle published, so a value can repeat across scrapes until the next cycle.
- **Fleet labels.** ResMan exports `hostname` and `server_role`. The `cluster` and
  `environment` variables work only if your Prometheus scrape configuration adds
  those labels (see [MULTI-INSTANCE-MONITORING.md](MULTI-INSTANCE-MONITORING.md));
  without them, keep both variables on `All`.

### A one-minute health check

1. **Enforcement mode** is `systemd_native`. If it reads `observation_only`, nothing
   is being limited by design; see "Observation-only state" below.
2. **CPU limits**: `eligible` is above zero. Zero usually means `USER_INCLUDE_LIST`
   is empty.
3. During an episode, **CPU Points denominator** reads `complete` and **CPU Points
   delivery** reads `available` or `throttled_parent`.
4. **Errors by component** is flat, and **Control cycle duration** stays far below
   `POLLING_INTERVAL`.

### Row 1: Pressure (PSI)

| Panel | What it shows |
|---|---|
| **CPU pressure** | Kernel PSI events per minute on the host CPU pressure file |
| **I/O pressure** | The same for I/O |
| **Memory pressure** | `memory.high` events per minute in the authoritative user slices |

**How to read it.** A PSI event means tasks spent more than the configured stall time
(`PSI_CPU_STALL_THRESHOLD` or `PSI_IO_STALL_THRESHOLD` within `PSI_WINDOW_US`)
waiting for CPU or I/O. It measures waiting, not usage: a host can run at 100% CPU
with no events if nothing queues. Green (0) is quiet, yellow (from 1 per minute)
means recurring stalls, red (from 6) means sustained contention. `PSI inactive` means
no event since the daemon started; that is normal when `PSI_EVENT_DRIVEN` is off or
the host was never contended.

Memory pressure counts `memory.high` events: the kernel reclaiming and throttling a
slice that reached its soft limit. They occur only where `memory.high` is set,
normally during a RAM limits episode. Sustained events with no OOM mean users are
slowed down rather than killed; raise `memory.high`, add swap or reclaimable memory,
or release the RAM limit if that is not intended. ResMan does not export memory PSI.

### Row 2: Limits

**CPU limits**

| Series | Meaning |
|---|---|
| `eligible` (dashed) | Monitored users eligible for CPU limits under the include and exclude lists |
| `limited` | User slices carrying a confirmed CPU Points weight in the current episode |

**How to read it.**

- `limited = 0` with `eligible > 0` is the normal idle state: no resource has crossed
  its threshold, the duration guard is still running, a blackout is active, or the
  host is in `observation_only`.
- During an episode, `limited` jumps to roughly `eligible` at once and drops back to
  zero at release. It is a step, not a ramp, because activation is host-wide.
- `limited` can be higher than `eligible`. Root's slice is limited when `root`
  matches the include list (for example `.*`), and active slices of UIDs outside the
  monitored range are limited too, yet neither is counted as eligible.
- `limited` can be lower than `eligible` when an eligible user has no user slice, for
  example processes started only by a system service.
- When RAM or I/O limits are enabled, an episode can start because of RAM or I/O, so
  `limited` can rise while CPU usage is moderate.

**RAM limits** and **I/O limits**

| Series | Meaning |
|---|---|
| `eligible` | Monitored users eligible under the RAM (or I/O) lists, counted even when that limit type is disabled |
| `RAM/IO limits active` (orange bars) | 1 while at least one user has a RAM or I/O limit applied and confirmed; it does not tell RAM from I/O |
| `memory.high events per cycle` (RAM only) | `memory.high` events in the user slices during the last decision interval |

**How to read it.** With `RAM_LIMIT_ENABLED=false` and `IO_LIMIT_ENABLED=false` the
orange bars never appear even during an episode, while `eligible` keeps its value.
If an episode is running, RAM or I/O is enabled, and the bars stay at zero, look at
the RAM and I/O coverage tables: users who are not `complete` receive no limit.

### Row 3: Memory and I/O

**RAM of monitored processes**

| Series | Meaning |
|---|---|
| `all monitored users` | Proportional set size (PSS) of every process of the monitored users |
| `RAM-eligible users` | The same for RAM-eligible users, without `PROCESS_EXCLUDE_LIST` processes; this is the value compared with `RAM_THRESHOLD` |
| `user slices memory.current` | Memory the kernel charges to the authoritative user slices (`systemd_native` only) |

**How to read it.** PSS shares each shared page among the processes that map it, so
the first two lines do not double-count shared libraries. `memory.current` also
includes page cache and kernel memory charged to the slices, so it is normally higher
than the process lines. A gap that keeps growing usually means file cache, not a leak.

**I/O by user** shows read plus write throughput per user and host (top 10), from
`/proc/PID/io` of each user's live processes. I/O of processes that exit between two
decision cycles is not counted, so short-lived jobs are under-reported.

**RAM by user** shows PSS per user and host (top 10). Use it with RAM limits: a user
near their `memory.max` is the one that may hit the OOM killer.

### Row 4: Load

| Panel | Meaning |
|---|---|
| **Load average vs online cores** | The 1-minute load average, with the logical CPU count as a dashed red line |
| **Load per core** | Load divided by cores; yellow from 0.7, red from 1.0 |

**How to read it.** Load above the core line means runnable work is queueing. Load
also counts tasks blocked on I/O, so high load with modest CPU usage points to
storage. With `IGNORE_SYSTEM_LOAD=false`, a high load does not suppress activation
when eligible users account for at least half of the measured CPU activity.

### Row 5: Enforcement and coverage

**Enforcement mode** shows `systemd_native` (green) or `observation_only` (orange)
per host. The mode is chosen at startup and changes only with a restart.

**CPU Points denominator** tells whether per-slice CPU shares can be trusted:

| State | Meaning | Action |
|---|---|---|
| `inactive` | No CPU limits episode | None; the normal idle state |
| `complete` | The parent quota, every active slice and its weight were confirmed in the kernel this cycle | None |
| `incomplete` | An episode is running but confirmation failed this cycle, for example a login or logout during the read | Transient is fine; if it persists, check **Errors by component** |
| `unavailable` | `observation_only`, or the systemd topology could not be read | Check the mode, then the logs |

**CPU Points delivery** tells whether the capped parent is saturated:

| State | Meaning |
|---|---|
| `available` | `user.slice` counters were read and no period was throttled in the last interval |
| `throttled_parent` | `user.slice` hit its finite quota at least once: users compete and share it by weight. Expected under load |
| `unavailable` | No episode, `observation_only`, the first cycle of an episode, or unreadable counters |

The **coverage tables** answer: does the policy cover all of this user's processes?

| Coverage | CPU | RAM and I/O |
|---|---|---|
| `complete` | All processes run inside `user-UID.slice` (also for an empty slice) | Verified: the limit is applied |
| `partial` | Some processes run elsewhere, for example a systemd service running as that user; root is usually `partial` | Same cause, and **no limit is applied** to that user |
| `refused` | Not used | A container workload runs inside the slice, or apply or readback failed; no limit is applied |
| `unavailable` | No user slice, `observation_only`, or the process inventory failed | Same, or the slice memory could not be read |

CPU coverage is computed every decision cycle, with or without an episode. RAM and I/O
rows appear only while an episode requests that resource for the user, and they
disappear after release. To fix `partial`, move or stop the processes that run outside
the user's slice. `refused` for a container is a deliberate choice: ResMan does not
limit a slice whose workload belongs to a container runtime.

### Row 6: CPU Points delivery

**Parent: programmed vs delivered cores**

| Series | Meaning |
|---|---|
| `programmed cores` (dashed) | Capacity of the capped `user.slice`: online CPUs × (1000 − reserve) / 1000. Shown only during an episode |
| `delivered cores` | CPU time all user slices actually used in the last interval, in cores |
| `throttled cores` (red) | Time `user.slice` run queues spent throttled, summed over CPUs |

**How to read it.**

- Delivered close to programmed, with throttled above zero: the pool is saturated and
  users share it by weight. That is CPU Points working as intended.
- Delivered well below programmed and throttled at zero: no contention; weights do not
  matter, because idle capacity goes to whoever is runnable.
- Throttled is a saturation signal, not unserved demand. It is summed over CPUs, so it
  can exceed delivered and the two cannot be compared directly.

**Delivered cores by user slice** shows the same delivered time per user slice (top 10
per host), with or without an episode. Under saturation, compare the slices with their
configured points: a user with twice the points tends toward twice the cores, but only
while both have enough runnable work. A low share alone does not mean starvation; it
does only when the parent is throttled and that user still has work waiting.

**Programmed weights** (during an episode) shows the `cpu.weight` sums of guaranteed
(mapped) slices, of best-effort slices, and of all siblings including root. Root's
weight is all siblings minus guaranteed minus best effort. `observed siblings` is the
kernel's sum: it appears only when the denominator is `complete`, and then it equals
`all siblings`. A missing observed line means confirmation did not complete.

**Points budget** shows the configured split of the 1000 points: `reserve` (outside
`user.slice`), `pool` (1000 − reserve), the `root` and `best effort` entitlements, and
`applied guarantees`, the points of mapped and eligible users with an active slice in
the current plan (zero outside an episode). A low applied value compared with the map
means mapped users are not logged in or not eligible.

### Row 7: Authority and daemon health

**Observation-only state** is 1 while the host runs in `observation_only`. The
reason, such as an operator choice or an unusable systemd adapter, is in the startup
log and in MCP status. An operator choice needs no action; any other reason needs a
host fix followed by a restart. Error increments on the same host are drawn alongside.

**Errors by component** shows error increments by `component/error_type`:

| Component | Meaning |
|---|---|
| `cpu_points_reconciliation/*` | Applying or confirming the CPU Points plan failed after retries; the `error_type` names the failing step |
| `enforcement_observation/cpu_points_observation_failure` | Reading systemd topology or slice accounting failed; isolated increments during logins and logouts are normal |
| `enforcement_observation/ram_observation_failure` | Reading slice memory failed |
| `metrics_database/write_failure` | Writing the SQLite history failed: check disk space and permissions |
| `metrics_collection/system_load_failure` | Reading `/proc/loadavg` failed |

A sustained `cpu_points_reconciliation` series means the plan stays degraded and needs
attention. RAM and I/O refusals appear in the coverage tables, not here.

**Limit hooks** shows hook executions by `hook_type` (`script`, `http`) and `outcome`,
the queue depth against its capacity (dashed), and executions in flight.

| Outcome | Meaning |
|---|---|
| `success` | The script exited successfully or the webhook answered with success |
| `failure` | The script or endpoint reported an error |
| `timeout` | `LIMIT_HOOK_TIMEOUT` expired |
| `cancelled` | Shutdown stopped the job; expected during a restart |
| `saturated` | The queue was full and the event was rejected |

A depth near capacity or `saturated` outcomes mean the receiver is too slow or
`LIMIT_HOOK_MAX_CONCURRENCY` too low. A rejected notification never cancels the limit.

**Control cycle duration** shows the 95th percentile and the mean duration of decision
cycles, failed ones included. Healthy cycles take well under a second. The p95 line
cannot rise above 10 s because of the histogram buckets; trust the mean beyond that. A
cycle approaching `POLLING_INTERVAL` (or `PSI_FALLBACK_INTERVAL` with PSI active)
means the daemon no longer keeps up, usually because systemd or `/proc` is slow.

**Control cycles by trigger**

| Trigger | When |
|---|---|
| `initial` | Once, right after startup |
| `ticker` | Every `POLLING_INTERVAL`, or every `PSI_FALLBACK_INTERVAL` while PSI is active |
| `psi_system_cpu`, `psi_system_io` | A PSI pressure event started the cycle |
| `manual` | An MCP client requested a cycle |
| `io_weight_activation` | The weighted-I/O probe was accepted |

With `PSI_EVENT_DRIVEN=true`, a `ticker` rate of one per `POLLING_INTERVAL` means the
PSI watcher did not start (see the startup warnings); one per `PSI_FALLBACK_INTERVAL`
with no PSI cycles means PSI is active but pressure stays below the thresholds.

### Row 8: Weighted I/O policy

**Weighted I/O lifecycle**

| State | Meaning | Action |
|---|---|---|
| `disabled` | `IO_WEIGHT_DEVICES` is empty | None |
| `requested_pending` | Configured and waiting for a usable mechanism, adapter or device | Wait; retried automatically. On an `observation_only` host it never resolves |
| `probe_candidate` | Classification passed; the probe is about to run | None, transient |
| `functionally_accepted` | The owned probe proved the mechanism and weights are in force | None |
| `refused_observation` | The host refused for now, for example a topology change | Retried automatically |
| `refused_intervention` | A condition that needs a host fix, for example an external property conflict | Fix the host, then reload or restart |
| `release_pending` | The feature was removed while weights were programmed | None, transient until restored |

**Weighted I/O proof and application** (0 or 1 each): `accepted` mirrors the
`functionally_accepted` state. `programmed` means at least one owned user slice
carries a weight. `read back` means those weights were confirmed after the last
reconcile; programmed 1 with read back 0 means weights are still in place but
unconfirmed. `effect qualified` is retained contention evidence from the one
qualified platform; 0 elsewhere is normal and never blocks the feature.

**Weighted I/O attempts and partial coverage**: `classifications` and `probes` count
attempts per window. In steady state there is about one classification every 30
seconds and no probes; a steady stream of probes means the probe keeps failing.
`partial slices` counts slices whose user also runs processes outside the slice; their
weight covers only the part inside it.

### Frequently asked questions

**What is the difference between eligible and limited?** Eligible is who *may* be
limited according to configuration. Limited is who *is* limited right now: during an
episode every eligible user slice gets its CPU Points weight; outside an episode
nobody is limited.

**Why are many users limited when only one is using a lot of CPU?** Activation is
host-wide. Limiting means each slice gets a relative weight inside the capped
`user.slice`; idle users lose nothing, and the busy user is held to their share only
while others want CPU too.

**Why is limited zero even though the host is busy?** Check, in order: the
enforcement mode, whether `eligible` is above zero, whether eligible usage really
exceeds `CPU_THRESHOLD` (a share of the whole host) for `CPU_THRESHOLD_DURATION`,
blackout windows, and whether most of the load comes from outside eligible users.

**Why does limited exceed eligible?** Root's slice, and slices of UIDs outside the
monitored range, are limited when the include list matches them, but are not counted
as eligible.

**A user is limited but still gets a lot of CPU. Is ResMan broken?** No. CPU Points is
a relative share of what the parent delivers: when others are idle, a limited user
may use the free capacity. Look at **CPU Points delivery**: the share is enforced only
while the parent is `throttled_parent`.

**Why does a user have no RAM limit during an episode?** RAM must be enabled, the user
must be RAM-eligible, and their RAM coverage must be `complete`. `partial` or
`refused` means ResMan deliberately did not apply the limit.

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
# Check the monitored UID range
grep -E "SYSTEM_UID_(MIN|MAX)" /etc/resman/resman.conf

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

The dashboard and this guide are maintained together: use the dashboard JSON shipped
with the same ResMan release.
