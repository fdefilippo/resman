# MCP Server Blueprint for ResMan

> **Archived design note.** This document records the original MCP integration
> plan and is not the runtime protocol contract. The current implementation is
> documented in [MCP-README.md](MCP-README.md) and supports only stateless MCP
> revision 2026-07-28 through go-sdk v1.7.0 or newer.

## Overview

This document outlines the blueprint for exposing ResMan functionality via a **Model Context Protocol (MCP)** server, enabling AI assistants to query and interact with the resource management system.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         ResMan                                   │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────────┐  │
│  │   Cgroup    │  │   Metrics   │  │      State Manager      │  │
│  │   Manager   │  │  Collector  │  │  (Control Logic)        │  │
│  └─────────────┘  └─────────────┘  └─────────────────────────┘  │
│         │                │                      │                │
│         └────────────────┼──────────────────────┘                │
│                          │                                       │
│              ┌───────────▼───────────┐                           │
│              │    MCP Server Layer   │                           │
│              │   (New Package: mcp)  │                           │
│              └───────────┬───────────┘                           │
│                          │                                       │
│         ┌────────────────┼────────────────┐                      │
│         │                │                │                      │
│    Stdio Transport  HTTP/SSE Transport   │                      │
│         │                │                │                      │
└─────────┼────────────────┼────────────────┼──────────────────────┘
          │                │                │
          ▼                ▼                ▼
    AI Assistant    MCP Clients     External Tools
```

## Package Structure

```
mcp/
├── server.go          # MCP server initialization and lifecycle
├── handlers.go        # Request handlers for MCP tools/resources
├── tools.go           # Tool definitions for MCP
├── resources.go       # Resource definitions for MCP
├── config.go          # MCP-specific configuration
└── server_test.go     # Unit tests
```

## MCP Features to Expose

### 1. Tools (Actions AI can request)

The complete production contract for tools, fixed resources, resource URI templates,
and prompts is maintained in the
[authoritative discovery inventories](MCP-README.md#discovery-inventories). Those
inventories are verified against production discovery; this blueprint does not maintain
parallel lists.

### 2. Resources (Data AI can read)

The cgroup tool and resource share one underscore-named JSON schema and expose
availability separately from each raw interface value. An unreadable interface is
absent with its corresponding `*_available` field set to `false` and a bounded
`*_unavailable_reason` of `not_present`, `permission_denied`, or `read_error`.
An empty string is never an alias for `max`; the bounded reason contains neither the
attempted interface path nor raw error text. The existing `path` field still identifies
the managed cgroup. Operator remedies are maintained in
[MCP-README](MCP-README.md#cgroup-interface-availability).

### 3. Prompts (Pre-built queries for AI)

The registered prompts are part of the authoritative discovery inventories linked
above.

## Implementation Details

### Dependencies

Add to `go.mod`:
```go
github.com/modelcontextprotocol/go-sdk v0.1.0  // or latest
```

### Server Configuration

Add to `/etc/resman/resman.conf`:
```bash
# MCP Server Configuration
MCP_ENABLED=true
MCP_TRANSPORT=stdio        # stdio, http, or sse
MCP_HTTP_PORT=1969         # For HTTP transport
MCP_HTTP_HOST=127.0.0.1    # Bind address
MCP_TLS_ENABLED=true       # Mandatory for HTTP transport
MCP_TLS_CERT_FILE=/etc/resman/tls/server.crt
MCP_TLS_KEY_FILE=/etc/resman/tls/server.key
# MCP_TLS_CA_FILE=/etc/resman/tls/ca.crt # Enables mandatory client certificates
MCP_TLS_MIN_VERSION=1.3
MCP_LOG_LEVEL=INFO         # MCP-specific log level
```

### Core Implementation

#### 1. `mcp/server.go`

```go
package mcp

import (
    "context"
    "github.com/modelcontextprotocol/go-sdk/mcp"
    "github.com/fdefilippo/resman-go/state"
    "github.com/fdefilippo/resman-go/metrics"
    "github.com/fdefilippo/resman-go/cgroup"
    "github.com/fdefilippo/resman-go/config"
)

