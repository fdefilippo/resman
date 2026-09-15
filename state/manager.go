/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// state/manager.go
package state

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/configepoch"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/ioweights"
	"github.com/fdefilippo/resman/internal/limithook"
	"github.com/fdefilippo/resman/internal/operationgate"
	"github.com/fdefilippo/resman/internal/systemdunit"
	"github.com/fdefilippo/resman/logging"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type stateLogger interface {
	Debug(string, ...interface{})
	Info(string, ...interface{})
	Warn(string, ...interface{})
	Error(string, ...interface{})
	DebugChecked(string, ...interface{}) error
	InfoChecked(string, ...interface{}) error
}

// Manager coordinates resource decisions and observed cgroup enforcement.
type Manager struct {
	cfg    *config.Config
	logger stateLogger
	mu     sync.RWMutex
	opGate operationgate.Gate
	epoch  configepoch.Barrier
	hookMu sync.Mutex
	hookWG sync.WaitGroup

	// Internal control and observed enforcement state.
	limitsActive               bool
	limitsAppliedTime          time.Time
	resourceLimitsActive       bool
	resourceLimitsAppliedTime  time.Time
	requestedCPUUsers          map[int]bool
	activeUsers                map[int]bool // UID -> user with an authoritative applied CPU plan
	userLimitedAt              map[int]time.Time
	resourceLimits             map[int]userResourceLimitState
	cpuPointsPolicy            cpupoints.PolicySnapshot
	cpuCapacity                CPUCapacityProvider
	persistencePreviousCPU     map[string]cgroup.CPUPointsNodeSnapshot
	persistencePreviousRAM     map[int]cgroup.MemoryAccountingSnapshot
	persistencePreviousTime    time.Time
	persistenceFailureState    string
	persistenceFailureActive   bool
	cpuPointsLifecycleEvents   map[int]cpuPointsLifecycleEvent
	cpuPointsSystemSnapshot    resmanmetrics.CPUPointsSystemSnapshot
	cpuPointsUserSnapshots     map[int]resmanmetrics.CPUPointsUserSnapshot
	pendingCPUPointsPolicy     *cpupoints.PolicySnapshot
	cpuPointsDegraded          bool
	enforcementStatus          cgroup.EnforcementStatus
	enforcementCycleState      cgroup.EnforcementCycleState
	systemdUnits               SystemdCPUUnitAdapter
	systemdCPURequested        bool
	systemdCPUComplete         bool
	systemdCPUParent           systemdunit.UnitIdentity
	systemdCPUSlices           map[int]systemdunit.UnitIdentity
	systemdCPUPlanSignature    string
	systemdCPUPlan             cpupoints.FlatPlan
	persistencePreviousSystemd map[int]systemdunit.UnitIdentity
	systemdResourcesRequested  bool
	systemdResourceUnits       map[int]systemdunit.UnitIdentity
	resolveSystemdIODevices    func(string) ([]string, error)
	systemdIOWeights           SystemdIODeviceWeightAdapter
	ioWeightClassifier         IODeviceWeightClassifier
	ioWeightPolicy             ioweights.PolicySnapshot
	ioWeightCapability         systemdunit.IODeviceWeightCapabilitySnapshot
	ioWeightStatus             IODeviceWeightStatus
	ioWeightUnits              map[int]systemdunit.UnitIdentity
	ioWeightProbeCancel        context.CancelFunc
	ioWeightProbeToken         uint64
	ioWeightUnavailableCycles  int

	// Threshold monitoring
	thresholdTracker    *ThresholdTracker
	ioThresholdTracker  *ThresholdTracker
	stabilityTracker    *UserStabilityTracker
	lastPatternAnalysis time.Time

	// Injected dependencies.
	metricsCollector         MetricsCollector
	cgroupManager            CgroupManager
	prometheusExporter       PrometheusExporter
	patternDetector          *PatternDetector
	hookCtx                  context.Context
	hookCancel               context.CancelFunc
	hookClosed               bool
	hookQueue                chan limitHookJob
	hookWorkerCount          int
	hookWorkersStarted       bool
	hookInFlight             atomic.Int64
	hookScriptIdentity       limithook.ScriptIdentity
	hookLastSaturationLog    time.Time
	hookSaturationSuppressed uint64
	hookNow                  func() time.Time
	executeHookScript        func(context.Context, hookScriptInvocation, limitHookEvent) error
	executeHookRequest       func(context.Context, string, limitHookEvent) error

	// Cached metrics state.
	metricsCache     map[string]interface{}
	metricsCacheTime map[string]time.Time

	// Control cycle history, initialized by NewManager.
	controlHist *controlHistory

	// I/O rate tracking: per-process deltas from consecutive decision samples
	// are converted to rates only for users eligible in both samples.
	previousIOEligibleUsers map[int]struct{}
	prevIOTime              time.Time
}

