# IO Limits Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add block I/O limiting via cgroups v2 `io` controller, following the same patterns as existing CPU and RAM limits.

**Architecture:** Extend the existing cgroup manager with IO functions, add configuration fields, integrate into the control cycle (activateLimits/deactivateLimits), and expose Prometheus metrics for IO bandwidth and IOPS per user.

**Tech Stack:** Go 1.25.7, cgroups v2 (`io` controller), Prometheus client library.

---

## Background: cgroups v2 `io` Controller

The `io` controller provides block I/O bandwidth and IOPS limiting per cgroup.

### Key Files

| File | Purpose |
|------|---------|
| `io.max` | Hard limits: `rbps`, `wbps`, `riops`, `wiops` per device |
| `io.weight` | Relative weight for IO scheduling (1-10000, default 100) |
| `io.stat` | Read-only statistics: `rios`, `wios`, `rbytes`, `wbytes` per device |
| `io.pressure` | Pressure Stall Information (PSI) for IO |

### `io.max` Format

```
<major>:<minor> rbps=<bytes_per_sec> wbps=<bytes_per_sec> riops=<iops> wiops=<iops>
```

Example:
```
8:0 rbps=104857600 wbps=52428800 riops=1000 wiops=500
```

This limits device 8:0 (first SCSI/SATA disk) to 100MB/s read, 50MB/s write, 1000 read IOPS, 500 write IOPS.

### `io.stat` Format

```
8:0 rios=1234 wios=567 rbytes=104857600 wbytes=52428800
```

### Enabling the Controller

Write `+io` to `cgroup.subtree_control` at root and base cgroup levels (same pattern as `+cpu`).

---

## File Structure

| File | Responsibility |
|------|----------------|
| `config/config.go` | New IO configuration fields and validation |
| `config/resman.conf.example` | Document new IO directives |
| `cgroup/manager.go` | IO controller enable, limit apply/remove, stats reading |
| `state/manager.go` | Extend `CgroupManager` interface, integrate IO into activate/deactivate |
| `state/manager_test.go` | Update mock implementations |
| `metrics/collector.go` | Add `IOReadBytes`, `IOWriteBytes`, `IOReadOps`, `IOWriteOps` to `UserMetrics` |
| `metrics/prometheus.go` | New Prometheus counters/gauges for IO metrics |
| `docs/IO-LIMITS.md` | User-facing documentation |
| `CHANGELOG.md` | Release notes |

---

## Task 1: Configuration

### Files:
- Modify: `config/config.go`
- Modify: `config/resman.conf.example`

### Step 1.1: Add IO fields to Config struct

In `config/config.go`, add after the RAM section (after line 85):

```go
// IO limits (block I/O via cgroups v2 io controller)
IOEnabled          bool   `config:"IO_LIMIT_ENABLED"`
IOThreshold        int    `config:"IO_THRESHOLD"`
IOReleaseThreshold int    `config:"IO_RELEASE_THRESHOLD"`
IOReadBPS          string `config:"IO_READ_BPS"`   // Read bandwidth limit (e.g., "100M", "1G")
IOWriteBPS         string `config:"IO_WRITE_BPS"`   // Write bandwidth limit (e.g., "50M", "500M")
IOReadIOPS         int    `config:"IO_READ_IOPS"`   // Read IOPS limit (0 = unlimited)
IOWriteIOPS        int    `config:"IO_WRITE_IOPS"`  // Write IOPS limit (0 = unlimited)
IODeviceFilter     string `config:"IO_DEVICE_FILTER"` // Device filter: "all" or "major:minor" (default "all")

// IO user filtering
IOUserIncludeList []string `config:"IO_USER_INCLUDE_LIST"`
IOUserExcludeList []string `config:"IO_USER_EXCLUDE_LIST"`
```

### Step 1.2: Add defaults in DefaultConfig()

In `config/config.go` DefaultConfig(), add after RAM defaults:

```go
// IO limits
IOEnabled:          false,
IOThreshold:        75,
IOReleaseThreshold: 40,
IOReadBPS:          "100M",   // 100 MB/s
IOWriteBPS:         "50M",    // 50 MB/s
IOReadIOPS:         1000,
IOWriteIOPS:        500,
IODeviceFilter:     "all",
IOUserIncludeList:  nil,
IOUserExcludeList:  nil,
```

