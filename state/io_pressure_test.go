package state

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

func ioDecisionConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 100
	cfg.CPUThresholdDuration = 0
	cfg.CPUReleaseThreshold = 40
	cfg.IgnoreSystemLoad = true
	cfg.MinActiveTime = 0
	cfg.IOEnabled = true
	cfg.IOThreshold = 75
	cfg.IOReleaseThreshold = 40
	cfg.IOThresholdDuration = 0
	cfg.IOReadBPS = "max"
	cfg.IOWriteBPS = "max"
	cfg.IOReadIOPS = 0
	cfg.IOWriteIOPS = 0
	return cfg
}

func TestMakeDecisionEvaluatesEveryIODimension(t *testing.T) {
	tests := []struct {
		name             string
		configure        func(*config.Config)
		metrics          SystemMetrics
		wantDecision     string
		wantReasonSignal string
	}{
		{
			name:         "read bandwidth alone activates",
			configure:    func(cfg *config.Config) { cfg.IOReadBPS = "100M" },
			metrics:      SystemMetrics{IOEligibleUsersCount: 1, IOEligibleReadBPS: 80 * 1024 * 1024},
			wantDecision: "ACTIVATE_LIMITS", wantReasonSignal: "read_bps",
		},
		{
			name:         "write bandwidth alone activates",
			configure:    func(cfg *config.Config) { cfg.IOWriteBPS = "100M" },
			metrics:      SystemMetrics{IOEligibleUsersCount: 1, IOEligibleWriteBPS: 80 * 1024 * 1024},
			wantDecision: "ACTIVATE_LIMITS", wantReasonSignal: "write_bps",
		},
		{
			name:         "read operations alone activate",
			configure:    func(cfg *config.Config) { cfg.IOReadIOPS = 1000 },
			metrics:      SystemMetrics{IOEligibleUsersCount: 1, IOEligibleReadBlockIOPS: 800},
			wantDecision: "ACTIVATE_LIMITS", wantReasonSignal: "read_iops",
		},
		{
			name:         "write operations alone activate",
			configure:    func(cfg *config.Config) { cfg.IOWriteIOPS = 1000 },
			metrics:      SystemMetrics{IOEligibleUsersCount: 1, IOEligibleWriteBlockIOPS: 800},
			wantDecision: "ACTIVATE_LIMITS", wantReasonSignal: "write_iops",
		},
		{
			name: "any configured dimension may activate",
			configure: func(cfg *config.Config) {
				cfg.IOReadBPS = "100M"
				cfg.IOWriteIOPS = 1000
			},
			metrics: SystemMetrics{
				IOEligibleUsersCount:     2,
				IOEligibleReadBPS:        100 * 1024 * 1024,
				IOEligibleWriteBlockIOPS: 1600,
			},
			wantDecision: "ACTIVATE_LIMITS", wantReasonSignal: "write_iops",
		},
		{
			name:      "disabled dimensions are ignored independently",
			configure: func(cfg *config.Config) { cfg.IOReadBPS = "100M" },
			metrics: SystemMetrics{
				IOEligibleUsersCount:     1,
				IOEligibleReadBPS:        50 * 1024 * 1024,
				IOEligibleWriteBPS:       1000 * 1024 * 1024,
				IOEligibleReadBlockIOPS:  100000,
				IOEligibleWriteBlockIOPS: 100000,
			},
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
		{
			name: "no configured dimensions cannot activate",
			metrics: SystemMetrics{
				IOEligibleUsersCount:     1,
				IOEligibleReadBPS:        1000 * 1024 * 1024,
				IOEligibleWriteBlockIOPS: 100000,
			},
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
		{
			name:      "configured dimension without eligible users cannot activate",
			configure: func(cfg *config.Config) { cfg.IOReadBPS = "100M" },
			metrics: SystemMetrics{
				IOEligibleUsersCount: 0,
				IOEligibleReadBPS:    1000 * 1024 * 1024,
			},
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ioDecisionConfig()
			if tt.configure != nil {
				tt.configure(cfg)
			}
			manager := &Manager{
				cfg:                cfg,
				thresholdTracker:   &ThresholdTracker{},
				ioThresholdTracker: &ThresholdTracker{},
			}
			tt.metrics.TotalCores = 4
			decision, reason := manager.makeDecision(&tt.metrics)
			if decision != tt.wantDecision {
				t.Fatalf("makeDecision() = %s (%s), want %s", decision, reason, tt.wantDecision)
			}
			if tt.wantReasonSignal != "" && !strings.Contains(reason, tt.wantReasonSignal) {
				t.Fatalf("reason %q does not identify %q", reason, tt.wantReasonSignal)
			}
		})
	}
}

func TestMakeDecisionReleasesIOOnlyWhenEveryConfiguredDimensionIsBelow(t *testing.T) {
	tests := []struct {
		name         string
		configure    func(*config.Config)
		metrics      SystemMetrics
		wantDecision string
	}{
		{
			name:      "read bandwidth at release threshold maintains",
			configure: func(cfg *config.Config) { cfg.IOReadBPS = "100M" },
			metrics: SystemMetrics{
				IOEligibleUsersCount: 1,
				IOEligibleReadBPS:    40 * 1024 * 1024,
			},
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
		{
			name:      "write bandwidth at release threshold maintains",
			configure: func(cfg *config.Config) { cfg.IOWriteBPS = "100M" },
			metrics: SystemMetrics{
				IOEligibleUsersCount: 1,
				IOEligibleWriteBPS:   40 * 1024 * 1024,
			},
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
		{
			name:      "read operations at release threshold maintain",
			configure: func(cfg *config.Config) { cfg.IOReadIOPS = 1000 },
			metrics: SystemMetrics{
				IOEligibleUsersCount:    1,
				IOEligibleReadBlockIOPS: 400,
			},
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
		{
			name:      "write operations at release threshold maintain",
			configure: func(cfg *config.Config) { cfg.IOWriteIOPS = 1000 },
			metrics: SystemMetrics{
				IOEligibleUsersCount:     1,
				IOEligibleWriteBlockIOPS: 400,
			},
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
		{
			name: "all configured dimensions below release",
			configure: func(cfg *config.Config) {
				cfg.IOReadBPS = "100M"
				cfg.IOWriteIOPS = 1000
			},
			metrics: SystemMetrics{
				IOEligibleUsersCount:     1,
				IOEligibleReadBPS:        30 * 1024 * 1024,
				IOEligibleWriteBlockIOPS: 300,
			},
			wantDecision: "DEACTIVATE_LIMITS",
		},
		{
			name:      "disabled dimensions do not prevent release",
			configure: func(cfg *config.Config) { cfg.IOReadBPS = "100M" },
			metrics: SystemMetrics{
				IOEligibleUsersCount:    1,
				IOEligibleReadBPS:       30 * 1024 * 1024,
				IOEligibleWriteBPS:      1000 * 1024 * 1024,
				IOEligibleReadBlockIOPS: 100000,
			},
			wantDecision: "DEACTIVATE_LIMITS",
		},
		{
			name:      "unavailable IO sample does not matter when every dimension is disabled",
			configure: func(*config.Config) {},
			metrics: SystemMetrics{
				IOEligibleUsersCount:           1,
				IOEligibleUnavailableProcesses: 1,
			},
			wantDecision: "DEACTIVATE_LIMITS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ioDecisionConfig()
			tt.configure(cfg)
			manager := &Manager{
				cfg:                       cfg,
				resourceLimitsActive:      true,
				resourceLimitsAppliedTime: time.Now().Add(-time.Hour),
				thresholdTracker:          &ThresholdTracker{},
				ioThresholdTracker:        &ThresholdTracker{},
				stabilityTracker:          newUserStabilityTracker(),
			}
			tt.metrics.TotalCores = 4
			decision, reason := manager.makeDecision(&tt.metrics)
			if decision != tt.wantDecision {
				t.Fatalf("makeDecision() = %s (%s), want %s", decision, reason, tt.wantDecision)
			}
		})
	}
}

func TestMakeDecisionTreatsIncompleteIOCoverageAsUnknownNotZero(t *testing.T) {
	tests := []struct {
		name             string
		active           bool
		cpuUsage         float64
		readBPS          float64
		unavailable      int
		wantDecision     string
		wantReasonSignal string
	}{
		{
			name:        "inactive partial sample below threshold maintains as unknown",
			unavailable: 1, wantDecision: "MAINTAIN_CURRENT_STATE", wantReasonSignal: "coverage incomplete",
		},
		{
			name:    "inactive partial sample above threshold still proves activation",
			readBPS: 80 * 1024 * 1024, unavailable: 1, wantDecision: "ACTIVATE_LIMITS", wantReasonSignal: "read_bps",
		},
		{
			name:             "incomplete IO coverage does not block proven CPU activation",
			cpuUsage:         100,
			unavailable:      1,
			wantDecision:     "ACTIVATE_LIMITS",
			wantReasonSignal: "CPU 100.0%",
		},
		{
			name:   "active partial sample below threshold cannot prove safe release",
			active: true, unavailable: 1, wantDecision: "MAINTAIN_CURRENT_STATE", wantReasonSignal: "cannot be released safely",
		},
		{
			name:   "complete sample below threshold retains normal release contract",
			active: true, wantDecision: "DEACTIVATE_LIMITS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ioDecisionConfig()
			cfg.IOReadBPS = "100M"
			manager := &Manager{
				cfg:                       cfg,
				resourceLimitsActive:      tt.active,
				resourceLimitsAppliedTime: time.Now().Add(-time.Hour),
				thresholdTracker:          &ThresholdTracker{},
				ioThresholdTracker:        &ThresholdTracker{},
				stabilityTracker:          newUserStabilityTracker(),
			}
			metrics := &SystemMetrics{
				TotalCores:                     4,
				CPUEligibleCPUUsage:            tt.cpuUsage,
				IOEligibleUsersCount:           1,
				IOEligibleReadBPS:              tt.readBPS,
				IOEligibleUnavailableProcesses: tt.unavailable,
			}
			decision, reason := manager.makeDecision(metrics)
			if decision != tt.wantDecision {
				t.Fatalf("makeDecision() = %s (%s), want %s", decision, reason, tt.wantDecision)
			}
			if tt.wantReasonSignal != "" && !strings.Contains(reason, tt.wantReasonSignal) {
				t.Fatalf("reason %q does not contain %q", reason, tt.wantReasonSignal)
			}
		})
	}
}

func TestMakeDecisionIOThresholdDurationAccumulatesAcrossCycles(t *testing.T) {
	cfg := ioDecisionConfig()
	cfg.IOReadBPS = "100M"
	cfg.IOThresholdDuration = 60
	manager := &Manager{
		cfg:                cfg,
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
	}
	metrics := &SystemMetrics{
		TotalCores:           4,
		IOEligibleUsersCount: 1,
		IOEligibleReadBPS:    80 * 1024 * 1024,
	}

	if decision, reason := manager.makeDecision(metrics); decision != "MAINTAIN_CURRENT_STATE" || !strings.Contains(reason, "waiting") {
		t.Fatalf("first decision = %s (%s), want waiting maintenance", decision, reason)
	}
	manager.ioThresholdTracker.mu.Lock()
	manager.ioThresholdTracker.firstOverThresholdTime = time.Now().Add(-60 * time.Second)
	manager.ioThresholdTracker.mu.Unlock()
	if decision, reason := manager.makeDecision(metrics); decision != "ACTIVATE_LIMITS" {
		t.Fatalf("decision after duration = %s (%s), want ACTIVATE_LIMITS", decision, reason)
	}
}

func TestCalculateIOByteRatesHandlesEveryCounterIndependently(t *testing.T) {
	delta := resmanmetrics.ProcessIODelta{ReadBytes: 200, WriteBytes: 100}
	rates := calculateIOByteRates(delta, 2*time.Second)

	if rates.readBytes != 100 || rates.writeBytes != 50 {
		t.Fatalf("calculateIOByteRates() = %+v, want readBPS=100 writeBPS=50", rates)
	}
	if got := calculateIOByteRates(delta, 0); got != (ioByteRate{}) {
		t.Fatalf("zero-duration rates = %+v, want zero", got)
	}
}

func TestByteRateLimitHandlesDisabledValuesPerDimension(t *testing.T) {
	tests := []struct {
		value string
		want  float64
	}{
		{value: "", want: 0},
		{value: "max", want: 0},
		{value: "1M", want: 1024 * 1024},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := byteRateLimit(tt.value); got != tt.want {
				t.Fatalf("byteRateLimit(%q) = %.0f, want %.0f", tt.value, got, tt.want)
			}
		})
	}
}

type blockIOSequenceCgroupManager struct {
	mockCgroupManager
	samples          []blockIOCounterSample
	index            int
	placements       []string
	placementErrors  map[int]error
	placementResults map[int]cgroup.ProcessMoveResult
	readErrors       map[int]error
	statsReads       int
	cleanupErrors    map[int]error
	cleanups         []int
	sharedReleases   int
}

func (m *blockIOSequenceCgroupManager) EnsureUserCgroupPlacement(uid int, sharedPath, _ string) (string, cgroup.ProcessMoveResult, error) {
	m.placements = append(m.placements, sharedPath)
	if err := m.placementErrors[uid]; err != nil {
		return "", cgroup.ProcessMoveResult{}, err
	}
	if result := m.placementResults[uid]; result != (cgroup.ProcessMoveResult{}) {
		return sharedPath, result, nil
	}
	return sharedPath, cgroup.ProcessMoveResult{AlreadyPresent: 1}, nil
}

func (m *blockIOSequenceCgroupManager) ReleaseUserFromSharedCgroup(_ int, _, _ string) error {
	m.sharedReleases++
	return nil
}

func (m *blockIOSequenceCgroupManager) GetIOStats(uid int) (uint64, uint64, uint64, uint64, error) {
	m.statsReads++
	if err := m.readErrors[uid]; err != nil {
		return 0, 0, 0, 0, err
	}
	if len(m.samples) == 0 {
		return 0, 0, 0, 0, nil
	}
	index := m.index
	if index >= len(m.samples) {
		index = len(m.samples) - 1
	}
	m.index++
	sample := m.samples[index]
	return 0, 0, sample.readOps, sample.writeOps, nil
}

func TestBlockIOObservationSkipsNestedNamespaceWithoutReadingManagedStats(t *testing.T) {
	cfg := ioDecisionConfig()
	cfg.IOReadIOPS = 1000
	collector := &mockMetricsCollector{
		preserveExplicitEnforceableUsage: true,
		allUserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {UID: 1000, Username: "nested-user"},
		},
	}
	cgroups := &blockIOSequenceCgroupManager{
		placementResults: map[int]cgroup.ProcessMoveResult{
			1000: {Candidates: 1, PIDNamespaceMismatches: 1},
		},
	}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, collector, cgroups, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sample, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("collectSystemMetrics() error: %v", err)
	}
	if sample.IOBlockIOPSUnavailableUsers != 1 || cgroups.statsReads != 0 {
		t.Fatalf("unavailable users=%d stats reads=%d, want 1/0", sample.IOBlockIOPSUnavailableUsers, cgroups.statsReads)
	}
	if len(exporter.ingressSkips) != 1 || exporter.ingressSkips[0].PIDNamespaceMismatches != 1 {
		t.Fatalf("ingress skip records = %+v", exporter.ingressSkips)
	}
}

