# Fix: Allineamento Controller CPU/RAM/IO Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix 3 critical inconsistencies between CPU, RAM and IO controllers: IO filtering uses wrong user lists, RAM/IO thresholds are unused, and UserMetrics fields are never populated.

**Architecture:** Add IO-specific filtering functions mirroring RAM pattern, integrate RAM/IO thresholds into makeDecision(), and clean up UserMetrics by removing unused fields.

**Tech Stack:** Go 1.25.7, cgroups v2

---

## File Structure

| File | Changes |
|------|---------|
| `config/config.go` | Add `IsUserIncludedForIO`, `IsUserExcludedForIO`, `IsUserWhitelistedForIO`; add `GetLimitedUsersRAMUsage`, `GetLimitedUsersIOUsage` methods or equivalent |
| `state/manager.go` | Update `shouldApplyIOLimits` to use IO-specific lists; extend `makeDecision` with RAM/IO threshold checks; add aggregate RAM/IO metrics to `SystemMetrics` |
| `metrics/collector.go` | Add `GetLimitedUsersRAMUsage()`, `GetLimitedUsersIOWriteBytes()` aggregate methods; remove unused fields from `UserMetrics` or populate them |
| `metrics/prometheus.go` | No changes needed (already handles IO correctly via cgroup manager) |
| `state/manager_test.go` | Update mock if interfaces change |

---

## Task 1: Fix IO Filtering — Use IO-specific User Lists

**Problem:** `shouldApplyIOLimits()` calls `cfg.IsUserIncluded(username)` which uses the CPU `UserIncludeList`. The config has `IOUserIncludeList` and `IOUserExcludeList` fields that are never used.

### Step 1.1: Add IO filtering functions to config/config.go

After `IsUserWhitelistedForRAM` (around line 995), add:

```go
// IsUserIncludedForIO verifica se l'utente è nella IO include list.
// Se la lista è vuota/nil, tutti sono inclusi.
func (c *Config) IsUserIncludedForIO(username string) bool {
	if c.IOUserIncludeList == nil || len(c.IOUserIncludeList) == 0 {
		return true
	}
	for _, pattern := range c.IOUserIncludeList {
		if matched, _ := regexp.MatchString(pattern, username); matched {
			return true
		}
	}
	return false
}

// IsUserExcludedForIO verifica se l'utente è nella IO exclude list.
// Se la lista è vuota/nil, nessuno è escluso.
func (c *Config) IsUserExcludedForIO(username string) bool {
	if c.IOUserExcludeList == nil || len(c.IOUserExcludeList) == 0 {
		return false
	}
	for _, pattern := range c.IOUserExcludeList {
		if matched, _ := regexp.MatchString(pattern, username); matched {
			return true
		}
	}
	return false
}

// IsUserWhitelistedForIO verifica se l'utente può essere limitato per IO.
func (c *Config) IsUserWhitelistedForIO(username string) bool {
	return c.IsUserIncludedForIO(username) && !c.IsUserExcludedForIO(username)
}
```

### Step 1.2: Update shouldApplyIOLimits in state/manager.go

Change from:
```go
func (m *Manager) shouldApplyIOLimits(uid int) bool {
	if !m.cfg.IOEnabled {
		return false
	}
	username := m.getUsername(uid)
	return m.cfg.IsUserIncluded(username)
}
```

To:
```go
func (m *Manager) shouldApplyIOLimits(uid int) bool {
	if !m.cfg.IOEnabled {
		return false
	}
	username := m.getUsername(uid)
	return m.cfg.IsUserWhitelistedForIO(username)
}
```

- [ ] Step 1.1: Add IO filtering functions to config/config.go
- [ ] Step 1.2: Update shouldApplyIOLimits in state/manager.go
- [ ] Step 1.3: Run `go vet ./config/ ./state/` and `go test ./config/ ./state/`

---

## Task 2: Integrate RAM/IO Thresholds in makeDecision()

**Problem:** `makeDecision()` only checks `LimitedUsersCPUUsage` against `CPUThreshold`. `RAMThreshold`, `RAMReleaseThreshold`, `IOThreshold`, `IOReleaseThreshold` are configured but never consulted. RAM and IO limits only activate when CPU limits activate.

