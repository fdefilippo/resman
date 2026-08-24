package state

import (
	"context"
	"errors"
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
	deferredErrors     []error
}

type controlCycleStage struct {
	name               string
	run                func(*Manager, *controlCycleContext) error
	continueAfterError bool
}

var defaultControlCyclePipeline = []controlCycleStage{
	{name: "check_blackout", run: (*Manager).stageCheckBlackout},
	{name: "collect_metrics", run: (*Manager).stageCollectMetrics},
	{name: "update_prometheus", run: (*Manager).stageUpdatePrometheus},
	{name: "write_database", run: (*Manager).stageWriteDatabase},
	{name: "make_decision", run: (*Manager).stageMakeDecision},
	{name: "execute_decision", run: (*Manager).stageExecuteDecision, continueAfterError: true},
	{name: "record_history", run: (*Manager).stageRecordHistory},
	{name: "io_remediation", run: (*Manager).stageIORemediation},
	{name: "workload_pattern_detection", run: (*Manager).stageWorkloadPatternDetection},
	{name: "revert_psi_boosts", run: (*Manager).stageRevertPSIBoosts},
	{name: "log_completion", run: (*Manager).stageLogCompletion},
}

func (m *Manager) RunControlCycle(ctx context.Context) error {
	return m.RunControlCycleWithTrigger(ctx, ControlCycleTriggerManual)
}

// RunMetricsRefresh refreshes Prometheus metrics without running a decision cycle.
func (m *Manager) RunMetricsRefresh(ctx context.Context, trigger string) error {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()

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
		m.updatePrometheusSystemMetrics(metrics)
	}

	m.logger.Debug("Metrics refresh completed",
		"trigger", trigger,
		"duration_ms", time.Since(startTime).Milliseconds(),
	)

	return nil
}

// RunControlCycleWithTrigger executes one control cycle for the supplied trigger.
func (m *Manager) RunControlCycleWithTrigger(ctx context.Context, trigger string) error {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()

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

	return runControlCyclePipeline(m, run, defaultControlCyclePipeline)
}

