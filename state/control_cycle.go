package state

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

const (
	ControlCycleTriggerInitial = "initial"
	ControlCycleTriggerTicker  = "ticker"
	ControlCycleTriggerManual  = "manual"
)

type controlCycleContext struct {
	ctx                context.Context
	cfg                *config.Config
	trigger            string
	startTime          time.Time
	cycleID            int64
	metrics            *SystemMetrics
	decision           string
	reason             string
	duration           time.Duration
	activeLimitedUsers int
	stopWithoutError   bool
}

type controlCycleStage func(*Manager, *controlCycleContext) error

var defaultControlCyclePipeline = []controlCycleStage{
	(*Manager).stageCheckBlackout,
	(*Manager).stageCollectMetrics,
	(*Manager).stageUpdatePrometheus,
	(*Manager).stageWriteDatabase,
	(*Manager).stageMakeDecision,
	(*Manager).stageExecuteDecision,
	(*Manager).stageRecordHistory,
	(*Manager).stageIORemediation,
	(*Manager).stageWorkloadPatternDetection,
	(*Manager).stageRevertPSIBoosts,
	(*Manager).stageLogCompletion,
}

func (m *Manager) RunControlCycle(ctx context.Context) error {
	return m.RunControlCycleWithTrigger(ctx, ControlCycleTriggerManual)
}

// RunMetricsRefresh refreshes Prometheus metrics without running a decision cycle.
func (m *Manager) RunMetricsRefresh(ctx context.Context, trigger string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if trigger == "" {
		trigger = "metrics_refresh"
	}

	startTime := time.Now()
	metrics, err := m.collectSystemMetricsForRefresh()
	if err != nil {
		m.logger.Error("Failed to collect metrics for refresh",
			"trigger", trigger,
			"error", err,
		)
		return fmt.Errorf("failed to collect metrics for refresh: %w", err)
	}

	if m.prometheusExporter != nil {
		m.updatePrometheusMetrics(metrics)
	}

	m.logger.Debug("Metrics refresh completed",
		"trigger", trigger,
		"duration_ms", time.Since(startTime).Milliseconds(),
	)

	return nil
}

// RunControlCycleWithTrigger executes one control cycle for the supplied trigger.
func (m *Manager) RunControlCycleWithTrigger(ctx context.Context, trigger string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if trigger == "" {
		trigger = ControlCycleTriggerManual
	}

	run := &controlCycleContext{
		ctx:       ctx,
		cfg:       m.GetConfig(),
		trigger:   trigger,
		startTime: time.Now(),
	}
	run.cycleID = run.startTime.Unix()

	if exporter := m.prometheusExporter; exporter != nil {
		exporter.RecordControlCycleTrigger(trigger)
		defer func() {
			exporter.RecordControlCycleDuration(time.Since(run.startTime))
		}()
	}

	m.logger.Debug("Starting control cycle", "cycle_id", run.cycleID, "trigger", trigger)

	for _, stage := range defaultControlCyclePipeline {
		if err := stage(m, run); err != nil {
			return err
		}
		if run.stopWithoutError {
			return nil
		}
	}

	return nil
}

func (m *Manager) stageCheckBlackout(run *controlCycleContext) error {
	// Controlla se siamo in un blackout timeframe
	nextEnd := run.cfg.GetNextBlackoutEnd()
	if nextEnd != nil {
		if err := m.revertAllPSIBoosts(); err != nil {
			m.logger.Warn("Failed to revert all PSI boosts while entering blackout",
				"cycle_id", run.cycleID,
				"error", err,
			)
		}

		ioBoostsReset := 0
		if m.ioRemediation != nil {
			ioBoostsReset = m.ioRemediation.ResetActiveBoosts()
		}

		m.mu.RLock()
		limitsNeedDeactivation := m.limitsActive || m.resourceLimitsActive || len(m.activeUsers) > 0 || len(m.resourceLimits) > 0 || m.sharedCgroupPath != ""
		m.mu.RUnlock()
		if limitsNeedDeactivation {
			if err := m.deactivateLimits(); err != nil {
				return fmt.Errorf("failed to deactivate limits for blackout (cycle %d): %w", run.cycleID, err)
			}
		}

		m.logger.Info("Control cycle suspended - blackout timeframe active",
			"cycle_id", run.cycleID,
			"trigger", run.trigger,
			"next_check", nextEnd.Format("2006-01-02 15:04:05"),
			"io_boosts_reset", ioBoostsReset,
		)
		run.stopWithoutError = true
	}
	return nil
}