type Server struct {
    mcpServer      *mcp.Server
    cfg            *config.Config
    stateManager   *state.Manager
    metricsCollector *metrics.Collector
    cgroupManager  *cgroup.Manager
    logger         *logging.Logger
}

func NewServer(cfg *config.Config, sm *state.Manager, mc *metrics.Collector, cg *cgroup.Manager) (*Server, error)
func (s *Server) Start(ctx context.Context) error
func (s *Server) Stop() error
```

#### 2. `mcp/tools.go`

Define all tools with schemas:

```go
func (s *Server) registerTools() {
    // Example: get_system_status tool
    s.mcpServer.AddTool(mcp.Tool{
        Name:        "get_system_status",
        Description: "Get current CPU and memory status of the system",
        InputSchema: mcp.Schema{Type: "object"},
    }, s.handleGetSystemStatus)
    
    // Register other tools...
}
```

#### 3. `mcp/handlers.go`

Implement handlers that delegate to existing components:

```go
func (s *Server) handleGetSystemStatus(ctx context.Context, req mcp.Request) (mcp.Response, error) {
    status := s.stateManager.GetStatus()
    observation := s.metricsCollector.GetObservationMetrics()
    
    return mcp.Response{
        Content: map[string]interface{}{
            "total_cpu_usage":                 observation.TotalCPUUsage,
            "total_cpu_usage_available":       observation.TotalCPUUsageAvailable,
            "total_cpu_usage_unavailable_reason": observation.TotalCPUUsageUnavailableReason,
            "observed_users_cpu_usage":        observation.ObservedUsersCPUUsage,
            "memory_usage_mb":                 observation.MemoryUsageMB,
            "observed_users_count":            observation.ObservedUsersCount,
            "actively_limited_users_count":    status.ActivelyLimitedUsersCount,
            "cpu_limits_active":               status.CPULimitsActive,
            "resource_limits_active":          status.ResourceLimitsActive,
        },
    }, nil
}
```

#### 4. `mcp/resources.go`

Define resources:

```go
func (s *Server) registerResources() {
    s.mcpServer.AddResource(mcp.Resource{
        URI:         "resman://system/status",
        Name:        "System Status",
        Description: "Real-time system CPU and memory status",
        MimeType:    "application/json",
    }, s.handleSystemStatusResource)
    
    // Other resources...
}
```

### Integration with main.go

Add MCP server initialization after existing components:

```go
// 6. MCP Server
var mcpServer *mcp.Server

if cfg.MCPEnabled {
    mcpServer, err = mcp.NewServer(cfg, stateManager, metricsCollector, cgroupMgr)
    if err != nil {
        logger.Error("Failed to initialize MCP server", "error", err)
        // Continue without MCP
    } else {
        if err := mcpServer.Start(ctx); err != nil {
            logger.Error("Failed to start MCP server", "error", err)
        } else {
            logger.Info("MCP server started",
                "transport", cfg.MCPTransport,
                "port", cfg.MCPHTTPPort,
            )
        }
    }
}
```

### Configuration Changes

Extend `config/config.go`:

```go
type Config struct {
    // ... existing fields ...
    
    // MCP Server
    MCPEnabled    bool   `config:"MCP_ENABLED"`
    MCPTransport  string `config:"MCP_TRANSPORT"`  // stdio, http, sse
    MCPHTTPPort   int    `config:"MCP_HTTP_PORT"`
    MCPHTTPHost   string `config:"MCP_HTTP_HOST"`
    MCPTLSEnabled bool   `config:"MCP_TLS_ENABLED"`
    MCPTLSCertFile string `config:"MCP_TLS_CERT_FILE"`
    MCPTLSKeyFile string `config:"MCP_TLS_KEY_FILE"`
    MCPTLSCAFile string `config:"MCP_TLS_CA_FILE"`
    MCPTLSMinVersion string `config:"MCP_TLS_MIN_VERSION"`
    MCPLogLevel   string `config:"MCP_LOG_LEVEL"`
}
```

## Security Considerations

1. **Authentication**: Consider adding token-based auth for HTTP/SSE transport
2. **Authorization**: Limit write operations (activate/deactivate limits) to authorized clients
3. **Rate Limiting**: Prevent abuse of MCP endpoints
4. **Audit Logging**: Log all MCP tool invocations

## Testing Strategy

1. **Unit Tests**: Test each handler in isolation
2. **Integration Tests**: Test MCP server with mock state/metrics
3. **E2E Tests**: Test with real MCP clients (Claude Desktop, etc.)

## Example MCP Client Usage

### Using Claude Desktop

Configure `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "resman": {
      "command": "/usr/bin/resman",
      "args": ["--config", "/etc/resman/resman.conf"]
    }
  }
}
```

### Example Query

```
User: "What's the current CPU usage and which users are most active?"