type userResourceLimitState struct {
	ram          bool
	ramApplied   bool
	swap         bool
	io           bool
	ioApplied    bool
	ramAuthority systemdunit.ResourceAuthority
	ioAuthority  systemdunit.ResourceAuthority
}

type cpuPointsLifecycleEvent struct {
	state resmanmetrics.CPUPointsLifecycleState
}

// CPUCapacityProvider supplies a fresh authoritative CPU denominator for each reconciliation.
type CPUCapacityProvider interface {
	Refresh(cpupoints.ParentPoolPoints) (cpupoints.CapacityState, error)
	State() cpupoints.CapacityState
}

// ManagerOption configures one state-manager dependency.
type ManagerOption func(*Manager) error

// WithCPUPointsRuntime installs the immutable policy and live-capacity provider.
func WithCPUPointsRuntime(policy cpupoints.PolicySnapshot, capacity CPUCapacityProvider) ManagerOption {
	return func(m *Manager) error {
		if capacity == nil {
			return fmt.Errorf("CPU Points live-capacity provider is required")
		}
		m.cpuPointsPolicy = policy
		m.cpuCapacity = capacity
		return nil
	}
}

// WithEnforcementStatus installs the immutable host ownership decision.
func WithEnforcementStatus(status cgroup.EnforcementStatus) ManagerOption {
	return func(m *Manager) error {
		switch status.Mode {
		case cgroup.EnforcementModeObservationOnly, cgroup.EnforcementModeSystemdNative:
			m.enforcementStatus = status
			return nil
		default:
			return fmt.Errorf("unsupported enforcement mode %q", status.Mode)
		}
	}
}

// UserLimitState separates policy eligibility, control intent, and observed enforcement.
type UserLimitState struct {
	EligibleForCPU    bool
	EligibleForRAM    bool
	EligibleForIO     bool
	CPULimitRequested bool
	CPULimitActive    bool
	RAMLimitRequested bool
	RAMLimitActive    bool
	IOLimitRequested  bool
	IOLimitActive     bool
}

// MetricsCollector defines the system metrics boundary used by the state manager.
type MetricsCollector interface {
	GetTotalCores() int
	GetDecisionHostCPUUsage() resmanmetrics.HostCPUUsageSample
	GetObservationHostCPUUsage() resmanmetrics.HostCPUUsageSample
	GetUserCPUUsage(uid int) float64

	// All non-system users.
	GetAllUsers() []int
	GetAllUsersCPUUsage() float64
	GetAllUsersMemoryUsage() uint64

	GetMemoryUsage() float64
	GetTotalMemoryMB() float64
	GetCachedMemoryMB() float64
	IsSystemUnderLoad() bool
	GetSystemLoad() (float64, error)
	// GetAllUserMetrics returns an observation-only sample.
	GetAllUserMetrics() map[int]*resmanmetrics.UserMetrics
	// GetAllUserMetricsForDecision advances only the control cadence state.
	GetAllUserMetricsForDecision() map[int]*resmanmetrics.UserMetrics
	GetDBWriter() *resmanmetrics.DBWriter
	WriteMetricsToDatabase(batch resmanmetrics.PersistenceBatch) error
	GetUsernameFromUID(uid int) string
}

// CgroupManager exposes the read-only cgroup observer's lifecycle hook. Runtime
// enforcement is performed exclusively through the authoritative systemd adapter.
type CgroupManager interface {
	CleanupAll() error
}

