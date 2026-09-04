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
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/configepoch"
	"github.com/fdefilippo/resman/internal/cpupoints"
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
	limitsActive              bool
	limitsAppliedTime         time.Time
	resourceLimitsActive      bool
	resourceLimitsAppliedTime time.Time
	requestedCPUUsers         map[int]bool
	activeUsers               map[int]bool // UID -> user observed in the CPU-limited cgroup
	userLimitedAt             map[int]time.Time
	resourceLimits            map[int]userResourceLimitState
	sharedCgroupPath          string // Shared CPU cgroup path
	cpuPointsHierarchy        cgroup.CPUPointsHierarchy
	cpuPointsPolicy           cpupoints.PolicySnapshot
	cpuCapacity               CPUCapacityProvider
	cpuAllocations            map[int]cpuPointsAllocation
	appliedGuaranteePoints    cpupoints.AppliedGuaranteePoints
	programmedGuaranteePoints uint64
	ramCoverage               map[int]ramCoverageState
	persistencePreviousCPU    map[string]cgroup.CPUPointsNodeSnapshot
	persistencePreviousRAM    map[int]cgroup.MemoryAccountingSnapshot
	persistencePreviousTime   time.Time
	cpuPointsLifecycleEvents  map[int]cpuPointsLifecycleEvent
	cpuPointsSystemSnapshot   resmanmetrics.CPUPointsSystemSnapshot
	cpuPointsUserSnapshots    map[int]resmanmetrics.CPUPointsUserSnapshot
	pendingCPUPointsPolicy    *cpupoints.PolicySnapshot
	cpuPointsDegraded         bool
	enforcementStatus         cgroup.EnforcementStatus
	systemdUnits              SystemdCPUUnitAdapter
	systemdCPURequested       bool
	systemdCPUComplete        bool
	systemdCPUParent          systemdunit.UnitIdentity
	systemdCPUSlices          map[int]systemdunit.UnitIdentity
	systemdCPUPlanSignature   string
	recoverySnapshot          cgroup.RecoverySnapshot

	// Threshold monitoring
	thresholdTracker    *ThresholdTracker
	ioThresholdTracker  *ThresholdTracker
	stabilityTracker    *UserStabilityTracker
	lastPatternAnalysis time.Time

	// Injected dependencies.
	metricsCollector              MetricsCollector
	cgroupManager                 CgroupManager
	prometheusExporter            PrometheusExporter
	ioRemediation                 *IORemediation
	patternDetector               *PatternDetector
	policyEngine                  *PolicyEngine
	pendingPatternReconciliations map[int]struct{}
	hookCtx                       context.Context
	hookCancel                    context.CancelFunc
	hookClosed                    bool
	hookQueue                     chan limitHookJob
	hookWorkerCount               int
	hookWorkersStarted            bool
	hookInFlight                  atomic.Int64
	hookScriptIdentity            limithook.ScriptIdentity
	hookLastSaturationLog         time.Time
	hookSaturationSuppressed      uint64
	hookNow                       func() time.Time
	executeHookScript             func(context.Context, hookScriptInvocation, limitHookEvent) error
	executeHookRequest            func(context.Context, string, limitHookEvent) error

	// Cached metrics state.
	metricsCache     map[string]interface{}
	metricsCacheTime map[string]time.Time

	// Control cycle history, initialized by NewManager.
	controlHist *controlHistory

	// I/O rate tracking: per-process deltas from consecutive decision samples
	// are converted to rates only for users eligible in both samples.
	previousIOEligibleUsers map[int]struct{}
	prevIOTime              time.Time
	previousBlockIOCounters map[int]blockIOCounterSample
	blockIOObservedUsers    map[int]bool

	// PSI watcher triggers observation and control cycles; CPU Points exclusively owns weights.
	psiWatcher *cgroup.PSIWatcher
}

type userResourceLimitState struct {
	ram        bool
	ramApplied bool
	swap       bool
	io         bool
	ioApplied  bool
	standalone bool
}

type cpuPointsAllocation struct {
	class                   cpupoints.AllocationClass
	weight                  cpupoints.KernelCPUWeight
	domainPath              string
	leafPath                string
	pidNamespaceMismatches  int
	pidNamespaceUnavailable int
}

