# MCP Tools for Long-Running Process Detection

**Version:** 1.0  
**Date:** 2026-03-24  
**Status:** Proposal (Not Implemented)  
**Related:** `SECURITY_AND_DESIGN_ANALYSIS.md`, `METRICS-DATABASE.md`

---

## 📋 Executive Summary

This document proposes MCP (Model Context Protocol) tools that enable AI assistants to answer natural language questions about long-running processes and high CPU usage, such as:

- ❓ "Are there processes running for more than a week?"
- ❓ "Are there processes using at least 1 CPU core?"
- ❓ "What's the activity summary for user www-data?"

The implementation extends the existing monitoring system with continuous activity tracking, alerting for prolonged activity, and MCP tools for natural language queries.

---

## 🎯 Requirements

### Primary Requirements

1. **Monitor and record** CPU and RAM usage for users defined in `USER_INCLUDE_LIST`
2. **Track even if below limits** (continuous monitoring, not just when thresholds exceeded)
3. **Store in SQLite database** for historical queries
4. **Remove from monitoring** if process inactive for configurable timeout
5. **Detect prolonged activity** (> 7 days configurable)
6. **External alerting** via Prometheus (ResMan does NOT decide what's excessive, only records and exposes)

### Use Cases

| Use Case | Description | Example |
|----------|-------------|---------|
| **Cryptocurrency Mining** | Detect hidden mining processes | Process running 24/7 for weeks at 80%+ CPU |
| **Runaway Process** | Detect buggy processes consuming resources | Process stuck in loop, high CPU for days |
| **Brute Force Attack** | Detect prolonged authentication attempts | SSH process active continuously for days |
| **Legitimate Long Process** | Scientific computing, backups | Low CPU but active for weeks (should not alert) |

---

## 🏗️ Architecture

### 1. Configuration Variables

```bash
# /etc/resman.conf

# ============================================
# MONITORING CONFIGURATION
# ============================================

# Enable monitoring database
METRICS_DB_ENABLED=true
METRICS_DB_PATH=/etc/resman/metrics.db
METRICS_DB_RETENTION_DAYS=30
METRICS_DB_WRITE_INTERVAL=30

# Inactive user removal
MONITOR_INACTIVE_TIMEOUT=3600        # Remove user if inactive for N seconds (default: 1h)
MONITOR_MIN_ACTIVITY_CYCLES=3        # Minimum cycles before marking as inactive (default: 3)

# Long activity detection
MONITOR_LONG_ACTIVITY_THRESHOLD=604800  # Alert if active for > N seconds (default: 7 days = 604800)
MONITOR_LONG_ACTIVITY_CPU_THRESHOLD=50  # Alert only if avg CPU > N% (default: 50%, filters idle processes)
MONITOR_LONG_ACTIVITY_RAM_THRESHOLD=0   # Alert only if avg RAM > N bytes (default: 0 = disabled)
```

**Variable Descriptions:**

| Variable | Default | Description |
|----------|---------|-------------|
| `MONITOR_INACTIVE_TIMEOUT` | 3600 (1h) | Remove user from monitoring after N seconds of inactivity |
| `MONITOR_MIN_ACTIVITY_CYCLES` | 3 | Number of consecutive inactive cycles before removal |
| `MONITOR_LONG_ACTIVITY_THRESHOLD` | 604800 (7 days) | Alert if continuously active for > N seconds |
| `MONITOR_LONG_ACTIVITY_CPU_THRESHOLD` | 50 | Only alert if average CPU > N% (prevents false positives) |
| `MONITOR_LONG_ACTIVITY_RAM_THRESHOLD` | 0 | Only alert if average RAM > N bytes (0 = disabled) |

---

### 2. Database Schema

#### New Table: `user_monitoring_status`

```sql
CREATE TABLE IF NOT EXISTS user_monitoring_status (
    -- Primary Key
    uid INTEGER PRIMARY KEY,
    
    -- User Info
    username TEXT NOT NULL,
    
    -- Monitoring Status
    is_monitored INTEGER DEFAULT 1,      -- 1 = actively monitored, 0 = removed
    is_active INTEGER DEFAULT 1,          -- 1 = currently active, 0 = inactive
    
    -- Timestamps
    first_seen_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    active_since TIMESTAMP,               -- When current activity started
    inactive_since TIMESTAMP,             -- When inactivity started (NULL if active)
    
    -- Activity Tracking
    inactive_cycles INTEGER DEFAULT 0,    -- Consecutive inactive cycles
    active_cycles INTEGER DEFAULT 0,      -- Consecutive active cycles
    total_active_duration_seconds REAL DEFAULT 0,  -- Cumulative active time
    continuous_active_start TIMESTAMP,    -- Start of current continuous activity
    
    -- Averages (exponential moving average)
    avg_cpu_usage REAL DEFAULT 0,         -- Average CPU usage percentage
    avg_ram_usage INTEGER DEFAULT 0,      -- Average RAM usage in bytes
    
    -- Alert Tracking
    long_activity_alerted INTEGER DEFAULT 0,  -- 1 = alert already sent
    last_alert_time TIMESTAMP,              -- When last alert was sent
    
    -- Foreign Key
    FOREIGN KEY (uid) REFERENCES user_metrics(uid) ON DELETE CASCADE
);

-- Indexes for performance
CREATE INDEX idx_user_monitoring_status_active ON user_monitoring_status(is_active);
CREATE INDEX idx_user_monitoring_status_monitored ON user_monitoring_status(is_monitored);
CREATE INDEX idx_user_monitoring_status_inactive_since ON user_monitoring_status(inactive_since);
CREATE INDEX idx_user_monitoring_status_continuous_start ON user_monitoring_status(continuous_active_start);
CREATE INDEX idx_user_monitoring_status_alerted ON user_monitoring_status(long_activity_alerted);
```

---

### 3. Go Data Structures

#### `metrics/collector.go`

```go
// MonitoringAlertType defines the type of monitoring alert
type MonitoringAlertType string

const (
    AlertLongActivity MonitoringAlertType = "LONG_ACTIVITY"
    AlertHighCPU      MonitoringAlertType = "HIGH_CPU"
    AlertHighRAM      MonitoringAlertType = "HIGH_RAM"
)

// MonitoringAlert represents a monitoring alert
type MonitoringAlert struct {
    Type               MonitoringAlertType
    UID                int
    Username           string
    ContinuousDuration time.Duration
    AvgCPUUsage        float64
    AvgRAMUsage        uint64
    FirstSeenAt        time.Time
    ContinuousStart    time.Time
    AlertTime          time.Time
}

// UserMonitoringStatus tracks monitoring state for a user
type UserMonitoringStatus struct {
    UID             int
    Username        string
    IsMonitored     bool
    IsActive        bool
    
    // Timestamps
    FirstSeenAt     time.Time
    LastSeenAt      time.Time
    InactiveSince   *time.Time  // nil if active
    ActiveSince     *time.Time   // nil if inactive
    
    // Counters
    InactiveCycles  int
    ActiveCycles    int
    
    // Activity tracking
    TotalActiveDuration   time.Duration
    ContinuousActiveStart *time.Time
    AvgCPUUsage          float64  // Exponential moving average
    AvgRAMUsage          uint64   // Exponential moving average
    
    // Alert tracking
    LongActivityAlerted bool
    LastAlertTime       *time.Time
}

// Collector additions
type Collector struct {
    // ... existing fields ...
    
    // User monitoring tracking
    monitoringStatus    map[int]*UserMonitoringStatus
    monitoringMu        sync.RWMutex
    inactiveTimeout     time.Duration
    minActiveCycles     int
    longActivityThreshold time.Duration
    longActivityCPUThreshold float64
    longActivityRAMThreshold uint64
}
```

---

### 4. Core Methods

#### `metrics/collector.go`

```go
// UpdateUserMonitoringStatus updates monitoring status for a user and returns alert if needed
func (c *Collector) UpdateUserMonitoringStatus(
    uid int, 
    username string, 
    isActive bool,
    cpuUsage float64,
    ramUsage uint64,
) *MonitoringAlert {
    c.monitoringMu.Lock()
    defer c.monitoringMu.Unlock()
    
    now := time.Now()
    var alert *MonitoringAlert
    
    status, exists := c.monitoringStatus[uid]
    if !exists {
        // New user: start monitoring
        c.monitoringStatus[uid] = &UserMonitoringStatus{
            UID:            uid,
            Username:       username,
            IsMonitored:    true,
            IsActive:       isActive,
            FirstSeenAt:    now,
            LastSeenAt:     now,
            ActiveSince:    &now,
            ActiveCycles:   1,
            ContinuousActiveStart: &now,
            AvgCPUUsage:    cpuUsage,
            AvgRAMUsage:    ramUsage,
        }
        return nil
    }
    
    // Existing user: update status
    status.LastSeenAt = now
    
    if isActive {
        // === USER ACTIVE ===
        status.IsActive = true
        status.InactiveSince = nil
        status.InactiveCycles = 0
        status.ActiveCycles++
        
        // Update CPU/RAM averages (exponential moving average)
        alpha := 0.1  // Weight for new measurements
        status.AvgCPUUsage = (alpha * cpuUsage) + ((1 - alpha) * status.AvgCPUUsage)
        status.AvgRAMUsage = uint64((float64(status.AvgRAMUsage) * (1 - alpha)) + (float64(ramUsage) * alpha))
        
        // Track continuous activity start
        if status.ContinuousActiveStart == nil {
            status.ContinuousActiveStart = &now
        }
        
        // === CHECK LONG ACTIVITY ===
        if !status.LongActivityAlerted {
            threshold := c.longActivityThreshold
            
            continuousDuration := now.Sub(*status.ContinuousActiveStart)
            if continuousDuration >= threshold {
                // Check CPU threshold (if configured)
                if c.longActivityCPUThreshold > 0 && status.AvgCPUUsage < c.longActivityCPUThreshold {
                    goto SKIP_ALERT
                }
                
                // Check RAM threshold (if configured)
                if c.longActivityRAMThreshold > 0 && status.AvgRAMUsage < c.longActivityRAMThreshold {
                    goto SKIP_ALERT
                }
                
                // ALERT: Long activity detected!
                alert = &MonitoringAlert{
                    Type:               AlertLongActivity,
                    UID:                uid,
                    Username:           username,
                    ContinuousDuration: continuousDuration,
                    AvgCPUUsage:        status.AvgCPUUsage,
                    AvgRAMUsage:        status.AvgRAMUsage,
                    FirstSeenAt:        status.FirstSeenAt,
                    ContinuousStart:    *status.ContinuousActiveStart,
                    AlertTime:          now,
                }
                
                status.LongActivityAlerted = true
                status.LastAlertTime = &now
                
                c.logger.Warn("Long activity detected - user may need investigation",
                    "uid", uid,
                    "username", username,
                    "continuous_duration", continuousDuration,
                    "avg_cpu", status.AvgCPUUsage,
                    "avg_ram", status.AvgRAMUsage,
                )
            }
        }
        
        SKIP_ALERT:
        
    } else {
        // === USER INACTIVE ===
        status.IsActive = false
        status.ActiveCycles = 0
        status.ContinuousActiveStart = nil  // Reset continuous activity
        
        status.InactiveCycles++
        if status.InactiveSince == nil {
            status.InactiveSince = &now
        }
        
        // Check if remove from monitoring
        if status.InactiveCycles >= c.minActiveCycles {
            inactiveDuration := now.Sub(*status.InactiveSince)
            if inactiveDuration >= c.inactiveTimeout {
                status.IsMonitored = false
                c.logger.Info("User removed from monitoring (inactive timeout)",
                    "uid", uid,
                    "username", username,
                    "inactive_duration", inactiveDuration,
                )
            }
        }
    }
    
    return alert
}

// GetLongActivityAlerts returns alerts for long activity
func (c *Collector) GetLongActivityAlerts() []*MonitoringAlert {
    c.monitoringMu.RLock()
    defer c.monitoringMu.RUnlock()
    
    alerts := make([]*MonitoringAlert, 0)
    now := time.Now()
    
    for uid, status := range c.monitoringStatus {
        if !status.IsMonitored || !status.IsActive {
            continue
        }
        
        if status.ContinuousActiveStart == nil {
            continue
        }
        
        continuousDuration := now.Sub(*status.ContinuousActiveStart)
        if continuousDuration >= c.longActivityThreshold && !status.LongActivityAlerted {
            // Check CPU threshold
            if c.longActivityCPUThreshold > 0 && status.AvgCPUUsage < c.longActivityCPUThreshold {
                continue
            }
            
            alerts = append(alerts, &MonitoringAlert{
                Type:               AlertLongActivity,
                UID:                uid,
                Username:           status.Username,
                ContinuousDuration: continuousDuration,
                AvgCPUUsage:        status.AvgCPUUsage,
                AvgRAMUsage:        status.AvgRAMUsage,
                FirstSeenAt:        status.FirstSeenAt,
                ContinuousStart:    *status.ContinuousActiveStart,
                AlertTime:          now,
            })
        }
    }
    
    return alerts
}

// ResetLongActivityAlert resets alert for a user (after investigation)
func (c *Collector) ResetLongActivityAlert(uid int) {
    c.monitoringMu.Lock()
    defer c.monitoringMu.Unlock()
    
    if status, exists := c.monitoringStatus[uid]; exists {
        status.LongActivityAlerted = false
        status.ContinuousActiveStart = nil  // Reset for new tracking
        c.logger.Info("Long activity alert reset for user",
            "uid", uid,
            "username", status.Username,
        )
    }
}

// GetMonitoringStatus returns copy of all monitoring status
func (c *Collector) GetMonitoringStatus() map[int]*UserMonitoringStatus {
    c.monitoringMu.RLock()
    defer c.monitoringMu.RUnlock()
    
    copy := make(map[int]*UserMonitoringStatus, len(c.monitoringStatus))
    for k, v := range c.monitoringStatus {
        copy[k] = v
    }
    return copy
}

// CleanupInactiveMonitoring removes inactive users from tracking
func (c *Collector) CleanupInactiveMonitoring() {
    c.monitoringMu.Lock()
    defer c.monitoringMu.Unlock()
    
    now := time.Now()
    for uid, status := range c.monitoringStatus {
        if !status.IsMonitored {
            // Remove entry if not monitored for > retention
            if status.InactiveSince != nil && 
               now.Sub(*status.InactiveSince) > c.inactiveTimeout*2 {
                delete(c.monitoringStatus, uid)
            }
            continue
        }
        
        // Check timeout
        if status.InactiveSince != nil {
            if now.Sub(*status.InactiveSince) > c.inactiveTimeout {
                status.IsMonitored = false
                c.logger.Debug("User marked as inactive",
                    "uid", uid,
                    "username", status.Username,
                )
            }
        }
    }
}
```

---

### 5. Database Methods

#### `database/manager.go`

```go
// SaveUserMonitoringStatus saves monitoring status to database
func (m *Manager) SaveUserMonitoringStatus(status *metrics.UserMonitoringStatus) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    if m.db == nil {
        return nil  // Database not enabled
    }
    
    query := `
    INSERT INTO user_monitoring_status (
        uid, username, is_monitored, is_active,
        first_seen_at, last_seen_at, active_since, inactive_since,
        inactive_cycles, active_cycles, total_active_duration_seconds,
        continuous_active_start, avg_cpu_usage, avg_ram_usage,
        long_activity_alerted, last_alert_time
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    ON CONFLICT(uid) DO UPDATE SET
        is_monitored = excluded.is_monitored,
        is_active = excluded.is_active,
        last_seen_at = excluded.last_seen_at,
        active_since = excluded.active_since,
        inactive_since = excluded.inactive_since,
        inactive_cycles = excluded.inactive_cycles,
        active_cycles = excluded.active_cycles,
        total_active_duration_seconds = total_active_duration_seconds + 
            CASE WHEN excluded.is_active THEN 
                (julianday(excluded.last_seen_at) - julianday(old.last_seen_at)) * 86400
            ELSE 0 END,
        continuous_active_start = excluded.continuous_active_start,
        avg_cpu_usage = excluded.avg_cpu_usage,
        avg_ram_usage = excluded.avg_ram_usage,
        long_activity_alerted = excluded.long_activity_alerted,
        last_alert_time = excluded.last_alert_time
    `
    
    var activeSinceStr, inactiveSinceStr, continuousStartStr, lastAlertStr interface{}
    
    if status.ActiveSince != nil {
        activeSinceStr = status.ActiveSince.Format(time.RFC3339)
    }
    if status.InactiveSince != nil {
        inactiveSinceStr = status.InactiveSince.Format(time.RFC3339)
    }
    if status.ContinuousActiveStart != nil {
        continuousStartStr = status.ContinuousActiveStart.Format(time.RFC3339)
    }
    if status.LastAlertTime != nil {
        lastAlertStr = status.LastAlertTime.Format(time.RFC3339)
    }
    
    _, err := m.db.Exec(query,
        status.UID,
        status.Username,
        boolToInt(status.IsMonitored),
        boolToInt(status.IsActive),
        status.FirstSeenAt.Format(time.RFC3339),
        status.LastSeenAt.Format(time.RFC3339),
        activeSinceStr,
        inactiveSinceStr,
        status.InactiveCycles,
        status.ActiveCycles,
        status.TotalActiveDuration.Seconds(),
        continuousStartStr,
        status.AvgCPUUsage,
        status.AvgRAMUsage,
        boolToInt(status.LongActivityAlerted),
        lastAlertStr,
    )
    
    return err
}

// GetInactiveMonitoredUsers returns inactive users to remove from monitoring
func (m *Manager) GetInactiveMonitoredUsers(timeout time.Duration) ([]int, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    if m.db == nil {
        return nil, nil
    }
    
    query := `
    SELECT uid FROM user_monitoring_status
    WHERE is_monitored = 1
      AND inactive_since IS NOT NULL
      AND datetime(inactive_since, '+' || ? || ' seconds') < datetime('now')
    `
    
    rows, err := m.db.Query(query, int(timeout.Seconds()))
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    
    uids := make([]int, 0)
    for rows.Next() {
        var uid int
        if err := rows.Scan(&uid); err != nil {
            return nil, err
        }
        uids = append(uids, uid)
    }
    
    return uids, nil
}

// GetLongActivityUsers returns users with continuous activity > threshold
func (m *Manager) GetLongActivityUsers(minDays float64) ([]map[string]interface{}, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    if m.db == nil {
        return nil, nil
    }
    
    query := `
    SELECT 
        uid, username,
        avg_cpu_usage, avg_ram_usage,
        continuous_active_start,
        first_seen_at, last_seen_at,
        long_activity_alerted,
        julianday('now') - julianday(continuous_active_start) as days_active
    FROM user_monitoring_status
    WHERE is_monitored = 1
      AND is_active = 1
      AND continuous_active_start IS NOT NULL
      AND datetime(continuous_active_start, '+' || ? || ' days') < datetime('now')
    ORDER BY days_active DESC
    `
    
    rows, err := m.db.Query(query, minDays)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    
    results := make([]map[string]interface{}, 0)
    for rows.Next() {
        var uid int
        var username string
        var avgCPU float64
        var avgRAM uint64
        var continuousStart, firstSeen, lastSeen string
        var alerted bool
        var daysActive float64
        
        err := rows.Scan(&uid, &username, &avgCPU, &avgRAM, &continuousStart, 
                        &firstSeen, &lastSeen, &alerted, &daysActive)
        if err != nil {
            return nil, err
        }
        
        results = append(results, map[string]interface{}{
            "uid":              uid,
            "username":         username,
            "avg_cpu_percent":  avgCPU,
            "avg_ram_bytes":    avgRAM,
            "continuous_start": continuousStart,
            "first_seen":       firstSeen,
            "last_seen":        lastSeen,
            "alerted":          alerted,
            "days_active":      math.Round(daysActive*100) / 100,
        })
    }
    
    return results, nil
}
```

---

### 6. MCP Tools

#### `mcp/tools.go`

##### Tool 1: `get_long_running_processes`

```go
// Tool: get_long_running_processes
// Query users/processes by continuous activity duration
func (s *Server) registerGetLongRunningProcesses() {
    s.mcpServer.AddTool(mcp.Tool{
        Name:        "get_long_running_processes",
        Description: "Get list of users with processes running continuously for a specified duration. Use this to detect long-running processes that may need investigation (e.g., mining, runaway processes, brute force attacks).",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "min_days": map[string]interface{}{
                    "type":        "number",
                    "description": "Minimum continuous activity duration in days (e.g., 7 for one week)",
                    "default":     7,
                },
                "min_cpu_percent": map[string]interface{}{
                    "type":        "number",
                    "description": "Minimum average CPU usage percentage to include (filters out idle processes)",
                    "default":     0,
                },
                "include_inactive": map[string]interface{}{
                    "type":        "boolean",
                    "description": "Include users who are currently inactive",
                    "default":     false,
                },
                "limit": map[string]interface{}{
                    "type":        "integer",
                    "description": "Maximum number of results to return",
                    "default":     100,
                },
            },
        },
    }, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
        // Parse arguments
        minDays := 7.0
        if val, ok := req.Params.Arguments["min_days"].(float64); ok {
            minDays = val
        }
        
        minCPU := 0.0
        if val, ok := req.Params.Arguments["min_cpu_percent"].(float64); ok {
            minCPU = val
        }
        
        includeInactive := false
        if val, ok := req.Params.Arguments["include_inactive"].(bool); ok {
            includeInactive = val
        }
        
        limit := 100
        if val, ok := req.Params.Arguments["limit"].(int); ok {
            limit = val
        }
        
        // Get all monitored users
        allMetrics := s.metricsCollector.GetAllUserMetrics()
        monitoringStatus := s.metricsCollector.GetMonitoringStatus()
        
        now := time.Now()
        results := make([]map[string]interface{}, 0)
        
        for uid, status := range monitoringStatus {
            // Skip if not monitored
            if !status.IsMonitored {
                continue
            }
            
            // Skip if inactive and not requested
            if !status.IsActive && !includeInactive {
                continue
            }
            
            // Skip if no continuous activity start
            if status.ContinuousActiveStart == nil {
                continue
            }
            
            // Calculate continuous duration
            continuousDuration := now.Sub(*status.ContinuousActiveStart)
            daysActive := continuousDuration.Hours() / 24
            
            // Filter by minimum days
            if daysActive < minDays {
                continue
            }
            
            // Filter by minimum CPU
            if minCPU > 0 && status.AvgCPUUsage < minCPU {
                continue
            }
            
            // Get current metrics if available
            var currentCPU float64
            var currentRAM uint64
            var processCount int
            if userMetrics, ok := allMetrics[uid]; ok {
                currentCPU = userMetrics.CPUUsage
                currentRAM = userMetrics.MemoryUsage
                processCount = userMetrics.ProcessCount
            }
            
            results = append(results, map[string]interface{}{
                "uid":                  uid,
                "username":             status.Username,
                "continuous_days":      math.Round(daysActive*100) / 100,
                "continuous_hours":     math.Round(continuousDuration.Hours()*100) / 100,
                "continuous_start":     status.ContinuousActiveStart.Format(time.RFC3339),
                "first_seen":           status.FirstSeenAt.Format(time.RFC3339),
                "is_active":            status.IsActive,
                "avg_cpu_percent":      math.Round(status.AvgCPUUsage*100) / 100,
                "current_cpu_percent":  math.Round(currentCPU*100) / 100,
                "avg_ram_bytes":        status.AvgRAMUsage,
                "current_ram_bytes":    currentRAM,
                "process_count":        processCount,
                "alerted":              status.LongActivityAlerted,
                "last_alert_time":      status.LastAlertTime,
            })
        }
        
        // Sort by continuous_days descending
        sort.Slice(results, func(i, j int) bool {
            return results[i]["continuous_days"].(float64) > results[j]["continuous_days"].(float64)
        })
        
        // Apply limit
        if len(results) > limit {
            results = results[:limit]
        }
        
        // Build response
        response := map[string]interface{}{
            "query": map[string]interface{}{
                "min_days":          minDays,
                "min_cpu_percent":   minCPU,
                "include_inactive":  includeInactive,
                "limit":             limit,
                "timestamp":         now.Format(time.RFC3339),
            },
            "count": len(results),
            "users": results,
        }
        
        return mcp.NewToolResultJSON(response), nil
    })
}
```

##### Tool 2: `get_high_cpu_processes`

```go
// Tool: get_high_cpu_processes
// Query users/processes by CPU usage threshold
func (s *Server) registerGetHighCPUProcesses() {
    s.mcpServer.AddTool(mcp.Tool{
        Name:        "get_high_cpu_processes",
        Description: "Get list of users with processes consuming high CPU. Use this to detect CPU-intensive processes, potential mining, runaway processes, or performance issues.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "min_cpu_percent": map[string]interface{}{
                    "type":        "number",
                    "description": "Minimum current CPU usage percentage (e.g., 100 for 1 full CPU core)",
                    "default":     100,
                },
                "use_avg_cpu": map[string]interface{}{
                    "type":        "boolean",
                    "description": "Use average CPU instead of current CPU (better for detecting sustained usage)",
                    "default":     false,
                },
                "min_duration_hours": map[string]interface{}{
                    "type":        "number",
                    "description": "Minimum continuous activity duration in hours (filters out temporary spikes)",
                    "default":     0,
                },
                "include_inactive": map[string]interface{}{
                    "type":        "boolean",
                    "description": "Include users who are currently inactive",
                    "default":     false,
                },
                "limit": map[string]interface{}{
                    "type":        "integer",
                    "description": "Maximum number of results to return",
                    "default":     100,
                },
            },
        },
    }, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
        // Parse arguments
        minCPU := 100.0  // Default: 1 full CPU core
        if val, ok := req.Params.Arguments["min_cpu_percent"].(float64); ok {
            minCPU = val
        }
        
        useAvgCPU := false
        if val, ok := req.Params.Arguments["use_avg_cpu"].(bool); ok {
            useAvgCPU = val
        }
        
        minDurationHours := 0.0
        if val, ok := req.Params.Arguments["min_duration_hours"].(float64); ok {
            minDurationHours = val
        }
        
        includeInactive := false
        if val, ok := req.Params.Arguments["include_inactive"].(bool); ok {
            includeInactive = val
        }
        
        limit := 100
        if val, ok := req.Params.Arguments["limit"].(int); ok {
            limit = val
        }
        
        // Get all monitored users
        allMetrics := s.metricsCollector.GetAllUserMetrics()
        monitoringStatus := s.metricsCollector.GetMonitoringStatus()
        
        now := time.Now()
        results := make([]map[string]interface{}, 0)
        
        for uid, status := range monitoringStatus {
            // Skip if not monitored
            if !status.IsMonitored {
                continue
            }
            
            // Skip if inactive and not requested
            if !status.IsActive && !includeInactive {
                continue
            }
            
            // Get current metrics
            userMetrics, ok := allMetrics[uid]
            if !ok {
                continue
            }
            
            // Determine CPU value to use
            cpuValue := userMetrics.CPUUsage
            if useAvgCPU {
                cpuValue = status.AvgCPUUsage
            }
            
            // Filter by minimum CPU
            if cpuValue < minCPU {
                continue
            }
            
            // Filter by minimum duration (if specified)
            if minDurationHours > 0 && status.ContinuousActiveStart != nil {
                duration := now.Sub(*status.ContinuousActiveStart).Hours()
                if duration < minDurationHours {
                    continue
                }
            }
            
            // Calculate continuous duration if available
            var continuousDays float64
            var continuousStart string
            if status.ContinuousActiveStart != nil {
                continuousDays = math.Round(now.Sub(*status.ContinuousActiveStart).Hours()/24*100) / 100
                continuousStart = status.ContinuousActiveStart.Format(time.RFC3339)
            }
            
            results = append(results, map[string]interface{}{
                "uid":                 uid,
                "username":            status.Username,
                "current_cpu_percent": math.Round(userMetrics.CPUUsage*100) / 100,
                "avg_cpu_percent":     math.Round(status.AvgCPUUsage*100) / 100,
                "current_ram_bytes":   userMetrics.MemoryUsage,
                "avg_ram_bytes":       status.AvgRAMUsage,
                "process_count":       userMetrics.ProcessCount,
                "is_active":           status.IsActive,
                "continuous_days":     continuousDays,
                "continuous_start":    continuousStart,
                "alerted":             status.LongActivityAlerted,
            })
        }
        
        // Sort by CPU descending
        sort.Slice(results, func(i, j int) bool {
            return results[i]["current_cpu_percent"].(float64) > results[j]["current_cpu_percent"].(float64)
        })
        
        // Apply limit
        if len(results) > limit {
            results = results[:limit]
        }
        
        // Build response
        response := map[string]interface{}{
            "query": map[string]interface{}{
                "min_cpu_percent":    minCPU,
                "use_avg_cpu":        useAvgCPU,
                "min_duration_hours": minDurationHours,
                "include_inactive":   includeInactive,
                "limit":              limit,
                "timestamp":          now.Format(time.RFC3339),
            },
            "count": len(results),
            "users": results,
        }
        
        return mcp.NewToolResultJSON(response), nil
    })
}
```

##### Tool 3: `get_user_activity_summary`

```go
// Tool: get_user_activity_summary
// Get comprehensive activity summary for a specific user
func (s *Server) registerGetUserActivitySummary() {
    s.mcpServer.AddTool(mcp.Tool{
        Name:        "get_user_activity_summary",
        Description: "Get comprehensive activity summary for a specific user including CPU, RAM, process count, continuous activity duration, and alert status. Use this for detailed investigation of a specific user.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "uid": map[string]interface{}{
                    "type":        "integer",
                    "description": "User ID to query",
                },
                "username": map[string]interface{}{
                    "type":        "string",
                    "description": "Username to query (alternative to uid)",
                },
            },
        },
    }, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
        // Parse arguments
        uid, uidOk := req.Params.Arguments["uid"].(int)
        username, userOk := req.Params.Arguments["username"].(string)
        
        if !uidOk && !userOk {
            return mcp.NewToolResultText("Error: Must provide either 'uid' or 'username'"), nil
        }
        
        // Get all metrics and monitoring status
        allMetrics := s.metricsCollector.GetAllUserMetrics()
        monitoringStatus := s.metricsCollector.GetMonitoringStatus()
        
        now := time.Now()
        var results []map[string]interface{}
        
        for metricUID, userMetrics := range allMetrics {
            // Filter by uid or username
            if uidOk && metricUID != uid {
                continue
            }
            if userOk && userMetrics.Username != username {
                continue
            }
            
            status := monitoringStatus[metricUID]
            
            var continuousDays float64
            var continuousStart string
            var totalActiveHours float64
            if status != nil && status.ContinuousActiveStart != nil {
                continuousDays = math.Round(now.Sub(*status.ContinuousActiveStart).Hours()/24*100) / 100
                continuousStart = status.ContinuousActiveStart.Format(time.RFC3339)
                totalActiveHours = math.Round(status.TotalActiveDuration.Hours()*100) / 100
            }
            
            results = append(results, map[string]interface{}{
                "uid":                    metricUID,
                "username":               userMetrics.Username,
                "current_cpu_percent":    math.Round(userMetrics.CPUUsage*100) / 100,
                "avg_cpu_percent":        status.AvgCPUUsage,
                "current_ram_bytes":      userMetrics.MemoryUsage,
                "avg_ram_bytes":          status.AvgRAMUsage,
                "process_count":          userMetrics.ProcessCount,
                "is_active":              status.IsActive,
                "is_monitored":           status.IsMonitored,
                "continuous_days":        continuousDays,
                "continuous_start":       continuousStart,
                "total_active_hours":     totalActiveHours,
                "first_seen":             status.FirstSeenAt.Format(time.RFC3339),
                "last_seen":              status.LastSeenAt.Format(time.RFC3339),
                "long_activity_alerted":  status.LongActivityAlerted,
                "last_alert_time":        status.LastAlertTime,
            })
        }
        
        if len(results) == 0 {
            return mcp.NewToolResultText("No user found with specified uid/username"), nil
        }
        
        return mcp.NewToolResultJSON(map[string]interface{}{
            "timestamp": now.Format(time.RFC3339),
            "count":     len(results),
            "users":     results,
        }), nil
    })
}
```

##### Tool 4: `reset_long_activity_alert`

```go
// Tool: reset_long_activity_alert
// Reset long activity alert for a user after investigation
func (s *Server) registerResetLongActivityAlert() {
    s.mcpServer.AddTool(mcp.Tool{
        Name:        "reset_long_activity_alert",
        Description: "Reset long activity alert for a user after investigation. This allows monitoring to restart and generate new alerts if the activity continues.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "uid": map[string]interface{}{
                    "type":        "integer",
                    "description": "User ID to reset alert for",
                },
                "username": map[string]interface{}{
                    "type":        "string",
                    "description": "Username (for verification)",
                },
                "investigated": map[string]interface{}{
                    "type":        "boolean",
                    "description": "Confirm that the alert has been investigated",
                },
            },
            "required": []string{"uid", "investigated"},
        },
    }, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
        uid := req.Params.Arguments["uid"].(int)
        investigated := req.Params.Arguments["investigated"].(bool)
        
        if !investigated {
            return mcp.NewToolResultText("Error: Must confirm investigation before resetting alert"), nil
        }
        
        s.metricsCollector.ResetLongActivityAlert(uid)
        
        return mcp.NewToolResultText(fmt.Sprintf(
            "Long activity alert reset for UID %d. Monitoring will restart.",
            uid,
        )), nil
    })
}
```

---

### 7. Prometheus Metrics

#### `metrics/prometheus.go`

```go
// New metrics for long activity monitoring

exp.monitoredUsersCount = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
    Namespace:   namespace,
    Name:        "monitored_users_count",
    Help:        "Number of users currently being monitored (regardless of limits)",
    ConstLabels: staticLabels,
})

exp.activeMonitoredUsersCount = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
    Namespace:   namespace,
    Name:        "active_monitored_users_count",
    Help:        "Number of active users being monitored",
    ConstLabels: staticLabels,
})

exp.inactiveMonitoredUsersCount = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
    Namespace:   namespace,
    Name:        "inactive_monitored_users_count",
    Help:        "Number of inactive users being monitored",
    ConstLabels: staticLabels,
})

exp.longActivityAlerts = promauto.With(exp.registry).NewGaugeVec(
    prometheus.GaugeOpts{
        Namespace:   namespace,
        Name:        "long_activity_alerts",
        Help:        "Alert: User active for longer than threshold (1=alerting, 0=normal)",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username", "duration_hours"},
)

exp.continuousActiveDuration = promauto.With(exp.registry).NewGaugeVec(
    prometheus.GaugeOpts{
        Namespace:   namespace,
        Name:        "user_continuous_active_duration_seconds",
        Help:        "How long a user has been continuously active",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username"},
)

exp.userAvgCPUUsage = promauto.With(exp.registry).NewGaugeVec(
    prometheus.GaugeOpts{
        Namespace:   namespace,
        Name:        "user_avg_cpu_usage_percent",
        Help:        "Average CPU usage per user (exponential moving average)",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username"},
)

exp.userAvgRAMUsage = promauto.With(exp.registry).NewGaugeVec(
    prometheus.GaugeOpts{
        Namespace:   namespace,
        Name:        "user_avg_ram_usage_bytes",
        Help:        "Average RAM usage per user (exponential moving average)",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username"},
)

exp.longActivityAlertsTotal = promauto.With(exp.registry).NewCounterVec(
    prometheus.CounterOpts{
        Namespace:   namespace,
        Name:        "long_activity_alerts_total",
        Help:        "Total number of long activity alerts triggered",
        ConstLabels: staticLabels,
    },
    []string{"uid", "username"},
)
```

---

### 8. Prometheus Alert Rules

#### `docs/alerting-rules.yml`

```yaml
groups:
  - name: resman_long_activity
    rules:
      # Activity > 7 days with avg CPU > 50%
      - alert: ResManLongActivity7Days
        expr: |
          resman_user_continuous_active_duration_seconds > 604800
          and
          resman_user_avg_cpu_usage_percent > 50
        for: 0m  # Immediate alert, threshold already reached
        labels:
          severity: warning
        annotations:
          summary: "User {{ $labels.username }} active for > 7 days"
          description: |
            User {{ $labels.username }} (UID {{ $labels.uid }}) has been continuously 
            active for {{ $value | humanizeDuration }} with average CPU {{ 
            $labels.avg_cpu }}%. This may indicate a runaway process or mining activity.
          investigation: |
            Check: ps -u {{ $labels.username }}, top -u {{ $labels.username }},
            systemctl --user status

      # Activity > 14 days (more severe)
      - alert: ResManLongActivity14Days
        expr: |
          resman_user_continuous_active_duration_seconds > 1209600
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "User {{ $labels.username }} active for > 14 days"
          description: |
            User {{ $labels.username }} has been continuously active for 
            {{ $value | humanizeDuration }}. Immediate investigation recommended.

      # Activity > 30 days (critical)
      - alert: ResManLongActivity30Days
        expr: |
          resman_user_continuous_active_duration_seconds > 2592000
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "User {{ $labels.username }} active for > 30 days"
          description: |
            User {{ $labels.username }} has been continuously active for 
            {{ $value | humanizeDuration }}. This is highly unusual and 
            likely indicates a problem.

      # High CPU (> 1 core for 1 hour)
      - alert: ResManHighCPU1Core
        expr: |
          resman_user_cpu_usage_percent > 100
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "User {{ $labels.username }} using > 1 CPU core"
          description: |
            User {{ $labels.username }} has been using {{ $value }}% CPU 
            (more than 1 full core) for over 1 hour.

      # Many inactive users (monitoring system health)
      - alert: ResManManyInactiveUsers
        expr: |
          resman_inactive_monitored_users_count / resman_monitored_users_count > 0.5
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "More than 50% of monitored users are inactive"
          description: |
            {{ $value | humanizePercentage }} of monitored users are inactive.
            This may indicate monitoring timeout is too aggressive or system is idle.
```

---

### 9. SQL Queries for Investigation

```sql
-- Users active for > 7 consecutive days
SELECT 
    username,
    uid,
    continuous_active_start,
    datetime(continuous_active_start, 'localtime') as start_local,
    julianday('now') - julianday(continuous_active_start) as days_active,
    avg_cpu_usage,
    avg_ram_usage,
    long_activity_alerted
FROM user_monitoring_status
WHERE is_monitored = 1
  AND is_active = 1
  AND continuous_active_start IS NOT NULL
  AND datetime(continuous_active_start, '+7 days') < datetime('now')
ORDER BY days_active DESC;

-- Top 10 users by total active time (historical)
SELECT 
    username,
    uid,
    total_active_duration / 3600 as total_hours_active,
    monitoring_since,
    last_seen_at,
    CASE WHEN long_activity_alerted THEN 'YES' ELSE 'NO' END as alerted
FROM user_monitoring_status
WHERE is_monitored = 1
ORDER BY total_hours_active DESC
LIMIT 10;

-- Users alerted but not yet investigated
SELECT 
    username,
    uid,
    datetime(last_alert_time, 'localtime') as alert_time,
    continuous_active_duration / 3600 as hours_active,
    avg_cpu_usage,
    avg_ram_usage
FROM user_monitoring_status
WHERE is_monitored = 1
  AND long_activity_alerted = 1
  AND last_alert_time IS NOT NULL
ORDER BY last_alert_time DESC;

-- Users removed from monitoring (last 24h)
SELECT 
    username,
    uid,
    datetime(inactive_since, 'localtime') as inactive_since,
    julianday(last_seen_at) - julianday(first_seen_at) as monitoring_duration_days
FROM user_monitoring_status
WHERE is_monitored = 0
  AND inactive_since > datetime('now', '-24 hours')
ORDER BY inactive_since DESC;
```

---

## 💬 Example AI Assistant Conversations

### Conversation 1: "Are there processes running for more than a week?"

**User:** "Ci sono processi running da più di una settimana?"

**AI Assistant:**
```
Calling MCP tool: get_long_running_processes(min_days=7)
```

**Tool Response:**
```json
{
  "query": {"min_days": 7, "timestamp": "2026-03-24T15:30:00Z"},
  "count": 3,
  "users": [
    {
      "uid": 1001,
      "username": "www-data",
      "continuous_days": 12.5,
      "avg_cpu_percent": 78.5,
      "current_cpu_percent": 82.3,
      "process_count": 5,
      "alerted": true
    },
    {
      "uid": 1002,
      "username": "miner",
      "continuous_days": 9.2,
      "avg_cpu_percent": 95.2,
      "current_cpu_percent": 97.1,
      "process_count": 8,
      "alerted": true
    },
    {
      "uid": 1003,
      "username": "backup",
      "continuous_days": 7.1,
      "avg_cpu_percent": 12.3,
      "current_cpu_percent": 8.5,
      "process_count": 2,
      "alerted": false
    }
  ]
}
```

**AI Assistant Response:**
> **Sì, ci sono 3 utenti con processi attivi da più di una settimana:**
>
> 1. **www-data** (UID 1001): Attivo da **12.5 giorni**
>    - CPU media: 78.5% | CPU corrente: 82.3%
>    - 5 processi in esecuzione
>    - ⚠️ **Alert già generato**
>
> 2. **miner** (UID 1002): Attivo da **9.2 giorni**
>    - CPU media: 95.2% | CPU corrente: 97.1%
>    - 8 processi in esecuzione
>    - ⚠️ **Alert già generato**
>
> 3. **backup** (UID 1003): Attivo da **7.1 giorni**
>    - CPU media: 12.3% | CPU corrente: 8.5%
>    - 2 processi in esecuzione
>    - ✅ Nessun alert (CPU bassa, probabilmente legittimo)
>
> **Raccomandazione:** Investigare `www-data` e `miner` per attività prolungata ad alto consumo CPU.

---

### Conversation 2: "Are there processes using at least 1 CPU?"

**User:** "Ci sono processi running che occupano almeno 1 cpu?"

**AI Assistant:**
```
Calling MCP tool: get_high_cpu_processes(min_cpu_percent=100)
```

**Tool Response:**
```json
{
  "query": {"min_cpu_percent": 100, "timestamp": "2026-03-24T15:30:00Z"},
  "count": 2,
  "users": [
    {
      "uid": 1002,
      "username": "miner",
      "current_cpu_percent": 312.5,
      "avg_cpu_percent": 298.7,
      "continuous_days": 9.2,
      "process_count": 8,
      "alerted": true
    },
    {
      "uid": 1001,
      "username": "www-data",
      "current_cpu_percent": 156.3,
      "avg_cpu_percent": 142.1,
      "continuous_days": 12.5,
      "process_count": 5,
      "alerted": true
    }
  ]
}
```

**AI Assistant Response:**
> **Sì, ci sono 2 utenti con processi che occupano almeno 1 CPU completa (100%):**
>
> 1. **miner** (UID 1002): **312.5% CPU** (~3 core completi)
>    - CPU media: 298.7%
>    - Attivo da 9.2 giorni consecutivi
>    - 8 processi in esecuzione
>    - ⚠️ **Alert già generato**
>
> 2. **www-data** (UID 1001): **156.3% CPU** (~1.5 core)
>    - CPU media: 142.1%
>    - Attivo da 12.5 giorni consecutivi
>    - 5 processi in esecuzione
>    - ⚠️ **Alert già generato**
>
> **Raccomandazione:** Entrambi gli utenti mostrano consumo CPU anomalo e prolungato. Investigare immediatamente per possibile mining criptovalute o processi runaway.

---

## 📊 Implementation Checklist

### Phase 1: Core Infrastructure
- [ ] Add configuration variables to `config/config.go`
- [ ] Create `user_monitoring_status` table in database
- [ ] Add `UserMonitoringStatus` struct to `metrics/collector.go`
- [ ] Implement `UpdateUserMonitoringStatus()` method
- [ ] Implement `GetLongActivityAlerts()` method
- [ ] Implement `ResetLongActivityAlert()` method

### Phase 2: Database Integration
- [ ] Add `SaveUserMonitoringStatus()` to `database/manager.go`
- [ ] Add `GetInactiveMonitoredUsers()` to `database/manager.go`
- [ ] Add `GetLongActivityUsers()` to `database/manager.go`
- [ ] Integrate database writes in control cycle

### Phase 3: MCP Tools
- [ ] Implement `get_long_running_processes` tool
- [ ] Implement `get_high_cpu_processes` tool
- [ ] Implement `get_user_activity_summary` tool
- [ ] Implement `reset_long_activity_alert` tool

### Phase 4: Prometheus Metrics
- [ ] Add new metrics to `metrics/prometheus.go`
- [ ] Update `UpdateUserMetrics()` to expose new metrics
- [ ] Create Prometheus alert rules in `docs/alerting-rules.yml`

### Phase 5: Testing & Documentation
- [ ] Add unit tests for monitoring status tracking
- [ ] Add integration tests for MCP tools
- [ ] Update `CHANGELOG.md` with new features
- [ ] Update `README.md` with monitoring documentation

---

## ⚠️ Considerations

### Performance
- **Memory:** `monitoringStatus` map grows with number of users (bounded by inactive timeout)
- **Database:** Additional writes per cycle (mitigated by batch writes)
- **CPU:** Exponential moving average calculation (minimal overhead)

### False Positives
- **Legitimate long processes:** Scientific computing, backups, downloads
- **Mitigation:** CPU/RAM thresholds filter idle processes
- **Investigation:** MCP tools provide context for human decision

### Privacy
- **User tracking:** All non-system users are monitored
- **Data retention:** Configurable via `METRICS_DB_RETENTION_DAYS`
- **Access control:** MCP tools require appropriate authentication

---

## 📝 References

- `SECURITY_AND_DESIGN_ANALYSIS.md` - Security analysis that identified need for monitoring
- `METRICS-DATABASE.md` - Existing database integration
- `MCP-README.md` - MCP server documentation
- `docs/alerting-rules.yml` - Existing Prometheus alert rules

---

**Status:** Proposal - Ready for Implementation  
**Priority:** Medium (after critical security fixes)  
**Estimated Effort:** 2-3 days development + testing