func (m *Manager) stageCollectMetrics(run *controlCycleContext) error {
	// 1. Raccogli metriche del sistema
	metrics, err := m.collectSystemMetrics()
	if err != nil {
		m.logger.Error("Failed to collect system metrics",
			"cycle_id", run.cycleID,
			"trigger", run.trigger,
			"error", err,
		)
		return fmt.Errorf("failed to collect system metrics (cycle %d): %w", run.cycleID, err)
	}
	run.metrics = metrics
	return nil
}

func (m *Manager) stageUpdatePrometheus(run *controlCycleContext) error {
	// 2. Aggiorna le metriche Prometheus (se abilitato)
	if m.prometheusExporter != nil {
		m.updatePrometheusMetrics(run.metrics)
	}
	return nil
}

func (m *Manager) stageWriteDatabase(run *controlCycleContext) error {
	// 3. Scrivi le metriche nel database (se abilitato)
	m.writeDatabaseMetrics(run.metrics)
	return nil
}

func (m *Manager) stageMakeDecision(run *controlCycleContext) error {
	// 4. Prendi decisione basata sulle metriche
	run.decision, run.reason = m.makeDecision(run.metrics)
	return nil
}

func (m *Manager) stageExecuteDecision(run *controlCycleContext) error {
	// 4. Esegui l'azione corrispondente
	if err := m.executeDecision(run.decision, run.metrics); err != nil {
		m.logger.Error("Failed to execute decision",
			"decision", run.decision,
			"reason", run.reason,
			"cycle_id", run.cycleID,
			"trigger", run.trigger,
			"error", err,
		)
		return fmt.Errorf("failed to execute decision %s (cycle %d): %w", run.decision, run.cycleID, err)
	}
	return nil
}

func (m *Manager) stageRecordHistory(run *controlCycleContext) error {
	// 6. Registra lo storico del ciclo
	run.duration = time.Since(run.startTime)
	m.recordControlCycle(run.decision, run.reason, run.metrics, run.duration)
	return nil
}

func (m *Manager) stageIORemediation(run *controlCycleContext) error {
	// Run I/O starvation auto-remediation for observed active, I/O-eligible users.
	if m.ioRemediation != nil {
		var limitedUsers []int
		if run.cfg.GetIOEnabled() {
			m.mu.RLock()
			for uid := range m.activeUsers {
				limitedUsers = append(limitedUsers, uid)
			}
			m.mu.RUnlock()
			limitedUsers = slices.DeleteFunc(limitedUsers, func(uid int) bool {
				return !run.cfg.EvaluateUserEligibility(m.getUsername(uid)).EligibleForIO
			})
		}
		m.ioRemediation.CheckAndRemediate(m.cgroupManager, run.cfg, limitedUsers)
		// Remove stale remediation state periodically.
		m.ioRemediation.Cleanup(24 * time.Hour)
	}
	return nil
}