// PrometheusExporter defines the Prometheus boundary used by the state manager.
type PrometheusExporter interface {
	UpdateSystemSnapshot(metrics resmanmetrics.SystemExporterMetrics)
	UpdateUserSnapshot(metrics resmanmetrics.UserExporterMetrics)
	UpdateUserWorkloadPattern(uid int, username string, pattern string, confidence float64)
	RecordControlCycleTrigger(trigger string)
	ObserveControlCycleHostCPUUsage(sample resmanmetrics.HostCPUUsageSample)
	ObserveObservationHostCPUUsage(sample resmanmetrics.HostCPUUsageSample)
	RecordControlCycleDuration(duration time.Duration)
	RecordMetricsCollectionDuration(duration time.Duration)
	RecordError(component, errorType string)
	RecordLimitHookExecution(hookType resmanmetrics.LimitHookType, outcome resmanmetrics.LimitHookOutcome)
	ObserveLimitHookExecutor(inFlight, queued, capacity int)
	Start(ctx context.Context) error
	Stop() error
	CleanupUserMetrics(activeUids map[int]bool)
	IncrementCPULimitsActivated()
	IncrementCPULimitsDeactivated()
}

// NewManager creates a resource manager with the supplied dependencies.
func NewManager(
	cfg *config.Config,
	metrics MetricsCollector,
	cgroups CgroupManager,
	prometheus PrometheusExporter,
	options ...ManagerOption,
) (*Manager, error) {

	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil: required for state manager initialization")
	}
	if cfg.LimitHookMaxConcurrency < 1 {
		return nil, fmt.Errorf("initialize limit-hook executor: LIMIT_HOOK_MAX_CONCURRENCY must be at least 1")
	}
	if cfg.LimitHookQueueCapacity < 1 {
		return nil, fmt.Errorf("initialize limit-hook executor: LIMIT_HOOK_QUEUE_CAPACITY must be at least 1")
	}

	logger := logging.GetLogger()
	hookCtx, hookCancel := context.WithCancel(context.Background())

	mgr := &Manager{
		cfg:                       cfg,
		logger:                    logger,
		limitsActive:              false,
		limitsAppliedTime:         time.Time{},
		resourceLimitsActive:      false,
		resourceLimitsAppliedTime: time.Time{},
		requestedCPUUsers:         make(map[int]bool),
		activeUsers:               make(map[int]bool),
		userLimitedAt:             make(map[int]time.Time),
		resourceLimits:            make(map[int]userResourceLimitState),
		persistencePreviousCPU:    make(map[string]cgroup.CPUPointsNodeSnapshot),
		persistencePreviousRAM:    make(map[int]cgroup.MemoryAccountingSnapshot),
		cpuPointsLifecycleEvents:  make(map[int]cpuPointsLifecycleEvent),
		cpuPointsUserSnapshots:    make(map[int]resmanmetrics.CPUPointsUserSnapshot),
		systemdCPUSlices:          make(map[int]systemdunit.UnitIdentity),
		systemdResourceUnits:      make(map[int]systemdunit.UnitIdentity),
		ioWeightUnits:             make(map[int]systemdunit.UnitIdentity),
		resolveSystemdIODevices:   systemdunit.ResolveBlockDevices,
		enforcementStatus: cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeObservationOnly,
			Reason: cgroup.EnforcementReasonSystemdRuntimeAbsent,
		},
		thresholdTracker:   &ThresholdTracker{},
		stabilityTracker:   newUserStabilityTracker(),
		ioThresholdTracker: &ThresholdTracker{},
		metricsCollector:   metrics,
		cgroupManager:      cgroups,
		prometheusExporter: prometheus,
		patternDetector:    NewPatternDetector(logger),
		hookCtx:            hookCtx,
		hookCancel:         hookCancel,
		hookQueue:          make(chan limitHookJob, cfg.LimitHookQueueCapacity),
		hookWorkerCount:    cfg.LimitHookMaxConcurrency,
		hookNow:            time.Now,
		executeHookScript:  runLimitHookScript,
		executeHookRequest: newLimitHookHTTPRequestExecutor(),
		metricsCache:       make(map[string]interface{}),
		metricsCacheTime:   make(map[string]time.Time),
		controlHist: &controlHistory{
			entries: make([]ControlCycleEntry, 0),
			maxSize: 100,
		},
		previousIOEligibleUsers: make(map[int]struct{}),
	}
	if cfg.LimitHookScript != "" {
		identity, identityErr := limithook.ResolveScriptIdentity(cfg.LimitHookScriptUser, cfg.LimitHookScriptGroup)
		if identityErr != nil {
			return nil, fmt.Errorf("initialize limit-hook script identity: %w", identityErr)
		}
		if pathErr := limithook.ValidateScriptPath(cfg.LimitHookScript, identity); pathErr != nil {
			return nil, fmt.Errorf("initialize limit-hook script path: %w", pathErr)
		}
		mgr.hookScriptIdentity = identity
	}
	if prometheus != nil {
		prometheus.ObserveLimitHookExecutor(0, 0, cfg.LimitHookQueueCapacity)
	}
	reserve, err := cpupoints.NewReservePoints(uint64(cfg.GetCPUReservePoints()))
	if err != nil {
		return nil, fmt.Errorf("initialize CPU Points reserve: %w", err)
	}
	root, err := cpupoints.NewRootPoints(uint64(cfg.GetCPURootPoints()))
	if err != nil {
		return nil, fmt.Errorf("initialize CPU Points root entitlement: %w", err)
	}
	bestEffort, err := cpupoints.NewBestEffortPoints(uint64(cfg.GetCPUBestEffortPoints()))
	if err != nil {
		return nil, fmt.Errorf("initialize CPU Points best-effort entitlement: %w", err)
	}
	mgr.cpuPointsPolicy, err = cpupoints.NewEmptyPolicySnapshot(reserve, root, bestEffort)
	if err != nil {
		return nil, fmt.Errorf("initialize empty CPU Points policy: %w", err)
	}
	oneCPU, _ := cpupoints.NewOnlineCPUCount(1)
	mgr.cpuCapacity, err = cpupoints.NewLiveCapacityProvider(cpupoints.OnlineCPUSourceFunc(func() (cpupoints.OnlineCPUCount, error) {
		return oneCPU, nil
	}), mgr.cpuPointsPolicy.Pool())
	if err != nil {
		return nil, fmt.Errorf("initialize CPU Points test capacity: %w", err)
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(mgr); err != nil {
			return nil, err
		}
	}
	if mgr.enforcementStatus.Mode == cgroup.EnforcementModeSystemdNative && mgr.systemdUnits == nil {
		return nil, fmt.Errorf("systemd-native enforcement requires the authoritative systemd adapter")
	}
	if mgr.systemdUnits != nil && mgr.enforcementStatus.Mode != cgroup.EnforcementModeSystemdNative {
		return nil, fmt.Errorf("systemd CPU adapter requires systemd-native enforcement mode")
	}
	mgr.enforcementCycleState = cgroup.InitialEnforcementCycleState(mgr.enforcementStatus)

	logger.Info("State manager initialized",
		"polling_interval", cfg.PollingInterval,
		"cpu_threshold", cfg.CPUThreshold,
		"cpu_release_threshold", cfg.CPUReleaseThreshold,
		"cpu_threshold_duration", cfg.CPUThresholdDuration,
		"ignore_system_load", cfg.IgnoreSystemLoad,
	)
	return mgr, nil
}