### Step 2.1: Add aggregate RAM/IO fields to SystemMetrics

In `state/manager.go`, extend `SystemMetrics`:

```go
type SystemMetrics struct {
	// ... existing fields ...

	// RAM aggregates for limited users (for threshold decisions)
	LimitedUsersRAMUsageBytes uint64

	// IO aggregates for limited users (for threshold decisions)
	LimitedUsersIOReadBytes  uint64
	LimitedUsersIOWriteBytes uint64
}
```

### Step 2.2: Populate aggregate RAM/IO in collectSystemMetrics()

In `collectSystemMetrics()`, after populating `LimitedUsersMemoryUsage`, add:

```go
// Aggregate RAM and IO stats for limited users (for threshold decisions)
limitedUsers := m.metricsCollector.GetLimitedUsers()
var limitedRAMBytes uint64
var limitedIORead, limitedIOWrite uint64
for _, uid := range limitedUsers {
	if um, ok := allUserMetrics[uid]; ok {
		limitedRAMBytes += um.MemoryUsage
	}
	if m.cgroupManager != nil {
		rBytes, wBytes, _, _, _ := m.cgroupManager.GetIOStats(uid)
		limitedIORead += rBytes
		limitedIOWrite += wBytes
	}
}
metrics.LimitedUsersRAMUsageBytes = limitedRAMBytes
metrics.LimitedUsersIOReadBytes = limitedIORead
metrics.LimitedUsersIOWriteBytes = limitedIOWrite
```

### Step 2.3: Add RAM/IO threshold checks in makeDecision()

In `makeDecision()`, after the CPU threshold check, add RAM and IO checks. The decision logic should activate limits if ANY of CPU, RAM, or IO exceeds its threshold:

In the "limits not active" block (around the `if metrics.LimitedUsersCPUUsage >= float64(cpuThreshold)` check), wrap into a combined condition:

```go
// Check if any resource exceeds its threshold
cpuExceeded := metrics.LimitedUsersCPUUsage >= float64(cpuThreshold)

ramExceeded := false
if m.cfg.RAMEnabled && m.cfg.RAMThreshold > 0 {
	// Calculate RAM usage as percentage of total available to limited users
	// Use a simple ratio: limited RAM usage vs total system RAM
	totalRAMMB := metrics.TotalMemoryMB
	if totalRAMMB > 0 {
		limitedRAMMB := float64(metrics.LimitedUsersRAMUsageBytes) / (1024 * 1024)
		ramPercent := (limitedRAMMB / totalRAMMB) * 100
		ramExceeded = ramPercent >= float64(m.cfg.RAMThreshold)
	}
}

ioExceeded := false
if m.cfg.IOEnabled && m.cfg.IOThreshold > 0 {
	// IO threshold: compare total IO bytes against a configured baseline
	// For simplicity, use total write bytes as the indicator
	// (reads are often benign cache reads)
	if m.cfg.IOWriteBPS != "" && m.cfg.IOWriteBPS != "max" {
		writeLimit, err := config.ParseRAMQuota(m.cfg.IOWriteBPS)
		if err == nil && writeLimit > 0 {
			// If limited users are writing more than threshold% of their combined limit
			totalWriteLimit := writeLimit * uint64(metrics.LimitedUsersCount)
			if totalWriteLimit > 0 {
				ioPercent := float64(metrics.LimitedUsersIOWriteBytes) / float64(totalWriteLimit) * 100
				ioExceeded = ioPercent >= float64(m.cfg.IOThreshold)
			}
		}
	}
}

anyExceeded := cpuExceeded || ramExceeded || ioExceeded
```

Then use `anyExceeded` instead of `cpuExceeded` for the activation decision.

Similarly for deactivation: deactivate only when ALL active resources are below their release thresholds.