type cpuPointsLifecycleEvent struct {
	state                   resmanmetrics.CPUPointsLifecycleState
	pidNamespaceMismatches  int
	pidNamespaceUnavailable int
	systemdOwnershipRefused int
	recoveryProcesses       int
	restoreFailedProcesses  int
}

// RAMCoverage describes whether a managed leaf accounts for the complete
// memory footprint of one UID. Dynamic ingress can only provide post-ingress
// coverage because cgroup v2 does not transfer existing page charges.
type RAMCoverage string

const (
	RAMCoverageComplete RAMCoverage = "complete"
	RAMCoveragePartial  RAMCoverage = "partial"
)

type ramCoverageState struct {
	coverage RAMCoverage
	partial  map[int]uint64 // PID -> start time
}

// RAMActiveCPUTransitionError reports a CPU placement transition refused to
// preserve the authoritative cgroup for already-applied RAM enforcement.
type RAMActiveCPUTransitionError struct {
	UID  int
	From string
	To   string
}

func (e *RAMActiveCPUTransitionError) Error() string {
	return fmt.Sprintf("refusing CPU cgroup transition for UID %d from %s to %s while RAM enforcement is active", e.UID, e.From, e.To)
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
		case cgroup.EnforcementModeMigrationEnabled, cgroup.EnforcementModeObservationOnlySystemd, cgroup.EnforcementModeSystemdNative:
			m.enforcementStatus = status
			return nil
		default:
			return fmt.Errorf("unsupported enforcement mode %q", status.Mode)
		}
	}
}

