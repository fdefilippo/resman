package state

import (
	"context"
	"fmt"
	"time"

	"github.com/fdefilippo/resman/internal/controlhistory"
)

// ControlHistoryResult describes both the available records and durable coverage.
type ControlHistoryResult struct {
	Entries     []ControlCycleEntry
	Persistence controlhistory.PersistenceStatus
	Pending     int
	Dropped     uint64
}

// finishControlCycle records exactly one terminal attempt after protective work.
// History delivery is non-authoritative: failure is reported, not enforcement.
func (m *Manager) finishControlCycle(run *controlCycleContext, cycleErr error) error {
	if m.controlHist == nil {
		return nil
	} // Managers constructed without runtime dependencies.
	finished := time.Now()
	entry := ControlCycleEntry{
		Details: controlhistory.Details{
			SessionID: m.historySessionID, CycleID: run.cycleID, StartedAt: run.startTime, Trigger: boundedHistoryTrigger(run.trigger),
			Outcome: controlhistory.Succeeded, FailedStage: string(run.failedStage), Suspended: run.stopWithoutError,
			ExecutionAttempted: run.enforcementState.Mode != "",
			RequestedIntent:    string(run.enforcementState.RequestedIntent), AppliedAction: string(run.enforcementState.AppliedAction), BlockReason: string(run.enforcementState.BlockReason),
			BlockIOPSSourceRequired: needsBlockIOPS(run.cfg),
		},
		Timestamp: finished, Decision: run.decision, Reason: run.reason, DurationMs: finished.Sub(run.startTime).Milliseconds(),
	}
	if len(run.degradedWarnings) > 0 {
		entry.Outcome = controlhistory.Degraded
	}
	if run.stopWithoutError {
		entry.Outcome = controlhistory.Suspended
	}
	if cycleErr != nil {
		entry.FailureKind = "stage_error"
		entry.Outcome = controlhistory.Failed
		if run.metrics != nil && run.decision != "" {
			entry.Outcome = controlhistory.Degraded
		}
		if IsControlCycleCancellation(run.ctx, cycleErr) {
			entry.Outcome = controlhistory.Cancelled
			entry.FailureKind = "context_cancelled"
		}
		if run.failedStage == "" && entry.FailureKind != "context_cancelled" {
			entry.FailureKind = "diagnostic_error"
		}
	}
	if sample := run.metrics; sample != nil {
		entry.SampleAvailable = true
		entry.HostCPUUsageAvailable = sample.HostCPUUsageAvailable
		entry.HostCPUUnavailableReason = string(sample.HostCPUUsageUnavailableReason)
		entry.SampleTimestamp = sample.Timestamp.UTC().Format(time.RFC3339Nano)
		entry.TotalCPUUsage = sample.TotalCPUUsage
		entry.CPUEligibleCPUUsage = sample.CPUEligibleCPUUsage
		entry.ObservedUsersCount = len(sample.UserMetrics)
		entry.BlockIOPSSourceAvailable = sample.IOBlockIOPSSourceAvailable
		entry.BlockIOPSUnavailableUsers = sample.IOBlockIOPSUnavailableUsers
		entry.IOUnavailableProcesses = sample.IOEligibleUnavailableProcesses
		if run.decision != "" {
			cpu := m.cpuDecisionMetrics(run)
			entry.CPUDecisionAvailable = true
			entry.CPUActivationRequested = cpu.ActivationRequested
			entry.CPUActivationApplied = cpu.ActivationApplied
		}
	}
	m.mu.RLock()
	entry.EnforcementMode = string(m.enforcementStatus.Mode)
	entry.CPULimitsActive = m.limitsActive
	entry.CPUActivelyLimitedUsersCount = len(m.activeUsers)
	m.mu.RUnlock()
	pending, dropped := m.addControlHistoryEntry(entry, run.cfg.MetricsDBEnabled)
	if !run.cfg.MetricsDBEnabled {
		return nil
	}

	if dropped {
		m.logger.Warn("Unpersisted control history evicted from bounded retry window")
		if m.prometheusExporter != nil {
			m.prometheusExporter.RecordError("control_history", "retry_window_overflow")
		}
	}
	writer := m.metricsCollector.GetDBWriter()
	if writer == nil {
		if m.prometheusExporter != nil {
			m.prometheusExporter.RecordError("control_history", "write_failure")
		}
		return fmt.Errorf("terminal control history database unavailable")
	}
	// Delivery has its own five-second budget, independent of caller cancellation,
	// so a cancelled attempt can still be recorded during orderly shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.WriteControlHistory(ctx, pending); err != nil {
		if m.prometheusExporter != nil {
			m.prometheusExporter.RecordError("control_history", "write_failure")
		}
		return fmt.Errorf("persist terminal control history: %w", err)
	}
	m.controlHist.mu.Lock()
	m.controlHist.pending = nil
	m.controlHist.mu.Unlock()
	return nil
}

func boundedHistoryTrigger(trigger string) string {
	switch trigger {
	case ControlCycleTriggerInitial, ControlCycleTriggerManual, ControlCycleTriggerTicker, ControlCycleTriggerIOWeight, "psi_system_cpu", "psi_system_io", "psi_user_cpu", "psi_user_io":
		return trigger
	default:
		return "other"
	}
}

// QueryControlHistory merges durable and recent attempts without advancing any
// decision state. An unavailable store leaves the recent window usable and explicit.
func (m *Manager) QueryControlHistory(ctx context.Context, limit int) ControlHistoryResult {
	limit = controlhistory.QueryLimit(limit)
	result := ControlHistoryResult{Persistence: controlhistory.PersistenceDisabled}
	m.controlHist.mu.RLock()
	result.Entries = append([]ControlCycleEntry(nil), m.controlHist.entries...)
	result.Pending = len(m.controlHist.pending)
	result.Dropped = m.controlHist.dropped
	m.controlHist.mu.RUnlock()
	if m.GetConfig().MetricsDBEnabled {
		result.Persistence = controlhistory.PersistenceUnavailable
		if writer := m.metricsCollector.GetDBWriter(); writer != nil {
			if durable, err := writer.ReadControlHistory(ctx, limit); err == nil {
				result.Entries = controlhistory.Merge(durable, result.Entries, limit)
				if writer.Healthy() {
					result.Persistence = controlhistory.PersistenceAvailable
				}
				return result
			}
		}
	}
	result.Entries = controlhistory.Merge(nil, result.Entries, limit)
	return result
}