```go
// Deactivate when ALL resources below release thresholds
cpuBelow := metrics.LimitedUsersCPUUsage < float64(cpuReleaseThreshold)

ramBelow := true
if m.cfg.RAMEnabled && m.cfg.RAMReleaseThreshold > 0 {
	totalRAMMB := metrics.TotalMemoryMB
	if totalRAMMB > 0 {
		limitedRAMMB := float64(metrics.LimitedUsersRAMUsageBytes) / (1024 * 1024)
		ramPercent := (limitedRAMMB / totalRAMMB) * 100
		ramBelow = ramPercent < float64(m.cfg.RAMReleaseThreshold)
	}
}

ioBelow := true
if m.cfg.IOEnabled && m.cfg.IOReleaseThreshold > 0 {
	// Same logic as activation but with release threshold
	if m.cfg.IOWriteBPS != "" && m.cfg.IOWriteBPS != "max" {
		writeLimit, err := config.ParseRAMQuota(m.cfg.IOWriteBPS)
		if err == nil && writeLimit > 0 {
			totalWriteLimit := writeLimit * uint64(metrics.LimitedUsersCount)
			if totalWriteLimit > 0 {
				ioPercent := float64(metrics.LimitedUsersIOWriteBytes) / float64(totalWriteLimit) * 100
				ioBelow = ioPercent < float64(m.cfg.IOReleaseThreshold)
			}
		}
	}
}

allBelow := cpuBelow && ramBelow && ioBelow
```

Use `allBelow` for deactivation instead of just `cpuBelow`.

### Step 2.4: Update decision reasons

The reason strings should mention which resource triggered the decision:

```go
if anyExceeded {
	reasons := []string{}
	if cpuExceeded {
		reasons = append(reasons, fmt.Sprintf("CPU %.1f%% >= %d%%", metrics.LimitedUsersCPUUsage, cpuThreshold))
	}
	if ramExceeded {
		reasons = append(reasons, fmt.Sprintf("RAM exceeded %d%%", m.cfg.RAMThreshold))
	}
	if ioExceeded {
		reasons = append(reasons, fmt.Sprintf("IO exceeded %d%%", m.cfg.IOThreshold))
	}
	return DecisionActivate, fmt.Sprintf("Threshold exceeded: %s", strings.Join(reasons, ", "))
}
```

- [ ] Step 2.1: Add aggregate fields to SystemMetrics
- [ ] Step 2.2: Populate aggregates in collectSystemMetrics()
- [ ] Step 2.3: Add RAM/IO threshold checks in makeDecision()
- [ ] Step 2.4: Update decision reasons
- [ ] Step 2.5: Run `go vet ./state/` and `go test ./state/`

---

## Task 3: Clean Up UserMetrics Unused Fields

**Problem:** `UserMetrics` has fields `MemoryHighEvents`, `IOReadBytes`, `IOWriteBytes`, `IOReadOps`, `IOWriteOps` that are never populated by the collector. They exist only for the Prometheus path which reads them directly from the cgroup manager.

### Step 3.1: Remove unused fields from UserMetrics

In `metrics/collector.go`, remove:

```go
MemoryHighEvents uint64  // never populated by collector
IOReadBytes      uint64  // never populated by collector
IOWriteBytes     uint64  // never populated by collector
IOReadOps        uint64  // never populated by collector
IOWriteOps       uint64  // never populated by collector
```

The resulting struct:

```go
type UserMetrics struct {
	UID          int
	Username     string
	CPUUsage     float64 // CPU percentage
	MemoryUsage  uint64  // Memory in bytes (VmRSS)
	ProcessCount int     // Number of processes
	IsLimited    bool    // Whether user has CPU limits applied
}
```

### Step 3.2: Verify no callers reference removed fields

Search for `MemoryHighEvents`, `IOReadBytes`, `IOWriteBytes`, `IOReadOps`, `IOWriteOps` in the codebase. These fields are only referenced in:
- The struct definition itself
- Nowhere else (the Prometheus path reads directly from cgroup manager, not from UserMetrics)

- [ ] Step 3.1: Remove unused fields from UserMetrics
- [ ] Step 3.2: Verify no callers reference removed fields
- [ ] Step 3.3: Run `go vet ./...` and `go test ./...`

---

## Execution Order

1. **Task 1** (IO filtering) — independent
2. **Task 2** (RAM/IO thresholds) — depends on Task 1 for consistency
3. **Task 3** (UserMetrics cleanup) — independent

Tasks 1 and 3 can be done in parallel.