AI (via MCP): 
  1. Calls get_system_status tool
  2. Calls get_user_metrics tool
  3. Returns: "Total CPU usage is 45%. Top users: francesco (12%), www-data (8%)..."
```

## Implementation Phases

### Phase 1: Core MCP Server ✅ COMPLETED
- [x] Basic server setup with stdio transport
- [x] Read-only tools (get_system_status, get_user_metrics, get_active_users, get_limits_status, get_cgroup_info, get_configuration, get_control_history)
- [x] Write operation tools (activate_limits, deactivate_limits) - with authorization flag
- [x] Resource definitions
- [x] Prompts implemented and covered by the discovery-inventory contract
- [x] Unit tests
- [x] Configuration integration

### Phase 2: HTTP/SSE Transport ✅ COMPLETED
- [x] HTTP transport implementation with `mcp.NewStreamableHTTPHandler`
- [x] Health check endpoint
- [x] Logging middleware for all HTTP requests

### Phase 3: Documentation & Examples ✅ COMPLETED
- [x] Blueprint document
- [x] Example configuration
- [x] Usage examples with report generation
- [x] Hostname in all metric outputs

### Phase 4: Reporting Tools ✅ COMPLETED
- [x] `get_cpu_report` - Comprehensive CPU usage report
- [x] `get_mem_report` - Comprehensive memory usage report
- [x] Formatted text output with structured data
- [x] Hostname identification for multi-server environments

### Phase 5: User Filter Management ✅ COMPLETED (v1.11.0)
- [x] `get_user_filters` - Get current filter configurations
- [x] `set_user_exclude_list` - Set exclude list with regex support
- [x] `set_user_include_list` - Set include list with regex support
- [x] `validate_user_filter_pattern` - Validate regex patterns
- [x] Secure bounded rolling configuration backup
- [x] Atomic save with rollback on error
- [x] Synchronous config reload acknowledgement with explicit failure/timeout

### Phase 6: Advanced Features (Future)
- [ ] Real-time notifications
- [ ] Metrics streaming
- [ ] Enhanced authorization
- [ ] Audit logging
- [ ] WebSocket transport

## File Changes Summary

| File | Action | Description |
|------|--------|-------------|
| `mcp/server.go` | ✅ Created | Main MCP server implementation with HTTP logging middleware |
| `mcp/tools.go` | ✅ Created | Tool definitions and handlers; inventory verified against MCP-README |
| `mcp/resources.go` | ✅ Created | Resource definitions and handlers |
| `mcp/config.go` | ✅ Created | MCP configuration |
| `mcp/server_test.go` | ✅ Created | Unit tests |
| `config/config.go` | ✅ Modified | Added MCP config fields, detached user-filter persistence, and the secure backup mechanism |
| `config/resman.conf.example` | ✅ Modified | Added MCP example config, USER_INCLUDE_LIST, USER_EXCLUDE_LIST |
| `state/manager.go` | ✅ Modified | Added GetConfig, GetControlHistory methods |
| `main.go` | ✅ Modified | Initialize MCP server, fixed logger initialization |
| `go.mod` | ✅ Modified | Added MCP SDK dependency |
| `docs/MCP-BLUEPRINT.md` | ✅ Created | Architecture and implementation plan |
| `docs/MCP-README.md` | ✅ Created | Usage guide with examples (updated with user filter tools) |
| `docs/TECHNICAL-SPECIFICATION.md` | ✅ Modified | Added user filter management documentation |
| `CHANGELOG.md` | ✅ Modified | Versions 1.3.0-1.11.0 with MCP features and user filter management |
| `README.md` | ✅ Modified | Added MCP server section |

## References

- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [MCP Specification](https://modelcontextprotocol.io/)
- [Existing ResMan Documentation](../README.md)
