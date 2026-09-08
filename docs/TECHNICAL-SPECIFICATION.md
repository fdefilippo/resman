# ResMan - Technical Specification

**Version:** 1.8.1
**Last Updated:** March 2026
**License:** GPLv3
**Repository:** https://github.com/fdefilippo/resman

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [Components](#3-components)
4. [Configuration](#4-configuration)
5. [Control Cycle](#5-control-cycle)
6. [Cgroup Management](#6-cgroup-management)
7. [Metrics Collection](#7-metrics-collection)
8. [MCP Server](#8-mcp-server)
9. [State Management](#9-state-management)
10. [Logging System](#10-logging-system)
11. [Configuration Reloader](#11-configuration-reloader)
12. [Prometheus Exporter](#12-prometheus-exporter)
13. [Data Structures](#13-data-structures)
14. [Error Handling](#14-error-handling)
15. [Build and Deployment](#15-build-and-deployment)

---

## 1. Overview

### 1.1 Purpose

ResMan is an enterprise-grade dynamic CPU resource management tool for Linux systems using cgroups v2. It automatically monitors CPU usage and applies limits to non-system users when configurable thresholds are exceeded.

### 1.2 Key Features

- **Dynamic CPU limiting** for non-system users (UID >= 1000)
- **Configurable thresholds** for activation (default: 75%) and release (default: 40%)
- **Proportional CPU sharing** using cgroup `cpu.weight`
- **User exclusion list** to exclude specific users from limits
- **Process exclusion list** to exclude system processes from limits
- **Prometheus metrics** export with per-user metrics
- **MCP server** for AI assistant integration
- **Automatic configuration reload** on file changes or SIGHUP
- **Graceful shutdown** with cleanup

### 1.3 System Requirements

- Linux kernel 4.5+ with cgroups v2
- Write access to `/sys/fs/cgroup`
- Root privileges or CAP_SYS_ADMIN capability
- GCC compiler (required for CGO)
- Go 1.21 or later

---

## 2. Architecture

### 2.1 High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         ResMan                                  │
│                                                                  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────┐  │
│  │   Cgroup     │  │   Metrics    │  │     State Manager    │  │
│  │   Manager    │  │   Collector  │  │   (Control Logic)    │  │
│  └──────────────┘  └──────────────┘  └──────────────────────┘  │
│         │                │                      │                │
│         └────────────────┼──────────────────────┘                │
│                          │                                       │
│              ┌───────────▼───────────┐                           │
│              │    MCP Server Layer   │                           │
│              │   (Package: mcp)      │                           │
│              └───────────┬───────────┘                           │
│                          │                                       │
│         ┌────────────────┼────────────────┐                      │
│         │                │                │                      │
│    Stdio Transport  HTTP Transport   Resources                  │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │              Configuration & Reloader                    │    │
│  │  - config.Config (configuration structure)              │    │
│  │  - config.Watcher (file monitoring)                     │    │
│  │  - reloader.Reloader (dynamic reload)                   │    │
│  └─────────────────────────────────────────────────────────┘    │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │              Logging & Prometheus                        │    │
│  │  - logging.Logger (structured logging)                  │    │
│  │  - metrics.PrometheusExporter (metrics export)          │    │
│  └─────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 Package Structure

```
resman/
├── main.go                 # Entry point, signal handling
├── config/
│   ├── config.go          # Configuration structure and parsing
│   ├── watcher.go         # File watcher for auto-reload
│   └── config_test.go     # Unit tests
├── cgroup/
│   └── manager.go         # Cgroup v2 management
├── metrics/
│   ├── collector.go       # System metrics collection
│   └── prometheus.go      # Prometheus exporter
├── state/
│   └── manager.go         # State management and control logic
├── reloader/
│   └── reloader.go        # Dynamic configuration reload
├── logging/
│   └── logger.go          # Structured logging
├── mcp/
│   ├── server.go          # MCP server implementation
│   ├── tools.go           # MCP tool definitions and handlers
│   ├── resources.go       # MCP resources and URI templates
│   ├── config.go          # MCP configuration
│   └── server_test.go     # Unit tests
└── docs/
    ├── TECHNICAL-SPECIFICATION.md  # This document
    ├── MCP-README.md               # MCP usage guide
    └── MCP-BLUEPRINT.md            # MCP architecture
```

---

## 3. Components

### 3.1 Main Entry Point (main.go)

**Responsibilities:**
- Parse command-line flags (`--config`, `--version`)
- Initialize logger with default values
- Load and validate configuration
- Initialize all components (cgroup, metrics, state, prometheus, mcp, reloader)
- Set up signal handling (SIGINT, SIGTERM, SIGHUP)
- Run main control loop
- Handle graceful shutdown

**Signal Handling:**
- `SIGHUP`: Force configuration reload
- `SIGINT/SIGTERM`: Graceful shutdown under the daemon-wide deadline configured by
  `DAEMON_SHUTDOWN_TIMEOUT` (60 seconds by default). The MCP server retains its
  independent `MCP_SHUTDOWN_TIMEOUT` step timeout.

**Initialization Order:**
1. Logger (default values)
2. Configuration (load from file)
3. Logger (reconfigure with config values)
4. Cgroup Manager
5. Metrics Collector
6. Prometheus Exporter (if enabled)
7. State Manager
8. Configuration Reloader/Watcher
9. MCP Server (if enabled)

### 3.2 Configuration Package (config/)

#### config.go

**Structure:**
```go
type Config struct {
    // Paths
    CgroupRoot         string
    CgroupBase   string
    ConfigFile         string
    LogFile            string
    CreatedCgroupsFile string

    // Timing
    PollingInterval   int  // seconds
    MinActiveTime     int  // seconds
    MetricsCacheTTL   int  // seconds

    // Thresholds
    CPUThreshold       int  // percentage
    CPUReleaseThreshold int // percentage

    // CPU Points
    CPUReservePoints    int
    CPUBestEffortPoints int
    CPUPointsFile       string

    // Prometheus
    EnablePrometheus        bool
    PrometheusMetricsBindHost string
    PrometheusMetricsBindPort int

    // Logging
    LogLevel   string
    LogMaxSize int
    UseSyslog  bool

    // System
    SystemUIDMin   int
    SystemUIDMax   int

    // User Exclusion
    UserExcludeList []string  // Users to EXCLUDE from limits

    // MCP Server
    MCPEnabled       bool
    MCPTransport     string  // "stdio" or "http"
    MCPHTTPPort      int
    MCPHTTPHost      string
    MCPLogLevel      string
    MCPAllowWriteOps bool

    // Server Role (for identification)
    ServerRole string
}
```

**Key Functions:**
- `DefaultConfig()`: Returns default configuration values
- `LoadAndValidate(path)`: Loads config from file, applies env vars, validates
- `loadFromFile()`: Parses key=value format with inline comment support
- `loadFromEnvironment()`: Overrides config with environment variables
- `setConfigField()`: Sets individual config fields by name
- `validateConfig()`: Validates configuration values
- `IsUserExcluded(username)`: Checks if user is in exclude list
- `IsProcessExcluded(processName)`: Checks if process is in exclusion blacklist

**Configuration Parsing:**
- Supports `#` comments (full line and inline)
- Supports environment variable overrides
- Supports quoted values (single and double quotes)
- Case-sensitive keys

#### watcher.go

**Responsibilities:**
- Monitor the parent directory with `fsnotify` and filter events for the
  configuration file, preserving monitoring across atomic file replacement
- Trigger configuration reload on file modification
- Debounce rapid changes (2 second delay)
- Serialize automatic, periodic, and SIGHUP reload paths
- Wait for the watcher loop and any active reload callback during shutdown

**Events Handled:**
- `fsnotify.Write`: File modified
- `fsnotify.Create`: File created
- `fsnotify.Remove`: File deleted
- `fsnotify.Rename`: File atomically replaced or renamed

### 3.3 Cgroup Manager (cgroup/manager.go)

Current metrics schema: 6.

**Responsibilities:**
- Create and manage cgroup v2 hierarchies
- Apply CPU limits using `cpu.max`
- Apply CPU weights using `cpu.weight`
- Move processes to cgroups
- Track created cgroups in file
- Clean up cgroups on shutdown

**Systemd ownership boundary:**

The `systemd_native` adapter uses authoritative D-Bus unit identities and
runtime-only SetUnitProperties calls, never raw cgroup writes or PID migration.
It verifies normalized properties and kernel values before acknowledging a plan.
The fallback `observation_only_systemd` applies when authority is unavailable;
`migration_enabled` is the separate non-systemd backend, not a systemd escape hatch.

**Native cgroup hierarchy:**
```text
user.slice                  finite CPU pool
├── user-0.slice             CPU_ROOT_POINTS=100, no leaf CPUQuota
├── user-1000.slice          exact mapped entitlement
│   ├── session-N.scope      unchanged PAM/logind membership
│   └── user@1000.service    unchanged child services and rootless descendants
└── user-1001.slice          share of aggregate best effort, including excluded users
```

The default reserve 100 + root 100 + best effort 100 leaves at most 700 mapped
points. All runnable siblings can borrow unused capacity. The reserve protects
system.slice and other workloads outside user.slice, not an unbounded root shell.
A UID split across parents has partial CPU coverage. Memory and I/O require complete
independent authority; authority_split and runtime_owned_descendant trigger release
of the affected resource's owned properties while preserving CPU scheduling.

Runtime-property ownership and crash recovery use the durable private journal in
[SYSTEMD-PROPERTY-LEASES.md](SYSTEMD-PROPERTY-LEASES.md). Startup reclaims exact
footprints; release compares before restoring and guarded unit-file cleanup never
erases operator changes. A root-only topology releases the finite pool. An unchanged
verified plan produces no property or journal rewrite. The legacy cgroup APIs below
remain specific to non-systemd migration and recovery of older acquisitions.

Where migration is supported, resman atomically persists PID, process start time, parent,
session ID, and original cgroup. Release restores the exact original cgroup
when it can legally accept processes. If that cgroup disappeared or is an
internal cgroup v2 node with controllers delegated to children, the process
enters the resman-owned recovery hierarchy. PID reuse is detected by
revalidating the start time immediately before every restore write. Descendants
inherit an unambiguous parent or session origin; otherwise they also use
recovery. Recovery leaves receive an internal unlimited `cpu.max`; no public
normal-quota setting is written into systemd-managed cgroups. Every attempted
restore returns one typed disposition: exact origin, recovery, disappeared, or
failed. Recovery is not successful release. Its occupants remain visibly stranded,
are never admitted again, and the recovery path is never recorded as a new origin.
An incomplete shutdown restoration is returned from the application and produces a
non-zero daemon exit status.

Live reconciliation deliberately differs from shutdown recovery. It does not
guess or create a replacement destination for an excluded process without a
same-start-time recorded or inherited origin: that process remains constrained.
The restore planner reports the typed per-process failure and still executes
valid peer restores. Enforcement errors are returned after history recording,
I/O remediation, workload pattern detection, and completion
logging have run. `resman_errors_total` distinguishes the persistent
`process_membership/origin_unavailable` outcome from transient
`process_membership/reconciliation_failure` outcomes.

At startup, the manager enables the controllers it may use and creates a
temporary child below the resman base cgroup. Capability is determined from the
interface files populated in that real child, not from controller names alone.
CPU Points always requires both `cpu.max` and `cpu.weight`; `RAM_LIMIT_ENABLED=true` additionally
requires `memory.max`, and `IO_LIMIT_ENABLED=true` requires `io.max`. A missing
required interface aborts startup with the feature, controller, and interface in
the error. Controllers for disabled RAM or I/O features may be absent. `cpuset`
is optional because quota and proportional CPU enforcement use the `cpu`
controller.

Capability discovery is a startup snapshot. If `memory.max` or `io.max` was
unusable during that probe, a reload that requests its feature reports the
capability error and publishes the effective configuration with the feature
still disabled. Repairing the host does not refresh the snapshot; resman must be
restarted to discover the repaired interface. This differs from a feature that
was merely disabled in the startup configuration: when its interface was found
usable, a reload can enable the feature without a restart. If the controller
cannot be enabled in an existing shared cgroup, the reload reports that failure
and leaves the feature disabled.

**Key Functions:**
- `NewManager(cfg)`: Creates cgroup manager
- `verifyCgroupSetup()`: Verifies cgroups v2 availability
- `CreateUserCgroup(uid)`: Creates cgroup for user
- `CreateSharedCgroup()`: Creates shared "limited" cgroup
- `EnsureCPUPointsHierarchy(quota, bestEffortWeight)`: Creates and verifies the finite pool and process-free domains
- `ApplyCPUPointsParentQuota(hierarchy, quota)`: Reconciles the typed finite parent quota
- `ApplyCPUPointsGuaranteedWeight(hierarchy, weight)`: Reconciles the active guaranteed entitlement
- `EnsureCPUPointsUserPlacement(uid, domain, weight)`: Fully configures and verifies an unlimited leaf before ingress
- `EnsureUnlimitedCPUQuota(uid)`: Keeps a RAM/I/O-only observation cgroup outside CPU allocation policy
- `MoveProcessToCgroup(pid, uid)`: Moves process to user cgroup
- `MoveAllUserProcesses(uid)`: Moves all user processes to a standalone cgroup
- `MoveAllUserProcessesToSharedCgroup(uid, path)`: Moves all user processes
- `CleanupUserCgroup(uid)`: Removes user cgroup
- `CleanupAll()`: Removes all created cgroups
- `GetCgroupInfo(uid)`: Returns cgroup information

CPU Points ingress owns process migration synchronously. `CGROUP_OPERATION_TIMEOUT`
cancels the scan between PID moves, but the call does not return until any in-flight
move has completed. A blocked kernel operation can therefore make the call exceed the
nominal timeout; after the returned error no background worker remains able to change
cgroup membership.

**CPU Limit Format:**
- `cpu.max` format: `"quota period"` (in microseconds)
- Example: `"50000 100000"` = 0.5 CPU cores
- Example: `"max 100000"` = no limit (1 core period)

**CPU Weight:**
- Range: 1 to 10000
- Default: 100 (equal share)
- Proportional distribution

### 3.4 Metrics Collector (metrics/collector.go)

**Responsibilities:**
- Collect system CPU usage
- Collect per-user CPU, memory, and process count
- Cache metrics with TTL
- Preserve observation for all non-system users while evaluating CPU, RAM, and I/O eligibility independently
- Separate observed process usage from enforceable usage with the shared process policy

**Key Functions:**
- `NewCollector(cfg)`: Creates metrics collector
- `GetTotalCores()`: Returns total CPU cores
- `GetDecisionHostCPUUsage()`: Returns the uncached typed host CPU sample for a control epoch
- `GetObservationHostCPUUsage()`: Returns the independently cached typed host CPU observation
- `GetAllUsersCPUUsage()`: Returns observed CPU usage for all non-system users
- `GetUserCPUUsage(uid)`: Returns CPU usage for specific user
- `GetAllUsers()`: Returns observed non-system UIDs
- `GetMemoryUsage()`: Returns total memory usage in MB
- `GetAllUserMetrics()`: Returns detailed metrics for all users
- `IsSystemUnderLoad()`: Checks if system load is high
- `UpdateConfig(newConfig)`: Updates configuration dynamically
- `ClearCache()`: Clears metrics cache

**Metrics Caching:**
- Cache TTL: Configurable, minimum 1 second (default: 15 seconds)
- Each entry retains the TTL of the read that populated it; the exact boundary is
  valid and the entry expires only after that boundary
- Cache key: Metric name + parameters
- Periodic cleanup honors each entry's TTL and does not impose a five-minute cap

**Process Exclusion:**
- `PROCESS_EXCLUDE_LIST` defines the process set enforceable by every resource.
- Excluded processes remain in observed totals but do not feed decision inputs.
- Matching uses only the `/proc/PID/exe` basename. `/proc/PID/comm` is display-only
  because a process can rewrite it. If executable identity is unavailable, the
  process remains in decision inputs and enforcement and an explicit error is emitted.
- While limits are active, every control cycle moves new enforceable processes into
  the current user cgroup and restores newly excluded processes to their captured
  origins.
- Origin restoration requires the same PID start time. A missing origin fails closed:
  the process remains constrained and the control cycle reports the error.
- Every ingress into a ResMan-owned cgroup requires the candidate PID namespace to
  equal the daemon's cached `/proc/self/ns/pid` device and inode. The identity is
  checked before origin persistence and again immediately before `cgroup.procs` is
  written. Mismatched or unreadable namespace entries are skipped without changing
  observation or decision aggregation. Restore and recovery deliberately bypass this
  guard because they remove an existing ResMan constraint.
- Keeping decision aggregation unchanged is conservative but asymmetric. A nested
  process that ResMan refuses to acquire still contributes to its host UID's CPU, RAM,
  and I/O totals. If that workload keeps a mixed UID above the release threshold,
  host-namespace processes already in ResMan cgroups remain constrained even though
  their restriction cannot reduce the nested workload's usage. The bounded
  `pid_namespace_boundary` warning identifies the UID and skip counts;
  `resman_cgroup_ingress_skipped_total` exposes the host-level occurrence trend.

**Procfs decision coverage:**
- Executable identity and I/O decision inputs carry explicit per-scan coverage.
- `ENOENT` after a process exits is discarded without an operator signal. Permission,
  parsing, and other persistent failures are aggregated by access type rather than
  logged once per PID.
- Available I/O rates are a lower bound. They may prove that activation is required,
  but incomplete coverage can never prove that pressure is below a threshold or that
  active limits are safe to release.
- `resman_procfs_unavailable_processes{access=~"executable_identity|io_decision"}`
  publishes the current number of affected observed processes with a bounded label set.

**User Exclusion:**
- Configured via `USER_EXCLUDE_LIST`
- Users in exclude list are never limited
- Empty list = no users excluded (all can be limited)

### 3.5 State Manager (state/manager.go)

**Responsibilities:**
- Execute control cycles
- Make decisions based on metrics
- Activate/deactivate CPU limits
- Track limit state
- Provide status information

**Control Cycle Logic:**

```
1. Collect system metrics
   ├─ Total CPU usage
   ├─ User CPU usage
   ├─ Memory usage
   ├─ Active users
   └─ System load

2. Update Prometheus metrics (if enabled)

3. Make decision:
   ├─ If limits active:
   │  ├─ Check minimum active time
   │  ├─ If CPU < release threshold AND system not under load
   │  │  └─ DEACTIVATE_LIMITS
   │  └─ Else: MAINTAIN_CURRENT_STATE
   │
   └─ If limits inactive:
      ├─ If CPU >= activation threshold
      │  ├─ Check minimum system cores
      │  ├─ If system load is high and not ignored, compare eligible CPU with
      │  │  aggregate host CPU; suppress only when external CPU is the majority
      │  └─ ACTIVATE_LIMITS
      └─ Else: MAINTAIN_CURRENT_STATE

4. Execute decision:
   ├─ ACTIVATE_LIMITS: Reconcile shared CPU and standalone RAM/IO cgroups
   ├─ DEACTIVATE_LIMITS: Remove limits, restore normal
   └─ MAINTAIN: No action

5. Record control cycle in history

6. Log results
```

**Key Functions:**
- `NewManager(cfg, metrics, cgroups, prometheus)`: Creates state manager
- `RunControlCycle(ctx)`: Executes one control cycle
- `collectSystemMetrics()`: Collects all system metrics
- `makeDecision(metrics)`: Decides whether to activate/deactivate limits
- `executeDecision(decision, metrics)`: Executes the decision
- `activateLimits(metrics)`: Activates CPU limits with proportional sharing
- `deactivateLimits()`: Deactivates CPU limits
- `GetStatus()`: Returns a typed runtime-enforcement snapshot, distinct from
  collector observations and policy eligibility
- `GetConfig()`: Returns current configuration
- `GetControlHistory(limit)`: Returns recent control cycle history
- `ForceActivateLimits()`: Force activates limits (admin override)
- `ForceDeactivateLimits()`: Force deactivates limits (admin override)
- `Cleanup()`: Cleans up on shutdown

**Control Cycle History:**
- Stores last 100 cycles
- Each entry: timestamp, decision, reason, metrics, duration
- Accessible via MCP tool `get_control_history`

### 3.6 MCP Server (mcp/)

**Responsibilities:**
- Expose ResMan functionality via Model Context Protocol
- Support stdio and HTTP transports
- Provide tools, resources, and prompts for AI assistants

**Tools:**

The complete production contract for tools, fixed resources, resource URI templates,
and prompts is maintained in the
[authoritative discovery inventories](MCP-README.md#discovery-inventories). The tool
inventory distinguishes registration conditions from invocation requirements, while
the remaining inventories distinguish fixed resources from URI templates. Cross-boundary
tests compare every inventory with the corresponding production discovery response.

The cgroup tool and resource share one JSON schema backed by a typed internal contract.
They expose `cpu.max`, `cpu.weight`, `memory.current`, `memory.max`, and `memory.high`
as the underscore-named fields `cpu_max`, `cpu_weight`, `memory_current`, `memory_max`,
and `memory_high`. Each value is paired with an explicit `*_available` boolean. An
unreadable interface is omitted with availability `false` and a bounded
`*_unavailable_reason`: `not_present`, `permission_denied`, or `read_error`. Available
values omit that reason. The reason carries neither the attempted interface path nor raw
error text; the existing `path` field still identifies the managed cgroup. Consumers
must not reinterpret an empty value as an unlimited limit. The operator action for each
reason is documented in
[MCP-README](MCP-README.md#cgroup-interface-availability).

All production MCP result and resource payloads use typed wire DTOs. Tool/resource pairs
for active users, resource-policy configuration, user metrics, system status, limits
status, and cgroup information share one projection per semantic contract. User-history
and system-history records intentionally remain distinct typed contracts. Dynamic maps
are limited to MCP input-schema metadata and decoded client arguments; they are never
serialized as production result payloads.

The shipped current-status inventory is explicit. Prometheus, `get_system_status`,
`get_limits_status`, `get_user_metrics`, their matching resources, `get_cpu_report`,
the system-health, user-analysis, and troubleshooting prompts, and limit-hook JSON/script
payloads expose the typed CPU Points distinction appropriate to their surface. The active-user
inventory remains identity-only, the memory report remains process-derived memory plus RAM-limit
state, configuration remains policy rather than observed state, cgroup information remains raw
kernel diagnostics, and the history tools retain the typed interval schema established by the
metrics database. Those unchanged surfaces do not project a CPU ceiling or a delivered guarantee.
Limit-hook RAM fields keep `memory.high` events, `memory.max` events, OOM attempts, and
OOM kills separate; zero max/OOM/kill deltas never reinterpret high throttling or stall as
absent enforcement.

**Transports:**
- **stdio**: For local MCP clients (Claude Desktop, etc.)
- **HTTP**: For remote clients (AnythingLLM, etc.)
  - Endpoint: `/mcp`
  - Health check: `/health`
  - Uses `mcp.NewStreamableHTTPHandler`

**Key Files:**
- `server.go`: MCP server, transport handling, logging middleware
- `tools.go`: Tool definitions and handlers
- `resources.go`: Resource definitions and handlers
- `config.go`: MCP-specific configuration

### 3.7 Logging System (logging/logger.go)

**Features:**
- Structured logging with key-value pairs
- Log levels: DEBUG, INFO, WARN, ERROR
- File logging with rotation
- Optional syslog support
- Bounded sink-health state with a non-recursive stderr fallback
- Thread-safe

**Log Format:**
```
[2026-03-12 21:00:00] [INFO] Message key1=value1 key2=value2
```

**Key Functions:**
- `InitLogger(level, filePath, maxSize, useSyslog)`: Initializes logger
- `GetLogger()`: Returns global logger instance
- `Debug/Info/Warn/Error(msg, keyvals...)`: Best-effort methods that publish health
- `DebugChecked/InfoChecked/WarnChecked/ErrorChecked(...)`: Return sink failures
- `Health()`: Detached health snapshot with cumulative/consecutive failure counts
- `logInternal()`: Internal logging with rotation check
- `checkAndRotate()`: Rotates log file when max size reached

**Log Rotation:**
- Triggered when file exceeds `LogMaxSize`
- Creates backup: `resman.log.1`
- Maximum one rotation per second
- Creates a new log at `0600`; an existing file retains only owner/group read-write
  access after all execute and other-user bits are removed
- Applies the same sanitized mode to the active file and rotated `.1` backup
- Preserves ownership across internal rotation, and rejects symbolic links or
  non-regular managed log destinations
- Write, stat, close, remove, rename and reopen failures are returned and recorded;
  stderr receives a sanitized diagnostic directly, without calling the failed logger
- A later successful primary-sink write clears the consecutive-failure state while
  preserving the cumulative count and last failure for diagnosis

Limit-hook failures are safe to record in this log. Script stdout/stderr is discarded,
script errors expose only a bounded execution reason, and webservice errors identify
only `scheme://host[:port]`, never URL userinfo, path, query values, or fragments.

### 3.8 Configuration Reloader (reloader/reloader.go)

**Responsibilities:**
- Apply configuration changes dynamically
- Treat the main configuration and strict CPU Points map as one confirmed
  candidate epoch while retaining independent content identities for both files
- Classify every public configuration key in the authoritative lifecycle table
  in `config/lifecycle.go`
- Reject restart-required changes explicitly while preserving their effective values
- Publish one configuration epoch across cgroup, state, metrics, and application consumers

**Reload Order:**
1. Read and validate the main configuration, apply lifecycle classification,
   then safely load and resolve the effective CPU Points map outside runtime locks
2. Confirm both source identities and content digests before kernel reconciliation
3. Close the configuration epoch barrier to new control cycles and wait for old
   cycles to drain
4. Compare all public keys with the lifecycle table and restore every
   restart-required key to its effective value
5. In native mode, reconcile active class changes, parent quota and the complete
   sibling-weight plan in place, with topology reconfirmation and read-back verification.
   Only the non-systemd migration backend rejects active cross-class placement changes.
6. Apply dynamic values to logging, cgroup, state, metrics, and the application
   runtime hook, including PSI watcher reconciliation
7. Confirm both files again and publish only after every consumer and kernel
   interface represent the same effective configuration and policy
8. Return component errors and an explicit restart-required error listing the
   rejected key names; configuration values and credentials are never included
9. Let the watcher classify the complete error tree and emit one terminal outcome
   record: pure restart-required rejection is `WARN`, while genuine and mixed
   failures are `ERROR`

A safe partial kernel mutation never advances the public configuration or policy
snapshot. It creates one typed internal reconciliation intent for the old
authoritative epoch. The control-cycle owner retries that intent without waiting
for another file event and reports a bounded degraded error until exact read-back
convergence clears it. Class-change preflight failures remain unprocessed so the
watcher retries them after the affected users have been released.

**Key Functions:**
- `NewReloader(state, cgroup, metrics, prometheus, hooks...)`: Creates reloader
- `OnConfigChange(newConfig)`: Applies new configuration
- `OnConfigCandidate(newConfig, confirm)`: Applies one confirmed main/map candidate
- `config.ApplyReloadLifecycle(effective, requested)`: Enforces the lifecycle table
- `state.Manager.BeginConfigUpdate()`: Starts the cross-component epoch barrier

**Dynamic Updates:**
- `USER_EXCLUDE_LIST`: Applied immediately, cache cleared
- `CPU_THRESHOLD`: Applied on next control cycle
- `CPU_RESERVE_POINTS`, `CPU_ROOT_POINTS`, `CPU_BEST_EFFORT_POINTS`, and map contents: Reconciled
  dynamically as one CPU Points epoch
- `CPU_POINTS_FILE`: Its active path is restart-required; a rejected new path is
  never opened by the reload candidate
- `POLLING_INTERVAL`: Applied on next cycle
- `USERNAME_CACHE_TTL`: Applied by the collector regardless of database enablement
- `METRICS_DB_RETENTION_DAYS`: Applied to cleanup and MCP database status
- `LOG_LEVEL`: Applied immediately
- `PSI_EVENT_DRIVEN`, PSI thresholds, and `PSI_WINDOW_US`: Rebuild the PSI watcher
- `PSI_FALLBACK_INTERVAL`, `METRICS_REFRESH_INTERVAL`: Rebuild loop tickers immediately
- `RAM_LIMIT_ENABLED`, `IO_LIMIT_ENABLED`: Applied only when startup capability
  discovery found the required interface usable; otherwise the requested feature
  remains disabled and the error requires a restart after repairing the host
- `ENABLE_PROMETHEUS`, Prometheus listener, TLS, and authentication: Rejected until restart
- Cgroup paths, created-cgroup state path, metrics database lifecycle/path/write interval,
  logging backend, and `SERVER_ROLE`: Rejected until restart
- MCP enablement, transport, listener, log level, authentication token, and
  write permissions: Rejected until restart

---

## 4. Configuration

### 4.1 Configuration File Format

**Location:** `/etc/resman/resman.conf`

The active file is selected with `--config` (default `/etc/resman/resman.conf`). The
removed `CONFIG_FILE` configuration key is rejected in files and environment
overrides; it never selects another file.

With the default path selected, startup rejects legacy `/etc/resman.conf`, the
`/etc/resman.conf.rpmsave` produced when RPM preserves a modified legacy file, and
the secret-bearing `/etc/resman.conf.backup` and `/etc/resman.conf.tmp`, plus every
potential configuration copy matching `/etc/resman.conf.backup_*`, even if the new
packaged file exists. The broad read-only guard is deliberately stricter than automatic
cleanup: it cannot prove whether an operator-named matching file is a credential-bearing
copy. The operator must choose the authoritative authored contents from the legacy or
RPM-saved file, install them as a regular file at the new path, remove the legacy source
files, move any needed operator-managed matching copies to a protected archive outside
the legacy path, securely remove generated or unneeded copies and fixed-name orphaned
artifacts, and restart. A custom `--config` path is authoritative and does not trigger
this default-layout guard.
When metrics persistence is enabled at
the default `/var/lib/resman/metrics.db`, `/etc/resman/metrics.db` is rejected before
component construction. A pre-version-6 database must be archived or deleted so schema
version 6 can be created; it is not moved or migrated. Version 6 persists one common
sample epoch across system and user rows, typed CPU Points configured/applied state,
nullable identity-safe cgroup deltas, PID-namespace and systemd-ownership refusal,
typed recovery lifecycle, RAM-charge coverage, and distinct memory high/max/OOM/kill
deltas. First baselines, counter resets and cgroup
recreation remain NULL rather than being reinterpreted as zero or wrapped deltas.

**Format:**
```ini
# Full line comment
KEY=value  # Inline comment
KEY="quoted value"
KEY='single quoted'
```

An inline `#` starts a comment only outside quotes and when preceded by
whitespace; quoted hashes and URL fragments are preserved.

### 4.2 Authoritative Configuration Reference

The complete public-key inventory is generated from the runtime `Config` type,
`DefaultConfig`, and the authoritative lifecycle table. See
[`CONFIGURATION.md`](CONFIGURATION.md) for every key, runtime default, hot-reload
lifecycle, and empty or disabled meaning. Use
[`config/resman.conf.example`](../config/resman.conf.example) as the copyable
configuration; `make verify-contracts` rejects drift between these sources.

The core default contracts are:

- Prometheus binds to `127.0.0.1`. Binding to `0.0.0.0` is a non-default remote
  exposure and requires TLS, authentication, and firewall restrictions.
- `SYSTEM_UID_MAX` follows `/proc/sys/kernel/pid_max` at startup, with `60000`
  only as the read-failure fallback. The example deliberately leaves it unset.
- An empty `USER_INCLUDE_LIST` makes nobody eligible for CPU limiting. Empty
  RAM and I/O include lists select every non-excluded user for those resources.

MCP over HTTP is HTTPS-only. TLS is enabled by default and cannot be disabled
while the HTTP transport is active. MCP and Prometheus use one shared TLS builder;
their default certificate and key paths are identical, while their configuration
keys remain independent. `MCP_TLS_CA_FILE` enables mTLS and makes a client
certificate mandatory in addition to the bearer token. Stdio remains the local
transport for deployments without certificate files.

### 4.3 Environment Variable Overrides

All configuration options can be overridden by environment variables:

```bash
LOG_LEVEL=DEBUG CPU_THRESHOLD=80 resman --config /etc/resman/resman.conf
```

---

## 5. Control Cycle

### 5.1 Cycle Execution

**Interval:** Configurable (default: 30 seconds)

**Steps:**
1. Collect metrics (CPU, memory, users, load)
2. Update Prometheus metrics
3. Make decision (activate/deactivate/maintain)
4. Execute decision
5. Record in history
6. Log results

### 5.2 Decision Logic

**Activate Limits When:**
- any independently eligible CPU, RAM, or I/O aggregate exceeds its threshold
- `system_load OK`, `IGNORE_SYSTEM_LOAD=true`, or CPU-eligible users account for at
  least 50% of measured aggregate host CPU activity

The attribution comparison converts normalized host CPU (0-100) into aggregate
per-core percentage by multiplying it by `total_cores`, which is the unit used by the
eligible process sum. If the host sample is unavailable, activation is delayed without
claiming that the load is external. Temporary external-load suppression preserves the
CPU threshold-duration tracker.

CPU activation refreshes the online CPU denominator and programs one finite parent
quota from `1000 - CPU_RESERVE_POINTS`. In native mode, mapped eligible slices receive
exactly scaled weights, root receives CPU_ROOT_POINTS, and unmapped/excluded slices
partition CPU_BEST_EFFORT_POINTS with integer weights differing by at most one.
ResMan does not write leaf CPUQuota, including on root. Configured operator limits
are not removed to manufacture delivery. No PID changes its unit.

**Deactivate Limits When:**
- `user_cpu_usage < CPU_RELEASE_THRESHOLD` (default: 40%)
- `time_since_most_recent_CPU_or_RAM/IO_activation >= MIN_ACTIVE_TIME`
- Every actively limited user's CPU EMA has remained below the release threshold for
  three `POLLING_INTERVAL` periods
- `system_load OK`

Deactivation is all-or-nothing, so the global hold is measured from the later of the
CPU enforcement activation epoch and the RAM/I/O enforcement activation epoch. A
newer activation cannot be released merely because the other resource family has
already been active longer than `MIN_ACTIVE_TIME`. A future per-resource release
model must replace this rule with explicit per-resource timestamps and tests.

The release stability guard uses elapsed wall-clock time. Extra control cycles
triggered by PSI events do not accelerate global deactivation.

Per-user decision CPU baselines and EMA advance only on control-cycle samples.
Metrics-only refreshes use independent per-process baselines, EMA state, and cache
entries. A refresh can update Prometheus without changing the next activation or
release input. The release stability guard consumes the same per-user snapshot as the
rest of its control decision; it does not perform a second collector read.

**Idle User Release:**
- Uses the per-user CPU EMA rather than one instantaneous sample
- Requires the user's own `MIN_ACTIVE_TIME` hold to expire
- Releases users immediately when they disappear or become ineligible
- Re-adds active eligible users without resetting the global activation time
- Reconciles tracked RAM/IO limits after dynamic enable or filter changes

RAM/IO-only users are placed in standalone per-user cgroups whose `cpu.max` is
explicitly `max 100000`. They never inherit the finite quota of `limited/`.
Changing eligibility at reload migrates the user between standalone resource
enforcement and the shared CPU hierarchy while preserving requested versus
successfully applied state for each resource.

### 5.3 Limit Application

**Systemd-native CPU Points hierarchy:**

The adapter programs a finite parent quota from the live online CPU set and
`floor(online_cpus * period * (1000 - reserve) / 1000)`. The minimum quota is
1000 microseconds. Active user slices remain in place, with exact mapped/root
weights and an exactly partitioned aggregate best-effort weight. Every active
sibling, including excluded users, belongs to the scheduling denominator.
Unused capacity is available to all runnable siblings.

[CPU Points observability](CPU-POINTS-OBSERVABILITY.md) defines schema 6, complete
denominator confirmation, resource coverage and the 60-second window measurement
procedure. No complete guarantee is published from a partial reconciliation.

The map is a root/daemon-owned regular mode-0600 file under trusted, non-writable,
non-symlink ancestors. Its exact marker and line grammar are validated before exact
NSS resolution. Main configuration and map content are one confirmed composite epoch.
Changing the class of an active UID is rejected before cgroup mutation and creates no
pending state. The UID must first be released; same-class guarantee changes and
inactive membership changes remain dynamic.

Dynamic process movement does not transfer cgroup v2 memory charges. First ingress
therefore yields complete process-derived UID memory but partial post-ingress cgroup
RAM coverage. A cross-parent CPU transition is refused while RAM enforcement is
active; release or disable RAM and retry after reconciliation. I/O-only transitions
remain permitted through the logical `io.stat` ledger. With `memory.high` below
`memory.max`, no swap, and unreclaimable pages, a live workload can stall indefinitely
at high with rising high events and zero max/OOM/kill events. An explicit
high-equals-max control is a distinct max/OOM experiment; requested allocation size
alone does not promise termination.

---

## 6. Cgroup Management

### 6.1 Cgroup v2 Requirements

ResMan discovers and enables controller capabilities at startup. CPU quota and
proportional-weight enforcement require `cpu.max`; `cpuset` is optional and its
absence does not disable CPU limiting. Enabling RAM or I/O limiting additionally
requires `memory.max` or `io.max`, respectively. The packaged systemd unit does
not write `cgroup.subtree_control` before startup, so the daemon remains the
single source of mandatory/optional capability diagnostics.

### 6.2 Cgroup Files Used

| File | Purpose |
|------|---------|
| `cpu.max` | CPU limit (quota period) |
| `cpu.weight` | CPU weight (1-10000) |
| `cpu.stat` | CPU statistics |
| `cgroup.procs` | Process list |
| `cgroup.subtree_control` | Controller enablement |

### 6.3 Process Movement

**Method:**
1. Read all PIDs from `/proc`
2. Filter by UID
3. Write PID to `cgroup.procs`
4. Verify movement

**Challenges:**
- Processes may exit during movement
- Some processes may resist movement (permissions)
- Kernel may reject movement (busy)

---

## 7. Metrics Collection

### 7.1 System Metrics

| Metric | Source | Cache TTL |
|--------|--------|-----------|
| Total cores | `cpu.Counts()` | 1 hour |
| Decision total CPU% | `/proc/stat` jiffy delta | Uncached, one independent sample per control epoch |
| Observation total CPU% | `/proc/stat` jiffy delta | `MetricsCacheTTL`, independent baseline |
| User CPU% | Per-process aggregation | 15 seconds |
| Memory MB | `mem.VirtualMemory()` | 15 seconds |
| Load average | `/proc/loadavg` | 10 seconds |
| Active users | Process scan | 15 seconds |

### 7.2 Per-User Metrics

| Metric | Calculation |
|--------|-------------|
| CPU% | Sum of all process CPU% for UID |
| Memory bytes | Sum of process PSS from `smaps_rollup`; RSS fallback when unavailable |
| Process count | Count of processes for UID |

### 7.3 CPU Usage Calculation

**Per-process method:** Use gopsutil process CPU times.

**Host-total method:** Calculate the active/total jiffy delta between consecutive
`/proc/stat` samples. Decision and observation own independent baselines and locks.
The decision stream bypasses the general cache and advances once per control epoch;
the observation stream may reuse its own sample for `MetricsCacheTTL`. The first
sample in either stream establishes a baseline and is unavailable rather than a
measured zero. A decision baseline remains valid for up to two effective control-loop intervals. The
application publishes the runtime cadence to the collector: `POLLING_INTERVAL` while
the PSI watcher is inactive, including when PSI is configured but unavailable, and
`PSI_FALLBACK_INTERVAL` only after the watcher starts successfully. The exact boundary
is valid; a longer gap, a clock regression, or regressed kernel counters resets the
baseline and reports a bounded unavailable reason. The next valid sample resumes delta
calculation immediately. A legitimate zero active-jiffy delta remains available.

**How it works:**
1. First call: Records baseline, reports unavailable
2. Second call: Calculates delta, returns an available percentage including zero
3. Subsequent calls: Continue delta calculation

**Caching:**
- Observation results are cached through the exact `MetricsCacheTTL` boundary and expire after it
- Periodic cleanup uses each entry's owning TTL, including values above five minutes
- Cache cleared on configuration reload
- Prevents excessive `/proc` reads
- Decision host-total CPU is uncached; cache expiry cannot define or reset its baseline
- Observation and decision per-user samples have separate cache entries, per-process
  baselines, and EMA state
- Active PID/start-time baselines are retained across every accepted sampling interval;
  completed scans prune identities that disappeared
- Only control-cycle samples advance temporal decision state

---

## 8. MCP Server

### 8.1 Protocol

**Protocol revision:** MCP `2026-07-28`, implemented with go-sdk v1.7.0 or newer

**Transport:**
- JSON-RPC 2.0 over stdio or HTTP
- Stateless Streamable HTTP with JSON responses

**Protocol state:**
- Every request is independent and self-describing
- HTTP authentication, protocol revision, method and method-specific name are
  validated per request
- `initialize`, `notifications/initialized`, session identifiers, resumability,
  batches, pre-2026 revisions and protocol fallbacks are rejected
- Request cancellation propagates to in-flight handlers
- Resource-manager application state remains shared and authoritative

### 8.2 Tool Implementation

**Registration:**
```go
mcp.AddTool(server, &mcp.Tool{
    Name:        "get_system_status",
    Description: "Get current CPU and memory status",
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{},
    },
}, handlerFunction)
```

**Handler Signature:**
```go
func handler(ctx context.Context, req *mcp.CallToolRequest, args Args) (*mcp.CallToolResult, Result, error)
```

### 8.2.1 User Filter Management Tools (v1.11.0+)

**Tool: `get_user_filters`**
- **Description:** Get current user include/exclude filter configurations
- **Input:** None
- **Output:** `user_include_list`, `user_exclude_list`, `config_file`
- **Implementation:** Reads from `state.Manager.GetConfig()`

**Tool: `set_user_exclude_list`**
- **Description:** Set users to exclude from CPU limits (regex patterns)
- **Input:** `patterns` ([]string); unknown fields and the removed `reload` field are rejected
- **Output:** `success`, `previous_value`, `new_value`, `persisted`, `applied`, `error`
- **Implementation:**
  1. Validates regex patterns
  2. Replaces the single rolling backup with the previous configuration
  3. Updates and syncs a detached config-file snapshot atomically without mutating live state
  4. Waits for the watcher to validate and apply the file, returning its success, failure, or timeout
- **Security:** Requires `MCP_ALLOW_WRITE_OPS=true`

**Tool: `set_user_include_list`**
- **Description:** Set CPU-eligibility patterns (regex patterns; an empty list disables CPU limiting)
- **Input:** `patterns` ([]string); unknown fields and the removed `reload` field are rejected
- **Output:** `success`, `previous_value`, `new_value`, `persisted`, `applied`, `error`
- **Implementation:** Same as `set_user_exclude_list`
- **Security:** Requires `MCP_ALLOW_WRITE_OPS=true`

**Tool: `validate_user_filter_pattern`**
- **Description:** Validate regex pattern and show example matches
- **Input:** `pattern` (string), `type` (string: "include"|"exclude")
- **Output:** `valid`, `pattern`, `type`, `test_matches`, `match_count`
- **Implementation:** Tests pattern against example usernames

### 8.2.2 Configuration Save Mechanism

**Backup and durability process:**

1. Acquire the configuration-file persistence coordinator, snapshot the managed
   values under the configuration lock, and release that lock before filesystem I/O.
   The coordinator is shared by configuration objects across reload epochs.
2. Inspect the current path with `lstat`. A symbolic link is rejected with the path
   and the required operator action; resman never replaces a managed link while
   leaving its target stale. For a regular file, preserve its permission mode and
   ownership. A new configuration defaults to mode `0600`.
3. Atomically replace the single rolling backup `<config>.backup` with the exact
   previous contents. The backup uses the same metadata as the source.
4. Remove only backups that match the historical generated name exactly,
   `<config>.backup_YYYYMMDD_HHMMSS`, plus the obsolete predictable
   `<config>.tmp` artifact. A file that merely starts with `.backup_` but has an
   operator suffix or a non-timestamp name is not removed. The persistence result
   carries removed basenames even when cleanup later fails.
5. Write the replacement through a randomly named same-directory temporary file,
   applying final metadata before secret-bearing content is written.
6. Sync the temporary file, rename it over the configuration, and sync the parent
   directory. A post-rename durability failure restores the previous contents before
   returning an error. If the rollback rename succeeds but its parent sync also fails,
   the original content is again readable and runtime publication remains unchanged;
   the joined error states that rollback durability is still unconfirmed. If rollback
   fails before its rename, the active file may contain the requested value while
   runtime remains unchanged. The persistence coordinator enters an explicit unusable
   state shared across reload epochs and rejects every later write: stop resman,
   restore `<config>.backup`, and restart before retrying. For a newly created file
   with no backup, failure to remove the non-durable replacement enters the same state;
   stop resman, remove the new file, and restart.

This keeps retention bounded to one previous version and prevents temporary or backup
files from becoming more readable than the active configuration.

An MCP write that performs this legacy cleanup emits one warning after persistence.
The warning reports the total count, at most three removed basenames, and an omitted
count. It never reports file contents, configuration values, or the full directory;
if cleanup fails after removing some artifacts, the same bounded warning reports the
completed removals before the tool returns the persistence error.

Ownership preservation is fail-closed. If the service account cannot apply the
source UID and GID to a replacement or backup, no secret-bearing content is written;
the error names the path and required owner and instructs the operator to grant the
service permission to `chown` or change the source ownership before retrying.

**Publication boundary:**
```go
previous, err := cfg.PersistUserExcludeList(patterns, path)
if err != nil {
    return err // the runtime snapshot is still unchanged
}
if err := watcher.Reload(ctx); err != nil {
    return err // persisted=true, applied is reported from observed runtime state
}
```

An individual user-filter transaction changes only the requested filter line and
preserves the other filter from the exact on-disk version it read. This prevents a
serialized transaction carrying an older runtime snapshot from losing an independent
filter update. No filesystem operation runs while the live `Config` mutex is held.

Concurrent MCP configuration writes are rejected explicitly, while the lower-level
persistence coordinator also serializes internal callers and adjacent reload epochs.
Automatic filesystem events use a content digest, rather than timestamp and size
alone, to avoid both missing same-size atomic replacements and reapplying a version
already acknowledged by the synchronous path.

### 8.3 HTTP Transport

**Endpoints:**
- `POST /mcp` - MCP JSON-RPC endpoint
- `GET /health` - Health check

**Middleware:**
- Request logging (method, path, duration)
- Response status tracking
- Mandatory Bearer token authentication for the HTTP MCP endpoint

---

## 9. State Management

### 9.1 State Structure

```go
type Manager struct {
    cfg              *config.Config
    limitsActive     bool
    limitsAppliedTime time.Time
    activeUsers      map[int]bool
    sharedCgroupPath string
    metricsCollector MetricsCollector
    cgroupManager    CgroupManager
    prometheusExporter PrometheusExporter
}
```

### 9.2 State Transitions

```
┌─────────────────┐
│  LIMITS OFF     │
│  (idle)         │
└────────┬────────┘
         │ CPU >= threshold
         │ cores OK
         │ load OK
         ▼
┌─────────────────┐
│  LIMITS ON      │
│  (active)       │
└────────┬────────┘
         │ CPU < release threshold
         │ time >= min_active
         │ load OK
         ▼
┌─────────────────┐
│  LIMITS OFF     │
│  (idle)         │
└─────────────────┘
```

### 9.3 Control History

**Stored Data:**
- Timestamp
- Decision (ACTIVATE/DEACTIVATE/MAINTAIN)
- Reason
- Metrics (CPU, users, load)
- Duration (milliseconds)

**Access:**
- MCP tool: `get_control_history`
- Maximum entries: 100
- Circular buffer (oldest removed)

### 9.4 Limit Hook Lifecycle

Script and HTTP hooks are independent jobs, asynchronous relative to the control
cycle, and owned by one fixed state-manager worker pool and one bounded queue.
Admission is non-blocking. A full queue terminates the rejected job as `saturated`
without retry, durable spooling, or any change to enforcement. Every accepted job
receives its own complete timeout and terminates as `success`, `failure`, `timeout`,
or `cancelled`; all five outcomes increment
`resman_limit_hook_executions_total{hook_type,outcome}` exactly once. Shutdown closes
admission, cancels the executor context, drains queued jobs through the cancelled
terminal path, and waits for the fixed workers before state cleanup continues.

Script configuration resolves an exact non-root NSS UID and GID at startup. The
absolute regular executable and each ancestor are checked for ownership, mode, and
symbolic links both during validation and immediately before execution. The process
receives no supplementary groups, starts in `/`, and inherits only the documented
fixed base and `RESMAN_LIMIT_*` event environment. Each execution owns a process
group; timeout and shutdown perform bounded TERM/KILL escalation and verify group
drain. This is ordinary-descendant cleanup, not hard containment of a hostile process
that deliberately creates a new session. HTTP uses a dedicated bounded client with
no retry and no inherited proxy. Script output and secret-bearing URL components
remain excluded from returned errors and logs. The complete operational contract is
in [`LIMIT-HOOKS.md`](LIMIT-HOOKS.md).
The transition payload also carries configured class and optional guarantee,
requested/applied lifecycle and weight, reconciliation state, complete/partial process
coverage, PID-namespace rejection counts, and post-ingress RAM cgroup coverage. It
never calls a partial host subset the complete UID workload and never promises that a
`memory.high` event will terminate a process.

---

## 10. Logging System

### 10.1 Log Levels

| Level | When Used |
|-------|-----------|
| DEBUG | Detailed debugging information |
| INFO | Normal operational messages |
| WARN | Warning conditions (non-fatal) |
| ERROR | Error conditions (may be fatal) |

### 10.2 Log Messages

**Key Events Logged:**
- Configuration load/reload
- Component initialization
- Control cycle start/complete
- Limit activate/deactivate
- Cgroup create/remove
- Process movement
- Errors and warnings

### 10.3 Log Rotation

**Trigger:** File size exceeds `LOG_MAX_SIZE`

**Process:**
1. Close current file
2. Rename to `.1`
3. Open new file
4. Continue logging

Every close, removal, rename and reopen result is checked. A failed step does not emit
the rotation-success record; the logger reports the failed operation through its
bounded health state and direct stderr fallback, then retries the primary file on a
later record where recovery is possible.

**Rate Limit:** Maximum one rotation per second

---

## 11. Configuration Reloader

### 11.1 Reload Triggers

**Automatic:**
- File modification (fsnotify)
- File creation (fsnotify)
- File deletion (fsnotify)
- Atomic file replacement (fsnotify)

**Manual:**
- SIGHUP signal

### 11.2 Reload Process

```
1. Config watcher detects a main-configuration or CPU Points map change
2. Debounce (2 second delay)
3. Serialize the reload with periodic and SIGHUP-triggered reloads
4. Capture independent content digests for both files
5. Load the main configuration, apply lifecycle, and load the effective map
6. Confirm both sources before and after kernel reconciliation
7. Call `reloader.OnConfigCandidate()`
8. Hold new control cycles outside the configuration epoch while every component updates
9. Preserve static effective values and report every rejected restart-required key
10. Record both processed digests after a partial kernel mutation, while leaving
    publication to the explicit runtime retry owner
11. Emit one terminal outcome record with deterministic rejected fields and whether
    the composite source epoch was recorded; lifecycle `INFO`/`DEBUG` records remain separate
```

### 11.3 Component Updates

| Component | Update Method | Immediate? |
|-----------|--------------|------------|
| Logging | Global variable | Yes |
| Metrics | `UpdateConfig()` | Yes (cache cleared) |
| State | Internal check | Next cycle |
| CPU Points topology | Candidate reconciliation plus read-back | Before acknowledgement |
| Cgroup | Internal check | Next activation |
| Application/PSI | Runtime hook | Yes (watcher rebuilt when needed) |
| Prometheus bind/lifecycle | Preserve active value | Restart required |
| Metrics database enable/path/write interval | Preserve active value | Restart required |
| Metrics database retention | Effective state configuration | Yes |

A control cycle or metrics refresh that began before reload completes on the old
epoch. A cycle that begins after reload was requested waits until cgroup, state,
collector, and application consumers all hold the new effective epoch. It can
never combine an old `PROCESS_EXCLUDE_LIST` collector scan with new cgroup or
decision policy.

---

## 12. Prometheus Exporter

### 12.1 Metrics Exposed

**System Metrics:**
- `resman_cpu_total_usage_percent` (gauge)
- `resman_control_cycle_host_cpu_sample_available` (gauge)
- `resman_control_cycle_host_cpu_sample_unavailable_total{reason}` (counter)
- `resman_observation_host_cpu_sample_available` (gauge)
- `resman_observation_host_cpu_sample_unavailable_total{reason}` (counter)
- `resman_all_users_cpu_usage_percent` (gauge)
- `resman_memory_usage_megabytes` (gauge)
- `resman_system_load_average` (gauge)
- `resman_all_users_count` (gauge)
- `resman_cpu_eligible_users_cpu_usage_percent` (gauge)
- `resman_cpu_eligible_users_memory_usage_bytes` (gauge)
- `resman_cpu_eligible_users_count` (gauge)
- `resman_ram_eligible_users_memory_usage_bytes` (gauge)
- `resman_ram_eligible_users_count` (gauge)
- `resman_io_eligible_users_count` (gauge)
- `resman_io_eligible_users_read_bytes_per_second` (gauge)
- `resman_io_eligible_users_write_bytes_per_second` (gauge)
- `resman_io_eligible_users_read_block_operations_per_second` (gauge)
- `resman_io_eligible_users_write_block_operations_per_second` (gauge)
- `resman_cpu_actively_limited_users_count` (gauge)
- `resman_actively_limited_users_count` (gauge)
- `resman_cpu_limits_active` (gauge)
- `resman_resource_limits_active` (gauge)
- `resman_any_limits_active` (gauge)
- `resman_enforcement_mode{mode}` (one bounded active mode)
- `resman_recovery_stranded_processes` (current live recovery occupants)

**Per-User Metrics:**
- `resman_user_cpu_usage_percent{uid, username}` (gauge)
- `resman_user_cpu_usage_average_percent{uid, username}` (gauge)
- `resman_user_cpu_usage_ema_percent{uid, username}` (gauge)
- `resman_user_memory_usage_bytes{uid, username}` (gauge)
- `resman_user_process_count{uid, username}` (gauge)
- `resman_user_cpu_limit_active{uid, username}` (gauge)

**CPU Points operational metrics:**
- configured reserve, nominal parent pool, aggregate best-effort entitlement, and
  applied guarantee total;
- verified live online-CPU denominator and programmed parent quota/period;
- synchronized parent and user-slice CPU usage
  deltas plus the common interval duration;
- parent period, throttled-period, and throttled-time deltas;
- optional mapped-user guarantee, requested enforcement, bounded configured/applied
  class, raw applied weight, lifecycle, reconciliation state, and complete/partial
  process coverage;
- cgroup `memory.current` explicitly labelled as post-ingress cgroup accounting,
  bounded RAM coverage, and distinct high/max/OOM/OOM-kill event deltas.

Effective parent CPU usage is the allocation denominator. The nominal pool and raw
`cpu.max` are capacity configuration, not observed delivery; raw `cpu.weight` is a
kernel scheduling value, not a second spelling of CPU Points. CFS can under-deliver
the nominal parent quota under saturation. All runnable sibling slices may borrow unused capacity. ResMan does not derive runnable points in userspace.

Every per-user series is published exclusively from the control-cycle decision
sample. Its CPU delta and smoothing window therefore match the sample used by
enforcement. Observation-only refreshes update system-wide gauges but do not write or
remove per-user series. The shipped per-user alert rules consequently evaluate one
defined sampling stream even when PSI event-driven refreshes run at another cadence.
CPU Points parent/slice deltas and memory event deltas use that same stream;
recreated cgroups, daemon restart, counter decrease, or an unavailable read removes
the interval series instead of creating a wrapped delta.

**Counters:**
- `resman_cpu_limits_activated_total` (confirmed inactive-to-active transitions)
- `resman_cpu_limits_deactivated_total` (confirmed active-to-inactive transitions)
- `resman_errors_total{component, error_type}` (operational errors with bounded labels)
- `resman_limit_hook_executions_total{hook_type, outcome}` (terminal script and HTTP
  hook outcomes using bounded labels)
- `resman_limit_hook_in_flight` (current deliveries owned by workers)
- `resman_limit_hook_queue_depth` (current pending deliveries)
- `resman_limit_hook_queue_capacity` (configured pending-delivery bound)
- `resman_cgroup_ingress_skipped_total{reason}` (processes not moved into ResMan-owned
  cgroups; reason is `pid_namespace_mismatch`, `pid_namespace_unavailable`,
  `systemd_ownership_preserved`, or `recovery_process_stranded`)
- `resman_process_restore_total{disposition}` (exact-origin, recovery,
  disappearance, and failure outcomes with bounded labels)
- `resman_procfs_unavailable_processes{access}` (current missing executable-identity
  or I/O-decision procfs inputs)

**Histograms:**
- `resman_control_cycle_duration_seconds` (complete cycles, including failed and suspended cycles)
- `resman_metrics_collection_duration_seconds` (control-cycle and metrics-only collection)

### 12.2 Exporter Lifecycle

**Start:**
1. Register metrics
2. Start HTTP server

**Stop:**
1. Request graceful HTTP shutdown with a bounded context
2. Force-close the listener if graceful shutdown fails
3. Wait for the serve goroutine to terminate
4. Return the combined shutdown and serve error to the application

**Update ownership:**
- Control cycles publish system-wide and per-user metrics.
- Observation-only refreshes publish system-wide metrics only.
- The application schedules both paths from the configured polling, PSI fallback,
  and metrics-refresh intervals; the exporter has no independent fixed update loop.

---

## 13. Data Structures

### 13.1 Key Structures

```go
// Configuration
type Config struct {
    // ... fields as defined in section 3.2
}

// User Metrics
type UserMetrics struct {
    UID          int
    Username     string
    CPUUsage     float64
    MemoryUsage  uint64
    ProcessCount int
}

// System Metrics (control cycle)
type SystemMetrics struct {
    Timestamp                 time.Time
    TotalCores                int
    TotalCPUUsage             float64
    AllUsersCPUUsage          float64
    AllUsersMemoryUsage       uint64
    AllUsersCount             int
    CPUEligibleCPUUsage       float64
    CPUEligibleMemoryUsage    uint64
    CPUEligibleUsersCount     int
    RAMEligibleUsageBytes     uint64
    RAMEligibleUsersCount     int
    IOEligibleUsersCount      int
    IOEligibleReadBPS         float64
    IOEligibleWriteBPS        float64
    IOEligibleReadBlockIOPS   float64
    IOEligibleWriteBlockIOPS  float64
    MemoryUsage               float64
    SystemUnderLoad           bool
    UserMetrics               map[int]*UserMetrics
}

// Control History Entry
type ControlCycleEntry struct {
    Timestamp                    time.Time
    Decision                     string
    Reason                       string
    TotalCPUUsage                float64
    CPUEligibleCPUUsage          float64
    ObservedUsersCount           int
    CPUActivelyLimitedUsersCount int
    CPULimitsActive              bool
    DurationMs                   int64
}
```

### 13.2 Interfaces

```go
// MetricsCollector interface
type MetricsCollector interface {
    GetTotalCores() int
    GetDecisionHostCPUUsage() HostCPUUsageSample
    GetObservationHostCPUUsage() HostCPUUsageSample
    GetUserCPUUsage(uid int) float64
    GetAllUsers() []int
    GetAllUsersCPUUsage() float64
    GetAllUsersMemoryUsage() uint64
    GetMemoryUsage() float64
    GetTotalMemoryMB() float64
    GetCachedMemoryMB() float64
    IsSystemUnderLoad() bool
    GetAllUserMetrics() map[int]*UserMetrics
    GetAllUserMetricsForDecision() map[int]*UserMetrics
    WriteMetricsToDatabase(map[int]*UserMetrics, SystemPersistenceMetrics) error
}

// PrometheusExporter accepts typed snapshots; positional scalar projections are forbidden.
type PrometheusExporter interface {
    UpdateSystemSnapshot(SystemExporterMetrics)
    UpdateUserSnapshot(UserExporterMetrics)
}

// CgroupManager interface
type CgroupManager interface {
    CreateUserCgroup(uid int) error
    EnsureUnlimitedCPUQuota(uid int) error
    EnsureCPUPointsHierarchy(ParentQuota, KernelCPUWeight) (CPUPointsHierarchy, error)
    ApplyCPUPointsParentQuota(CPUPointsHierarchy, ParentQuota) error
    ApplyCPUPointsGuaranteedWeight(CPUPointsHierarchy, KernelCPUWeight) error
    EnsureCPUPointsUserPlacement(uid int, domain string, weight KernelCPUWeight) (string, ProcessMoveResult, error)
    ReleaseCPUPointsUser(uid int, domain string) error
    CleanupUserCgroup(uid int) error
    MoveProcessToCgroup(pid int, uid int) error
    MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) error
    CleanupAll() error
    GetCgroupInfo(uid int) (cgroup.CgroupInfo, error)
}
```

---

## 14. Error Handling

### 14.1 Error Categories

**Configuration Errors:**
- File not found (use defaults)
- Parse error (fail fast)
- Validation error (fail fast)

**Runtime Errors:**
- Cgroup operation failed (log, continue)
- Process movement failed (log, retry next cycle)
- Metrics collection failed (use fallback, log)
- Prometheus export failed (log, continue)

**Fatal Errors:**
- Cannot create cgroup manager (exit)
- Cannot create state manager (exit)
- Cannot bind Prometheus port (disable Prometheus)

### 14.2 Error Recovery

**Automatic Recovery:**
- Failed process movement: Retry next cycle
- Temporary cgroup error: Retry on next activation
- Metrics cache miss: Recalculate

**Manual Recovery:**
- Configuration error: Fix config, send SIGHUP
- Cgroup corruption: Restart service (cleanup on start)

---

## 15. Build and Deployment

### 15.1 Build Requirements

**Software:**
- Go 1.21 or later
- GCC (for CGO)
- Make (optional, for Makefile)

**Dependencies:**
```go
require (
    github.com/fsnotify/fsnotify v1.9.0
    github.com/modelcontextprotocol/go-sdk v1.7.0
    github.com/prometheus/client_golang v1.23.2
    github.com/shirou/gopsutil/v3 v3.24.5
)
```

### 15.2 Build Commands

**Standard Build:**
```bash
cd /path/to/resman
export CGO_ENABLED=1
export CC=gcc
go build -v -ldflags="-s -w -X 'main.version=1.34.0-1'" -o resman .
```

**Build RPM:**
```bash
make rpm
# Creates: ~/rpmbuild/RPMS/*/resman-*.rpm
```

**Build Debian:**
```bash
make deb
# Creates: build/deb/resman_<version>-<release>_<architecture>.deb
```

The Debian build is native because CGO is required for NSS, LDAP, NIS and SSSD
username resolution. Run it on the target architecture (`amd64` or `arm64`) with
`dpkg-dev`, a C compiler and Go 1.25.7 or newer installed.
`dpkg-shlibdeps` derives the minimum runtime library versions from the resulting
binary. Build release artifacts on the oldest supported distribution baseline
when the same package must run across multiple Debian and Ubuntu releases.

### 15.3 Installation

**RPM:**
```bash
sudo rpm -ivh resman-*.rpm
sudo systemctl enable resman
```

**Debian:**
```bash
sudo apt install ./resman_*.deb
sudo systemctl enable --now resman
```

The package preserves `/etc/resman/resman.conf` as a conffile and does not enable or
start the service during a fresh installation. An upgrade restarts the service
only when it is already active.

**Manual:**
```bash
sudo cp resman /usr/bin/
sudo cp packaging/systemd/resman.service /usr/lib/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable resman
```

### 15.4 Configuration

**Default Location:** `/etc/resman/resman.conf`

**Initial Setup:**
```bash
sudo install -d -m 0700 /etc/resman /var/lib/resman
sudo install -m 0600 config/resman.conf.example /etc/resman/resman.conf
sudo vi /etc/resman/resman.conf  # Edit as needed
sudo systemctl start resman
```

### 15.5 Monitoring

**Check Status:**
```bash
systemctl status resman
journalctl -u resman -f
tail -f /var/log/resman.log
```

**Prometheus Metrics:**
```bash
curl http://localhost:1974/metrics
```

**MCP Tools:**
```bash
# Via MCP client (AnythingLLM, Claude Desktop, etc.)
# Tools: get_system_status, get_cpu_report, etc.
```

---

## Appendix A: File Locations

| File | Purpose |
|------|---------|
| `/usr/bin/resman` | Binary |
| `/etc/resman/resman.conf` | Configuration |
| `/etc/resman/resman.conf.backup` | Rolling configuration backup |
| `/etc/resman/tls/` | Operator-supplied TLS material |
| `/var/lib/resman/metrics.db` | Mutable metrics database; mode `0600` below a process-owned mode `0700` directory, with the same regular-file contract for existing WAL/SHM sidecars |
| `/var/log/resman.log` | Restrictive log file (new/package default mode `0600`) |
| `/run/resman-cgroups.txt` | Boot-scoped cgroup tracking |
| `/usr/lib/systemd/system/resman.service` | Systemd unit |

---

## Appendix B: Signal Handling

| Signal | Action |
|--------|--------|
| `SIGHUP` | Force configuration reload |
| `SIGINT` | Graceful shutdown |
| `SIGTERM` | Graceful shutdown |
| `SIGKILL` | Immediate termination (no cleanup) |

---

## Appendix C: Version History

| Version | Date | Key Changes |
|---------|------|-------------|
| 1.12.0 | Mar 2026 | **Blackout Timeframes**: `CPU_MANAGER_BLACKOUT` configuration. The daemon skips limit application during configured timeframes. Crontab-like format. System timezone support. |
| 1.11.0 | Mar 2026 | **MCP User Filter Management**: `set_user_exclude_list`, `set_user_include_list`, `get_user_filters`, `validate_user_filter_pattern`. Secure bounded rolling backup. Atomic durable save. |
| 1.10.1 | Mar 2026 | Config watcher periodic check (30s) for reliable reload |
| 1.10.0 | Mar 2026 | **USER_EXCLUDE_LIST regex support**: Pattern matching for user exclusion |
| 1.9.0 | Mar 2026 | **USER_INCLUDE_LIST**: Regex-based user inclusion filtering |
| 1.8.1 | Mar 2026 | Config reload for USER_EXCLUDE_LIST |
| 1.8.0 | Mar 2026 | USER_EXCLUDE_LIST (exclude users from limits) |
| 1.7.0 | Mar 2026 | Process exclusion blacklist |
| 1.6.0 | Mar 2026 | User whitelist fix, CGO requirement |
| 1.5.0 | Mar 2026 | Prometheus port change, inline comments |
| 1.4.0 | Mar 2026 | SERVER_ROLE, hostname in outputs |
| 1.3.0 | Mar 2026 | CPU/memory reports, hostname support |
| 1.2.0 | Mar 2026 | MCP server initial release |
| 1.1.0 | Feb 2026 | TLS, auth, per-user metrics |
| 1.0.0 | Jan 2026 | Initial stable release |

---

**End of Technical Specification**
