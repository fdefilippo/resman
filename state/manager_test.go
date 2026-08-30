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
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	resmandatabase "github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/metrics"
)

// Mock implementations for testing
type mockMetricsCollector struct {
	allUserMetrics                   map[int]*metrics.UserMetrics
	decisionUserMetrics              map[int]*metrics.UserMetrics
	preserveExplicitEnforceableUsage bool
	usernames                        map[int]string
	systemLoad                       float64
	systemLoadErr                    error
	callMu                           sync.Mutex
	observationCalls                 int
	decisionCalls                    int
}

type failingCompletionLogger struct {
	err      error
	messages []string
}

func (l *failingCompletionLogger) Debug(string, ...interface{}) {}
func (l *failingCompletionLogger) Warn(string, ...interface{})  {}
func (l *failingCompletionLogger) Error(string, ...interface{}) {}
func (l *failingCompletionLogger) Info(string, ...interface{})  {}
func (l *failingCompletionLogger) DebugChecked(string, ...interface{}) error {
	return nil
}
func (l *failingCompletionLogger) InfoChecked(message string, _ ...interface{}) error {
	l.messages = append(l.messages, message)
	if message == "Control cycle completed" {
		return l.err
	}
	return nil
}

func (m *mockMetricsCollector) GetTotalCores() int              { return 4 }
func (m *mockMetricsCollector) GetTotalCPUUsage() float64       { return 50.0 }
func (m *mockMetricsCollector) GetUserCPUUsage(uid int) float64 { return 10.0 }
func (m *mockMetricsCollector) GetMemoryUsage() float64         { return 1024.0 }
func (m *mockMetricsCollector) GetTotalMemoryMB() float64       { return 16384.0 }
func (m *mockMetricsCollector) GetCachedMemoryMB() float64      { return 4096.0 }
func (m *mockMetricsCollector) IsSystemUnderLoad() bool         { return false }
func (m *mockMetricsCollector) GetSystemLoad() (float64, error) {
	return m.systemLoad, m.systemLoadErr
}
func (m *mockMetricsCollector) GetAllUserMetrics() map[int]*metrics.UserMetrics {
	m.callMu.Lock()
	m.observationCalls++
	m.callMu.Unlock()
	return m.prepareUserMetrics(m.allUserMetrics)
}
func (m *mockMetricsCollector) GetAllUserMetricsForDecision() map[int]*metrics.UserMetrics {
	m.callMu.Lock()
	m.decisionCalls++
	m.callMu.Unlock()
	userMetrics := m.decisionUserMetrics
	if userMetrics == nil {
		userMetrics = m.allUserMetrics
	}
	return m.prepareUserMetrics(userMetrics)
}
func (m *mockMetricsCollector) prepareUserMetrics(userMetrics map[int]*metrics.UserMetrics) map[int]*metrics.UserMetrics {
	if !m.preserveExplicitEnforceableUsage {
		for _, sample := range userMetrics {
			if sample == nil {
				continue
			}
			sample.EnforceableUsage = metrics.ProcessSetMetrics{
				CPUUsage:                               sample.CPUUsage,
				CPUUsageAverage:                        sample.CPUUsageAverage,
				CPUUsageEMA:                            sample.CPUUsageEMA,
				MemoryUsage:                            sample.MemoryUsage,
				ProcessCount:                           sample.ProcessCount,
				IOReadBytes:                            sample.IOReadBytes,
				IOWriteBytes:                           sample.IOWriteBytes,
				IOReadOps:                              sample.IOReadOps,
				IOWriteOps:                             sample.IOWriteOps,
				ExecutableIdentityUnavailableProcesses: sample.ExecutableIdentityUnavailableProcesses,
				IOUnavailableProcesses:                 sample.IOUnavailableProcesses,
			}
		}
	}
	return userMetrics
}
func (m *mockMetricsCollector) GetDBWriter() *metrics.DBWriter { return nil }
func (m *mockMetricsCollector) WriteMetricsToDatabase(userMetrics map[int]*metrics.UserMetrics, system metrics.SystemPersistenceMetrics) error {
	return nil
}

// ALL USERS metrics
func (m *mockMetricsCollector) GetAllUsers() []int             { return []int{1000, 1001, 1002} }
func (m *mockMetricsCollector) GetAllUsersCPUUsage() float64   { return 40.0 }
func (m *mockMetricsCollector) GetAllUsersMemoryUsage() uint64 { return 2000000000 }

// LIMITED USERS metrics
func (m *mockMetricsCollector) GetUsernameFromUID(uid int) string {
	if username, ok := m.usernames[uid]; ok {
		return username
	}
	return fmt.Sprintf("user%d", uid)
}

type mockCgroupManager struct{}

func (m *mockCgroupManager) CreateUserCgroup(uid int) error                            { return nil }
func (m *mockCgroupManager) ApplyCPULimit(uid int, quota string) error                 { return nil }
func (m *mockCgroupManager) ApplyCPUQuota(uid int, quota string) error                 { return nil }
func (m *mockCgroupManager) ApplyCPUWeight(uid int, weight int) error                  { return nil }
func (m *mockCgroupManager) RemoveCPULimit(uid int) error                              { return nil }
func (m *mockCgroupManager) ApplyRAMLimit(uid int, limit string) error                 { return nil }
func (m *mockCgroupManager) ApplyRAMLimitWithSwapDisabled(uid int, limit string) error { return nil }
func (m *mockCgroupManager) ApplyRAMHigh(uid int, limit string) error                  { return nil }
func (m *mockCgroupManager) ApplyRAMLimitWithHigh(uid int, maxLimit, highLimit string) error {
	return nil
}
func (m *mockCgroupManager) ApplyRAMLimitWithHighAndSwapDisabled(uid int, maxLimit, highLimit string) error {
	return nil
}
func (m *mockCgroupManager) RemoveRAMLimit(uid int) error { return nil }
func (m *mockCgroupManager) RemoveRAMHigh(uid int) error  { return nil }
func (m *mockCgroupManager) RemoveRAMSwapLimit(uid int) error {
	return nil
}
func (m *mockCgroupManager) GetCgroupRAMUsage(uid int) (uint64, error) {
	return 0, nil
}
func (m *mockCgroupManager) GetMemoryHighEvents(uid int) (uint64, error) {
	return 0, nil
}
func (m *mockCgroupManager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	return nil
}
func (m *mockCgroupManager) RemoveIOLimit(uid int) error { return nil }
func (m *mockCgroupManager) GetIOStats(uid int) (uint64, uint64, uint64, uint64, error) {
	return 0, 0, 0, 0, nil
}
func (m *mockCgroupManager) EnsureUserCgroupPlacement(uid int, sharedPath, normalQuota string) (string, cgroup.ProcessMoveResult, error) {
	return sharedPath, cgroup.ProcessMoveResult{AlreadyPresent: 1}, nil
}
func (m *mockCgroupManager) GetUserCgroupMetrics(uid int) (string, string, uint64, uint64, uint64, uint64, uint64, error) {
	return "", "", 0, 0, 0, 0, 0, nil
}
func (m *mockCgroupManager) GetPSIStats(uid int) (cgroup.PSIStats, error) {
	return cgroup.PSIStats{}, nil
}
func (m *mockCgroupManager) ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error {
	return nil
}
func (m *mockCgroupManager) CleanupUserCgroup(uid int) error { return nil }
func (m *mockCgroupManager) MoveProcessToCgroup(pid int, uid int) (cgroup.ProcessMoveResult, error) {
	return cgroup.ProcessMoveResult{Moved: 1}, nil
}
func (m *mockCgroupManager) MoveAllUserProcesses(uid int) (cgroup.ProcessMoveResult, error) {
	return cgroup.ProcessMoveResult{AlreadyPresent: 1}, nil
}
func (m *mockCgroupManager) MoveAllUserProcessesToSharedCgroup(uid int, path string) (cgroup.ProcessMoveResult, error) {
	return cgroup.ProcessMoveResult{AlreadyPresent: 1}, nil
}
func (m *mockCgroupManager) ReconcileUserProcessMembership(uid int, sharedPath, normalQuota string) (cgroup.ProcessMembershipResult, error) {
	return cgroup.ProcessMembershipResult{}, nil
}
func (m *mockCgroupManager) ReleaseUserFromSharedCgroup(uid int, path, normalQuota string) error {
	return nil
}
func (m *mockCgroupManager) CreateSharedCgroup() (string, error)                      { return "", nil }
func (m *mockCgroupManager) ApplySharedCPULimit(path string, quota string) error      { return nil }
func (m *mockCgroupManager) CreateUserSubCgroup(uid int, path string) (string, error) { return "", nil }
func (m *mockCgroupManager) CleanupAll() error                                        { return nil }
func (m *mockCgroupManager) GetCgroupInfo(uid int) (cgroup.CgroupInfo, error) {
	return cgroup.CgroupInfo{}, nil
}
func (m *mockCgroupManager) GetCreatedCgroups() []int { return nil }

type moveResultCgroupManager struct {
	mockCgroupManager
	moveResult cgroup.ProcessMoveResult
	moveErr    error
}

type membershipCall struct {
	uid         int
	sharedPath  string
	normalQuota string
}

type membershipCgroupManager struct {
	mockCgroupManager
	calls  []membershipCall
	result cgroup.ProcessMembershipResult
	err    error
}

func (m *membershipCgroupManager) ReconcileUserProcessMembership(uid int, sharedPath, normalQuota string) (cgroup.ProcessMembershipResult, error) {
	m.calls = append(m.calls, membershipCall{uid: uid, sharedPath: sharedPath, normalQuota: normalQuota})
	return m.result, m.err
}

func (m *moveResultCgroupManager) MoveAllUserProcessesToSharedCgroup(uid int, path string) (cgroup.ProcessMoveResult, error) {
	if m.moveErr != nil {
		return cgroup.ProcessMoveResult{}, m.moveErr
	}
	if m.moveResult == (cgroup.ProcessMoveResult{}) {
		return cgroup.ProcessMoveResult{Moved: 1}, nil
	}
	return m.moveResult, nil
}

type resourceOnlyCgroupManager struct {
	mockCgroupManager
	createdStandalone []int
	movedStandalone   []int
	cleanedStandalone []int
	cpuQuotas         map[int]string
	sharedCreates     int
	releasedShared    []int
	sharedQuotas      []string
	ramApplyErr       error
	ioApplyErr        error
	cleanupErr        error
}

type remediationStageCgroupManager struct {
	mockCgroupManager
	psiErrors       map[int]error
	temporaryErrors map[int][]error
	temporaryCalls  []int
}

func (m *remediationStageCgroupManager) GetPSIStats(uid int) (cgroup.PSIStats, error) {
	if err := m.psiErrors[uid]; err != nil {
		return cgroup.PSIStats{}, err
	}
	return cgroup.PSIStats{SomeAvg10: 100}, nil
}

func (m *remediationStageCgroupManager) ApplyTemporaryIOLimit(uid int, _ string, _ string, _ int, _ int, _ string, _ float64) error {
	m.temporaryCalls = append(m.temporaryCalls, uid)
	errorsForUID := m.temporaryErrors[uid]
	if len(errorsForUID) == 0 {
		return nil
	}
	err := errorsForUID[0]
	m.temporaryErrors[uid] = errorsForUID[1:]
	return err
}

type patternPolicyCgroupManager struct {
	mockCgroupManager
	cpuErrors map[int][]error
	cpuCalls  map[int]int
	ramErrors map[int][]error
	ramCalls  map[int]int
}

func (m *patternPolicyCgroupManager) ApplyCPUQuota(uid int, _ string) error {
	if m.cpuCalls == nil {
		m.cpuCalls = make(map[int]int)
	}
	m.cpuCalls[uid]++
	errorsForUID := m.cpuErrors[uid]
	if len(errorsForUID) == 0 {
		return nil
	}
	err := errorsForUID[0]
	m.cpuErrors[uid] = errorsForUID[1:]
	return err
}

func (m *patternPolicyCgroupManager) ApplyRAMLimitWithHigh(uid int, _ string, _ string) error {
	if m.ramCalls == nil {
		m.ramCalls = make(map[int]int)
	}
	m.ramCalls[uid]++
	errorsForUID := m.ramErrors[uid]
	if len(errorsForUID) == 0 {
		return nil
	}
	err := errorsForUID[0]
	m.ramErrors[uid] = errorsForUID[1:]
	return err
}

func (m *resourceOnlyCgroupManager) CreateUserCgroup(uid int) error {
	m.createdStandalone = append(m.createdStandalone, uid)
	return nil
}

func (m *resourceOnlyCgroupManager) MoveAllUserProcesses(uid int) (cgroup.ProcessMoveResult, error) {
	m.movedStandalone = append(m.movedStandalone, uid)
	return cgroup.ProcessMoveResult{Moved: 1}, nil
}

func (m *resourceOnlyCgroupManager) CleanupUserCgroup(uid int) error {
	m.cleanedStandalone = append(m.cleanedStandalone, uid)
	return m.cleanupErr
}

func (m *resourceOnlyCgroupManager) ApplyCPUQuota(uid int, quota string) error {
	if m.cpuQuotas == nil {
		m.cpuQuotas = make(map[int]string)
	}
	m.cpuQuotas[uid] = quota
	return nil
}

func (m *resourceOnlyCgroupManager) CreateSharedCgroup() (string, error) {
	m.sharedCreates++
	return "/shared", nil
}

func (m *resourceOnlyCgroupManager) ReleaseUserFromSharedCgroup(uid int, sharedPath, normalQuota string) error {
	m.releasedShared = append(m.releasedShared, uid)
	return nil
}

func (m *resourceOnlyCgroupManager) ApplySharedCPULimit(sharedPath, quota string) error {
	m.sharedQuotas = append(m.sharedQuotas, quota)
	return nil
}

func (m *resourceOnlyCgroupManager) ApplyRAMLimitWithHigh(uid int, maxLimit, highLimit string) error {
	return m.ramApplyErr
}

func (m *resourceOnlyCgroupManager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	return m.ioApplyErr
}

type prometheusErrorRecord struct {
	component string
	errorType string
}

type limitHookMetricRecord struct {
	hookType metrics.LimitHookType
	outcome  metrics.LimitHookOutcome
}

type mockPrometheusExporter struct {
	mu                         sync.Mutex
	errors                     []prometheusErrorRecord
	limitHookExecutions        []limitHookMetricRecord
	controlCycleDurations      []time.Duration
	metricsCollectionDurations []time.Duration
	lastSystemSnapshot         metrics.SystemExporterMetrics
	lastUserSnapshot           metrics.UserExporterMetrics
	systemSnapshots            int
	userMetricUpdates          int
	userMetricCleanups         int
	limitsActivated            int
	limitsDeactivated          int
	ingressSkips               []cgroup.ProcessMoveResult
}

func (m *mockPrometheusExporter) UpdateSystemSnapshot(snapshot metrics.SystemExporterMetrics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.systemSnapshots++
	m.lastSystemSnapshot = snapshot
}
func (m *mockPrometheusExporter) UpdateUserSnapshot(snapshot metrics.UserExporterMetrics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.userMetricUpdates++
	m.lastUserSnapshot = snapshot
}
func (m *mockPrometheusExporter) UpdateUserWorkloadPattern(uid int, username string, pattern string, confidence float64) {
}
func (m *mockPrometheusExporter) RecordControlCycleTrigger(trigger string) {}
func (m *mockPrometheusExporter) RecordControlCycleDuration(duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.controlCycleDurations = append(m.controlCycleDurations, duration)
}
func (m *mockPrometheusExporter) RecordMetricsCollectionDuration(duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metricsCollectionDurations = append(m.metricsCollectionDurations, duration)
}
func (m *mockPrometheusExporter) RecordError(component, errorType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors = append(m.errors, prometheusErrorRecord{component: component, errorType: errorType})
}