func runControlCyclePipeline(m *Manager, run *controlCycleContext, stages []controlCycleStage) error {
	var cycleErrors []error
	for _, stage := range stages {
		if err := stage.run(m, run); err != nil {
			cycleErrors = append(cycleErrors, err)
			if !stage.continueAfterError {
				return errors.Join(cycleErrors...)
			}
			run.deferredErrors = append(run.deferredErrors, err)
		}
		if run.stopWithoutError {
			return errors.Join(cycleErrors...)
		}
	}

	return errors.Join(cycleErrors...)
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
	// Publish system observations and decision-owned per-user metrics.
	if m.prometheusExporter != nil {
		m.updatePrometheusSystemMetrics(run.metrics)
		m.updatePrometheusDecisionUserMetrics(run.metrics)
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
	// Execute the selected enforcement action. The application-level caller owns
	// the single cycle failure log after protective stages have completed.
	if err := m.executeDecision(run.decision, run.metrics); err != nil {
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
	// Pattern detection is an enforcement input and must consume the control
	// cycle's authoritative decision snapshot, not an observation-only re-read.
	allMetrics := run.metrics.UserMetrics
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

	// Analyze patterns once per hour.
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

	outcome := "success"
	if len(run.deferredErrors) > 0 {
		outcome = "degraded"
	}

	// Log the complete cycle outcome after all protective stages have run.
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
		"outcome", outcome,
		"deferred_error_count", len(run.deferredErrors),
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
	IOEligibleUnavailableProcesses   int

	// Current procfs coverage failures across all observed processes.
	ProcFSExecutableIdentityUnavailableProcesses int
	ProcFSIOUnavailableProcesses                 int

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
	return m.collectSystemMetricsForPurpose(true)
}

func (m *Manager) collectSystemMetricsForRefresh() (*SystemMetrics, error) {
	return m.collectSystemMetricsForPurpose(false)
}

func (m *Manager) collectSystemMetricsForPurpose(decisionSample bool) (*SystemMetrics, error) {
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

	// Decision samples own temporal enforcement state. Observation refreshes use
	// a separate stream and cannot populate or advance the decision stream.
	var allUserMetrics map[int]*resmanmetrics.UserMetrics
	if decisionSample {
		allUserMetrics = m.metricsCollector.GetAllUserMetricsForDecision()
	} else {
		allUserMetrics = m.metricsCollector.GetAllUserMetrics()
	}

	// Compute total and per-resource eligible-user aggregates in one pass.
	for uid, um := range allUserMetrics {
		metrics.AllUsersCPUUsage += um.CPUUsage
		metrics.AllUsersMemoryUsage += um.MemoryUsage
		metrics.AllUsersCount++
		metrics.ProcFSExecutableIdentityUnavailableProcesses += um.ExecutableIdentityUnavailableProcesses
		metrics.ProcFSIOUnavailableProcesses += um.IOUnavailableProcesses

		metrics.UserCPUUsage[uid] = um.CPUUsage

		limitState := m.GetUserLimitState(uid, um.Username)

		corrected := &resmanmetrics.UserMetrics{
			UID:                                    um.UID,
			Username:                               um.Username,
			CPUUsage:                               um.CPUUsage,
			CPUUsageAverage:                        um.CPUUsageAverage,
			CPUUsageEMA:                            um.CPUUsageEMA,
			MemoryUsage:                            um.MemoryUsage,
			ProcessCount:                           um.ProcessCount,
			EligibleForCPU:                         limitState.EligibleForCPU,
			EligibleForRAM:                         limitState.EligibleForRAM,
			EligibleForIO:                          limitState.EligibleForIO,
			CPULimitRequested:                      limitState.CPULimitRequested,
			CPULimitActive:                         limitState.CPULimitActive,
			RAMLimitRequested:                      limitState.RAMLimitRequested,
			RAMLimitActive:                         limitState.RAMLimitActive,
			IOLimitRequested:                       limitState.IOLimitRequested,
			IOLimitActive:                          limitState.IOLimitActive,
			IOReadBytes:                            um.IOReadBytes,
			IOWriteBytes:                           um.IOWriteBytes,
			IOReadOps:                              um.IOReadOps,
			IOWriteOps:                             um.IOWriteOps,
			ExecutableIdentityUnavailableProcesses: um.ExecutableIdentityUnavailableProcesses,
			IOUnavailableProcesses:                 um.IOUnavailableProcesses,
			EnforceableUsage:                       um.EnforceableUsage,
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
			metrics.IOEligibleUnavailableProcesses += um.EnforceableUsage.IOUnavailableProcesses
			if decisionSample && !m.prevIOTime.IsZero() {
				if _, wasEligible := m.previousIOEligibleUsers[uid]; wasEligible {
					rates := calculateIORates(um.EnforceableUsage.IODelta, sampleTime.Sub(m.prevIOTime))
					metrics.IOEligibleReadBPS += rates.readBytes
					metrics.IOEligibleWriteBPS += rates.writeBytes
					metrics.IOEligibleReadSyscallsPerSecond += rates.readOps
					metrics.IOEligibleWriteSyscallsPerSecond += rates.writeOps
				}
			}
		}
	}
	metrics.CPUEligibleUsersCount = len(metrics.CPUEligibleUsers)
	metrics.RAMEligibleUsersCount = len(metrics.RAMEligibleUsers)
	metrics.IOEligibleUsersCount = len(metrics.IOEligibleUsers)

	if decisionSample {
		m.prevIOTime = sampleTime
		m.previousIOEligibleUsers = make(map[int]struct{}, len(metrics.IOEligibleUsers))
		for _, uid := range metrics.IOEligibleUsers {
			m.previousIOEligibleUsers[uid] = struct{}{}
		}
	}

	return metrics, nil
}

// calculateIORates converts per-process counter growth into per-second rates.
// The operation counters originate from /proc/PID/io syscr and syscw; they are
// read/write-family syscall rates, not block-device IOPS from cgroup io.stat.
func calculateIORates(delta resmanmetrics.ProcessIODelta, elapsed time.Duration) ioCountersRate {
	seconds := elapsed.Seconds()
	if seconds <= 0 {
		return ioCountersRate{}
	}
	return ioCountersRate{
		readBytes:  float64(delta.ReadBytes) / seconds,
		writeBytes: float64(delta.WriteBytes) / seconds,
		readOps:    float64(delta.ReadOps) / seconds,
		writeOps:   float64(delta.WriteOps) / seconds,
	}
}

type ioCountersRate struct {
	readBytes  float64
	writeBytes float64
	readOps    float64
	writeOps   float64
}

func (m *Manager) updatePrometheusSystemMetrics(metrics *SystemMetrics) {
	if m.prometheusExporter == nil {
		return
	}

	m.mu.RLock()
	limitedUsers := len(m.activeUsers)
	limitsActive := m.limitsActive
	m.mu.RUnlock()

	m.prometheusExporter.UpdateSystemSnapshot(resmanmetrics.ExporterMetrics{
		TotalCPUUsage:                metrics.TotalCPUUsage,
		TotalCores:                   metrics.TotalCores,
		ObservedUsersCPUUsage:        metrics.AllUsersCPUUsage,
		ObservedUsersCount:           metrics.AllUsersCount,
		ObservedUsersMemoryUsage:     metrics.AllUsersMemoryUsage,
		CPUEligibleUsersCPUUsage:     metrics.CPUEligibleCPUUsage,
		CPUEligibleUsersCount:        metrics.CPUEligibleUsersCount,
		CPUEligibleUsersMemoryUsage:  metrics.CPUEligibleMemoryUsage,
		CPUActivelyLimitedUsersCount: limitedUsers,
		CPULimitsActive:              limitsActive,
		MemoryUsageMB:                metrics.MemoryUsage,
		TotalMemoryMB:                metrics.TotalMemoryMB,
		CachedMemoryMB:               metrics.CachedMemoryMB,
		SystemLoad:                   metrics.SystemLoad,
		ProcFSExecutableIdentityUnavailableProcesses: metrics.ProcFSExecutableIdentityUnavailableProcesses,
		ProcFSIOUnavailableProcesses:                 metrics.ProcFSIOUnavailableProcesses,
	})

	// Update system metrics.
	actionCores := metrics.TotalCores - m.GetConfig().GetMinSystemCores()
	if actionCores < 1 {
		actionCores = 1
	}
	m.prometheusExporter.UpdateSystemMetrics(metrics.TotalCores, actionCores, metrics.SystemLoad)
}

func (m *Manager) updatePrometheusDecisionUserMetrics(metrics *SystemMetrics) {
	if m.prometheusExporter == nil {
		return
	}

	// Per-user series are owned exclusively by the decision sample. Observation
	// refreshes must not overwrite them with a different baseline or EMA history.
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
}

const (
	metricsCollectionErrorComponent    = "metrics_collection"
	metricsCollectionSystemLoadError   = "system_load_failure"
	metricsDatabaseErrorComponent      = "metrics_database"
	metricsDatabaseWriteFailure        = "write_failure"
	limitTransitionErrorComponent      = "limit_transition"
	limitTransitionActivationFailure   = "activation_failure"
	limitTransitionDeactivationFailure = "deactivation_failure"
	processMembershipErrorComponent    = "process_membership"
	processMembershipReconcileFailure  = "reconciliation_failure"
	processMembershipOriginUnavailable = "origin_unavailable"
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
	Timestamp                    time.Time `json:"timestamp"`
	Decision                     string    `json:"decision"`
	Reason                       string    `json:"reason"`
	TotalCPUUsage                float64   `json:"total_cpu_usage"`
	CPUEligibleCPUUsage          float64   `json:"cpu_eligible_users_cpu_usage"`
	ObservedUsersCount           int       `json:"observed_users_count"`
	CPUActivelyLimitedUsersCount int       `json:"cpu_actively_limited_users_count"`
	CPULimitsActive              bool      `json:"cpu_limits_active"`
	DurationMs                   int64     `json:"duration_ms"`
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
	activelyLimitedUsers := len(m.activeUsers)
	m.mu.RUnlock()

	entry := ControlCycleEntry{
		Timestamp:                    time.Now(),
		Decision:                     decision,
		Reason:                       reason,
		TotalCPUUsage:                metrics.TotalCPUUsage,
		CPUEligibleCPUUsage:          metrics.CPUEligibleCPUUsage,
		ObservedUsersCount:           len(metrics.UserMetrics),
		CPUActivelyLimitedUsersCount: activelyLimitedUsers,
		CPULimitsActive:              limitsActive,
		DurationMs:                   duration.Milliseconds(),
	}

	m.addControlHistoryEntry(entry)
}
