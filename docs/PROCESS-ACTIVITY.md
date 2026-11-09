# Sampled process activity and collection cost

ResMan observes actual processes in the configured `SYSTEM_UID_MIN`–`SYSTEM_UID_MAX` range during the same fresh process scan used by a control cycle. `get_process_activity` reads these observations; it never starts a scan or advances CPU baselines. Processes excluded from enforcement remain observable. No PID or executable is added to Prometheus labels.

## Identity and coverage

A process identity is `(boot_id, pid, start_ticks)`. `start_ticks` is the exact kernel `/proc/PID/stat` start counter, serialized as a decimal **string** to preserve integers above JavaScript's safe integer range. A reused PID or another boot has a different identity. Lifetime comes from current uptime minus kernel start ticks; wall-clock changes do not redefine the identity.

| Field | Meaning |
|---|---|
| `runtime_available`, `runtime_seconds` | Availability and process lifetime at the most recent observation, including time before ResMan started. Missing, malformed or nonfinite uptime, or start after uptime, makes lifetime unavailable. |
| `first_observed_at`, `last_seen_at`, `samples` | Actual observations made by ResMan. |
| `cpu_covered_wall_seconds` | Sum of accepted sampled intervals; it excludes observation gaps. |
| `covered_cpu_seconds`, `average_cpu_percent` | Nonnegative cumulative CPU deltas over covered intervals, and their mean; 100% means one CPU core. |
| `cpu_available`, `cpu_average_available` | Availability of the last CPU counter and of the covered-interval mean, respectively. A first sample has no measured mean. |
| `memory_source`, `memory_bytes` | Fresh PSS when readable; otherwise RSS from the current stat snapshot. Unknown is explicit. |
| `memory_covered_seconds`, `average_memory_bytes` | Trapezoidal integral and mean of adjacent samples with matching memory provenance; availability is `memory_average_available`. |
| `gap_count`, `executable_changes` | Observed interruptions and executable/ownership identity changes. |
| `active` | Observed in the last process scan. It is not proof of continued execution at query time. |

An accepted CPU interval needs two consecutive observations of the same identity and executable segment, available counters, no counter reset, a positive duration, a complete scan, and a duration at most twice the control loop's effective sampling cadence. This uses the same runtime fallback contract as host CPU sampling: a working PSI loop can have a longer fallback interval than `POLLING_INTERVAL`; reload updates the accepted interval. Missing lifetime/uptime does not invalidate otherwise available raw CPU deltas or covered CPU observations. A counter reset or missing/unreadable sample does not become zero usage. The first sample after restart or a gap establishes a new baseline; durable totals survive, but daemon downtime contributes no covered interval. Temporary restore failure prevents activity writes until the previous totals can be recovered; independently covered new intervals are then added without bridging downtime.

An observed executable, owner, or executable-availability change starts a new activity segment and clears its CPU/RAM integrals. A PSS/RSS/unknown source change clears the RAM integral so different memory measurements are not averaged together. `first_observed_at` remains the first observation of that process identity; `activity_since` marks the current executable segment. The latest executable summary replaces earlier segments rather than claiming a complete executable event log.

These are polling observations. Short-lived tasks and an exec followed by another exec between polls may be missed. An identical executable path does not prove that no exec occurred. A missing task is unobserved, not a confirmed exit event. A high covered-interval mean does not prove continuous high usage during every second of a long lifetime.

`scan_complete`, `sampled_at`, and `unavailable_identities` describe the source snapshot separately from the entries. Missing boot identity makes process activity incomplete and increments unavailable identities, while valid authoritative CPU sampling can continue. Non-vanishing stat/identity-guard failures invalidate process CPU/I/O baselines and block control decisions from consuming partial process totals; they also leave CPU EMA unchanged. Executable and I/O permission failures retain their existing conservative enforcement semantics and separate availability flags.

## MCP query

The same typed tool is registered on HTTP and stdio. For example, processes with a lifetime of at least seven days and a mean of at least 80% over at least one hour of actual coverage:

```json
{
  "name": "get_process_activity",
  "arguments": {
    "uid": 1000,
    "min_runtime_seconds": 604800,
    "min_average_cpu_percent": 80,
    "min_covered_seconds": 3600,
    "active_only": true,
    "limit": 100
  }
}
```

All filters are optional; thresholds must be finite and nonnegative. `limit` defaults to 100 and is capped at 1000. Positive CPU thresholds require a measured mean; positive runtime thresholds require `runtime_available=true`. Unavailable runtime remains queryable without a positive lifetime filter and is never inferred from a numeric zero. `active_only` selects current in-memory observations; durable records alone never assert that a process remains active. Results sort by latest observation, with deterministic identity ties. A recent memory observation supersedes the same durable identity before final filtering, including when an exec resets a formerly qualifying mean.