func (m *mockPrometheusExporter) RecordCgroupIngressSkips(result cgroup.ProcessMoveResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ingressSkips = append(m.ingressSkips, result)
}

func (m *mockPrometheusExporter) RecordLimitHookExecution(hookType metrics.LimitHookType, outcome metrics.LimitHookOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limitHookExecutions = append(m.limitHookExecutions, limitHookMetricRecord{hookType: hookType, outcome: outcome})
}
func (m *mockPrometheusExporter) Start(ctx context.Context) error { return nil }
func (m *mockPrometheusExporter) Stop() error                     { return nil }
func (m *mockPrometheusExporter) CleanupUserMetrics(activeUids map[int]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.userMetricCleanups++
}
func (m *mockPrometheusExporter) IncrementCPULimitsActivated() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limitsActivated++
}
func (m *mockPrometheusExporter) IncrementCPULimitsDeactivated() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limitsDeactivated++
}

func (m *mockPrometheusExporter) recordedErrors() []prometheusErrorRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]prometheusErrorRecord(nil), m.errors...)
}

func (m *mockPrometheusExporter) recordedLimitHookExecutions() []limitHookMetricRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]limitHookMetricRecord(nil), m.limitHookExecutions...)
}

type prometheusMetricSnapshot struct {
	errors                    []prometheusErrorRecord
	controlCycleDurations     int
	metricsCollectionDuration int
	systemSnapshots           int
	userMetricUpdates         int
	userMetricCleanups        int
	limitsActivated           int
	limitsDeactivated         int
}

func (m *mockPrometheusExporter) snapshot() prometheusMetricSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return prometheusMetricSnapshot{
		errors:                    append([]prometheusErrorRecord(nil), m.errors...),
		controlCycleDurations:     len(m.controlCycleDurations),
		metricsCollectionDuration: len(m.metricsCollectionDurations),
		systemSnapshots:           m.systemSnapshots,
		userMetricUpdates:         m.userMetricUpdates,
		userMetricCleanups:        m.userMetricCleanups,
		limitsActivated:           m.limitsActivated,
		limitsDeactivated:         m.limitsDeactivated,
	}
}

func TestNewManager(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, err := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	if manager == nil {
		t.Fatal("NewManager() returned nil")
	}
}

func TestNewManagerNilConfig(t *testing.T) {
	_, err := NewManager(nil, nil, nil, nil)

	if err == nil {
		t.Error("NewManager() should error with nil config")
	}
}

func TestControlCycleRecordsOperationalOutcomes(t *testing.T) {
	tests := []struct {
		name          string
		userMetrics   map[int]*metrics.UserMetrics
		systemLoadErr error
		moveErr       error
		wantErr       bool
		wantErrors    []prometheusErrorRecord
		wantActivated int
	}{
		{
			name: "successful cycle without transition",
		},
		{
			name:          "degraded metrics collection",
			systemLoadErr: errors.New("load unavailable"),
			wantErrors: []prometheusErrorRecord{{
				component: metricsCollectionErrorComponent,
				errorType: metricsCollectionSystemLoadError,
			}},
		},
		{
			name: "confirmed activation",
			userMetrics: map[int]*metrics.UserMetrics{
				1000: {UID: 1000, Username: "user1000", CPUUsage: 90, CPUUsageEMA: 90, EligibleForCPU: true},
			},
			wantActivated: 1,
		},
		{
			name: "failed activation",
			userMetrics: map[int]*metrics.UserMetrics{
				1000: {UID: 1000, Username: "user1000", CPUUsage: 90, CPUUsageEMA: 90, EligibleForCPU: true},
			},
			moveErr:    errors.New("move rejected"),
			wantErr:    true,
			wantErrors: []prometheusErrorRecord{{component: limitTransitionErrorComponent, errorType: limitTransitionActivationFailure}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.UserIncludeList = []string{".*"}
			cfg.CPUThreshold = 1
			cfg.CPUThresholdDuration = 0
			cfg.IgnoreSystemLoad = true
			exporter := &mockPrometheusExporter{}
			manager, err := NewManager(
				cfg,
				&mockMetricsCollector{allUserMetrics: tt.userMetrics, systemLoadErr: tt.systemLoadErr},
				&moveResultCgroupManager{moveErr: tt.moveErr},
				exporter,
			)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			manager.sharedCgroupPath = filepath.Join(t.TempDir(), "shared")

			err = manager.RunControlCycleWithTrigger(context.Background(), ControlCycleTriggerManual)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunControlCycleWithTrigger() error = %v, wantErr %t", err, tt.wantErr)
			}
			if history := manager.GetControlHistory(1); len(history) != 1 {
				t.Fatalf("control-cycle history entries = %d, want 1 even after enforcement failure", len(history))
			}

			got := exporter.snapshot()
			if got.controlCycleDurations != 1 {
				t.Errorf("control cycle duration observations = %d, want 1", got.controlCycleDurations)
			}
			if got.metricsCollectionDuration != 1 {
				t.Errorf("metrics collection duration observations = %d, want 1", got.metricsCollectionDuration)
			}
			if got.limitsActivated != tt.wantActivated {
				t.Errorf("confirmed activation count = %d, want %d", got.limitsActivated, tt.wantActivated)
			}
			if !reflect.DeepEqual(got.errors, tt.wantErrors) {
				t.Errorf("recorded errors = %+v, want %+v", got.errors, tt.wantErrors)
			}
		})
	}
}

func TestControlCyclePipelineContinuesOnlyAfterDeferredEnforcementFailure(t *testing.T) {
	stageNames := make([]string, 0, len(defaultControlCyclePipeline))
	for _, stage := range defaultControlCyclePipeline {
		stageNames = append(stageNames, stage.name)
	}
	tests := []struct {
		name               string
		failingStage       string
		wantVisited        []string
		wantDeferredErrors int
	}{
		{
			name:               "execute failure runs every protective stage",
			failingStage:       "execute_decision",
			wantVisited:        stageNames,
			wantDeferredErrors: 1,
		},
		{
			name:               "IO remediation failure runs the remaining protective stages",
			failingStage:       "io_remediation",
			wantVisited:        stageNames,
			wantDeferredErrors: 1,
		},
		{
			name:               "pattern enforcement failure runs the remaining protective stages",
			failingStage:       "workload_pattern_detection",
			wantVisited:        stageNames,
			wantDeferredErrors: 1,
		},
		{
			name:         "collection failure remains fatal",
			failingStage: "collect_metrics",
			wantVisited:  []string{"check_blackout", "collect_metrics"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			permanentErr := errors.New("permanent stage failure")
			stages := make([]controlCycleStage, len(defaultControlCyclePipeline))
			copy(stages, defaultControlCyclePipeline)
			visited := make([]string, 0, len(stages))
			for i := range stages {
				stageName := stages[i].name
				stages[i].run = func(*Manager, *controlCycleContext) error {
					visited = append(visited, stageName)
					if stageName == tt.failingStage {
						return permanentErr
					}
					return nil
				}
			}
			run := &controlCycleContext{}

			err := runControlCyclePipeline(&Manager{}, run, stages)
			if !errors.Is(err, permanentErr) {
				t.Fatalf("runControlCyclePipeline() error = %v, want permanent failure", err)
			}
			if strings.Count(err.Error(), permanentErr.Error()) != 1 {
				t.Fatalf("cycle error reported %d times, want once: %v", strings.Count(err.Error(), permanentErr.Error()), err)
			}
			if !reflect.DeepEqual(visited, tt.wantVisited) {
				t.Fatalf("visited stages = %v, want %v", visited, tt.wantVisited)
			}
			if len(run.deferredErrors) != tt.wantDeferredErrors {
				t.Fatalf("deferred errors = %d, want %d", len(run.deferredErrors), tt.wantDeferredErrors)
			}
		})
	}
}

func TestControlCycleOwnerReceivesCompletionLogFailureAfterProtectiveTail(t *testing.T) {
	sinkErr := errors.New("injected completion log failure")
	logger := &failingCompletionLogger{err: sinkErr}
	manager := &Manager{logger: logger}
	run := &controlCycleContext{
		cfg:       config.DefaultConfig(),
		metrics:   &SystemMetrics{},
		startTime: time.Now(),
		trigger:   ControlCycleTriggerManual,
	}
	tailRan := false
	stages := []controlCycleStage{
		{
			name: "protective_tail",
			run: func(*Manager, *controlCycleContext) error {
				tailRan = true
				return nil
			},
		},
		{name: "log_completion", run: (*Manager).stageLogCompletion},
	}

	err := runControlCyclePipeline(manager, run, stages)
	if !tailRan {
		t.Fatal("control-cycle protective tail did not run before logging failed")
	}
	if !errors.Is(err, sinkErr) {
		t.Fatalf("runControlCyclePipeline() error = %v, want logging sink failure", err)
	}
	if !reflect.DeepEqual(logger.messages, []string{"Control cycle completed"}) {
		t.Fatalf("logger messages = %v", logger.messages)
	}
}

func TestMetricsRefreshRecordsCollectionWithoutControlCycle(t *testing.T) {
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(
		config.DefaultConfig(),
		&mockMetricsCollector{},
		&mockCgroupManager{},
		exporter,
	)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	if err := manager.RunMetricsRefresh(context.Background(), "test"); err != nil {
		t.Fatalf("RunMetricsRefresh() error: %v", err)
	}

	got := exporter.snapshot()
	if got.metricsCollectionDuration != 1 {
		t.Errorf("metrics collection duration observations = %d, want 1", got.metricsCollectionDuration)
	}
	if got.controlCycleDurations != 0 {
		t.Errorf("control cycle duration observations = %d, want 0", got.controlCycleDurations)
	}
}

func TestDeactivationMetricsRequireConfirmedTransition(t *testing.T) {
	tests := []struct {
		name            string
		releaseErr      error
		repeat          bool
		wantErr         bool
		wantErrors      []prometheusErrorRecord
		wantDeactivated int
	}{
		{
			name:            "successful transition is counted once",
			repeat:          true,
			wantDeactivated: 1,
		},
		{
			name:       "failed transition is not counted",
			releaseErr: errors.New("release rejected"),
			wantErr:    true,
			wantErrors: []prometheusErrorRecord{{component: limitTransitionErrorComponent, errorType: limitTransitionDeactivationFailure}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exporter := &mockPrometheusExporter{}
			cgroups := &deactivateCgroupManager{releaseErrors: map[int]error{1000: tt.releaseErr}}
			manager, err := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, cgroups, exporter)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			manager.limitsActive = true
			manager.sharedCgroupPath = t.TempDir()
			manager.activeUsers[1000] = true

			err = manager.deactivateLimits()
			if (err != nil) != tt.wantErr {
				t.Fatalf("deactivateLimits() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.repeat {
				if err := manager.deactivateLimits(); err != nil {
					t.Fatalf("second deactivateLimits() error: %v", err)
				}
			}

			got := exporter.snapshot()
			if got.limitsDeactivated != tt.wantDeactivated {
				t.Errorf("confirmed deactivation count = %d, want %d", got.limitsDeactivated, tt.wantDeactivated)
			}
			if !reflect.DeepEqual(got.errors, tt.wantErrors) {
				t.Errorf("recorded errors = %+v, want %+v", got.errors, tt.wantErrors)
			}
		})
	}
}

func TestWriteDatabaseMetricsReportsTransactionFailureAndRetries(t *testing.T) {
	dbPath := privateMetricsDatabasePath(t)
	dbManager, err := resmandatabase.NewDatabaseManager(dbPath)
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	t.Cleanup(func() {
		if err := dbManager.Close(); err != nil {
			t.Errorf("DatabaseManager.Close() error: %v", err)
		}
	})

	controlDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}
	t.Cleanup(func() {
		if err := controlDB.Close(); err != nil {
			t.Errorf("control database Close() error: %v", err)
		}
	})
	if _, err := controlDB.Exec(`
		CREATE TRIGGER reject_uid_1001
		BEFORE INSERT ON user_metrics
		WHEN NEW.uid = 1001
		BEGIN
			SELECT RAISE(ABORT, 'rejected test UID');
		END;
	`); err != nil {
		t.Fatalf("failed to create rejection trigger: %v", err)
	}

	cfg := config.DefaultConfig()
	collector, err := metrics.NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)

	writer := metrics.NewDBWriter(dbManager, 3600)
	collector.SetDBWriter(writer)
	exporter := &mockPrometheusExporter{}
	manager := &Manager{
		logger:             logging.GetLogger(),
		metricsCollector:   collector,
		prometheusExporter: exporter,
		activeUsers:        map[int]bool{1001: true},
	}
	sample := &SystemMetrics{
		TotalCPUUsage: 50,
		TotalCores:    4,
		SystemLoad:    2.5,
		UserMetrics: map[int]*metrics.UserMetrics{
			1001: {
				UID:            1001,
				Username:       "transaction-test",
				CPUUsage:       25,
				MemoryUsage:    4096,
				ProcessCount:   2,
				EligibleForCPU: true,
				CPULimitActive: true,
			},
		},
	}

	err = collector.WriteMetricsToDatabase(
		sample.UserMetrics,
		metrics.SystemPersistenceMetrics{
			TotalCPUUsagePercent:         sample.TotalCPUUsage,
			TotalCores:                   sample.TotalCores,
			SystemLoad:                   sample.SystemLoad,
			CPULimitsActive:              true,
			AnyLimitsActive:              true,
			CPUActivelyLimitedUsersCount: 1,
			ActivelyLimitedUsersCount:    1,
		},
	)
	if err == nil {
		t.Fatal("WriteMetricsToDatabase() expected a transaction error")
	}
	for _, fragment := range []string{"write metrics to database", "rejected test UID"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("WriteMetricsToDatabase() error = %q, want fragment %q", err, fragment)
		}
	}
	if !writer.ShouldWrite() {
		t.Fatal("failed direct write marked the database writer as written")
	}

	start := time.Now().Add(-time.Minute)
	end := time.Now().Add(time.Minute)
	tests := []struct {
		name            string
		prepare         func(t *testing.T)
		wantErrors      []prometheusErrorRecord
		wantShouldWrite bool
		wantSystemRows  int
		wantUserRows    int
	}{
		{
			name:            "failed transaction remains retryable",
			wantErrors:      []prometheusErrorRecord{{component: metricsDatabaseErrorComponent, errorType: metricsDatabaseWriteFailure}},
			wantShouldWrite: true,
		},
		{
			name: "successful retry marks write",
			prepare: func(t *testing.T) {
				t.Helper()
				if _, err := controlDB.Exec("DROP TRIGGER reject_uid_1001"); err != nil {
					t.Fatalf("failed to drop rejection trigger: %v", err)
				}
			},
			wantErrors:      []prometheusErrorRecord{{component: metricsDatabaseErrorComponent, errorType: metricsDatabaseWriteFailure}},
			wantShouldWrite: false,
			wantSystemRows:  1,
			wantUserRows:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepare != nil {
				tt.prepare(t)
			}

			manager.writeDatabaseMetrics(sample)

			if got := exporter.recordedErrors(); !reflect.DeepEqual(got, tt.wantErrors) {
				t.Errorf("recorded Prometheus errors = %+v, want %+v", got, tt.wantErrors)
			}
			if got := writer.ShouldWrite(); got != tt.wantShouldWrite {
				t.Errorf("DBWriter.ShouldWrite() = %t, want %t", got, tt.wantShouldWrite)
			}

			systemHistory, err := dbManager.GetSystemHistory(start, end, 10)
			if err != nil {
				t.Fatalf("GetSystemHistory() error: %v", err)
			}
			if len(systemHistory) != tt.wantSystemRows {
				t.Errorf("system rows = %d, want %d", len(systemHistory), tt.wantSystemRows)
			}
			userHistory, err := dbManager.GetUserHistory(1001, start, end, 10)
			if err != nil {
				t.Fatalf("GetUserHistory() error: %v", err)
			}
			if len(userHistory) != tt.wantUserRows {
				t.Errorf("user rows = %d, want %d", len(userHistory), tt.wantUserRows)
			}
		})
	}
}