func (m *blockIOSequenceCgroupManager) CleanupUserCgroup(uid int) error {
	m.cleanups = append(m.cleanups, uid)
	return m.cleanupErrors[uid]
}

func TestCollectSystemMetricsBuildsByteRatesAndBlockIOPS(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IOEnabled = true
	cfg.IOReadIOPS = 1000
	cfg.IOWriteIOPS = 1000
	collector := &mockMetricsCollector{
		preserveExplicitEnforceableUsage: true,
		allUserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {
				UID:      1000,
				Username: "alice",
				EnforceableUsage: resmanmetrics.ProcessSetMetrics{
					IODelta: resmanmetrics.ProcessIODelta{},
				},
			},
		},
	}
	cgroups := &blockIOSequenceCgroupManager{samples: []blockIOCounterSample{
		{},
		{readOps: 40, writeOps: 80},
	}}
	manager, err := NewManager(cfg, collector, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	if _, err := manager.collectSystemMetrics(); err != nil {
		t.Fatalf("first collectSystemMetrics() error: %v", err)
	}

	collector.allUserMetrics[1000].EnforceableUsage.IODelta = resmanmetrics.ProcessIODelta{
		ReadBytes:  200,
		WriteBytes: 400,
	}
	manager.prevIOTime = time.Now().Add(-2 * time.Second)
	sample, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("second collectSystemMetrics() error: %v", err)
	}

	if sample.IOEligibleReadBPS < 99 || sample.IOEligibleWriteBPS < 199 ||
		sample.IOEligibleReadBlockIOPS < 19 || sample.IOEligibleWriteBlockIOPS < 39 {
		t.Fatalf("I/O rate sample = read %.1f BPS, write %.1f BPS, read %.1f ops/s, write %.1f ops/s",
			sample.IOEligibleReadBPS,
			sample.IOEligibleWriteBPS,
			sample.IOEligibleReadBlockIOPS,
			sample.IOEligibleWriteBlockIOPS,
		)
	}
}

