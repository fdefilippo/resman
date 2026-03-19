# MCP Server for CPU Manager Go

This document describes the Model Context Protocol (MCP) server implementation for CPU Manager Go.

## Overview

The MCP server exposes CPU Manager Go functionality to AI assistants and MCP-compatible clients, allowing them to:
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

- `cpu-manager://system/status` - Real-time system status
- `cpu-manager://users/active` - List of active users
- `cpu-manager://limits/status` - Current limits status
- `cpu-manager://config` - Current configuration
- `cpu-manager://users/{uid}/metrics` - Per-user metrics
- `cpu-manager://cgroups/{uid}` - Cgroup information

### Prompts (3 pre-built queries)

- `system-health` - Quick system health check with assessment
- `user-analysis` - Analyze resource usage by user (table format)
- `troubleshooting` - Diagnose CPU limit issues

## Configuration

Add to `/etc/cpu-manager.conf`:

```bash
# Enable MCP server
MCP_ENABLED=true

# Transport: stdio, http, sse
MCP_TRANSPORT=stdio

# HTTP/SSE settings (only for http/sse transport)
# MCP_HTTP_HOST=0.0.0.0      # Default: all interfaces (0.0.0.0)
# MCP_HTTP_PORT=1969         # Default: 1969
# MCP endpoint: http://HOST:PORT/mcp

# Log level
MCP_LOG_LEVEL=INFO

# Allow write operations (activate/deactivate limits)
# WARNING: Enable only if you trust all MCP clients
MCP_ALLOW_WRITE_OPS=false

# Optional authentication token for HTTP/SSE
# MCP_AUTH_TOKEN=your-secret-token
```

## Usage

### With stdio transport (recommended for local AI assistants)

1. Enable MCP in configuration:
```bash
MCP_ENABLED=true
MCP_TRANSPORT=stdio
```

2. Start CPU Manager:
```bash
sudo systemctl start cpu-manager
```

3. Configure your MCP client (e.g., Claude Desktop) to use the cpu-manager binary as an MCP server.

### With HTTP transport

1. Configure HTTP transport:
```bash
MCP_ENABLED=true
MCP_TRANSPORT=http
MCP_HTTP_HOST=127.0.0.1
MCP_HTTP_PORT=8080
```

2. Access endpoints:
- `http://127.0.0.1:8080/mcp` - MCP endpoint
- `http://127.0.0.1:8080/health` - Health check

### Example: Claude Desktop Configuration

Add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "cpu-manager": {
      "command": "/usr/bin/cpu-manager",
      "args": ["--config", "/etc/cpu-manager.conf"],
      "env": {
        "MCP_ENABLED": "true",
        "MCP_TRANSPORT": "stdio"
      }
    }
  }
}
```

**Note:** The current implementation requires running CPU Manager as a daemon. For Claude Desktop integration, you may want to create a separate MCP server binary or use HTTP transport.

### Example: Using HTTP Transport with curl

```bash
# Health check
curl http://127.0.0.1:8080/health

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
User: "Genera un report CPU"
AI: [Calls get_cpu_report tool]
AI: Returns formatted report:
```

**Example CPU Report Output:**
```
Report Utilizzo CPU
Hostname: server-web01
Data: 2026-03-11 18:45:00
Totale CPU disponibile: 400.0%
Utilizzo attuale: 45.2%

Utenti Attivi:
francesco
    Utilizzo CPU: 12.5%
    Limiti: Attivi
www-data
    Utilizzo CPU: 8.2%
    Limiti: Non attivi

Stato delle Risorse:
Media Utilizzo CPU: 6.9%
Picco Utilizzo CPU: 12.5%
Limiti CPU: Attivi
Utenti limitati: 1 su 2
```

### Query 5: Generate Memory Report ⭐ NEW
```
User: "Genera un report memoria"
AI: [Calls get_mem_report tool]
```

**Example Memory Report Output:**
```
Report Utilizzo Memoria
Hostname: server-web01
Data: 2026-03-11 18:45:00
Memoria Totale di Sistema: 2048.5 MB

Utenti Attivi:
francesco
    Memoria: 512.3 MB (537231360 bytes)
    Processi: 15
    Limiti: Attivi

Stato delle Risorse:
Media Utilizzo Memoria: 256.1 MB
Picco Utilizzo Memoria: 512.3 MB
Limiti CPU: Attivi
Utenti limitati: 1 su 1
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

For HTTP/SSE transport, you can enable token-based authentication:

```bash
MCP_AUTH_TOKEN=your-secret-token
```

Clients must then include:
```
Authorization: Bearer your-secret-token
```

### Network Exposure

**WARNING:** The MCP server is designed for **local access only**. Do not expose it to untrusted networks without additional security measures:
- Use firewall rules to restrict access
- Enable TLS for HTTPS (future feature)
- Use strong authentication tokens

## Testing

Run unit tests:
```bash
go test ./mcp/... -v
```

## Troubleshooting

### MCP server not starting