// WithRecoverySnapshot installs the startup view of processes already stranded
// in recovery so public status is truthful before the first control cycle.
func WithRecoverySnapshot(snapshot cgroup.RecoverySnapshot) ManagerOption {
	return func(m *Manager) error {
		m.recoverySnapshot = snapshot
		return nil
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

// CgroupManager defines the cgroup v2 operations used by the state manager.
type CgroupManager interface {
	CreateUserCgroup(uid int) error
	EnsureUnlimitedCPUQuota(uid int) error
	ApplyRAMLimit(uid int, limit string) error
	ApplyRAMLimitWithSwapDisabled(uid int, limit string) error
	ApplyRAMHigh(uid int, limit string) error
	ApplyRAMLimitWithHigh(uid int, maxLimit string, highLimit string) error
	ApplyRAMLimitWithHighAndSwapDisabled(uid int, maxLimit string, highLimit string) error
	RemoveRAMLimit(uid int) error
	RemoveRAMHigh(uid int) error
	RemoveRAMSwapLimit(uid int) error
	GetCgroupRAMUsage(uid int) (uint64, error)
	GetMemoryHighEvents(uid int) (uint64, error)
	ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error
	RemoveIOLimit(uid int) error
	GetIOStats(uid int) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error)
	EnsureUserCgroupPlacement(uid int, sharedPath, normalQuota string) (string, cgroup.ProcessMoveResult, error)
	GetUserCgroupMetrics(uid int) (cgroupPath, cpuQuota string, memoryHighEvents uint64, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64, err error)
	GetCPUPointsNodeSnapshot(path string) (cgroup.CPUPointsNodeSnapshot, error)
	GetMemoryAccountingSnapshot(uid int) (cgroup.MemoryAccountingSnapshot, error)
	GetPSIStats(uid int) (cgroup.PSIStats, error)
	ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error
	CleanupUserCgroup(uid int) error
	MoveProcessToCgroup(pid int, uid int) (cgroup.ProcessMoveResult, error)
	MoveAllUserProcesses(uid int) (cgroup.ProcessMoveResult, error)
	MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) (cgroup.ProcessMoveResult, error)
	ReconcileUserProcessMembership(uid int, sharedPath, normalQuota string) (cgroup.ProcessMembershipResult, error)
	ReleaseUserFromSharedCgroup(uid int, sharedPath, normalQuota string) error
	EnsureCPUPointsHierarchy(cpupoints.ParentQuota, cpupoints.KernelCPUWeight) (cgroup.CPUPointsHierarchy, error)
	ApplyCPUPointsParentQuota(cgroup.CPUPointsHierarchy, cpupoints.ParentQuota) error
	ApplyCPUPointsGuaranteedWeight(cgroup.CPUPointsHierarchy, cpupoints.KernelCPUWeight) error
	ApplyCPUPointsBestEffortWeight(cgroup.CPUPointsHierarchy, cpupoints.KernelCPUWeight) error
	ApplyCPUPointsUserWeight(string, cpupoints.KernelCPUWeight) error
	EnsureCPUPointsUserPlacement(int, string, cpupoints.KernelCPUWeight) (string, cgroup.ProcessMoveResult, error)
	ReleaseCPUPointsUser(int, string) error
	RemoveCPUPointsHierarchy(cgroup.CPUPointsHierarchy) error
	CleanupAll() error
	GetCgroupInfo(uid int) (cgroup.CgroupInfo, error)
	GetCreatedCgroups() []int
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
	RecordCgroupIngressSkips(result cgroup.ProcessMoveResult)
	RecordProcessRestoreResult(result cgroup.ProcessRestoreResult)
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
		sharedCgroupPath:          "",
		cpuAllocations:            make(map[int]cpuPointsAllocation),
		ramCoverage:               make(map[int]ramCoverageState),
		persistencePreviousCPU:    make(map[string]cgroup.CPUPointsNodeSnapshot),
		persistencePreviousRAM:    make(map[int]cgroup.MemoryAccountingSnapshot),
		cpuPointsLifecycleEvents:  make(map[int]cpuPointsLifecycleEvent),
		cpuPointsUserSnapshots:    make(map[int]resmanmetrics.CPUPointsUserSnapshot),
		systemdCPUSlices:          make(map[int]systemdunit.UnitIdentity),
		enforcementStatus: cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeMigrationEnabled,
			Reason: cgroup.EnforcementReasonNoSystemdRuntime,
		},
		thresholdTracker:              &ThresholdTracker{},
		stabilityTracker:              newUserStabilityTracker(),
		ioThresholdTracker:            &ThresholdTracker{},
		metricsCollector:              metrics,
		cgroupManager:                 cgroups,
		prometheusExporter:            prometheus,
		ioRemediation:                 NewIORemediation(logger),
		patternDetector:               NewPatternDetector(logger),
		policyEngine:                  NewPolicyEngine(logger),
		pendingPatternReconciliations: make(map[int]struct{}),
		hookCtx:                       hookCtx,
		hookCancel:                    hookCancel,
		hookQueue:                     make(chan limitHookJob, cfg.LimitHookQueueCapacity),
		hookWorkerCount:               cfg.LimitHookMaxConcurrency,
		hookNow:                       time.Now,
		executeHookScript:             runLimitHookScript,
		executeHookRequest:            newLimitHookHTTPRequestExecutor(),
		metricsCache:                  make(map[string]interface{}),
		metricsCacheTime:              make(map[string]time.Time),
		controlHist: &controlHistory{
			entries: make([]ControlCycleEntry, 0),
			maxSize: 100,
		},
		previousIOEligibleUsers: make(map[int]struct{}),
		previousBlockIOCounters: make(map[int]blockIOCounterSample),
		blockIOObservedUsers:    make(map[int]bool),
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
	EnforcementMode               cgroup.EnforcementMode
	EnforcementReason             string
	MigrationEnforcementAvailable bool
	RecoveryOccupants             []cgroup.RecoveryOccupant
	CPULimitsActive               bool
	ResourceLimitsActive          bool
	AnyLimitsActive               bool
	CPULimitsAppliedTime          time.Time
	ResourceLimitsAppliedTime     time.Time
	ActivelyLimitedUsers          []int
	ActivelyLimitedUsersCount     int
	CPUActivelyLimitedUsers       []int
	CPUActivelyLimitedUsersCount  int
	SharedCgroupPath              string
	SharedCgroupActive            bool
	SharedCgroupQuota             string
	SharedCgroupUserCount         int
	CPUPoints                     resmanmetrics.CPUPointsSystemSnapshot
	CPUPointUsers                 []resmanmetrics.CPUPointsUserSnapshot
}