func privateMetricsDatabasePath(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", root, err)
	}
	dir := filepath.Join(root, "database")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("os.Mkdir(%s) error = %v", dir, err)
	}
	return filepath.Join(dir, "metrics.db")
}

func TestMakeDecision(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 75
	cfg.CPUReleaseThreshold = 40
	cfg.MinActiveTime = 60
	cfg.CPUThresholdDuration = 0 // Disable time window for immediate activation

	manager := &Manager{
		cfg:                cfg,
		limitsActive:       false,
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
	}

	metrics := &SystemMetrics{
		CPUEligibleCPUUsage: 80.0, // Above threshold
		TotalCores:          4,
		SystemUnderLoad:     false,
	}

	decision, reason := manager.makeDecision(metrics)

	if decision != "ACTIVATE_LIMITS" {
		t.Errorf("makeDecision(): got %s, expected ACTIVATE_LIMITS", decision)
	}
	if reason == "" {
		t.Error("makeDecision() should return a reason")
	}
}

func TestMakeDecisionMinimumActiveTimeUsesMostRecentEnforcementEpoch(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUReleaseThreshold = 40
	cfg.MinActiveTime = 300

	now := time.Now()
	expired := now.Add(-10 * time.Minute)
	recent := now.Add(-30 * time.Second)
	tests := []struct {
		name                string
		cpuActivation       time.Time
		resourceActivation  time.Time
		wantDecision        string
		wantReasonSubstring string
	}{
		{
			name:                "newer RAM or IO epoch protects older CPU enforcement",
			cpuActivation:       expired,
			resourceActivation:  recent,
			wantDecision:        "MAINTAIN_CURRENT_STATE",
			wantReasonSubstring: "minimum activation time",
		},
		{
			name:                "newer CPU epoch protects older RAM or IO enforcement",
			cpuActivation:       recent,
			resourceActivation:  expired,
			wantDecision:        "MAINTAIN_CURRENT_STATE",
			wantReasonSubstring: "minimum activation time",
		},
		{
			name:               "release proceeds after both enforcement epochs expire",
			cpuActivation:      expired,
			resourceActivation: expired,
			wantDecision:       "DEACTIVATE_LIMITS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{
				cfg:                       cfg,
				limitsActive:              true,
				limitsAppliedTime:         tt.cpuActivation,
				resourceLimitsActive:      true,
				resourceLimitsAppliedTime: tt.resourceActivation,
				thresholdTracker:          &ThresholdTracker{},
				ioThresholdTracker:        &ThresholdTracker{},
				stabilityTracker:          newUserStabilityTracker(),
			}
			decision, reason := manager.makeDecision(&SystemMetrics{
				CPUEligibleCPUUsage: 0,
				TotalCores:          4,
				SystemUnderLoad:     false,
				UserMetrics:         map[int]*metrics.UserMetrics{},
			})

			if decision != tt.wantDecision {
				t.Fatalf("decision = %s, want %s (reason: %s)", decision, tt.wantDecision, reason)
			}
			if tt.wantReasonSubstring != "" && !strings.Contains(reason, tt.wantReasonSubstring) {
				t.Fatalf("reason = %q, want substring %q", reason, tt.wantReasonSubstring)
			}
		})
	}
}

func TestCollectSystemMetricsUsesIndependentEligibilityAggregates(t *testing.T) {
	cfg := config.DefaultConfig()
	collector := &mockMetricsCollector{allUserMetrics: map[int]*metrics.UserMetrics{
		1000: {
			UID:                                    1000,
			Username:                               "alice",
			CPUUsage:                               42,
			MemoryUsage:                            4096,
			IOWriteBytes:                           8192,
			ExecutableIdentityUnavailableProcesses: 2,
			IOUnavailableProcesses:                 3,
		},
	}}
	manager, err := NewManager(cfg, collector, &mockCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sample, err := manager.collectSystemMetricsForRefresh()
	if err != nil {
		t.Fatalf("collectSystemMetricsForRefresh() error: %v", err)
	}
	if sample.CPUEligibleUsersCount != 0 || sample.CPUEligibleCPUUsage != 0 {
		t.Fatalf("CPU aggregate = count %d usage %.1f, want disabled by empty CPU include list",
			sample.CPUEligibleUsersCount, sample.CPUEligibleCPUUsage)
	}
	if sample.RAMEligibleUsersCount != 1 || sample.RAMEligibleUsageBytes != 4096 {
		t.Fatalf("RAM aggregate = count %d bytes %d, want 1 and 4096",
			sample.RAMEligibleUsersCount, sample.RAMEligibleUsageBytes)
	}
	if sample.IOEligibleUsersCount != 1 {
		t.Fatalf("IO eligible count = %d, want 1", sample.IOEligibleUsersCount)
	}
	if sample.IOEligibleUnavailableProcesses != 3 {
		t.Fatalf("IO eligible unavailable processes = %d, want 3", sample.IOEligibleUnavailableProcesses)
	}
	if sample.ProcFSExecutableIdentityUnavailableProcesses != 2 || sample.ProcFSIOUnavailableProcesses != 3 {
		t.Fatalf("procfs coverage = identity %d IO %d, want 2 and 3",
			sample.ProcFSExecutableIdentityUnavailableProcesses, sample.ProcFSIOUnavailableProcesses)
	}
	user := sample.UserMetrics[1000]
	if user == nil || user.EligibleForCPU || !user.EligibleForRAM || !user.EligibleForIO {
		t.Fatalf("explicit user eligibility = %+v, want CPU=false RAM=true IO=true", user)
	}
}

func TestInterleavedMetricsRefreshDoesNotChangeControlDecisionSample(t *testing.T) {
	tests := []struct {
		name         string
		refreshCount int
	}{
		{name: "control only", refreshCount: 0},
		{name: "one refresh", refreshCount: 1},
		{name: "three refreshes", refreshCount: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.UserIncludeList = []string{"^alice$"}
			cfg.CPUThreshold = 75
			cfg.CPUThresholdDuration = 0
			cfg.AutodetectPatterns = true
			collector := &mockMetricsCollector{
				allUserMetrics: map[int]*metrics.UserMetrics{
					1000: {UID: 1000, Username: "alice", CPUUsage: 90, CPUUsageEMA: 90, ProcessCount: 1},
				},
				decisionUserMetrics: map[int]*metrics.UserMetrics{
					1000: {UID: 1000, Username: "alice", CPUUsage: 10, CPUUsageEMA: 10, ProcessCount: 1},
				},
			}
			exporter := &mockPrometheusExporter{}
			manager, err := NewManager(cfg, collector, &mockCgroupManager{}, exporter)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}

			for range tt.refreshCount {
				if err := manager.RunMetricsRefresh(context.Background(), "test_refresh"); err != nil {
					t.Fatalf("RunMetricsRefresh() error: %v", err)
				}
			}
			refreshSnapshot := exporter.snapshot()
			if refreshSnapshot.systemSnapshots != tt.refreshCount {
				t.Fatalf("system snapshots after observation refresh = %d, want %d", refreshSnapshot.systemSnapshots, tt.refreshCount)
			}
			if refreshSnapshot.userMetricUpdates != 0 || refreshSnapshot.userMetricCleanups != 0 {
				t.Fatalf(
					"observation refresh touched decision-owned user series: updates=%d cleanups=%d",
					refreshSnapshot.userMetricUpdates,
					refreshSnapshot.userMetricCleanups,
				)
			}
			if err := manager.RunControlCycle(context.Background()); err != nil {
				t.Fatalf("RunControlCycle() error: %v", err)
			}
			cycleSnapshot := exporter.snapshot()
			if cycleSnapshot.userMetricUpdates != 1 || cycleSnapshot.userMetricCleanups != 1 {
				t.Fatalf(
					"decision-owned user series writes = %d, cleanups = %d, want one each",
					cycleSnapshot.userMetricUpdates,
					cycleSnapshot.userMetricCleanups,
				)
			}

			if manager.limitsActive {
				t.Fatal("observation-only CPU sample activated limits")
			}
			collector.callMu.Lock()
			observationCalls := collector.observationCalls
			decisionCalls := collector.decisionCalls
			collector.callMu.Unlock()
			if observationCalls != tt.refreshCount {
				t.Fatalf("observation calls = %d, want %d", observationCalls, tt.refreshCount)
			}
			if decisionCalls != 1 {
				t.Fatalf("decision calls = %d, want 1", decisionCalls)
			}
			history := manager.GetControlHistory(1)
			if len(history) != 1 || history[0].Decision != "MAINTAIN_CURRENT_STATE" {
				t.Fatalf("control history = %+v, want one MAINTAIN decision", history)
			}
		})
	}
}

func TestCollectSystemMetricsKeepsExcludedUsageOutOfEveryDecisionAggregate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UserIncludeList = []string{"^alice$"}
	cfg.CPUThreshold = 1
	cfg.CPUThresholdDuration = 0
	cfg.RAMEnabled = true
	cfg.RAMThreshold = 1
	cfg.IOEnabled = true
	cfg.IOThreshold = 1
	cfg.IOThresholdDuration = 0
	cfg.IOReadBPS = "1"
	cfg.IOWriteBPS = "max"
	cfg.IOReadIOPS = 0
	cfg.IOWriteIOPS = 0
	collector := &mockMetricsCollector{
		preserveExplicitEnforceableUsage: true,
		allUserMetrics: map[int]*metrics.UserMetrics{
			1000: {
				UID: 1000, Username: "alice", CPUUsage: 90, CPUUsageEMA: 90,
				MemoryUsage: 16 * 1024 * 1024 * 1024, ProcessCount: 1,
				IOReadBytes: 1 << 30,
			},
		},
	}
	manager, err := NewManager(cfg, collector, &mockCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	first, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("first collectSystemMetrics() error: %v", err)
	}
	manager.prevIOTime = time.Now().Add(-time.Second)
	collector.allUserMetrics[1000].IOReadBytes += 1 << 30
	second, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("second collectSystemMetrics() error: %v", err)
	}
	if first.AllUsersCPUUsage != 90 || second.AllUsersMemoryUsage != 16*1024*1024*1024 {
		t.Fatalf("observed totals lost excluded usage: first=%+v second=%+v", first, second)
	}
	if second.CPUEligibleCPUUsage != 0 || second.RAMEligibleUsageBytes != 0 || second.IOEligibleReadBPS != 0 {
		t.Fatalf("decision aggregates include excluded usage: CPU=%.1f RAM=%d IO=%.1f",
			second.CPUEligibleCPUUsage, second.RAMEligibleUsageBytes, second.IOEligibleReadBPS)
	}
	if decision, reason := manager.makeDecision(second); decision != "MAINTAIN_CURRENT_STATE" {
		t.Fatalf("excluded-only decision = %s (%s), want maintenance", decision, reason)
	}
}

func TestManagerUpdateConfigResetsIODecisionBaselineOnProcessPolicyChange(t *testing.T) {
	cfg := config.DefaultConfig()
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.previousIOEligibleUsers[1000] = struct{}{}
	manager.prevIOTime = time.Now()

	reloaded := config.DefaultConfig()
	reloaded.ProcessExcludeList = []string{"^stress$"}
	manager.UpdateConfig(reloaded)
	if len(manager.previousIOEligibleUsers) != 0 || !manager.prevIOTime.IsZero() {
		t.Fatalf("I/O baseline after process-policy reload = %v at %v, want empty",
			manager.previousIOEligibleUsers, manager.prevIOTime)
	}
}

func TestMakeDecisionUsesIndependentResourceAggregates(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*config.Config)
		metrics   *SystemMetrics
	}{
		{
			name: "RAM can activate with no CPU-eligible users",
			configure: func(cfg *config.Config) {
				cfg.RAMEnabled = true
				cfg.RAMThreshold = 50
			},
			metrics: &SystemMetrics{
				TotalCores:            4,
				TotalMemoryMB:         100,
				RAMEligibleUsageBytes: 60 * 1024 * 1024,
			},
		},
		{
			name: "IO can activate with no CPU-eligible users",
			configure: func(cfg *config.Config) {
				cfg.IOEnabled = true
				cfg.IOThreshold = 50
				cfg.IOWriteBPS = "100M"
				cfg.IOThresholdDuration = 0
			},
			metrics: &SystemMetrics{
				TotalCores:           4,
				IOEligibleUsersCount: 1,
				IOEligibleWriteBPS:   60 * 1024 * 1024,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.CPUThresholdDuration = 0
			tt.configure(cfg)
			manager := &Manager{
				cfg:                cfg,
				thresholdTracker:   &ThresholdTracker{},
				ioThresholdTracker: &ThresholdTracker{},
			}
			decision, reason := manager.makeDecision(tt.metrics)
			if decision != "ACTIVATE_LIMITS" {
				t.Fatalf("makeDecision() = %s (%s), want ACTIVATE_LIMITS", decision, reason)
			}
		})
	}
}

func TestMinSystemCoresGatesOnlyCPUEnforcement(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*config.Config)
		metrics   *SystemMetrics
	}{
		{
			name: "RAM-only pressure activates on a reserved two-core host",
			configure: func(cfg *config.Config) {
				cfg.RAMEnabled = true
				cfg.RAMThreshold = 10
			},
			metrics: &SystemMetrics{TotalCores: 2, TotalMemoryMB: 1024, RAMEligibleUsageBytes: 256 * 1024 * 1024},
		},
		{
			name: "IO-only pressure activates on a reserved two-core host",
			configure: func(cfg *config.Config) {
				cfg.IOEnabled = true
				cfg.IOThreshold = 10
				cfg.IOWriteBPS = "1M"
			},
			metrics: &SystemMetrics{TotalCores: 2, IOEligibleUsersCount: 1, IOEligibleWriteBPS: 2 * 1024 * 1024},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.MinSystemCores = 2
			cfg.CPUThreshold = 100
			cfg.IgnoreSystemLoad = true
			tt.configure(cfg)
			manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, &mockPrometheusExporter{})
			if err != nil {
				t.Fatalf("NewManager() error = %v", err)
			}

			decision, _ := manager.makeDecision(tt.metrics)
			if decision != "ACTIVATE_LIMITS" {
				t.Fatalf("makeDecision() = %q, want ACTIVATE_LIMITS", decision)
			}
		})
	}
}

