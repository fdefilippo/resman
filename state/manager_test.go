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
	"github.com/fdefilippo/resman/internal/cpupoints"
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
	err           error
	messages      []string
	fields        []interface{}
	fieldsHistory [][]interface{}
}

func (l *failingCompletionLogger) Debug(string, ...interface{}) {}
func (l *failingCompletionLogger) Warn(string, ...interface{})  {}
func (l *failingCompletionLogger) Error(string, ...interface{}) {}
func (l *failingCompletionLogger) Info(string, ...interface{})  {}
func (l *failingCompletionLogger) DebugChecked(string, ...interface{}) error {
	return nil
}
func (l *failingCompletionLogger) InfoChecked(message string, fields ...interface{}) error {
	l.messages = append(l.messages, message)
	l.fields = append([]interface{}(nil), fields...)
	l.fieldsHistory = append(l.fieldsHistory, append([]interface{}(nil), fields...))
	if message == "Control cycle completed" {
		return l.err
	}
	return nil
}

func logFieldsByKey(t *testing.T, fields []interface{}) map[string]interface{} {
	t.Helper()
	result := make(map[string]interface{}, len(fields)/2)
	for index := 0; index+1 < len(fields); index += 2 {
		key, ok := fields[index].(string)
		if !ok {
			t.Fatalf("log field key %d = %#v, want string", index, fields[index])
		}
		result[key] = fields[index+1]
	}
	return result
}

