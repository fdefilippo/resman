package state

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/systemdunit"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

const (
	ControlCycleTriggerInitial = "initial"
	ControlCycleTriggerTicker  = "ticker"
	ControlCycleTriggerManual  = "manual"
)

type controlCycleContext struct {
	ctx                 context.Context
	cfg                 *config.Config
	trigger             string
	startTime           time.Time
	cycleID             int64
	metrics             *SystemMetrics
	decision            string
	reason              string
	enforcementState    cgroup.EnforcementCycleState
	duration            time.Duration
	activeLimitedUsers  int
	ingressRefusedCount int
	stopWithoutError    bool
	deferredErrors      []error
	degradedErrors      []error
	degradedWarnings    []error
}

type controlCycleStage struct {
	name               string
	run                func(*Manager, *controlCycleContext) error
	continueAfterError bool
}

var defaultControlCyclePipeline = []controlCycleStage{
	{name: "reconcile_cpu_points", run: (*Manager).stageReconcileCPUPoints, continueAfterError: true},
	{name: "check_blackout", run: (*Manager).stageCheckBlackout},
	{name: "collect_metrics", run: (*Manager).stageCollectMetrics},
	{name: "make_decision", run: (*Manager).stageMakeDecision},
	{name: "execute_decision", run: (*Manager).stageExecuteDecision, continueAfterError: true},
	{name: "finalize_enforcement_observation", run: (*Manager).stageFinalizeEnforcementObservation},
	{name: "update_prometheus", run: (*Manager).stageUpdatePrometheus},
	{name: "write_database", run: (*Manager).stageWriteDatabase},
	{name: "record_history", run: (*Manager).stageRecordHistory},
	{name: "workload_pattern_detection", run: (*Manager).stageWorkloadPatternDetection, continueAfterError: true},
	{name: "log_completion", run: (*Manager).stageLogCompletion},
}

func (m *Manager) stageReconcileCPUPoints(run *controlCycleContext) error {
	if m.enforcementStatus.Mode == cgroup.EnforcementModeSystemdNative {
		m.mu.RLock()
		requested := m.systemdCPURequested
		policy := m.cpuPointsPolicy
		m.mu.RUnlock()
		if !requested {
			return nil
		}
		ctx := context.Background()
		if run != nil && run.ctx != nil {
			ctx = run.ctx
		}
		if err := m.reconcileSystemdCPUPoints(ctx, policy); err != nil {
			return fmt.Errorf("systemd-native CPU Points topology remains degraded: %w", err)
		}
		return nil
	}
	return nil
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
		m.prometheusExporter.ObserveObservationHostCPUUsage(resmanmetrics.HostCPUUsageSample{
			UsagePercent:      metrics.TotalCPUUsage,
			Available:         metrics.HostCPUUsageAvailable,
			UnavailableReason: metrics.HostCPUUsageUnavailableReason,
		})
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
		if run.ctx != nil && run.ctx.Err() != nil {
			return errors.Join(errors.Join(cycleErrors...), errors.Join(run.degradedErrors...), run.ctx.Err())
		}
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
		m.mu.RLock()
		limitsNeedDeactivation := m.limitsActive || m.resourceLimitsActive || len(m.activeUsers) > 0 || len(m.resourceLimits) > 0
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
	return nil
}