### Step 1.3: Add validation in validateConfig()

In `config/config.go` validateConfig(), add after RAM validation:

```go
// Validate IO limits
if cfg.IOEnabled {
    if cfg.IOThreshold < 1 || cfg.IOThreshold > 100 {
        errors = append(errors, "IO_THRESHOLD must be between 1 and 100")
    }
    if cfg.IOReleaseThreshold < 1 || cfg.IOReleaseThreshold > 100 {
        errors = append(errors, "IO_RELEASE_THRESHOLD must be between 1 and 100")
    }
    if cfg.IOThreshold <= cfg.IOReleaseThreshold {
        errors = append(errors, "IO_THRESHOLD must be greater than IO_RELEASE_THRESHOLD")
    }
    if cfg.IOReadBPS != "" && cfg.IOReadBPS != "max" {
        if _, err := parseIOQuota(cfg.IOReadBPS); err != nil {
            errors = append(errors, "IO_READ_BPS must be a valid byte value (e.g., '104857600', '100M', '1G')")
        }
    }
    if cfg.IOWriteBPS != "" && cfg.IOWriteBPS != "max" {
        if _, err := parseIOQuota(cfg.IOWriteBPS); err != nil {
            errors = append(errors, "IO_WRITE_BPS must be a valid byte value (e.g., '52428800', '50M', '500M')")
        }
    }
    if cfg.IOReadIOPS < 0 {
        errors = append(errors, "IO_READ_IOPS must be >= 0 (0 = unlimited)")
    }
    if cfg.IOWriteIOPS < 0 {
        errors = append(errors, "IO_WRITE_IOPS must be >= 0 (0 = unlimited)")
    }
}
```

### Step 1.4: Add parseIOQuota helper

In `config/config.go`, add a helper function (similar to `ParseRAMQuota`):

```go
// parseIOQuota converte una stringa di quota IO in bytes per secondo.
// Supporta suffissi: K, M, G, T (base 1024).
func parseIOQuota(s string) (uint64, error) {
    return ParseRAMQuota(s) // Reuse existing parser
}
```

### Step 1.5: Add getter methods

Add thread-safe getters:

```go
func (c *Config) GetIOEnabled() bool {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.IOEnabled
}

func (c *Config) GetIOReadBPS() string {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.IOReadBPS
}

func (c *Config) GetIOWriteBPS() string {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.IOWriteBPS
}

func (c *Config) GetIOReadIOPS() int {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.IOReadIOPS
}

func (c *Config) GetIOWriteIOPS() int {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.IOWriteIOPS
}

func (c *Config) GetIODeviceFilter() string {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.IODeviceFilter
}
```

### Step 1.6: Update resman.conf.example

Add IO section after RAM section:

```bash
# ========================
# IO LIMITS [D]
# ========================
# Abilita/disabilita limitazione IO tramite cgroups v2 io controller
# IO_LIMIT_ENABLED: true = abilita limiti IO, false = disabilita
#
# IO_THRESHOLD: Percentuale di IO utenti totali per attivare i limiti
# IO_RELEASE_THRESHOLD: Percentuale per rilasciare i limiti
#
# IO_READ_BPS: Limite banda lettura per utente (bytes/s con suffix K/M/G/T)
# IO_WRITE_BPS: Limite banda scrittura per utente
# IO_READ_IOPS: Limite IOPS lettura per utente (0 = illimitato)
# IO_WRITE_IOPS: Limite IOPS scrittura per utente (0 = illimitato)
#
# IO_DEVICE_FILTER: "all" = applica a tutti i dispositivi, oppure "8:0" per device specifico
#
IO_LIMIT_ENABLED=false
IO_THRESHOLD=75
IO_RELEASE_THRESHOLD=40
IO_READ_BPS=100M
IO_WRITE_BPS=50M
IO_READ_IOPS=1000
IO_WRITE_IOPS=500
IO_DEVICE_FILTER=all
```

- [ ] Step 1.1: Add IO fields to Config struct
- [ ] Step 1.2: Add defaults in DefaultConfig()
- [ ] Step 1.3: Add validation in validateConfig()
- [ ] Step 1.4: Add parseIOQuota helper
- [ ] Step 1.5: Add getter methods
- [ ] Step 1.6: Update resman.conf.example
- [ ] Step 1.7: Run `go vet ./config/` and `go test ./config/`

---