func (m *mockMetricsCollector) GetTotalCores() int { return 4 }
func (m *mockMetricsCollector) GetDecisionHostCPUUsage() metrics.HostCPUUsageSample {
	return metrics.HostCPUUsageSample{UsagePercent: 50, Available: true}
}
func (m *mockMetricsCollector) GetObservationHostCPUUsage() metrics.HostCPUUsageSample {
	return metrics.HostCPUUsageSample{UsagePercent: 50, Available: true}
}
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
func (m *mockMetricsCollector) WriteMetricsToDatabase(batch metrics.PersistenceBatch) error {
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

func (m *mockCgroupManager) CleanupAll() error { return nil }

type observationOnlyMutationCgroupManager struct {
	mockCgroupManager
	mutations []string
}

type moveResultCgroupManager struct {
	mockCgroupManager
}

type stateExactResolverFunc func(string) ([]cpupoints.ResolvedUserIdentity, error)

func (f stateExactResolverFunc) ResolveExactUsername(username string) ([]cpupoints.ResolvedUserIdentity, error) {
	return f(username)
}

type mutableCPUCapacityProvider struct {
	state        cpupoints.CapacityState
	cpus         uint64
	refreshCalls int
	onRefresh    func(int, *mutableCPUCapacityProvider)
}

func (p *mutableCPUCapacityProvider) Refresh(pool cpupoints.ParentPoolPoints) (cpupoints.CapacityState, error) {
	p.refreshCalls++
	if p.onRefresh != nil {
		p.onRefresh(p.refreshCalls, p)
	}
	count, err := cpupoints.NewOnlineCPUCount(p.cpus)
	if err != nil {
		return p.state, err
	}
	quota, err := cpupoints.PlanParentQuota(count, pool)
	if err != nil {
		return p.state, err
	}
	p.state = cpupoints.CapacityState{Available: true, LastVerified: quota}
	return p.state, nil
}

func (p *mutableCPUCapacityProvider) State() cpupoints.CapacityState { return p.state }

type prometheusErrorRecord struct {
	component string
	errorType string
}

type limitHookMetricRecord struct {
	hookType metrics.LimitHookType
	outcome  metrics.LimitHookOutcome
}

type mockPrometheusExporter struct {
	mu                           sync.Mutex
	errors                       []prometheusErrorRecord
	limitHookExecutions          []limitHookMetricRecord
	controlCycleDurations        []time.Duration
	metricsCollectionDurations   []time.Duration
	lastSystemSnapshot           metrics.SystemExporterMetrics
	lastUserSnapshot             metrics.UserExporterMetrics
	userSnapshots                map[int]metrics.UserExporterMetrics
	systemSnapshots              int
	userMetricUpdates            int
	userMetricCleanups           int
	limitsActivated              int
	limitsDeactivated            int
	limitHookInFlight            int
	limitHookQueued              int
	limitHookCapacity            int
	lastHostCPUSample            metrics.HostCPUUsageSample
	hostCPUSampleObservations    int
	lastObservationHostCPUSample metrics.HostCPUUsageSample
	observationHostCPUSamples    int
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
	if m.userSnapshots == nil {
		m.userSnapshots = make(map[int]metrics.UserExporterMetrics)
	}
	m.userSnapshots[snapshot.UID] = snapshot
}
func (m *mockPrometheusExporter) UpdateUserWorkloadPattern(uid int, username string, pattern string, confidence float64) {
}
func (m *mockPrometheusExporter) RecordControlCycleTrigger(trigger string) {}
func (m *mockPrometheusExporter) ObserveControlCycleHostCPUUsage(sample metrics.HostCPUUsageSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastHostCPUSample = sample
	m.hostCPUSampleObservations++
}
func (m *mockPrometheusExporter) ObserveObservationHostCPUUsage(sample metrics.HostCPUUsageSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastObservationHostCPUSample = sample
	m.observationHostCPUSamples++
}
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
func (m *mockPrometheusExporter) IncrementIODeviceWeightClassification() {}
func (m *mockPrometheusExporter) IncrementIODeviceWeightProbe()          {}

func (m *mockPrometheusExporter) RecordLimitHookExecution(hookType metrics.LimitHookType, outcome metrics.LimitHookOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limitHookExecutions = append(m.limitHookExecutions, limitHookMetricRecord{hookType: hookType, outcome: outcome})
}
func (m *mockPrometheusExporter) ObserveLimitHookExecutor(inFlight, queued, capacity int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limitHookInFlight = inFlight
	m.limitHookQueued = queued
	m.limitHookCapacity = capacity
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
	errors                       []prometheusErrorRecord
	controlCycleDurations        int
	metricsCollectionDuration    int
	systemSnapshots              int
	userMetricUpdates            int
	userMetricCleanups           int
	limitsActivated              int
	limitsDeactivated            int
	hostCPUSampleObservations    int
	lastHostCPUSample            metrics.HostCPUUsageSample
	observationHostCPUSamples    int
	lastObservationHostCPUSample metrics.HostCPUUsageSample
	lastSystemSnapshot           metrics.SystemExporterMetrics
}

func (m *mockPrometheusExporter) snapshot() prometheusMetricSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return prometheusMetricSnapshot{
		errors:                       append([]prometheusErrorRecord(nil), m.errors...),
		controlCycleDurations:        len(m.controlCycleDurations),
		metricsCollectionDuration:    len(m.metricsCollectionDurations),
		systemSnapshots:              m.systemSnapshots,
		userMetricUpdates:            m.userMetricUpdates,
		userMetricCleanups:           m.userMetricCleanups,
		limitsActivated:              m.limitsActivated,
		limitsDeactivated:            m.limitsDeactivated,
		hostCPUSampleObservations:    m.hostCPUSampleObservations,
		lastHostCPUSample:            m.lastHostCPUSample,
		observationHostCPUSamples:    m.observationHostCPUSamples,
		lastObservationHostCPUSample: m.lastObservationHostCPUSample,
		lastSystemSnapshot:           m.lastSystemSnapshot,
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

func testCPUPointsPolicy(t *testing.T, entries map[string]struct {
	uid    int
	points int
}) cpupoints.PolicySnapshot {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".cpu-points-state-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	var content strings.Builder
	content.WriteString(cpupoints.PolicyMapMarker)
	for _, name := range names {
		fmt.Fprintf(&content, "\n%s=%d", name, entries[name].points)
	}
	path := filepath.Join(dir, "cpu-points.map")
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0600); err != nil {
		t.Fatal(err)
	}
	reserve, _ := cpupoints.NewReservePoints(100)
	root, _ := cpupoints.NewRootPoints(100)
	bestEffort, _ := cpupoints.NewBestEffortPoints(100)
	mapPath, err := cpupoints.NewPolicyMapPath(path)
	if err != nil {
		t.Fatal(err)
	}
	resolver := stateExactResolverFunc(func(username string) ([]cpupoints.ResolvedUserIdentity, error) {
		entry, ok := entries[username]
		if !ok {
			return nil, nil
		}
		return []cpupoints.ResolvedUserIdentity{{Username: username, UID: entry.uid}}, nil
	})
	snapshot, err := cpupoints.NewPolicyLoader().Load(cpupoints.PolicyInputs{
		Reserve: reserve, Root: root, BestEffort: bestEffort, MapPath: mapPath,
	}, resolver)
	if err != nil {
		t.Fatalf("load CPU Points test policy: %v", err)
	}
	return snapshot
}

func TestControlCycleRecordsOperationalOutcomes(t *testing.T) {
	tests := []struct {
		name          string
		userMetrics   map[int]*metrics.UserMetrics
		systemLoadErr error
		wantErrors    []prometheusErrorRecord
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
				&moveResultCgroupManager{},
				exporter,
			)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}

			err = manager.RunControlCycleWithTrigger(context.Background(), ControlCycleTriggerManual)
			if err != nil {
				t.Fatalf("RunControlCycleWithTrigger() error = %v", err)
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
			if got.limitsActivated != 0 {
				t.Errorf("observation-only activation count = %d, want 0", got.limitsActivated)
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
			name:               "pattern enforcement failure runs the remaining protective stages",
			failingStage:       "workload_pattern_detection",
			wantVisited:        stageNames,
			wantDeferredErrors: 1,
		},
		{
			name:         "collection failure remains fatal",
			failingStage: "collect_metrics",
			wantVisited:  []string{"reconcile_cpu_points", "check_blackout", "collect_metrics"},
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

func TestControlCyclePublishesOnlyAfterTheDecisionOutcomeIsFinalized(t *testing.T) {
	positions := make(map[string]int, len(defaultControlCyclePipeline))
	for index, stage := range defaultControlCyclePipeline {
		positions[stage.name] = index
	}
	for _, relation := range []struct {
		before string
		after  string
	}{
		{before: "collect_metrics", after: "make_decision"},
		{before: "make_decision", after: "execute_decision"},
		{before: "execute_decision", after: "finalize_enforcement_observation"},
		{before: "finalize_enforcement_observation", after: "update_prometheus"},
		{before: "finalize_enforcement_observation", after: "write_database"},
	} {
		before, beforeExists := positions[relation.before]
		after, afterExists := positions[relation.after]
		if !beforeExists || !afterExists || before >= after {
			t.Fatalf("pipeline order %s=%d (present=%t), %s=%d (present=%t)", relation.before, before, beforeExists, relation.after, after, afterExists)
		}
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
	if got.hostCPUSampleObservations != 0 {
		t.Errorf("control-cycle host CPU observations = %d, want 0", got.hostCPUSampleObservations)
	}
	if got.observationHostCPUSamples != 1 || !got.lastObservationHostCPUSample.Available {
		t.Errorf("observation host CPU samples = %d last=%+v, want one available sample", got.observationHostCPUSamples, got.lastObservationHostCPUSample)
	}
}

func TestControlCyclePublishesTheExactDecisionHostCPUSample(t *testing.T) {
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

	want := metrics.HostCPUUsageSample{
		UsagePercent:      37.5,
		UnavailableReason: metrics.HostCPUUsageUnavailableStale,
	}
	run := &controlCycleContext{metrics: &SystemMetrics{
		TotalCPUUsage:                 want.UsagePercent,
		HostCPUUsageAvailable:         want.Available,
		HostCPUUsageUnavailableReason: want.UnavailableReason,
	}}
	if err := manager.stageUpdatePrometheus(run); err != nil {
		t.Fatalf("stageUpdatePrometheus() error: %v", err)
	}

	got := exporter.snapshot()
	if got.hostCPUSampleObservations != 1 {
		t.Fatalf("control-cycle host CPU observations = %d, want 1", got.hostCPUSampleObservations)
	}
	if got.lastHostCPUSample != want {
		t.Fatalf("published host CPU sample = %+v, want %+v", got.lastHostCPUSample, want)
	}
}

func TestObservationOnlyCyclesSeparateIntentFromAppliedActionAndSuppressRepeatedNoise(t *testing.T) {
	logger := &failingCompletionLogger{}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(
		config.DefaultConfig(),
		&mockMetricsCollector{},
		&mockCgroupManager{},
		exporter,
		WithEnforcementStatus(cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeObservationOnly,
			Reason: cgroup.EnforcementReasonSystemdOwnsHostWorkloads,
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.logger = logger
	newRun := func(cycleID int64, decision string) *controlCycleContext {
		return &controlCycleContext{
			cfg:      config.DefaultConfig(),
			cycleID:  cycleID,
			trigger:  ControlCycleTriggerManual,
			decision: decision,
			reason:   "test decision",
			metrics: &SystemMetrics{
				CPUEligibleUsers: []int{1000},
				UserMetrics: map[int]*metrics.UserMetrics{
					1000: {EnforceableUsage: metrics.ProcessSetMetrics{ProcessCount: 3}},
				},
			},
		}
	}

	var run *controlCycleContext
	for cycleID := int64(1); cycleID <= 3; cycleID++ {
		run = newRun(cycleID, decisionActivate)
		if err := manager.stageExecuteDecision(run); err != nil {
			t.Fatalf("stageExecuteDecision() cycle %d error: %v", cycleID, err)
		}
	}
	if !reflect.DeepEqual(logger.messages, []string{"Enforcement action state changed"}) {
		t.Fatalf("repeated intent log messages = %v, want one state transition", logger.messages)
	}
	if err := manager.stageUpdatePrometheus(run); err != nil {
		t.Fatalf("stageUpdatePrometheus() error: %v", err)
	}
	if err := manager.stageLogCompletion(run); err != nil {
		t.Fatalf("stageLogCompletion() error: %v", err)
	}

	fields := logFieldsByKey(t, logger.fields)
	if fields["enforcement_mode"] != cgroup.EnforcementModeObservationOnly {
		t.Fatalf("enforcement_mode = %#v", fields["enforcement_mode"])
	}
	if fields["requested_policy_intent"] != cgroup.EnforcementPolicyIntentActivate ||
		fields["applied_enforcement_action"] != cgroup.AppliedEnforcementActionNone ||
		fields["enforcement_block_reason"] != cgroup.EnforcementBlockReasonSystemdOwnsWorkloads {
		t.Fatalf("completion enforcement projection = intent=%#v applied=%#v block=%#v",
			fields["requested_policy_intent"], fields["applied_enforcement_action"], fields["enforcement_block_reason"])
	}
	if _, legacy := fields["decision"]; legacy {
		t.Fatalf("completion contains ambiguous legacy decision field: %v", fields)
	}
	if fields["ingress_refused_count"] != 0 {
		t.Fatalf("ingress_refused_count = %#v, want 0 when no ingress is attempted", fields["ingress_refused_count"])
	}
	if fields["outcome"] != "success" {
		t.Fatalf("outcome = %#v, want pipeline success with explicit refusal fields", fields["outcome"])
	}

	wantState := cgroup.EnforcementCycleState{
		Mode:            cgroup.EnforcementModeObservationOnly,
		RequestedIntent: cgroup.EnforcementPolicyIntentActivate,
		AppliedAction:   cgroup.AppliedEnforcementActionNone,
		BlockReason:     cgroup.EnforcementBlockReasonSystemdOwnsWorkloads,
	}
	status := manager.GetStatus()
	if status.RequestedPolicyIntent != wantState.RequestedIntent ||
		status.AppliedEnforcementAction != wantState.AppliedAction ||
		status.EnforcementBlockReason != wantState.BlockReason {
		t.Fatalf("MCP runtime state = %+v, want %+v", status, wantState)
	}
	gotSnapshot := exporter.snapshot()
	if gotSnapshot.lastSystemSnapshot.EnforcementCycleState != wantState {
		t.Fatalf("Prometheus state = %+v, want %+v", gotSnapshot.lastSystemSnapshot.EnforcementCycleState, wantState)
	}
	if gotSnapshot.limitsActivated != 0 || gotSnapshot.limitsDeactivated != 0 {
		t.Fatalf("transition counters = activate %d deactivate %d, want zero",
			gotSnapshot.limitsActivated, gotSnapshot.limitsDeactivated)
	}

	deactivate := newRun(4, decisionDeactivate)
	if err := manager.stageExecuteDecision(deactivate); err != nil {
		t.Fatalf("deactivation intent error: %v", err)
	}
	if len(logger.messages) != 3 || logger.messages[2] != "Enforcement action state changed" {
		t.Fatalf("material intent change messages = %v", logger.messages)
	}
	if err := manager.stageExecuteDecision(newRun(5, decisionDeactivate)); err != nil {
		t.Fatalf("repeated deactivation intent error: %v", err)
	}
	if len(logger.messages) != 3 {
		t.Fatalf("repeated deactivation emitted another event: %v", logger.messages)
	}
	if err := manager.publishEnforcementCycleState(cgroup.EnforcementCycleState{
		Mode:            cgroup.EnforcementModeSystemdNative,
		RequestedIntent: cgroup.EnforcementPolicyIntentDeactivate,
		AppliedAction:   cgroup.AppliedEnforcementActionDeactivate,
		BlockReason:     cgroup.EnforcementBlockReasonNone,
	}); err != nil {
		t.Fatalf("publish mode change: %v", err)
	}
	if len(logger.messages) != 4 || logger.messages[3] != "Enforcement action state changed" {
		t.Fatalf("mode change messages = %v", logger.messages)
	}
	eventFields := logFieldsByKey(t, logger.fieldsHistory[3])
	if !reflect.DeepEqual(eventFields, map[string]interface{}{
		"enforcement_mode":           cgroup.EnforcementModeSystemdNative,
		"requested_policy_intent":    cgroup.EnforcementPolicyIntentDeactivate,
		"applied_enforcement_action": cgroup.AppliedEnforcementActionDeactivate,
		"enforcement_block_reason":   cgroup.EnforcementBlockReasonNone,
	}) {
		t.Fatalf("bounded state-change fields = %v", eventFields)
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
		logger:                   logging.GetLogger(),
		metricsCollector:         collector,
		prometheusExporter:       exporter,
		activeUsers:              map[int]bool{1001: true},
		cpuPointsLifecycleEvents: map[int]cpuPointsLifecycleEvent{1001: {state: metrics.CPUPointsLifecycleFailed}},
		ioWeightStatus:           newDisabledIODeviceWeightStatus(),
	}
	sample := &SystemMetrics{
		Timestamp:     time.Now().UTC(),
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
	sample.PersistenceSystem = metrics.SystemPersistenceMetrics{
		SampleEpochID:                   sample.Timestamp.UnixNano(),
		IntervalEnd:                     sample.Timestamp,
		IODeviceWeightState:             "disabled",
		IODeviceWeightMechanism:         "none",
		IODeviceWeightProgrammedState:   "not_attempted",
		IODeviceWeightReadBackState:     "not_attempted",
		IODeviceWeightAuthorityCoverage: "unavailable",
		IODeviceWeightValuesJSON:        "[]",
		IODeviceWeightObservedDelivery:  "not_measured",
		TotalCPUUsagePercent:            sample.TotalCPUUsage,
		TotalCores:                      sample.TotalCores,
		SystemLoad:                      sample.SystemLoad,
	}
	sample.PersistenceUsers = map[int]metrics.UserPersistenceMetrics{
		1001: {
			Metrics:         sample.UserMetrics[1001],
			ConfiguredClass: "best_effort",
			LifecycleState:  metrics.CPUPointsLifecycleApplied,
		},
	}

	err = collector.WriteMetricsToDatabase(metrics.PersistenceBatch{
		System: sample.PersistenceSystem,
		Users:  sample.PersistenceUsers,
	})
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
		name             string
		prepare          func(t *testing.T)
		wantErrors       []prometheusErrorRecord
		wantShouldWrite  bool
		wantSystemRows   int
		wantUserRows     int
		wantPendingEvent bool
	}{
		{
			name:             "failed transaction remains retryable",
			wantErrors:       []prometheusErrorRecord{{component: metricsDatabaseErrorComponent, errorType: metricsDatabaseWriteFailure}},
			wantShouldWrite:  true,
			wantPendingEvent: true,
		},
		{
			name: "successful retry marks write",
			prepare: func(t *testing.T) {
				t.Helper()
				if _, err := controlDB.Exec("DROP TRIGGER reject_uid_1001"); err != nil {
					t.Fatalf("failed to drop rejection trigger: %v", err)
				}
			},
			wantErrors:       []prometheusErrorRecord{{component: metricsDatabaseErrorComponent, errorType: metricsDatabaseWriteFailure}},
			wantShouldWrite:  false,
			wantSystemRows:   1,
			wantUserRows:     1,
			wantPendingEvent: false,
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
			_, pendingEvent := manager.cpuPointsLifecycleEvents[1001]
			if pendingEvent != tt.wantPendingEvent {
				t.Errorf("pending lifecycle event = %t, want %t", pendingEvent, tt.wantPendingEvent)
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

func TestRAMAndIOPressureActivateIndependentlyOfCPUCapacity(t *testing.T) {
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

func TestObservationOnlyModePreservesIntentWithoutAnyCgroupMutation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.IOEnabled = true
	cgroups := &observationOnlyMutationCgroupManager{}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(
		cfg,
		&mockMetricsCollector{},
		cgroups,
		exporter,
		WithEnforcementStatus(cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeObservationOnly,
			Reason: cgroup.EnforcementReasonSystemdOwnsHostWorkloads,
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	sample := &SystemMetrics{
		CPUEligibleUsers: []int{1000},
		RAMEligibleUsers: []int{1000},
		IOEligibleUsers:  []int{1000},
		UserMetrics: map[int]*metrics.UserMetrics{
			1000: {
				UID:      1000,
				Username: "alice",
				EnforceableUsage: metrics.ProcessSetMetrics{
					ProcessCount: 3,
				},
			},
		},
	}

	if err := manager.executeDecision("ACTIVATE_LIMITS", sample); err != nil {
		t.Fatalf("executeDecision() error: %v", err)
	}
	if len(cgroups.mutations) != 0 {
		t.Fatalf("observation-only activation performed cgroup mutations: %v", cgroups.mutations)
	}
	if !manager.requestedCPUUsers[1000] || manager.activeUsers[1000] {
		t.Fatalf("CPU requested=%t active=%t, want true/false", manager.requestedCPUUsers[1000], manager.activeUsers[1000])
	}
	resourceState := manager.resourceLimits[1000]
	if !resourceState.ram || resourceState.ramApplied || !resourceState.io || resourceState.ioApplied {
		t.Fatalf("resource state = %+v, want RAM/IO requested but unapplied", resourceState)
	}
	event := manager.cpuPointsLifecycleEvents[1000]
	if event.state != metrics.CPUPointsLifecycleEligibleInactive {
		t.Fatalf("lifecycle event = %+v", event)
	}
	status := manager.GetStatus()
	if status.EnforcementMode != cgroup.EnforcementModeObservationOnly ||
		status.EnforcementReason != cgroup.EnforcementReasonSystemdOwnsHostWorkloads ||
		status.AnyLimitsActive || status.ActivelyLimitedUsersCount != 0 {
		t.Fatalf("runtime status = %+v", status)
	}
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

type cleanupLockCheckingCgroupManager struct {
	checkLock func() error
}

type blockingCleanupCgroupManager struct {
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
	cgroupCleanupErr := errors.New("cgroup cleanup failed")
	exporterStopErr := errors.New("exporter stop failed")
	cgroupManager := &cleanupBestEffortCgroupManager{
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
	manager.activeUsers[1000] = true

	cleanupErr := manager.Cleanup()
	if !errors.Is(cleanupErr, cgroupCleanupErr) ||
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
	cgroupManager := &mockCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.patternDetector.userStats[1001] = &UserHourlyStats{}

	run := &controlCycleContext{
		cfg:     cfg,
		metrics: &SystemMetrics{UserMetrics: collector.GetAllUserMetricsForDecision()},
	}
	if err := manager.stageWorkloadPatternDetection(run); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.patternDetector.userStats[1000]; !exists {
		t.Fatal("eligible user 1000 lost its observational pattern history")
	}
	if _, exists := manager.patternDetector.userStats[1001]; exists {
		t.Fatal("excluded user 1001 remained in pattern statistics")
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
