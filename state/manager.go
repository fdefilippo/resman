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
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/configepoch"
	"github.com/fdefilippo/resman/internal/operationgate"
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
	executeHookScript             func(context.Context, string, limitHookEvent) error
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

	// PSI watcher for per-user adaptive CPU weight boosting
	psiWatcher   *cgroup.PSIWatcher
	psiBoostedAt map[int]time.Time // uid -> when last boosted
}

type userResourceLimitState struct {
	ram        bool
	ramApplied bool
	swap       bool
	io         bool
	ioApplied  bool
	standalone bool
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
	GetTotalCPUUsage() float64
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
	WriteMetricsToDatabase(userMetrics map[int]*resmanmetrics.UserMetrics, system resmanmetrics.SystemPersistenceMetrics) error
	GetUsernameFromUID(uid int) string
}

// CgroupManager defines the cgroup v2 operations used by the state manager.
type CgroupManager interface {
	CreateUserCgroup(uid int) error
	ApplyCPULimit(uid int, quota string) error
	ApplyCPUQuota(uid int, quota string) error
	ApplyCPUWeight(uid int, weight int) error
	RemoveCPULimit(uid int) error
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
	GetPSIStats(uid int) (cgroup.PSIStats, error)
	ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error
	CleanupUserCgroup(uid int) error
	MoveProcessToCgroup(pid int, uid int) (cgroup.ProcessMoveResult, error)
	MoveAllUserProcesses(uid int) (cgroup.ProcessMoveResult, error)
	MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) (cgroup.ProcessMoveResult, error)
	ReconcileUserProcessMembership(uid int, sharedPath, normalQuota string) (cgroup.ProcessMembershipResult, error)
	ReleaseUserFromSharedCgroup(uid int, sharedPath, normalQuota string) error
	CreateSharedCgroup() (string, error)
	ApplySharedCPULimit(sharedPath string, quota string) error
	CreateUserSubCgroup(uid int, sharedPath string) (string, error)
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
	RecordControlCycleDuration(duration time.Duration)
	RecordMetricsCollectionDuration(duration time.Duration)
	RecordError(component, errorType string)
	RecordCgroupIngressSkips(result cgroup.ProcessMoveResult)
	RecordLimitHookExecution(hookType resmanmetrics.LimitHookType, outcome resmanmetrics.LimitHookOutcome)
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
) (*Manager, error) {

	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil: required for state manager initialization")
	}

	logger := logging.GetLogger()
	hookCtx, hookCancel := context.WithCancel(context.Background())

	mgr := &Manager{
		cfg:                           cfg,
		logger:                        logger,
		limitsActive:                  false,
		limitsAppliedTime:             time.Time{},
		resourceLimitsActive:          false,
		resourceLimitsAppliedTime:     time.Time{},
		requestedCPUUsers:             make(map[int]bool),
		activeUsers:                   make(map[int]bool),
		userLimitedAt:                 make(map[int]time.Time),
		resourceLimits:                make(map[int]userResourceLimitState),
		sharedCgroupPath:              "",
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
		executeHookScript:             runLimitHookScript,
		executeHookRequest:            postLimitHook,
		metricsCache:                  make(map[string]interface{}),
		metricsCacheTime:              make(map[string]time.Time),
		controlHist: &controlHistory{
			entries: make([]ControlCycleEntry, 0),
			maxSize: 100,
		},
		previousIOEligibleUsers: make(map[int]struct{}),
		previousBlockIOCounters: make(map[int]blockIOCounterSample),
		blockIOObservedUsers:    make(map[int]bool),
		psiBoostedAt:            make(map[int]time.Time),
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
	CPULimitsActive              bool
	ResourceLimitsActive         bool
	AnyLimitsActive              bool
	CPULimitsAppliedTime         time.Time
	ResourceLimitsAppliedTime    time.Time
	ActivelyLimitedUsers         []int
	ActivelyLimitedUsersCount    int
	CPUActivelyLimitedUsers      []int
	CPUActivelyLimitedUsersCount int
	SharedCgroupPath             string
	SharedCgroupActive           bool
	SharedCgroupQuota            string
	SharedCgroupUserCount        int
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
		CPULimitsActive:              summary.cpuLimitsActive,
		ResourceLimitsActive:         summary.resourceLimitsActive,
		AnyLimitsActive:              summary.cpuLimitsActive || summary.resourceLimitsActive,
		CPULimitsAppliedTime:         summary.cpuLimitsAppliedTime,
		ResourceLimitsAppliedTime:    summary.resourceLimitsAppliedTime,
		ActivelyLimitedUsers:         summary.activelyLimitedUsers,
		ActivelyLimitedUsersCount:    len(summary.activelyLimitedUsers),
		CPUActivelyLimitedUsers:      summary.cpuUsers,
		CPUActivelyLimitedUsersCount: len(summary.cpuUsers),
		SharedCgroupPath:             summary.sharedCgroupPath,
		SharedCgroupActive:           summary.sharedCgroupPath != "" && summary.cpuLimitsActive,
	}

	// Read shared cgroup details without holding the manager lock.
	if summary.sharedCgroupPath != "" {
		cpuMaxFile := filepath.Join(summary.sharedCgroupPath, "cpu.max")
		if data, err := os.ReadFile(cpuMaxFile); err == nil {
			status.SharedCgroupQuota = strings.TrimSpace(string(data))
		}

		if entries, err := os.ReadDir(summary.sharedCgroupPath); err == nil {
			userCount := 0
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "user_") {
					userCount++
				}
			}
			status.SharedCgroupUserCount = userCount
		}
	}

	return status
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
		limitsActive := m.limitsActive || m.resourceLimitsActive
		m.mu.RUnlock()
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