func (m *Manager) stageWorkloadPatternDetection(run *controlCycleContext) error {
	// 8. Workload Pattern Detection
	if m.patternDetector == nil || m.policyEngine == nil {
		return nil
	}

	if !run.cfg.GetAutodetectPatterns() {
		for _, uid := range m.policyEngine.Clear() {
			m.reconcilePatternPolicy(uid, run.cfg)
		}
		return nil
	}

	configuredEligible := make(map[int]bool)
	allMetrics := m.metricsCollector.GetAllUserMetrics()
	for uid, um := range allMetrics {
		if um == nil {
			continue
		}
		username := um.Username
		if username == "" {
			username = m.metricsCollector.GetUsernameFromUID(uid)
		}
		if !run.cfg.IsUserWhitelisted(username) {
			continue
		}
		configuredEligible[uid] = true
		m.patternDetector.Update(uid, um.EnforceableUsage.CPUUsage)
	}

	// Preserve history for configured users even when they have no live processes.
	for _, uid := range m.patternDetector.UserIDs() {
		if configuredEligible[uid] {
			continue
		}
		username := m.metricsCollector.GetUsernameFromUID(uid)
		if run.cfg.IsUserWhitelisted(username) {
			configuredEligible[uid] = true
		}
	}

	m.patternDetector.RetainUsers(configuredEligible)
	for _, uid := range m.policyEngine.RetainUsers(configuredEligible) {
		m.reconcilePatternPolicy(uid, run.cfg)
	}

	// Analizza pattern ogni ora
	if time.Since(m.lastPatternAnalysis) <= time.Hour {
		return nil
	}

	m.lastPatternAnalysis = time.Now()
	for _, uid := range m.patternDetector.Cleanup(time.Duration(run.cfg.GetPatternHistoryHours()) * time.Hour) {
		if m.policyEngine.RemovePolicy(uid) {
			m.reconcilePatternPolicy(uid, run.cfg)
		}
	}
	patterns := m.patternDetector.Analyze(run.cfg)
	for uid, result := range patterns {
		if m.prometheusExporter != nil {
			username := m.metricsCollector.GetUsernameFromUID(uid)
			m.prometheusExporter.UpdateUserWorkloadPattern(uid, username, string(result.Pattern), result.Confidence)
		}

		changed := false
		if result.Pattern == PatternUnknown {
			changed = m.policyEngine.RemovePolicy(uid)
		} else {
			changed = m.policyEngine.ApplyPolicy(uid, result.Pattern, run.cfg)
		}
		if changed {
			m.reconcilePatternPolicy(uid, run.cfg)
		}
	}

	return nil
}

func (m *Manager) reconcilePatternPolicy(uid int, cfg *config.Config) {
	if !m.isUserLimited(uid) {
		return
	}
	if err := m.applyUserResourceLimits(uid, cfg, cfg.EvaluateUserEligibility(m.getUsername(uid))); err != nil {
		m.logger.Warn("Failed to reconcile resource limits for detected workload pattern", "uid", uid, "error", err)
	}
	m.mu.Lock()
	m.refreshResourceLimitsActiveLocked(time.Now())
	m.mu.Unlock()
}

func (m *Manager) stageRevertPSIBoosts(run *controlCycleContext) error {
	// 9a. Revert PSI weight boosts that have expired
	if m.psiWatcher != nil {
		m.revertPSIBoosts()
	}
	return nil
}

func (m *Manager) stageLogCompletion(run *controlCycleContext) error {
	m.mu.RLock()
	run.activeLimitedUsers = len(m.activeUsers)
	m.mu.RUnlock()

	// 9. Logga il risultato del ciclo
	m.logger.Info("Control cycle completed",
		"cycle_id", run.cycleID,
		"trigger", run.trigger,
		"decision", run.decision,
		"reason", run.reason,
		"total_cpu_usage", run.metrics.TotalCPUUsage,
		"limited_users_cpu_usage", run.metrics.CPUEligibleCPUUsage,
		"eligible_users", run.metrics.CPUEligibleUsersCount,
		"active_limited_users", run.activeLimitedUsers,
		"system_under_load", run.metrics.SystemUnderLoad,
		"ignore_system_load", run.cfg.GetIgnoreSystemLoad(),
		"duration_ms", run.duration.Milliseconds(),
	)

	return nil
}

