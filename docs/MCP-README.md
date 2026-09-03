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

### Discovery inventories

#### Tool inventory

This table is the authoritative inventory of tools exposed by the production server.
"Registered when" controls whether a tool appears in `tools/list`; "Invocation
requirement" controls whether a registered tool can complete successfully.

<!-- BEGIN MCP TOOL INVENTORY -->

| Tool | Description | Registered when | Invocation requirement |
|------|-------------|-----------------|------------------------|
| `get_system_status` | Get current CPU and memory status | Always | None |
| `get_user_metrics` | Get metrics for specific users | Always | None |
| `get_active_users` | List active non-system users | Always | None |
| `get_limits_status` | Get current resource-limit status | Always | None |
| `get_cgroup_info` | Get cgroup details for a user | Always | None |
| `get_configuration` | Get current CPU, RAM, and I/O resource-policy configuration | Always | None |
| `get_configuration_editor` | Get the redacted versioned editor snapshot | Always | None |
| `update_configuration` | Apply a revision-bound partial configuration update | Always | `MCP_ALLOW_WRITE_OPS=true` |
| `update_cpu_points` | Apply revision-bound CPU Points map changes | Always | `MCP_ALLOW_WRITE_OPS=true` |
| `get_cpu_report` | Generate a CPU usage report | Always | None |
| `get_mem_report` | Generate a memory usage report | Always | None |
| `get_control_history` | Get recent control-cycle history | Always | None |
| `set_user_exclude_list` | Persist and apply CPU exclusion patterns | Always | `MCP_ALLOW_WRITE_OPS=true` |
| `set_user_include_list` | Persist and apply CPU eligibility patterns | Always | `MCP_ALLOW_WRITE_OPS=true` |
| `get_user_filters` | Get current CPU include and exclude patterns | Always | None |
| `validate_user_filter_pattern` | Validate a user-filter regular expression | Always | None |
| `get_user_history` | Get historical metrics for a user | Always | `METRICS_DB_ENABLED=true` |
| `get_system_history` | Get historical system metrics | Always | `METRICS_DB_ENABLED=true` |
| `get_user_summary` | Get aggregate historical statistics for a user | Always | `METRICS_DB_ENABLED=true` |
| `get_metrics_database_info` | Get metrics-database status and retention information | Always | `METRICS_DB_ENABLED=true` |
| `activate_limits` | Manually request CPU-limit activation | `MCP_ALLOW_WRITE_OPS=true` | `MCP_ALLOW_WRITE_OPS=true` |
| `deactivate_limits` | Manually deactivate CPU limits | `MCP_ALLOW_WRITE_OPS=true` | `MCP_ALLOW_WRITE_OPS=true` |

<!-- END MCP TOOL INVENTORY -->

The database-backed tools remain visible when the metrics database is disabled so a
client receives an explicit `metrics database is not enabled` error instead of a
different discovery schema. The two user-filter setters are also always visible, but
reject invocation while write operations are disabled. Only manual activation and
deactivation are omitted from discovery unless write operations are enabled.

HTTP supports two deliberately separate bearer principals. `MCP_AUTH_TOKEN` is the
full operator credential. `MCP_EDITOR_AUTH_TOKEN` is a distinct least-privilege
credential accepted only for the configuration-editor tools and observed-state
reads in ResMan's positive allowlist; it cannot activate or deactivate limits, use
the legacy filter setters, or acquire access to a future tool by default. Both
editor update tools also require `MCP_ALLOW_WRITE_OPS=true`. Stdio remains a local
operator transport and does not use bearer authentication.

`get_configuration_editor` returns typed field metadata, redacted authored and
effective values, CPU Points entries and one composite revision for the two source
files. It never returns a complete configuration file or secret value. Updates are
partial and revision-bound: omitted values, comments and secret bytes are preserved;
a stale source is refused with a bounded non-sensitive conflict description. A
terminal result distinguishes `applied`, `pending_restart`, `refused` and `failed`.
Mutation completion logs contain only the bounded principal, operation, terminal state
and requested/persisted/applied revision digests, allowing correlation with a client
audit record without treating the service credential as a human identity.
The versioned client schema and compatibility fixtures are shipped under an Apache
2.0 grant in `protocol/config-editor/`; daemon code remains GPL-3.0-or-later.

System-wide payloads and the shared active-user/configuration schemas include
`hostname` for multi-server identification. Per-user entries are identified by `uid`
and `username` inside those host-scoped responses.

#### Fixed resources

<!-- BEGIN MCP FIXED RESOURCE INVENTORY -->

| Resource URI | Description |
|--------------|-------------|
| `resman://system/status` | Real-time system status |
| `resman://users/active` | List of active users |
| `resman://limits/status` | Current limits status |
| `resman://config` | Current configuration |

<!-- END MCP FIXED RESOURCE INVENTORY -->

#### Resource URI templates

<!-- BEGIN MCP RESOURCE TEMPLATE INVENTORY -->

| URI template | Description |
|--------------|-------------|
| `resman://users/{uid}/metrics` | Per-user metrics |
| `resman://cgroups/{uid}` | Cgroup information |

<!-- END MCP RESOURCE TEMPLATE INVENTORY -->

#### Prompts

<!-- BEGIN MCP PROMPT INVENTORY -->