## Task 2: Cgroup Manager — IO Controller

### Files:
- Modify: `cgroup/manager.go`

### Step 2.1: Enable `io` controller in verifyCgroupSetup()

In `cgroup/manager.go`, modify `verifyCgroupSetup()` to also enable the `io` controller:

After the existing `writeControllerIfMissing` calls for `+cpu` and `+cpuset`, add:

```go
// Enable io controller for block I/O limits
if err := m.writeControllerIfMissing(filepath.Join(basePath, "cgroup.subtree_control"), "+io"); err != nil {
    m.logger.Warn("Failed to enable io controller (IO limits will not work)", "error", err)
}
```

Also add `+io` in `enableCPUControllers()` (rename to `enableControllers()` or add alongside):

```go
// Enable io controller
if err := m.writeControllerIfMissing(filepath.Join(m.cfg.CgroupRoot, "cgroup.subtree_control"), "+io"); err != nil {
    m.logger.Warn("Failed to enable io controller", "error", err)
}
```

### Step 2.2: Add ApplyIOLimit function

```go
// ApplyIOLimit applica limiti di IO (bandwidth e IOPS) a un cgroup utente.
// Scrive nel file io.max del cgroup.
// readBPS, writeBPS: bytes per secondo (stringa, es. "100M", "max")
// readIOPS, writeIOPS: operazioni per secondo (int, 0 = unlimited)
// deviceFilter: "all" per tutti i dispositivi, oppure "major:minor" (es. "8:0")
func (m *Manager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
    cgroupPath, exists := m.getCgroupPath(uid)
    if !exists {
        if err := m.CreateUserCgroup(uid); err != nil {
            return fmt.Errorf("failed to create cgroup before applying IO limit: %w", err)
        }
        cgroupPath, _ = m.getCgroupPath(uid)
    }

    ioMaxFile := filepath.Join(cgroupPath, "io.max")

    // Normalizza valori
    if readBPS == "" || readBPS == "0" {
        readBPS = "max"
    }
    if writeBPS == "" || writeBPS == "0" {
        writeBPS = "max"
    }
    readIOPSStr := "max"
    if readIOPS > 0 {
        readIOPSStr = strconv.Itoa(readIOPS)
    }
    writeIOPSStr := "max"
    if writeIOPS > 0 {
        writeIOPSStr = strconv.Itoa(writeIOPS)
    }

    // Device: "all" o "major:minor"
    device := "default"
    if deviceFilter != "" && deviceFilter != "all" {
        device = deviceFilter
    }

    // Formato: "major:minor rbps=X wbps=Y riops=Z wiops=W"
    value := fmt.Sprintf("%s rbps=%s wbps=%s riops=%s wiops=%s",
        device, readBPS, writeBPS, readIOPSStr, writeIOPSStr)

    if err := os.WriteFile(ioMaxFile, []byte(value), defaultFilePerm); err != nil {
        return fmt.Errorf("failed to apply IO limit for UID %d: %w", uid, err)
    }

    m.logger.Debug("IO limit applied",
        "uid", uid,
        "readBPS", readBPS,
        "writeBPS", writeBPS,
        "readIOPS", readIOPSStr,
        "writeIOPS", writeIOPSStr,
        "device", device,
        "path", ioMaxFile,
    )

    return nil
}
```

### Step 2.3: Add RemoveIOLimit function

```go
// RemoveIOLimit rimuove i limiti di IO (imposta tutti i valori a "max").
func (m *Manager) RemoveIOLimit(uid int) error {
    cgroupPath, exists := m.getCgroupPath(uid)
    if !exists {
        return fmt.Errorf("cgroup for UID %d not found", uid)
    }

    ioMaxFile := filepath.Join(cgroupPath, "io.max")
    return os.WriteFile(ioMaxFile, []byte("default rbps=max wbps=max riops=max wiops=max"), defaultFilePerm)
}
```

### Step 2.4: Add GetIOStats function