// getUsername resolves a UID through the shared metrics collector.
func (m *Manager) getUsername(uid int) string {
	if m.metricsCollector != nil {
		return m.metricsCollector.GetUsernameFromUID(uid)
	}
	return strconv.Itoa(uid)
}

// GetUIDFromUsername resolves a username from the current metrics snapshot.
// It returns zero when the user is not present.
func (m *Manager) GetUIDFromUsername(username string) int {
	if username == "" {
		return 0
	}

	// Resolve only users present in the current metrics snapshot.
	allMetrics := m.metricsCollector.GetAllUserMetrics()
	for uid, metrics := range allMetrics {
		if metrics.Username == username {
			return uid
		}
	}

	return 0
}

// GetUserLimitState returns the authoritative policy, intent, and enforcement snapshot for a user.
func (m *Manager) GetUserLimitState(uid int, username string) UserLimitState {
	eligibility := m.GetConfig().EvaluateUserEligibility(username)
	m.mu.RLock()
	requestedCPU := m.requestedCPUUsers[uid]
	activeCPU := m.activeUsers[uid]
	resources := m.resourceLimits[uid]
	m.mu.RUnlock()
	return UserLimitState{
		EligibleForCPU:    eligibility.EligibleForCPU,
		EligibleForRAM:    eligibility.EligibleForRAM,
		EligibleForIO:     eligibility.EligibleForIO,
		CPULimitRequested: requestedCPU,
		CPULimitActive:    activeCPU,
		RAMLimitRequested: resources.ram,
		RAMLimitActive:    resources.ramApplied,
		IOLimitRequested:  resources.io,
		IOLimitActive:     resources.ioApplied,
	}
}

