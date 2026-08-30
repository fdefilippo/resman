package state

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/fdefilippo/resman/cgroup"
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
	degradedErrors     []error
	degradedWarnings   []error
}

type controlCycleStage struct {
	name               string
	run                func(*Manager, *controlCycleContext) error
	continueAfterError bool
}

type patternPolicyError struct {
	uid int
	err error
}

func (e *patternPolicyError) Error() string {
	return fmt.Sprintf("reconcile workload pattern policy for UID %d: %v", e.uid, e.err)
}

func (e *patternPolicyError) Unwrap() error {
	return e.err
}

var defaultControlCyclePipeline = []controlCycleStage{
	{name: "check_blackout", run: (*Manager).stageCheckBlackout},
	{name: "collect_metrics", run: (*Manager).stageCollectMetrics},
	{name: "update_prometheus", run: (*Manager).stageUpdatePrometheus},
	{name: "write_database", run: (*Manager).stageWriteDatabase},
	{name: "make_decision", run: (*Manager).stageMakeDecision},
	{name: "execute_decision", run: (*Manager).stageExecuteDecision, continueAfterError: true},
	{name: "record_history", run: (*Manager).stageRecordHistory},
	{name: "io_remediation", run: (*Manager).stageIORemediation, continueAfterError: true},
	{name: "workload_pattern_detection", run: (*Manager).stageWorkloadPatternDetection, continueAfterError: true},
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

	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

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

	if err := m.logger.DebugChecked("Metrics refresh completed",
		"trigger", trigger,
		"duration_ms", time.Since(startTime).Milliseconds(),
	); err != nil {
		return fmt.Errorf("write metrics-refresh completion log: %w", err)
	}

	return nil
}

// RunControlCycleWithTrigger executes one control cycle for the supplied trigger.
func (m *Manager) RunControlCycleWithTrigger(ctx context.Context, trigger string) error {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()

	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

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

	if err := m.logger.DebugChecked("Starting control cycle", "cycle_id", run.cycleID, "trigger", trigger); err != nil {
		run.degradedErrors = append(run.degradedErrors, fmt.Errorf("write control-cycle start log: %w", err))
	}

	return runControlCyclePipeline(m, run, defaultControlCyclePipeline)
}

func runControlCyclePipeline(m *Manager, run *controlCycleContext, stages []controlCycleStage) error {
	var cycleErrors []error
	for _, stage := range stages {
		if err := stage.run(m, run); err != nil {
			cycleErrors = append(cycleErrors, err)
			if !stage.continueAfterError {
				return errors.Join(errors.Join(cycleErrors...), errors.Join(run.degradedErrors...))
			}
			run.deferredErrors = append(run.deferredErrors, err)
		}
		if run.stopWithoutError {
			return errors.Join(errors.Join(cycleErrors...), errors.Join(run.degradedErrors...))
		}
	}

	return errors.Join(errors.Join(cycleErrors...), errors.Join(run.degradedErrors...))
}

func (m *Manager) stageCheckBlackout(run *controlCycleContext) error {
	// Check whether the current time is within a blackout window.
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
	// 1. Collect system metrics.
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
	run.degradedErrors = append(run.degradedErrors, metrics.blockIOObservationErrors...)
	run.degradedWarnings = append(run.degradedWarnings, metrics.blockIOObservationWarnings...)
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
	// 3. Write metrics to the database when enabled.
	m.writeDatabaseMetrics(run.metrics)
	return nil
}

func (m *Manager) stageMakeDecision(run *controlCycleContext) error {
	// 4. Make a decision from the collected metrics.
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
	// 6. Record the control-cycle history.
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
			sort.Ints(limitedUsers)
		}
		remediationErrors := m.ioRemediation.CheckAndRemediate(m.cgroupManager, run.cfg, limitedUsers)
		for _, err := range remediationErrors {
			var remediationErr *ioRemediationError
			if errors.As(err, &remediationErr) && m.prometheusExporter != nil {
				m.prometheusExporter.RecordError(ioRemediationErrorComponent, remediationErr.operation)
			}
		}
		// Remove stale remediation state periodically.
		m.ioRemediation.Cleanup(24 * time.Hour)
		return errors.Join(remediationErrors...)
	}
	return nil
}