type SystemMetrics struct {
	Timestamp     time.Time
	TotalCores    int
	TotalCPUUsage float64 // Percentage

	// All non-system users with UID at or above SYSTEM_UID_MIN.
	AllUsersCPUUsage    float64
	AllUsersMemoryUsage uint64
	AllUsersCount       int

	// Per-resource eligible-user metrics.
	CPUEligibleCPUUsage              float64
	CPUEligibleMemoryUsage           uint64
	CPUEligibleUsersCount            int
	RAMEligibleUsersCount            int
	IOEligibleUsersCount             int
	RAMEligibleUsageBytes            uint64
	IOEligibleReadBPS                float64
	IOEligibleWriteBPS               float64
	IOEligibleReadSyscallsPerSecond  float64
	IOEligibleWriteSyscallsPerSecond float64

	MemoryUsage      float64 // MB
	TotalMemoryMB    float64 // MB
	CachedMemoryMB   float64 // MB
	SystemLoad       float64
	SystemUnderLoad  bool
	UserCPUUsage     map[int]float64                    // UID to CPU percentage
	UserMetrics      map[int]*resmanmetrics.UserMetrics // Detailed per-user metrics
	CPUEligibleUsers []int
	RAMEligibleUsers []int
	IOEligibleUsers  []int
}

func (m *Manager) collectSystemMetrics() (*SystemMetrics, error) {
	return m.collectSystemMetricsWithIOState(true)
}

func (m *Manager) collectSystemMetricsForRefresh() (*SystemMetrics, error) {
	return m.collectSystemMetricsWithIOState(false)
}

func (m *Manager) collectSystemMetricsWithIOState(updateIOState bool) (*SystemMetrics, error) {
	collectionStarted := time.Now()
	sampleTime := collectionStarted
	if exporter := m.prometheusExporter; exporter != nil {
		defer func() {
			exporter.RecordMetricsCollectionDuration(time.Since(collectionStarted))
		}()
	}

	metrics := &SystemMetrics{
		Timestamp:    sampleTime,
		UserCPUUsage: make(map[int]float64),
		UserMetrics:  make(map[int]*resmanmetrics.UserMetrics),
	}

	// Collect base system metrics.
	metrics.TotalCores = m.metricsCollector.GetTotalCores()
	metrics.TotalCPUUsage = m.metricsCollector.GetTotalCPUUsage()

	metrics.MemoryUsage = m.metricsCollector.GetMemoryUsage()
	metrics.TotalMemoryMB = m.metricsCollector.GetTotalMemoryMB()
	metrics.CachedMemoryMB = m.metricsCollector.GetCachedMemoryMB()
	metrics.SystemUnderLoad = m.metricsCollector.IsSystemUnderLoad()
	systemLoad, err := m.metricsCollector.GetSystemLoad()
	if err != nil {
		m.logger.Warn("Failed to collect system load", "error", err)
		if m.prometheusExporter != nil {
			m.prometheusExporter.RecordError(metricsCollectionErrorComponent, metricsCollectionSystemLoadError)
		}
	} else {
		metrics.SystemLoad = systemLoad
	}

	// Collect detailed per-user CPU, memory, process, and I/O metrics in one call.
	allUserMetrics := m.metricsCollector.GetAllUserMetrics()

	// Compute total and per-resource eligible-user aggregates in one pass.
	for uid, um := range allUserMetrics {
		metrics.AllUsersCPUUsage += um.CPUUsage
		metrics.AllUsersMemoryUsage += um.MemoryUsage
		metrics.AllUsersCount++

		metrics.UserCPUUsage[uid] = um.CPUUsage

		limitState := m.GetUserLimitState(uid, um.Username)

		corrected := &resmanmetrics.UserMetrics{
			UID:               um.UID,
			Username:          um.Username,
			CPUUsage:          um.CPUUsage,
			CPUUsageAverage:   um.CPUUsageAverage,
			CPUUsageEMA:       um.CPUUsageEMA,
			MemoryUsage:       um.MemoryUsage,
			ProcessCount:      um.ProcessCount,
			EligibleForCPU:    limitState.EligibleForCPU,
			EligibleForRAM:    limitState.EligibleForRAM,
			EligibleForIO:     limitState.EligibleForIO,
			CPULimitRequested: limitState.CPULimitRequested,
			CPULimitActive:    limitState.CPULimitActive,
			RAMLimitRequested: limitState.RAMLimitRequested,
			RAMLimitActive:    limitState.RAMLimitActive,
			IOLimitRequested:  limitState.IOLimitRequested,
			IOLimitActive:     limitState.IOLimitActive,
			IOReadBytes:       um.IOReadBytes,
			IOWriteBytes:      um.IOWriteBytes,
			IOReadOps:         um.IOReadOps,
			IOWriteOps:        um.IOWriteOps,
			EnforceableUsage:  um.EnforceableUsage,
		}
		metrics.UserMetrics[uid] = corrected

		if corrected.EligibleForCPU {
			metrics.CPUEligibleUsers = append(metrics.CPUEligibleUsers, uid)
			metrics.CPUEligibleCPUUsage += um.EnforceableUsage.CPUUsage
			metrics.CPUEligibleMemoryUsage += um.EnforceableUsage.MemoryUsage
		}
		if corrected.EligibleForRAM {
			metrics.RAMEligibleUsers = append(metrics.RAMEligibleUsers, uid)
			metrics.RAMEligibleUsageBytes += um.EnforceableUsage.MemoryUsage
		}
		if corrected.EligibleForIO {
			metrics.IOEligibleUsers = append(metrics.IOEligibleUsers, uid)
			if updateIOState {
				current := ioCounters{
					readBytes:  um.EnforceableUsage.IOReadBytes,
					writeBytes: um.EnforceableUsage.IOWriteBytes,
					readOps:    um.EnforceableUsage.IOReadOps,
					writeOps:   um.EnforceableUsage.IOWriteOps,
				}
				if previous, ok := m.prevIOCounters[uid]; ok && !m.prevIOTime.IsZero() {
					rates := calculateIORates(current, previous, sampleTime.Sub(m.prevIOTime))
					metrics.IOEligibleReadBPS += rates.readBytes
					metrics.IOEligibleWriteBPS += rates.writeBytes
					metrics.IOEligibleReadSyscallsPerSecond += rates.readOps
					metrics.IOEligibleWriteSyscallsPerSecond += rates.writeOps
				}
				m.prevIOCounters[uid] = current
			}
		}
	}
	metrics.CPUEligibleUsersCount = len(metrics.CPUEligibleUsers)
	metrics.RAMEligibleUsersCount = len(metrics.RAMEligibleUsers)
	metrics.IOEligibleUsersCount = len(metrics.IOEligibleUsers)

	if updateIOState {
		m.prevIOTime = sampleTime

		// Remove baselines for users that are no longer I/O eligible. If they
		// become eligible again, their first sample establishes a fresh baseline.
		ioEligibleUsers := make(map[int]struct{}, len(metrics.IOEligibleUsers))
		for _, uid := range metrics.IOEligibleUsers {
			ioEligibleUsers[uid] = struct{}{}
		}
		for uid := range m.prevIOCounters {
			if _, exists := ioEligibleUsers[uid]; !exists {
				delete(m.prevIOCounters, uid)
			}
		}
	}

	return metrics, nil
}

