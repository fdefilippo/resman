# Upgrading from ResMan 1.25.x to ResMan 1.30.5

This guide applies when moving from ResMan 1.25.x to ResMan 1.30.5. This release
contains the post-1.25.1 audit remediation and intentionally breaks incorrect or
ambiguous contracts. It does not migrate old database schemas, accept removed
configuration keys, preserve old MCP shapes, or alias renamed metrics.

Read this document before installing the new package. Complete the required actions
while ResMan is stopped; otherwise the service can correctly refuse startup before the
operator-authored configuration has been recovered.

## Pre-upgrade checklist

1. Stop ResMan and retain a protected copy of the operator-authored configuration.
2. Compare that configuration with [`CONFIGURATION.md`](CONFIGURATION.md), remove all
   rejected keys, and account for corrected defaults and list semantics.
3. Install the authoritative configuration as a regular mode-`0600` file at
   `/etc/resman/resman.conf` below a root-owned mode-`0700` `/etc/resman` directory.
4. Remove or securely archive every legacy configuration artifact described below.
5. Archive or delete the 1.25.x metrics database. It cannot be opened by the new
   schema.
6. If MCP uses HTTP, provision its certificate and key, update clients to HTTPS and
   MCP revision 2026-07-28, and update the health probe.
7. Update Prometheus queries, alert names, MCP decoders, and limit-hook consumers for
   the removed fields listed below.
8. Verify the cgroup interfaces required by enabled features and, for containers,
   adopt the supported rootful Podman topology.
9. Install the package, inspect `journalctl -u resman`, and start the service. If the
   systemd start limit is reached, repair the reported cause before running
   `systemctl reset-failed resman` and starting it again.

## Filesystem layout and persisted state

### Configuration moves below `/etc/resman`

**Visible change.** The default configuration moves from `/etc/resman.conf` to
`/etc/resman/resman.conf`. Startup rejects the old file, an RPM-created
`/etc/resman.conf.rpmsave`, `/etc/resman.conf.backup`, `/etc/resman.conf.tmp`, and
every `/etc/resman.conf.backup_*` entry, even when the new file already exists.
Timestamped backups created by 1.25.1 can be mode `0644` and contain
`MCP_AUTH_TOKEN`.

**Cause.** Configuration and its rolling backup are now secret-bearing state below a
root-owned mode-`0700` directory. The startup guard prevents an upgrade from silently
using a package default while leaving the authored configuration or credential copies
orphaned in `/etc`.

**Action.** Stop ResMan. Choose the authoritative contents from `/etc/resman.conf` or
`/etc/resman.conf.rpmsave`, install them as a regular root-owned mode-`0600`
`/etc/resman/resman.conf`, and remove both legacy source files. Move any needed
operator-managed `backup_*` copy into a protected archive outside the legacy path;
securely remove generated or unneeded matching copies, `.backup`, and `.tmp`. A custom
explicit `--config` path is exempt from the default-layout guard.

### Configuration persistence is stricter

**Visible change.** MCP filter writes preserve the source mode and owner, use one
rolling backup, and reject symbolic links, including dangling links. A process that
cannot reproduce the source ownership receives an actionable failure instead of a
more-permissive replacement. Cleanup beside a custom configuration removes only exact
historical names of the form `.backup_YYYYMMDD_HHMMSS` and the exact `.tmp` file;
operator-named prefix matches are retained.

**Cause.** Previous releases produced unbounded, potentially world-readable copies of
the configuration and followed a link while replacing a different inode. Persistence
is now atomic, durable, bounded, and fail-closed.

**Action.** Select the real target with `--config` or replace the link with a regular
file. Ensure the service can preserve the file UID:GID without weakening its mode. If
an error says the persistence coordinator is unusable, stop ResMan, restore
`<config>.backup` (or remove a newly created file when instructed), verify the active
contents, and restart. Do not retry writes in the same process after an unconfirmed
rollback.

### Metrics history moves and requires a reset

**Visible change.** The default database moves from `/etc/resman/metrics.db` to
`/var/lib/resman/metrics.db`. Unversioned, schema-version-2, and other incompatible
stores are rejected; the current schema version is 3. The immediate parent must be a
real, process-owned mode-`0700` directory. The database and pre-existing `-wal` and
`-shm` sidecars must be regular, process-owned mode-`0600` files. Replaceable or
symbolic-link ancestors are rejected. Failure disables historical persistence with an
explicit remedy while resource enforcement remains active.