```go
// GetIOStats restituisce le statistiche di IO aggregate per tutti i dispositivi.
// Legge da io.stat e somma rbytes, wbytes, rios, wios.
func (m *Manager) GetIOStats(uid int) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error) {
    cgroupPath, exists := m.getCgroupPath(uid)
    if !exists {
        return 0, 0, 0, 0, fmt.Errorf("cgroup for UID %d not found", uid)
    }

    ioStatFile := filepath.Join(cgroupPath, "io.stat")
    data, err := os.ReadFile(ioStatFile)
    if err != nil {
        return 0, 0, 0, 0, fmt.Errorf("failed to read io.stat for UID %d: %w", uid, err)
    }

    // Parse lines like: "8:0 rios=1234 wios=567 rbytes=104857600 wbytes=52428800"
    for _, line := range strings.Split(string(data), "\n") {
        line = strings.TrimSpace(line)
        if line == "" {
            continue
        }
        // Skip device prefix (e.g., "8:0")
        parts := strings.Fields(line)
        for _, part := range parts {
            kv := strings.SplitN(part, "=", 2)
            if len(kv) != 2 {
                continue
            }
            val, err := strconv.ParseUint(kv[1], 10, 64)
            if err != nil {
                continue
            }
            switch kv[0] {
            case "rios":
                readOps += val
            case "wios":
                writeOps += val
            case "rbytes":
                readBytes += val
            case "wbytes":
                writeBytes += val
            }
        }
    }

    return readBytes, writeBytes, readOps, writeOps, nil
}
```

- [ ] Step 2.1: Enable `io` controller in verifyCgroupSetup()
- [ ] Step 2.2: Add ApplyIOLimit function
- [ ] Step 2.3: Add RemoveIOLimit function
- [ ] Step 2.4: Add GetIOStats function
- [ ] Step 2.5: Run `go vet ./cgroup/`

---

## Task 3: Extend CgroupManager Interface

### Files:
- Modify: `state/manager.go`
- Modify: `state/manager_test.go`

### Step 3.1: Add IO methods to CgroupManager interface

In `state/manager.go`, add to the `CgroupManager` interface:

```go
// IO limits
ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error
RemoveIOLimit(uid int) error
GetIOStats(uid int) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error)
```

### Step 3.2: Update mock in manager_test.go

In `state/manager_test.go`, add to `mockCgroupManager`:

```go
func (m *mockCgroupManager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
    return nil
}
func (m *mockCgroupManager) RemoveIOLimit(uid int) error { return nil }
func (m *mockCgroupManager) GetIOStats(uid int) (uint64, uint64, uint64, uint64, error) {
    return 0, 0, 0, 0, nil
}
```

- [ ] Step 3.1: Add IO methods to CgroupManager interface
- [ ] Step 3.2: Update mock in manager_test.go
- [ ] Step 3.3: Run `go vet ./state/` and `go test ./state/`

---

## Task 4: State Manager — Control Cycle Integration

### Files:
- Modify: `state/manager.go`

### Step 4.1: Add shouldApplyIOLimits helper

```go
// shouldApplyIOLimits verifica se i limiti IO devono essere applicati per l'utente.
func (m *Manager) shouldApplyIOLimits(uid int) bool {
    if !m.cfg.IOEnabled {
        return false
    }
    username := m.getUsername(uid)
    return m.cfg.IsUserIncluded(username) // Use same filter as CPU, or add IO-specific filter
}
```

### Step 4.2: Integrate IO in activateLimits()

In `activateLimits()`, after the RAM block (around line 686), add:

```go
// Applica limiti IO se abilitati
if m.shouldApplyIOLimits(uid) {
    readBPS := m.cfg.GetIOReadBPS()
    writeBPS := m.cfg.GetIOWriteBPS()
    readIOPS := m.cfg.GetIOReadIOPS()
    writeIOPS := m.cfg.GetIOWriteIOPS()
    deviceFilter := m.cfg.GetIODeviceFilter()

    if err := m.cgroupManager.ApplyIOLimit(uid, readBPS, writeBPS, readIOPS, writeIOPS, deviceFilter); err != nil {
        m.logger.Warn("Failed to apply IO limit for user",
            "uid", uid,
            "readBPS", readBPS,
            "writeBPS", writeBPS,
            "error", err,
        )
    } else {
        m.logger.Debug("IO limit applied for user",
            "uid", uid,
            "readBPS", readBPS,
            "writeBPS", writeBPS,
        )
    }
}
```

### Step 4.3: Integrate IO in deactivateLimits()

In `deactivateLimits()`, after the RAM removal block, add:

