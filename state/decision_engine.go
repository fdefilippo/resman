package state

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

// ThresholdTracker tracks a threshold crossing over time.
type ThresholdTracker struct {
	firstOverThresholdTime time.Time // First threshold crossing.
	overThresholdCycles    int       // Cycles above threshold.
	totalCycles            int       // Total cycles.
	mu                     sync.RWMutex
}

type UserStabilityTracker struct {
	mu                  sync.RWMutex
	belowThresholdSince map[int]time.Time
}

func (m *Manager) makeDecision(metrics *SystemMetrics) (string, string) {
	cfg := m.GetConfig()
	m.mu.RLock()
	limitsActive := m.limitsActive || m.resourceLimitsActive
	limitsAppliedTime := m.limitsAppliedTime
	if limitsAppliedTime.IsZero() || (!m.resourceLimitsAppliedTime.IsZero() && m.resourceLimitsAppliedTime.Before(limitsAppliedTime)) {
		limitsAppliedTime = m.resourceLimitsAppliedTime
	}
	m.mu.RUnlock()

	// Get configuration values atomically to prevent inconsistency during reload
	minActiveTime := cfg.GetMinActiveTime()
	cpuReleaseThreshold := cfg.GetCPUReleaseThreshold()
	cpuThreshold := cfg.GetCPUThreshold()
	minSystemCores := cfg.GetMinSystemCores()
	ignoreSystemLoad := cfg.GetIgnoreSystemLoad()
	cpuThresholdDuration := cfg.GetCPUThresholdDuration()

	// Supported decisions.
	const (
		DecisionActivate   = "ACTIVATE_LIMITS"
		DecisionMaintain   = "MAINTAIN_CURRENT_STATE"
		DecisionDeactivate = "DEACTIVATE_LIMITS"
	)

	// Evaluate activation thresholds independently for every resource.
	cpuExceeded := metrics.CPUEligibleCPUUsage >= float64(cpuThreshold)

	ramExceeded := false
	if cfg.RAMEnabled && cfg.RAMThreshold > 0 && metrics.TotalMemoryMB > 0 {
		limitedRAMMB := float64(metrics.RAMEligibleUsageBytes) / (1024 * 1024)
		ramPercent := (limitedRAMMB / metrics.TotalMemoryMB) * 100
		ramExceeded = ramPercent >= float64(cfg.RAMThreshold)
	}

	ioExceeded := false
	ioPercent := 0.0
	ioThresholdDuration := cfg.GetIOThresholdDuration()
	if cfg.IOEnabled && cfg.IOThreshold > 0 && cfg.IOWriteBPS != "" && cfg.IOWriteBPS != "max" {
		writeLimit, err := config.ParseRAMQuota(cfg.IOWriteBPS)
		if err == nil && writeLimit > 0 {
			totalWriteLimit := writeLimit * uint64(metrics.IOEligibleUsersCount)
			if totalWriteLimit > 0 {
				ioPercent = float64(metrics.IOEligibleWriteBytes) / float64(totalWriteLimit) * 100
				ioExceeded = ioPercent >= float64(cfg.IOThreshold)
			}
		}
	}

	// Apply the I/O threshold duration when configured.
	if ioThresholdDuration > 0 && ioExceeded {
		ioTrackerReady := m.ioThresholdTracker.ShouldActivateLimits(
			ioPercent,
			float64(cfg.IOThreshold),
			time.Duration(ioThresholdDuration)*time.Second,
		)
		if !ioTrackerReady {
			// I/O is above threshold but has not remained there long enough.
			ioExceeded = false
		}
	} else {
		// Reset when I/O is below threshold or the duration guard is disabled.
		m.ioThresholdTracker.Reset()
	}

	anyExceeded := cpuExceeded || ramExceeded || ioExceeded

	// Evaluate release thresholds independently for every resource.
	cpuBelow := metrics.CPUEligibleCPUUsage < float64(cpuReleaseThreshold)

	ramBelow := true
	if cfg.RAMEnabled && cfg.RAMReleaseThreshold > 0 && metrics.TotalMemoryMB > 0 {
		limitedRAMMB := float64(metrics.RAMEligibleUsageBytes) / (1024 * 1024)
		ramPercent := (limitedRAMMB / metrics.TotalMemoryMB) * 100
		ramBelow = ramPercent < float64(cfg.RAMReleaseThreshold)
	}

	ioBelow := true
	if cfg.IOEnabled && cfg.IOReleaseThreshold > 0 && cfg.IOWriteBPS != "" && cfg.IOWriteBPS != "max" {
		writeLimit, err := config.ParseRAMQuota(cfg.IOWriteBPS)
		if err == nil && writeLimit > 0 {
			totalWriteLimit := writeLimit * uint64(metrics.IOEligibleUsersCount)
			if totalWriteLimit > 0 {
				ioPercent := float64(metrics.IOEligibleWriteBytes) / float64(totalWriteLimit) * 100
				ioBelow = ioPercent < float64(cfg.IOReleaseThreshold)
			}
		}
	}

	allBelow := cpuBelow && ramBelow && ioBelow

	// Active limits may be released only when all resource conditions allow it.
	if limitsActive {
		// Enforce the minimum activation time.
		if time.Since(limitsAppliedTime) < time.Duration(minActiveTime)*time.Second {
			return DecisionMaintain, "Limits active, waiting for minimum activation time"
		}

		// Deactivate only when every resource is below its release threshold.
		if allBelow {
			if m.stabilityTracker == nil {
				m.stabilityTracker = newUserStabilityTracker()
			}

			// Only users currently tracked in the shared cgroup participate in
			// release stability. Configuration eligibility alone is not runtime state.
			m.mu.RLock()
			limitedUsers := make([]int, 0, len(m.activeUsers))
			for uid := range m.activeUsers {
				limitedUsers = append(limitedUsers, uid)
			}
			m.mu.RUnlock()
			allUserMetrics := make(map[int]*resmanmetrics.UserMetrics)
			if m.metricsCollector != nil {
				allUserMetrics = m.metricsCollector.GetAllUserMetrics()
			}

			// Preserve the historical three-poll cool-down as wall-clock time.
			// PSI-triggered cycles therefore cannot shorten the release guard.
			stabilityDuration := 3 * time.Duration(cfg.GetPollingInterval()) * time.Second
			stable := m.stabilityTracker.AllBelowThreshold(
				limitedUsers,
				allUserMetrics,
				float64(cpuReleaseThreshold),
				stabilityDuration,
				time.Now(),
			)

			if !metrics.SystemUnderLoad && stable {
				m.thresholdTracker.Reset()
				m.ioThresholdTracker.Reset()
				return DecisionDeactivate, m.buildDeactivateReason(cpuBelow, ramBelow, ioBelow, metrics, cpuReleaseThreshold)
			}
			if !stable {
				return DecisionMaintain, "Resources below thresholds but waiting for stability (cool-down period)"
			}
			return DecisionMaintain, "Resources below thresholds but system still under load"
		}

		if m.stabilityTracker != nil {
			m.stabilityTracker.Reset()
		}
		return DecisionMaintain, "Limits active, at least one resource still above release threshold"
	}

	// When inactive, activate if any enabled resource exceeds its threshold.
	if anyExceeded {
		// MIN_SYSTEM_CORES protects only CPU enforcement. RAM or I/O may still
		// activate in standalone user cgroups that retain an unlimited CPU quota.
		if cpuExceeded && !ramExceeded && !ioExceeded && metrics.TotalCores <= minSystemCores {
			m.thresholdTracker.Reset()
			m.ioThresholdTracker.Reset()
			return DecisionMaintain, fmt.Sprintf(
				"Threshold exceeded but insufficient cores (%d <= %d)",
				metrics.TotalCores, minSystemCores,
			)
		}

		// Respect the configured system-load guard.
		if !ignoreSystemLoad && metrics.SystemUnderLoad {
			m.thresholdTracker.Reset()
			m.ioThresholdTracker.Reset()
			return DecisionMaintain, "Threshold exceeded but system already under load from other factors"
		}

		// Apply the CPU duration guard only when CPU is the sole exceeded resource.
		if cpuExceeded && cpuThresholdDuration > 0 {
			shouldActivate := m.thresholdTracker.ShouldActivateLimits(
				metrics.CPUEligibleCPUUsage,
				float64(cpuThreshold),
				time.Duration(cpuThresholdDuration)*time.Second,
			)
			if !shouldActivate && !ramExceeded && !ioExceeded {
				// CPU alone is above threshold but has not stayed there long enough.
				elapsed := m.thresholdTracker.GetElapsed()
				remaining := time.Duration(cpuThresholdDuration)*time.Second - elapsed
				return DecisionMaintain, fmt.Sprintf(
					"CPU threshold exceeded, waiting %s before activating limits (%.1f%% >= %d%%)",
					remaining.Round(time.Second),
					metrics.CPUEligibleCPUUsage, cpuThreshold,
				)
			}
		}

		return DecisionActivate, m.buildActivateReason(cpuExceeded, ramExceeded, ioExceeded, metrics, cpuThreshold)
	}

	// No resource exceeds its threshold; reset activation tracking.
	m.thresholdTracker.Reset()
	m.ioThresholdTracker.Reset()
	return DecisionMaintain, "All resources within normal range"
}