// calculateIORates converts monotonic cumulative counters into per-second rates.
// The operation counters originate from /proc/PID/io syscr and syscw; they are
// read/write-family syscall rates, not block-device IOPS from cgroup io.stat.
func calculateIORates(current, previous ioCounters, elapsed time.Duration) ioCountersRate {
	seconds := elapsed.Seconds()
	if seconds <= 0 {
		return ioCountersRate{}
	}
	return ioCountersRate{
		readBytes:  monotonicRate(current.readBytes, previous.readBytes, seconds),
		writeBytes: monotonicRate(current.writeBytes, previous.writeBytes, seconds),
		readOps:    monotonicRate(current.readOps, previous.readOps, seconds),
		writeOps:   monotonicRate(current.writeOps, previous.writeOps, seconds),
	}
}

type ioCountersRate struct {
	readBytes  float64
	writeBytes float64
	readOps    float64
	writeOps   float64
}

func monotonicRate(current, previous uint64, elapsedSeconds float64) float64 {
	if current < previous {
		return 0
	}
	return float64(current-previous) / elapsedSeconds
}

func (m *Manager) updatePrometheusMetrics(metrics *SystemMetrics) {
	if m.prometheusExporter == nil {
		return
	}

	m.mu.RLock()
	limitedUsers := len(m.activeUsers)
	limitsActive := m.limitsActive
	m.mu.RUnlock()

	// Metriche base per il metodo UpdateMetrics
	promMetrics := map[string]float64{
		// System metrics
		"cpu_total_usage": metrics.TotalCPUUsage,
		"total_cores":     float64(metrics.TotalCores),

		// ALL USERS metrics
		"all_users_cpu_usage":    metrics.AllUsersCPUUsage,
		"all_users_count":        float64(metrics.AllUsersCount),
		"all_users_memory_usage": float64(metrics.AllUsersMemoryUsage),

		// LIMITED USERS metrics
		"limited_users_cpu_usage":    metrics.CPUEligibleCPUUsage,
		"limited_users_count":        float64(metrics.CPUEligibleUsersCount),
		"limited_users_memory_usage": float64(metrics.CPUEligibleMemoryUsage),

		// Other metrics
		"memory_usage_mb":  metrics.MemoryUsage,
		"total_memory_mb":  metrics.TotalMemoryMB,
		"cached_memory_mb": metrics.CachedMemoryMB,
		"limited_users":    float64(limitedUsers),
		"limits_active":    boolToFloat(limitsActive),
		"system_load":      metrics.SystemLoad,
	}

	m.prometheusExporter.UpdateMetrics(promMetrics)

	// Update per-user metrics from the explicit sample state.
	for uid, userMetrics := range metrics.UserMetrics {
		username := userMetrics.Username
		if username == "" || username == strconv.Itoa(uid) {
			username = m.getUsername(uid)
		}

		// Batch cgroup reads: single call instead of 3 separate ones
		var cgroupPath, cpuQuota string
		var memoryHighEvents uint64
		var cgroupIOReadBytes, cgroupIOWriteBytes, cgroupIOReadOps, cgroupIOWriteOps uint64
		if m.cgroupManager != nil {
			var err error
			cgroupPath, cpuQuota, memoryHighEvents, cgroupIOReadBytes, cgroupIOWriteBytes, cgroupIOReadOps, cgroupIOWriteOps, err = m.cgroupManager.GetUserCgroupMetrics(uid)
			if err != nil {
				if isMissingUserCgroupError(err) {
					m.logger.Debug("Cgroup metrics unavailable for user without cgroup", "uid", uid)
				} else {
					m.logger.Warn("Failed to get cgroup metrics for user", "uid", uid, "error", err)
				}
			}
		}

		// Use per-user IO from GetAllUserMetrics
		ioReadBytes := userMetrics.IOReadBytes
		ioWriteBytes := userMetrics.IOWriteBytes
		ioReadOps := userMetrics.IOReadOps
		ioWriteOps := userMetrics.IOWriteOps
		if ioReadBytes == 0 && ioWriteBytes == 0 && cgroupIOReadBytes > 0 {
			ioReadBytes = cgroupIOReadBytes
			ioWriteBytes = cgroupIOWriteBytes
			ioReadOps = cgroupIOReadOps
			ioWriteOps = cgroupIOWriteOps
		}

		// Publish the explicit observed CPU enforcement state.
		m.prometheusExporter.UpdateUserMetrics(
			uid,
			username,
			userMetrics.CPUUsage,
			userMetrics.CPUUsageAverage,
			userMetrics.CPUUsageEMA,
			userMetrics.MemoryUsage,
			userMetrics.ProcessCount,
			userMetrics.CPULimitActive,
			cgroupPath,
			cpuQuota,
			memoryHighEvents,
			ioReadBytes,
			ioWriteBytes,
			ioReadOps,
			ioWriteOps,
		)
	}

	// Remove metric series for users absent from the current sample.
	activeUids := make(map[int]bool)
	for uid := range metrics.UserMetrics {
		activeUids[uid] = true
	}
	m.prometheusExporter.CleanupUserMetrics(activeUids)

	// Update system metrics.
	actionCores := metrics.TotalCores - m.GetConfig().GetMinSystemCores()
	if actionCores < 1 {
		actionCores = 1
	}
	m.prometheusExporter.UpdateSystemMetrics(metrics.TotalCores, actionCores, metrics.SystemLoad)
}