func TestBlockIOPSRateContinuesAcrossEnforcementPlacementChanges(t *testing.T) {
	cfg := ioDecisionConfig()
	cfg.IOReadIOPS = 1000
	collector := &mockMetricsCollector{
		preserveExplicitEnforceableUsage: true,
		allUserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {UID: 1000, Username: "alice"},
		},
	}
	cgroups := &blockIOSequenceCgroupManager{samples: []blockIOCounterSample{
		{},
		{readOps: 50},
		{readOps: 100},
	}}
	manager, err := NewManager(cfg, collector, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	first, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("first sample: %v", err)
	}
	if first.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatal("first block IOPS sample was reported as complete without a baseline")
	}

	manager.mu.Lock()
	manager.activeUsers[1000] = true
	manager.sharedCgroupPath = "/limited"
	manager.mu.Unlock()
	manager.prevIOTime = time.Now().Add(-time.Second)
	activated, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("activation placement sample: %v", err)
	}
	if activated.IOBlockIOPSUnavailableUsers != 0 || activated.IOEligibleReadBlockIOPS < 49 {
		t.Fatalf("activation placement produced unavailable_users=%d read_iops=%.1f", activated.IOBlockIOPSUnavailableUsers, activated.IOEligibleReadBlockIOPS)
	}

	manager.mu.Lock()
	delete(manager.activeUsers, 1000)
	manager.mu.Unlock()
	manager.prevIOTime = time.Now().Add(-time.Second)
	released, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("release placement sample: %v", err)
	}
	if released.IOBlockIOPSUnavailableUsers != 0 || released.IOEligibleReadBlockIOPS < 49 {
		t.Fatalf("release placement produced unavailable_users=%d read_iops=%.1f", released.IOBlockIOPSUnavailableUsers, released.IOEligibleReadBlockIOPS)
	}
	wantPlacements := []string{"", "/limited", ""}
	if !slices.Equal(cgroups.placements, wantPlacements) {
		t.Fatalf("placements = %v, want %v", cgroups.placements, wantPlacements)
	}
}

