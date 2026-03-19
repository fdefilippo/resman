# CPU Manager Go - Technical Specification

**Version:** 1.8.1  
**Last Updated:** March 2026  
**License:** GPLv3  
**Repository:** https://github.com/fdefilippo/cpu-manager-go

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

CPU Manager Go is an enterprise-grade dynamic CPU resource management tool for Linux systems using cgroups v2. It automatically monitors CPU usage and applies limits to non-system users when configurable thresholds are exceeded.

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
│                      CPU Manager Go                              │
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
cpu-manager-go/
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
│   ├── tools.go           # MCP tools (11 tools)
│   ├── resources.go       # MCP resources (6 URIs)
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
- `SIGINT/SIGTERM`: Graceful shutdown with 10-second timeout

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
    ScriptCgroupBase   string
    ConfigFile         string
    LogFile            string
    CreatedCgroupsFile string
    MetricsCacheFile   string
    
    // Timing
    PollingInterval   int  // seconds
    MinActiveTime     int  // seconds
    MetricsCacheTTL   int  // seconds
    
    // Thresholds
    CPUThreshold       int  // percentage
    CPUReleaseThreshold int // percentage
    
    // CPU Limits
    CPUQuotaNormal   string  // "max 100000"
    CPUQuotaLimited  string  // "50000 100000"
    
    // Prometheus
    EnablePrometheus        bool
    PrometheusMetricsBindHost string
    PrometheusMetricsBindPort int
    
    // Logging
    LogLevel   string
    LogMaxSize int
    UseSyslog  bool
    
    // System
    MinSystemCores int
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
- Monitor configuration file for changes using `fsnotify`
- Trigger configuration reload on file modification
- Debounce rapid changes (100ms delay)
- Handle file creation/deletion events

**Events Handled:**
- `fsnotify.Write`: File modified
- `fsnotify.Create`: File created
- `fsnotify.Remove`: File deleted (triggers reload to defaults)

### 3.3 Cgroup Manager (cgroup/manager.go)

**Responsibilities:**
- Create and manage cgroup v2 hierarchies
- Apply CPU limits using `cpu.max`
- Apply CPU weights using `cpu.weight`
- Move processes to cgroups
- Track created cgroups in file
- Clean up cgroups on shutdown

**Cgroup Hierarchy:**
```
/sys/fs/cgroup/
└── cpu_manager/              # Base cgroup (ScriptCgroupBase)
    ├── limited/              # Shared cgroup for limited users
    │   ├── user_1000/        # Per-user sub-cgroup
    │   ├── user_1001/
    │   └── ...
    ├── user_1000/            # Legacy individual cgroups
    └── ...
```

**Key Functions:**
- `NewManager(cfg)`: Creates cgroup manager
- `verifyCgroupSetup()`: Verifies cgroups v2 availability
- `CreateUserCgroup(uid)`: Creates cgroup for user
- `CreateSharedCgroup()`: Creates shared "limited" cgroup
- `ApplyCPULimit(uid, quota)`: Applies CPU limit to user
- `ApplyCPUWeight(uid, weight)`: Applies CPU weight to user
- `ApplySharedCPULimit(path, quota)`: Applies limit to shared cgroup
- `MoveProcessToCgroup(pid, uid)`: Moves process to user cgroup
- `MoveAllUserProcessesToSharedCgroup(uid, path)`: Moves all user processes
- `CleanupUserCgroup(uid)`: Removes user cgroup
- `CleanupAll()`: Removes all created cgroups
- `GetCgroupInfo(uid)`: Returns cgroup information

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
- Filter users by exclude list
- Filter processes by exclusion blacklist

**Key Functions:**
- `NewCollector(cfg)`: Creates metrics collector
- `GetTotalCores()`: Returns total CPU cores
- `GetTotalCPUUsage()`: Returns total CPU usage percentage
- `GetTotalUserCPUUsage()`: Returns non-system user CPU usage
- `GetUserCPUUsage(uid)`: Returns CPU usage for specific user
- `GetActiveUsers()`: Returns list of active non-system UIDs
- `GetMemoryUsage()`: Returns total memory usage in MB
- `GetAllUserMetrics()`: Returns detailed metrics for all users
- `IsSystemUnderLoad()`: Checks if system load is high
- `UpdateConfig(newConfig)`: Updates configuration dynamically
- `ClearCache()`: Clears metrics cache

