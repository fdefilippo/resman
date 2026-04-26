# ResMan

Dynamic CPU, RAM, and IO resource manager for Linux using cgroups v2.

ResMan monitors system resources and automatically applies limits to users when load exceeds configurable thresholds. It exposes Prometheus metrics, supports hot-reload configuration, and includes an MCP server for AI assistant integration.

## Features

- Dynamic CPU, RAM, and IO limiting via cgroups v2
- PSI event-driven mode: uses poll() on cpu.pressure/io.pressure to trigger control cycles only when system pressure exceeds thresholds, reducing polling overhead when idle
- Per-user resource tracking with Prometheus metrics
- Configurable thresholds with time-window delay to prevent false activations
- User filtering via include/exclude regex lists
- Blackout timeframes to avoid applying limits during business hours
- Automatic configuration reload on file changes
- MCP server for AI assistant integration (17 tools)
- SQLite metrics database for historical data
- Optional script/webhook notification when a user is limited
- LDAP/NIS username resolution support (CGO)
- Grafana dashboard included

## Requirements

- Linux with cgroups v2 support
- Go 1.21+
- CGO enabled (for LDAP/NIS username resolution)

## Build

```bash
# Development build
make build

# RPM package
make rpm

# Debian package
make deb

# All packages
make all-with-packages
```

CGO must be enabled for LDAP/NIS support:

```bash
CGO_ENABLED=1 go build -v -ldflags="-s -w" -o resman .
```

## Install

```bash
# From packages
sudo rpm -ivh resman-*.rpm
# or
sudo dpkg -i resman-*.deb

# From source
sudo cp resman /usr/bin/
sudo cp config/resman.conf.example /etc/resman.conf
sudo cp packaging/systemd/resman.service /usr/lib/systemd/system/
sudo systemctl enable --now resman
```

## Usage

Edit `/etc/resman.conf` to configure thresholds and filters:

```bash
# CPU thresholds
CPU_THRESHOLD=75
CPU_RELEASE_THRESHOLD=40
CPU_THRESHOLD_DURATION=90

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
LIMIT_HOOK_TIMEOUT=10

# MCP server
MCP_ENABLED=true
MCP_TRANSPORT=stdio
# MCP_AUTH_TOKEN=change-me-for-http-transport

# PSI event-driven mode (optional, Linux >= 4.20 with CONFIG_PSI=y)
# PSI_EVENT_DRIVEN=true
# PSI_CPU_STALL_THRESHOLD=50000
# PSI_FALLBACK_INTERVAL=300
```

Limit hook scripts receive `RESMAN_LIMIT_*` environment variables. Webhooks receive
a JSON `POST` with `uid`, `username`, `cpu_usage`, `limited_users`,
`shared_cgroup`, `timestamp`, and `server_role`.

Restart the service after configuration changes:

```bash
sudo systemctl restart resman
```

Monitor the service:

```bash
sudo systemctl status resman
journalctl -u resman -f
curl -s http://localhost:1974/metrics | grep resman
```

## Documentation

- Man page: `man resman`
- Grafana dashboard: `docs/dashboard-grafana-v2.json`
- Architecture: `docs/ARCHITECTURE.md`
- IO limits: `docs/IO-LIMITS.md`
- Full configuration reference: `/etc/resman.conf.example`

## License

GNU General Public License v3.0 - see LICENSE file.
