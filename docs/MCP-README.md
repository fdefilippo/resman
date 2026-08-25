# MCP Server for ResMan

This document describes the Model Context Protocol (MCP) server implemented by ResMan.

## Protocol contract

ResMan uses `github.com/modelcontextprotocol/go-sdk/mcp` v1.7.0 and accepts only
MCP revision `2026-07-28` over stdio and Streamable HTTP. The server is
protocol-stateless: every request carries its protocol revision, client
capabilities and any method-specific name, and no client session is retained.

The HTTP endpoint accepts `POST` only and requires matching
`Mcp-Protocol-Version`, `Mcp-Method`, and, where defined by MCP, `Mcp-Name`
headers. Bearer authentication is checked on every HTTP request. Legacy
`initialize`/`notifications/initialized`, pre-2026 revisions,
`Mcp-Session-Id`, resumability headers, JSON-RPC batches, and compatibility
fallbacks are rejected. Closing an HTTP request cancels its in-flight handler.

Protocol statelessness does not make ResMan application-stateless: tools and
resources still read the shared, authoritative resource-manager state.

## Overview

The MCP server exposes ResMan functionality to AI assistants and MCP-compatible clients, allowing them to:
- Query system CPU and memory status
- Get per-user metrics (CPU, memory, processes)
- Check and manage CPU limits
- Access configuration and control history
- Generate comprehensive CPU and memory reports

## Features

### Tools (15 available)

| Tool | Description | Write Operation |
|------|-------------|-----------------|
| `get_system_status` | Get current CPU/memory status with hostname | No |
| `get_user_metrics` | Get metrics for specific user(s) | No |
| `get_active_users` | List active non-system users with hostname | No |
| `get_limits_status` | Check if CPU limits are active with hostname | No |
| `get_cgroup_info` | Get cgroup details for a user | No |
| `get_configuration` | Get current configuration with hostname | No |
| `get_control_history` | Get recent control cycle history | No |
| `get_cpu_report` | **Generate comprehensive CPU usage report** | No |
| `get_mem_report` | **Generate comprehensive memory usage report** | No |
| `get_user_filters` | **Get current user include/exclude filters** | No |
| `set_user_exclude_list` | **Set users to exclude from limits (regex)** | Yes* |
| `set_user_include_list` | **Set users to include in monitoring (regex)** | Yes* |
| `validate_user_filter_pattern` | **Validate regex pattern for filters** | No |
| `activate_limits` | Manually activate CPU limits | Yes* |
| `deactivate_limits` | Manually deactivate CPU limits | Yes* |

*Write operations require `MCP_ALLOW_WRITE_OPS=true`

**All metric outputs include the `hostname` field** for multi-server environments.

### Resources (6 URIs)

- `resman://system/status` - Real-time system status
- `resman://users/active` - List of active users
- `resman://limits/status` - Current limits status
- `resman://config` - Current configuration
- `resman://users/{uid}/metrics` - Per-user metrics
- `resman://cgroups/{uid}` - Cgroup information

### Prompts (3 pre-built queries)

- `system-health` - Quick system health check with assessment
- `user-analysis` - Analyze resource usage by user (table format)
- `troubleshooting` - Diagnose CPU limit issues

## Configuration

Add to `/etc/resman/resman.conf`:

```bash
# Enable MCP server
MCP_ENABLED=true

# Transport: stdio or http
MCP_TRANSPORT=stdio

# HTTPS settings (only for http transport)
# MCP_HTTP_HOST=127.0.0.1    # Default: loopback
# MCP_HTTP_PORT=1969         # Default: 1969
# MCP endpoint: https://HOST:PORT/mcp
MCP_TLS_ENABLED=true         # Mandatory for HTTP transport
MCP_TLS_CERT_FILE=/etc/resman/tls/server.crt
MCP_TLS_KEY_FILE=/etc/resman/tls/server.key
# MCP_TLS_CA_FILE=/etc/resman/tls/ca.crt # Enables mandatory client certificates
MCP_TLS_MIN_VERSION=1.3

# Log level
MCP_LOG_LEVEL=INFO

# Allow write operations (activate/deactivate limits)
# WARNING: Enable only if you trust all MCP clients
MCP_ALLOW_WRITE_OPS=false

# Required authentication token for HTTP
# MCP_AUTH_TOKEN=your-secret-token
```

## Usage

### With stdio transport (recommended for local AI assistants)