**Metrics Caching:**
- Cache TTL: Configurable (default: 15 seconds)
- Cache key: Metric name + parameters
- Automatic cleanup of stale entries

**Process Exclusion:**
The following processes are automatically excluded from CPU limits:
- System: systemd, dbus-daemon, polkitd, udisks2d
- Network: NetworkManager, wpa_supplicant, sshd
- Services: cron, rsyslogd, auditd, firewalld
- Containers: dockerd, containerd, kubelet, lxcfs
- Web: nginx, apache2, httpd, php-fpm
- Database: mysqld, mariadbd, postgres, mongod, redis-server
- Mail: postfix, master
- Monitoring: zabbix_agentd, prometheus, node_exporter, grafana-server
- Virtualization: qemu-system, libvirtd, vmtoolsd, VBoxService
- Desktop: gdm, gnome-shell, lightdm, sddm
- Other: cupsd, avahi-daemon, bluetoothd, chronyd, smartd

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
      │  ├─ Check system load (if not ignored)
      │  └─ ACTIVATE_LIMITS
      └─ Else: MAINTAIN_CURRENT_STATE

4. Execute decision:
   ├─ ACTIVATE_LIMITS: Create shared cgroup, apply weights
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
- `GetStatus()`: Returns current status
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
- Expose CPU Manager functionality via Model Context Protocol
- Support stdio and HTTP transports
- Provide tools, resources, and prompts for AI assistants

**Tools (11 total):**

| Tool | Description | Write Op |
|------|-------------|----------|
| `get_system_status` | Get CPU/memory status with hostname | No |
| `get_user_metrics` | Get metrics for specific user(s) | No |
| `get_active_users` | List active non-system users | No |
| `get_limits_status` | Check if CPU limits are active | No |
| `get_cgroup_info` | Get cgroup details for user | No |
| `get_configuration` | Get current configuration | No |
| `get_control_history` | Get recent control cycles | No |
| `get_cpu_report` | Generate CPU usage report | No |
| `get_mem_report` | Generate memory usage report | No |
| `activate_limits` | Manually activate limits | Yes* |
| `deactivate_limits` | Manually deactivate limits | Yes* |

*Requires `MCP_ALLOW_WRITE_OPS=true`

**Resources (6 URIs):**
- `cpu-manager://system/status` - Real-time system status
- `cpu-manager://users/active` - Active users list
- `cpu-manager://limits/status` - Limits status
- `cpu-manager://config` - Configuration
- `cpu-manager://users/{uid}/metrics` - Per-user metrics
- `cpu-manager://cgroups/{uid}` - Cgroup information

**Prompts (3 pre-built):**
- `system-health` - Quick health check with assessment
- `user-analysis` - User resource analysis table
- `troubleshooting` - CPU limit diagnostic

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
- Thread-safe

**Log Format:**
```
[2026-03-12 21:00:00] [INFO] Message key1=value1 key2=value2
```

**Key Functions:**
- `InitLogger(level, filePath, maxSize, useSyslog)`: Initializes logger
- `GetLogger()`: Returns global logger instance
- `Debug/Info/Warn/Error(msg, keyvals...)`: Log methods
- `logInternal()`: Internal logging with rotation check
- `checkAndRotate()`: Rotates log file when max size reached

**Log Rotation:**
- Triggered when file exceeds `LogMaxSize`
- Creates backup: `cpu-manager.log.1`
- Maximum one rotation per second

### 3.8 Configuration Reloader (reloader/reloader.go)

**Responsibilities:**
- Apply configuration changes dynamically
- Update all components with new configuration
- Handle component-specific reload logic