type enforcementSummary struct {
	cpuUsers                  []int
	activelyLimitedUsers      []int
	cpuLimitsActive           bool
	resourceLimitsActive      bool
	cpuLimitsAppliedTime      time.Time
	resourceLimitsAppliedTime time.Time
	sharedCgroupPath          string
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
		sharedCgroupPath:          m.sharedCgroupPath,
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

	status := RuntimeStatus{
		EnforcementMode:               m.enforcementStatus.Mode,
		EnforcementReason:             m.enforcementStatus.Reason,
		MigrationEnforcementAvailable: m.enforcementStatus.Mode == cgroup.EnforcementModeMigrationEnabled,
		CPULimitsActive:               summary.cpuLimitsActive,
		ResourceLimitsActive:          summary.resourceLimitsActive,
		AnyLimitsActive:               summary.cpuLimitsActive || summary.resourceLimitsActive,
		CPULimitsAppliedTime:          summary.cpuLimitsAppliedTime,
		ResourceLimitsAppliedTime:     summary.resourceLimitsAppliedTime,
		ActivelyLimitedUsers:          summary.activelyLimitedUsers,
		ActivelyLimitedUsersCount:     len(summary.activelyLimitedUsers),
		CPUActivelyLimitedUsers:       summary.cpuUsers,
		CPUActivelyLimitedUsersCount:  len(summary.cpuUsers),
		SharedCgroupPath:              summary.sharedCgroupPath,
		SharedCgroupActive:            summary.sharedCgroupPath != "" && summary.cpuLimitsActive,
	}
	m.mu.RLock()
	status.CPUPoints = m.cpuPointsSystemSnapshot
	status.RecoveryOccupants = append([]cgroup.RecoveryOccupant(nil), m.recoverySnapshot.Occupants...)
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
	status.CPUPoints.ReconciliationDegraded = degraded
	if status.CPUPoints.IntervalEnd.IsZero() && m.cpuCapacity != nil {
		capacity := m.cpuCapacity.State()
		status.CPUPoints.CapacityAvailable = capacity.Available
		status.CPUPoints.CapacityUnavailableReason = string(capacity.UnavailableReason)
		if capacity.LastVerified.PeriodMicroseconds() != 0 {
			quota := capacity.LastVerified.QuotaMicroseconds()
			period := capacity.LastVerified.PeriodMicroseconds()
			online := capacity.LastVerified.OnlineCPUs().Value()
			status.CPUPoints.ProgrammedParentQuotaUsec = &quota
			status.CPUPoints.ProgrammedParentPeriodUsec = &period
			if capacity.Available {
				status.CPUPoints.OnlineCPUs = &online
			}
		}
		if capacity.Available {
			status.CPUPoints.DeliveryState = resmanmetrics.CPUPointsDeliveryAvailable
			status.CPUPoints.LendingState = resmanmetrics.CPUPointsLendingInactive
		} else {
			status.CPUPoints.DeliveryState = resmanmetrics.CPUPointsDeliveryUnavailable
			status.CPUPoints.LendingState = resmanmetrics.CPUPointsLendingUnavailable
		}
	}
	sort.Slice(status.CPUPointUsers, func(i, j int) bool { return status.CPUPointUsers[i].UID < status.CPUPointUsers[j].UID })

	// Read shared cgroup details without holding the manager lock.
	if summary.sharedCgroupPath != "" {
		cpuMaxFile := filepath.Join(summary.sharedCgroupPath, "cpu.max")
		if data, err := os.ReadFile(cpuMaxFile); err == nil {
			status.SharedCgroupQuota = strings.TrimSpace(string(data))
		}

		// The CPU Points hierarchy nests leaves below guaranteed and
		// best_effort. The observed allocation snapshot is authoritative;
		// counting only direct children of the parent would report zero.
		status.SharedCgroupUserCount = len(summary.cpuUsers)
	}

	return status
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

		// Clean up managed cgroups.
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
	if processPolicyChanged {
		m.previousIOEligibleUsers = make(map[int]struct{})
		m.previousBlockIOCounters = make(map[int]blockIOCounterSample)
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
	return m.epoch.BeginUpdate()
}

// RegisterPSIWatcher sets the PSI watcher for per-user cgroup monitoring.
func (m *Manager) RegisterPSIWatcher(w *cgroup.PSIWatcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.psiWatcher = w
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

func isMissingUserCgroupError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cgroup for UID") && strings.Contains(err.Error(), "not found")
}

// ControlCycleEntry represents a single control cycle entry in history
// GetControlHistory returns the recent control cycle history
// recordControlCycle records a control cycle in history
// Reset clears threshold tracking state.
// ShouldActivateLimits checks if limits should be activated based on threshold duration.
// GetElapsed returns the time since the first threshold crossing.