The response contains `entries`, snapshot diagnostics, cumulative `evicted_entries`, and `persistence_status`: `disabled`, `available`, or `unavailable`. An empty result with incomplete coverage or unavailable persistence is not evidence of measured absence. `sampled_at` exposes age; queries do not refresh it.

## Bounds and persistence

The independent tracker keeps at most 10,000 process identities and no more than one interval baseline per retained identity. Eviction candidates are sorted once per scan; overflow never repeatedly scans the entire cache for each new process. Entries seen in the current scan are preferred over inactive history. If a scan exceeds capacity, omitted identities increment the loss counter. This is an explicit bounded observation cache, not an unlimited audit log.

With `METRICS_DB_ENABLED=true`, schema 12 stores typed JSON summaries and indexed identity/filter fields in `process_activity`. Writes use the existing metrics persistence cadence and atomically upsert a bounded snapshot. Same-boot restart restoration is bounded to the most recent 10,000 identities; other-boot durable history remains queryable. Indexed filters precede the durable query limit; internal overfetch is bounded to 11,000 rows to allow at most 10,000 in-memory identities to invalidate durable matches, and the public result still caps at 1000. Context deadlines/cancellation cover both database gate waits and SQL operations.

Raw metrics retention (`METRICS_DB_RETENTION_DAYS`) also expires old process summaries. Inactive memory summaries are pruned before persistence so the next write cannot recreate rows already removed by cleanup. Current observations refresh their own `last_seen_at`. Restore/persistence failure is explicit, immediately affects activity writer health, and does not become an authoritative process decision input. A successful raw SQL commit advances raw cadence and acknowledges its lifecycle events even if the separate activity write fails; the typed partial error, warning/error counter and unhealthy activity state remain visible until activity restore and persistence recover. Before the first process observation, an unused activity tracker adds no persistence obligation and its no-op cannot clear a previous activity failure. No new process-activity configuration knobs, continuous-activity alerts, or enforcement thresholds are introduced.

`processactivity.Tracker.mu` protects only bounded in-memory state. Collector and writer state mutexes are released before database or procfs I/O. Existing operation gates serialize scans and database mutations. Each scan uses at most `min(4, GOMAXPROCS)` workers, with an unbuffered indexed job channel and one result slot per enumerated PID; it never launches a goroutine per PID. A worker brackets UID/executable and fresh PSS/I/O with two matching start-tick reads. The scan owner joins every worker before updating baselines, aggregates results in enumeration order, and then publishes the snapshot. Normal and fallback enumeration use this same sampling/aggregation path. Stop waits for the current scan to join its workers. Kernel procfs reads themselves are not forcibly interrupted; there is no wall-time SLA. HTTP/stdio reads combine frozen memory observations with durable summaries without acquiring a process scan gate.

## Collection measurements

The reproducible fixture contains **45 UIDs and 240 PIDs**, with fresh stat/status/smaps_rollup/I/O files. It isolates the legacy CPU/memory read sequence from the shared guarded stat sequence and separately measures the complete scan including activity tracking, first with one worker and then with the bounded pool. The old routines are retained only in the benchmark file. Fixture construction replaces `NewProcess` with `Process{Pid}` plus its eager `CreateTime` read because synthetic PIDs cannot pass the host `signal(0)` check; the three legacy stat reads remain real library calls. Both paths update the same CPU baseline helper. This is a read-path comparison, not a full old-daemon versus new-daemon comparison.

Initial shared-stat measurements before the bounded pool on 2026-11-09, Intel N150, Linux 7.2.9+deb13-amd64, Go 1.27.3, `GOMAXPROCS=1`, five runs of ten scans:

| Measurement | Legacy read sequence | Shared guarded read sequence | Complete shared-stat scan including tracker (one worker) |
|---|---:|---:|---:|
| Median time per 240-process fixture scan | 32.184 ms | 22.595 ms | 14.527 ms |
| Allocated bytes per scan (representative run) | 12,368,172 | 2,328,468 | 3,957,817 |
| Allocations per scan (representative run) | 36,004 | 9,364 | 15,846 |
| Actual `stat` opens, one scan, `strace` | 720 | 480 | — |
| Actual `statm` opens, one scan, `strace` | 240 | 0 | — |

The separately timed scans experience different scheduling/cache conditions, so the complete-scan column must not be interpreted as a subtraction from the isolated shared path. The demonstrated structural result is one fewer stat read and no separate statm read per monitored process: CPU delta, lifetime, exact start identity, and RSS share one stat snapshot, followed by a second identity guard around UID/executable and fresh memory/I/O reads. The final source reads stat once for each unmonitored PID before learning its UID; a stat error for a successfully identified unmonitored UID does not block decisions. Legacy enumeration eagerly read stat for unmonitored PIDs as well, while monitored PIDs retain the measured two-read budget. No stale PSS is reused. The process-activity tracker performs no extra procfs reads.