**Cause.** Persisted fields now distinguish CPU enforcement, RAM/I/O enforcement, any
enforcement, CPU-active users, and the active-user union. Reinterpreting old rows would
silently corrupt history, and relying on SQLite defaults or the service umask could
expose per-user data.

**Action.** Stop ResMan and archive or delete the old database; it is not migrated.
Create a stable, non-symlink hierarchy with a service-owned mode-`0700` immediate
parent. Set any retained database and sidecars to the service UID and mode `0600`, or
let ResMan create a new schema-3 store. The `:memory:` database is unchanged.

### Runtime-state and log paths change

**Visible change.** Created-cgroup state moves from `/var/run/resman-cgroups.txt` to
`/run/resman-cgroups.txt`; the process-origin state follows that base. File logs and
packaged rsyslog/logrotate output are mode `0600`. Existing regular logs lose execute
bits and all permissions for other users; symbolic-link and non-regular log targets
are rejected.

**Cause.** Boot-scoped state now uses the canonical `/run` path, while logs can contain
user names, errors, and previously unredacted endpoint material and are therefore
treated as secret-bearing state.

**Action.** Update tooling that reads the runtime-state path. Log collectors that need
shared access must use an intended group and a group-only mode such as `0640`; access
for other users is no longer supported.

## Configuration loading and reload

### Removed and unknown keys now fail startup

**Visible change.** Unknown file keys fail with file, line, and key. The following
removed keys are explicitly rejected in files and environment overrides:

- `CPU_QUOTA_LIMITED`
- `RAM_QUOTA_LIMITED`
- `METRICS_CACHE_FILE`
- `PROMETHEUS_FILE`
- `PROMETHEUS_JWT_EXPIRY`
- `CONFIG_FILE`
- `PROMETHEUS_HOST`
- `PROMETHEUS_PORT`
- `USER_WHITELIST`

**Cause.** These keys were inert, misleading aliases, or duplicated a contract owned
elsewhere.

**Action.** Remove them. Select the file with `--config`; use
`PROMETHEUS_METRICS_BIND_HOST`, `PROMETHEUS_METRICS_BIND_PORT`, and
`USER_EXCLUDE_LIST` where applicable. JWT lifetime comes from each signed `exp` claim.

### Defaults and eligibility semantics are explicit

**Visible change.** The Prometheus exporter default bind is `127.0.0.1`, not
`0.0.0.0`. `SYSTEM_UID_MAX` normally follows `/proc/sys/kernel/pid_max`, with `60000`
only as a read-failure fallback. An empty `USER_INCLUDE_LIST` selects nobody for CPU
limiting; empty RAM and I/O include lists select every non-excluded user. The shipped
example now matches runtime defaults, including empty exclusion lists,
`IGNORE_SYSTEM_LOAD=false`, and empty credential fields.

**Cause.** Earlier shipped references disagreed with runtime, and CPU eligibility was
incorrectly reused for RAM and I/O.

**Action.** Compare authored configuration with `CONFIGURATION.md`. Use
`USER_INCLUDE_LIST=.*` when all non-excluded users must be CPU-eligible. Treat a
deliberate Prometheus `0.0.0.0` bind as remote exposure requiring TLS, authentication,
firewall restrictions, and an explicit configuration entry.

### Reload has one authoritative lifecycle

**Visible change.** A reload that changes a restart-required key returns an error
listing key names without values and preserves the active values. Dynamic changes in
the same file are applied atomically as one epoch. Configuration persistence snapshots
values without holding the live configuration lock across filesystem I/O, so slow
backup or directory sync no longer blocks control-cycle getters. A pure restart-required
outcome now emits one structured `WARN` with `rejected_fields` and `processed=true`;
the previous per-field warning, partial-apply warning, and duplicate error records are
not emitted. A genuine or mixed failure emits one `ERROR` and is never downgraded merely
because it also contains rejected restart-required fields.

**Cause.** Previous reloads could publish a new configuration while constructed
components still used old listener, storage, logging, or security settings.