func (m *Manager) stageWorkloadPatternDetection(run *controlCycleContext) error {
	// 8. Workload Pattern Detection
	if m.patternDetector == nil || m.policyEngine == nil {
		return nil
	}
	toReconcile := m.pendingPatternReconciliationSnapshot()

	if !run.cfg.GetAutodetectPatterns() {
		for _, uid := range m.policyEngine.Clear() {
			toReconcile[uid] = struct{}{}
		}
		return m.reconcilePatternPolicies(toReconcile, run.cfg)
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
		toReconcile[uid] = struct{}{}
	}

	// Analyze patterns once per hour.
	if time.Since(m.lastPatternAnalysis) <= time.Hour {
		return m.reconcilePatternPolicies(toReconcile, run.cfg)
	}

	m.lastPatternAnalysis = time.Now()
	for _, uid := range m.patternDetector.Cleanup(time.Duration(run.cfg.GetPatternHistoryHours()) * time.Hour) {
		if m.policyEngine.RemovePolicy(uid) {
			toReconcile[uid] = struct{}{}
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
			toReconcile[uid] = struct{}{}
		}
	}

	return m.reconcilePatternPolicies(toReconcile, run.cfg)
}

func (m *Manager) pendingPatternReconciliationSnapshot() map[int]struct{} {
	m.mu.RLock()
	pending := make(map[int]struct{}, len(m.pendingPatternReconciliations))
	for uid := range m.pendingPatternReconciliations {
		pending[uid] = struct{}{}
	}
	m.mu.RUnlock()
	return pending
}

func (m *Manager) reconcilePatternPolicies(uids map[int]struct{}, cfg *config.Config) error {
	ordered := make([]int, 0, len(uids))
	for uid := range uids {
		ordered = append(ordered, uid)
	}
	sort.Ints(ordered)

	var reconcileErrors []error
	for _, uid := range ordered {
		err := m.reconcilePatternPolicy(uid, cfg)
		m.mu.Lock()
		if err != nil {
			if m.pendingPatternReconciliations == nil {
				m.pendingPatternReconciliations = make(map[int]struct{})
			}
			m.pendingPatternReconciliations[uid] = struct{}{}
		} else {
			delete(m.pendingPatternReconciliations, uid)
		}
		m.mu.Unlock()
		if err == nil {
			continue
		}
		reconcileErrors = append(reconcileErrors, err)
		if m.prometheusExporter != nil {
			m.prometheusExporter.RecordError(patternPolicyErrorComponent, patternPolicyApplicationFailure)
		}
	}
	return errors.Join(reconcileErrors...)
}

