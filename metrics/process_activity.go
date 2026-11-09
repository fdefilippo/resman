package metrics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fdefilippo/resman/internal/processactivity"
)

func (c *Collector) restoreProcessActivity() {
	if c.activity == nil {
		return
	}
	writer := c.GetDBWriter()
	c.mu.RLock()
	restored := c.activityRestored
	c.mu.RUnlock()
	if writer == nil || restored {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := writer.RestoreProcessActivity(ctx, c.processBootID)
	if err != nil {
		writer.setProcessActivityWriteFailed(true)
		c.logger.Warn("Process activity restore unavailable", "error", err)
		return
	}
	c.activity.Restore(entries)
	c.mu.Lock()
	c.activityRestored = true
	c.mu.Unlock()
}

func (c *Collector) publishProcessActivity(state *userMetricsSamplingState, samples []processactivity.Sample, sampledAt time.Time, complete bool, unavailable int) {
	if state != c.decisionState || c.activity == nil {
		return
	}
	c.restoreProcessActivity()
	if c.processBootID == "" {
		unavailable += len(samples)
		complete = false
	}
	maxGap := c.fallbackCPUSampleMaxGap()
	c.activity.Update(samples, sampledAt, maxGap, complete, unavailable)
}

func readProcessBootID() string {
	body, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// QueryProcessActivity reads frozen observations and optional durable summaries only.
// It never scans procfs, advances decision state, or treats old stored activity as live.
func (c *Collector) QueryProcessActivity(ctx context.Context, q processactivity.Query) (processactivity.Result, error) {
	if err := q.Validate(); err != nil {
		return processactivity.Result{}, err
	}
	result := processactivity.Result{Entries: []processactivity.Entry{}, Persistence: processactivity.PersistenceDisabled}
	if c.activity != nil {
		result = c.activity.Snapshot()
		result.Persistence = processactivity.PersistenceDisabled
	}
	var durable []processactivity.Entry
	if c.getConfig().MetricsDBEnabled {
		result.Persistence = processactivity.PersistenceUnavailable
		if writer := c.GetDBWriter(); writer != nil {
			if entries, err := writer.QueryProcessActivity(ctx, q); err == nil {
				durable = entries
				if writer.Healthy() {
					result.Persistence = processactivity.PersistenceAvailable
				}
			}
		}
	}
	result.Entries = processactivity.Merge(durable, result.Entries, q)
	return result, nil
}

func (c *Collector) persistProcessActivity(writer *DBWriter) (persistErr error) {
	if c.activity == nil {
		return nil
	}
	// Raw database writes can precede the first process scan. No activity
	// snapshot is owed yet, and a no-op cannot recover an earlier failure.
	if !c.activity.HasObservations() {
		return nil
	}
	defer func() { writer.setProcessActivityWriteFailed(persistErr != nil) }()
	c.mu.RLock()
	restored := c.activityRestored
	c.mu.RUnlock()
	if !restored {
		return fmt.Errorf("process activity restore incomplete; preserving existing durable totals")
	}
	c.activity.Prune(time.Now().UTC().AddDate(0, 0, -c.getConfig().MetricsDBRetentionDays))
	result := c.activity.Snapshot()
	if len(result.Entries) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.UpsertProcessActivity(ctx, result.Entries); err != nil {
		return fmt.Errorf("persist process activity: %w", err)
	}
	return nil
}

// DecisionProcessDiagnostics exposes source failures that must block a decision,
// instead of allowing an unreadable process to contribute a fabricated zero.
type DecisionProcessDiagnostics interface{ DecisionProcessScanError() error }

// DecisionProcessScanError returns only the last authoritative scan failure.
func (c *Collector) DecisionProcessScanError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.decisionProcessScanError
}
func (c *Collector) setDecisionProcessScanError(state *userMetricsSamplingState, err error) {
	if state != c.decisionState {
		return
	}
	c.mu.Lock()
	c.decisionProcessScanError = err
	c.mu.Unlock()
}
func (c *Collector) procRoot() string {
	if c.processRoot != "" {
		return c.processRoot
	}
	return "/proc"
}
func (c *Collector) readProcessStat(root string, pid int, ticks float64) (processactivity.ProcStat, error) {
	if c.processStatReader != nil {
		return c.processStatReader(root, pid, ticks)
	}
	return processactivity.ReadStat(root, pid, ticks)
}
func discardProcessCPUBaseline(state *userMetricsSamplingState, pid int32) {
	if state == nil || state.process == nil {
		return
	}
	state.process.mu.Lock()
	defer state.process.mu.Unlock()
	delete(state.process.prevProcCPU, pid)
	delete(state.process.prevProcTime, pid)
	delete(state.process.procStartTime, pid)
}