func TestPerUserBlockIOObservationFailurePreservesTheControlCycleTail(t *testing.T) {
	tests := []struct {
		name            string
		placementErrors map[int]error
		readErrors      map[int]error
		wantErrorType   string
	}{
		{
			name:            "placement failure",
			placementErrors: map[int]error{1001: errors.New("simulated transient EBUSY")},
			wantErrorType:   blockIOObservationPlacementFailure,
		},
		{
			name:          "counter read failure",
			readErrors:    map[int]error{1001: errors.New("simulated transient ENOENT")},
			wantErrorType: blockIOObservationCounterReadFailure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ioDecisionConfig()
			cfg.IOReadIOPS = 100
			collector := &mockMetricsCollector{
				preserveExplicitEnforceableUsage: true,
				allUserMetrics: map[int]*resmanmetrics.UserMetrics{
					1000: {UID: 1000, Username: "alice"},
					1001: {UID: 1001, Username: "bob"},
				},
			}
			cgroups := &blockIOSequenceCgroupManager{
				samples:         []blockIOCounterSample{{readOps: 50}},
				placementErrors: tt.placementErrors,
				readErrors:      tt.readErrors,
			}
			exporter := &mockPrometheusExporter{}
			manager, err := NewManager(cfg, collector, cgroups, exporter)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			manager.previousBlockIOCounters[1000] = blockIOCounterSample{}
			manager.previousIOEligibleUsers[1000] = struct{}{}
			manager.prevIOTime = time.Now().Add(-time.Second)

			tailRan := false
			run := &controlCycleContext{cfg: cfg, startTime: time.Now(), trigger: ControlCycleTriggerManual}
			stages := []controlCycleStage{
				{name: "collect_metrics", run: (*Manager).stageCollectMetrics},
				{name: "protective_tail", run: func(_ *Manager, _ *controlCycleContext) error {
					tailRan = true
					return nil
				}},
			}
			err = runControlCyclePipeline(manager, run, stages)
			if err == nil || !strings.Contains(err.Error(), "UID 1001") {
				t.Fatalf("pipeline error = %v, want degraded UID 1001 observation error", err)
			}
			if !tailRan {
				t.Fatal("per-user observation failure aborted the protective tail")
			}
			if run.metrics == nil {
				t.Fatal("per-user observation failure discarded the metrics snapshot")
			}
			if run.metrics.IOBlockIOPSUnavailableUsers != 1 {
				t.Fatalf("unavailable block IOPS users = %d, want 1", run.metrics.IOBlockIOPSUnavailableUsers)
			}
			if got := run.metrics.IOEligibleReadBlockIOPS; got < 49 {
				t.Fatalf("available-user lower-bound read IOPS = %.1f, want about 50", got)
			}
			if _, retained := manager.previousBlockIOCounters[1001]; retained {
				t.Fatal("failed UID retained a baseline that could inflate the next rate")
			}
			if len(exporter.errors) != 1 || exporter.errors[0] != (prometheusErrorRecord{
				component: blockIOObservationErrorComponent,
				errorType: tt.wantErrorType,
			}) {
				t.Fatalf("Prometheus errors = %+v, want one bounded %s", exporter.errors, tt.wantErrorType)
			}
		})
	}
}