**Reload Order:**
1. Logging (immediate, for tracing)
2. Prometheus exporter (may require restart)
3. State manager (update thresholds)
4. Cgroup manager (update paths)
5. Metrics collector (update cache TTL, exclude lists)

**Key Functions:**
- `NewReloader(state, cgroup, metrics, prometheus)`: Creates reloader
- `OnConfigChange(newConfig)`: Applies new configuration
- `SafeConfigUpdate(updateFunc)`: Thread-safe configuration update
- `handlePrometheusConfigChange(newConfig)`: Handles Prometheus changes

**Dynamic Updates:**
- `USER_EXCLUDE_LIST`: Applied immediately, cache cleared
- `CPU_THRESHOLD`: Applied on next control cycle
- `POLLING_INTERVAL`: Applied on next cycle
- `LOG_LEVEL`: Applied on next log message
- `PROMETHEUS_*`: May require restart for some changes

---

## 4. Configuration

### 4.1 Configuration File Format

**Location:** `/etc/cpu-manager.conf`

**Format:**
```ini
# Full line comment
KEY=value  # Inline comment
KEY="quoted value"
KEY='single quoted'
```

### 4.2 All Configuration Options

```bash
# ========================
# PATHS
# ========================
CGROUP_ROOT="/sys/fs/cgroup"
SCRIPT_CGROUP_BASE="cpu_manager"
CONFIG_FILE="/etc/cpu-manager.conf"
LOG_FILE="/var/log/cpu-manager.log"
CREATED_CGROUPS_FILE="/var/run/cpu-manager/cgroups.txt"
METRICS_CACHE_FILE="/var/run/cpu-manager/metrics.cache"

# ========================
# TIMING (seconds)
# ========================
POLLING_INTERVAL=30          # Control cycle interval
MIN_ACTIVE_TIME=60           # Minimum time limits stay active
METRICS_CACHE_TTL=15         # Metrics cache duration

# ========================
# CPU THRESHOLDS (percentage)
# ========================
CPU_THRESHOLD=75             # Activation threshold
CPU_RELEASE_THRESHOLD=40     # Deactivation threshold

# ========================
# CPU LIMITS (cpu.max format)
# ========================
CPU_QUOTA_NORMAL="max 100000"      # No limit
CPU_QUOTA_LIMITED="50000 100000"   # 0.5 cores

# ========================
# PROMETHEUS
# ========================
ENABLE_PROMETHEUS=false
PROMETHEUS_METRICS_BIND_HOST="0.0.0.0"
PROMETHEUS_METRICS_BIND_PORT=1974

# ========================
# USER EXCLUSION
# ========================
USER_EXCLUDE_LIST=           # Empty = no users excluded
# USER_EXCLUDE_LIST=francesco,www-data  # Exclude specific users

# ========================
# SYSTEM
# ========================
MIN_SYSTEM_CORES=1
SYSTEM_UID_MIN=1000
SYSTEM_UID_MAX=60000         # Auto-detected from /proc/sys/kernel/pid_max

# ========================
# USER FILTERS (v1.9.0+)
# ========================
# USER_INCLUDE_LIST: Regex patterns for users to INCLUDE in monitoring
# Empty = all users included
# Example: USER_INCLUDE_LIST=^www.*,^app-.*,mysql
USER_INCLUDE_LIST=

# USER_EXCLUDE_LIST: Regex patterns for users to EXCLUDE from limits
# Empty = no users excluded
# Example: USER_EXCLUDE_LIST=^test-.*,^dev-.*,francesco
USER_EXCLUDE_LIST=

# ========================
# LOGGING
# ========================
LOG_LEVEL="INFO"             # DEBUG, INFO, WARN, ERROR
LOG_MAX_SIZE=10485760        # 10MB
USE_SYSLOG=false

# ========================
# MCP SERVER
# ========================
MCP_ENABLED=false
MCP_TRANSPORT="stdio"        # stdio or http
MCP_HTTP_HOST="127.0.0.1"
MCP_HTTP_PORT=8080
MCP_LOG_LEVEL="INFO"
MCP_ALLOW_WRITE_OPS=false

# ========================
# SERVER ROLE
# ========================
SERVER_ROLE=                 # For identification in reports
```