func (m *Manager) buildActivateReason(cpuExceeded, ramExceeded, ioExceeded bool, metrics *SystemMetrics, cpuThreshold int) string {
	cfg := m.GetConfig()
	reasons := []string{}
	if cpuExceeded {
		reasons = append(reasons, fmt.Sprintf("CPU %.1f%% >= %d%%", metrics.CPUEligibleCPUUsage, cpuThreshold))
	}
	if ramExceeded {
		reasons = append(reasons, fmt.Sprintf("RAM >= %d%%", cfg.RAMThreshold))
	}
	if ioExceeded {
		reasons = append(reasons, fmt.Sprintf("IO >= %d%%", cfg.IOThreshold))
	}
	return fmt.Sprintf("Threshold exceeded: %s", strings.Join(reasons, ", "))
}

func (m *Manager) buildDeactivateReason(cpuBelow, ramBelow, ioBelow bool, metrics *SystemMetrics, cpuReleaseThreshold int) string {
	return fmt.Sprintf(
		"All resources below release thresholds (CPU %.1f%% < %d%%)",
		metrics.CPUEligibleCPUUsage, cpuReleaseThreshold,
	)
}

func (m *Manager) executeDecision(decision string, metrics *SystemMetrics) error {
	switch decision {
	case "ACTIVATE_LIMITS":
		return m.activateLimits(metrics)
	case "DEACTIVATE_LIMITS":
		return m.deactivateLimits()
	case "MAINTAIN_CURRENT_STATE":
		// Reconcile users and release idle CPU enforcement.
		return m.releaseIdleUsers(metrics)
	default:
		return fmt.Errorf("unknown decision '%s': expected ACTIVATE_LIMITS, DEACTIVATE_LIMITS, or MAINTAIN_CURRENT_STATE", decision)
	}
}