func TestSplitBlockIOPlacementIsAWarningAndCoverageGap(t *testing.T) {
	cfg := ioDecisionConfig()
	cfg.IOReadIOPS = 100
	collector := &mockMetricsCollector{
		preserveExplicitEnforceableUsage: true,
		allUserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {UID: 1000, Username: "alice"},
		},
	}
	cgroups := &blockIOSequenceCgroupManager{
		placementErrors: map[int]error{1000: &cgroup.UserCgroupPlacementIncompleteError{
			UID:           1000,
			AlternatePath: "/old/user_1000",
			DesiredPath:   "/new/user_1000",
			Processes:     1,
		}},
	}
	manager, err := NewManager(cfg, collector, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	run := &controlCycleContext{cfg: cfg, startTime: time.Now(), trigger: ControlCycleTriggerManual}
	tailRan := false
	stages := []controlCycleStage{
		{name: "collect_metrics", run: (*Manager).stageCollectMetrics},
		{name: "tail", run: func(_ *Manager, _ *controlCycleContext) error {
			tailRan = true
			return nil
		}},
	}
	if err := runControlCyclePipeline(manager, run, stages); err != nil {
		t.Fatalf("split-placement warning returned a hard cycle error: %v", err)
	}
	if !tailRan || len(run.degradedWarnings) != 1 || len(run.degradedErrors) != 0 {
		t.Fatalf("tail=%t warnings=%d errors=%d, want true/1/0", tailRan, len(run.degradedWarnings), len(run.degradedErrors))
	}
	if run.metrics.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatalf("split placement unavailable users = %d, want 1", run.metrics.IOBlockIOPSUnavailableUsers)
	}
}