// isUserLimited reports whether CPU enforcement is observed for a user.
func (m *Manager) isUserLimited(uid int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.activeUsers[uid]
	return exists
}

// RuntimeStatus is an observed snapshot of current enforcement state.
type RuntimeStatus struct {
	EnforcementMode              cgroup.EnforcementMode
	EnforcementReason            string
	RequestedPolicyIntent        cgroup.EnforcementPolicyIntent
	AppliedEnforcementAction     cgroup.AppliedEnforcementAction
	EnforcementBlockReason       cgroup.EnforcementBlockReason
	CPULimitsActive              bool
	ResourceLimitsActive         bool
	AnyLimitsActive              bool
	CPULimitsAppliedTime         time.Time
	ResourceLimitsAppliedTime    time.Time
	ActivelyLimitedUsers         []int
	ActivelyLimitedUsersCount    int
	CPUActivelyLimitedUsers      []int
	CPUActivelyLimitedUsersCount int
	CPUPoints                    resmanmetrics.CPUPointsSystemSnapshot
	CPUPointUsers                []resmanmetrics.CPUPointsUserSnapshot
}

type enforcementSummary struct {
	cpuUsers                  []int
	activelyLimitedUsers      []int
	cpuLimitsActive           bool
	resourceLimitsActive      bool
	cpuLimitsAppliedTime      time.Time
	resourceLimitsAppliedTime time.Time
}

func (m *Manager) getEnforcementSummary() enforcementSummary {
	m.mu.RLock()
	cpuUsers := make([]int, 0, len(m.activeUsers))
	activelyLimited := make(map[int]struct{}, len(m.activeUsers)+len(m.resourceLimits))
	for uid := range m.activeUsers {
		cpuUsers = append(cpuUsers, uid)
		activelyLimited[uid] = struct{}{}
	}
	resourceEnforcementObserved := false
	for uid, resourceState := range m.resourceLimits {
		if resourceState.ramApplied || resourceState.ioApplied {
			resourceEnforcementObserved = true
			activelyLimited[uid] = struct{}{}
		}
	}
	summary := enforcementSummary{
		cpuUsers:                  cpuUsers,
		cpuLimitsActive:           len(cpuUsers) > 0,
		resourceLimitsActive:      resourceEnforcementObserved,
		cpuLimitsAppliedTime:      m.limitsAppliedTime,
		resourceLimitsAppliedTime: m.resourceLimitsAppliedTime,
	}
	m.mu.RUnlock()

	summary.activelyLimitedUsers = make([]int, 0, len(activelyLimited))
	for uid := range activelyLimited {
		summary.activelyLimitedUsers = append(summary.activelyLimitedUsers, uid)
	}
	sort.Ints(summary.cpuUsers)
	sort.Ints(summary.activelyLimitedUsers)
	return summary
}