1. Check logs: `journalctl -u cpu-manager -f`
2. Verify configuration: `MCP_ENABLED=true`
3. Check port availability (for HTTP transport): `netstat -tlnp | grep 8080`

### Tools not available

1. Verify `MCP_ENABLED=true` in configuration
2. Check that CPU Manager started successfully
3. Ensure MCP server initialized without errors

### Permission errors

The MCP server runs with the same permissions as CPU Manager. Ensure:
- CPU Manager runs as root (required for cgroup access)
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
│  │  - cpu-manager://system/status  │    │
│  │  - cpu-manager://users/{uid}    │    │
│  └─────────────────────────────────┘    │
└────────────┬────────────────────────────┘
             │
┌────────────▼────────────────────────────┐
│      CPU Manager Go Components          │
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
  "user_cpu_usage": 12.3,
  "memory_usage_mb": 2345.6,
  "active_users_count": 5,
  "total_cores": 8,
  "system_under_load": false,
  "limits_active": true,
  "limits_applied_time": "2026-03-11T14:30:00Z",
  "shared_cgroup_active": true
}
```

### Tool: get_cpu_report ⭐ NEW

**Input:** None

**Output:** Text report with structured data
```
Report Utilizzo CPU
Hostname: server-web01
Data: 2026-03-11 18:45:00
Totale CPU disponibile: 400.0%
Utilizzo attuale: 45.2%

Utenti Attivi:
francesco
    Utilizzo CPU: 12.5%
    Limiti: Attivi

Stato delle Risorse:
Media Utilizzo CPU: 6.9%
Picco Utilizzo CPU: 12.5%
Limiti CPU: Attivi
Utenti limitati: 1 su 2
```

### Tool: get_mem_report ⭐ NEW

**Input:** None

**Output:** Text report with structured data
```
Report Utilizzo Memoria
Hostname: server-web01
Data: 2026-03-11 18:45:00
Memoria Totale di Sistema: 2048.5 MB

Utenti Attivi:
francesco
    Memoria: 512.3 MB (537231360 bytes)
    Processi: 15
    Limiti: Attivi

Stato delle Risorse:
Media Utilizzo Memoria: 256.1 MB
Picco Utilizzo Memoria: 512.3 MB
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

Ottiene le configurazioni correnti dei filtri utente.

**Input:** Nessuno

**Output:**
```json
{
  "user_include_list": ["^www.*", "^app-.*"],
  "user_exclude_list": ["^test-.*", "francesco"],
  "config_file": "/etc/cpu-manager.conf"
}
```

### Tool: set_user_exclude_list

Imposta la lista di utenti da escludere dai limiti CPU (supporta regex).

**Input:**
```json
{
  "patterns": ["^test-.*", "^dev-.*", "francesco"],
  "reload": true
}
```

**Parametri:**
- `patterns` (array di stringhe): Lista di pattern regex per utenti da escludere
- `reload` (boolean, opzionale, default=true): Se true, ricarica automaticamente la configurazione

**Output:**
```json
{
  "success": true,
  "message": "User exclude list updated successfully",
  "previous_value": ["^old-.*"],
  "new_value": ["^test-.*", "^dev-.*", "francesco"],
  "reload_triggered": true
}
```

**Backup Automatico:**
- Prima di ogni modifica, viene creato un backup: `/etc/cpu-manager.conf.backup_YYYYMMDD_HHMMSS`
- In caso di errore, la configurazione viene ripristinata automaticamente

### Tool: set_user_include_list

Imposta la lista di pattern per includere utenti nel monitoraggio (supporta regex).

**Input:**
```json
{
  "patterns": ["^www.*", "^app-.*", "mysql"],
  "reload": true
}
```

**Parametri:**
- `patterns` (array di stringhe): Lista di pattern regex per utenti da includere
- `reload` (boolean, opzionale, default=true): Se true, ricarica automaticamente la configurazione

**Output:**
```json
{
  "success": true,
  "message": "User include list updated successfully",
  "previous_value": [],
  "new_value": ["^www.*", "^app-.*", "mysql"],
  "reload_triggered": true
}
```

### Tool: validate_user_filter_pattern

Valida se un pattern regex è valido e mostra esempi di match.

**Input:**
```json
{
  "pattern": "^www.*",
  "type": "exclude"
}
```

**Parametri:**
- `pattern` (string): Pattern regex da validare (richiesto)
- `type` (string, opzionale): Tipo di filtro - "include" o "exclude"

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

**Utenti di Test:**
Il tool testa il pattern contro questi utenti di esempio:
- francesco, www-data, mysql, nobody, root
- test-user, dev-web, app-prod, svc-db, admin

## Future Enhancements

- [ ] WebSocket transport
- [ ] Real-time metrics streaming
- [ ] Enhanced authentication (OAuth2, mTLS)
- [ ] Audit logging for write operations
- [ ] Rate limiting
- [ ] Custom resource templates
- [ ] Notification support

## References

- [MCP Specification](https://modelcontextprotocol.io/)
- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [CPU Manager README](../README.md)
