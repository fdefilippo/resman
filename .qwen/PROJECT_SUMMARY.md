# ResMan - Project Summary

## Overall Goal
Implement a comprehensive MCP-enabled CPU and RAM resource management tool using Linux cgroups v2 with dynamic limiting, multi-cluster monitoring support, and AI assistant integration.

**Project Name:** ResMan (Resource Manager) - formerly cpu-manager-go  
**Current Version:** 1.18.2  
**Last Updated:** 2026-03-24

---

## Key Knowledge

### Architecture & Technology
- **Language**: Go 1.21+ with CGO support (required for LDAP/NIS username resolution)
- **Core Technology**: Linux cgroups v2 with CPU, cpuset, and memory controllers
- **MCP SDK**: `github.com/modelcontextprotocol/go-sdk v1.4.0`
- **Metrics**: Prometheus with custom metrics (default port 1974)
- **Database**: SQLite for historical metrics (optional)
- **Build Command**: `CGO_ENABLED=1 go build -v -ldflags="-s -w" -o resman .`

### Configuration System
- **Location**: `/etc/resman.conf`
- **Format**: key=value with inline comment support (#)
- **Auto-reload**: File watcher with 30-second periodic check
- **Key Variables**:

#### CPU Management
- `CPU_THRESHOLD=75` - Activation threshold (%)
- `CPU_RELEASE_THRESHOLD=40` - Deactivation threshold (%)
- `CPU_THRESHOLD_DURATION=90` - Time window before activating limits (seconds, 0=immediate)
- `CPU_QUOTA_NORMAL="max 100000"` - Normal CPU quota
- `CPU_QUOTA_LIMITED="50000 100000"` - Limited CPU quota (0.5 core)

#### RAM Management (v1.16.4+)
- `RAM_LIMIT_ENABLED=false` - Enable RAM limiting
- `RAM_THRESHOLD=75` - RAM activation threshold (%)
- `RAM_RELEASE_THRESHOLD=40` - RAM deactivation threshold (%)
- `RAM_QUOTA_LIMITED=2G` - Total RAM quota for limited users
- `RAM_QUOTA_PER_USER=512M` - Per-user RAM quota
- `DISABLE_SWAP=false` - Disable swap in cgroups

#### User Filtering
- `SYSTEM_UID_MIN=1000` - Minimum UID to monitor
- `SYSTEM_UID_MAX=auto` - Maximum UID (from /proc/sys/kernel/pid_max)
- `USER_INCLUDE_LIST` - Regex patterns for users who CAN be limited (empty = none)
- `USER_EXCLUDE_LIST` - Regex patterns for users who are NEVER limited
- `PROCESS_EXCLUDE_LIST=^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$` - Processes never limited (regex support)

#### Monitoring & Alerting
- `MONITOR_INACTIVE_TIMEOUT=3600` - Remove user if inactive for N seconds (proposal)
- `MONITOR_LONG_ACTIVITY_THRESHOLD=604800` - Alert if active for > N seconds (proposal)
- `MONITOR_LONG_ACTIVITY_CPU_THRESHOLD=50` - Alert only if avg CPU > N% (proposal)

#### Blackout Timeframes
- `CPU_MANAGER_BLACKOUT` - When NOT to apply limits (format: "days hours", e.g., "1-5 08-18")

#### Prometheus Metrics
- `ENABLE_PROMETHEUS=true` - Enable Prometheus metrics
- `PROMETHEUS_METRICS_BIND_HOST=127.0.0.1` - **SECURE DEFAULT: localhost only**
- `PROMETHEUS_METRICS_BIND_PORT=1974` - Metrics port
- `SERVER_ROLE=database` - Server role label (database, web-frontend, batch, etc.)

#### MCP Server
- `MCP_ENABLED=true` - Enable MCP server
- `MCP_TRANSPORT=stdio` - Transport: stdio, http, sse
- `MCP_HTTP_PORT=1969` - HTTP port (if transport=http)
- `MCP_ALLOW_WRITE_OPS=false` - Allow MCP write operations
- `MCP_AUTH_TOKEN` - Optional authentication token for HTTP transport

#### Database (SQLite)
- `METRICS_DB_ENABLED=true` - Enable metrics database
- `METRICS_DB_PATH=/etc/resman/metrics.db` - Database path
- `METRICS_DB_RETENTION_DAYS=30` - How long to keep historical data
- `METRICS_DB_WRITE_INTERVAL=30` - Database write interval (seconds)

#### Performance & Cache
- `POLLING_INTERVAL=30` - Control cycle interval (seconds)
- `MIN_ACTIVE_TIME=60` - Minimum time limits stay active (seconds)
- `METRICS_CACHE_TTL=15` - Metrics cache TTL (seconds)
- `USERNAME_CACHE_TTL=60` - Username cache TTL (minutes)

### Filtering Pipeline (Critical)

```
1. SYSTEM_UID_MIN/MAX → Only non-system users (UID 1000-pid_max)
   ↓
2. USER_INCLUDE_LIST → Filter who CAN be limited (empty = none)
   ↓
3. USER_EXCLUDE_LIST → Exclude users from limiting (regex)
   ↓
4. PROCESS_EXCLUDE_LIST → Exclude specific processes (regex)
   ↓
5. BLACKOUT → Skip limit application during timeframes
```

**Important Distinction (v1.18.0+):**
- **Monitoring**: ALL non-system users (UID >= SYSTEM_UID_MIN)
- **Limiting**: Only users passing all filters above

### Metrics Architecture (v1.18.0+)

#### ALL USERS Metrics (no filters)
- `resman_all_users_cpu_usage_percent{hostname, server_role}` - CPU of ALL users
- `resman_all_users_memory_usage_bytes{hostname, server_role}` - RAM of ALL users
- `resman_all_users_count{hostname, server_role}` - Count of ALL users

#### LIMITED USERS Metrics (pass filters)
- `resman_limited_users_cpu_usage_percent{hostname, server_role}` - CPU of limitable users
- `resman_limited_users_memory_usage_bytes{hostname, server_role}` - RAM of limitable users
- `resman_limited_users_count_filtered{hostname, server_role}` - Count of limitable users

#### Per-User Metrics (with is_limited label)
- `resman_user_cpu_usage_percent{uid, username, hostname, server_role, is_limited}`
- `resman_user_memory_usage_bytes{uid, username, hostname, server_role, is_limited}`
- `resman_user_process_count{uid, username, hostname, server_role, is_limited}`
- `resman_user_cpu_limited{uid, username, hostname, server_role, is_limited}`

#### System Metrics
- `resman_cpu_total_usage_percent{hostname, server_role}` - Total system CPU
- `resman_memory_usage_megabytes{hostname, server_role}` - System memory
- `resman_system_load_average{hostname, server_role}` - Load average (1 min)
- `resman_limits_active{hostname, server_role}` - Limits active (1/0)
- `resman_limited_users_count{hostname, server_role}` - Users with limits applied

#### Counter Metrics
- `resman_limits_activated_total{hostname, server_role}` - Total activations
- `resman_limits_deactivated_total{hostname, server_role}` - Total deactivations
- `resman_control_cycles_total{hostname, server_role}` - Control cycles executed
- `resman_errors_total{component, error_type, hostname, server_role}` - Errors by type

### MCP Server (17+ Tools)

#### Read-Only Tools
1. `get_system_status` - Overall system health
2. `get_user_metrics` - Per-user CPU/RAM/process metrics
3. `get_active_users` - List of currently active users
4. `get_limits_status` - Current limits state
5. `get_cgroup_info` - Cgroup details
6. `get_configuration` - Current configuration
7. `get_control_history` - Historical control decisions
8. `get_cpu_report` - CPU usage report
9. `get_mem_report` - Memory usage report
10. `get_user_filters` - Current filter configuration
11. `validate_user_filter_pattern` - Validate regex patterns
12. `get_user_history` - Historical user metrics (SQLite)
13. `get_system_history` - Historical system metrics (SQLite)

#### Write Operations (require MCP_ALLOW_WRITE_OPS=true)
14. `set_user_exclude_list` - Update USER_EXCLUDE_LIST
15. `set_user_include_list` - Update USER_INCLUDE_LIST
16. `activate_limits` - Manually activate CPU limits
17. `deactivate_limits` - Manually deactivate CPU limits

#### Proposed Tools (v1.19.0+)
- `get_long_running_processes` - Query by continuous activity duration
- `get_high_cpu_processes` - Query by CPU usage threshold
- `get_user_activity_summary` - Comprehensive user summary
- `reset_long_activity_alert` - Reset alert after investigation

### MCP Resources (6 endpoints)
- `resman://system/status` - System overview
- `resman://system/metrics` - Detailed metrics
- `resman://users/active` - Active users list
- `resman://users/{uid}/metrics` - Per-user metrics
- `resman://limits/status` - Limits state
- `resman://configuration` - Current config

### MCP Prompts (3 templates)
- `system_health_check` - Quick health assessment
- `user_cpu_analysis` - User CPU usage analysis
- `troubleshoot_limits` - Debug limit activations

### Grafana Dashboard
- **File**: `docs/dashboard-grafana.json`
- **Sections**:
  - SYSTEM OVERVIEW - Total CPU, memory, limits status
  - ALL USERS - Monitoring (no filters)
  - LIMITED USERS - Subset passing filters
  - LIMITS MANAGEMENT - Activations/deactivations
- **Variables**: cluster (external label), server_role, hostname, uid, username, is_limited
- **Multi-cluster Support**: Via Prometheus external_labels configuration
- **Documentation**: `docs/GRAFANA-MULTI-CLUSTER-GUIDE.md`

### Username Resolution
- **Method**: `os/user.LookupId()` (supports LDAP/NIS with CGO)
- **Fallback**: `/etc/passwd` parsing
- **Final Fallback**: UID as string
- **Cache**: TTL-based (USERNAME_CACHE_TTL, default 60 minutes)
- **Requirement**: Must compile with `CGO_ENABLED=1` for LDAP support

### CPU Usage Calculation
- **Method**: Delta calculation between two readings
- **Formula**: `cpuPercent = ((user2-user1) + (system2-system1)) / elapsed_seconds * 100`
- **Cache**: Per-process CPU times stored with automatic cleanup
- **Cleanup**: Automatic removal of processes inactive > 5 minutes

### RAM Usage Calculation
- **Method**: Read VmRSS from /proc/[pid]/status
- **Aggregation**: Sum across all processes per user
- **Cache**: Per-user RAM with TTL-based cleanup
- **Units**: Bytes (for Prometheus metrics)

---

## Version History (Complete)

### Version 1.18.2 (Latest - Critical Bug Fixes)
**Date:** 2026-03-24

#### 🐛 Critical Bug Fixes
- **[FIXED]** Race condition in `CleanupAll()` - concurrent map iteration panic
  - Problem: Lock released before iterating `createdCgroups` map
  - Solution: Make atomic copy of UIDs before releasing lock
  - Impact: Prevents crash during shutdown
  
- **[FIXED]** Double increment in `ThresholdTracker.ShouldActivateLimits()`
  - Problem: `totalCycles` incremented twice when CPU >= threshold
  - Solution: Removed duplicate increment at end of function
  - Impact: Accurate threshold duration tracking

#### 📝 Code Quality
- English comments standardization (continued)
- `CleanupAll()` comments → English
- `ShouldActivateLimits()` comments → English

**Files Changed:** `cgroup/manager.go`, `state/manager.go`, `CHANGELOG.md`

---

### Version 1.18.1 - Security and Performance Hardening
**Date:** 2026-03-24

#### 🔒 Security Improvements (5 fixes)
1. **Prometheus default bind to localhost (127.0.0.1)**
   - Changed from `0.0.0.0` to `127.0.0.1`
   - Prevents unauthorized network access to metrics
   - Migration: Set `PROMETHEUS_METRICS_BIND_HOST=0.0.0.0` to expose remotely

2. **MCP HTTP Bearer token authentication**
   - Validates `Authorization: Bearer <token>` header
   - Returns 401 Unauthorized for missing/invalid tokens
   - Configured via `MCP_AUTH_TOKEN` environment variable

3. **UID 0 (root) exclusion from cgroup operations**
   - Explicit check in `MoveProcessToCgroup()` and `MoveAllUserProcesses()`
   - Prevents accidental root process containment
   - Logs warning when UID 0 is detected

4. **Path traversal prevention**
   - `SCRIPT_CGROUP_BASE` validation
   - Rejects paths containing `..` or starting with `/`

5. **Port range validation (1-65535)**
   - `PROMETHEUS_METRICS_BIND_PORT`
   - `PROMETHEUS_PORT` (backward compatibility)
   - `MCP_HTTP_PORT`

#### ⚡ Performance Improvements (5 improvements)
1. **Cache size limits with LRU eviction**
   - `MAX_CACHE_SIZE`: 10000 entries (general metrics)
   - `MAX_PROC_CACHE_SIZE`: 5000 entries (process CPU times)
   - `MAX_USERNAME_CACHE_SIZE`: 10000 entries (username resolution)

2. **Race condition fix in `ApplyCPULimit()`**
   - Changed async process movement to synchronous with 5s timeout
   - Uses context-based cancellation
   - Prevents inconsistent cgroup state

3. **Backpressure for control cycles**
   - Skip cycle if previous is still running
   - `cycleComplete` channel signaling
   - Prevents resource exhaustion under slow cycles

4. **/proc scanning optimization**
   - Pre-allocation with estimated capacity
   - Formula: `len(entries) / 50` UIDs estimated
   - Reduces dynamic map allocations

5. **English comments standardization**
   - All comments in `metrics/collector.go` → English
   - Improved maintainability for international contributors

**Files Changed:** `config/config.go`, `mcp/server.go`, `cgroup/manager.go`, `metrics/collector.go`, `main.go`, `Makefile`, `README.md`

---

### Version 1.18.0 - Metrics Separation (ALL USERS vs LIMITED USERS)
**Date:** 2026-03-24

#### ⚠️ BREAKING CHANGES

**Metrics Separation: ALL USERS vs LIMITED USERS**

**NEW Metrics (clear separation):**

**ALL USERS** (tutti gli utenti non-system, UID >= SYSTEM_UID_MIN, SENZA filtri):
- `resman_all_users_cpu_usage_percent` → CPU totale di TUTTI gli utenti
- `resman_all_users_memory_usage_bytes` → RAM totale di TUTTI gli utenti
- `resman_all_users_count` → Numero di TUTTI gli utenti

**LIMITED USERS** (solo utenti che passano USER_INCLUDE_LIST && !USER_EXCLUDE_LIST):
- `resman_limited_users_cpu_usage_percent` → CPU solo utenti limitabili
- `resman_limited_users_memory_usage_bytes` → RAM solo utenti limitabili
- `resman_limited_users_count_filtered` → Numero utenti limitabili

**DEPRECATED Metrics (backward compatible):**
- `resman_cpu_user_usage_percent` → Use `resman_all_users_cpu_usage_percent`
- `resman_active_users_count` → Use `resman_all_users_count`

**Per-User Metrics (unchanged, already clear):**
- `resman_user_cpu_usage_percent{uid,username,is_limited}`
- `resman_user_memory_usage_bytes{uid,username,is_limited}`
- `resman_user_process_count{uid,username,is_limited}`

**Rationale:**
- Before: `resman_cpu_user_usage_percent` included only filtered users (confusing!)
- After: Clear distinction between "all monitored users" and "limitable subset"
- Grafana dashboard: two separate sections "ALL USERS" and "LIMITED USERS"

**Files Changed:** `metrics/collector.go`, `metrics/prometheus.go`, `state/manager.go`, `docs/dashboard-grafana.json`, `CHANGELOG.md`

---

### Version 1.17.1 - PROCESS_EXCLUDE_LIST Reintroduced
**Date:** 2026-03-23

#### ⚠️ BREAKING CHANGES

**USER_INCLUDE_LIST Behavior Change**
- **CHANGED**: Empty `USER_INCLUDE_LIST` now means NO users are limited (monitoring only)
- **CHANGED**: To limit ALL users (previous default), set `USER_INCLUDE_LIST=.*`
- **Added**: Warning message at startup when `USER_INCLUDE_LIST` is empty

**Migration:**
```bash
# BEFORE (v1.17.0): Empty = all users limited
USER_INCLUDE_LIST=

# AFTER (v1.17.1): Empty = no users limited (monitoring only)
# To limit all users:
USER_INCLUDE_LIST=.*
```

#### 🔄 Reversed Changes

**Reintroduced PROCESS_EXCLUDE_LIST**
- **REINTRODUCED**: `PROCESS_EXCLUDE_LIST` configuration variable (v1.17.0 removed it)
- **ADDED**: `IsProcessExcluded()` function
- **NEW**: Full regex support for process name patterns
- **Default**: `^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$`

**Bug Fixes:**
- **FIX**: `IsUserWhitelisted()` now considers both `USER_INCLUDE_LIST` and `USER_EXCLUDE_LIST`
  - Before: Only checked `!IsUserExcluded()` (ignored include list)
  - After: Checks `IsUserIncluded() && !IsUserExcluded()`

**Files Changed:** `config/config.go`, `metrics/collector.go`, `config/resman.conf.example`, `CHANGELOG.md`

---

### Version 1.17.0 - Separate Monitoring from Limiting
**Date:** 2026-03-23

#### ⚠️ BREAKING CHANGES

**Removed PROCESS_EXCLUDE_LIST**
- **REMOVED**: `PROCESS_EXCLUDE_LIST` configuration variable
- **REMOVED**: `IsProcessExcluded()` function
- **Rationale**: Feature didn't work correctly with per-user cgroups

#### 🎯 Major Changes

**Separate Monitoring from Limiting**
- **NEW**: ResMan now monitors ALL non-system users (UID >= 1000)
- **NEW**: CPU limits applied only to users passing filters
- **NEW**: `is_limited` label in all per-user Prometheus metrics
- **NEW**: `IsLimited` field in UserMetrics struct

**Before v1.17.0:**
- Only users passing filters were monitored
- Metrics exposed only for limited users

**Since v1.17.0:**
- ALL non-system users are monitored
- Metrics exposed for everyone with `is_limited` label
- Clear distinction between "monitored" and "limited" users

**Files Changed:** `metrics/collector.go`, `metrics/prometheus.go`, `state/manager.go`, `docs/dashboard-grafana.json`

---

### Version 1.16.5 - Project Renamed to ResMan
**Date:** 2026-03-23

#### Modificato
- Rinominato il progetto da `cpu-manager-go` a `resman`
- Aggiornati tutti i riferimenti nel codice e nella documentazione
- Il pacchetto RPM ora sostituisce il vecchio pacchetto `cpu-manager-go` (Obsoletes)

---

### Version 1.16.4 - RAM Limiting
**Date:** 2026-03-21

#### Aggiunto

**Limitazione RAM con cgroups v2**
- `RAM_LIMIT_ENABLED`: Abilita/disabilita limitazione RAM
- `RAM_THRESHOLD`: % RAM per attivazione limiti (default: 75)
- `RAM_RELEASE_THRESHOLD`: % RAM per rilascio limiti (default: 40)
- `RAM_QUOTA_LIMITED`: Limite RAM totale in bytes
- `RAM_QUOTA_PER_USER`: Limite RAM per utente (default: 512M)
- `DISABLE_SWAP`: Imposta memory.swap.max=0

**Metriche Prometheus:**
- `resman_ram_total_usage_percent` - RAM totale sistema %
- `resman_user_ram_usage_bytes` - RAM per utente in bytes

**Funzioni cgroup manager:**
- `ApplyRAMLimit(uid, limit)` - Applica limite RAM
- `RemoveRAMLimit(uid)` - Rimuovi limite RAM
- `GetCgroupRAMUsage(uid)` - Leggi uso RAM da cgroup

---

### Version 1.16.3 - Metrics Counter Fixes
**Date:** 2026-03-20

#### Corretto

**Metriche Prometheus**
- **FIX**: `resman_limits_activated_total` non veniva incrementata
- **FIX**: `resman_limits_deactivated_total` non veniva incrementata

**Log Verbosity Ridotta**
- **FIX**: Log "Active users detected" scritto solo quando la lista cambia
- **FIX**: Log metriche per-utente cambiato da INFO a DEBUG

---

### Version 1.16.2 - Shutdown Fix
**Date:** 2026-03-19

#### Corretto

**Critical Bug Fixes**
- **FIX**: Added `metricsCollector.Stop()` to shutdown sequence
- **FIX**: Added username cache cleanup in `cleanupCache()`

---

### Version 1.16.1 - Username Cache
**Date:** 2026-03-19

#### Aggiunto

**Username Resolution Cache**
- **NUOVO**: Cache con TTL configurabile per risoluzione UID -> username
- **Configurazione**: `USERNAME_CACHE_TTL` (default: 60 minuti)
- **Miglioramento**: Ridotte chiamate LDAP/NIS del 90%+

---

### Version 1.16.0 - Metrics Database
**Date:** 2026-03-19

#### Aggiunto

**Metrics Database - Storico Metriche via MCP**
- **NUOVO**: Persistenza delle metriche in database SQLite locale
- **NUOVO**: 4 nuovi MCP tools per interrogare lo storico
- **Configurazione**:
  - `METRICS_DB_ENABLED=true`
  - `METRICS_DB_PATH=/etc/resman/metrics.db`
  - `METRICS_DB_RETENTION_DAYS=30`
  - `METRICS_DB_WRITE_INTERVAL=30`

**Nuovi MCP Tools:**
- `get_user_history` - Storico CPU/RAM per utente
- `get_system_history` - Storico metriche di sistema
- `get_user_summary` - Statistiche aggregate (avg, min, max)
- `get_metrics_database_info` - Informazioni sul database

---

### Version 1.15.2 - Prometheus Metrics Cleanup
**Date:** 2026-03-17

#### Corretto

**Fix Cleanup Metriche Prometheus**
- **Fix critico**: Rimosse automaticamente le metriche per utenti non più attivi
- Previene "ghost" users in Prometheus queries

---

### Version 1.15.1 - Inactive User Release
**Date:** 2026-03-17

#### Corretto

**Fix Rilascio Utenti Inattivi**
- **Fix critico**: Utenti inattivi ora vengono rilasciati dal cgroup "limited"
- Utenti con CPU < 0.1% automaticamente rilasciati

---

### Version 1.15.0 - Threshold Time Window
**Date:** 2026-03-17

#### Aggiunto

**CPU_THRESHOLD_DURATION**
- Tempo di attesa prima di attivare i limiti dopo superamento soglia
- Default: 90 secondi (3 cicli da 30s)
- Previene attivazione per picchi CPU temporanei

---

### Version 1.14.1 - CPU Usage Fix
**Date:** 2026-03-13

#### Corretto

**Fix Calcolo CPU Usage**
- **Fix critico**: `getProcessCPUUsageSimple()` ora calcola correttamente il delta CPU
- Cambiato da `proc.CPUPercent()` a manual delta calculation
- Aggiunta cache per-processo CPU times

---

### Version 1.14.0 - Process Exclude List
**Date:** 2026-03-13

#### Aggiunto

**PROCESS_EXCLUDE_LIST Configurabile**
- Rimossa lista hardcoded di processi di sistema
- Nuova variabile `PROCESS_EXCLUDE_LIST` per specificare processi da escludere
- Supporto regex per pattern matching avanzato

---

### Version 1.13.1 - LDAP Support
**Date:** 2026-03-13

#### Corretto

**LDAP/NIS Username Resolution**
- **Fix**: `getUsername()` ora usa `os/user.LookupId()` per risolvere gli UID
- Supporto LDAP/NIS quando compilato con `CGO_ENABLED=1`
- Fallback automatico su `/etc/passwd`

---

### Version 1.13.0 - Grafana Enhancement
**Date:** 2026-03-13

#### Aggiunto

**Grafana Dashboard Enhancement**
- Aggiunte label `hostname` e `server_role` a tutte le metriche Prometheus
- Dashboard aggiornata con variabili: cluster, server_role, hostname
- Selezione multi-cluster tramite label esterna Prometheus `cluster`

---

### Version 1.12.0 - Blackout Timeframes
**Date:** 2026-03-13

#### Aggiunto

**CPU_MANAGER_BLACKOUT**
- Specifica quando CPU Manager NON deve applicare limiti CPU
- Formato crontab-like: "giorni ore" (es: "1-5 08-18")
- Supporto multipli timeframe separati da punto e virgola

---

### Version 1.11.0 - MCP User Filter Management
**Date:** 2026-03-13

#### Aggiunto

**MCP User Filter Management**
- 4 nuovi MCP tools per gestire dinamicamente USER_INCLUDE_LIST e USER_EXCLUDE_LIST
- Backup automatico della configurazione con timestamp
- Salvataggio atomico con rollback su errore

---

## Build & Deployment

### Production Build
```bash
CGO_ENABLED=1 go build -v -ldflags="-s -w" -o resman .
```

### Development Build (with debug symbols)
```bash
CGO_ENABLED=1 go build -v -gcflags="all=-N -l" -o resman .
```

### RPM Package
```bash
make rpm
# Output: ~/rpmbuild/RPMS/*/resman-*.rpm
```

### Debian Package
```bash
make deb
# Output: ~/build/deb/resman_*.deb
```

### Installation
```bash
sudo cp resman /usr/bin/
sudo cp config/resman.conf.example /etc/resman.conf
sudo cp packaging/systemd/resman.service /usr/lib/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now resman
```

### Verification
```bash
# Check service status
systemctl status resman

# Verify metrics endpoint
curl -s http://localhost:1974/metrics | grep resman

# Check logs
journalctl -u resman -f

# Check version
resman --version
```

---

## Known Issues & Workarounds

### Issue: First CPU Reading Always 0
**Status**: Expected behavior (by design)  
**Workaround**: Wait for second control cycle (30 seconds)  
**Reason**: Delta calculation requires two samples

### Issue: LDAP Users Show as UID
**Status**: Fixed in v1.13.1  
**Requirement**: Must compile with `CGO_ENABLED=1`  
**Verification**: `ldd /usr/bin/resman | grep libc`

### Issue: Blackout Not Triggering
**Status**: Configuration issue  
**Check**: Verify format "days hours" (e.g., "1-5 08-18")  
**Debug**: Check logs for "Entering blackout timeframe"

### Issue: Prometheus Metrics Not Visible Remotely
**Status**: Secure by default since v1.18.1  
**Solution**: Set `PROMETHEUS_METRICS_BIND_HOST=0.0.0.0` (not recommended without auth/TLS)

---

## Repository Structure

```
resman/
├── main.go                    # Entry point, signal handling
├── config/
│   ├── config.go             # Configuration structure and parsing
│   ├── watcher.go            # File watcher for auto-reload
│   └── resman.conf.example   # Example configuration
├── cgroup/
│   └── manager.go            # Cgroup v2 management (CPU + RAM)
├── metrics/
│   ├── collector.go          # System metrics collection
│   ├── prometheus.go         # Prometheus exporter
│   └── db_writer.go          # SQLite database writer
├── database/
│   └── manager.go            # SQLite database manager
├── state/
│   └── manager.go            # State management and control logic
├── reloader/
│   └── reloader.go           # Dynamic configuration reload
├── mcp/
│   ├── server.go             # MCP server implementation
│   ├── tools.go              # 17+ MCP tools
│   ├── resources.go          # 6 MCP resources
│   └── config.go             # MCP configuration
├── logging/
│   └── logger.go             # Structured logging
└── docs/
    ├── MCP-README.md
    ├── MCP-BLUEPRINT.md
    ├── GRAFANA-MULTI-CLUSTER-GUIDE.md
    ├── LDAP-USERNAME-RESOLUTION.md
    ├── METRICS-DATABASE.md
    ├── MCP-LONG-ACTIVITY-PROPOSAL.md  # Future feature proposal
    ├── dashboard-grafana.json
    └── alerting-rules.yml
```

---

## Future Enhancements (Proposal)

### Long Activity Detection (v1.19.0+)
See `docs/MCP-LONG-ACTIVITY-PROPOSAL.md` for complete specification.

**Proposed Features:**
- Monitor and record CPU/RAM for users in USER_INCLUDE_LIST even if below limits
- Detect processes active for > N days (configurable, default 7 days)
- Alert via Prometheus for long-running high-CPU processes
- MCP tools for natural language queries:
  - `get_long_running_processes` - "Are there processes running for > 1 week?"
  - `get_high_cpu_processes` - "Are there processes using >= 1 CPU core?"
  - `get_user_activity_summary` - "What's user www-data doing?"
  - `reset_long_activity_alert` - Reset alert after investigation

**Configuration (Proposed):**
```bash
MONITOR_INACTIVE_TIMEOUT=3600        # Remove if inactive for N seconds
MONITOR_LONG_ACTIVITY_THRESHOLD=604800  # Alert if active for > N seconds (7 days)
MONITOR_LONG_ACTIVITY_CPU_THRESHOLD=50  # Alert only if avg CPU > N%
```

---

## Summary Metadata

**Current Version:** 1.18.2  
**Last Updated:** 2026-03-24  
**Total Commits:** 15+ since v1.15.2  
**Security Fixes:** 5 (v1.18.1)  
**Performance Improvements:** 5 (v1.18.1)  
**Critical Bug Fixes:** 2 (v1.18.2)  
**Breaking Changes:** 3 (v1.17.0, v1.17.1, v1.18.0)

---

**End of Summary**