func TestBlockIOPSIncompleteCoverageUsesLowerBoundForActivationButBlocksRelease(t *testing.T) {
	tests := []struct {
		name         string
		active       bool
		readIOPS     float64
		wantDecision string
		wantReason   string
	}{
		{
			name:         "available users can still prove activation",
			readIOPS:     160,
			wantDecision: "ACTIVATE_LIMITS",
			wantReason:   "read_iops",
		},
		{
			name:         "one warming user blocks global release",
			active:       true,
			wantDecision: "MAINTAIN_CURRENT_STATE",
			wantReason:   "unavailable for 1 users",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ioDecisionConfig()
			cfg.IOReadIOPS = 100
			manager := &Manager{
				cfg:                       cfg,
				resourceLimitsActive:      tt.active,
				resourceLimitsAppliedTime: time.Now().Add(-time.Hour),
				thresholdTracker:          &ThresholdTracker{},
				ioThresholdTracker:        &ThresholdTracker{},
				stabilityTracker:          newUserStabilityTracker(),
			}
			metrics := &SystemMetrics{
				TotalCores:                  4,
				IOEligibleUsersCount:        2,
				IOEligibleReadBlockIOPS:     tt.readIOPS,
				IOBlockIOPSUnavailableUsers: 1,
			}
			decision, reason := manager.makeDecision(metrics)
			if decision != tt.wantDecision || !strings.Contains(reason, tt.wantReason) {
				t.Fatalf("makeDecision() = %s (%s), want %s containing %q", decision, reason, tt.wantDecision, tt.wantReason)
			}
		})
	}
}

