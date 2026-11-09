package app

import (
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/internal/readiness"
)

const (
	phaseStarting uint32 = iota
	phaseRunning
	phaseStopping
)

// ReadinessSnapshot reads bounded component state without filesystem, database,
// D-Bus or kernel probes. Optional failures do not disable protective operation.
func (a *App) ReadinessSnapshot() readiness.Snapshot {
	result := readiness.Snapshot{Status: readiness.NotReady, ConfigWatcher: readiness.Disabled, Database: readiness.Disabled, Systemd: readiness.Unavailable, PSI: readiness.Disabled}
	if a.phase.Load() != phaseRunning || a.ctx.Err() != nil || a.stateManager == nil {
		return result
	}
	result.Status = readiness.Ready
	if a.configPath != "" {
		result.ConfigWatcher = readiness.Degraded
		if a.configWatcher != nil && a.configWatcher.IsRunning() {
			result.ConfigWatcher = readiness.Active
		}
	}
	if a.databaseRequested {
		result.Database = readiness.Degraded
		if a.dbManager != nil && a.metricsCollector != nil {
			if writer := a.metricsCollector.GetDBWriter(); writer != nil && writer.Healthy() {
				result.Database = readiness.Active
			}
		}
	}
	cfg := a.currentConfig()
	if cfg.GetPSIEventDriven() {
		result.PSI = readiness.Degraded
		a.psiMu.RLock()
		watcher := a.psiWatcher
		a.psiMu.RUnlock()
		expectedMonitors := 2
		if cfg.GetPSIMemoryEventDriven() {
			expectedMonitors++
		}
		if a.isPSIEventDrivenActive() && watcher != nil && watcher.ActiveSystemMonitors() == expectedMonitors {
			result.PSI = readiness.Active
		}
	}
	cycle := a.stateManager.ReadinessSnapshot()
	result.DecisionAt = cycle.DecisionAt
	result.DecisionsSuspended = cycle.Suspended
	switch cycle.Mode {
	case cgroup.EnforcementModeObservationOnly:
		result.Systemd = readiness.Disabled
	case cgroup.EnforcementModeSystemdNative:
		result.Systemd = readiness.Active
		if cycle.SystemdDegraded {
			result.Systemd = readiness.Degraded
		}
	}
	for _, status := range []readiness.Status{result.ConfigWatcher, result.Database, result.Systemd, result.PSI} {
		if status == readiness.Degraded || status == readiness.Unavailable {
			result.Status = readiness.Degraded
		}
	}
	// Blackout is a valid deliberate suspension, including startup in blackout.
	// A failed core collection or a stalled loop is unavailable, not a zero sample.
	budget := 3 * time.Duration(a.controlCycleInterval()) * time.Second
	if !cycle.AvailableAt(time.Now(), budget) {
		result.Status = readiness.NotReady
	}
	return result
}