func (m *Manager) reconcilePatternPolicy(uid int, cfg *config.Config) error {
	if !m.isUserLimited(uid) {
		return nil
	}
	err := m.applyUserResourceLimits(uid, cfg, cfg.EvaluateUserEligibility(m.getUsername(uid)))
	m.mu.Lock()
	m.refreshResourceLimitsActiveLocked(time.Now())
	m.mu.Unlock()
	if err != nil {
		return &patternPolicyError{uid: uid, err: err}
	}
	m.logger.Info("Workload pattern resource limits reconciled", "uid", uid)
	return nil
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
	if len(run.deferredErrors) > 0 || len(run.degradedErrors) > 0 || len(run.degradedWarnings) > 0 {
		outcome = "degraded"
	}

	// Log the complete cycle outcome after all protective stages have run.
	if err := m.logger.InfoChecked("Control cycle completed",
		"cycle_id", run.cycleID,
		"trigger", run.trigger,
		"decision", run.decision,
		"reason", run.reason,
		"total_cpu_usage", run.metrics.TotalCPUUsage,
		"cpu_eligible_users_cpu_usage", run.metrics.CPUEligibleCPUUsage,
		"eligible_users", run.metrics.CPUEligibleUsersCount,
		"active_limited_users", run.activeLimitedUsers,
		"system_under_load", run.metrics.SystemUnderLoad,
		"ignore_system_load", run.cfg.GetIgnoreSystemLoad(),
		"duration_ms", run.duration.Milliseconds(),
		"outcome", outcome,
		"deferred_error_count", len(run.deferredErrors)+len(run.degradedErrors),
		"degraded_warning_count", len(run.degradedWarnings),
	); err != nil {
		return fmt.Errorf("write control-cycle completion log: %w", err)
	}

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
	CPUEligibleCPUUsage            float64
	CPUEligibleMemoryUsage         uint64
	CPUEligibleUsersCount          int
	RAMEligibleUsersCount          int
	IOEligibleUsersCount           int
	RAMEligibleUsageBytes          uint64
	IOEligibleReadBPS              float64
	IOEligibleWriteBPS             float64
	IOEligibleReadBlockIOPS        float64
	IOEligibleWriteBlockIOPS       float64
	IOBlockIOPSUnavailableUsers    int
	IOEligibleUnavailableProcesses int
	blockIOObservationErrors       []error
	blockIOObservationWarnings     []error

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
					rates := calculateIOByteRates(um.EnforceableUsage.IODelta, sampleTime.Sub(m.prevIOTime))
					metrics.IOEligibleReadBPS += rates.readBytes
					metrics.IOEligibleWriteBPS += rates.writeBytes
				}
			}
		}
	}
	metrics.CPUEligibleUsersCount = len(metrics.CPUEligibleUsers)
	metrics.RAMEligibleUsersCount = len(metrics.RAMEligibleUsers)
	metrics.IOEligibleUsersCount = len(metrics.IOEligibleUsers)

	if decisionSample {
		decisionConfig := m.GetConfig()
		m.collectEligibleBlockIOPS(metrics, sampleTime, decisionConfig.GetIODecisionPolicy(), decisionConfig.CPUQuotaNormal)
		m.prevIOTime = sampleTime
		m.previousIOEligibleUsers = make(map[int]struct{}, len(metrics.IOEligibleUsers))
		for _, uid := range metrics.IOEligibleUsers {
			m.previousIOEligibleUsers[uid] = struct{}{}
		}
	}

	return metrics, nil
}

type blockIOCounterSample struct {
	readOps  uint64
	writeOps uint64
}

