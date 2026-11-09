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
	"github.com/fdefilippo/resman/internal/blockiops"
	"github.com/fdefilippo/resman/internal/controlhistory"
	"github.com/fdefilippo/resman/internal/pressure"
	"github.com/fdefilippo/resman/internal/systemdunit"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

const (
	ControlCycleTriggerInitial  = "initial"
	ControlCycleTriggerTicker   = "ticker"
	ControlCycleTriggerManual   = "manual"
	ControlCycleTriggerIOWeight = "io_weight_activation"
)

type controlCycleContext struct {
	ctx       context.Context
	cfg       *config.Config
	trigger   string
	startTime time.Time
	cycleID   int64
	// systemdOwnedVerified records that this cycle verified the owned systemd
	// leases, directly or through a CPU Points plan confirmed within one
	// POLLING_INTERVAL, so a later stage need not reread them.
	systemdOwnedVerified bool
	metrics              *SystemMetrics
	decision             string
	reason               string
	enforcementState     cgroup.EnforcementCycleState
	duration             time.Duration
	activeLimitedUsers   int
	ingressRefusedCount  int
	stopWithoutError     bool
	deferredErrors       []error
	degradedErrors       []error
	degradedWarnings     []error
	failedStage          resmanmetrics.ControlCycleStage
}

type controlCycleStage struct {
	name               resmanmetrics.ControlCycleStage
	run                func(*Manager, *controlCycleContext) error
	continueAfterError bool
}

var defaultControlCyclePipeline = []controlCycleStage{
	{name: resmanmetrics.StageReconcileCPUPoints, run: (*Manager).stageReconcileCPUPoints, continueAfterError: true},
	{name: resmanmetrics.StageCheckBlackout, run: (*Manager).stageCheckBlackout},
	{name: resmanmetrics.StageCollectMetrics, run: (*Manager).stageCollectMetrics},
	{name: resmanmetrics.StageReconcileIOWeights, run: (*Manager).stageReconcileIODeviceWeights, continueAfterError: true},
	{name: resmanmetrics.StageMakeDecision, run: (*Manager).stageMakeDecision},
	{name: resmanmetrics.StageExecuteDecision, run: (*Manager).stageExecuteDecision, continueAfterError: true},
	{name: resmanmetrics.StageFinalizeEnforcementObservation, run: (*Manager).stageFinalizeEnforcementObservation},
	{name: resmanmetrics.StageDispatchLimitHooks, run: (*Manager).stageDispatchLimitHooks, continueAfterError: true},
	{name: resmanmetrics.StageUpdatePrometheus, run: (*Manager).stageUpdatePrometheus},
	{name: resmanmetrics.StageWriteDatabase, run: (*Manager).stageWriteDatabase},
	{name: resmanmetrics.StageWorkloadPatternDetection, run: (*Manager).stageWorkloadPatternDetection, continueAfterError: true},
	{name: resmanmetrics.StageLogCompletion, run: (*Manager).stageLogCompletion},
}

func (m *Manager) stageReconcileCPUPoints(run *controlCycleContext) error {
	if m.enforcementStatus.Mode == cgroup.EnforcementModeSystemdNative {
		m.mu.RLock()
		requested := m.systemdCPURequested
		policy := m.cpuPointsPolicy
		reusable := m.systemdCPUPlanReusableLocked(run)
		m.mu.RUnlock()
		if !requested {
			return nil
		}
		if reusable {
			run.systemdOwnedVerified = true
			return nil
		}
		ctx := context.Background()
		if run != nil && run.ctx != nil {
			ctx = run.ctx
		}
		if err := m.reconcileSystemdCPUPoints(ctx, policy); err != nil {
			return fmt.Errorf("systemd-native CPU Points topology remains degraded: %w", err)
		}
		if run != nil {
			run.systemdOwnedVerified = true
		}
		return nil
	}
	return nil
}

