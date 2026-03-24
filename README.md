# ResMan

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-blue.svg)](https://golang.org/)
[![RPM Package](https://img.shields.io/badge/RPM-Package-red.svg)](https://github.com/fdefilippo/resman/releases)
[![Prometheus](https://img.shields.io/badge/Metrics-Prometheus-orange.svg)](https://prometheus.io/)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)
[![CI](https://github.com/fdefilippo/resman/actions/workflows/ci.yml/badge.svg)](https://github.com/fdefilippo/resman/actions/workflows/ci.yml)

**ResMan** is an enterprise-grade dynamic CPU and RAM resource management tool for Linux using cgroups v2. It automatically monitors system resources and applies limits to users when load exceeds configurable thresholds.

## ✨ Key Features

### Resource Management
- **Dynamic CPU limiting** based on configurable thresholds
- **RAM limiting** with cgroups v2 memory controller
- **Per-user resource tracking**: CPU%, Memory (bytes), Process count
- **Threshold time window** to prevent false activations (CPU_THRESHOLD_DURATION)
- **Blackout timeframes** support (CPU_MANAGER_BLACKOUT)

### User Filtering (v1.18.0+)
Clear separation between **ALL USERS** (monitoring) and **LIMITED USERS** (subset passing filters):

| Category | Description | Metrics |
|----------|-------------|---------|
| **ALL USERS** | All non-system users (UID ≥ SYSTEM_UID_MIN), NO filters applied | `resman_all_users_*` |
| **LIMITED USERS** | Users passing `USER_INCLUDE_LIST` && !`USER_EXCLUDE_LIST` | `resman_limited_users_*` |

### Monitoring & Integration
- **Prometheus metrics** export with comprehensive dashboard
- **MCP server** for AI assistant integration (Model Context Protocol)
- **SQLite metrics database** for historical data (METRICS_DB_*)
- **LDAP/NIS** username resolution support (requires CGO)
- **Username cache** for improved performance (USERNAME_CACHE_TTL)

### Operations
- **Automatic configuration reload** on file changes
- **Systemd service** integration with hardening
- **Graceful shutdown** with proper resource cleanup
- **Complete man page** documentation
- **Unit tests** for core packages

## 📊 Metrics Architecture (v1.18.0)

### ALL USERS Metrics
Monitor **all** non-system users without any filters:

```prometheus
resman_all_users_cpu_usage_percent      # Total CPU usage of ALL users
resman_all_users_memory_usage_bytes     # Total RAM usage of ALL users
resman_all_users_count                  # Number of ALL users
```

### LIMITED USERS Metrics
Track only users who **pass filters** (can be limited):

```prometheus
resman_limited_users_cpu_usage_percent      # CPU usage of limitable users
resman_limited_users_memory_usage_bytes     # RAM usage of limitable users
resman_limited_users_count_filtered         # Number of limitable users
```

### Per-User Metrics
Detailed metrics for each user with `is_limited` label:

```prometheus
resman_user_cpu_usage_percent{uid, username, hostname, server_role, is_limited}
resman_user_memory_usage_bytes{uid, username, hostname, server_role, is_limited}
resman_user_process_count{uid, username, hostname, server_role, is_limited}
```

### Example Configuration

```bash
# Monitor ALL users (UID >= 1000), limit only specific ones
USER_INCLUDE_LIST=^test.*     # Only users matching ^test.* can be limited
USER_EXCLUDE_LIST=admin       # But never limit 'admin' user
PROCESS_EXCLUDE_LIST=^systemd$,^dbus-.*  # Never limit these processes

# Result:
# - testuser1 (UID 1001) → Monitored + Limited (is_limited="true")
# - testuser2 (UID 1002) → Monitored + Limited (is_limited="true")
# - admin (UID 1003)      → Monitored + NOT Limited (is_limited="false")
# - normaluser (UID 1004) → Monitored + NOT Limited (is_limited="false")
```

## 🤖 MCP Server (AI Integration)

ResMan includes a built-in **Model Context Protocol (MCP)** server for AI assistant integration.

### MCP Tools (17 total)

**Read-only (13 tools):**
- `get_system_status` - Overall system health
- `get_user_metrics` - Per-user CPU/RAM/process metrics
- `get_active_users` - List of currently active users
- `get_limits_status` - Current limits state
- `get_cgroup_info` - Cgroup details
- `get_configuration` - Current configuration
- `get_control_history` - Historical control decisions
- `get_cpu_report` - CPU usage report
- `get_mem_report` - Memory usage report
- `get_user_filters` - Current filter configuration
- `validate_user_filter_pattern` - Validate regex patterns
- `get_user_history` - Historical user metrics (SQLite)
- `get_system_history` - Historical system metrics (SQLite)

**Write Operations (4 tools, require MCP_ALLOW_WRITE_OPS=true):**
- `set_user_exclude_list` - Update USER_EXCLUDE_LIST
- `set_user_include_list` - Update USER_INCLUDE_LIST
- `activate_limits` - Manually activate CPU limits
- `deactivate_limits` - Manually deactivate CPU limits

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

## 🚀 Quick Start

### Installation

#### From Source
```bash
# Clone repository
git clone https://github.com/fdefilippo/resman.git
cd resman

# Build (CGO required for LDAP/NIS support)
CGO_ENABLED=1 go build -v -ldflags="-s -w" -o resman .

# Install
sudo cp resman /usr/bin/
sudo cp config/resman.conf.example /etc/resman.conf
sudo cp packaging/systemd/resman.service /usr/lib/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now resman
```

#### From RPM
```bash
# Download latest RPM
wget https://github.com/fdefilippo/resman/releases/latest/download/resman-1.18.0-1.x86_64.rpm

# Install
sudo rpm -ivh resman-1.18.0-1.x86_64.rpm
sudo systemctl enable --now resman
```

### Configuration

Edit `/etc/resman.conf`:

```bash
# CPU thresholds (percentage)
CPU_THRESHOLD=75              # Activate limits when user CPU >= 75%
CPU_RELEASE_THRESHOLD=40      # Release limits when user CPU < 40%
CPU_THRESHOLD_DURATION=90     # Wait 90s before activating (prevent false positives)

# User filtering
USER_INCLUDE_LIST=.*          # All users can be limited
USER_EXCLUDE_LIST=root,admin  # Never limit root and admin

# Process exclusion (regex support)
PROCESS_EXCLUDE_LIST=^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$

# RAM limits (optional)
RAM_LIMIT_ENABLED=false
RAM_THRESHOLD=75
RAM_RELEASE_THRESHOLD=40
RAM_QUOTA_LIMITED=2G
RAM_QUOTA_PER_USER=512M

# Monitoring
SYSTEM_UID_MIN=1000           # Monitor users with UID >= 1000
POLLING_INTERVAL=30           # Check every 30 seconds

# Prometheus metrics (SECURE DEFAULT: localhost only)
ENABLE_PROMETHEUS=true
PROMETHEUS_METRICS_BIND_HOST=127.0.0.1  # Default: localhost (secure)
PROMETHEUS_METRICS_BIND_PORT=1974
SERVER_ROLE=database          # Optional: server role label

# MCP server
MCP_ENABLED=true
MCP_TRANSPORT=stdio           # or 'http'
MCP_ALLOW_WRITE_OPS=false     # Enable write operations
```

### Verify Installation

```bash
# Check service status
systemctl status resman

# View logs
journalctl -u resman -f

# Test Prometheus metrics endpoint
curl -s http://localhost:1974/metrics | grep resman

# Check man page
man resman
```

## 📈 Prometheus Metrics

### System Metrics
```prometheus
resman_cpu_total_usage_percent{hostname, server_role}          # Total system CPU
resman_memory_usage_megabytes{hostname, server_role}           # System memory
resman_system_load_average{hostname, server_role}              # Load average (1m)
resman_cpu_total_cores{hostname, server_role}                  # Total CPU cores
resman_limits_active{hostname, server_role}                    # Limits active (1/0)
resman_limited_users_count{hostname, server_role}              # Users with limits
```

### User Metrics (ALL)
```prometheus
resman_all_users_cpu_usage_percent{hostname, server_role}      # All users CPU
resman_all_users_memory_usage_bytes{hostname, server_role}     # All users RAM
resman_all_users_count{hostname, server_role}                  # All users count
```

### User Metrics (LIMITED)
```prometheus
resman_limited_users_cpu_usage_percent{hostname, server_role}      # Limited users CPU
resman_limited_users_memory_usage_bytes{hostname, server_role}     # Limited users RAM
resman_limited_users_count_filtered{hostname, server_role}         # Limited users count
```

### Per-User Metrics
```prometheus
resman_user_cpu_usage_percent{uid, username, hostname, server_role, is_limited}
resman_user_memory_usage_bytes{uid, username, hostname, server_role, is_limited}
resman_user_process_count{uid, username, hostname, server_role, is_limited}
resman_user_cpu_limited{uid, username, hostname, server_role, is_limited}
```

### Counter Metrics
```prometheus
resman_limits_activated_total{hostname, server_role}           # Total activations
resman_limits_deactivated_total{hostname, server_role}         # Total deactivations
resman_control_cycles_total{hostname, server_role}             # Control cycles
resman_errors_total{component, error_type, hostname, server_role}  # Errors
```

## 🔧 Configuration Variables

### Paths
| Variable | Default | Description |
|----------|---------|-------------|
| `CGROUP_ROOT` | `/sys/fs/cgroup` | Cgroup filesystem root |
| `CONFIG_FILE` | `/etc/resman.conf` | Configuration file path |
| `LOG_FILE` | `/var/log/resman.log` | Log file path |
| `METRICS_DB_PATH` | `/etc/resman/metrics.db` | SQLite database path |

### Timing
| Variable | Default | Description |
|----------|---------|-------------|
| `POLLING_INTERVAL` | `30` | Control cycle interval (seconds) |
| `MIN_ACTIVE_TIME` | `60` | Minimum time limits stay active (seconds) |
| `METRICS_CACHE_TTL` | `15` | Metrics cache TTL (seconds) |
| `USERNAME_CACHE_TTL` | `60` | Username cache TTL (minutes) |

### CPU Thresholds
| Variable | Default | Description |
|----------|---------|-------------|
| `CPU_THRESHOLD` | `75` | Activate limits when user CPU ≥ X% |
| `CPU_RELEASE_THRESHOLD` | `40` | Release limits when user CPU < X% |
| `CPU_THRESHOLD_DURATION` | `90` | Wait time before activating limits (seconds) |
| `CPU_QUOTA_NORMAL` | `max 100000` | Normal CPU quota (cpu.max format) |
| `CPU_QUOTA_LIMITED` | `50000 100000` | Limited CPU quota (0.5 core) |

### RAM Thresholds
| Variable | Default | Description |
|----------|---------|-------------|
| `RAM_LIMIT_ENABLED` | `false` | Enable RAM limiting |
| `RAM_THRESHOLD` | `75` | Activate RAM limits when ≥ X% |
| `RAM_RELEASE_THRESHOLD` | `40` | Release RAM limits when < X% |
| `RAM_QUOTA_LIMITED` | `2G` | Total RAM quota for limited users |
| `RAM_QUOTA_PER_USER` | `512M` | Per-user RAM quota |
| `DISABLE_SWAP` | `false` | Disable swap in cgroups |

### User Filtering
| Variable | Default | Description |
|----------|---------|-------------|
| `SYSTEM_UID_MIN` | `1000` | Minimum UID to monitor |
| `SYSTEM_UID_MAX` | Auto | Maximum UID (from /proc/sys/kernel/pid_max) |
| `USER_INCLUDE_LIST` | Empty | Regex patterns for users to limit |
| `USER_EXCLUDE_LIST` | Empty | Regex patterns for users to exclude |
| `PROCESS_EXCLUDE_LIST` | `^systemd$,^dbus-daemon$,^dbus-broker$,^polkitd$` | Processes to never limit |

### Blackout Timeframes
| Variable | Default | Description |
|----------|---------|-------------|
| `CPU_MANAGER_BLACKOUT` | Empty | When NOT to apply limits (format: "days hours") |

**Examples:**
```bash
# Business hours (Mon-Fri, 8-18)
CPU_MANAGER_BLACKOUT=1-5 08-18

# Weekends
CPU_MANAGER_BLACKOUT=0,6 00-23

# Business hours + weekends
CPU_MANAGER_BLACKOUT=1-5 08-18;0,6 00-23
```

### Database
| Variable | Default | Description |
|----------|---------|-------------|
| `METRICS_DB_ENABLED` | `false` | Enable SQLite metrics database |
| `METRICS_DB_RETENTION_DAYS` | `30` | How long to keep historical data |
| `METRICS_DB_WRITE_INTERVAL` | `30` | Database write interval (seconds) |

## 📚 Documentation

- **Man page**: `man resman` or `docs/resman.8`
- **Grafana dashboard**: `docs/dashboard-grafana.json`
- **Multi-cluster guide**: `docs/GRAFANA-MULTI-CLUSTER-GUIDE.md`
- **MCP documentation**: `docs/MCP-README.md`
- **TLS configuration**: `docs/TLS-CONFIGURATION.md`
- **Prometheus queries**: `docs/prometheus-queries.md`
- **Alerting rules**: `docs/alerting-rules.yml`

## 🏗️ Architecture

ResMan uses Linux cgroups v2 with the following controllers:
- **cpu** - CPU bandwidth control (cpu.max, cpu.weight)
- **memory** - RAM limiting (memory.max)

### Control Flow
```
1. Collect metrics (every POLLING_INTERVAL seconds)
   ├─ All users (UID >= SYSTEM_UID_MIN)
   ├─ CPU%, Memory, Process count
   └─ System load, Total CPU

2. Apply filters
   ├─ USER_INCLUDE_LIST (if configured)
   ├─ USER_EXCLUDE_LIST
   ├─ PROCESS_EXCLUDE_LIST
   └─ BLACKOUT timeframes

3. Make decision
   ├─ CPU_THRESHOLD_DURATION check
   ├─ Activate if CPU >= CPU_THRESHOLD
   ├─ Release if CPU < CPU_RELEASE_THRESHOLD
   └─ Respect MIN_ACTIVE_TIME

4. Apply limits
   ├─ Create cgroups
   ├─ Apply cpu.max / memory.max
   └─ Move user processes
```

## 🧪 Testing

```bash
# Run all tests
make test

# Test with coverage
make test-cover

# Run linters
make lint

# Format code
make fmt
```

## 📦 Building

```bash
# Development build
make build

# Release build (multi-architecture)
make release

# Static binary
make static

# RPM package
make rpm

# Debian package
make deb

# All-inclusive
make all-with-packages
```

## 🤝 Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development guidelines.

## 📄 License

This project is licensed under the GNU General Public License v3.0 - see the [LICENSE](LICENSE) file for details.

## 👥 Authors

- **Francesco Defilippo** - *Initial work and maintenance*

## 🙏 Acknowledgments

- Linux cgroups v2 documentation
- Prometheus community
- Model Context Protocol specification
- Go programming language community