func TestResourceOnlyUsersUseStandaloneCgroupsWithoutCPUThrottle(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*config.Config)
		eligibility config.UserEligibility
		wantRAM     bool
		wantIO      bool
	}{
		{
			name: "RAM-only user",
			configure: func(cfg *config.Config) {
				cfg.RAMEnabled = true
				cfg.IOEnabled = false
			},
			eligibility: config.UserEligibility{EligibleForRAM: true},
			wantRAM:     true,
		},
		{
			name: "IO-only user",
			configure: func(cfg *config.Config) {
				cfg.RAMEnabled = false
				cfg.IOEnabled = true
			},
			eligibility: config.UserEligibility{EligibleForIO: true},
			wantIO:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.MinSystemCores = 2
			tt.configure(cfg)
			cgroups := &resourceOnlyCgroupManager{}
			exporter := &mockPrometheusExporter{}
			manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, exporter)
			if err != nil {
				t.Fatalf("NewManager() error = %v", err)
			}
			metrics := &SystemMetrics{
				TotalCores:   2,
				UserCPUUsage: map[int]float64{1000: 1},
				UserMetrics: map[int]*metrics.UserMetrics{
					1000: {
						UID:            1000,
						Username:       "resource-user",
						EligibleForRAM: tt.eligibility.EligibleForRAM,
						EligibleForIO:  tt.eligibility.EligibleForIO,
					},
				},
			}

			if err := manager.activateLimits(metrics); err != nil {
				t.Fatalf("activateLimits() error = %v", err)
			}
			if cgroups.sharedCreates != 0 {
				t.Fatalf("shared CPU cgroup creations = %d, want 0", cgroups.sharedCreates)
			}
			if len(cgroups.createdStandalone) != 1 || len(cgroups.movedStandalone) != 1 {
				t.Fatalf("standalone create/move = %v/%v, want [1000]/[1000]", cgroups.createdStandalone, cgroups.movedStandalone)
			}
			if got := cgroups.cpuQuotas[1000]; got != "max 100000" {
				t.Fatalf("standalone cpu.max = %q, want max 100000", got)
			}
			state := manager.resourceLimits[1000]
			if !state.standalone || state.ram != tt.wantRAM || state.ramApplied != tt.wantRAM || state.io != tt.wantIO || state.ioApplied != tt.wantIO {
				t.Fatalf("resource state = %+v, want standalone RAM=%t IO=%t requested and applied", state, tt.wantRAM, tt.wantIO)
			}
			if manager.activeUsers[1000] || manager.requestedCPUUsers[1000] {
				t.Fatal("resource-only user was marked as CPU-requested or CPU-active")
			}
			if manager.limitsActive || !manager.resourceLimitsActive {
				t.Fatal("resource-only enforcement changed CPU status or failed to activate resource status")
			}

			if err := manager.deactivateLimits(); err != nil {
				t.Fatalf("deactivateLimits() error = %v", err)
			}
			if len(cgroups.cleanedStandalone) != 1 || cgroups.cleanedStandalone[0] != 1000 {
				t.Fatalf("standalone cleanup = %v, want [1000]", cgroups.cleanedStandalone)
			}
			if manager.limitsActive || manager.resourceLimitsActive {
				t.Fatal("CPU or resource status remained active after standalone cleanup")
			}
		})
	}
}

func TestStandalonePartialFailurePreservesIntentAndObservedSuccess(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MinSystemCores = 2
	cfg.RAMEnabled = true
	cfg.IOEnabled = true
	cgroups := &resourceOnlyCgroupManager{ioApplyErr: errors.New("io controller rejected limit")}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	metrics := &SystemMetrics{
		TotalCores:   2,
		UserCPUUsage: map[int]float64{1000: 1},
		UserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "resource-user", EligibleForRAM: true, EligibleForIO: true},
		},
	}

	err = manager.activateLimits(metrics)
	if err == nil || !strings.Contains(err.Error(), "io controller rejected limit") {
		t.Fatalf("activateLimits() error = %v, want IO failure", err)
	}
	state := manager.resourceLimits[1000]
	if !state.standalone || !state.ram || !state.ramApplied || !state.io || state.ioApplied {
		t.Fatalf("resource state after partial failure = %+v, want RAM active and IO requested but inactive", state)
	}
	if manager.limitsActive || !manager.resourceLimitsActive {
		t.Fatal("successful RAM enforcement was lost after partial IO failure")
	}
}

func TestStandaloneResourceLifecycleReconcilesReloadAndCleanupFailure(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MinSystemCores = 2
	cfg.RAMEnabled = true
	cfg.IOEnabled = false
	cgroups := &resourceOnlyCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	metrics := &SystemMetrics{
		TotalCores:   2,
		UserCPUUsage: map[int]float64{1000: 1},
		UserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "resource-user", EligibleForRAM: true, EligibleForIO: true},
		},
	}
	if err := manager.activateLimits(metrics); err != nil {
		t.Fatalf("activateLimits() error = %v", err)
	}

	cfg.RAMEnabled = false
	cfg.IOEnabled = true
	if err := manager.reconcileStandaloneResourceUsers(metrics, cfg); err != nil {
		t.Fatalf("reconcileStandaloneResourceUsers() error = %v", err)
	}
	state := manager.resourceLimits[1000]
	if !state.standalone || state.ram || state.ramApplied || !state.io || !state.ioApplied {
		t.Fatalf("resource state after reload = %+v, want standalone IO-only enforcement", state)
	}

	cgroups.cleanupErr = errors.New("cgroup still populated")
	err = manager.deactivateLimits()
	if err == nil || !strings.Contains(err.Error(), "cgroup still populated") {
		t.Fatalf("deactivateLimits() error = %v, want cleanup failure", err)
	}
	state = manager.resourceLimits[1000]
	if !state.standalone || !state.ioApplied || !manager.resourceLimitsActive {
		t.Fatalf("failed cleanup lost observed state: state=%+v resourceLimitsActive=%t", state, manager.resourceLimitsActive)
	}

	cgroups.cleanupErr = nil
	if err := manager.deactivateLimits(); err != nil {
		t.Fatalf("deactivateLimits() retry error = %v", err)
	}
	if _, exists := manager.resourceLimits[1000]; exists || manager.limitsActive || manager.resourceLimitsActive {
		t.Fatalf("successful cleanup retry left state=%+v cpuActive=%t resourceActive=%t", manager.resourceLimits[1000], manager.limitsActive, manager.resourceLimitsActive)
	}
}

func TestMaintenanceMigratesStandaloneUserAfterCPUEligibilityReload(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MinSystemCores = 2
	cfg.RAMEnabled = true
	cgroups := &resourceOnlyCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	resourceMetrics := &SystemMetrics{
		TotalCores:   4,
		UserCPUUsage: map[int]float64{1000: 1},
		UserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "resource-user", CPUUsageEMA: 1, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: 1}, EligibleForRAM: true},
		},
	}
	if err := manager.activateLimits(resourceMetrics); err != nil {
		t.Fatalf("activateLimits() error = %v", err)
	}

	resourceMetrics.CPUEligibleUsers = []int{1000}
	resourceMetrics.UserMetrics[1000].EligibleForCPU = true
	if err := manager.releaseIdleUsers(resourceMetrics); err != nil {
		t.Fatalf("releaseIdleUsers() error = %v", err)
	}
	if cgroups.sharedCreates != 1 {
		t.Fatalf("shared CPU cgroup creations = %d, want 1", cgroups.sharedCreates)
	}
	if len(cgroups.cleanedStandalone) != 1 || !manager.activeUsers[1000] {
		t.Fatalf("standalone cleanup/CPU state = %v/%t, want [1000]/true", cgroups.cleanedStandalone, manager.activeUsers[1000])
	}
	if !manager.limitsActive {
		t.Fatal("CPU aggregate state did not activate after standalone-to-shared migration")
	}
	if manager.resourceLimits[1000].standalone {
		t.Fatal("user remained marked standalone after migration to shared CPU enforcement")
	}
}

func TestMaintenanceMigratesCPUUserToStandaloneWhenMinSystemCoresBlocksCPU(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MinSystemCores = 2
	cfg.RAMEnabled = true
	cgroups := &resourceOnlyCgroupManager{}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, exporter)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	manager.limitsActive = true
	manager.limitsAppliedTime = time.Now().Add(-time.Minute)
	manager.activeUsers[1000] = true
	manager.requestedCPUUsers[1000] = true
	manager.sharedCgroupPath = "/shared"

	sample := &SystemMetrics{
		TotalCores:       2,
		CPUEligibleUsers: []int{1000},
		UserCPUUsage:     map[int]float64{1000: 1},
		UserMetrics: map[int]*metrics.UserMetrics{
			1000: {
				UID:            1000,
				Username:       "resource-user",
				CPUUsageEMA:    1,
				EligibleForCPU: true,
				EligibleForRAM: true,
			},
		},
	}
	if err := manager.releaseIdleUsers(sample); err != nil {
		t.Fatalf("releaseIdleUsers() error = %v", err)
	}
	if !reflect.DeepEqual(cgroups.releasedShared, []int{1000}) {
		t.Fatalf("released shared users = %v, want [1000]", cgroups.releasedShared)
	}
	state := manager.resourceLimits[1000]
	if !state.standalone || !state.ram || !state.ramApplied {
		t.Fatalf("resource state = %+v, want active standalone RAM", state)
	}
	if manager.limitsActive || manager.activeUsers[1000] || manager.requestedCPUUsers[1000] {
		t.Fatalf("CPU state remained active: aggregate=%t active=%t requested=%t",
			manager.limitsActive, manager.activeUsers[1000], manager.requestedCPUUsers[1000])
	}
	if !manager.resourceLimitsActive {
		t.Fatal("RAM enforcement was not kept active during CPU release")
	}
	if !reflect.DeepEqual(cgroups.sharedQuotas, []string{"max 100000"}) {
		t.Fatalf("shared CPU quotas = %v, want [max 100000]", cgroups.sharedQuotas)
	}
	if got := exporter.snapshot(); got.limitsDeactivated != 1 || got.limitsActivated != 0 {
		t.Fatalf("CPU transition counters = activated %d deactivated %d, want 0/1",
			got.limitsActivated, got.limitsDeactivated)
	}
}

func TestActivationWithoutEligibleEnforcementReportsBoundedFailure(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MinSystemCores = 2
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	err = manager.activateLimits(&SystemMetrics{
		TotalCores:   2,
		UserCPUUsage: map[int]float64{1000: 1},
		UserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "ineligible-user"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no CPU, RAM, or IO enforcement") {
		t.Fatalf("activateLimits() error = %v, want explicit no-enforcement failure", err)
	}
	recorded := exporter.recordedErrors()
	if len(recorded) != 1 || recorded[0].component != limitTransitionErrorComponent || recorded[0].errorType != limitTransitionActivationFailure {
		t.Fatalf("recorded errors = %+v, want one bounded activation failure", recorded)
	}
}

func TestActivationWithOnlyNestedNamespaceProcessesIsDegradedNoop(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroups := &moveResultCgroupManager{moveResult: cgroup.ProcessMoveResult{
		Candidates:             1,
		PIDNamespaceMismatches: 1,
	}}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, exporter)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	err = manager.activateLimits(&SystemMetrics{
		TotalCores:       4,
		UserCPUUsage:     map[int]float64{1000: 100},
		CPUEligibleUsers: []int{1000},
		UserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "nested-user", EligibleForCPU: true},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no process entered the ResMan cgroup") {
		t.Fatalf("activateLimits() error = %v, want namespace-boundary no-op", err)
	}
	if manager.activeUsers[1000] || manager.limitsActive {
		t.Fatalf("nested-only enforcement reported active: user=%t aggregate=%t", manager.activeUsers[1000], manager.limitsActive)
	}
	if exporter.limitsActivated != 0 || len(exporter.ingressSkips) != 1 {
		t.Fatalf("activation counter=%d ingress skip records=%+v", exporter.limitsActivated, exporter.ingressSkips)
	}
}

func TestActivationWithHostAndNestedProcessesConfirmsOnlyMovedWork(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroups := &moveResultCgroupManager{moveResult: cgroup.ProcessMoveResult{
		Candidates:             2,
		Moved:                  1,
		PIDNamespaceMismatches: 1,
	}}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, exporter)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	err = manager.activateLimits(&SystemMetrics{
		TotalCores:       4,
		UserCPUUsage:     map[int]float64{1000: 100},
		CPUEligibleUsers: []int{1000},
		UserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "mixed-user", EligibleForCPU: true},
		},
	})
	if err != nil {
		t.Fatalf("activateLimits() error = %v", err)
	}
	if !manager.activeUsers[1000] || !manager.limitsActive || exporter.limitsActivated != 1 {
		t.Fatalf("mixed enforcement state: user=%t aggregate=%t activations=%d", manager.activeUsers[1000], manager.limitsActive, exporter.limitsActivated)
	}
	if len(exporter.ingressSkips) != 1 || exporter.ingressSkips[0].PIDNamespaceMismatches != 1 {
		t.Fatalf("ingress skip records = %+v", exporter.ingressSkips)
	}
}

func TestReconcileActiveProcessMembershipVisitsEachObservedTargetOnce(t *testing.T) {
	cgroups := &membershipCgroupManager{}
	manager, err := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.sharedCgroupPath = "/shared"
	manager.activeUsers[1001] = true
	manager.resourceLimits[1001] = userResourceLimitState{ramApplied: true}
	manager.resourceLimits[1002] = userResourceLimitState{standalone: true, ioApplied: true}

	if err := manager.reconcileActiveProcessMembership(config.DefaultConfig()); err != nil {
		t.Fatalf("reconcileActiveProcessMembership() error: %v", err)
	}
	want := []membershipCall{
		{uid: 1001, sharedPath: "/shared", normalQuota: "max 100000"},
		{uid: 1002, normalQuota: "max 100000"},
	}
	if !reflect.DeepEqual(cgroups.calls, want) {
		t.Fatalf("membership reconciliation calls = %+v, want %+v", cgroups.calls, want)
	}
}

func TestMaintainDecisionClassifiesMembershipFailureWithoutClearingActiveState(t *testing.T) {
	tests := []struct {
		name          string
		membershipErr error
		wantErrorType string
	}{
		{
			name:          "transient reconciliation failure",
			membershipErr: errors.New("cgroup.procs write rejected"),
			wantErrorType: processMembershipReconcileFailure,
		},
		{
			name:          "origin unavailable until operator action",
			membershipErr: &cgroup.ProcessOriginUnavailableError{PID: 9001, UID: 1000},
			wantErrorType: processMembershipOriginUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cgroups := &membershipCgroupManager{err: tt.membershipErr}
			exporter := &mockPrometheusExporter{}
			cfg := config.DefaultConfig()
			manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, exporter)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			manager.sharedCgroupPath = "/shared"
			manager.activeUsers[1000] = true
			manager.limitsActive = true
			manager.userLimitedAt[1000] = time.Now()
			snapshot := &SystemMetrics{
				TotalCores:       4,
				UserCPUUsage:     map[int]float64{1000: 50},
				CPUEligibleUsers: []int{1000},
				UserMetrics: map[int]*metrics.UserMetrics{
					1000: {
						UID: 1000, Username: "limited-user", EligibleForCPU: true,
						EnforceableUsage: metrics.ProcessSetMetrics{CPUUsage: 50, CPUUsageEMA: 50},
					},
				},
			}

			err = manager.executeDecision("MAINTAIN_CURRENT_STATE", snapshot)
			if !errors.Is(err, tt.membershipErr) || !strings.Contains(err.Error(), "reconcile active process membership") {
				t.Fatalf("maintain decision error = %v, want membership failure", err)
			}
			if !manager.activeUsers[1000] || !manager.limitsActive {
				t.Fatalf("active state changed after reconciliation failure: users=%v active=%t", manager.activeUsers, manager.limitsActive)
			}
			recorded := exporter.recordedErrors()
			want := prometheusErrorRecord{
				component: processMembershipErrorComponent,
				errorType: tt.wantErrorType,
			}
			if len(recorded) != 1 || recorded[0] != want {
				t.Fatalf("recorded errors = %+v, want one %+v signal", recorded, want)
			}
		})
	}
}

