# AGENTS.md — resman

## Project Overview

resman is a Linux resource manager (Go 1.25.7) that monitors and limits CPU/memory
per user via cgroups v2. It exposes Prometheus metrics, supports hot-reload config,
an MCP server, and SQLite metrics persistence.

Module: `github.com/fdefilippo/resman`

## Build / Test / Lint Commands

```bash
# Build (CGO_ENABLED=1 required for NSS/LDAP user resolution)
go build -ldflags="-s -w -X 'main.version=...'" -o resman

# Run all tests
go test -v ./...

# Run a single test
go test -v -run TestMakeDecision ./state/
go test -v -run TestGetTotalCores ./metrics/

# Run tests with coverage
go test -v -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Lint (golangci-lint if available, otherwise go vet)
golangci-lint run          # preferred
go vet ./...               # fallback

# Format
go fmt ./...

# Dependencies
go mod tidy && go mod verify

# Makefile shortcuts
make build    # local build
make test     # all tests
make lint     # lint
make fmt      # format
make clean    # clean artifacts
```

## Architecture

```
main.go              — entry point, wires all components
config/              — config loading from file + env vars, hot-reload support
cgroup/              — cgroups v2 management (create, apply limits, cleanup)
metrics/             — system metrics collection, Prometheus exporter, SQLite writer
state/               — decision engine, control cycle logic
database/            — SQLite manager
mcp/                 — MCP server (stdio/HTTP)
reloader/            — hot-reload handler
logging/             — structured logger (file/syslog)
packaging/           — systemd unit, Dockerfile, RPM/DEB specs
docs/                — Prometheus configs, Grafana dashboard, alerting rules
```

## Code Style

### Imports

Group stdlib first, then third-party. No blank line separator required but keep
the order. Use full module paths for internal packages:

```go
import (
    "fmt"
    "os"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/fdefilippo/resman/config"
)
```

### Naming

- Receivers: single lowercase letter (`c` for Collector, `m` for Manager, `cfg` for Config)
- Public functions: CamelCase, descriptive — `GetTotalCores`, `IsUserIncluded`
- Acronyms stay uppercase: `GetUID`, `SystemUIDMax`
- Constants: `DEFAULT_USERNAME_CACHE_TTL` style for config keys; Go CamelCase for code constants
- Variables: short but clear (`uid`, `err`, `metrics`)

### Comments

Comments must be in English. Use godoc style: start with the function/type name.

```go
// GetMemoryUsage restituisce l'uso della memoria in MB.
func (c *Collector) GetMemoryUsage() float64 {
```

### Error Handling

Always wrap errors with context using `fmt.Errorf` + `%w`:

```go
if err != nil {
    return fmt.Errorf("failed to create shared cgroup (min_system_cores=%d): %w", minCores, err)
}
```

Use early returns on error. Never silently discard errors.

### Struct Tags

Custom config tags for env var mapping: `config:"MIN_SYSTEM_CORES"`
Exclude fields: `config:"-"`
JSON tags where needed: `json:"timestamp"`

### Prometheus Metrics

All metrics use namespace `resman`. Register via `promauto.With(registry)`.
Update through the `UpdateMetrics(map[string]float64)` or dedicated
`UpdateSystemMetrics()` / `UpdateUserMetrics()` methods on the exporter.

## Testing

- Test files live in the same package as the code under test
- Use table-driven tests with `t.Run()` subtests
- Create mock implementations as local struct types (e.g., `mockMetricsCollector`)
- Assert with `t.Errorf` / `t.Fatalf`
- After changes to interfaces, update all mock implementations in test files

## Key Patterns

- **Metrics flow**: `Collector.CollectMetrics()` → map → `PrometheusExporter.UpdateMetrics()`
- **Prometheus metrics via struct fields**: add field to `SystemMetrics`, populate in
  control cycle, include in `promMetrics` map inside `updatePrometheusMetrics()`
- **Config hot-reload**: use thread-safe getters (`GetMinSystemCores()`) with `sync.RWMutex`
- **CGO required**: `CGO_ENABLED=1` for user name resolution via NSS (LDAP, NIS, SSSD)

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Commit all changes** - All work must be committed locally
5. **Clean up** - Clear stashes, prune local branches
6. **Verify** - `git status` MUST show clean working tree
7. **Hand off** - Provide context for next session