**Action.** Restart after changing cgroup or storage paths, database enablement/path or
write interval, Prometheus or MCP listener/security settings, logging backend, or
`SERVER_ROLE`. Treat the reload rejection as proof that the old values remain active.
`METRICS_DB_RETENTION_DAYS` remains dynamic; `USERNAME_CACHE_TTL` applies at startup
and reload even when the database is disabled. Log-based monitoring should match the
single terminal record and consume `rejected_fields` instead of the former `field` key.

### Cache TTL validation and retention change

**Visible change.** `METRICS_CACHE_TTL` below one second is rejected. Values above five
minutes retain their full configured lifetime, and active PID/start-time CPU and I/O
baselines are pruned by completed scans rather than a fixed five-minute timer.

**Cause.** A generic cleanup timer previously shortened unrelated cache and sampling
contracts; non-positive TTLs silently disabled reuse.

**Action.** Configure at least one second. No action is required for longer values;
sustained activity may now keep a valid baseline where 1.25.x periodically reset it.

## MCP clients and probes

### MCP is revision-2026-07-28, stateless, and HTTPS-only

**Visible change.** Both HTTP and stdio accept only MCP revision `2026-07-28`.
Pre-2026 revisions, `initialize`/`initialized` legacy lifecycle flows, legacy session
identifiers, and compatibility flags are rejected. HTTP is protocol-stateless,
defaults to `127.0.0.1:1969`, requires TLS, and defaults to TLS 1.3. Missing or invalid
certificate/key material makes the configured MCP service fail startup. `/health`
shares the same HTTPS listener and, when `MCP_TLS_CA_FILE` is set, requires a client
certificate just like `/mcp`.

**Cause.** The server now implements one current protocol boundary without session or
transport compatibility state, and bearer credentials never travel in clear text.

**Action.** Upgrade every client to revision 2026-07-28 and remove initialization and
session handling. Use `https://`, trust the configured CA or server certificate, and
update health probes. Generate local certificates with `generate-tls-certs.sh` or use
`MCP_TRANSPORT=stdio` for local integrations that do not need a network listener.
MCP TLS keys use the same literal default paths as Prometheus but do not inherit
Prometheus overrides. Setting `MCP_TLS_CA_FILE` enables mTLS in addition to the bearer
token.

### Filter-write tools have a synchronous contract

**Visible change.** `set_user_include_list` and `set_user_exclude_list` reject the
removed `reload` argument. Success reports `persisted=true` and `applied=true` only
after the watcher has validated and applied the requested policy. Concurrent MCP write
calls are rejected. A failure can report persisted and applied state separately.

**Cause.** The old flag had no truthful setting, and a fixed sleep acknowledged reload
before the watcher could complete.

**Action.** Remove `reload` from calls and handle the two result fields. On watcher
failure, deadline, or concurrent file replacement, reconcile the persisted file with
the reported runtime state before retrying.

### Status and resource payloads are resource-specific

**Visible change.** The following ambiguous or nonexistent fields are absent:
`total_user_cpu_usage`, `user_cpu_usage`, `active_users_count`, `active_users`,
`limits_active`, and `limits_applied_time`. Clients must use:

- `observed_users_cpu_usage` and `observed_users_count` for observation;
- `actively_limited_users_count` and `actively_limited_users` for the enforcement
  union;
- `cpu_actively_limited_users_count` and `cpu_actively_limited_users` for CPU;
- `cpu_limits_active`, `resource_limits_active`, and `any_limits_active` for state;
- `cpu_limits_applied_time` and `resource_limits_applied_time` for activation epochs.

`resman://users/active` changes from a bare `[{"uid":...}]` array to the typed object
returned by `get_active_users`, including `hostname`, `server_role`, and users with
`uid` and `username`. `get_configuration` and `resman://config` now share the complete
CPU/RAM/I/O policy schema. `resman://users/{uid}/metrics` shares the tool projection;
optional zero numeric values can be omitted under the existing `omitempty` contract.

**Cause.** Metrics, runtime state, and sibling MCP surfaces formerly used duplicated
string maps and incompatible projections.