// GetStatus returns a typed snapshot of observed enforcement state.
func (m *Manager) GetStatus() RuntimeStatus {
	summary := m.getEnforcementSummary()
	cycleState := m.currentEnforcementCycleState()

	status := RuntimeStatus{
		EnforcementMode:              m.enforcementStatus.Mode,
		EnforcementReason:            m.enforcementStatus.Reason,
		RequestedPolicyIntent:        cycleState.RequestedIntent,
		AppliedEnforcementAction:     cycleState.AppliedAction,
		EnforcementBlockReason:       cycleState.BlockReason,
		CPULimitsActive:              summary.cpuLimitsActive,
		ResourceLimitsActive:         summary.resourceLimitsActive,
		AnyLimitsActive:              summary.cpuLimitsActive || summary.resourceLimitsActive,
		CPULimitsAppliedTime:         summary.cpuLimitsAppliedTime,
		ResourceLimitsAppliedTime:    summary.resourceLimitsAppliedTime,
		ActivelyLimitedUsers:         summary.activelyLimitedUsers,
		ActivelyLimitedUsersCount:    len(summary.activelyLimitedUsers),
		CPUActivelyLimitedUsers:      summary.cpuUsers,
		CPUActivelyLimitedUsersCount: len(summary.cpuUsers),
	}
	m.mu.RLock()
	status.CPUPoints = m.cpuPointsSystemSnapshot
	status.CPUPointUsers = make([]resmanmetrics.CPUPointsUserSnapshot, 0, len(m.cpuPointsUserSnapshots))
	for _, snapshot := range m.cpuPointsUserSnapshots {
		status.CPUPointUsers = append(status.CPUPointUsers, snapshot)
	}
	policy := m.cpuPointsPolicy
	degraded := m.cpuPointsDegraded
	m.mu.RUnlock()
	status.CPUPoints.ReservePoints = policy.Reserve().Value()
	status.CPUPoints.NominalParentPoolPoints = policy.Pool().Value()
	status.CPUPoints.ConfiguredBestEffortPoints = policy.BestEffort().Value()
	status.CPUPoints.EnforcementMode = string(status.EnforcementMode)
	rootPoints := policy.Root().Value()
	if m.systemdUnits != nil {
		status.CPUPoints.ConfiguredRootPoints = &rootPoints
	}
	status.CPUPoints.ReconciliationDegraded = status.CPUPoints.ReconciliationDegraded || degraded
	if status.CPUPoints.IntervalEnd.IsZero() && m.cpuCapacity != nil {
		capacity := m.cpuCapacity.State()
		status.CPUPoints.CapacityAvailable = capacity.Available
		status.CPUPoints.CapacityUnavailableReason = string(capacity.UnavailableReason)
		if capacity.LastVerified.PeriodMicroseconds() != 0 {
			quota := capacity.LastVerified.QuotaMicroseconds()
			period := capacity.LastVerified.PeriodMicroseconds()
			online := capacity.LastVerified.OnlineCPUs().Value()
			if m.systemdUnits == nil {
				status.CPUPoints.ProgrammedParentQuotaUsec = &quota
				status.CPUPoints.ProgrammedParentPeriodUsec = &period
			}
			if capacity.Available {
				status.CPUPoints.OnlineCPUs = &online
			}
		}
		status.CPUPoints.DeliveryState = resmanmetrics.CPUPointsDeliveryUnavailable
		status.CPUPoints.DenominatorState = resmanmetrics.CPUPointsDenominatorUnavailable
	}
	sort.Slice(status.CPUPointUsers, func(i, j int) bool { return status.CPUPointUsers[i].UID < status.CPUPointUsers[j].UID })

	return status
}

func (m *Manager) currentEnforcementCycleState() cgroup.EnforcementCycleState {
	m.mu.RLock()
	state := m.enforcementCycleState
	m.mu.RUnlock()
	return cgroup.NormalizedEnforcementCycleState(state, m.enforcementStatus)
}

// GetCPUPointsUserStatus returns the latest authoritative decision-sample
// status for one UID. The boolean is false before that UID has been observed.
func (m *Manager) GetCPUPointsUserStatus(uid int) (resmanmetrics.CPUPointsUserSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot, ok := m.cpuPointsUserSnapshots[uid]
	return snapshot, ok
}

