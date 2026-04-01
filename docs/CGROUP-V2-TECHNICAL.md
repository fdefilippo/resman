# Cgroup v2 Technical Reference for ResMan

## Overview

This document describes how Linux cgroups v2 work and how ResMan uses them to manage CPU and memory limits.

---

## Cgroup v2 Architecture

### Independent Controllers

In cgroups v2, CPU and memory are controlled by **independent controllers**. Each controller:
- Has its own configuration files
- Can be enabled/disabled independently via `cgroup.subtree_control`
- Operates completely separately from other controllers

### Directory Structure

```
/sys/fs/cgroup/
└── user_slice/
    └── user-1000.slice/
        ├── cgroup.controllers      → Available controllers (cpu memory io...)
        ├── cgroup.subtree_control  → Enabled controllers for children
        ├── cpu.max                 → CPU bandwidth limit
        ├── cpu.weight              → CPU relative priority
        ├── memory.max              → Memory hard limit
        ├── memory.high             → Memory soft limit (throttling)
        ├── memory.swap.max         → Swap limit
        ├── memory.current          → Current memory usage (read-only)
        ├── cpu.stat                → CPU statistics (read-only)
        └── memory.events           → Memory events (read-only)
```

---

## CPU Limiting (`cpu.max`)

### Configuration

```bash
# Format: $MAX $PERIOD (in microseconds)
echo "50000 100000" > /sys/fs/cgroup/mygroup/cpu.max
```

This limits the cgroup to 50% of one CPU core (50000µs out of 100000µs period).

### Behavior When Exceeded: **THROTTLING**

When a process exceeds its CPU quota:
- Processes are **throttled** (paused) until the next period
- Processes are **NEVER killed** for exceeding CPU limits
- Statistics are tracked in `cpu.stat`:
  - `nr_throttled` → Number of periods where throttling occurred
  - `throttled_usec` → Total throttling duration (microseconds)

### Example

```bash
# Apply limit: 0.5 core
echo "50000 100000" > /sys/fs/cgroup/mygroup/cpu.max

# Check throttling statistics
cat /sys/fs/cgroup/mygroup/cpu.stat
# Output:
# nr_periods 1523
# nr_throttled 847
# throttled_usec 42350000
```

### ResMan Usage

ResMan uses `cpu.max` with a **shared cgroup** approach:
- All limited users share a common cgroup under `/sys/fs/cgroup/resman/`
- Total quota = `(TotalCores - MinSystemCores) * 100000`
- Users get proportional `cpu.weight` (default: 100 each)
- Processes are moved to the shared cgroup via `MoveAllUserProcessesToSharedCgroup()`

---

## Memory Limiting (`memory.max` and `memory.high`)

### Configuration

```bash
# Hard limit (current ResMan default)
echo "536870912" > /sys/fs/cgroup/mygroup/memory.max  # 512MB

# Soft limit (alternative)
echo "268435456" > /sys/fs/cgroup/mygroup/memory.high  # 256MB
```

### `memory.max` - Hard Limit (Current ResMan Behavior)

**Behavior when exceeded:**
1. Kernel attempts **memory reclaim** (drop page cache, reclaim swap if enabled)
2. If reclaim fails → **OOM killer invoked within the cgroup**
3. OOM killer terminates **one or more processes INSIDE the cgroup**
4. Events tracked in `memory.events`:
   ```
   oom 1          # Number of OOM events
   oom_kill 1     # Processes killed
   ```

**Characteristics:**
- Absolute ceiling (with minor temporary breaches possible)
- Triggers OOM killer when limit is reached
- **Processes DIE when exceeding this limit**

### `memory.high` - Soft Limit (Proposed Alternative)

**Behavior when exceeded:**
1. Processes are **throttled** on memory allocation
2. Kernel applies **aggressive reclaim pressure**
3. **OOM killer is NEVER invoked** for exceeding `memory.high` alone
4. Limit may be temporarily breached under extreme conditions

**Characteristics:**
- Warning threshold, not a hard ceiling
- Causes slowdown, not termination
- Useful for external monitoring and graceful degradation

### Comparison Table

| Feature | `memory.high` | `memory.max` |
|---------|---------------|--------------|
| Type | Soft limit | Hard limit |
| When exceeded | Throttling + reclaim | OOM killer |
| Processes killed | ❌ Never | ✅ Yes |
| Can be breached | ✅ Temporarily | ⚠️ Rarely (temporary) |
| Use case | Warning, monitoring | Absolute enforcement |
| ResMan current | ❌ Not used | ✅ Default |

---

## Combined CPU + Memory Limits

### Independent Operation

CPU and memory limits work **independently**:

```
Cgroup with:
- cpu.max = "50000 100000"   (0.5 core)
- memory.max = "536870912"   (512MB)

Scenario A: Process uses 100% CPU for 10 seconds
→ Throttled to 0.5 core
→ Process SURVIVES

Scenario B: Process allocates 600MB RAM
→ OOM killer terminates process
→ Process DIES

Scenario C: Process uses 100% CPU AND 600MB RAM
→ Throttled on CPU
→ OOM killer on RAM
→ Result: Process killed by RAM limit
```

### Key Insight

**Memory limits are more dangerous than CPU limits** because:
- CPU throttling slows down but doesn't kill
- Memory limits trigger OOM killer that terminates processes

---

## ResMan Current Implementation

### CPU Management

**File:** `cgroup/manager.go`