// OnUserPSIEvent handles a per-user PSI pressure event by boosting CPU weight.
func (m *Manager) OnUserPSIEvent(event cgroup.PSIEvent) {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()

	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

	if event.UID <= 0 {
		return
	}
	cfg := m.GetConfig()
	boostWeight := cfg.GetPSIBoostWeight()

	m.mu.RLock()
	active := m.activeUsers[event.UID]
	m.mu.RUnlock()
	if !active {
		m.logger.Debug("Ignoring PSI event for user without active limits",
			"uid", event.UID, "type", event.Type)
		return
	}

	if err := m.cgroupManager.ApplyCPUWeight(event.UID, boostWeight); err != nil {
		m.logger.Warn("Failed to boost CPU weight on PSI event",
			"uid", event.UID, "type", event.Type,
			"weight", boostWeight, "error", err)
		return
	}

	m.mu.Lock()
	m.psiBoostedAt[event.UID] = time.Now()
	m.mu.Unlock()

	m.logger.Info("CPU weight boosted for user due to PSI pressure",
		"uid", event.UID, "type", event.Type,
		"psi_avg10", event.SomeAvg10, "weight", boostWeight)
}

// revertPSIBoosts reverts CPU weight for users whose boost duration has expired.
func (m *Manager) revertPSIBoosts() {
	cfg := m.GetConfig()
	duration := time.Duration(cfg.GetPSIBoostDuration()) * time.Second
	now := time.Now()

	// Collect expired UIDs under lock, do cgroup IO outside lock
	m.mu.Lock()
	var expired []int
	for uid, boostedAt := range m.psiBoostedAt {
		if now.Sub(boostedAt) >= duration {
			expired = append(expired, uid)
		}
	}
	m.mu.Unlock()

	if len(expired) == 0 {
		return
	}

	for _, uid := range expired {
		if err := m.cgroupManager.ApplyCPUWeight(uid, 100); err != nil {
			m.logger.Warn("Failed to revert CPU weight after PSI boost",
				"uid", uid, "error", err)
			continue
		}
		m.logger.Debug("CPU weight reverted to normal after PSI boost expired",
			"uid", uid, "boost_duration_s", cfg.GetPSIBoostDuration())
	}

	// Clean up expired entries
	m.mu.Lock()
	for _, uid := range expired {
		delete(m.psiBoostedAt, uid)
	}
	m.mu.Unlock()
}

func (m *Manager) revertAllPSIBoosts() error {
	m.mu.RLock()
	boosted := make([]int, 0, len(m.psiBoostedAt))
	for uid := range m.psiBoostedAt {
		boosted = append(boosted, uid)
	}
	m.mu.RUnlock()

	var firstError error
	var reverted []int
	for _, uid := range boosted {
		if err := m.cgroupManager.ApplyCPUWeight(uid, 100); err != nil {
			m.logger.Warn("Failed to revert CPU weight while suspending limits",
				"uid", uid,
				"error", err,
			)
			if firstError == nil {
				firstError = err
			}
			continue
		}
		reverted = append(reverted, uid)
	}

	m.mu.Lock()
	for _, uid := range reverted {
		delete(m.psiBoostedAt, uid)
	}
	m.mu.Unlock()
	return firstError
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