const (
	metricsCollectionErrorComponent    = "metrics_collection"
	metricsCollectionSystemLoadError   = "system_load_failure"
	metricsDatabaseErrorComponent      = "metrics_database"
	metricsDatabaseWriteFailure        = "write_failure"
	limitTransitionErrorComponent      = "limit_transition"
	limitTransitionActivationFailure   = "activation_failure"
	limitTransitionDeactivationFailure = "deactivation_failure"
)

// writeDatabaseMetrics persists one collection cycle without blocking enforcement on failure.
func (m *Manager) writeDatabaseMetrics(metrics *SystemMetrics) {
	if m.metricsCollector == nil {
		return
	}

	// Verifica se il DB writer è configurato
	writer := m.metricsCollector.GetDBWriter()
	if writer == nil {
		return
	}

	// Verifica se è il momento di scrivere
	if !writer.ShouldWrite() {
		return
	}

	m.mu.RLock()
	limitsActive := m.limitsActive
	activeUsers := len(m.activeUsers)
	m.mu.RUnlock()

	if err := m.metricsCollector.WriteMetricsToDatabase(
		metrics.UserMetrics,
		metrics.TotalCPUUsage,
		metrics.TotalCores,
		metrics.SystemLoad,
		limitsActive,
		activeUsers,
	); err != nil {
		m.logger.Warn("Failed to write metrics to database",
			"users", len(metrics.UserMetrics),
			"limits_active", limitsActive,
			"error", err,
		)
		if m.prometheusExporter != nil {
			m.prometheusExporter.RecordError(metricsDatabaseErrorComponent, metricsDatabaseWriteFailure)
		}
		return
	}

	m.logger.Debug("Metrics written to database",
		"users", len(metrics.UserMetrics),
		"limits_active", limitsActive,
	)
}

