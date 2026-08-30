# ResMan

Dynamic CPU, RAM, and IO resource manager for Linux using cgroups v2.

ResMan monitors system resources and automatically applies limits to users when load exceeds configurable thresholds. It exposes Prometheus metrics, supports hot-reload configuration, and includes an MCP server for AI assistant integration.

## Features

- Dynamic CPU, RAM, and IO limiting via cgroups v2
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
- Go 1.25.7+
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
# Creates build/deb/resman_1.30.8-1_<architecture>.deb

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
sudo cp packaging/systemd/resman.service /usr/lib/systemd/system/
sudo systemctl enable --now resman
```

Package installation does not enable or start the service automatically. Review
`/etc/resman/resman.conf`, then use `systemctl enable --now resman`. During an upgrade,
an already active service is restarted after the new package is configured.

Upgrading from 1.25.x to 1.30.8 is intentionally breaking. Complete the filesystem,
database, configuration, MCP, Prometheus, hook, capability, and container actions in
[`docs/UPGRADING.md`](docs/UPGRADING.md) before installing ResMan 1.30.8.

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
# LIMIT_HOOK_URL=https://example.internal/resman/user-limited
# Hook process output and URL credentials/paths are never copied into daemon logs.
# Shutdown cancels and drains in-flight hooks; Prometheus records each terminal outcome.
LIMIT_HOOK_TIMEOUT=10

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
without discarding threshold-duration progress.
Idle release uses each user's CPU EMA rather than a single instantaneous sample.
Because deactivation releases every resource together, `MIN_ACTIVE_TIME` protects
the most recent active enforcement epoch across CPU and RAM/I/O. It is also the
minimum hold time after an individual CPU user is initially limited or re-added;
re-adding a user does not restart the CPU enforcement epoch. Global deactivation
additionally requires all actively limited users to remain below
`CPU_RELEASE_THRESHOLD` for three `POLLING_INTERVAL` periods. This cool-down is
wall-clock based, so PSI events cannot shorten it.

Dynamic RAM/IO enable and user-filter changes are reconciled for cgroups that
are already active. Limits that are disabled or no longer applicable are reset
and retried on later cycles if cleanup fails.
RAM/IO-only eligible users run in standalone per-user cgroups with an unlimited
`cpu.max`; they do not inherit the finite shared CPU quota, and
`MIN_SYSTEM_CORES` gates CPU enforcement only.

`total_cpu_usage` is the host-wide normalized CPU percentage (0-100). Threshold
activation uses per-user CPU (`cpu_eligible_users_cpu_usage`), which is the sum of
process CPU and can exceed 100 on multi-core systems.
Host-wide CPU uses consecutive `/proc/stat` jiffy samples. Its baseline tolerates
normal scheduling jitter and one missed decision-loop tick: it expires after two
effective decision-loop intervals. That interval is `POLLING_INTERVAL` unless the PSI
watcher is active at runtime; only an active watcher switches it to
`PSI_FALLBACK_INTERVAL`. Configuring PSI when the watcher cannot start therefore keeps
the polling cadence. `METRICS_CACHE_TTL` controls only value reuse and does not set
this sampling window. The TTL must be at least one second. A cached value remains valid at the exact TTL boundary and
expires immediately after it; values above five minutes are not shortened by the
collector's periodic cleanup. Per-process CPU and I/O baselines remain attached to
active PID/start-time identities regardless of the configured sampling interval and
are pruned when a completed scan proves that the process disappeared.

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

Before moving a process into the shared limited cgroup, resman persistently
records its original cgroup together with its PID start time. On release, the
process is restored to that exact cgroup when it can legally accept processes.
An original cgroup that distributes controllers to children is an internal node
under cgroup v2 and cannot accept the process; it therefore uses the same
dedicated `resman/recovery/user_UID` leaf as a process whose original systemd
scope disappeared. The PID start time is revalidated immediately before every
restore write. A finite `CPU_QUOTA_NORMAL` is applied only to these resman-owned
recovery cgroups; resman never writes it into cgroups managed by systemd. If any
process cannot be restored safely during shutdown, the daemon exits non-zero
instead of reporting a successful service stop.

Live policy reconciliation remains fail-closed: if an excluded process has no
same-start-time recorded or inherited origin, it stays constrained while the
rest of the control cycle continues. Prometheus distinguishes this persistent
condition as `resman_errors_total{component="process_membership",error_type="origin_unavailable"}`;
other reconciliation failures use `error_type="reconciliation_failure"`. To
clear an unavailable-origin condition, stop or restart the affected process
under its owning service/session. If that is not possible while resman is
running, stop resman cleanly so shutdown can place the process in its recovery
leaf, then restart the owning service/session before starting resman again.

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

Limit hook scripts receive `RESMAN_LIMIT_*` environment variables. Webhooks receive
a JSON `POST` with `uid`, `username`, `enforceable_cpu_usage_percent`,
`cpu_eligible_users_count`,
`shared_cgroup`, `timestamp`, and `server_role`.

Dynamic fields are reloaded automatically. Restart the service after changing
fields marked static in `config/resman.conf.example`, such as cgroup paths or
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
- Supported rootful Podman deployment: `docs/CONTAINER.md`
- Grafana dashboard: `docs/dashboard-grafana-operations.json`
- Architecture: `docs/ARCHITECTURE.md`
- IO limits: `docs/IO-LIMITS.md`
- Authoritative defaults and lifecycle reference: [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)
- Upgrade guide from 1.25.x to 1.30.8: [`docs/UPGRADING.md`](docs/UPGRADING.md)
- Copyable configuration: `config/resman.conf.example`

## License

GNU General Public License v3.0 - see LICENSE file.
