# CPU Manager Go

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-blue.svg)](https://golang.org/)
[![RPM Package](https://img.shields.io/badge/RPM-Package-red.svg)](https://github.com/fdefilippo/cpu-manager-go/releases)
[![Prometheus](https://img.shields.io/badge/Metrics-Prometheus-orange.svg)](https://prometheus.io/)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)
[![CI](https://github.com/fdefilippo/cpu-manager-go/actions/workflows/ci.yml/badge.svg)](https://github.com/fdefilippo/cpu-manager-go/actions/workflows/ci.yml)
[![Test Coverage](https://github.com/fdefilippo/cpu-manager-go/actions/workflows/test-coverage.yml/badge.svg)](https://github.com/fdefilippo/cpu-manager-go/actions/workflows/test-coverage.yml)

Enterprise-grade dynamic CPU resource management tool using Linux cgroups v2. Automatically limits CPU for non-system users based on configurable thresholds.

## ✨ Features

- **Dynamic CPU limiting** for non-system users (UID >=1000)
- **Configurable thresholds** for activation and release
- **Absolute CPU limits** using `cpu.max` cgroup controller
- **Prometheus metrics** export with comprehensive dashboard
- **Per-user metrics**: CPU%, Memory (bytes), Process count
- **Systemd service** integration with hardening
- **Automatic configuration reload** on file changes
- **Detailed process logging** with process name tracking
- **Load average awareness** (optional)
- **Graceful shutdown** with cleanup
- **Complete man page** documentation
- **Unit tests** for core packages
- **MCP server** for AI assistant integration (Model Context Protocol)

## 🤖 MCP Server (AI Integration)

CPU Manager Go includes a built-in **Model Context Protocol (MCP)** server that exposes system metrics and control capabilities to AI assistants.

### Features
- **9 MCP Tools**: Query system status, user metrics, limits status, and manage CPU limits
- **6 Resources**: REST-like URIs for system data (`cpu-manager://system/status`, `cpu-manager://users/{uid}/metrics`, etc.)
- **3 Prompts**: Pre-built queries for system health, user analysis, and troubleshooting
- **Multiple Transports**: stdio (for local AI clients), HTTP, and SSE
- **SQLite Metrics Database** (v1.16.0+): Historical metrics storage with 4 additional MCP tools for temporal queries

### Metrics Database (New in v1.16.0)

CPU Manager now supports persistent storage of metrics in a local SQLite database, enabling historical queries via MCP:

**New MCP Tools:**
- `get_user_history`: Historical CPU/RAM metrics for a specific user
- `get_system_history`: Historical system-wide metrics
- `get_user_summary`: Aggregated statistics (avg, min, max) for a user
- `get_database_info`: Database information (size, record counts, retention)

**Configuration:**
```bash
# /etc/cpu-manager.conf
METRICS_DB_ENABLED=true
METRICS_DB_PATH=/etc/cpu-manager/metrics.db
METRICS_DB_RETENTION_DAYS=30
METRICS_DB_WRITE_INTERVAL=30
```

**Benefits:**
- ✅ Historical metrics accessible via MCP (previously only available via Prometheus)
- ✅ Flexible temporal queries (predefined periods or custom ranges)
- ✅ AI assistant integration for time-based analysis
- ✅ Low performance impact (asynchronous writes)
- ✅ Automatic data retention management

For complete documentation, see:
- **[docs/METRICS-DATABASE.md](docs/METRICS-DATABASE.md)** - Complete guide and SQL examples

### Quick Start
```bash
# /etc/cpu-manager.conf
MCP_ENABLED=true
MCP_TRANSPORT=stdio        # or http, sse
MCP_ALLOW_WRITE_OPS=false  # Enable write operations with caution
```

### Example Usage
An AI assistant can query:
- "What's the current CPU usage?" → `get_system_status`
- "Which users are using the most CPU?" → `get_user_metrics`
- "Are CPU limits active?" → `get_limits_status`

For complete documentation, see:
- **[docs/MCP-README.md](docs/MCP-README.md)** - Usage guide and examples
- **[docs/MCP-BLUEPRINT.md](docs/MCP-BLUEPRINT.md)** - Architecture and implementation details

## 📊 Prometheus Metrics

CPU Manager Go exports detailed metrics for monitoring and alerting:

### System Metrics
| Metric | Type | Description |
|--------|------|-------------|
| `cpu_manager_cpu_total_usage_percent` | Gauge | Total CPU usage percentage |
| `cpu_manager_cpu_user_usage_percent` | Gauge | Non-system user CPU usage |
| `cpu_manager_memory_usage_megabytes` | Gauge | Total memory usage |
| `cpu_manager_system_load_average` | Gauge | System load (1 min) |
| `cpu_manager_active_users_count` | Gauge | Active non-system users |
| `cpu_manager_limited_users_count` | Gauge | Users with CPU limits |
| `cpu_manager_limits_active` | Gauge | Limits status (1=active) |

### Per-User Metrics (with `uid` and `username` labels)
| Metric | Type | Description |
|--------|------|-------------|
| `cpu_manager_user_cpu_usage_percent` | Gauge | CPU usage per user |
| `cpu_manager_user_memory_usage_bytes` | Gauge | Memory usage per user |
| `cpu_manager_user_process_count` | Gauge | Process count per user |
| `cpu_manager_user_cpu_limited` | Gauge | User limit status |

### Example Queries
```promql
# Top 5 users by CPU usage
topk(5, cpu_manager_user_cpu_usage_percent)

# Total memory used by all users
sum(cpu_manager_user_memory_usage_bytes)

# Alert: User memory exceeds 2GB
cpu_manager_user_memory_usage_bytes > 2147483648

# Processes for specific user
cpu_manager_user_process_count{username="francesco"}
```

See [docs/prometheus-queries.md](docs/prometheus-queries.md) for more examples.

## 🔐 Prometheus Authentication

CPU Manager Go supports optional authentication for securing metrics endpoints:

### Basic Authentication
```bash
# /etc/cpu-manager.conf
PROMETHEUS_AUTH_TYPE=basic
PROMETHEUS_AUTH_USERNAME=prometheus
PROMETHEUS_AUTH_PASSWORD_FILE=/etc/cpu-manager/prometheus_password
```

### JWT (Bearer Token) Authentication
```bash
# /etc/cpu-manager.conf
PROMETHEUS_AUTH_TYPE=jwt
PROMETHEUS_JWT_SECRET_FILE=/etc/cpu-manager/jwt_secret
PROMETHEUS_JWT_ISSUER=cpu-manager
PROMETHEUS_JWT_AUDIENCE=prometheus
PROMETHEUS_JWT_EXPIRY=3600
```

### Both Methods
```bash
# Support both Basic Auth and JWT
PROMETHEUS_AUTH_TYPE=both
```

## 🔒 TLS/HTTPS Encryption

Enable HTTPS encryption for metrics endpoints:

```bash
# /etc/cpu-manager.conf
PROMETHEUS_TLS_ENABLED=true
PROMETHEUS_TLS_CERT_FILE=/etc/cpu-manager/tls/server.crt
PROMETHEUS_TLS_KEY_FILE=/etc/cpu-manager/tls/server.key
PROMETHEUS_TLS_CA_FILE=/etc/cpu-manager/tls/ca.crt
PROMETHEUS_TLS_MIN_VERSION=1.2
```

### Generate TLS Certificates

```bash
# Use the provided script
sudo ./docs/generate-tls-certs.sh /etc/cpu-manager/tls
```

See [docs/TLS-CONFIGURATION.md](docs/TLS-CONFIGURATION.md) for detailed TLS setup.

See [docs/MULTI-INSTANCE-MONITORING.md](docs/MULTI-INSTANCE-MONITORING.md) for detailed configuration.

## 📈 Grafana Dashboard

A comprehensive Grafana dashboard is included with panels for:
- CPU Usage Overview (total + per user)
- Memory Usage Per User
- Processes Per User
- Active Users & Limit Status
- Control Cycle Performance
- Error Rate by Component

Import `docs/dashboard-grafana.json` into Grafana to get started.

## 🚀 Quick Start

### Build RPM package
```bash
make rpm
```

### Build Debian package
```bash
make deb
```

### Install
```bash
# RPM
rpm -ivh ~/rpmbuild/RPMS/*/cpu-manager-go-*.rpm

# Debian
dpkg -i build/deb/cpu-manager-go_*.deb
```

### Configure
```bash
vi /etc/cpu-manager.conf
```

### Start service
```bash
systemctl enable --now cpu-manager
```

## Prerequisites: Enabling cgroups v2 on Enterprise Linux ≥ 8
CPU Manager requires cgroups v2 with CPU and cpuset controllers enabled.
Here's how to enable them on RHEL/CentOS/Rocky/AlmaLinux ≥ 8:
```
# Enable unified cgroup hierarchy
grubby --update-kernel=ALL --args="systemd.unified_cgroup_hierarchy=1"

# Verify the change
grubby --info=ALL | grep "systemd.unified_cgroup_hierarchy"

# Reboot
reboot

# After reboot, enable CPU controllers
echo "+cpu" | sudo tee -a /sys/fs/cgroup/cgroup.subtree_control
echo "+cpuset" | sudo tee -a /sys/fs/cgroup/cgroup.subtree_control
```

Persistent via Systemd Service (Recommended)
Create /etc/systemd/system/cgroup-tweaks.service:
```
[Unit]
Description=Configure cgroup subtree controls
Before=systemd-user-sessions.service
Before=cpu-manager.service

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo "+cpu" >> /sys/fs/cgroup/cgroup.subtree_control'
ExecStart=/bin/sh -c 'echo "+cpuset" >> /sys/fs/cgroup/cgroup.subtree_control'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

## 🧪 Testing

Run unit tests:
```bash
make test
```

Run tests with coverage:
```bash
make test-cover
```

## 📖 Documentation

- [Man page](docs/cpu-manager.8) - Full command reference
- [Prometheus Queries](docs/prometheus-queries.md) - Example queries
- [Alerting Rules](docs/alerting-rules.yml) - Prometheus alerting
- [Grafana Dashboard](docs/dashboard-grafana.json) - Pre-built dashboard

## Configuration

See [config/cpu-manager.conf.example](config/cpu-manager.conf.example) for all available options.

Key settings:
- `CPU_THRESHOLD` - Activation threshold (default: 75%)
- `CPU_RELEASE_THRESHOLD` - Deactivation threshold (default: 40%)
- `POLLING_INTERVAL` - Control cycle interval (default: 30s)
- `MIN_SYSTEM_CORES` - Cores reserved for system (default: 2)
- `ENABLE_PROMETHEUS` - Enable metrics export (default: false)
- `PROMETHEUS_PORT` - Metrics port (default: 9101)

## System Requirements

- Linux kernel 4.5+ (cgroups v2)
- Write access to /sys/fs/cgroup
- Root privileges or CAP_SYS_ADMIN capability
- **GCC compiler** (required for CGO and user lookup via NSS)
- **glibc with NSS support** (for LDAP, NIS, SSSD authentication backends)

### Build Requirements

To build CPU Manager Go from source:

- Go 1.21 or later
- GCC (for CGO support)
- CGO enabled (`CGO_ENABLED=1`)

**Important:** CGO is required for proper user name resolution via NSS (Name Service Switch). This allows CPU Manager to work with:
- Local users (`/etc/passwd`)
- LDAP/Active Directory users
- NIS users
- SSSD-managed users

### Building with CGO

```bash
# Standard build with CGO enabled
export CGO_ENABLED=1
export CC=gcc
go build -v -ldflags="-s -w -X 'main.version=1.6.0'" -o cpu-manager-go .

# Build RPM package (CGO automatically enabled)
make rpm

# Build Debian package (CGO automatically enabled)
make deb
```

## License

GNU General Public License v3.0 - see [LICENSE](LICENSE) for details.