type ControlCycleEntry struct {
	Timestamp     time.Time `json:"timestamp"`
	Decision      string    `json:"decision"`
	Reason        string    `json:"reason"`
	TotalCPUUsage float64   `json:"total_cpu_usage"`
	UserCPUUsage  float64   `json:"user_cpu_usage"`
	ActiveUsers   int       `json:"active_users"`
	LimitsActive  bool      `json:"limits_active"`
	DurationMs    int64     `json:"duration_ms"`
}

// controlHistory stores recent control cycle entries
type controlHistory struct {
	entries []ControlCycleEntry
	mu      sync.RWMutex
	maxSize int
}

func (m *Manager) addControlHistoryEntry(entry ControlCycleEntry) {
	m.controlHist.mu.Lock()
	defer m.controlHist.mu.Unlock()

	m.controlHist.entries = append(m.controlHist.entries, entry)

	// Keep only the last maxSize entries
	if len(m.controlHist.entries) > m.controlHist.maxSize {
		m.controlHist.entries = m.controlHist.entries[len(m.controlHist.entries)-m.controlHist.maxSize:]
	}
}

func (m *Manager) GetControlHistory(limit int) []ControlCycleEntry {
	m.controlHist.mu.RLock()
	defer m.controlHist.mu.RUnlock()

	if limit <= 0 || limit > len(m.controlHist.entries) {
		limit = len(m.controlHist.entries)
	}

	// Return the most recent entries
	start := len(m.controlHist.entries) - limit
	if start < 0 {
		start = 0
	}

	result := make([]ControlCycleEntry, limit)
	copy(result, m.controlHist.entries[start:])
	return result
}

func (m *Manager) recordControlCycle(decision, reason string, metrics *SystemMetrics, duration time.Duration) {
	m.mu.RLock()
	limitsActive := m.limitsActive
	m.mu.RUnlock()

	entry := ControlCycleEntry{
		Timestamp:     time.Now(),
		Decision:      decision,
		Reason:        reason,
		TotalCPUUsage: metrics.TotalCPUUsage,
		UserCPUUsage:  metrics.CPUEligibleCPUUsage,
		ActiveUsers:   len(metrics.UserCPUUsage),
		LimitsActive:  limitsActive,
		DurationMs:    duration.Milliseconds(),
	}

	m.addControlHistoryEntry(entry)
}