func newUserStabilityTracker() *UserStabilityTracker {
	return &UserStabilityTracker{belowThresholdSince: make(map[int]time.Time)}
}

// AllBelowThreshold reports whether every listed user has remained below the
// CPU threshold for the complete required duration.
func (t *UserStabilityTracker) AllBelowThreshold(
	users []int,
	userMetrics map[int]*resmanmetrics.UserMetrics,
	threshold float64,
	requiredDuration time.Duration,
	now time.Time,
) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	currentUsers := make(map[int]struct{}, len(users))
	stable := true
	for _, uid := range users {
		currentUsers[uid] = struct{}{}
		metrics, ok := userMetrics[uid]
		if !ok || metrics == nil || metrics.CPUUsageEMA >= threshold {
			delete(t.belowThresholdSince, uid)
			stable = false
			continue
		}

		since, tracked := t.belowThresholdSince[uid]
		if !tracked || now.Before(since) {
			t.belowThresholdSince[uid] = now
			stable = false
			continue
		}
		if now.Sub(since) < requiredDuration {
			stable = false
		}
	}

	for uid := range t.belowThresholdSince {
		if _, exists := currentUsers[uid]; !exists {
			delete(t.belowThresholdSince, uid)
		}
	}
	return stable
}

// ForgetUsers discards release stability state for users leaving the limited set.
func (t *UserStabilityTracker) ForgetUsers(users []int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, uid := range users {
		delete(t.belowThresholdSince, uid)
	}
}

// Reset discards release stability state from the current limiting epoch.
func (t *UserStabilityTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.belowThresholdSince = make(map[int]time.Time)
}

// Reset clears threshold activation state.
func (t *ThresholdTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.firstOverThresholdTime = time.Time{}
	t.overThresholdCycles = 0
	t.totalCycles = 0
}

// ShouldActivateLimits checks if limits should be activated based on threshold duration.
func (t *ThresholdTracker) ShouldActivateLimits(
	currentCPU float64,
	threshold float64,
	requiredDuration time.Duration,
) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if currentCPU >= threshold {
		t.overThresholdCycles++

		if t.firstOverThresholdTime.IsZero() {
			t.firstOverThresholdTime = time.Now()
		}

		elapsed := time.Since(t.firstOverThresholdTime)
		t.totalCycles++

		// Activate only after the required duration.
		if elapsed >= requiredDuration {
			return true
		}
	} else {
		// Reset after the value falls below the threshold.
		t.firstOverThresholdTime = time.Time{}
		t.overThresholdCycles = 0
	}

	return false
}

// GetElapsed returns the elapsed time since the first threshold crossing.
func (t *ThresholdTracker) GetElapsed() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.firstOverThresholdTime.IsZero() {
		return 0
	}
	return time.Since(t.firstOverThresholdTime)
}