// systemdCPUPlanReusableLocked reports whether this cycle may rely on the CPU
// Points plan that a complete verification confirmed less than one
// POLLING_INTERVAL ago. A complete verification rereads every owned lease, the
// user-slice topology and every slice twice; with PSI-triggered cycles about
// once per second it dominated the cycle on a 45-user host (resman-2wm).
// External drift and new user slices are therefore found within one
// POLLING_INTERVAL. Any incomplete, degraded or pending state, and every
// configuration change, forces a complete verification. The caller holds m.mu.
func (m *Manager) systemdCPUPlanReusableLocked(run *controlCycleContext) bool {
	if run == nil || run.cfg == nil || !m.systemdCPUComplete || m.cpuPointsDegraded ||
		m.pendingCPUPointsPolicy != nil || m.systemdCPUVerifiedAt.IsZero() {
		return false
	}
	interval := time.Duration(run.cfg.GetPollingInterval()) * time.Second
	elapsed := run.startTime.Sub(m.systemdCPUVerifiedAt)
	return elapsed >= 0 && elapsed < interval
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
	// A per-run sequence, not the start second: cycles triggered by PSI and the
	// ticker within one second used to share an identifier (resman-qi6).
	run.cycleID = m.cycleSequence.Add(1)

	if exporter := m.prometheusExporter; exporter != nil {
		exporter.RecordControlCycleTrigger(trigger)
		defer func() {
			exporter.RecordControlCycleDuration(time.Since(run.startTime))
		}()
	}

	if err := m.logger.DebugChecked("Starting control cycle", "cycle_id", run.cycleID, "trigger", trigger); err != nil {
		run.degradedErrors = append(run.degradedErrors, fmt.Errorf("write control-cycle start log: %w", err))
	}

	err := runControlCyclePipeline(m, run, defaultControlCyclePipeline)
	systemdFailed := systemdCycleFailed(run)
	checkedAt := time.Now()
	m.mu.Lock()
	m.readinessCycle = CycleReadiness{CheckedAt: checkedAt, Suspended: run.stopWithoutError, CoreAvailable: run.metrics != nil && run.decision != "", Mode: m.enforcementStatus.Mode, SystemdDegraded: m.cpuPointsDegraded || systemdFailed}
	if run.metrics != nil && run.decision != "" {
		m.readinessCycle.DecisionAt = run.metrics.Timestamp
	}
	m.mu.Unlock()
	historyStarted := time.Now()
	historyErr := m.finishControlCycle(run, err)
	if m.prometheusExporter != nil {
		outcome := resmanmetrics.StageSucceeded
		if historyErr != nil {
			outcome = resmanmetrics.StageFailed
		}
		m.prometheusExporter.ObserveControlStage(resmanmetrics.ControlStageObservation{Stage: resmanmetrics.StageRecordHistory, Outcome: outcome, Duration: time.Since(historyStarted)})
	}
	return errors.Join(err, historyErr)
}