func TestReconcileUserResourceLimitsUsesIndependentEligibility(t *testing.T) {
	tests := []struct {
		name         string
		eligibility  config.UserEligibility
		wantRAMCalls int
		wantIOCalls  int
	}{
		{
			name:         "RAM eligibility does not require CPU eligibility",
			eligibility:  config.UserEligibility{EligibleForRAM: true},
			wantRAMCalls: 1,
		},
		{
			name:        "IO eligibility does not require CPU eligibility",
			eligibility: config.UserEligibility{EligibleForIO: true},
			wantIOCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.RAMEnabled = true
			cfg.RAMQuotaPerUser = "1G"
			cfg.IOEnabled = true
			cgroups := &deactivateCgroupManager{}
			manager, err := NewManager(
				cfg,
				&mockMetricsCollector{usernames: map[int]string{1000: "alice"}},
				cgroups,
				&mockPrometheusExporter{},
			)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			if err := manager.reconcileUserResourceLimits(1000, cfg, tt.eligibility); err != nil {
				t.Fatalf("reconcileUserResourceLimits() error: %v", err)
			}
			if got := len(cgroups.applyRAMLimitCalls); got != tt.wantRAMCalls {
				t.Fatalf("RAM apply calls = %d, want %d", got, tt.wantRAMCalls)
			}
			if got := len(cgroups.applyIOLimitCalls); got != tt.wantIOCalls {
				t.Fatalf("IO apply calls = %d, want %d", got, tt.wantIOCalls)
			}
		})
	}
}

func TestUserLimitStateSeparatesRequestedFromObservedEnforcement(t *testing.T) {
	t.Run("CPU move failure", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.UserIncludeList = []string{".*"}
		cfg.RAMEnabled = true
		cfg.RAMQuotaPerUser = "1G"
		manager, err := NewManager(
			cfg,
			&mockMetricsCollector{usernames: map[int]string{1000: "alice"}},
			&moveResultCgroupManager{moveErr: errors.New("move rejected")},
			&mockPrometheusExporter{},
		)
		if err != nil {
			t.Fatalf("NewManager() error: %v", err)
		}
		manager.sharedCgroupPath = filepath.Join(t.TempDir(), "shared")
		err = manager.activateLimits(&SystemMetrics{
			TotalCores:       4,
			UserCPUUsage:     map[int]float64{1000: 50},
			CPUEligibleUsers: []int{1000},
			UserMetrics: map[int]*metrics.UserMetrics{1000: {
				UID: 1000, Username: "alice", EligibleForCPU: true, EligibleForRAM: true,
			}},
		})
		if err == nil {
			t.Fatal("activateLimits() expected a move error")
		}
		state := manager.GetUserLimitState(1000, "alice")
		if !state.CPULimitRequested || state.CPULimitActive ||
			!state.RAMLimitRequested || state.RAMLimitActive {
			t.Fatalf("state after failed move = %+v, want CPU and RAM requested but inactive", state)
		}
	})

	t.Run("RAM application failure", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.RAMEnabled = true
		cfg.RAMQuotaPerUser = "1G"
		manager, err := NewManager(
			cfg,
			&mockMetricsCollector{usernames: map[int]string{1000: "alice"}},
			&flakyResourceCgroupManager{ramFailures: 1},
			&mockPrometheusExporter{},
		)
		if err != nil {
			t.Fatalf("NewManager() error: %v", err)
		}
		if err := manager.applyUserResourceLimits(1000, cfg, config.UserEligibility{EligibleForRAM: true}); err == nil {
			t.Fatal("applyUserResourceLimits() error = nil, want injected RAM failure")
		}
		state := manager.GetUserLimitState(1000, "alice")
		if !state.RAMLimitRequested || state.RAMLimitActive {
			t.Fatalf("RAM state = %+v, want requested=true active=false", state)
		}
	})
}

func TestMakeDecisionDeactivate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 75
	cfg.CPUReleaseThreshold = 40

	manager := &Manager{
		cfg:                cfg,
		limitsActive:       true,
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
	}

	metrics := &SystemMetrics{
		CPUEligibleCPUUsage: 30.0, // Below release threshold
		SystemUnderLoad:     false,
	}

	decision, reason := manager.makeDecision(metrics)

	if decision != "DEACTIVATE_LIMITS" {
		t.Errorf("makeDecision(): got %s, expected DEACTIVATE_LIMITS", decision)
	}
	if reason == "" {
		t.Error("makeDecision() should return a reason")
	}
}

func TestMakeDecisionMaintain(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 75
	cfg.CPUReleaseThreshold = 40

	manager := &Manager{
		cfg:                cfg,
		limitsActive:       false,
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
	}

	metrics := &SystemMetrics{
		CPUEligibleCPUUsage: 50.0, // Between thresholds
		SystemUnderLoad:     false,
	}

	decision, _ := manager.makeDecision(metrics)

	if decision != "MAINTAIN_CURRENT_STATE" {
		t.Errorf("makeDecision(): got %s, expected MAINTAIN_CURRENT_STATE", decision)
	}
}

func TestUserStabilityTrackerUsesWallClockDuration(t *testing.T) {
	tracker := newUserStabilityTracker()
	users := []int{1000}
	userMetrics := map[int]*metrics.UserMetrics{
		1000: {UID: 1000, CPUUsageEMA: 10, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: 10}},
	}
	now := time.Now()
	required := 90 * time.Second

	if tracker.AllBelowThreshold(users, userMetrics, 40, required, now) {
		t.Fatal("first below-threshold sample reported stable")
	}
	for i := 1; i <= 10; i++ {
		if tracker.AllBelowThreshold(users, userMetrics, 40, required, now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("rapid cycle %d shortened the wall-clock stability guard", i)
		}
	}
	if !tracker.AllBelowThreshold(users, userMetrics, 40, required, now.Add(required)) {
		t.Fatal("user was not stable after the required wall-clock duration")
	}
}

func TestUserStabilityTrackerResetsOnThresholdCrossing(t *testing.T) {
	tracker := newUserStabilityTracker()
	users := []int{1000}
	userMetrics := map[int]*metrics.UserMetrics{
		1000: {UID: 1000, CPUUsageEMA: 10, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: 10}},
	}
	now := time.Now()
	required := 30 * time.Second

	tracker.AllBelowThreshold(users, userMetrics, 40, required, now)
	userMetrics[1000].CPUUsageEMA = 50
	userMetrics[1000].EnforceableUsage.CPUUsageEMA = 50
	if tracker.AllBelowThreshold(users, userMetrics, 40, required, now.Add(required)) {
		t.Fatal("above-threshold sample reported stable")
	}
	userMetrics[1000].CPUUsageEMA = 10
	userMetrics[1000].EnforceableUsage.CPUUsageEMA = 10
	if tracker.AllBelowThreshold(users, userMetrics, 40, required, now.Add(2*required)) {
		t.Fatal("stability duration was not restarted after threshold crossing")
	}
}

func TestMakeDecisionReleaseStabilityUsesActiveUsersAndWallClock(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUReleaseThreshold = 40
	cfg.MinActiveTime = 0
	cfg.PollingInterval = 30
	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, CPUUsageEMA: 10},
			1001: {UID: 1001, CPUUsageEMA: 80},
		},
	}
	manager := &Manager{
		cfg:                cfg,
		limitsActive:       true,
		activeUsers:        map[int]bool{1000: true},
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
		stabilityTracker:   newUserStabilityTracker(),
		metricsCollector:   collector,
	}
	systemMetrics := &SystemMetrics{
		CPUEligibleCPUUsage: 30,
		SystemUnderLoad:     false,
		UserMetrics:         collector.allUserMetrics,
	}

	if decision, _ := manager.makeDecision(systemMetrics); decision != "MAINTAIN_CURRENT_STATE" {
		t.Fatalf("first release sample decision = %s, want MAINTAIN_CURRENT_STATE", decision)
	}
	manager.stabilityTracker.mu.Lock()
	manager.stabilityTracker.belowThresholdSince[1000] = time.Now().Add(-90 * time.Second)
	manager.stabilityTracker.mu.Unlock()
	if decision, _ := manager.makeDecision(systemMetrics); decision != "DEACTIVATE_LIMITS" {
		t.Fatalf("decision after wall-clock guard = %s, want DEACTIVATE_LIMITS", decision)
	}
	collector.callMu.Lock()
	observationCalls := collector.observationCalls
	decisionCalls := collector.decisionCalls
	collector.callMu.Unlock()
	if observationCalls != 0 || decisionCalls != 0 {
		t.Fatalf(
			"release stability re-read collector: observation=%d decision=%d, want both zero",
			observationCalls,
			decisionCalls,
		)
	}
}

func TestGetStatusSeparatesCPUAndAnyObservedEnforcement(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)
	manager.activeUsers[1002] = true
	manager.activeUsers[1000] = true
	manager.resourceLimits[1001] = userResourceLimitState{ramApplied: true}

	status := manager.GetStatus()
	if !status.CPULimitsActive || !status.ResourceLimitsActive || !status.AnyLimitsActive {
		t.Fatalf("GetStatus() active flags = %+v, want all true", status)
	}
	if !reflect.DeepEqual(status.CPUActivelyLimitedUsers, []int{1000, 1002}) {
		t.Errorf("CPUActivelyLimitedUsers = %v, want [1000 1002]", status.CPUActivelyLimitedUsers)
	}
	if !reflect.DeepEqual(status.ActivelyLimitedUsers, []int{1000, 1001, 1002}) {
		t.Errorf("ActivelyLimitedUsers = %v, want [1000 1001 1002]", status.ActivelyLimitedUsers)
	}
	if status.CPUActivelyLimitedUsersCount != 2 || status.ActivelyLimitedUsersCount != 3 {
		t.Errorf("limited counts = CPU %d, any %d; want 2, 3", status.CPUActivelyLimitedUsersCount, status.ActivelyLimitedUsersCount)
	}
}

func TestPrometheusSystemSnapshotKeepsEligibilityAndEnforcementResourcesDistinct(t *testing.T) {
	cfg := config.DefaultConfig()
	exporter := &mockPrometheusExporter{}
	manager, _ := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	manager.resourceLimits[1002] = userResourceLimitState{ramApplied: true}

	manager.updatePrometheusSystemMetrics(&SystemMetrics{
		TotalCores:               4,
		CPUEligibleUsersCount:    1,
		CPUEligibleCPUUsage:      10,
		CPUEligibleMemoryUsage:   100,
		RAMEligibleUsersCount:    2,
		RAMEligibleUsageBytes:    200,
		IOEligibleUsersCount:     3,
		IOEligibleReadBPS:        300,
		IOEligibleWriteBPS:       400,
		IOEligibleReadBlockIOPS:  5,
		IOEligibleWriteBlockIOPS: 6,
	})

	exporter.mu.Lock()
	snapshot := exporter.lastSystemSnapshot
	exporter.mu.Unlock()
	if snapshot.CPUEligibleUsersCount != 1 || snapshot.RAMEligibleUsersCount != 2 || snapshot.IOEligibleUsersCount != 3 {
		t.Fatalf("per-resource eligibility counts = CPU %d RAM %d IO %d; want 1, 2, 3", snapshot.CPUEligibleUsersCount, snapshot.RAMEligibleUsersCount, snapshot.IOEligibleUsersCount)
	}
	if snapshot.CPULimitsActive || !snapshot.ResourceLimitsActive || !snapshot.AnyLimitsActive {
		t.Fatalf("RAM-only enforcement flags = CPU %t resource %t any %t; want false, true, true", snapshot.CPULimitsActive, snapshot.ResourceLimitsActive, snapshot.AnyLimitsActive)
	}
	if snapshot.CPUActivelyLimitedUsersCount != 0 || snapshot.ActivelyLimitedUsersCount != 1 {
		t.Fatalf("RAM-only enforcement counts = CPU %d any %d; want 0, 1", snapshot.CPUActivelyLimitedUsersCount, snapshot.ActivelyLimitedUsersCount)
	}
}

func TestPrometheusUserSnapshotUsesTypedFieldProjection(t *testing.T) {
	exporter := &mockPrometheusExporter{}
	manager, _ := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	manager.updatePrometheusDecisionUserMetrics(&SystemMetrics{UserMetrics: map[int]*metrics.UserMetrics{
		1000: {
			UID: 1000, Username: "alice", CPUUsage: 11, CPUUsageAverage: 12,
			CPUUsageEMA: 13, MemoryUsage: 14, ProcessCount: 15, CPULimitActive: true,
			IOReadBytes: 16, IOWriteBytes: 17, IOReadOps: 18, IOWriteOps: 19,
		},
	}})

	exporter.mu.Lock()
	snapshot := exporter.lastUserSnapshot
	exporter.mu.Unlock()
	if snapshot.UID != 1000 || snapshot.Username != "alice" || snapshot.CPUUsagePercent != 11 || snapshot.CPUUsageAverage != 12 || snapshot.CPUUsageEMA != 13 {
		t.Fatalf("typed CPU projection = %+v", snapshot)
	}
	if snapshot.MemoryUsageBytes != 14 || snapshot.ProcessCount != 15 || !snapshot.CPULimitActive {
		t.Fatalf("typed state projection = %+v", snapshot)
	}
	if snapshot.ObservedIOReadBytes != 16 || snapshot.ObservedIOWriteBytes != 17 || snapshot.ObservedIOReadOps != 18 || snapshot.ObservedIOWriteOps != 19 {
		t.Fatalf("typed I/O projection = %+v", snapshot)
	}
}

func TestCollectSystemMetrics(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{systemLoad: 2.5}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	sysMetrics, err := manager.collectSystemMetrics()

	if err != nil {
		t.Fatalf("collectSystemMetrics() error: %v", err)
	}

	if sysMetrics.TotalCores != 4 {
		t.Errorf("collectSystemMetrics(): got %d cores, expected 4", sysMetrics.TotalCores)
	}
	if sysMetrics.SystemLoad != 2.5 {
		t.Errorf("collectSystemMetrics(): got load %f, expected 2.5", sysMetrics.SystemLoad)
	}
}

func TestIsUserLimited(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	// Initially no users should be limited
	if manager.isUserLimited(1000) {
		t.Error("isUserLimited() should return false initially")
	}

	// Add user to activeUsers
	manager.activeUsers[1000] = true

	if !manager.isUserLimited(1000) {
		t.Error("isUserLimited() should return true after adding user")
	}
}

func TestGetUsername(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	username := manager.getUsername(1000)

	// Should now return the username from the metrics collector (mock returns "user1000")
	if username != "user1000" {
		t.Errorf("getUsername(): got %s, expected user1000", username)
	}
}

func TestForceActivateLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	err := manager.ForceActivateLimits()
	if err != nil {
		t.Logf("ForceActivateLimits() error: %v", err)
	}
}

func TestForceDeactivateLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	err := manager.ForceDeactivateLimits()
	if err != nil {
		t.Logf("ForceDeactivateLimits() error: %v", err)
	}
}

type deactivateCgroupManager struct {
	mockCgroupManager
	applyCPULimitCalls       []int
	applyCPUQuotaCalls       []string
	applyCPUWeightCalls      []int
	applyRAMLimitCalls       []string
	applyIOLimitCalls        []int
	removeRAMHighCalls       []int
	removeRAMLimitCalls      []int
	removeRAMSwapLimitCalls  []int
	removeIOLimitCalls       []int
	applySharedCPULimitCalls []string
	createdUserSubgroups     []int
	movedSharedUsers         []int
	releasedUsers            []int
	releaseErrors            map[int]error
}

type blockingReleaseCgroupManager struct {
	deactivateCgroupManager
	releaseStarted chan struct{}
	releaseProceed chan struct{}
}

type flakyResourceCgroupManager struct {
	deactivateCgroupManager
	ramFailures int
	ioFailures  int
}

func (m *flakyResourceCgroupManager) ApplyRAMLimitWithHigh(uid int, maxLimit, highLimit string) error {
	m.applyRAMLimitCalls = append(m.applyRAMLimitCalls, fmt.Sprintf("%d:%s:%s", uid, maxLimit, highLimit))
	if m.ramFailures > 0 {
		m.ramFailures--
		return errors.New("RAM apply failed")
	}
	return nil
}