func (m *Manager) collectEligibleBlockIOPS(metrics *SystemMetrics, sampleTime time.Time, policy config.IODecisionPolicy, normalQuota string) {
	needsBlockIO := policy.Enabled && (policy.ReadIOPS > 0 || policy.WriteIOPS > 0)
	desired := make(map[int]bool)
	if needsBlockIO {
		for _, uid := range metrics.IOEligibleUsers {
			desired[uid] = true
		}
	}

	m.mu.RLock()
	sharedPath := m.sharedCgroupPath
	activeUsers := make(map[int]bool, len(m.activeUsers))
	for uid := range m.activeUsers {
		activeUsers[uid] = true
	}
	observedBefore := make(map[int]bool, len(m.blockIOObservedUsers))
	for uid := range m.blockIOObservedUsers {
		observedBefore[uid] = true
	}
	resourceStates := make(map[int]userResourceLimitState, len(m.resourceLimits))
	for uid, state := range m.resourceLimits {
		resourceStates[uid] = state
	}
	m.mu.RUnlock()

	uids := append([]int(nil), metrics.IOEligibleUsers...)
	sort.Ints(uids)
	current := make(map[int]blockIOCounterSample, len(uids))
	observedNext := make(map[int]bool, len(desired)+len(observedBefore))
	for uid := range desired {
		observedNext[uid] = true
	}
	for _, uid := range uids {
		if !desired[uid] {
			continue
		}
		placement := ""
		if activeUsers[uid] {
			placement = sharedPath
		}
		_, ingress, err := m.cgroupManager.EnsureUserCgroupPlacement(uid, placement, normalQuota)
		m.recordCgroupIngressSkips(ingress)
		if err == nil && ingress.NamespaceSkipped() > 0 && !ingress.Applied() {
			err = cgroupIngressNoopError(uid, ingress)
		}
		if err != nil {
			metrics.IOBlockIOPSUnavailableUsers++
			var incomplete *cgroup.UserCgroupPlacementIncompleteError
			if errors.As(err, &incomplete) {
				m.recordBlockIOObservationIssue(metrics, uid, blockIOObservationPlacementIncomplete, err, true)
			} else {
				m.recordBlockIOObservationIssue(metrics, uid, blockIOObservationPlacementFailure, err, false)
			}
			continue
		}
		_, _, readOps, writeOps, err := m.cgroupManager.GetIOStats(uid)
		if err != nil {
			metrics.IOBlockIOPSUnavailableUsers++
			m.recordBlockIOObservationIssue(metrics, uid, blockIOObservationCounterReadFailure, err, false)
			continue
		}
		now := blockIOCounterSample{readOps: readOps, writeOps: writeOps}
		current[uid] = now
		previous, hadPrevious := m.previousBlockIOCounters[uid]
		_, wasEligible := m.previousIOEligibleUsers[uid]
		if hadPrevious && wasEligible && !m.prevIOTime.IsZero() {
			seconds := sampleTime.Sub(m.prevIOTime).Seconds()
			if seconds > 0 {
				metrics.IOEligibleReadBlockIOPS += float64(monotonicUint64Delta(now.readOps, previous.readOps)) / seconds
				metrics.IOEligibleWriteBlockIOPS += float64(monotonicUint64Delta(now.writeOps, previous.writeOps)) / seconds
			} else {
				metrics.IOBlockIOPSUnavailableUsers++
			}
		} else {
			metrics.IOBlockIOPSUnavailableUsers++
		}
	}

	for uid := range observedBefore {
		if desired[uid] || activeUsers[uid] {
			continue
		}
		state := resourceStates[uid]
		if state.ramApplied || state.ioApplied || state.standalone {
			continue
		}
		if err := m.cgroupManager.CleanupUserCgroup(uid); err != nil {
			observedNext[uid] = true
			m.recordBlockIOObservationIssue(metrics, uid, blockIOObservationCleanupFailure, err, false)
		}
	}

	m.previousBlockIOCounters = current
	m.mu.Lock()
	m.blockIOObservedUsers = observedNext
	m.mu.Unlock()
}

func (m *Manager) recordBlockIOObservationIssue(metrics *SystemMetrics, uid int, errorType string, err error, warning bool) {
	issue := fmt.Errorf("block I/O observation for UID %d (%s): %w", uid, errorType, err)
	if warning {
		metrics.blockIOObservationWarnings = append(metrics.blockIOObservationWarnings, issue)
		m.logger.Warn("Block I/O observation placement remains split; retrying next cycle",
			"uid", uid,
			"error", err,
		)
	} else {
		metrics.blockIOObservationErrors = append(metrics.blockIOObservationErrors, issue)
	}
	if m.prometheusExporter != nil {
		m.prometheusExporter.RecordError(blockIOObservationErrorComponent, errorType)
	}
}

func monotonicUint64Delta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

// calculateIOByteRates converts per-process counter growth into per-second rates.
// Byte counters originate from per-process /proc/PID/io deltas. Block IOPS are
// collected separately from the user's observation cgroup.
func calculateIOByteRates(delta resmanmetrics.ProcessIODelta, elapsed time.Duration) ioByteRate {
	seconds := elapsed.Seconds()
	if seconds <= 0 {
		return ioByteRate{}
	}
	return ioByteRate{
		readBytes:  float64(delta.ReadBytes) / seconds,
		writeBytes: float64(delta.WriteBytes) / seconds,
	}
}

type ioByteRate struct {
	readBytes  float64
	writeBytes float64
}