```go
// ApplyCPULimit applies CPU quota via cpu.max
func (m *Manager) ApplyCPULimit(uid int, quota string) error {
    cpuMaxFile := filepath.Join(cgroupPath, "cpu.max")
    return os.WriteFile(cpuMaxFile, []byte(quota), 0644)
}

// ApplySharedCPULimit sets quota on shared cgroup
func (m *Manager) ApplySharedCPULimit(sharedPath string, quota string) error {
    cpuMaxFile := filepath.Join(sharedPath, "cpu.max")
    return os.WriteFile(cpuMaxFile, []byte(quota), 0644)
}
```

**Approach:**
- Shared cgroup for all limited users
- Proportional `cpu.weight` distribution
- Throttling-based enforcement (safe)

### Memory Management

**File:** `cgroup/manager.go`

```go
// ApplyRAMLimit applies hard memory limit via memory.max
func (m *Manager) ApplyRAMLimit(uid int, limit string) error {
    memoryMaxFile := filepath.Join(cgroupPath, "memory.max")
    return os.WriteFile(memoryMaxFile, []byte(limitValue), defaultFilePerm)
}

// ApplyRAMHigh applies soft memory limit via memory.high
func (m *Manager) ApplyRAMHigh(uid int, limit string) error {
    memoryHighFile := filepath.Join(cgroupPath, "memory.high")
    return os.WriteFile(memoryHighFile, []byte(limit), defaultFilePerm)
}

// ApplyRAMLimitWithHigh applies both memory.high and memory.max
func (m *Manager) ApplyRAMLimitWithHigh(uid int, maxLimit string, highLimit string) error {
    // Apply soft limit first (memory.high)
    if err := m.ApplyRAMHigh(uid, highLimit); err != nil {
        return fmt.Errorf("failed to apply RAM high: %w", err)
    }
    // Apply hard limit (memory.max)
    if err := m.ApplyRAMLimit(uid, maxLimit); err != nil {
        return fmt.Errorf("failed to apply RAM max: %w", err)
    }
    m.logger.Info("RAM limits applied (high + max)",
        "uid", uid,
        "high", highLimit,
        "max", maxLimit,
    )
    return nil
}

// ApplyRAMLimitWithSwapDisabled also disables swap
func (m *Manager) ApplyRAMLimitWithSwapDisabled(uid int, limit string) error {
    if err := m.ApplyRAMLimit(uid, limit); err != nil {
        return err
    }
    swapMaxFile := filepath.Join(cgroupPath, "memory.swap.max")
    return os.WriteFile(swapMaxFile, []byte("0"), defaultFilePerm)
}

// GetMemoryHighEvents returns the number of times memory.high was exceeded
func (m *Manager) GetMemoryHighEvents(uid int) (uint64, error) {
    memoryEventsFile := filepath.Join(cgroupPath, "memory.events")
    data, err := os.ReadFile(memoryEventsFile)
    if err != nil {
        return 0, err
    }
    // Parse "high 123" from memory.events
    // Returns count of memory.high breaches
}
```

**Approach:**
- Per-user cgroup with `memory.high` (soft) + `memory.max` (hard) limits
- `memory.high` = `RAM_QUOTA_PER_USER * RAM_HIGH_RATIO` (default: 80%)
- `memory.max` = `RAM_QUOTA_PER_USER` (100%)
- Optional swap disable via `memory.swap.max=0`
- OOM killer enforcement only when `memory.max` is exceeded
- Memory.high events tracked via `memory.events` file

### Configuration Variables

```bash
# RAM Management
RAM_LIMIT_ENABLED=false          # Enable RAM limiting
RAM_THRESHOLD=75                 # Activation threshold (%)
RAM_RELEASE_THRESHOLD=40         # Deactivation threshold (%)
RAM_QUOTA_LIMITED=2G             # Total RAM quota for limited users
RAM_QUOTA_PER_USER=512M          # Per-user RAM quota
DISABLE_SWAP=false               # Set memory.swap.max=0
RAM_HIGH_RATIO=0.8               # memory.high = 80% of memory.max (NEW!)
```

### Prometheus Metrics

**New metric (v1.19.0+):**
```
resman_user_memory_high_breaches_total{uid, username, hostname, server_role}
```

This counter tracks how many times each user exceeded their `memory.high` soft limit,
allowing monitoring and alerting on memory pressure before OOM kills occur.

---

## Implications for Production Use

### Current Behavior (memory.high + memory.max) - v1.19.0+

**Pros:**
- Graceful throttling when exceeding memory.high (80% by default)
- OOM killer only when exceeding memory.max (100%)
- External monitoring via `resman_user_memory_high_breaches_total` metric
- Reduced process kills with early warning system
- Configurable ratio via `RAM_HIGH_RATIO`

**Cons:**
- More complex configuration
- Two thresholds to tune (high ratio + max limit)
- May still OOM under extreme pressure

### Legacy Behavior (memory.max only) - Pre-v1.19.0

**Pros:**
- Absolute memory enforcement
- Prevents memory exhaustion attacks
- Clear boundary
- Simple configuration

**Cons:**
- Processes can be killed unexpectedly
- May cause service disruptions
- No graceful degradation
- No early warning system

---

## References

- [Kernel Documentation - Cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- [Memory Controller Documentation](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory)
- [CPU Controller Documentation](https://docs.kernel.org/admin-guide/cgroup-v2.html#cpu)

---

## Document Metadata

- **Version:** 1.1
- **Date:** 2026-03-31
- **Author:** ResMan Development Team
- **Related Project:** ResMan v1.19.0 (memory.high support added)
- **Changes:** Added memory.high soft limit implementation, RAM_HIGH_RATIO configuration, user_memory_high_breaches_total metric