func (m *Manager) stageUpdatePrometheus(run *controlCycleContext) error {
	// Publish system observations and decision-owned per-user metrics.
	if m.prometheusExporter != nil {
		m.prometheusExporter.ObserveControlCycleHostCPUUsage(resmanmetrics.HostCPUUsageSample{
			UsagePercent:      run.metrics.TotalCPUUsage,
			Available:         run.metrics.HostCPUUsageAvailable,
			UnavailableReason: run.metrics.HostCPUUsageUnavailableReason,
		})
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
	intent, err := enforcementPolicyIntent(run.decision)
	if err != nil {
		return err
	}
	run.enforcementState = cgroup.EnforcementCycleState{
		Mode:            m.enforcementStatus.Mode,
		RequestedIntent: intent,
		AppliedAction:   cgroup.AppliedEnforcementActionNone,
		BlockReason:     cgroup.EnforcementBlockReasonNone,
	}
	if m.enforcementStatus.Mode == cgroup.EnforcementModeObservationOnly {
		run.ingressRefusedCount = m.recordObservationOnlyIntent(run.decision, run.metrics)
		if intent == cgroup.EnforcementPolicyIntentActivate || intent == cgroup.EnforcementPolicyIntentDeactivate {
			run.enforcementState.BlockReason = cgroup.BoundedEnforcementBlockReason(m.enforcementStatus.Reason)
		}
		return m.publishEnforcementCycleState(run.enforcementState)
	}
	if executionErr := m.executeDecision(run.decision, run.metrics); executionErr != nil {
		publishErr := m.publishEnforcementCycleState(run.enforcementState)
		return errors.Join(
			fmt.Errorf("failed to execute decision %s (cycle %d): %w", run.decision, run.cycleID, executionErr),
			publishErr,
		)
	}
	run.enforcementState.AppliedAction = appliedEnforcementAction(intent)
	return m.publishEnforcementCycleState(run.enforcementState)
}

func (m *Manager) stageFinalizeEnforcementObservation(run *controlCycleContext) error {
	m.finalizeSystemdResourceCoverage(run.metrics)
	return nil
}

func (m *Manager) stageRecordHistory(run *controlCycleContext) error {
	// 6. Record the control-cycle history.
	run.duration = time.Since(run.startTime)
	m.recordControlCycle(run.decision, run.reason, run.metrics, run.duration)
	return nil
}

func (m *Manager) stageWorkloadPatternDetection(run *controlCycleContext) error {
	// Workload classification remains observational. Pattern-selected resource
	// mutation ended with the retired PID-relocation backend.
	if m.patternDetector == nil {
		return nil
	}

	if !run.cfg.GetAutodetectPatterns() {
		m.patternDetector.RetainUsers(map[int]bool{})
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

	// Analyze patterns once per hour.
	if time.Since(m.lastPatternAnalysis) <= time.Hour {
		return nil
	}

	m.lastPatternAnalysis = time.Now()
	m.patternDetector.Cleanup(time.Duration(run.cfg.GetPatternHistoryHours()) * time.Hour)
	patterns := m.patternDetector.Analyze(run.cfg)
	for uid, result := range patterns {
		if m.prometheusExporter != nil {
			username := m.metricsCollector.GetUsernameFromUID(uid)
			m.prometheusExporter.UpdateUserWorkloadPattern(uid, username, string(result.Pattern), result.Confidence)
		}
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
		"requested_policy_intent", run.enforcementState.RequestedIntent,
		"applied_enforcement_action", run.enforcementState.AppliedAction,
		"enforcement_block_reason", run.enforcementState.BlockReason,
		"decision_reason", run.reason,
		"total_cpu_usage", run.metrics.TotalCPUUsage,
		"cpu_eligible_users_cpu_usage", run.metrics.CPUEligibleCPUUsage,
		"eligible_users", run.metrics.CPUEligibleUsersCount,
		"active_limited_users", run.activeLimitedUsers,
		"enforcement_mode", m.enforcementStatus.Mode,
		"ingress_refused_count", run.ingressRefusedCount,
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
	Timestamp                     time.Time
	TotalCores                    int
	TotalCPUUsage                 float64 // Percentage
	HostCPUUsageAvailable         bool
	HostCPUUsageUnavailableReason resmanmetrics.HostCPUUsageUnavailableReason
	PersistenceSystem             resmanmetrics.SystemPersistenceMetrics
	PersistenceUsers              map[int]resmanmetrics.UserPersistenceMetrics
	CPUPointsSystem               resmanmetrics.CPUPointsSystemSnapshot
	CPUPointsUsers                map[int]resmanmetrics.CPUPointsUserSnapshot
	systemdRAMAuthority           map[int]*systemdunit.ResourceAuthority
	systemdIOAuthority            map[int]*systemdunit.ResourceAuthority

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
	var hostCPU resmanmetrics.HostCPUUsageSample
	if decisionSample {
		hostCPU = m.metricsCollector.GetDecisionHostCPUUsage()
	} else {
		hostCPU = m.metricsCollector.GetObservationHostCPUUsage()
	}
	metrics.TotalCPUUsage = hostCPU.UsagePercent
	metrics.HostCPUUsageAvailable = hostCPU.Available
	metrics.HostCPUUsageUnavailableReason = hostCPU.UnavailableReason

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
		m.collectEligibleBlockIOPS(metrics, decisionConfig.GetIODecisionPolicy())
		m.prevIOTime = sampleTime
		m.previousIOEligibleUsers = make(map[int]struct{}, len(metrics.IOEligibleUsers))
		for _, uid := range metrics.IOEligibleUsers {
			m.previousIOEligibleUsers[uid] = struct{}{}
		}
		m.collectPersistenceInterval(metrics)
	}

	return metrics, nil
}

func (m *Manager) collectEligibleBlockIOPS(metrics *SystemMetrics, policy config.IODecisionPolicy) {
	needsBlockIO := policy.Enabled && (policy.ReadIOPS > 0 || policy.WriteIOPS > 0)
	if needsBlockIO {
		metrics.IOBlockIOPSUnavailableUsers += len(metrics.IOEligibleUsers)
	}
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
	var cpuPoints *resmanmetrics.CPUPointsSystemSnapshot
	if metrics.CPUPointsUsers != nil {
		cpuPoints = &metrics.CPUPointsSystem
	}

	m.prometheusExporter.UpdateSystemSnapshot(resmanmetrics.SystemExporterMetrics{
		EnforcementMode:                              m.enforcementStatus.Mode,
		EnforcementCycleState:                        m.currentEnforcementCycleState(),
		TotalCPUUsage:                                metrics.TotalCPUUsage,
		TotalCPUUsageAvailable:                       metrics.HostCPUUsageAvailable,
		TotalCores:                                   metrics.TotalCores,
		CPUPoints:                                    cpuPoints,
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
	exporterUsers := make(map[int]*resmanmetrics.UserMetrics, len(metrics.UserMetrics))
	for uid, user := range metrics.UserMetrics {
		exporterUsers[uid] = user
	}
	for uid, user := range metrics.PersistenceUsers {
		if exporterUsers[uid] == nil {
			exporterUsers[uid] = user.Metrics
		}
	}
	for uid, userMetrics := range exporterUsers {
		username := userMetrics.Username
		if username == "" || username == strconv.Itoa(uid) {
			username = m.getUsername(uid)
		}

		var cgroupPath, cpuQuota string
		var memoryHighEvents uint64
		cpuPoints := metrics.CPUPointsUsers[uid]
		if persisted, ok := metrics.PersistenceUsers[uid]; ok {
			if persisted.CgroupPath != "" {
				cgroupPath = persisted.CgroupPath
			}
			if persisted.CPUQuota != "" {
				cpuQuota = persisted.CPUQuota
			}
		}
		ioReadBytes := userMetrics.IOReadBytes
		ioWriteBytes := userMetrics.IOWriteBytes

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
			CgroupMemoryCurrent:  cpuPoints.RAMCgroupUsageBytes,
			MemoryHighEvents:     memoryHighEvents,
			ObservedIOReadBytes:  ioReadBytes,
			ObservedIOWriteBytes: ioWriteBytes,
			ObservedIOReadOps:    userMetrics.IOReadOps,
			ObservedIOWriteOps:   userMetrics.IOWriteOps,
			CPUPoints:            cpuPoints,
		})
	}

	// Remove metric series for users absent from the current sample.
	activeUids := make(map[int]bool)
	for uid := range exporterUsers {
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
	ioRemediationErrorComponent        = "io_remediation"
	patternPolicyErrorComponent        = "pattern_policy"
	patternPolicyApplicationFailure    = "application_failure"
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

	persistenceSystem := metrics.PersistenceSystem
	persistenceSystem.CPULimitsActive = summary.cpuLimitsActive
	persistenceSystem.ResourceLimitsActive = summary.resourceLimitsActive
	persistenceSystem.AnyLimitsActive = summary.cpuLimitsActive || summary.resourceLimitsActive
	persistenceSystem.CPUActivelyLimitedUsersCount = len(summary.cpuUsers)
	persistenceSystem.ActivelyLimitedUsersCount = len(summary.activelyLimitedUsers)

	if err := m.metricsCollector.WriteMetricsToDatabase(resmanmetrics.PersistenceBatch{
		System: persistenceSystem,
		Users:  metrics.PersistenceUsers,
	}); err != nil {
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
	m.clearPersistedCPUPointsLifecycleEvents()

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