| Prompt | Description |
|--------|-------------|
| `system-health` | Quick system health check with assessment |
| `user-analysis` | Analyze resource usage by user in table format |
| `troubleshooting` | Diagnose CPU limit issues |

<!-- END MCP PROMPT INVENTORY -->

### Shared tool and resource schemas

MCP result payloads are typed contracts. `get_active_users` and
`resman://users/active` share one object schema containing `hostname`, `server_role`,
and a `users` array whose entries contain both `uid` and `username`.
`get_configuration` and `resman://config` share one resource-policy schema covering
CPU, RAM, and I/O thresholds and limits plus the host identity. The configuration
payload is deliberately scoped to policy and exporter settings; it is not a dump of
secrets or every daemon setting.

`get_user_metrics` and `resman://users/{uid}/metrics` also share the same per-user
projection, including explicit eligibility, requested-limit, and active-limit fields.
Historical user and system records remain distinct typed schemas because their fields
have different meanings. Input JSON Schema maps used by MCP discovery are protocol
metadata, not result payloads.

Current system and limit status include a nested `cpu_points` contract. It separates
reserve and nominal pool from effective parent-delivered CPU time, publishes the live
online-CPU denominator and synchronized interval deltas, and reports bounded delivery
and class-priority lending states. Current per-user status separates configured class
and optional mapped guarantee from requested enforcement, applied class/raw weight,
lifecycle, reconciliation state, and complete or partial process coverage. Best-effort
users omit `configured_guarantee_points`; absence is not zero. `get_system_status` and
`resman://system/status` use the same projection over HTTP and stdio.

The status never calls raw `cpu.weight` CPU Points and never calls programmed
`cpu.max` delivered bandwidth. A `throttled_parent` state can be normal evidence that
the finite pool is active, because CFS can under-deliver its nominal quota. A
`best_effort_borrowed` lending state means the complete guaranteed domain was inactive
for the observed interval; ResMan does not estimate runnable points in userspace.

`get_cpu_report` carries the same nested system CPU Points projection and includes each
observed user's bounded configured-class, lifecycle, and process-coverage state in its text.
The system-health, user-analysis, and troubleshooting prompts expose the corresponding bounded
summary. The active-user inventory remains identity-only; the memory report remains scoped to
process-derived memory and RAM-limit state rather than duplicating CPU Points status.

#### Cgroup interface availability

`get_cgroup_info` and `resman://cgroups/{uid}` share one JSON schema. They expose the
`cpu.max`, `cpu.weight`, `memory.current`, `memory.max`, and `memory.high` interfaces as
`cpu_max`, `cpu_weight`, `memory_current`, `memory_max`, and `memory_high`, each paired
with an explicit `*_available` boolean. When an interface cannot be read, its value is
omitted, the boolean is `false`, and `*_unavailable_reason` contains one bounded reason.
Clients must not interpret an empty or absent value as an unlimited setting.

| Unavailable reason | Meaning | Operator action |
|--------------------|---------|-----------------|
| `not_present` | The interface or its managed cgroup does not exist. | Verify that the managed cgroup still exists. If the interface is required by the enabled configuration, repair/enable the corresponding `cpu` or `memory` controller in the delegated hierarchy and restart ResMan; an interface for a disabled feature may legitimately be absent. |
| `permission_denied` | The daemon cannot read an existing interface. | Restore the documented root/delegation model and cgroup mount permissions; do not make individual interface files world-readable. |
| `read_error` | The read failed for another bounded class, such as a transient kernel or cgroup-filesystem error. | Inspect the ResMan journal and kernel log, verify that the managed cgroup still exists, and investigate persistent cgroup-filesystem failures. |

The reason never contains the attempted interface path or a raw error string. The
existing `path` field continues to identify the managed cgroup itself.

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

# Allow operator control tools and revision-bound configuration editor writes
MCP_ALLOW_WRITE_OPS=false

# Full-privilege operator token required for HTTP
# MCP_AUTH_TOKEN=your-secret-token

# Distinct least-privilege editor token, required when HTTP writes are enabled
# MCP_EDITOR_AUTH_TOKEN=your-independent-editor-token
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

By default, write operations are **disabled**. Enabling them exposes manual control
tools to the operator credential and allows the independently scoped editor
credential to submit revision-bound configuration changes. The editor credential
cannot invoke manual activation, deactivation, or user-filter setter tools.

Enable with:
```bash
MCP_ALLOW_WRITE_OPS=true
```

### Authentication

HTTP transport requires token-based authentication:

```bash
MCP_AUTH_TOKEN=your-secret-token
MCP_EDITOR_AUTH_TOKEN=your-distinct-editor-token # Required with HTTP writes
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
  "total_cpu_usage_available": true,
  "total_cpu_usage_unavailable_reason": "",
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

When `total_cpu_usage_available` is false, `total_cpu_usage` is not a measured zero
and must not be interpreted as one. The bounded reason identifies a missing baseline,
read failure, stale baseline, counter reset, or zero-delta sample.

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
- Legacy backups matching the exact generated name
  `<config>.backup_YYYYMMDD_HHMMSS`, and the obsolete exact `<config>.tmp` file, are
  removed on the next update after the secure rolling backup has been created.
  Operator-named files that merely start with `.backup_` are preserved. Cleanup emits
  one warning with the total count, at most three removed basenames, and the number
  omitted; it never includes file contents, configuration values, or the full path.
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