```go
// Rimuovi limiti IO se abilitati e l'utente era soggetto a IO limits
if m.shouldApplyIOLimits(uid) {
    if err := m.cgroupManager.RemoveIOLimit(uid); err != nil {
        m.logger.Warn("Failed to remove IO limit for user",
            "user", userStr,
            "error", err,
        )
    } else {
        m.logger.Debug("IO limit removed for user", "uid", uid)
    }
}
```

### Step 4.4: Collect IO metrics in updatePrometheusMetrics()

In `updatePrometheusMetrics()`, after the memoryHighEvents collection, add:

```go
// Ottieni statistiche IO
var ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64
if m.cgroupManager != nil {
    ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps, _ = m.cgroupManager.GetIOStats(uid)
}
```

Pass these values to `UpdateUserMetrics()` (will need to extend the signature).

- [ ] Step 4.1: Add shouldApplyIOLimits helper
- [ ] Step 4.2: Integrate IO in activateLimits()
- [ ] Step 4.3: Integrate IO in deactivateLimits()
- [ ] Step 4.4: Collect IO metrics in updatePrometheusMetrics()
- [ ] Step 4.5: Run `go vet ./state/` and `go test ./state/`

---

## Task 5: Prometheus Metrics

### Files:
- Modify: `metrics/collector.go`
- Modify: `metrics/prometheus.go`

### Step 5.1: Extend UserMetrics struct

In `metrics/collector.go`, add to `UserMetrics`:

```go
IOReadBytes  uint64 // Total bytes read from disk
IOWriteBytes uint64 // Total bytes written to disk
IOReadOps    uint64 // Total read operations
IOWriteOps   uint64 // Total write operations
```

### Step 5.2: Add Prometheus metric declarations

In `metrics/prometheus.go`, add to the `PrometheusExporter` struct:

```go
userIOReadBytes  *prometheus.CounterVec
userIOWriteBytes *prometheus.CounterVec
userIOReadOps    *prometheus.CounterVec
userIOWriteOps   *prometheus.CounterVec
```

Add `prevIOStats` map for delta tracking:

```go
prevIOStats map[string]ioStatsSnapshot // "uid_username" -> previous values
```

Define the snapshot type:

```go
type ioStatsSnapshot struct {
    ReadBytes  uint64
    WriteBytes uint64
    ReadOps    uint64
    WriteOps   uint64
}
```

### Step 5.3: Register metrics in registerMetrics()

```go
exp.userIOReadBytes = promauto.With(exp.registry).NewCounterVec(
    prometheus.CounterOpts{
        Namespace:   namespace,
        Name:        "user_io_read_bytes_total",
        Help:        "Total bytes read from block devices by user",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username"},
)

exp.userIOWriteBytes = promauto.With(exp.registry).NewCounterVec(
    prometheus.CounterOpts{
        Namespace:   namespace,
        Name:        "user_io_write_bytes_total",
        Help:        "Total bytes written to block devices by user",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username"},
)

exp.userIOReadOps = promauto.With(exp.registry).NewCounterVec(
    prometheus.CounterOpts{
        Namespace:   namespace,
        Name:        "user_io_read_ops_total",
        Help:        "Total read operations on block devices by user",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username"},
)

exp.userIOWriteOps = promauto.With(exp.registry).NewCounterVec(
    prometheus.CounterOpts{
        Namespace:   namespace,
        Name:        "user_io_write_ops_total",
        Help:        "Total write operations on block devices by user",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username"},
)
```

### Step 5.4: Extend UpdateUserMetrics signature

Change signature to include IO stats:

```go
func (exp *PrometheusExporter) UpdateUserMetrics(
    uid int, username string,
    cpuUsage float64, memoryUsage uint64, processCount int, isLimited bool,
    cgroupPath, cpuQuota string,
    memoryHighEvents uint64,
    ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64,
)
```

### Step 5.5: Update metrics with delta tracking

```go
// IO stats (counters with delta)
ioKey := fmt.Sprintf("%s_%s", uidStr, username)
prevIO := exp.prevIOStats[ioKey]

if ioReadBytes >= prevIO.ReadBytes {
    exp.userIOReadBytes.WithLabelValues(uidStr, username).Add(float64(ioReadBytes - prevIO.ReadBytes))
}
if ioWriteBytes >= prevIO.WriteBytes {
    exp.userIOWriteBytes.WithLabelValues(uidStr, username).Add(float64(ioWriteBytes - prevIO.WriteBytes))
}
if ioReadOps >= prevIO.ReadOps {
    exp.userIOReadOps.WithLabelValues(uidStr, username).Add(float64(ioReadOps - prevIO.ReadOps))
}
if ioWriteOps >= prevIO.WriteOps {
    exp.userIOWriteOps.WithLabelValues(uidStr, username).Add(float64(ioWriteOps - prevIO.WriteOps))
}

exp.prevIOStats[ioKey] = ioStatsSnapshot{
    ReadBytes:  ioReadBytes,
    WriteBytes: ioWriteBytes,
    ReadOps:    ioReadOps,
    WriteOps:   ioWriteOps,
}
```