**Action.** Update typed decoders to the documented shared schemas. Do not interpret a
missing old field as zero or alias it to a new resource-specific field.

### Cgroup information uses typed availability and reasons

**Visible change.** `get_cgroup_info` and `resman://cgroups/{uid}` use underscore names
such as `cpu_max`, `cpu_weight`, and `memory_current`; the resource no longer emits raw
dotted keys. Optional values are omitted when unreadable. Each has a `*_available`
gate, including `cpu_max_available`, `cpu_weight_available`, and
`memory_current_available`, and, when unavailable, a bounded
`*_unavailable_reason`: `not_present`, `permission_denied`, or `read_error`.

**Cause.** Empty strings and absent raw-map keys conflated `max`, missing controllers,
permission failures, and transient reads.

**Action.** Make `*_available` authoritative. For `not_present`, first verify that the
managed cgroup exists and only repair a controller when the enabled feature requires
that interface. For `permission_denied`, restore the documented root/delegation model.
For persistent `read_error`, inspect ResMan and kernel logs. Never interpret an empty
or absent value as `max`.

## Prometheus, alerts, and hooks

### Unavailable cgroup gauges no longer retain old values

**Visible change.** `resman_cgroup_cpu_quota_microseconds` is absent while `cpu.max`
is unlimited or unavailable. The period remains present for a valid `max PERIOD`
record. Quota, period, and memory-usage series are removed when their source becomes
unavailable, the user moves to another managed cgroup path, or enforcement releases
the cgroup. Previously published values and old path labels no longer remain visible.

**Cause.** Skipping a Prometheus `GaugeVec.Set` does not remove an existing series.
The exporter previously presented old finite quotas, memory values, and cgroup paths as
current observations, while an unreadable `memory.current` became a real zero.

**Action.** Treat absence as unavailable or unlimited according to the companion
period and runtime enforcement state; do not substitute zero. Queries that retain the
last sample must apply their own explicit staleness policy.

### Series identity changes once at upgrade

**Visible change.** Cgroup metrics, counters, and histograms gain constant `hostname`
and `server_role` labels. Pre-upgrade series end and new series begin; counters appear
to restart and `increase()` can under-report for one evaluation window across the
upgrade.

**Cause.** These labels were missing from exactly the series that shipped dashboards
filter by host and role.

**Action.** Expect the one-time discontinuity. Do not diagnose it as a counter reset in
the daemon. `cluster` and `environment` remain scrape-time labels.

### CPU-only metric and alert names are removed

**Visible change.** These Prometheus series are removed without aliases:

- `resman_limited_users_count_filtered`
- `resman_limited_users_cpu_usage_percent`
- `resman_limited_users_memory_usage_bytes`
- `resman_limited_users_count`
- `resman_limits_active`
- `resman_user_cpu_limited`
- `resman_limits_activated_total`
- `resman_limits_deactivated_total`

Queries must use the `resman_cpu_eligible_users_*`,
`resman_cpu_actively_limited_users_count`, `resman_cpu_limits_active`,
`resman_user_cpu_limit_active`, and `resman_cpu_limits_*_total` families as appropriate.
New `ram_eligible_users_*`, `io_eligible_users_*`,
`resman_actively_limited_users_count`, `resman_resource_limits_active`, and
`resman_any_limits_active` series expose RAM/I/O and union state.

Alert identifiers also change:

- `ResManLimitsNotActivating` -> `ResManCPULimitsNotActivating`
- `ResManFrequentLimitToggling` -> `ResManFrequentCPULimitToggling`
- `ResManLimitsActivated` -> `ResManCPULimitsActivated`
- `ResManLimitsDeactivated` -> `ResManCPULimitsDeactivated`

**Cause.** The old generic names described CPU-only eligibility or enforcement and
could not truthfully represent independent RAM and I/O state.

**Action.** Update dashboards, recording rules, alerts, Alertmanager routes, silences,
and automation before upgrading. MCP system-history decoders must likewise replace
the removed `limits_active` and `limited_users_count` fields with the five explicit
resource and count fields.

### Per-user telemetry follows the decision sample

**Visible change.** Observation-only refreshes no longer advance CPU baselines or EMA
used by decisions, and no longer overwrite/remove per-user Prometheus series between
control cycles. Existing per-user series now advance only at decision cadence. Alerts
can fire or clear at different times when `METRICS_REFRESH_INTERVAL` differs from the
control cadence.