The initial complete fixture CPU profile attributes 29.75% cumulative samples to stat reads, 13.44% to fresh memory reads, and 2.11% to tracker update. This regular-file fixture does not reproduce kernel page-table traversal. A separate read-only scan of the current host observed **254 enumerated PIDs, 45 monitored processes, one UID**, averaging **266 ms/scan**; five processes denied executable/I/O access. Its CPU profile attributed 91.45% cumulative samples to fresh memory reads. Host profiling created no workload, changed no settings, and performed no enforcement. Permissions and population make these observations host-specific.

The dominant host PSS cost motivated the implemented bounded pool. With `GOMAXPROCS=4`, five runs of ten complete 45-UID/240-PID fixture scans measured a final median **13.026 ms with one worker** and **8.936 ms with four workers**, with approximately 3.96 MB allocated per scan in both cases. Separate three-second profiles measured 14.718 ms and 11.366 ms respectively, illustrating timing variability; fresh-memory reads accounted for 11.41%/13.09% of cumulative CPU samples. The same bracket, fresh reads, aggregation order, and independent sampling states are preserved in both paths. Worker bounds, drain/join, serial/parallel equality, and observation-cadence isolation are regression tested.

Initial read-only host pool profiles observed 247 enumerated PIDs and one UID, but 37 monitored processes with one worker and 40 with four workers. Measured times were 144.851 ms and 144.083 ms; fresh memory accounted for 88.00% and 87.79% of sampled CPU. A final one-second-per-mode profile with the full metadata bracket observed **248 enumerated PIDs, 38 monitored processes and one UID** in both modes, measuring **128.279 ms with one worker and 110.475 ms with four**. Equal counts do not establish equal live memory maps or workload; neither run qualifies a production speedup. The fixture demonstrates bounded concurrency and a reproducible local comparison; it does not reproduce kernel page-table traversal or production topology.

The former guest's reported 515 ms scan has **not** been reproduced or qualified as fixed. Fresh PSS, UID discovery, and total procfs work remain proportional to process count and kernel memory-map cost, with at most four simultaneous per-process reads. The implementation removes proven duplicate CPU/statm reads and the mandatory serial PSS path, bounds concurrency and tracker/query state, and retains a fresh identity bracket. It does not impose a process-count cutoff on authoritative collection or reuse old PSS as a fresh observation; production latency and host impact still require qualification on the target population.

Raw benchmark output and profile summaries are retained in [collection-20261109.txt](evidence/collection-20261109.txt). Reproduce from the repository root:

```bash
export PATH=/usr/local/go/bin:/home/francesco/go/bin:/usr/bin:/bin
export GOCACHE=/home/francesco/go/cache
export GOMODCACHE=/home/francesco/go/pkg/mod
go test -c -o /tmp/resman-metrics.test ./metrics
GOMAXPROCS=1 /tmp/resman-metrics.test -test.run '^$' \
  -test.bench 'Benchmark(ProcessReadPath240|CollectAllUserMetrics240)$' \
  -test.benchtime=10x -test.count=5 -test.benchmem
GOMAXPROCS=1 /tmp/resman-metrics.test -test.run '^$' \
  -test.bench '^BenchmarkCollectAllUserMetrics240$' -test.benchtime=3s \
  -test.cpuprofile=/tmp/resman-collection-fixture.pprof
go tool pprof -top -cum /tmp/resman-metrics.test /tmp/resman-collection-fixture.pprof
GOMAXPROCS=4 /tmp/resman-metrics.test -test.run '^$' \
  -test.bench '^BenchmarkCollectProcessPool240$' -test.benchtime=10x -test.count=5
# For serial/parallel CPU profiles, select /1$ or /4$ and add -test.cpuprofile.
# Optional: reads the current host, with its real population and permissions.
GOMAXPROCS=4 /tmp/resman-metrics.test -test.run '^$' \
  -test.bench '^BenchmarkCollectCurrentProcReadOnly$' -test.benchtime=1s \
  -test.cpuprofile=/tmp/resman-collection-host.pprof
strace -f -e trace=openat -o /tmp/resman-stat-legacy.trace \
  /tmp/resman-metrics.test -test.run '^$' \
  -test.bench '^BenchmarkProcessReadPath240/legacy$' -test.benchtime=1x
strace -f -e trace=openat -o /tmp/resman-stat-shared.trace \
  /tmp/resman-metrics.test -test.run '^$' \
  -test.bench '^BenchmarkProcessReadPath240/shared_guarded$' -test.benchtime=1x
```

Regression tests cover exact and canonical zero start ticks, guarded metadata/PID reuse, unknown uptime/lifetime, missing samples, restart/restore failure, exec changes, memory provenance, capped churn, retention, HTTP/stdio parity, unavailable persistence, and the two-stat read budget in both normal and fallback scans. Existing CPU delta/EMA and decision-observation isolation tests remain applicable.