### Step 5.6: Cleanup in CleanupUserMetrics()

```go
// Cleanup IO stats tracking
delete(exp.prevIOStats, ioKey)
exp.userIOReadBytes.DeleteLabelValues(uidStr, username)
exp.userIOWriteBytes.DeleteLabelValues(uidStr, username)
exp.userIOReadOps.DeleteLabelValues(uidStr, username)
exp.userIOWriteOps.DeleteLabelValues(uidStr, username)
```

### Step 5.7: Initialize prevIOStats in NewPrometheusExporter()

```go
prevIOStats: make(map[string]ioStatsSnapshot),
```

- [ ] Step 5.1: Extend UserMetrics struct
- [ ] Step 5.2: Add Prometheus metric declarations
- [ ] Step 5.3: Register metrics in registerMetrics()
- [ ] Step 5.4: Extend UpdateUserMetrics signature
- [ ] Step 5.5: Update metrics with delta tracking
- [ ] Step 5.6: Cleanup in CleanupUserMetrics()
- [ ] Step 5.7: Initialize prevIOStats in NewPrometheusExporter()
- [ ] Step 5.8: Update all callers of UpdateUserMetrics (state/manager.go)
- [ ] Step 5.9: Update mock in state/manager_test.go
- [ ] Step 5.10: Run `go vet ./metrics/` and `go test ./metrics/`

---

## Task 6: Documentation

### Files:
- Create: `docs/IO-LIMITS.md`
- Modify: `CHANGELOG.md`
- Modify: `docs/dashboard-grafana.json` (optional: add IO panels)

### Step 6.1: Create IO-LIMITS.md

Create comprehensive documentation covering:
- What IO limits do (block I/O bandwidth and IOPS)
- Configuration variables explained
- How to find device major:minor numbers
- Monitoring with Prometheus
- Alerting rules examples
- Troubleshooting

### Step 6.2: Update CHANGELOG.md

Add v1.20.0 entry with IO limits feature.

### Step 6.3: Update Grafana dashboard (optional)

Add panels for:
- `resman_user_io_read_bytes_total` (rate)
- `resman_user_io_write_bytes_total` (rate)
- `resman_user_io_read_ops_total` (rate)
- `resman_user_io_write_ops_total` (rate)

- [ ] Step 6.1: Create IO-LIMITS.md
- [ ] Step 6.2: Update CHANGELOG.md
- [ ] Step 6.3: Update Grafana dashboard

---

## Task 7: Testing & Verification

### Step 7.1: Unit tests

- Test `ApplyIOLimit` writes correct format to `io.max`
- Test `RemoveIOLimit` writes "max" values
- Test `GetIOStats` parses `io.stat` correctly
- Test config validation rejects invalid IO values

### Step 7.2: Integration test

- Verify `io` controller is enabled in `cgroup.subtree_control`
- Verify `io.max` file is created in user cgroup
- Verify Prometheus metrics appear after IO activity

### Step 7.3: Build verification

```bash
CGO_ENABLED=1 go build -o resman
go test ./...
golangci-lint run
```

- [ ] Step 7.1: Write unit tests for IO functions
- [ ] Step 7.2: Run integration test
- [ ] Step 7.3: Build and lint verification

---

## Execution Order

1. **Task 1** (Config) — no dependencies
2. **Task 2** (Cgroup Manager) — depends on Task 1 for config types
3. **Task 3** (Interface) — depends on Task 2 for function signatures
4. **Task 4** (State Manager) — depends on Tasks 1-3
5. **Task 5** (Prometheus) — depends on Tasks 3-4
6. **Task 6** (Docs) — can run in parallel with Tasks 4-5
7. **Task 7** (Testing) — after all tasks complete

Tasks 1 and 2 can be done in parallel by different agents.