func TestDeactivateLimitsPreservesBlockIOObservationCgroup(t *testing.T) {
	cfg := ioDecisionConfig()
	sharedPath := t.TempDir()
	cgroups := &blockIOSequenceCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.activeUsers[1000] = true
	manager.blockIOObservedUsers[1000] = true
	manager.sharedCgroupPath = sharedPath

	if err := manager.deactivateLimits(); err != nil {
		t.Fatalf("deactivateLimits() error: %v", err)
	}
	if cgroups.sharedReleases != 0 {
		t.Fatalf("origin releases = %d, want 0 for observed user", cgroups.sharedReleases)
	}
	if !slices.Equal(cgroups.placements, []string{""}) {
		t.Fatalf("placements = %v, want standalone observation placement", cgroups.placements)
	}
	if !manager.blockIOObservedUsers[1000] {
		t.Fatal("deactivation discarded block I/O observation state")
	}
}

func TestObservedStandaloneResourceStateBecomesSharedWithoutStaleStandaloneFlag(t *testing.T) {
	cfg := ioDecisionConfig()
	cgroups := &blockIOSequenceCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.blockIOObservedUsers[1000] = true
	manager.resourceLimits[1000] = userResourceLimitState{
		standalone: true,
		io:         true,
		ioApplied:  true,
	}

	if _, _, err := manager.placeUserInSharedCgroup(1000, "/limited", cfg.CPUQuotaNormal); err != nil {
		t.Fatalf("placeUserInSharedCgroup() error: %v", err)
	}
	state := manager.resourceLimits[1000]
	if state.standalone || !state.io || !state.ioApplied {
		t.Fatalf("resource state after shared placement = %+v", state)
	}
}