**Cause.** Two sampling streams previously wrote one series and shared temporal state,
so monitoring cadence changed enforcement and whichever loop wrote last changed alert
inputs.

**Action.** No configuration change is required. Treat per-user series as
decision-facing telemetry; system-wide observation telemetry can remain fresher.

### Procfs coverage failures are visible and conservative

**Visible change.** Denied or malformed `/proc/PID/exe` and `/proc/PID/io` reads no
longer become trusted names or zero-valued decision inputs. Available I/O remains a
lower bound that may activate limits; incomplete coverage prevents normal-pressure
claims and release. `resman_procfs_unavailable_processes` and the shipped warning alert
report the affected observation kind.

**Cause.** Foreign-process procfs reads require the documented privilege and namespace
model. A zero fallback silently disabled enforcement and allowed process-controlled
`comm` names to spoof exclusions.

**Action.** Restore rootful host-PID/cgroup access and verify both `/proc/PID/exe` and
`/proc/PID/io`. Investigate persistent non-zero coverage before diagnosing limits that
remain active.

### Limit-hook payloads and completion change

**Visible change.** Webhook/script payload field `cpu_usage` becomes
`enforceable_cpu_usage_percent`, and `limited_users` becomes
`cpu_eligible_users_count`. Environment variables become
`RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT` and
`RESMAN_LIMIT_CPU_ELIGIBLE_USERS_COUNT`. Script stdout/stderr is discarded, and URL
failures expose only `scheme://host[:port]`. Hook deliveries are cancelled and drained
during shutdown and export bounded terminal outcomes.

**Cause.** The old names described generic runtime state while carrying CPU decision
inputs, and hook output or full URLs could leak secrets into logs.

**Action.** Update hook consumers before upgrade. Do not rely on script output in the
daemon log; send intended diagnostics to an independently protected sink.

### More failures produce degraded or non-zero outcomes

**Visible change.** Database writes, I/O remediation, pattern-policy cgroup writes,
process reconciliation, logger sinks, Prometheus shutdown, and hook completion no
longer disappear behind success logs. Cycles can finish as `degraded`; clean shutdown
can exit non-zero. Logger fallback diagnostics go directly to stderr/journal and do
not copy the failed record.

**Cause.** Side effects were previously logged locally or ignored while their owner
reported successful completion.

**Action.** Keep service stderr/journal observable and update monitoring to treat
degraded cycle outcomes and bounded `resman_errors_total` labels as actionable. For
`process_membership/origin_unavailable`, restart the affected process under its owning
service/session so ResMan captures a valid origin; alternatively stop ResMan cleanly,
restart the owner after recovery, then restart ResMan.

## Enforcement behavior

### CPU, RAM, and I/O eligibility and decisions are independent

**Visible change.** RAM and I/O no longer inherit CPU eligibility. Read bandwidth,
write bandwidth, read IOPS, and write IOPS can independently activate I/O enforcement;
disabling one dimension no longer disables the others. A host can therefore begin
limiting with unchanged configuration where 1.25.x silently ignored a configured
resource or dimension.

**Cause.** One overloaded CPU field and a write-bandwidth-only decision gate coupled
three policy domains and four I/O dimensions.

**Action.** Review each resource's include/exclude lists and all four I/O limits before
upgrade. No compatibility switch restores the ignored behavior.

### Process exclusion and active membership are enforced continuously

**Visible change.** Excluded processes remain in total-user observation but no longer
contribute to CPU, RAM, or I/O decision inputs. Exclusion uses the canonical
`/proc/PID/exe` basename; unreadable identity remains enforceable and is reported.
Every control cycle reconciles newly created or newly eligible processes into an
active user's cgroup and restores newly excluded processes only to an origin captured
for the same PID start time.

**Cause.** Accounting and placement previously used different identities and only
reconciled membership at activation, allowing post-activation sessions to bypass
enforcement or excluded work to trigger it.

**Action.** Review `PROCESS_EXCLUDE_LIST` against executable basenames, not `comm` or
`argv[0]`. When origin is unavailable, the process stays constrained fail-closed;
restart it under its owner or use the clean-shutdown recovery procedure described
above.

