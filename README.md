# ResMan

Current metrics schema: 7.

Dynamic CPU, RAM, and IO resource manager for Linux using cgroups v2.

ResMan monitors system resources and applies limits to users when the active host
ownership model permits safe enforcement. It exposes Prometheus metrics, supports
hot-reload configuration, and includes an MCP server for AI assistant integration.

> **Breaking 1.35 upgrade:** `systemd_native` is the only enforcing backend.
> ResMan never moves PIDs or creates an enforcement hierarchy of its own. When an
> authoritative systemd adapter is unavailable, it reports `observation_only` and
> applies no CPU, RAM or I/O limits. Read the upgrade guide before installation.

## Features

- Systemd-native CPU limiting without PID migration; RAM and I/O require complete independent authority
- PSI event-driven mode: uses poll() on cpu.pressure/io.pressure to trigger control cycles when the kernel reports real pressure/stall, while keeping polling as a heartbeat
- Per-user resource tracking with Prometheus metrics
- Configurable thresholds with time-window delay to prevent false activations
- User filtering via include/exclude regex lists
- Blackout timeframes to avoid applying limits during business hours
- Automatic configuration reload on file changes
- MCP server for AI assistant integration ([authoritative discovery inventories](docs/MCP-README.md#discovery-inventories))
- SQLite metrics database for historical data
- Optional script/webhook notification when a user is limited
- LDAP/NIS username resolution support (CGO)
- Grafana dashboard included

## Requirements

- Linux kernel 4.18 or later with cgroups v2 support
- Enterprise Linux 8 or later for the published RPM package
- Go 1.27.1+
- CGO enabled (for LDAP/NIS username resolution)
- Debian package tools (`dpkg-dev`) when building `.deb` packages

## Build

```bash
# Development build
make build

# RPM package
make rpm

# Native Debian/Ubuntu package (amd64 or arm64)
make deb
# Creates build/deb/resman_1.36.2-1_<architecture>.deb

# All packages
make all-with-packages
```

CGO must be enabled for LDAP/NIS support:

```bash
CGO_ENABLED=1 go build -v -ldflags="-s -w" -o resman .
```

The release RPM is a CGO build produced on Enterprise Linux 8. EL8 is the
minimum supported RPM userspace baseline (glibc 2.28); building on that baseline
keeps the package compatible with EL8 and later compatible Enterprise Linux
releases. The RPM is not a static binary, because static builds cannot preserve
the required NSS/LDAP/SSSD user resolution behavior.

The `.deb` is a native CGO build. `dpkg-shlibdeps` records the actual minimum
runtime library versions, so release artifacts should be built on the oldest
Debian or Ubuntu baseline that the release intends to support.

## Install

```bash
# From packages
sudo rpm -ivh resman-*.rpm
# or
sudo apt install ./resman_*.deb

# From source
sudo cp resman /usr/bin/
sudo install -d -m 0700 /etc/resman /var/lib/resman
sudo install -m 0600 config/resman.conf.example /etc/resman/resman.conf
sudo install -m 0600 config/cpu-points.map.example /etc/resman/cpu-points.map
sudo cp packaging/systemd/resman.service /usr/lib/systemd/system/
sudo systemctl enable --now resman
```

Package installation does not enable or start the service automatically. Review
`/etc/resman/resman.conf`, then use `systemctl enable --now resman`. During an upgrade,
an already active service is restarted after the new package is configured.

Upgrading any ResMan release from 1.25.x through 1.35.4 to 1.36.2 is intentionally
breaking. Complete the filesystem, database, configuration, MCP, Prometheus, hook,
capability, and container actions in [`docs/UPGRADING.md`](docs/UPGRADING.md) before
installing ResMan 1.36.2.

The packaged unit does not retry configuration, required cgroup-capability, or MCP TLS
credential rejections: these exit with status 78 and remain failed until the operator
fixes the cause. Failures reading or writing setup state for an otherwise present
cgroup capability, including `cgroup.subtree_control`, remain transient. They and other
failures are retried after 10 seconds, with at most three starts per minute; after the
start limit is reached, use `systemctl reset-failed resman` after correcting the cause.
See `man resman` and `journalctl -u resman` for diagnostics.

## Usage

Edit `/etc/resman/resman.conf` to configure thresholds and filters:

```bash
# CPU thresholds
CPU_THRESHOLD=75
CPU_RELEASE_THRESHOLD=40
CPU_THRESHOLD_DURATION=90
PROCESS_MIN_AGE_SECONDS=60

# CPU Points: reserve 100/1000 outside ResMan; assign lendable minimums of
# 100/1000 to active root sessions and 100/1000 to best-effort users.
CPU_RESERVE_POINTS=100
CPU_ROOT_POINTS=100
CPU_BEST_EFFORT_POINTS=100
CPU_POINTS_FILE=/etc/resman/cpu-points.map

# User filtering (empty = no users limited, .* = all users)
USER_INCLUDE_LIST=.*
USER_EXCLUDE_LIST=root,admin

# Enable RAM and IO limits
RAM_LIMIT_ENABLED=false
IO_LIMIT_ENABLED=false

# Prometheus metrics (default: localhost:1974)
ENABLE_PROMETHEUS=true

# Notify when a user is newly limited
LIMIT_HOOK_ENABLED=false
# LIMIT_HOOK_SCRIPT=/usr/local/bin/resman-user-limited
# LIMIT_HOOK_SCRIPT_USER=resman-hook
# LIMIT_HOOK_SCRIPT_GROUP=resman-hook
# LIMIT_HOOK_URL=https://example.internal/resman/user-limited
# Hook process output and URL credentials/paths are never copied into daemon logs.
# Script and URL jobs receive independent deadlines. Shutdown cancels and drains them.
LIMIT_HOOK_TIMEOUT=10
LIMIT_HOOK_MAX_CONCURRENCY=2
LIMIT_HOOK_QUEUE_CAPACITY=64

# MCP server
MCP_ENABLED=true
MCP_TRANSPORT=stdio
# MCP_AUTH_TOKEN=replace-with-a-random-token  # Required with MCP_TRANSPORT=http

# PSI event-driven mode (optional, Linux >= 4.20 with CONFIG_PSI=y)
# PSI_EVENT_DRIVEN=true
# PSI_CPU_STALL_THRESHOLD=50000
# PSI_FALLBACK_INTERVAL=300
# METRICS_REFRESH_INTERVAL=30
```

RPM and Debian packages also ship a no-argument email adapter example at
`/usr/share/doc/resman/scripts/resman-sendmail-hook.sh`. Copy and configure it before
use; the adjacent generic `sendmail.sh` requires command-line arguments and is not
itself a ResMan hook. See [docs/scripts/README.md](docs/scripts/README.md).

The MCP endpoint is protocol-stateless and accepts only revision `2026-07-28`
over stdio or HTTP. HTTP authentication and protocol metadata are validated on
every request; legacy initialization, sessions, and protocol fallbacks are not
supported. See [docs/MCP-README.md](docs/MCP-README.md) for the wire contract.

`USER_INCLUDE_LIST` controls CPU-limit eligibility, not metrics collection.
When it is empty or unset, resman continues to monitor users but applies no CPU
limits. Set it to `.*` to make every non-excluded user eligible. Empty
`RAM_USER_INCLUDE_LIST` and `IO_USER_INCLUDE_LIST` values continue to include
all users for their respective optional controllers.

Activation timing for a new CPU-bound process includes one baseline CPU sample
before `CPU_THRESHOLD_DURATION` starts. Every process state is sampled
immediately; `PROCESS_MIN_AGE_SECONDS` affects only the lifetime-average metric.
With `POLLING_INTERVAL=30` and `CPU_THRESHOLD_DURATION=90`, limits normally
activate about 120 seconds after the first observation, plus polling alignment.
With `IGNORE_SYSTEM_LOAD=false`, a high load average suppresses activation only when
CPU-eligible users account for less than half of measured aggregate host CPU activity.
At least half remains actionable; an unavailable host CPU sample delays activation
without discarding threshold-duration progress. The control cycle owns an uncached
`/proc/stat` baseline; `METRICS_CACHE_TTL` applies only to the independent observation
stream used by status surfaces.
Idle release uses each user's CPU EMA rather than a single instantaneous sample.
Because deactivation releases every resource together, `MIN_ACTIVE_TIME` protects
the most recent active enforcement epoch across CPU and RAM/I/O. It is also the
minimum hold time after an individual CPU user is initially limited or re-added;
re-adding a user does not restart the CPU enforcement epoch. Global deactivation
additionally requires all actively limited users to remain below
`CPU_RELEASE_THRESHOLD` for three `POLLING_INTERVAL` periods. This cool-down is
wall-clock based, so PSI events cannot shorten it.

Dynamic RAM/IO enable and user-filter changes are reconciled on authoritative user
slices. Limits that are disabled or no longer applicable are restored and retried on
later cycles if restoration fails. RAM/IO-only eligibility does not create a separate
cgroup or change CPU membership.

CPU Points normalizes the online host capacity to 1000 points. The finite parent
pool is `1000 - CPU_RESERVE_POINTS`. The reserve is nominal headroom outside that
parent, not exclusive isolation from arbitrary host workloads. Reserve zero still
programs a finite full-capacity `cpu.max`, which can throttle and under-deliver under
saturation. A mapped user receives its configured relative share of the bandwidth
actually delivered to the parent while CPU enforcement is active. The reserve protects
system.slice and other workloads outside user.slice, not an unbounded root shell. Native CPU coverage
includes all descendants of the authoritative user slice; an authority-split UID has
partial coverage. This is not an absolute host CPU floor or cpuset isolation.

All runnable user slices may borrow unused capacity. Root has its own entitlement;
excluded and unmapped slices share aggregate best effort. Processes remain in their
authoritative systemd units.

The strict map begins with `[resman-cpu-points-map-v1]` and then contains exact
`username=points` assignments. A dot is part of the username, so
`john.smith=200` maps the exact NSS identity `john.smith`. Configured guarantees plus
the root and best-effort entitlements must not exceed the pool. UID 0 has the dedicated
`CPU_ROOT_POINTS` entitlement and is rejected from the map. The default package installs
`/etc/resman/cpu-points.map` as a root-owned regular mode-`0600` file below the
mode-`0700` configuration directory. Custom paths must satisfy the same regular-file,
ownership, mode, trusted-ancestor, and no-symlink checks.

Reserve, root, best-effort, and map-content changes form one atomic hot-reload epoch;
`CPU_POINTS_FILE` path changes require a restart. Active class and weight changes
reconcile in place without moving processes.

For accounting, coverage, schema 7 and operator measurements, see
[CPU Points observability](docs/CPU-POINTS-OBSERVABILITY.md). Use the daemon's
synchronized deltas and a 60-second observation window; raw weights do not prove delivery.

Native RAM observes existing slice charges and applies page-aligned limits in place;
`authority_split` or `runtime_owned_descendant` refuses RAM/I/O independently of CPU.
With the normal `memory.high < memory.max` configuration and no
swap or reclaimable pages, a process can stay alive but effectively stall at high:
`high` events rise while `max`, `oom`, and `oom_kill` remain zero indefinitely. Raise
or disable `memory.high`, provide reclaimable capacity or swap, or release the RAM
limit. An explicit `memory.high = memory.max` control has different max/OOM behavior.

When metrics persistence is enabled, SQLite schema version 7 records each decision
sample as one common system/user epoch. History distinguishes configured guarantee,
applied CPU class and weight, delivered parent/slice bandwidth and throttling, raw
unit diagnostics, independent resource authority, and process-derived memory from
slice RAM charges and high/max/OOM events. Missing comparable baselines are `null`, not
zero; incompatible older databases must be archived or deleted before restart. See
[`docs/METRICS-DATABASE.md`](docs/METRICS-DATABASE.md).

`total_cpu_usage` is the host-wide normalized CPU percentage (0-100). Threshold
activation uses per-user CPU (`cpu_eligible_users_cpu_usage`), which is the sum of
process CPU and can exceed 100 on multi-core systems.
Host-wide CPU uses two independent streams of consecutive `/proc/stat` jiffy samples.
The decision stream is read once per control epoch without the general metrics cache.
Its first sample establishes a baseline and is explicitly unavailable; later samples
distinguish a measured zero from an unreadable, stale, reset, or zero-delta sample.
Its baseline tolerates normal scheduling jitter and one missed decision-loop tick: it
expires after two effective decision-loop intervals. That interval is
`POLLING_INTERVAL` unless the PSI watcher is active at runtime; only an active watcher
switches it to `PSI_FALLBACK_INTERVAL`. The observation stream owns a separate baseline
and may reuse its value for `METRICS_CACHE_TTL`; observation refreshes cannot satisfy or
advance a decision sample. The TTL must be at least one second. A cached observation
remains valid at the exact TTL boundary and expires immediately after it; values above
five minutes are not shortened by periodic cleanup. Per-process CPU and I/O baselines
remain attached to active PID/start-time identities regardless of the configured
sampling interval and are pruned when a completed scan proves that the process
disappeared.

Prometheus per-user series are published only from the authoritative control-cycle
sample. Observation-only refreshes update system-wide telemetry but never overwrite
per-user CPU deltas or EMA with values from their independent sampling window.

Per-user memory uses proportional set size (PSS) from
`/proc/PID/smaps_rollup`, preventing shared pages from being counted once per
process. RSS is used only when PSS is unavailable.

SQLite history is written one collection cycle per transaction. File-backed
databases use WAL mode and a 5-second busy timeout, and all stored/query
timestamps are normalized to UTC. Existing databases that still use SQLite's
default `auto_vacuum=NONE` are migrated once to incremental auto-vacuum at
startup. That first upgraded startup runs a full `VACUUM`, which can take longer
and requires temporary free disk space proportional to the database size. The
database parent is process-owned mode `0700`; the database and existing WAL/SHM
sidecars are regular, non-symlink mode `0600` files. Unsafe existing custom
paths or replaceable/symlinked ancestors are refused before SQLite opens them
rather than relying on the umask or a check-then-open race.

ResMan enforces only through authoritative systemd units. It never creates an
enforcement cgroup hierarchy, records process origins, or writes PID membership.
If the systemd adapter is unavailable, the daemon remains in explicit
`observation_only` mode and applies no CPU, RAM or I/O limit.

`PSI_EVENT_DRIVEN` is a pressure trigger, not another CPU usage threshold. PSI
events mean that runnable tasks or IO operations spent time waiting for resources.
For example, on a 4-core host, 4 CPU-bound threads can show 100% CPU with little
PSI if there is no meaningful queue; 8 runnable CPU-bound threads are more likely
to generate CPU PSI because work is waiting. resman records the cycle trigger
(`initial`, `ticker`, `psi_system_cpu`, `psi_system_io`, `psi_user_cpu`,
`psi_user_io`) and exports PSI event counters so dashboards can separate usage
from pressure.

CPU pressure files that expose only the `some` line, as on older kernels with
PSI backports, are supported; the `full` line is treated as optional.

On Red Hat Enterprise Linux 8 and later, PSI may be compiled in but disabled at
boot. Enable it with:

```bash
sudo grubby --update-kernel=ALL --args="systemd.unified_cgroup_hierarchy=1 psi=1"
sudo reboot
```

Red Hat documents the performance impact of enabling PSI as slight (<1%).
After reboot, verify that PSI is active with `ls /proc/pressure`. If PSI files
are still unavailable, resman falls back to the normal polling loop.

When `PSI_EVENT_DRIVEN=true`, `PSI_FALLBACK_INTERVAL` is only the decision-loop
heartbeat. Prometheus/Grafana metrics are refreshed separately every
`METRICS_REFRESH_INTERVAL` seconds so dashboards remain current even without PSI
events. That refresh does not apply or remove limits and uses independent per-process
CPU baselines, EMA state, and cache entries, so changing the observability cadence
cannot change the next control decision.

PSI mode, trigger thresholds, tracking window, fallback interval, and metrics
refresh interval support hot reload. Changes that affect kernel PSI triggers
rebuild the watcher; loop interval changes take effect immediately.

Limit hook scripts run under an explicit non-root NSS user and group, without
supplementary groups or the daemon environment. Script and webhook deliveries use a
fixed worker pool and bounded queue; saturation never blocks enforcement. Webhooks
receive a JSON `POST` with `uid`, `username`,
`enforceable_cpu_usage_percent`, `cpu_eligible_users_count`, `timestamp`, and
`server_role`. Each mechanism receives an independent
`LIMIT_HOOK_TIMEOUT` deadline. See [Limit-hook execution](docs/LIMIT-HOOKS.md) for the
complete security, shutdown, and observability contract.

Dynamic fields are reloaded automatically. Restart the service after changing
fields marked static in `config/resman.conf.example`, such as the cgroup observation root or
Prometheus listener, TLS and authentication settings, logging backend settings,
or MCP listener and security settings:

```bash
sudo systemctl restart resman
```

When Prometheus TLS is enabled, `PROMETHEUS_TLS_MIN_VERSION` is enforced by the
server. Setting `PROMETHEUS_TLS_CA_FILE` also enables mandatory client
certificate verification; leave it empty for one-way TLS.

Monitor the service:

```bash
sudo systemctl status resman
journalctl -u resman -f
curl -s http://localhost:1974/metrics | grep resman
```

## Documentation

- Man page: `man resman`
- Development guide (required reading before contributing code): `docs/DEVELOPMENT.md`
- Dependency inspection and controlled updates: [`docs/DEPENDENCY-MANAGEMENT.md`](docs/DEPENDENCY-MANAGEMENT.md)
- Supported rootful Podman deployment: `docs/CONTAINER.md`
- Grafana dashboard: `docs/dashboard-grafana-operations.json`
- Architecture: `docs/ARCHITECTURE.md`
- IO limits: `docs/IO-LIMITS.md`
- Authoritative defaults and lifecycle reference: [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)
- Upgrade guide from 1.25.x through 1.35.4 to 1.36.2: [`docs/UPGRADING.md`](docs/UPGRADING.md)
- Copyable configuration: `config/resman.conf.example`

## License

GNU General Public License v3.0 - see LICENSE file.