func TestBlockIOPSDecisionIgnoresSyscallsAndUsesDeviceOperations(t *testing.T) {
	cfg := ioDecisionConfig()
	cfg.IOReadIOPS = 100
	collector := &mockMetricsCollector{
		preserveExplicitEnforceableUsage: true,
		allUserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {
				UID:      1000,
				Username: "alice",
				EnforceableUsage: resmanmetrics.ProcessSetMetrics{
					IOReadOps: 1_000_000,
				},
			},
		},
	}
	cgroups := &blockIOSequenceCgroupManager{samples: []blockIOCounterSample{
		{},
		{},
		{readOps: 100},
	}}
	manager, err := NewManager(cfg, collector, cgroups, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	if _, err := manager.collectSystemMetrics(); err != nil {
		t.Fatalf("initial collectSystemMetrics() error: %v", err)
	}
	manager.prevIOTime = time.Now().Add(-time.Second)
	pageCacheSample, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("page-cache sample error: %v", err)
	}
	if pageCacheSample.IOEligibleReadBlockIOPS != 0 {
		t.Fatalf("syscall-only activity produced %.1f block IOPS", pageCacheSample.IOEligibleReadBlockIOPS)
	}
	if decision, _ := manager.makeDecision(pageCacheSample); decision == "ACTIVATE_LIMITS" {
		t.Fatal("syscall-only activity activated block IOPS enforcement")
	}

	manager.prevIOTime = time.Now().Add(-time.Second)
	directIOSample, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("direct-I/O sample error: %v", err)
	}
	if directIOSample.IOEligibleReadBlockIOPS < 99 {
		t.Fatalf("direct block I/O produced %.1f IOPS, want about 100", directIOSample.IOEligibleReadBlockIOPS)
	}
	if decision, reason := manager.makeDecision(directIOSample); decision != "ACTIVATE_LIMITS" {
		t.Fatalf("direct block I/O decision = %s (%s), want ACTIVATE_LIMITS", decision, reason)
	}
}

func TestCollectSystemMetricsKeepsSustainedIORateWhenProcessChurnLowersAggregate(t *testing.T) {
	cfg := config.DefaultConfig()
	collector := &mockMetricsCollector{
		preserveExplicitEnforceableUsage: true,
		allUserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {
				UID:      1000,
				Username: "alice",
				EnforceableUsage: resmanmetrics.ProcessSetMetrics{
					IOWriteBytes: 50_000,
				},
			},
		},
	}
	manager, err := NewManager(cfg, collector, &mockCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	if _, err := manager.collectSystemMetrics(); err != nil {
		t.Fatalf("initial collectSystemMetrics() error: %v", err)
	}

	for cycle, aggregate := range []uint64{40_000, 30_000, 20_000, 10_000} {
		user := collector.allUserMetrics[1000]
		user.EnforceableUsage.IOWriteBytes = aggregate
		user.EnforceableUsage.IODelta = resmanmetrics.ProcessIODelta{WriteBytes: 100}
		manager.prevIOTime = time.Now().Add(-time.Second)

		sample, collectErr := manager.collectSystemMetrics()
		if collectErr != nil {
			t.Fatalf("cycle %d collectSystemMetrics() error: %v", cycle+1, collectErr)
		}
		if sample.IOEligibleWriteBPS <= 0 {
			t.Fatalf(
				"cycle %d I/O rate = %.1f with aggregate lowered to %d, want non-zero sustained rate",
				cycle+1,
				sample.IOEligibleWriteBPS,
				aggregate,
			)
		}
	}
}