func (m *flakyResourceCgroupManager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	m.applyIOLimitCalls = append(m.applyIOLimitCalls, uid)
	if m.ioFailures > 0 {
		m.ioFailures--
		return errors.New("IO apply failed")
	}
	return nil
}

func (m *blockingReleaseCgroupManager) ReleaseUserFromSharedCgroup(uid int, path, normalQuota string) error {
	close(m.releaseStarted)
	<-m.releaseProceed
	return nil
}

type cleanupLockCheckingCgroupManager struct {
	mockCgroupManager
	checkLock func() error
}

type blockingCleanupCgroupManager struct {
	mockCgroupManager
	started chan struct{}
	release chan struct{}
}

func (m *blockingCleanupCgroupManager) CleanupAll() error {
	close(m.started)
	<-m.release
	return nil
}

func (m *cleanupLockCheckingCgroupManager) CleanupAll() error {
	return m.checkLock()
}

type cleanupBestEffortCgroupManager struct {
	deactivateCgroupManager
	cleanupCalled bool
	cleanupErr    error
}

func (m *cleanupBestEffortCgroupManager) CleanupAll() error {
	m.cleanupCalled = true
	return m.cleanupErr
}

type cleanupPrometheusExporter struct {
	mockPrometheusExporter
	stopCalled bool
	stopErr    error
}

func (m *cleanupPrometheusExporter) Stop() error {
	m.stopCalled = true
	return m.stopErr
}

type blockingCleanupPrometheusExporter struct {
	mockPrometheusExporter
	stopStarted chan struct{}
	stopRelease chan struct{}
}

func (m *blockingCleanupPrometheusExporter) Stop() error {
	close(m.stopStarted)
	<-m.stopRelease
	return nil
}

func (m *deactivateCgroupManager) ApplyCPULimit(uid int, quota string) error {
	m.applyCPULimitCalls = append(m.applyCPULimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) ApplyCPUQuota(uid int, quota string) error {
	m.applyCPUQuotaCalls = append(m.applyCPUQuotaCalls, fmt.Sprintf("%d:%s", uid, quota))
	return nil
}

func (m *deactivateCgroupManager) ApplyCPUWeight(uid int, weight int) error {
	m.applyCPUWeightCalls = append(m.applyCPUWeightCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) ApplyRAMLimitWithHigh(uid int, maxLimit, highLimit string) error {
	m.applyRAMLimitCalls = append(m.applyRAMLimitCalls, fmt.Sprintf("%d:%s:%s", uid, maxLimit, highLimit))
	return nil
}

func (m *deactivateCgroupManager) ApplyRAMLimitWithHighAndSwapDisabled(uid int, maxLimit, highLimit string) error {
	m.applyRAMLimitCalls = append(m.applyRAMLimitCalls, fmt.Sprintf("%d:%s:%s:swap-off", uid, maxLimit, highLimit))
	return nil
}

func (m *deactivateCgroupManager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	m.applyIOLimitCalls = append(m.applyIOLimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) RemoveRAMHigh(uid int) error {
	m.removeRAMHighCalls = append(m.removeRAMHighCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) RemoveRAMLimit(uid int) error {
	m.removeRAMLimitCalls = append(m.removeRAMLimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) RemoveRAMSwapLimit(uid int) error {
	m.removeRAMSwapLimitCalls = append(m.removeRAMSwapLimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) RemoveIOLimit(uid int) error {
	m.removeIOLimitCalls = append(m.removeIOLimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) ApplySharedCPULimit(path string, quota string) error {
	m.applySharedCPULimitCalls = append(m.applySharedCPULimitCalls, fmt.Sprintf("%s:%s", path, quota))
	return nil
}

func (m *deactivateCgroupManager) CreateUserSubCgroup(uid int, path string) (string, error) {
	m.createdUserSubgroups = append(m.createdUserSubgroups, uid)
	return filepath.Join(path, fmt.Sprintf("user_%d", uid)), nil
}

func (m *deactivateCgroupManager) MoveAllUserProcessesToSharedCgroup(uid int, path string) (cgroup.ProcessMoveResult, error) {
	m.movedSharedUsers = append(m.movedSharedUsers, uid)
	return cgroup.ProcessMoveResult{Moved: 1}, nil
}

func (m *deactivateCgroupManager) ReleaseUserFromSharedCgroup(uid int, path, normalQuota string) error {
	m.releasedUsers = append(m.releasedUsers, uid)
	return m.releaseErrors[uid]
}

func TestDeactivateLimitsReleasesSharedCgroups(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &deactivateCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, err := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("failed to create shared cgroup test path: %v", err)
	}

	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true
	manager.activeUsers[1001] = true
	manager.stabilityTracker.belowThresholdSince[999] = time.Now().Add(-time.Hour)
	manager.stabilityTracker.belowThresholdSince[1000] = time.Now().Add(-time.Hour)
	manager.psiBoostedAt[1000] = time.Now().Add(-time.Hour)
	manager.psiBoostedAt[1001] = time.Now().Add(-time.Hour)

	if err := manager.deactivateLimits(); err != nil {
		t.Fatalf("deactivateLimits() error: %v", err)
	}

	if len(cgroupManager.applyCPULimitCalls) != 0 {
		t.Fatalf("ApplyCPULimit should not be used for shared cgroup deactivation, got calls for %v", cgroupManager.applyCPULimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.applySharedCPULimitCalls, []string{sharedPath + ":max 100000"}) {
		t.Fatalf("ApplySharedCPULimit calls = %v", cgroupManager.applySharedCPULimitCalls)
	}

	sort.Ints(cgroupManager.releasedUsers)
	if !reflect.DeepEqual(cgroupManager.releasedUsers, []int{1000, 1001}) {
		t.Fatalf("released users = %v, want [1000 1001]", cgroupManager.releasedUsers)
	}
	if manager.limitsActive {
		t.Fatal("limitsActive should be false after all shared users are released")
	}
	if manager.sharedCgroupPath != "" {
		t.Fatalf("sharedCgroupPath = %q, want empty", manager.sharedCgroupPath)
	}
	if len(manager.activeUsers) != 0 {
		t.Fatalf("activeUsers = %v, want empty", manager.activeUsers)
	}
	if len(manager.psiBoostedAt) != 0 {
		t.Fatalf("psiBoostedAt = %v, want empty", manager.psiBoostedAt)
	}
	if len(manager.stabilityTracker.belowThresholdSince) != 0 {
		t.Fatalf("stability state = %v, want empty", manager.stabilityTracker.belowThresholdSince)
	}

	manager.revertPSIBoosts()
	if len(cgroupManager.applyCPUWeightCalls) != 0 {
		t.Fatalf("expired PSI boosts attempted after deactivation: %v", cgroupManager.applyCPUWeightCalls)
	}
}

func TestDeactivateLimitsKeepsFailedSharedUsersActive(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &deactivateCgroupManager{
		releaseErrors: map[int]error{
			1001: errors.New("restore failed"),
		},
	}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("failed to create shared cgroup test path: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true
	manager.activeUsers[1001] = true
	manager.stabilityTracker.belowThresholdSince[1000] = time.Now().Add(-time.Hour)
	manager.stabilityTracker.belowThresholdSince[1001] = time.Now().Add(-time.Hour)
	manager.psiBoostedAt[1000] = time.Now()
	manager.psiBoostedAt[1001] = time.Now()

	if err := manager.deactivateLimits(); err == nil {
		t.Fatal("deactivateLimits() should report the failed user release")
	}
	if manager.activeUsers[1000] {
		t.Fatal("successfully restored user 1000 should be removed from activeUsers")
	}
	if !manager.activeUsers[1001] {
		t.Fatal("failed user 1001 should remain in activeUsers")
	}
	if !manager.limitsActive {
		t.Fatal("limitsActive should remain true while a user still needs release")
	}
	if manager.sharedCgroupPath != sharedPath {
		t.Fatalf("sharedCgroupPath = %q, want %q", manager.sharedCgroupPath, sharedPath)
	}
	if _, exists := manager.psiBoostedAt[1000]; exists {
		t.Fatal("PSI boost state for released user 1000 should be removed")
	}
	if _, exists := manager.psiBoostedAt[1001]; !exists {
		t.Fatal("PSI boost state for failed user 1001 should be retained")
	}
	if _, exists := manager.stabilityTracker.belowThresholdSince[1000]; exists {
		t.Fatal("stability state for released user 1000 should be removed")
	}
	if _, exists := manager.stabilityTracker.belowThresholdSince[1001]; !exists {
		t.Fatal("stability state for failed user 1001 should be retained for immediate retry")
	}

	cgroupManager.applySharedCPULimitCalls = nil
	metrics := &SystemMetrics{
		TotalCores: 4,
		UserCPUUsage: map[int]float64{
			1001: 10,
		},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}
	wantQuota := sharedPath + ":300000 100000"
	if !reflect.DeepEqual(cgroupManager.applySharedCPULimitCalls, []string{wantQuota}) {
		t.Fatalf("shared quota reconciliation calls = %v, want [%s]", cgroupManager.applySharedCPULimitCalls, wantQuota)
	}
}

func TestDeactivateLimitsRemovesResourcesAppliedBeforeReload(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.DisableSwap = true
	cfg.IOEnabled = true

	collector := &mockMetricsCollector{usernames: map[int]string{1000: "user1000"}}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("MkdirAll(%s) error: %v", sharedPath, err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true
	if err := manager.applyUserResourceLimits(1000, cfg, config.UserEligibility{EligibleForRAM: true, EligibleForIO: true}); err != nil {
		t.Fatalf("applyUserResourceLimits() error: %v", err)
	}

	manager.UpdateConfig(config.DefaultConfig())
	if err := manager.deactivateLimits(); err != nil {
		t.Fatalf("deactivateLimits() error: %v", err)
	}

	if !reflect.DeepEqual(cgroupManager.removeRAMHighCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeRAMLimitCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeRAMSwapLimitCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeIOLimitCalls, []int{1000}) {
		t.Fatalf("tracked resource cleanup calls high=%v max=%v swap=%v io=%v, want [1000] each",
			cgroupManager.removeRAMHighCalls,
			cgroupManager.removeRAMLimitCalls,
			cgroupManager.removeRAMSwapLimitCalls,
			cgroupManager.removeIOLimitCalls,
		)
	}
}

func TestReleaseIdleUsersReappliesRAMAndIOLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.IOEnabled = true

	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	metrics := &SystemMetrics{
		TotalCores:   4,
		UserCPUUsage: map[int]float64{1000: 10},
		UserMetrics: map[int]*metrics.UserMetrics{1000: {
			UID: 1000, CPUUsageEMA: 10, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: 10}, EligibleForCPU: true, EligibleForRAM: true, EligibleForIO: true,
		}},
		CPUEligibleUsers: []int{1000},
	}

	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if !reflect.DeepEqual(cgroupManager.createdUserSubgroups, []int{1000}) {
		t.Fatalf("created subgroups = %v, want [1000]", cgroupManager.createdUserSubgroups)
	}
	if !reflect.DeepEqual(cgroupManager.movedSharedUsers, []int{1000}) {
		t.Fatalf("moved users = %v, want [1000]", cgroupManager.movedSharedUsers)
	}
	if !reflect.DeepEqual(cgroupManager.applyRAMLimitCalls, []string{"1000:1G:858993459"}) {
		t.Fatalf("RAM limit calls = %v", cgroupManager.applyRAMLimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.applyIOLimitCalls, []int{1000}) {
		t.Fatalf("IO limit calls = %v, want [1000]", cgroupManager.applyIOLimitCalls)
	}
	if !manager.activeUsers[1000] {
		t.Fatal("re-added user 1000 should be active")
	}
}

func TestReleaseIdleUsersReaddsUsersWithoutPerUserSettleDelay(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")

	const userCount = 10
	userCPUUsage := make(map[int]float64, userCount)
	userMetrics := make(map[int]*metrics.UserMetrics, userCount)
	eligibleUsers := make([]int, 0, userCount)
	for uid := 1000; uid < 1000+userCount; uid++ {
		userCPUUsage[uid] = 10
		userMetrics[uid] = &metrics.UserMetrics{UID: uid, CPUUsageEMA: 10, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: 10}}
		eligibleUsers = append(eligibleUsers, uid)
	}

	startedAt := time.Now()
	if err := manager.releaseIdleUsers(&SystemMetrics{
		TotalCores:       4,
		UserCPUUsage:     userCPUUsage,
		UserMetrics:      userMetrics,
		CPUEligibleUsers: eligibleUsers,
	}); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
		t.Fatalf("re-adding %d users took %s; per-user settle delay is still serialized", userCount, elapsed)
	}
	if len(cgroupManager.movedSharedUsers) != userCount {
		t.Fatalf("moved users = %d, want %d", len(cgroupManager.movedSharedUsers), userCount)
	}
}

func TestActivateLimitsMovesUsersWithoutPerUserSettleDelay(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")

	const userCount = 10
	userCPUUsage := make(map[int]float64, userCount)
	eligibleUsers := make([]int, 0, userCount)
	for uid := 1000; uid < 1000+userCount; uid++ {
		userCPUUsage[uid] = 10
		eligibleUsers = append(eligibleUsers, uid)
	}

	startedAt := time.Now()
	if err := manager.activateLimits(&SystemMetrics{
		TotalCores:       4,
		UserCPUUsage:     userCPUUsage,
		CPUEligibleUsers: eligibleUsers,
	}); err != nil {
		t.Fatalf("activateLimits() error: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
		t.Fatalf("activating %d users took %s; per-user settle delay is still serialized", userCount, elapsed)
	}
	if len(cgroupManager.movedSharedUsers) != userCount {
		t.Fatalf("moved users = %d, want %d", len(cgroupManager.movedSharedUsers), userCount)
	}
}

func TestReleaseIdleUsersReconcilesResourceLimitsAfterReload(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.DisableSwap = true
	cfg.IOEnabled = true

	collector := &mockMetricsCollector{usernames: map[int]string{1000: "user1000"}}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	manager.userLimitedAt[1000] = time.Now().Add(-time.Minute)
	if err := manager.applyUserResourceLimits(1000, cfg, config.UserEligibility{EligibleForRAM: true, EligibleForIO: true}); err != nil {
		t.Fatalf("applyUserResourceLimits() error: %v", err)
	}

	reloaded := config.DefaultConfig()
	manager.UpdateConfig(reloaded)
	metrics := &SystemMetrics{
		TotalCores:       4,
		UserCPUUsage:     map[int]float64{1000: 10},
		UserMetrics:      map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 10, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: 10}}},
		CPUEligibleUsers: []int{1000},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if !reflect.DeepEqual(cgroupManager.removeRAMHighCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeRAMLimitCalls, []int{1000}) {
		t.Fatalf("RAM removal calls high=%v max=%v, want [1000] for both",
			cgroupManager.removeRAMHighCalls, cgroupManager.removeRAMLimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.removeIOLimitCalls, []int{1000}) {
		t.Fatalf("IO removal calls = %v, want [1000]", cgroupManager.removeIOLimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.removeRAMSwapLimitCalls, []int{1000}) {
		t.Fatalf("RAM swap removal calls = %v, want [1000]", cgroupManager.removeRAMSwapLimitCalls)
	}
	if !manager.activeUsers[1000] {
		t.Fatal("resource-only reload removed the user from CPU limiting")
	}
	if _, exists := manager.resourceLimits[1000]; exists {
		t.Fatal("resource limit tracking remained after successful reconciliation")
	}
}

func TestReleaseIdleUsersRetriesPartialResourceLimitApplication(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.IOEnabled = true

	collector := &mockMetricsCollector{usernames: map[int]string{1000: "user1000"}}
	cgroupManager := &flakyResourceCgroupManager{ramFailures: 1, ioFailures: 1}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	manager.userLimitedAt[1000] = time.Now()
	if err := manager.applyUserResourceLimits(1000, cfg, config.UserEligibility{EligibleForRAM: true, EligibleForIO: true}); err == nil {
		t.Fatal("applyUserResourceLimits() error = nil, want injected RAM and IO failures")
	}

	initialState := manager.resourceLimits[1000]
	if !initialState.ram || initialState.ramApplied || !initialState.io || initialState.ioApplied {
		t.Fatalf("partial resource state = %+v, want tracked but not fully applied", initialState)
	}

	systemMetrics := &SystemMetrics{
		TotalCores:   4,
		UserCPUUsage: map[int]float64{1000: 10},
		UserMetrics: map[int]*metrics.UserMetrics{1000: {
			UID: 1000, CPUUsageEMA: 10, EligibleForCPU: true, EligibleForRAM: true, EligibleForIO: true,
		}},
		CPUEligibleUsers: []int{1000},
	}
	if err := manager.releaseIdleUsers(systemMetrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if len(cgroupManager.applyRAMLimitCalls) != 2 || len(cgroupManager.applyIOLimitCalls) != 2 {
		t.Fatalf("resource apply attempts RAM=%d IO=%d, want two each",
			len(cgroupManager.applyRAMLimitCalls), len(cgroupManager.applyIOLimitCalls))
	}
	finalState := manager.resourceLimits[1000]
	if !finalState.ramApplied || !finalState.ioApplied {
		t.Fatalf("resource state after retry = %+v, want fully applied", finalState)
	}
}

func TestReleaseIdleUsersReleasesNewlyIneligibleUser(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.IOEnabled = true

	collector := &mockMetricsCollector{usernames: map[int]string{1000: "user1000"}}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	manager.userLimitedAt[1000] = time.Now()
	if err := manager.applyUserResourceLimits(1000, cfg, config.UserEligibility{EligibleForRAM: true, EligibleForIO: true}); err != nil {
		t.Fatalf("applyUserResourceLimits() error: %v", err)
	}

	metrics := &SystemMetrics{
		TotalCores:   4,
		UserCPUUsage: map[int]float64{1000: 10},
		UserMetrics:  map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 10}},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if manager.activeUsers[1000] {
		t.Fatal("newly ineligible user remained in activeUsers")
	}
	if !reflect.DeepEqual(cgroupManager.releasedUsers, []int{1000}) {
		t.Fatalf("released users = %v, want [1000]", cgroupManager.releasedUsers)
	}
	if !reflect.DeepEqual(cgroupManager.removeRAMHighCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeRAMLimitCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeIOLimitCalls, []int{1000}) {
		t.Fatalf("tracked resource limits were not removed: high=%v max=%v io=%v",
			cgroupManager.removeRAMHighCalls,
			cgroupManager.removeRAMLimitCalls,
			cgroupManager.removeIOLimitCalls,
		)
	}
}

func TestReleaseIdleUsersUsesEMAAndPerUserHoldTime(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MinActiveTime = 60

	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	manager.userLimitedAt[1000] = time.Now().Add(-2 * time.Minute)

	metrics := &SystemMetrics{
		TotalCores:       4,
		UserCPUUsage:     map[int]float64{1000: 0},
		UserMetrics:      map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 5, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: 5}}},
		CPUEligibleUsers: []int{1000},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers(high EMA) error: %v", err)
	}
	if !manager.activeUsers[1000] {
		t.Fatal("instantaneous idle sample released a user whose EMA was active")
	}

	metrics.UserMetrics[1000].CPUUsageEMA = 0
	metrics.UserMetrics[1000].EnforceableUsage.CPUUsageEMA = 0
	manager.userLimitedAt[1000] = time.Now()
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers(hold time) error: %v", err)
	}
	if !manager.activeUsers[1000] {
		t.Fatal("user was released before its per-user minimum active time")
	}

	manager.userLimitedAt[1000] = time.Now().Add(-2 * time.Minute)
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers(expired hold) error: %v", err)
	}
	if manager.activeUsers[1000] {
		t.Fatal("idle user remained limited after EMA and hold-time conditions were met")
	}
}

func TestReleaseIdleUsersReAddDoesNotResetGlobalActivationTime(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	globalActivation := time.Now().Add(-10 * time.Minute)
	manager.limitsActive = true
	manager.limitsAppliedTime = globalActivation
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")

	metrics := &SystemMetrics{
		TotalCores:       4,
		UserCPUUsage:     map[int]float64{1000: 10},
		UserMetrics:      map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 10, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: 10}}},
		CPUEligibleUsers: []int{1000},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if !manager.activeUsers[1000] {
		t.Fatal("active eligible user was not re-added")
	}
	if !manager.limitsAppliedTime.Equal(globalActivation) {
		t.Fatalf("global limitsAppliedTime changed from %s to %s",
			globalActivation, manager.limitsAppliedTime)
	}
	if manager.userLimitedAt[1000].Before(globalActivation) {
		t.Fatalf("per-user activation time = %s, want re-add time", manager.userLimitedAt[1000])
	}
}