// Cleanup releases active enforcement and shuts down manager dependencies.
func (m *Manager) Cleanup() error {
	m.stopLimitHooks()

	m.logger.Info("Cleaning up state manager")
	var cleanupErrors []error
	func() {
		leaveOperation := m.opGate.Enter()
		defer leaveOperation()

		// Remove all active limits.
		m.mu.RLock()
		limitsActive := m.limitsActive || m.resourceLimitsActive || m.systemdCPURequested
		m.mu.RUnlock()
		if m.systemdUnits != nil && len(m.systemdUnits.OwnedUnits()) > 0 {
			limitsActive = true
		}
		if limitsActive {
			if err := m.deactivateLimits(); err != nil {
				m.logger.Error("Error during cleanup deactivation", "error", err)
				cleanupErrors = append(cleanupErrors, fmt.Errorf("deactivate limits: %w", err))
			}
		}

		// Finish the read-only cgroup observer lifecycle.
		if m.cgroupManager != nil {
			if err := m.cgroupManager.CleanupAll(); err != nil {
				m.logger.Error("Error during cgroup cleanup", "error", err)
				cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup cgroups: %w", err))
			}
		}
		if m.systemdUnits != nil {
			m.systemdUnits.Close()
		}
	}()

	// Prometheus shutdown can wait for network I/O and must not hold the
	// operation lock needed by control-cycle and reconciliation work.
	if m.prometheusExporter != nil {
		if err := m.prometheusExporter.Stop(); err != nil {
			m.logger.Error("Error stopping Prometheus exporter", "error", err)
			cleanupErrors = append(cleanupErrors, fmt.Errorf("stop Prometheus exporter: %w", err))
		}
	}

	cleanupErr := errors.Join(cleanupErrors...)
	if cleanupErr != nil {
		m.logger.Warn("State manager cleanup incomplete",
			"error_count", len(cleanupErrors),
			"error", cleanupErr,
		)
		return cleanupErr
	}
	m.logger.Info("State manager cleanup completed")
	return nil
}

// UpdateConfig replaces the manager configuration used by subsequent cycles.
func (m *Manager) UpdateConfig(newConfig *config.Config) {
	if newConfig == nil {
		return
	}
	leaveOperation := m.opGate.Enter()
	defer leaveOperation()
	oldConfig := m.GetConfig()
	processPolicyChanged := oldConfig == nil || !slices.Equal(
		oldConfig.GetProcessExcludeList(),
		newConfig.GetProcessExcludeList(),
	)
	m.mu.Lock()
	m.cfg = newConfig
	if oldConfig == nil || oldConfig.GetIOWeightDevices() != newConfig.GetIOWeightDevices() {
		m.ioWeightCapability = systemdunit.IODeviceWeightCapabilitySnapshot{}
		m.ioWeightStatus.State = IODeviceWeightRequestedPending
		m.ioWeightStatus.Reason = "configuration_changed"
		m.ioWeightStatus.Selector = newConfig.GetIOWeightDevices()
		m.ioWeightStatus.Programmed = len(m.ioWeightUnits) > 0
		m.ioWeightStatus.ReadBack = false
		m.ioWeightUnavailableCycles = 0
	}
	if processPolicyChanged {
		m.previousIOEligibleUsers = make(map[int]struct{})
		m.prevIOTime = time.Time{}
	}
	m.mu.Unlock()

	m.logger.Info("State manager configuration updated",
		"polling_interval", newConfig.PollingInterval,
		"cpu_threshold", newConfig.CPUThreshold,
		"cpu_release_threshold", newConfig.CPUReleaseThreshold,
		"cpu_threshold_duration", newConfig.CPUThresholdDuration,
	)
}

// BeginConfigUpdate starts an exclusive configuration epoch update. Component
// callbacks may perform I/O because the epoch barrier does not remain locked.
func (m *Manager) BeginConfigUpdate() func() {
	m.mu.Lock()
	cancel := m.ioWeightProbeCancel
	m.ioWeightProbeCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return m.epoch.BeginUpdate()
}

// OnUserPSIEvent records pressure for an actively enforced user. CPU Points
// remains the sole owner of cpu.weight; PSI never mutates allocation policy.
func (m *Manager) OnUserPSIEvent(event cgroup.PSIEvent) {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()

	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

	if event.UID <= 0 {
		return
	}
	m.mu.RLock()
	active := m.activeUsers[event.UID]
	m.mu.RUnlock()
	if !active {
		m.logger.Debug("Ignoring PSI event for user without active limits",
			"uid", event.UID, "type", event.Type)
		return
	}

	m.logger.Info("PSI pressure observed for actively enforced user",
		"uid", event.UID, "type", event.Type,
		"psi_avg10", event.SomeAvg10)
}

// GetConfig returns the current configuration
func (m *Manager) GetConfig() *config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// ControlCycleEntry represents a single control cycle entry in history
// GetControlHistory returns the recent control cycle history
// recordControlCycle records a control cycle in history
// Reset clears threshold tracking state.
// ShouldActivateLimits checks if limits should be activated based on threshold duration.
// GetElapsed returns the time since the first threshold crossing.