### 4.3 Environment Variable Overrides

All configuration options can be overridden by environment variables:

```bash
LOG_LEVEL=DEBUG CPU_THRESHOLD=80 cpu-manager-go --config /etc/cpu-manager.conf
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
- `user_cpu_usage >= CPU_THRESHOLD` (default: 75%)
- `total_cores > MIN_SYSTEM_CORES`
- `system_load OK` OR `IGNORE_SYSTEM_LOAD=true`

**Deactivate Limits When:**
- `user_cpu_usage < CPU_RELEASE_THRESHOLD` (default: 40%)
- `time_since_activation >= MIN_ACTIVE_TIME`
- `system_load OK`

### 5.3 Limit Application

**Shared Cgroup Approach:**
1. Create `/sys/fs/cgroup/cpu_manager/limited/`
2. Apply total quota: `available_cores * 100000`
3. For each active user:
   - Create `user_{uid}/` sub-cgroup
   - Apply equal weight (default: 100)
   - Move all user processes to sub-cgroup

**Proportional Sharing:**
- Users share total quota proportionally
- Idle users don't consume their share
- Active users can use more than their fair share

---

## 6. Cgroup Management

### 6.1 Cgroup v2 Requirements

**Controllers Required:**
- `cpu` - CPU accounting
- `cpuset` - CPU affinity

**Enable Controllers:**
```bash
echo "+cpu" >> /sys/fs/cgroup/cgroup.subtree_control
echo "+cpuset" >> /sys/fs/cgroup/cgroup.subtree_control
```

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
| Total CPU% | `cpu.Percent()` | 15 seconds |
| User CPU% | Per-process aggregation | 15 seconds |
| Memory MB | `mem.VirtualMemory()` | 15 seconds |
| Load average | `/proc/loadavg` | 10 seconds |
| Active users | Process scan | 15 seconds |

### 7.2 Per-User Metrics

| Metric | Calculation |
|--------|-------------|
| CPU% | Sum of all process CPU% for UID |
| Memory bytes | Sum of VmRSS for all processes |
| Process count | Count of processes for UID |

### 7.3 CPU Usage Calculation

**Method:** Use gopsutil `p.CPUPercent()`

**How it works:**
1. First call: Records baseline, returns 0
2. Second call: Calculates delta, returns percentage
3. Subsequent calls: Continue delta calculation

**Caching:**
- Results cached for `MetricsCacheTTL` seconds
- Cache cleared on configuration reload
- Prevents excessive `/proc` reads

---

## 8. MCP Server

### 8.1 Protocol

**Based on:** Model Context Protocol Specification

**Transport:**
- JSON-RPC 2.0 over stdio or HTTP
- Streamable HTTP for request/response

**Session:**
- Stateless (each request independent)
- No session persistence required

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
- **Input:** `patterns` ([]string), `reload` (bool, default=true)
- **Output:** `success`, `previous_value`, `new_value`, `reload_triggered`
- **Implementation:**
  1. Validates regex patterns
  2. Creates timestamped backup
  3. Updates config file atomically
  4. Triggers config reload if requested
- **Security:** Requires `MCP_ALLOW_WRITE_OPS=true`

**Tool: `set_user_include_list`**
- **Description:** Set users to include in monitoring (regex patterns)
- **Input:** `patterns` ([]string), `reload` (bool, default=true)
- **Output:** `success`, `previous_value`, `new_value`, `reload_triggered`
- **Implementation:** Same as `set_user_exclude_list`
- **Security:** Requires `MCP_ALLOW_WRITE_OPS=true`

**Tool: `validate_user_filter_pattern`**
- **Description:** Validate regex pattern and show example matches
- **Input:** `pattern` (string), `type` (string: "include"|"exclude")
- **Output:** `valid`, `pattern`, `type`, `test_matches`, `match_count`
- **Implementation:** Tests pattern against example usernames

### 8.2.2 Configuration Save Mechanism

**Backup Process:**
```go
func (c *Config) SaveToFile(path string) error {
    // 1. Create timestamped backup
    timestamp := time.Now().Format("20060102_150405")
    backupPath := fmt.Sprintf("%s.backup_%s", path, timestamp)
    
    // 2. Read and write backup
    content, _ := os.ReadFile(path)
    os.WriteFile(backupPath, content, 0644)
    
    // 3. Update config lines
    lines := c.updateConfigLines(path)
    
    // 4. Atomic write (temp file + rename)
    tmpPath := path + ".tmp"
    os.WriteFile(tmpPath, content, 0644)
    os.Rename(tmpPath, path)
}
```

**Rollback on Error:**
```go
func (c *Config) SetUserExcludeList(patterns []string) ([]string, error) {
    // Validate patterns
    for _, pattern := range patterns {
        if _, err := regexp.Compile(pattern); err != nil {
            return nil, err
        }
    }
    
    // Save previous value
    previousValue := make([]string, len(c.UserExcludeList))
    copy(previousValue, c.UserExcludeList)
    
    // Attempt save
    if err := c.SaveToFile(path); err != nil {
        // Rollback on failure
        c.UserExcludeList = previousValue
        return nil, err
    }
    
    return previousValue, nil
}
```

### 8.3 HTTP Transport

**Endpoints:**
- `POST /mcp` - MCP JSON-RPC endpoint
- `GET /health` - Health check

**Middleware:**
- Request logging (method, path, duration)
- Response status tracking
- Authentication support (optional token)

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

**Rate Limit:** Maximum one rotation per second

---

## 11. Configuration Reloader

### 11.1 Reload Triggers

**Automatic:**
- File modification (fsnotify)
- File creation (fsnotify)
- File deletion (fsnotify)

**Manual:**
- SIGHUP signal

### 11.2 Reload Process

```
1. Config watcher detects change
2. Debounce (100ms delay)
3. Load new configuration
4. Validate configuration
5. Call reloader.OnConfigChange()
6. Update each component
7. Log success/failure
```

### 11.3 Component Updates

| Component | Update Method | Immediate? |
|-----------|--------------|------------|
| Logging | Global variable | Yes |
| Metrics | `UpdateConfig()` | Yes (cache cleared) |
| State | Internal check | Next cycle |
| Cgroup | Internal check | Next activation |
| Prometheus | Check changes | May need restart |

---

## 12. Prometheus Exporter

### 12.1 Metrics Exposed

**System Metrics:**
- `cpu_manager_cpu_total_usage_percent` (gauge)
- `cpu_manager_cpu_user_usage_percent` (gauge)
- `cpu_manager_memory_usage_megabytes` (gauge)
- `cpu_manager_system_load_average` (gauge)
- `cpu_manager_active_users_count` (gauge)
- `cpu_manager_limited_users_count` (gauge)
- `cpu_manager_limits_active` (gauge)

**Per-User Metrics:**
- `cpu_manager_user_cpu_usage_percent{uid, username}` (gauge)
- `cpu_manager_user_memory_usage_bytes{uid, username}` (gauge)
- `cpu_manager_user_process_count{uid, username}` (gauge)
- `cpu_manager_user_cpu_limited{uid, username}` (gauge)

**Counters:**
- `cpu_manager_limits_activated_total` (counter)
- `cpu_manager_limits_deactivated_total` (counter)

**Histograms:**
- `cpu_manager_control_cycle_duration_seconds` (histogram)

### 12.2 Exporter Lifecycle

**Start:**
1. Register metrics
2. Start HTTP server
3. Begin update loop (15s interval)

**Stop:**
1. Stop update loop
2. Shutdown HTTP server
3. Unregister metrics

**Update Loop:**
- Fetch metrics from state manager
- Update Prometheus gauges
- Sleep 15 seconds

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
    Timestamp         time.Time
    TotalCores        int
    TotalCPUUsage     float64
    TotalUserCPUUsage float64
    MemoryUsage       float64
    SystemUnderLoad   bool
    ActiveUsers       []int
    UserCPUUsage      map[int]float64
    UserMetrics       map[int]*UserMetrics
}

// Control History Entry
type ControlCycleEntry struct {
    Timestamp     time.Time
    Decision      string
    Reason        string
    TotalCPUUsage float64
    UserCPUUsage  float64
    ActiveUsers   int
    LimitsActive  bool
    DurationMs    int64
}
```