func (m *Manager) updatePrometheusSystemMetrics(metrics *SystemMetrics) {
	if m.prometheusExporter == nil {
		return
	}

	summary := m.getEnforcementSummary()

	actionCores := metrics.TotalCores - m.GetConfig().GetMinSystemCores()
	if actionCores < 1 {
		actionCores = 1
	}
	m.prometheusExporter.UpdateSystemSnapshot(resmanmetrics.SystemExporterMetrics{
		TotalCPUUsage:                                metrics.TotalCPUUsage,
		TotalCores:                                   metrics.TotalCores,
		ActionCores:                                  actionCores,
		ObservedUsersCPUUsage:                        metrics.AllUsersCPUUsage,
		ObservedUsersCount:                           metrics.AllUsersCount,
		ObservedUsersMemoryUsage:                     metrics.AllUsersMemoryUsage,
		CPUEligibleUsersCPUUsage:                     metrics.CPUEligibleCPUUsage,
		CPUEligibleUsersCount:                        metrics.CPUEligibleUsersCount,
		CPUEligibleUsersMemoryUsage:                  metrics.CPUEligibleMemoryUsage,
		RAMEligibleUsersCount:                        metrics.RAMEligibleUsersCount,
		RAMEligibleUsersMemoryUsage:                  metrics.RAMEligibleUsageBytes,
		IOEligibleUsersCount:                         metrics.IOEligibleUsersCount,
		IOEligibleUsersReadBytesPerSecond:            metrics.IOEligibleReadBPS,
		IOEligibleUsersWriteBytesPerSecond:           metrics.IOEligibleWriteBPS,
		IOEligibleUsersReadBlockOperationsPerSecond:  metrics.IOEligibleReadBlockIOPS,
		IOEligibleUsersWriteBlockOperationsPerSecond: metrics.IOEligibleWriteBlockIOPS,
		CPUActivelyLimitedUsersCount:                 len(summary.cpuUsers),
		ActivelyLimitedUsersCount:                    len(summary.activelyLimitedUsers),
		CPULimitsActive:                              summary.cpuLimitsActive,
		ResourceLimitsActive:                         summary.resourceLimitsActive,
		AnyLimitsActive:                              summary.cpuLimitsActive || summary.resourceLimitsActive,
		MemoryUsageMB:                                metrics.MemoryUsage,
		TotalMemoryMB:                                metrics.TotalMemoryMB,
		CachedMemoryMB:                               metrics.CachedMemoryMB,
		SystemLoad:                                   metrics.SystemLoad,
		ProcFSExecutableIdentityUnavailableProcesses: metrics.ProcFSExecutableIdentityUnavailableProcesses,
		ProcFSIOUnavailableProcesses:                 metrics.ProcFSIOUnavailableProcesses,
	})

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
		var cgroupIOReadBytes, cgroupIOWriteBytes uint64
		if m.cgroupManager != nil {
			var err error
			cgroupPath, cpuQuota, memoryHighEvents, cgroupIOReadBytes, cgroupIOWriteBytes, _, _, err = m.cgroupManager.GetUserCgroupMetrics(uid)
			if err != nil {
				if isMissingUserCgroupError(err) {
					m.logger.Debug("Cgroup metrics unavailable for user without cgroup", "uid", uid)
				} else {
					m.logger.Warn("Failed to get cgroup metrics for user", "uid", uid, "error", err)
				}
			}
		}
		ioReadBytes := userMetrics.IOReadBytes
		ioWriteBytes := userMetrics.IOWriteBytes
		if ioReadBytes == 0 && ioWriteBytes == 0 && cgroupIOReadBytes > 0 {
			ioReadBytes = cgroupIOReadBytes
			ioWriteBytes = cgroupIOWriteBytes
		}

		// Publish the explicit observed CPU enforcement state.
		m.prometheusExporter.UpdateUserSnapshot(resmanmetrics.UserExporterMetrics{
			UID:                  uid,
			Username:             username,
			CPUUsagePercent:      userMetrics.CPUUsage,
			CPUUsageAverage:      userMetrics.CPUUsageAverage,
			CPUUsageEMA:          userMetrics.CPUUsageEMA,
			MemoryUsageBytes:     userMetrics.MemoryUsage,
			ProcessCount:         userMetrics.ProcessCount,
			CPULimitActive:       userMetrics.CPULimitActive,
			CgroupPath:           cgroupPath,
			CPUQuota:             cpuQuota,
			MemoryHighEvents:     memoryHighEvents,
			ObservedIOReadBytes:  ioReadBytes,
			ObservedIOWriteBytes: ioWriteBytes,
			ObservedIOReadOps:    userMetrics.IOReadOps,
			ObservedIOWriteOps:   userMetrics.IOWriteOps,
		})
	}

	// Remove metric series for users absent from the current sample.
	activeUids := make(map[int]bool)
	for uid := range metrics.UserMetrics {
		activeUids[uid] = true
	}
	m.prometheusExporter.CleanupUserMetrics(activeUids)
}