### CPU sampling follows the effective control cadence

**Visible change.** A missed tick plus jitter no longer resets valid host CPU usage to
zero. When PSI is configured but its watcher cannot start, staleness follows two
`POLLING_INTERVAL` periods; `PSI_FALLBACK_INTERVAL` applies only while PSI event-driven
mode is actually active. Hosts affected by false-zero baselines can begin enforcing
with unchanged configuration.

**Cause.** Baseline validity was derived first from an unrelated cache TTL and later
from configured PSI intent rather than observed runtime cadence.

**Action.** No configuration change is required. Re-evaluate thresholds on hosts that
previously reported repeated zero CPU samples, especially where PSI is configured but
unavailable.

### High system load is attributed before suppressing enforcement

**Visible change.** With `IGNORE_SYSTEM_LOAD=false`, a host whose CPU-eligible users
produce at least half of measured aggregate CPU activity can now activate limits after
`CPU_THRESHOLD_DURATION`, even while load average reports the host under load. The old
behavior suppressed every activation and reset the duration tracker, so an unchanged
configuration may begin enforcing after upgrade. External-majority load remains
protected; an unavailable host CPU sample delays activation conservatively.

**Cause.** Load average described host pressure but not its owner. The decision engine
previously asserted that high load came from other factors without comparing it with
the already-collected eligible-user CPU aggregate.

**Action.** No compatibility switch restores the circular guard. Review
`USER_INCLUDE_LIST`, `CPU_THRESHOLD`, and `CPU_THRESHOLD_DURATION` before upgrade.
Set `IGNORE_SYSTEM_LOAD=true` only when enforcement must proceed regardless of measured
external CPU activity.

### I/O rates use stable identities and real block operations

**Visible change.** `/proc/PID/io` byte rates are differenced by PID plus start time
before user aggregation, so process exit or PID reuse no longer zeros a user's entire
rate. Finite `IO_READ_IOPS` and `IO_WRITE_IOPS` decisions use `io.stat` `rios/wios`, not
`syscr/syscw`. Page-cache, pipe, socket, and character-device syscalls no longer count
as block IOPS, while direct block I/O does. Finite-IOPS-eligible processes enter an
unlimited per-user observation cgroup before enforcement.

**Cause.** A sum over changing process membership is not monotonic, and syscall counts
measure a different event from the device IOPS controlled by `io.max`.

**Action.** No configuration conversion is required, but expect different activation
and release for the same workload. Ensure the delegated hierarchy exposes `io.max` and
`io.stat`. The first observation sample and any placement/policy transition are
incomplete rather than zero. If any eligible user lacks a baseline, available users
can still prove activation but global release waits; continuous eligible-user churn can
therefore prolong active I/O limits.

### Byte-suffixed I/O bandwidth limits now reach the kernel

**Visible change.** `IO_READ_BPS` and `IO_WRITE_BPS` values using the documented
case-insensitive `K`, `M`, `G`, or `T` suffixes are converted to decimal byte counts
before writing `io.max`. An unchanged configuration such as `IO_READ_BPS=100M` that
previously failed every enforcement attempt can now apply its limit.

**Cause.** Configuration validation accepted byte suffixes, but the cgroup writer
forwarded the original string even though the kernel interface accepts only a decimal
byte count.

**Action.** No syntax conversion is required. Review suffixed bandwidth limits before
upgrading because they now enforce the value the configuration already requested.

### Minimum active time protects the newest enforcement

**Visible change.** Global release remains all-or-nothing, but `MIN_ACTIVE_TIME` starts
from the later of CPU activation and aggregate RAM/I/O activation. Staggered activation
can keep all limits active longer than in 1.25.x.

**Cause.** Selecting the older timestamp allowed a newly activated resource to bypass
the anti-flap hold.

**Action.** No configuration change is required. Account for the newer-epoch hold when
investigating delayed release.

## Startup, shutdown, and deployment

### Enabled features require real cgroup interfaces