### 13.2 Interfaces

```go
// MetricsCollector interface
type MetricsCollector interface {
    GetTotalCores() int
    GetTotalCPUUsage() float64
    GetUserCPUUsage(uid int) float64
    GetTotalUserCPUUsage() float64
    GetActiveUsers() []int
    GetMemoryUsage() float64
    IsSystemUnderLoad() bool
    GetAllUserMetrics() map[int]*UserMetrics
}

// CgroupManager interface
type CgroupManager interface {
    CreateUserCgroup(uid int) error
    ApplyCPULimit(uid int, quota string) error
    ApplyCPUWeight(uid int, weight int) error
    RemoveCPULimit(uid int) error
    CleanupUserCgroup(uid int) error
    MoveProcessToCgroup(pid int, uid int) error
    MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) error
    CreateSharedCgroup() (string, error)
    ApplySharedCPULimit(sharedPath string, quota string) error
    CleanupAll() error
    GetCgroupInfo(uid int) (map[string]string, error)
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
    github.com/modelcontextprotocol/go-sdk v1.4.0
    github.com/prometheus/client_golang v1.23.2
    github.com/shirou/gopsutil/v3 v3.24.5
)
```

### 15.2 Build Commands

**Standard Build:**
```bash
cd /path/to/cpu-manager-go
export CGO_ENABLED=1
export CC=gcc
go build -v -ldflags="-s -w -X 'main.version=1.8.1'" -o cpu-manager-go .
```