func TestReleaseIdleUsersForgetsIORemediationOnlyAfterSuccessfulRelease(t *testing.T) {
	tests := []struct {
		name       string
		releaseErr error
		wantState  bool
		wantActive bool
	}{
		{
			name:      "successful release",
			wantState: false,
		},
		{
			name:       "failed release",
			releaseErr: errors.New("restore failed"),
			wantState:  true,
			wantActive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cgroupManager := &deactivateCgroupManager{
				releaseErrors: map[int]error{1000: tt.releaseErr},
			}
			manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}

			manager.limitsActive = true
			manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
			manager.activeUsers[1000] = true
			manager.psiBoostedAt[1000] = time.Now()
			manager.ioRemediation.boostStates[1000] = &IOBoostState{IsActive: true}

			metrics := &SystemMetrics{
				TotalCores:   4,
				UserCPUUsage: map[int]float64{1000: 0},
			}
			if err := manager.releaseIdleUsers(metrics); err != nil {
				t.Fatalf("releaseIdleUsers() error: %v", err)
			}

			manager.ioRemediation.mu.RLock()
			_, stateExists := manager.ioRemediation.boostStates[1000]
			manager.ioRemediation.mu.RUnlock()
			if stateExists != tt.wantState {
				t.Fatalf("IO remediation state exists = %t, want %t", stateExists, tt.wantState)
			}
			manager.mu.RLock()
			active := manager.activeUsers[1000]
			manager.mu.RUnlock()
			if active != tt.wantActive {
				t.Fatalf("activeUsers[1000] = %t, want %t", active, tt.wantActive)
			}
			manager.mu.RLock()
			_, psiStateExists := manager.psiBoostedAt[1000]
			manager.mu.RUnlock()
			if psiStateExists != tt.wantActive {
				t.Fatalf("PSI boost state exists = %t, want %t", psiStateExists, tt.wantActive)
			}
		})
	}
}

func TestReleaseIdleUsersWaitsBeforeCommittingState(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &blockingReleaseCgroupManager{
		releaseStarted: make(chan struct{}),
		releaseProceed: make(chan struct{}),
	}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	metrics := &SystemMetrics{
		TotalCores:   4,
		UserCPUUsage: map[int]float64{1000: 0},
	}

	releaseDone := make(chan error, 1)
	go func() {
		releaseDone <- manager.releaseIdleUsers(metrics)
	}()
	<-cgroupManager.releaseStarted

	manager.mu.RLock()
	activeWhilePending := manager.activeUsers[1000]
	manager.mu.RUnlock()
	if !activeWhilePending {
		t.Fatal("user was removed from activeUsers before cgroup release completed")
	}
	select {
	case err := <-releaseDone:
		t.Fatalf("releaseIdleUsers() returned before cgroup release completed: %v", err)
	default:
	}

	close(cgroupManager.releaseProceed)
	if err := <-releaseDone; err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	manager.mu.RLock()
	activeAfterRelease := manager.activeUsers[1000]
	manager.mu.RUnlock()
	if activeAfterRelease {
		t.Fatal("successfully released user remained in activeUsers")
	}
}

func TestCleanupSerializesCgroupOperations(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &cleanupLockCheckingCgroupManager{}
	cgroupManager.checkLock = func() error { return nil }
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	leaveOperation := manager.opGate.Enter()
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- manager.Cleanup() }()
	select {
	case err := <-cleanupDone:
		t.Fatalf("Cleanup() bypassed the operation gate: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	leaveOperation()
	if err := <-cleanupDone; err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}
}

func TestStateSnapshotRemainsAvailableWhileCgroupOperationBlocks(t *testing.T) {
	cgroupManager := &blockingCleanupCgroupManager{started: make(chan struct{}), release: make(chan struct{})}
	manager, err := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- manager.Cleanup() }()
	<-cgroupManager.started

	statusDone := make(chan RuntimeStatus, 1)
	go func() { statusDone <- manager.GetStatus() }()
	select {
	case <-statusDone:
	case <-time.After(time.Second):
		close(cgroupManager.release)
		<-cleanupDone
		t.Fatal("GetStatus() blocked behind cgroup I/O")
	}
	close(cgroupManager.release)
	if err := <-cleanupDone; err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}
}

func TestCleanupAttemptsEveryPhaseAfterErrors(t *testing.T) {
	releaseErr := errors.New("restore failed")
	cgroupCleanupErr := errors.New("cgroup cleanup failed")
	exporterStopErr := errors.New("exporter stop failed")
	cgroupManager := &cleanupBestEffortCgroupManager{
		deactivateCgroupManager: deactivateCgroupManager{
			releaseErrors: map[int]error{1000: releaseErr},
		},
		cleanupErr: cgroupCleanupErr,
	}
	exporter := &cleanupPrometheusExporter{stopErr: exporterStopErr}
	manager, err := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, cgroupManager, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("failed to create shared cgroup test path: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true

	cleanupErr := manager.Cleanup()
	if !errors.Is(cleanupErr, releaseErr) ||
		!errors.Is(cleanupErr, cgroupCleanupErr) ||
		!errors.Is(cleanupErr, exporterStopErr) {
		t.Fatalf("Cleanup() error = %v, want all phase errors", cleanupErr)
	}
	if !cgroupManager.cleanupCalled {
		t.Fatal("CleanupAll() was skipped after deactivation failed")
	}
	if !exporter.stopCalled {
		t.Fatal("Prometheus exporter Stop() was skipped after earlier failures")
	}
}

func TestCleanupKeepsStateAvailableWhilePrometheusStops(t *testing.T) {
	exporter := &blockingCleanupPrometheusExporter{
		stopStarted: make(chan struct{}),
		stopRelease: make(chan struct{}),
	}
	manager, err := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- manager.Cleanup() }()
	<-exporter.stopStarted
	configDone := make(chan *config.Config, 1)
	go func() { configDone <- manager.GetConfig() }()
	select {
	case got := <-configDone:
		if got == nil {
			t.Fatal("GetConfig() returned nil while Prometheus Stop() was blocked")
		}
	case <-time.After(time.Second):
		close(exporter.stopRelease)
		<-cleanupDone
		t.Fatal("GetConfig() blocked behind Prometheus Stop()")
	}
	close(exporter.stopRelease)
	if err := <-cleanupDone; err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}
}

func TestPatternDetectionFiltersUsersAndKeepsSharedProcessesInPlace(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternMinSamples = 1
	cfg.PatternConfidenceThreshold = 0.7
	cfg.UserIncludeList = []string{"^allowed$"}

	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "allowed", CPUUsage: 0, EligibleForCPU: true},
			1001: {UID: 1001, Username: "excluded", CPUUsage: 0},
		},
		usernames: map[int]string{1000: "allowed", 1001: "excluded"},
	}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.patternDetector.userStats[1000] = batchNightStats()
	manager.patternDetector.userStats[1001] = batchNightStats()

	run := &controlCycleContext{
		cfg:     cfg,
		metrics: &SystemMetrics{UserMetrics: collector.GetAllUserMetricsForDecision()},
	}
	if err := manager.stageWorkloadPatternDetection(run); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.policyEngine.GetPolicy(1000); !exists {
		t.Fatal("eligible user 1000 did not receive a pattern policy")
	}
	if _, exists := manager.policyEngine.GetPolicy(1001); exists {
		t.Fatal("excluded user 1001 received a pattern policy")
	}
	if _, exists := manager.patternDetector.userStats[1001]; exists {
		t.Fatal("excluded user 1001 remained in pattern statistics")
	}
	if len(cgroupManager.applyCPULimitCalls) != 0 {
		t.Fatalf("pattern policy migrated processes through ApplyCPULimit: %v", cgroupManager.applyCPULimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUQuotaCalls, []string{"1000:200000 100000"}) {
		t.Fatalf("CPU quota calls = %v, want [1000:200000 100000]", cgroupManager.applyCPUQuotaCalls)
	}
}

func TestPatternHistorySurvivesTemporaryProcessAbsence(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternMinSamples = 1
	cfg.UserIncludeList = []string{"^batch$"}

	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{},
		usernames:      map[int]string{1000: "batch"},
	}
	manager, err := NewManager(cfg, collector, &deactivateCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	stats := batchNightStats()
	manager.patternDetector.userStats[1000] = stats
	manager.policyEngine.ApplyPolicy(1000, PatternBatchNight, cfg)

	run := &controlCycleContext{
		cfg:     cfg,
		metrics: &SystemMetrics{UserMetrics: collector.GetAllUserMetricsForDecision()},
	}
	if err := manager.stageWorkloadPatternDetection(run); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	retained, exists := manager.patternDetector.userStats[1000]
	if !exists {
		t.Fatal("pattern history was removed while the configured user had no live processes")
	}
	if len(retained.Buckets) != 1 || retained.Buckets[0].SampleCount != 30 {
		t.Fatalf("retained buckets = %+v, want one bucket with 30 samples", retained.Buckets)
	}
	if _, exists := manager.policyEngine.GetPolicy(1000); !exists {
		t.Fatal("pattern policy was removed while retained history was still valid")
	}
}

func TestPatternHistoryExpiryRemovesPolicy(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternHistoryHours = 1
	cfg.UserIncludeList = []string{"^batch$"}

	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{},
		usernames:      map[int]string{1000: "batch"},
	}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	stats := batchNightStats()
	stats.Buckets[0].Hour = patternHourStart(time.Now().Add(-2 * time.Hour))
	manager.patternDetector.userStats[1000] = stats
	manager.policyEngine.ApplyPolicy(1000, PatternBatchNight, cfg)
	manager.activeUsers[1000] = true

	run := &controlCycleContext{
		cfg:     cfg,
		metrics: &SystemMetrics{UserMetrics: collector.GetAllUserMetricsForDecision()},
	}
	if err := manager.stageWorkloadPatternDetection(run); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.patternDetector.userStats[1000]; exists {
		t.Fatal("expired pattern history was retained")
	}
	if _, exists := manager.policyEngine.GetPolicy(1000); exists {
		t.Fatal("policy survived after its pattern history expired")
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUQuotaCalls, []string{"1000:max 100000"}) {
		t.Fatalf("CPU quota calls = %v, want [1000:max 100000]", cgroupManager.applyCPUQuotaCalls)
	}
}

func TestPatternPolicyIsRevertedWhenClassificationDecays(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternMinSamples = 1

	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "user1000", EligibleForCPU: true},
		},
	}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.policyEngine.ApplyPolicy(1000, PatternBatchNight, cfg)
	noon := mostRecentHour(12)
	manager.patternDetector.userStats[1000] = &UserHourlyStats{Buckets: []hourlyPatternBucket{{
		Hour:        noon,
		CPUSum:      0,
		SampleCount: 30,
	}}}

	run := &controlCycleContext{
		cfg:     cfg,
		metrics: &SystemMetrics{UserMetrics: collector.GetAllUserMetricsForDecision()},
	}
	if err := manager.stageWorkloadPatternDetection(run); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.policyEngine.GetPolicy(1000); exists {
		t.Fatal("unknown classification did not remove the existing policy")
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUQuotaCalls, []string{"1000:max 100000"}) {
		t.Fatalf("CPU quota calls = %v, want [1000:max 100000]", cgroupManager.applyCPUQuotaCalls)
	}
}