1. Enable MCP in configuration:
```bash
MCP_ENABLED=true
MCP_TRANSPORT=stdio
```

2. Start ResMan:
```bash
sudo systemctl start resman
```

3. Configure your MCP client (e.g., Claude Desktop) to use the resman binary as an MCP server.

### With HTTP transport

1. Configure HTTP transport:
```bash
MCP_ENABLED=true
MCP_TRANSPORT=http
MCP_HTTP_HOST=127.0.0.1
MCP_HTTP_PORT=1969
MCP_TLS_ENABLED=true
MCP_TLS_CERT_FILE=/etc/resman/tls/server.crt
MCP_TLS_KEY_FILE=/etc/resman/tls/server.key
MCP_TLS_MIN_VERSION=1.3
MCP_AUTH_TOKEN=replace-with-a-long-random-token
```

2. Access endpoints:
- `https://127.0.0.1:1969/mcp` - MCP endpoint
- `https://127.0.0.1:1969/health` - Health check

### Example: Claude Desktop Configuration

Add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "resman": {
      "command": "/usr/bin/resman",
      "args": ["--config", "/etc/resman/resman.conf"],
      "env": {
        "MCP_ENABLED": "true",
        "MCP_TRANSPORT": "stdio"
      }
    }
  }
}
```

**Note:** The current implementation requires running ResMan as a daemon. Local clients can use stdio without certificates because that transport never crosses the network.

### Example: Using HTTP Transport with curl

```bash
# Health check with the generated CA
curl --cacert /etc/resman/tls/ca.crt https://127.0.0.1:1969/health

# Get system status (via MCP client)
# MCP clients will handle the JSON-RPC protocol automatically
```

## Example Queries

### Query 1: Check system status
```
User: "What's the current CPU usage?"
AI: [Calls get_system_status tool]
AI: "Total CPU usage is 45% on server-web01, with 12% from non-system users. Memory usage is 2.3 GB."
```

### Query 2: Find high CPU users
```
User: "Which users are using the most CPU?"
AI: [Calls get_user_metrics tool]
AI: "Top users by CPU on server-web01: francesco (12.5%), www-data (8.2%), mysql (3.1%)"
```

### Query 3: Check limits status
```
User: "Are CPU limits currently active?"
AI: [Calls get_limits_status tool]
AI: "Yes, CPU limits are active since 14:30 on server-web01. Currently limiting 3 users."
```

### Query 4: Generate CPU Report ⭐ NEW
```
User: "Generate a CPU report"
AI: [Calls get_cpu_report tool]
AI: Returns formatted report:
```

**Example CPU Report Output:**
```
CPU Usage Report
Hostname: server-web01
Date: 2026-03-11 18:45:00
Total CPU available: 400.0%
Current usage: 45.2%

Active users:
francesco
    CPU usage: 12.5%
    Limits: Active
www-data
    CPU usage: 8.2%
    Limits: Inactive

Resource status:
Average CPU usage: 6.9%
Peak CPU usage: 12.5%
CPU limits: Active
Limited users: 1 of 2
```

### Query 5: Generate Memory Report ⭐ NEW
```
User: "Generate a memory report"
AI: [Calls get_mem_report tool]
```

**Example Memory Report Output:**
```
Memory Usage Report
Hostname: server-web01
Date: 2026-03-11 18:45:00
Total system memory: 2048.5 MB

Active users:
francesco
    Memory: 512.3 MB (537231360 bytes)
    Processes: 15
    Limits: Active