**Build RPM:**
```bash
make rpm
# Creates: ~/rpmbuild/RPMS/*/cpu-manager-go-*.rpm
```

**Build Debian:**
```bash
make deb
# Creates: build/deb/cpu-manager-go_*.deb
```

### 15.3 Installation

**RPM:**
```bash
sudo rpm -ivh cpu-manager-go-*.rpm
sudo systemctl enable cpu-manager
```

**Debian:**
```bash
sudo dpkg -i cpu-manager-go_*.deb
sudo systemctl enable cpu-manager
```

**Manual:**
```bash
sudo cp cpu-manager-go /usr/bin/
sudo cp packaging/systemd/cpu-manager.service /usr/lib/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable cpu-manager
```

### 15.4 Configuration

**Default Location:** `/etc/cpu-manager.conf`

**Initial Setup:**
```bash
sudo cp config/cpu-manager.conf.example /etc/cpu-manager.conf
sudo vi /etc/cpu-manager.conf  # Edit as needed
sudo systemctl start cpu-manager
```

### 15.5 Monitoring

**Check Status:**
```bash
systemctl status cpu-manager
journalctl -u cpu-manager -f
tail -f /var/log/cpu-manager.log
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
| `/usr/bin/cpu-manager-go` | Binary |
| `/etc/cpu-manager.conf` | Configuration |
| `/var/log/cpu-manager.log` | Log file |
| `/var/run/cpu-manager/cgroups.txt` | Cgroup tracking |
| `/var/run/cpu-manager/metrics.cache` | Metrics cache |
| `/usr/lib/systemd/system/cpu-manager.service` | Systemd unit |

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
| 1.12.0 | Mar 2026 | **Blackout Timeframes**: `CPU_MANAGER_BLACKOUT` configuration. CPU Manager skips limit application during configured timeframes. Crontab-like format. System timezone support. |
| 1.11.0 | Mar 2026 | **MCP User Filter Management**: `set_user_exclude_list`, `set_user_include_list`, `get_user_filters`, `validate_user_filter_pattern`. Automatic config backup with timestamp. Atomic save with rollback. |
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