func TestPatternPolicyIsRevertedWhenAutodetectIsDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = false

	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.policyEngine.ApplyPolicy(1000, PatternBatchNight, cfg)

	if err := manager.stageWorkloadPatternDetection(&controlCycleContext{cfg: cfg}); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.policyEngine.GetPolicy(1000); exists {
		t.Fatal("disabled autodetect did not clear the existing policy")
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUQuotaCalls, []string{"1000:max 100000"}) {
		t.Fatalf("CPU quota calls = %v, want [1000:max 100000]", cgroupManager.applyCPUQuotaCalls)
	}
}

func TestIORemediationUsesOnlyActiveIOEligibleUsers(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IOEnabled = true
	cfg.IORemediationEnabled = true
	cfg.IOStarvationCheckInterval = 0
	cfg.IOUserIncludeList = []string{"^allowed$"}

	collector := &mockMetricsCollector{
		usernames: map[int]string{
			1000: "allowed",
			1001: "excluded",
		},
	}
	manager, err := NewManager(cfg, collector, &deactivateCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.activeUsers[1001] = true

	if err := manager.stageIORemediation(&controlCycleContext{cfg: cfg}); err != nil {
		t.Fatalf("stageIORemediation() error: %v", err)
	}

	if _, exists := manager.ioRemediation.boostStates[1000]; !exists {
		t.Fatal("active IO-eligible user was not checked")
	}
	if _, exists := manager.ioRemediation.boostStates[1001]; exists {
		t.Fatal("IO-excluded user was checked for remediation")
	}
}

func TestIORemediationFailuresDegradeCycleWithoutSkippingUsersOrTail(t *testing.T) {
	tests := []struct {
		name       string
		failedUIDs []int
	}{
		{name: "partial failure", failedUIDs: []int{1000}},
		{name: "all users fail", failedUIDs: []int{1000, 1001}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.IOEnabled = true
			cfg.IORemediationEnabled = true
			cfg.IOStarvationCheckInterval = 0
			cfg.IOStarvationThreshold = 0
			cfg.IOPSIThreshold = 1

			cgroups := &remediationStageCgroupManager{temporaryErrors: make(map[int][]error)}
			for _, uid := range tt.failedUIDs {
				cgroups.temporaryErrors[uid] = []error{fmt.Errorf("injected boost failure for UID %d", uid)}
			}
			exporter := &mockPrometheusExporter{}
			manager, err := NewManager(cfg, &mockMetricsCollector{usernames: map[int]string{
				1000: "alice",
				1001: "bob",
			}}, cgroups, exporter)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			manager.activeUsers[1000] = true
			manager.activeUsers[1001] = true

			tailRan := false
			run := &controlCycleContext{cfg: cfg}
			stages := []controlCycleStage{
				{name: "io_remediation", run: (*Manager).stageIORemediation, continueAfterError: true},
				{name: "protective_tail", run: func(_ *Manager, _ *controlCycleContext) error {
					tailRan = true
					return nil
				}},
			}
			err = runControlCyclePipeline(manager, run, stages)
			if err == nil {
				t.Fatal("runControlCyclePipeline() error = nil, want degraded remediation failure")
			}
			if !tailRan {
				t.Fatal("remediation failure skipped the protective tail")
			}
			if !reflect.DeepEqual(cgroups.temporaryCalls, []int{1000, 1001}) {
				t.Fatalf("temporary IO calls = %v, want both users in deterministic order", cgroups.temporaryCalls)
			}
			if len(run.deferredErrors) != 1 {
				t.Fatalf("deferred errors = %d, want one stage error", len(run.deferredErrors))
			}
			for _, uid := range tt.failedUIDs {
				fragment := fmt.Sprintf("injected boost failure for UID %d", uid)
				if strings.Count(err.Error(), fragment) != 1 {
					t.Errorf("cycle error reports %q %d times, want once: %v", fragment, strings.Count(err.Error(), fragment), err)
				}
			}
			gotMetrics := exporter.recordedErrors()
			if len(gotMetrics) != len(tt.failedUIDs) {
				t.Fatalf("Prometheus errors = %+v, want %d", gotMetrics, len(tt.failedUIDs))
			}
			for _, got := range gotMetrics {
				if got != (prometheusErrorRecord{component: ioRemediationErrorComponent, errorType: ioRemediationBoostApplyFailure}) {
					t.Errorf("Prometheus error = %+v, want bounded IO remediation apply failure", got)
				}
			}
		})
	}
}

func TestIORemediationFailedBoostRetriesNextCycle(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IOEnabled = true
	cfg.IORemediationEnabled = true
	cfg.IOStarvationCheckInterval = 0
	cfg.IOStarvationThreshold = 0
	cfg.IOPSIThreshold = 1

	cgroups := &remediationStageCgroupManager{temporaryErrors: map[int][]error{
		1000: {errors.New("first boost rejected")},
	}}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{usernames: map[int]string{1000: "alice"}}, cgroups, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true

	if err := manager.stageIORemediation(&controlCycleContext{cfg: cfg}); err == nil {
		t.Fatal("first stageIORemediation() error = nil, want injected failure")
	}
	if err := manager.stageIORemediation(&controlCycleContext{cfg: cfg}); err != nil {
		t.Fatalf("second stageIORemediation() error: %v", err)
	}
	if !reflect.DeepEqual(cgroups.temporaryCalls, []int{1000, 1000}) {
		t.Fatalf("temporary IO calls = %v, want failed attempt and next-cycle retry", cgroups.temporaryCalls)
	}
	if state := manager.ioRemediation.boostStates[1000]; state == nil || !state.IsActive {
		t.Fatalf("remediation state after retry = %+v, want active boost", state)
	}
	if got := exporter.recordedErrors(); !reflect.DeepEqual(got, []prometheusErrorRecord{{
		component: ioRemediationErrorComponent,
		errorType: ioRemediationBoostApplyFailure,
	}}) {
		t.Fatalf("Prometheus errors = %+v, want only the failed attempt", got)
	}
}

func TestPatternPolicyFailuresDegradeCycleWithoutSkippingUsersOrTail(t *testing.T) {
	tests := []struct {
		name       string
		failedUIDs []int
	}{
		{name: "partial failure", failedUIDs: []int{1000}},
		{name: "all users fail", failedUIDs: []int{1000, 1001}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.AutodetectPatterns = true
			cfg.PatternMinSamples = 1
			cfg.PatternConfidenceThreshold = 0.7
			cfg.UserIncludeList = []string{".*"}
			cfg.RAMEnabled = true

			cgroups := &patternPolicyCgroupManager{ramErrors: make(map[int][]error)}
			for _, uid := range tt.failedUIDs {
				cgroups.ramErrors[uid] = []error{fmt.Errorf("injected RAM failure for UID %d", uid)}
			}
			collector := &mockMetricsCollector{
				allUserMetrics: map[int]*metrics.UserMetrics{
					1000: {UID: 1000, Username: "alice", EligibleForCPU: true},
					1001: {UID: 1001, Username: "bob", EligibleForCPU: true},
				},
				usernames: map[int]string{1000: "alice", 1001: "bob"},
			}
			exporter := &mockPrometheusExporter{}
			manager, err := NewManager(cfg, collector, cgroups, exporter)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			manager.activeUsers[1000] = true
			manager.activeUsers[1001] = true
			manager.patternDetector.userStats[1000] = batchNightStats()
			manager.patternDetector.userStats[1001] = batchNightStats()

			tailRan := false
			run := &controlCycleContext{
				cfg:     cfg,
				metrics: &SystemMetrics{UserMetrics: collector.GetAllUserMetricsForDecision()},
			}
			stages := []controlCycleStage{
				{name: "workload_pattern_detection", run: (*Manager).stageWorkloadPatternDetection, continueAfterError: true},
				{name: "protective_tail", run: func(_ *Manager, _ *controlCycleContext) error {
					tailRan = true
					return nil
				}},
			}
			err = runControlCyclePipeline(manager, run, stages)
			if err == nil {
				t.Fatal("runControlCyclePipeline() error = nil, want degraded pattern enforcement failure")
			}
			if !tailRan {
				t.Fatal("pattern enforcement failure skipped the protective tail")
			}
			if cgroups.ramCalls[1000] != 1 || cgroups.ramCalls[1001] != 1 {
				t.Fatalf("RAM calls = %v, want every user attempted once", cgroups.ramCalls)
			}
			for _, uid := range tt.failedUIDs {
				fragment := fmt.Sprintf("injected RAM failure for UID %d", uid)
				if strings.Count(err.Error(), fragment) != 1 {
					t.Errorf("cycle error reports %q %d times, want once: %v", fragment, strings.Count(err.Error(), fragment), err)
				}
				if _, pending := manager.pendingPatternReconciliations[uid]; !pending {
					t.Errorf("failed UID %d is not pending retry", uid)
				}
			}
			gotMetrics := exporter.recordedErrors()
			if len(gotMetrics) != len(tt.failedUIDs) {
				t.Fatalf("Prometheus errors = %+v, want %d", gotMetrics, len(tt.failedUIDs))
			}
			for _, got := range gotMetrics {
				if got != (prometheusErrorRecord{component: patternPolicyErrorComponent, errorType: patternPolicyApplicationFailure}) {
					t.Errorf("Prometheus error = %+v, want bounded pattern-policy application failure", got)
				}
			}
		})
	}
}

func TestPatternPolicyFailedEnforcementRetriesNextCycle(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternMinSamples = 1
	cfg.PatternConfidenceThreshold = 0.7
	cfg.UserIncludeList = []string{".*"}
	cfg.RAMEnabled = true

	cgroups := &patternPolicyCgroupManager{ramErrors: map[int][]error{
		1000: {errors.New("first RAM application rejected")},
	}}
	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "alice", EligibleForCPU: true},
		},
		usernames: map[int]string{1000: "alice"},
	}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, collector, cgroups, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.patternDetector.userStats[1000] = batchNightStats()
	run := &controlCycleContext{
		cfg:     cfg,
		metrics: &SystemMetrics{UserMetrics: collector.GetAllUserMetricsForDecision()},
	}

	if err := manager.stageWorkloadPatternDetection(run); err == nil {
		t.Fatal("first stageWorkloadPatternDetection() error = nil, want injected failure")
	}
	if err := manager.stageWorkloadPatternDetection(run); err != nil {
		t.Fatalf("second stageWorkloadPatternDetection() error: %v", err)
	}
	if cgroups.ramCalls[1000] != 2 {
		t.Fatalf("RAM calls for UID 1000 = %d, want failed attempt and next-cycle retry", cgroups.ramCalls[1000])
	}
	if _, pending := manager.pendingPatternReconciliations[1000]; pending {
		t.Fatal("successful retry left UID 1000 pending")
	}
	if state := manager.resourceLimits[1000]; !state.ramApplied {
		t.Fatalf("resource state after retry = %+v, want RAM applied", state)
	}
	if got := exporter.recordedErrors(); !reflect.DeepEqual(got, []prometheusErrorRecord{{
		component: patternPolicyErrorComponent,
		errorType: patternPolicyApplicationFailure,
	}}) {
		t.Fatalf("Prometheus errors = %+v, want only the failed attempt", got)
	}
}

func TestPatternPolicyCPUQuotaFailureIsPartOfTheCycleOutcome(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternMinSamples = 1
	cfg.PatternConfidenceThreshold = 0.7
	cfg.UserIncludeList = []string{".*"}
	cpuErr := errors.New("CPU quota rejected")
	cgroups := &patternPolicyCgroupManager{cpuErrors: map[int][]error{1000: {cpuErr}}}
	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "alice", EligibleForCPU: true},
		},
		usernames: map[int]string{1000: "alice"},
	}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, collector, cgroups, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.patternDetector.userStats[1000] = batchNightStats()
	run := &controlCycleContext{
		cfg:     cfg,
		metrics: &SystemMetrics{UserMetrics: collector.GetAllUserMetricsForDecision()},
	}

	err = manager.stageWorkloadPatternDetection(run)
	if !errors.Is(err, cpuErr) || !strings.Contains(err.Error(), "CPU quota") || !strings.Contains(err.Error(), "UID 1000") {
		t.Fatalf("stageWorkloadPatternDetection() error = %v, want UID and CPU quota context", err)
	}
	var patternErr *patternPolicyError
	if !errors.As(err, &patternErr) || patternErr.uid != 1000 {
		t.Fatalf("stageWorkloadPatternDetection() error = %v, want typed UID 1000 pattern failure", err)
	}
	if got := exporter.recordedErrors(); !reflect.DeepEqual(got, []prometheusErrorRecord{{
		component: patternPolicyErrorComponent,
		errorType: patternPolicyApplicationFailure,
	}}) {
		t.Fatalf("Prometheus errors = %+v, want one bounded pattern-policy failure", got)
	}
}

func batchNightStats() *UserHourlyStats {
	return &UserHourlyStats{Buckets: []hourlyPatternBucket{{
		Hour:        mostRecentHour(23),
		CPUSum:      3000,
		SampleCount: 30,
	}}}
}

func mostRecentHour(hour int) time.Time {
	now := time.Now()
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if candidate.After(now) {
		candidate = candidate.Add(-24 * time.Hour)
	}
	return candidate
}

func TestBlackoutDeactivatesLimitsAndResetsBoosts(t *testing.T) {
	cfg := config.DefaultConfig()
	timeframes, err := config.ParseTimeframe("* 00-24")
	if err != nil {
		t.Fatalf("ParseTimeframe() error: %v", err)
	}
	cfg.BlackoutSpec = "* 00-24"
	cfg.BlackoutTimeframes = timeframes

	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("failed to create shared cgroup test path: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true
	manager.psiBoostedAt[1000] = time.Now()
	manager.ioRemediation.boostStates[1000] = &IOBoostState{
		IsActive:        true,
		StartTime:       time.Now(),
		StarvationStart: time.Now(),
	}

	run := &controlCycleContext{
		cfg:     cfg,
		cycleID: 123,
		trigger: ControlCycleTriggerTicker,
	}
	if err := manager.stageCheckBlackout(run); err != nil {
		t.Fatalf("stageCheckBlackout() error: %v", err)
	}

	if !run.stopWithoutError {
		t.Fatal("blackout did not suspend the control cycle")
	}
	if manager.limitsActive || len(manager.activeUsers) != 0 {
		t.Fatalf("limits remained active during blackout: active=%v users=%v", manager.limitsActive, manager.activeUsers)
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUWeightCalls, []int{1000}) {
		t.Fatalf("ApplyCPUWeight calls = %v, want [1000]", cgroupManager.applyCPUWeightCalls)
	}
	if !reflect.DeepEqual(cgroupManager.releasedUsers, []int{1000}) {
		t.Fatalf("released users = %v, want [1000]", cgroupManager.releasedUsers)
	}
	if len(manager.psiBoostedAt) != 0 {
		t.Fatalf("PSI boost state remained after blackout: %v", manager.psiBoostedAt)
	}
	if _, exists := manager.ioRemediation.boostStates[1000]; exists {
		t.Fatal("IO boost state remained after blackout released the user cgroup")
	}
}

func TestCleanup(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	err := manager.Cleanup()
	if err != nil {
		t.Logf("Cleanup() error: %v", err)
	}
}