Resource status:
Average memory usage: 256.1 MB
Peak memory usage: 512.3 MB
CPU limits: Active
Limited users: 1 of 1
```

### Query 4: Activate limits (if enabled)
```
User: "Activate CPU limits now"
AI: [Calls activate_limits tool with force=true]
AI: "CPU limits have been activated successfully. 2 users are now being limited."
```

## Security Considerations

### Write Operations

By default, write operations (`activate_limits`, `deactivate_limits`) are **disabled**. Enable them only if:
- You trust all MCP clients with access
- You understand the security implications
- You have additional authentication in place

Enable with:
```bash
MCP_ALLOW_WRITE_OPS=true
```

### Authentication

HTTP transport requires token-based authentication:

```bash
MCP_AUTH_TOKEN=your-secret-token
```

Clients must then include:
```
Authorization: Bearer your-secret-token
```

### TLS and Network Exposure

The MCP HTTP transport is HTTPS-only. `MCP_TLS_ENABLED=false` is rejected whenever
HTTP transport is enabled, and the server loads the configured certificate before it
starts listening. The default bind remains `127.0.0.1`; a deliberate non-loopback
bind is allowed only through the same TLS-protected path.

The MCP TLS keys are independent from the Prometheus TLS keys. Their defaults point
at the same `/etc/resman/tls/server.crt` and `server.key` files, so the bundled
`docs/generate-tls-certs.sh` output can serve both listeners, but changing a
Prometheus path does not silently change MCP. The generated certificate covers
`localhost`, `resman`, `resman.local`, and `127.0.0.1`.

Setting `MCP_TLS_CA_FILE` enables mutual TLS: every client must present a certificate
issued by that CA in addition to sending the mandatory per-request bearer token.
Leave it empty for server-only TLS. The default minimum version is TLS 1.3.

An existing installation without certificate files cannot enable MCP over HTTP until
it generates or installs them. Use `docs/generate-tls-certs.sh`, configure the
`MCP_TLS_*` paths, or use `MCP_TRANSPORT=stdio` for a local non-network client.

## Testing

Run unit tests:
```bash
go test ./mcp/... -v
```

## Troubleshooting

### MCP server not starting

1. Check logs: `journalctl -u resman -f`
2. Verify configuration: `MCP_ENABLED=true`
3. Check port availability (for HTTP transport): `ss -tlnp | grep 1969`

### Tools not available

1. Verify `MCP_ENABLED=true` in configuration
2. Check that ResMan started successfully
3. Ensure the MCP server started without errors

### Permission errors

The MCP server runs with the same permissions as ResMan. Ensure:
- ResMan runs as root (required for cgroup access)
- Log file permissions are correct

## Architecture

```
┌─────────────────────────────────────────┐
│         MCP Client (AI Assistant)       │
└────────────────┬────────────────────────┘
                 │ JSON-RPC over stdio/HTTP
┌────────────────▼────────────────────────┐
│         MCP Server Layer                │
│  ┌─────────────────────────────────┐    │
│  │  Tools Handler                  │    │
│  │  - get_system_status            │    │
│  │  - get_user_metrics             │    │
│  │  - activate_limits              │    │
│  │  - etc.                         │    │
│  └─────────────────────────────────┘    │
│  ┌─────────────────────────────────┐    │
│  │  Resources Handler              │    │
│  │  - resman://system/status  │    │
│  │  - resman://users/{uid}    │    │
│  └─────────────────────────────────┘    │
└────────────┬────────────────────────────┘
             │
┌────────────▼────────────────────────────┐
│          ResMan Go Components           │
│  - State Manager                        │
│  - Metrics Collector                    │
│  - Cgroup Manager                       │
└─────────────────────────────────────────┘
```

## API Reference

### Tool: get_system_status

**Input:** None

**Output:**
```json
{
  "hostname": "server-web01",
  "total_cpu_usage": 45.5,
  "observed_users_cpu_usage": 12.3,
  "memory_usage_mb": 2345.6,
  "observed_users_count": 5,
  "actively_limited_users_count": 2,
  "total_cores": 8,
  "system_under_load": false,
  "any_limits_active": true,
  "cpu_limits_active": true,
  "resource_limits_active": false,
  "cpu_limits_applied_time": "2026-03-11T14:30:00Z",
  "resource_limits_applied_time": "",
  "shared_cgroup_active": true
}
```

### Tool: get_cpu_report ⭐ NEW

**Input:** None

**Output:** Text report with structured data
```
CPU Usage Report
Hostname: server-web01
Date: 2026-03-11 18:45:00
Total CPU available: 400.0%
Current usage: 45.2%

Active users:
francesco
    CPU usage: 12.5%
    Limits: Active

Resource status:
Average CPU usage: 6.9%
Peak CPU usage: 12.5%
CPU limits: Active
Limited users: 1 of 2
```

### Tool: get_mem_report ⭐ NEW

**Input:** None

**Output:** Text report with structured data
```
Memory Usage Report
Hostname: server-web01
Date: 2026-03-11 18:45:00
Total system memory: 2048.5 MB

Active users:
francesco
    Memory: 512.3 MB (537231360 bytes)
    Processes: 15
    Limits: Active