**Visible change.** Startup creates a real child cgroup and requires `cpu.max`, plus
`memory.max` when `RAM_LIMIT_ENABLED=true` and `io.max` when
`IO_LIMIT_ENABLED=true`. Missing structural capability exits with status 78. `cpuset`
is optional. A hot reload cannot enable a feature whose interface was unavailable at
startup; the effective feature remains disabled and restart is required after host
repair.

**Cause.** Controller names alone did not prove that the kernel exposed the file
enforcement writes, while packaging made optional `cpuset` fatal.

**Action.** Repair kernel, delegation, and controller availability or disable the
unsupported feature before upgrade. Restart after changing host capability; reload is
not a capability discovery boundary.

### systemd distinguishes permanent and transient startup failure

**Visible change.** Invalid configuration, structurally missing required cgroup
capabilities, and invalid MCP TLS credentials exit with status 78 and are not retried.
Other failures retry after 10 seconds but stop after three starts in 60 seconds.
Present-controller setup I/O failures, including `cgroup.subtree_control` contention,
remain transient. The packaged unit now waits for an explicit readiness notification;
`systemctl start resman` fails directly when bootstrap is rejected instead of returning
success while the process is about to exit.

**Cause.** `Restart=always` with the former timing retried permanent rejection forever,
hiding the stable cause in an endless start loop.

**Action.** Treat a successful `systemctl start resman` as confirmation that
configuration, capability, and configured listener initialization completed. Repair
status-78 failures, then start the unit. After the transient start
limit, repair the cause and run `systemctl reset-failed resman` before starting it.

### Shutdown restoration and completion are stricter

**Visible change.** When a recorded delegated origin is an internal cgroup that cannot
accept processes, clean shutdown restores same-start-time processes into a ResMan-owned
recovery leaf with normal CPU quota. Every PID start time is revalidated before the
move. Incomplete cleanup propagates to a non-zero daemon exit. CPU-limit operations no
longer return while a detached worker can still move processes; a blocked kernel write
can make the call exceed `CGROUP_OPERATION_TIMEOUT`. Hook and Prometheus shutdown wait
for owned goroutines within their configured bounds.

**Cause.** Returning success while cleanup or asynchronous workers still mutated
membership left processes constrained and made service automation trust a false
completion.

**Action.** Treat non-zero stop as evidence that processes may remain constrained and
inspect the cleanup log before restart. Allow shutdown automation for the configured
hook and HTTP bounds, and diagnose a cgroup write that exceeds the nominal operation
timeout rather than assuming a worker was abandoned.

### The supported container is rootful Podman on Oracle Linux 9

**Visible change.** The shipped image changes from static Alpine running as `appuser`
to Oracle Linux 9 built with CGO and running as UID 0. The old `docker-build` and
`docker-run` make targets are removed. Rootless, unprivileged, private PID/cgroup
namespace deployments are unsupported.

**Cause.** ResMan must resolve host NSS identities, read foreign-process procfs data,
and write the host cgroup hierarchy. Image build success or container UID 0 alone does
not provide those capabilities.

**Action.** Use `make container-build` and the `sudo podman` invocation in
[`CONTAINER.md`](CONTAINER.md): host PID and cgroup namespaces, privileged access, host
networking, writable `/sys/fs/cgroup`, persistent state/log mounts, and host NSS files.
Mount `/etc/passwd`, `/etc/group`, and `/etc/nsswitch.conf` for local users; SSSD also
requires `/var/lib/sss/pipes`.

## After starting the upgraded service

Verify all of the following before treating the upgrade as complete:

- `systemctl is-active resman` succeeds and the journal has no startup rejection.
- `/etc/resman/resman.conf` and `/var/lib/resman` have the required ownership and modes.
- MCP HTTP and `/health` negotiate the expected TLS/mTLS contract, or stdio clients use
  revision 2026-07-28.
- Prometheus exposes the renamed CPU, RAM, I/O, and union series with `hostname` and
  `server_role` labels.
- Alertmanager routes and silences use the new CPU alert identifiers.
- The metrics database reports schema version 3 or persistence is deliberately
  disabled with its remedy understood.
- Enabled RAM and I/O features passed their real-interface probes.
- Procfs and block-I/O coverage metrics are zero or their conservative enforcement
  effect is understood.
- Limit-hook receivers accept the new payload names and shutdown leaves no in-flight
  delivery.