func runControlCyclePipeline(m *Manager, run *controlCycleContext, stages []controlCycleStage) error {
	var cycleErrors []error
	for _, stage := range stages {
		if run.ctx != nil && run.ctx.Err() != nil {
			return errors.Join(errors.Join(cycleErrors...), errors.Join(run.degradedErrors...), run.ctx.Err())
		}
		stageStart := time.Now()
		err := stage.run(m, run)
		outcome := resmanmetrics.StageSucceeded
		if onlyCancellation(err) {
			outcome = resmanmetrics.StageCancelled
		} else if err != nil {
			outcome = resmanmetrics.StageFailed
		} else if run.stopWithoutError {
			outcome = resmanmetrics.StageSuspended
		}
		if m.prometheusExporter != nil {
			m.prometheusExporter.ObserveControlStage(resmanmetrics.ControlStageObservation{Stage: stage.name, Outcome: outcome, Duration: time.Since(stageStart)})
		}
		if err != nil {
			if run.failedStage == "" {
				run.failedStage = stage.name
			}
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
	if m.prometheusExporter != nil {
		until := time.Time{}
		if nextEnd != nil {
			until = *nextEnd
		}
		m.prometheusExporter.ObserveDecisionSuspension(until)
	}
	if nextEnd != nil {
		m.mu.RLock()
		limitsNeedDeactivation := m.limitsActive || m.resourceLimitsActive || len(m.activeUsers) > 0 || len(m.resourceLimits) > 0 || m.ioWeightStatus.Programmed
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
	for _, failure := range metrics.systemdObservationFailures {
		run.degradedWarnings = append(run.degradedWarnings, failure)
	}
	m.reportPersistenceObservationFailures(metrics)
	return nil
}

func (m *Manager) stageUpdatePrometheus(run *controlCycleContext) error {
	// Publish system observations and decision-owned per-user metrics.
	run.metrics.CPUDecision = m.cpuDecisionMetrics(run)
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
	m.finalizeSystemdCPUPointsDegradation(run.metrics)
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
	run.duration = time.Since(run.startTime)
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
		"cpu_eligible_users_host_cpu_percent", eligibleHostCPUPercentLogValue(run.metrics),
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

// eligibleHostCPUPercentLogValue reports the value compared with the CPU
// thresholds, so an operator can read it next to cpu_eligible_users_cpu_usage,
// which stays an aggregate per-core percentage. An unknown core count is logged
// as unavailable instead of a fabricated zero.
func eligibleHostCPUPercentLogValue(metrics *SystemMetrics) interface{} {
	percent, ok := eligibleHostCPUPercent(metrics)
	if !ok {
		return "unavailable"
	}
	return percent
}

type SystemMetrics struct {
	Pressure                      *pressure.Snapshot
	CPUDecision                   *resmanmetrics.CPUDecisionMetrics
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
	systemdAuthorityInventory     *systemdunit.ProcessAuthorityInventory
	systemdAuthorityInventoryErr  *systemdAuthorityInventoryError
	systemdObservationFailures    []persistenceObservationFailure
	systemdObservationContext     string

	// All non-system users with UID at or above SYSTEM_UID_MIN.
	AllUsersCPUUsage    float64
	AllUsersMemoryUsage uint64
	AllUsersCount       int

	// Per-resource eligible-user metrics.
	CPUEligibleCPUUsage            float64 // Aggregate per-core percentage: 100 is one busy CPU.
	CPUEligibleMemoryUsage         uint64
	CPUEligibleUsersCount          int
	RAMEligibleUsersCount          int
	IOEligibleUsersCount           int
	RAMEligibleUsageBytes          uint64
	IOEligibleReadBPS              float64
	IOEligibleWriteBPS             float64
	IOEligibleReadBlockIOPS        float64
	IOEligibleWriteBlockIOPS       float64
	IOBlockIOPSSourceAvailable     bool
	IOBlockIOPSDecisionAt          time.Time
	IOBlockIOPSDevices             map[blockiops.Device]blockIOPSDeviceBounds
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

type systemdAuthorityInventoryFailure string

const (
	systemdAuthorityInventoryDiscoveryFailed  systemdAuthorityInventoryFailure = "topology_discovery_failed"
	systemdAuthorityInventoryInspectionFailed systemdAuthorityInventoryFailure = "process_inspection_failed"
)

type systemdAuthorityInventoryError struct {
	Failure systemdAuthorityInventoryFailure
	Err     error
}

func (e *systemdAuthorityInventoryError) Error() string {
	return fmt.Sprintf("capture systemd process-authority inventory (%s): %v", e.Failure, e.Err)
}

func (e *systemdAuthorityInventoryError) Unwrap() error { return e.Err }

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

	// Pressure is observation only, owned by this decision sample. A host
	// refresh carries nil and therefore cannot overwrite or renew it.
	if decisionSample {
		snapshot := pressure.ReadSystem("/proc/pressure", sampleTime)
		metrics.Pressure = &snapshot
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
		if diagnostics, ok := m.metricsCollector.(resmanmetrics.DecisionProcessDiagnostics); ok {
			if err := diagnostics.DecisionProcessScanError(); err != nil {
				return nil, fmt.Errorf("authoritative process decision sample unavailable: %w", err)
			}
		}
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
		m.collectPersistenceInterval(metrics)
		if err := m.collectEligibleBlockIOPS(metrics, decisionConfig.GetIODecisionPolicy()); err != nil {
			metrics.IOBlockIOPSDevices = nil
			metrics.IOBlockIOPSSourceAvailable = false
			metrics.IOBlockIOPSUnavailableUsers = len(metrics.IOEligibleUsers)
			m.rememberBlockIOPSDecision(metrics)
			m.updatePrometheusSystemMetrics(metrics)
			return nil, err
		}
		m.rememberBlockIOPSDecision(metrics)
		m.prevIOTime = sampleTime
		m.previousIOEligibleUsers = make(map[int]struct{}, len(metrics.IOEligibleUsers))
		for _, uid := range metrics.IOEligibleUsers {
			m.previousIOEligibleUsers[uid] = struct{}{}
		}
	} else {
		m.projectBlockIOPSDecision(metrics)
	}

	return metrics, nil
}

// calculateIOByteRates converts per-process counter growth into per-second rates.
// Byte counters originate from per-process /proc/PID/io deltas. Block IOPS are
// collected separately at the authoritative systemd unit's kernel boundary.
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
	ioWeightStatus := m.GetIODeviceWeightStatus()
	ioWeight := &resmanmetrics.IODeviceWeightExporterMetrics{
		State: string(ioWeightStatus.State), Reason: ioWeightStatus.Reason,
		Mechanism:  string(ioWeightStatus.Mechanism),
		Programmed: ioWeightStatus.Programmed, ReadBack: ioWeightStatus.ReadBack,
		ProgrammedState: string(ioWeightStatus.ProgrammedState), ReadBackState: string(ioWeightStatus.ReadBackState),
		FunctionallyAccepted: ioWeightStatus.State == IODeviceWeightFunctionallyAccepted,
		EffectQualified:      ioWeightStatus.EffectQualified, PartialUsers: ioWeightStatus.PartialUsers,
		EffectQualificationProvenance: string(ioWeightStatus.EffectQualificationProvenance),
		AuthorityCoverage:             string(ioWeightStatus.AuthorityCoverage), CompleteUsers: ioWeightStatus.CompleteUsers,
		UnavailableUsers: ioWeightStatus.UnavailableUsers, SiblingSlices: ioWeightStatus.SiblingSlices,
		TotalPoints: ioWeightStatus.TotalPoints, RequestedAt: ioWeightStatus.RequestedAt, NextRetryAt: ioWeightStatus.NextRetryAt,
		ObservedDelivery: ioWeightStatus.ObservedDelivery,
	}
	for _, value := range ioWeightStatus.Values {
		ioWeight.Values = append(ioWeight.Values, resmanmetrics.IODeviceWeightValueMetrics{
			UID: value.UID, Class: value.Class, Device: value.Device, Mechanism: value.Mechanism, Coverage: string(value.Coverage),
			RequestedValue: value.RequestedValue, SystemdValue: value.SystemdValue, KernelValue: value.KernelValue, NominalShare: value.NominalShare,
			Programmed: value.Programmed, ReadBack: value.ReadBack,
		})
	}

	blockDevices := make([]resmanmetrics.BlockIOPSDeviceMetrics, 0, len(metrics.IOBlockIOPSDevices))
	exactBlockIOPS := metrics.IOBlockIOPSDevices != nil && metrics.IOBlockIOPSUnavailableUsers == 0
	for device, bounds := range metrics.IOBlockIOPSDevices {
		exactBlockIOPS = exactBlockIOPS && bounds.Read.ActivationMinimum == 0 && bounds.Read.ReleaseMaximum == 0 && bounds.Write.ActivationMinimum == 0 && bounds.Write.ReleaseMaximum == 0
		blockDevices = append(blockDevices, resmanmetrics.BlockIOPSDeviceMetrics{Device: fmt.Sprintf("%d:%d", device.Major, device.Minor), ReadMinimum: bounds.Read.ActivationMinimum, ReadMaximum: bounds.Read.ReleaseMaximum, WriteMinimum: bounds.Write.ActivationMinimum, WriteMaximum: bounds.Write.ReleaseMaximum, Uncertain: bounds.Uncertain, UpperAvailable: metrics.IOBlockIOPSUnavailableUsers == 0})
	}
	m.prometheusExporter.UpdateSystemSnapshot(resmanmetrics.SystemExporterMetrics{
		Pressure:                metrics.Pressure,
		CPUDecision:             metrics.CPUDecision,
		BlockIOPSExactAvailable: exactBlockIOPS, BlockIOPSSourceRequired: needsBlockIOPS(m.GetConfig()), BlockIOPSDecisionAt: metrics.IOBlockIOPSDecisionAt, BlockIOPSDevices: blockDevices, BlockIOPSUnavailableUsers: metrics.IOBlockIOPSUnavailableUsers, BlockIOPSSourceAvailable: metrics.IOBlockIOPSSourceAvailable,
		EnforcementMode:                              m.enforcementStatus.Mode,
		EnforcementCycleState:                        m.currentEnforcementCycleState(),
		TotalCPUUsage:                                metrics.TotalCPUUsage,
		TotalCPUUsageAvailable:                       metrics.HostCPUUsageAvailable,
		TotalCores:                                   metrics.TotalCores,
		CPUPoints:                                    cpuPoints,
		IODeviceWeight:                               ioWeight,
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

		cpuPoints := metrics.CPUPointsUsers[uid]
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
	metricsCollectionErrorComponent  = "metrics_collection"
	metricsCollectionSystemLoadError = "system_load_failure"
	metricsDatabaseErrorComponent    = "metrics_database"
	metricsDatabaseWriteFailure      = "write_failure"
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
	ioWeight := m.GetIODeviceWeightStatus()
	persistenceSystem.IODeviceWeightState = string(ioWeight.State)
	persistenceSystem.IODeviceWeightReason = ioWeight.Reason
	persistenceSystem.IODeviceWeightSelector = ioWeight.Selector
	persistenceSystem.IODeviceWeightMechanism = string(ioWeight.Mechanism)
	persistenceSystem.IODeviceWeightClassificationAttempts = ioWeight.ClassificationAttempts
	persistenceSystem.IODeviceWeightProbeAttempts = ioWeight.ProbeAttempts
	persistenceSystem.IODeviceWeightProgrammed = ioWeight.Programmed
	persistenceSystem.IODeviceWeightProgrammedState = string(ioWeight.ProgrammedState)
	persistenceSystem.IODeviceWeightReadBack = ioWeight.ReadBack
	persistenceSystem.IODeviceWeightReadBackState = string(ioWeight.ReadBackState)
	persistenceSystem.IODeviceWeightFunctionallyAccepted = ioWeight.State == IODeviceWeightFunctionallyAccepted
	persistenceSystem.IODeviceWeightEffectQualified = ioWeight.EffectQualified
	persistenceSystem.IODeviceWeightEffectQualificationProvenance = string(ioWeight.EffectQualificationProvenance)
	persistenceSystem.IODeviceWeightAuthorityCoverage = string(ioWeight.AuthorityCoverage)
	persistenceSystem.IODeviceWeightCompleteUsers = ioWeight.CompleteUsers
	persistenceSystem.IODeviceWeightPartialUsers = ioWeight.PartialUsers
	persistenceSystem.IODeviceWeightUnavailableUsers = ioWeight.UnavailableUsers
	persistenceSystem.IODeviceWeightSiblingSlices = ioWeight.SiblingSlices
	persistenceSystem.IODeviceWeightTotalPoints = ioWeight.TotalPoints
	persistenceSystem.IODeviceWeightRequestedAt = optionalIODeviceWeightTime(ioWeight.RequestedAt)
	persistenceSystem.IODeviceWeightNextRetryAt = optionalIODeviceWeightTime(ioWeight.NextRetryAt)
	persistenceSystem.IODeviceWeightValuesJSON = ioDeviceWeightValuesJSON(ioWeight.Values)
	persistenceSystem.IODeviceWeightObservedDelivery = ioWeight.ObservedDelivery
	persistenceSystem.CPULimitsActive = summary.cpuLimitsActive
	persistenceSystem.ResourceLimitsActive = summary.resourceLimitsActive
	persistenceSystem.AnyLimitsActive = summary.cpuLimitsActive || summary.resourceLimitsActive
	persistenceSystem.CPUActivelyLimitedUsersCount = len(summary.cpuUsers)
	persistenceSystem.ActivelyLimitedUsersCount = len(summary.activelyLimitedUsers)

	if err := m.metricsCollector.WriteMetricsToDatabase(resmanmetrics.PersistenceBatch{
		System: persistenceSystem,
		Users:  metrics.PersistenceUsers,
	}); err != nil {
		var partial *resmanmetrics.PartialPersistenceError
		if errors.As(err, &partial) && partial.RawMetricsCommitted() {
			m.clearPersistedCPUPointsLifecycleEvents()
		}
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

// ControlCycleEntry is the typed terminal history contract shared with storage.
type ControlCycleEntry = controlhistory.Entry

// controlHistory stores recent control cycle entries
type controlHistory struct {
	entries []ControlCycleEntry
	mu      sync.RWMutex
	maxSize int
	pending []ControlCycleEntry
	dropped uint64
}

// addControlHistoryEntry publishes the recent record and pending delivery together.
func (m *Manager) addControlHistoryEntry(entry ControlCycleEntry, persist bool) ([]ControlCycleEntry, bool) {
	m.controlHist.mu.Lock()
	defer m.controlHist.mu.Unlock()
	m.controlHist.entries = append(m.controlHist.entries, entry)
	if len(m.controlHist.entries) > m.controlHist.maxSize {
		m.controlHist.entries = m.controlHist.entries[len(m.controlHist.entries)-m.controlHist.maxSize:]
	}
	if !persist {
		return nil, false
	}
	m.controlHist.pending = append(m.controlHist.pending, entry)
	dropped := len(m.controlHist.pending) > m.controlHist.maxSize
	if dropped {
		m.controlHist.pending = m.controlHist.pending[1:]
		m.controlHist.dropped++
	}
	return append([]ControlCycleEntry(nil), m.controlHist.pending...), dropped
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

// cpuDecisionMetrics projects the plan's actual request and verified outcome,
// rather than inferring intent from raw CPU usage or a combined resource action.
func (m *Manager) cpuDecisionMetrics(run *controlCycleContext) *resmanmetrics.CPUDecisionMetrics {
	hostPercent, available := eligibleHostCPUPercent(run.metrics)
	m.mu.RLock()
	requested, applied := m.systemdCPURequested, m.systemdCPUComplete
	m.mu.RUnlock()
	if m.enforcementStatus.Mode != cgroup.EnforcementModeSystemdNative {
		requested = run.decision == decisionActivate && len(run.metrics.CPUEligibleUsers) > 0
		applied = false
	}
	return &resmanmetrics.CPUDecisionMetrics{At: run.metrics.Timestamp, HostPercent: hostPercent, HostPercentAvailable: available, ActivationRequested: requested, ActivationApplied: applied}
}

// CycleReadiness is the bounded in-memory outcome of the latest control attempt.
type CycleReadiness struct {
	CheckedAt       time.Time
	DecisionAt      time.Time
	Suspended       bool
	CoreAvailable   bool
	Mode            cgroup.EnforcementMode
	SystemdDegraded bool
}

// ReadinessSnapshot returns cycle health without collecting or reconciling anything.
func (m *Manager) ReadinessSnapshot() CycleReadiness {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.readinessCycle
}

// systemdCycleFailed classifies errors without conflating hook, logger or sink failures.
func systemdCycleFailed(run *controlCycleContext) bool {
	for _, err := range run.deferredErrors {
		var resource *SystemdResourceReconciliationError
		var adapter *systemdunit.AdapterError
		if errors.As(err, &resource) || errors.As(err, &adapter) {
			return true
		}
	}
	return false
}

// AvailableAt refuses stale attempts even during blackout, so a stalled control
// loop cannot preserve a successful readiness result indefinitely.
func (r CycleReadiness) AvailableAt(now time.Time, maxAge time.Duration) bool {
	fresh := func(at time.Time) bool { age := now.Sub(at); return !at.IsZero() && age >= 0 && age <= maxAge }
	return maxAge > 0 && fresh(r.CheckedAt) && (r.Suspended || (r.CoreAvailable && fresh(r.DecisionAt)))
}