Resource status:
Average memory usage: 256.1 MB
Peak memory usage: 512.3 MB
```
```json
{
  "uids": [1000, 1001],  // optional
  "username": "francesco"  // optional
}
```

**Output:**
```json
{
  "users": [
    {
      "uid": 1000,
      "username": "francesco",
      "cpu_usage": 12.5,
      "memory_usage": 524288000,
      "process_count": 15
    }
  ]
}
```

### Tool: activate_limits

**Input:**
```json
{
  "force": true
}
```

**Output:**
```json
{
  "success": true,
  "message": "Limits activated successfully"
}
```

## User Filter Management (NEW in v1.11.0)

### Tool: get_user_filters

Returns the current user-filter configuration.

**Input:** None

**Output:**
```json
{
  "user_include_list": ["^www.*", "^app-.*"],
  "user_exclude_list": ["^test-.*", "francesco"],
  "config_file": "/etc/resman/resman.conf"
}
```

### Tool: set_user_exclude_list

Sets the users excluded from CPU limits (regex supported).

**Input:**
```json
{
  "patterns": ["^test-.*", "^dev-.*", "francesco"]
}
```

**Parameters:**
- `patterns` (array of strings): Regex patterns for users to exclude

The tool returns only after the new file has been validated and its runtime
application has succeeded or failed. The removed `reload` parameter is rejected
explicitly; persistence without runtime application is not supported.

**Output:**
```json
{
  "success": true,
  "message": "User exclude filters persisted and applied successfully",
  "previous_value": ["^old-.*"],
  "new_value": ["^test-.*", "^dev-.*", "francesco"],
  "persisted": true,
  "applied": true
}
```

**Automatic backup:**
- Before each change, the previous configuration atomically replaces the single
  rolling backup `/etc/resman/resman.conf.backup`.
- The active file and backup preserve the source mode and ownership; a new file uses
  mode `0600`.
- The configured path must be a regular file. Symbolic links, including dangling
  links, are rejected without replacing the link or changing its target; select the
  target itself with `--config` or replace the link with a regular file.
- If source ownership cannot be applied to the replacement, the write fails before
  secret-bearing content is written. Grant the resman service permission to `chown`
  the file or change its ownership to the service account before retrying.
- Legacy timestamped backups are removed on the next update after the secure rolling
  backup has been created.
- A write or durability failure restores the previous readable configuration before
  returning an error. If both the replacement and rollback parent-directory syncs
  fail, runtime state remains unchanged and the error reports that rollback durability
  could not be confirmed. If rollback fails before replacing the active file, stop
  resman, restore `/etc/resman/resman.conf.backup`, and restart before accepting another
  configuration write; the error reports that disk and runtime may differ, and resman
  rejects every later persistence attempt until restart. If the path was newly created
  and therefore has no backup, remove the new file while resman is stopped and restart.

### Tool: set_user_include_list

Sets the patterns used to include users in monitoring (regex supported).

**Input:**
```json
{
  "patterns": ["^www.*", "^app-.*", "mysql"]
}
```

**Parameters:**
- `patterns` (array of strings): Regex patterns for users to include

An empty array disables CPU eligibility. The operation has the same synchronous
persistence and runtime-application contract as `set_user_exclude_list`.

**Output:**
```json
{
  "success": true,
  "message": "User include filters persisted and applied successfully",
  "previous_value": [],
  "new_value": ["^www.*", "^app-.*", "mysql"],
  "persisted": true,
  "applied": true
}
```

### Tool: validate_user_filter_pattern

Validates a regex pattern and shows example matches.

**Input:**
```json
{
  "pattern": "^www.*",
  "type": "exclude"
}
```

**Parameters:**
- `pattern` (string): Regex pattern to validate (required)
- `type` (string, optional): Filter type, `include` or `exclude`

**Output:**
```json
{
  "valid": true,
  "pattern": "^www.*",
  "type": "exclude",
  "test_matches": ["www-data", "www-run"],
  "match_count": 2
}
```

**Test users:**
The tool tests the pattern against these example users:
- francesco, www-data, mysql, nobody, root
- test-user, dev-web, app-prod, svc-db, admin

## Future Enhancements

- [ ] WebSocket transport
- [ ] Real-time metrics streaming
- [ ] OAuth2 authentication
- [ ] Audit logging for write operations
- [ ] Rate limiting
- [ ] Custom resource templates
- [ ] Notification support

## References

- [MCP Specification](https://modelcontextprotocol.io/)
- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [ResMan README](../README.md)