const (
	metricsCollectionErrorComponent       = "metrics_collection"
	metricsCollectionSystemLoadError      = "system_load_failure"
	metricsDatabaseErrorComponent         = "metrics_database"
	metricsDatabaseWriteFailure           = "write_failure"
	limitTransitionErrorComponent         = "limit_transition"
	limitTransitionActivationFailure      = "activation_failure"
	limitTransitionDeactivationFailure    = "deactivation_failure"
	processMembershipErrorComponent       = "process_membership"
	processMembershipReconcileFailure     = "reconciliation_failure"
	processMembershipOriginUnavailable    = "origin_unavailable"
	blockIOObservationErrorComponent      = "block_io_observation"
	blockIOObservationPlacementFailure    = "placement_failure"
	blockIOObservationPlacementIncomplete = "placement_incomplete"
	blockIOObservationCounterReadFailure  = "counter_read_failure"
	blockIOObservationCleanupFailure      = "cleanup_failure"
	ioRemediationErrorComponent           = "io_remediation"
	patternPolicyErrorComponent           = "pattern_policy"
	patternPolicyApplicationFailure       = "application_failure"
)

// writeDatabaseMetrics persists one collection cycle without blocking enforcement on failure.
func (m *Manager) writeDatabaseMetrics(metrics *SystemMetrics) {
	if m.metricsCollector == nil {
		return
	}

	// Skip persistence when no database writer is configured.
	writer := m.metricsCollector.GetDBWriter()
	if writer == nil {
		return
	}

	// Respect the configured persistence cadence.
	if !writer.ShouldWrite() {
		return
	}

	summary := m.getEnforcementSummary()

	if err := m.metricsCollector.WriteMetricsToDatabase(
		metrics.UserMetrics,
		resmanmetrics.SystemPersistenceMetrics{
			TotalCPUUsagePercent:         metrics.TotalCPUUsage,
			TotalCores:                   metrics.TotalCores,
			SystemLoad:                   metrics.SystemLoad,
			CPULimitsActive:              summary.cpuLimitsActive,
			ResourceLimitsActive:         summary.resourceLimitsActive,
			AnyLimitsActive:              summary.cpuLimitsActive || summary.resourceLimitsActive,
			CPUActivelyLimitedUsersCount: len(summary.cpuUsers),
			ActivelyLimitedUsersCount:    len(summary.activelyLimitedUsers),
		},
	); err != nil {
		m.logger.Warn("Failed to write metrics to database",
			"users", len(metrics.UserMetrics),
			"cpu_limits_active", summary.cpuLimitsActive,
			"resource_limits_active", summary.resourceLimitsActive,
			"error", err,
		)
		if m.prometheusExporter != nil {
			m.prometheusExporter.RecordError(metricsDatabaseErrorComponent, metricsDatabaseWriteFailure)
		}
		return
	}

	m.logger.Debug("Metrics written to database",
		"users", len(metrics.UserMetrics),
		"cpu_limits_active", summary.cpuLimitsActive,
		"resource_limits_active", summary.resourceLimitsActive,
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
