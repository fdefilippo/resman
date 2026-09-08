package state

import (
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
	placements []string
	statsReads int
}

func TestObservationOnlyBlockIOAccountingDoesNotCreatePlacement(t *testing.T) {
	cfg := ioDecisionConfig()
	cfg.IOReadIOPS = 1000
	collector := &mockMetricsCollector{
		preserveExplicitEnforceableUsage: true,
		allUserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {UID: 1000, Username: "alice", EligibleForIO: true},
		},
	}
	cgroups := &blockIOSequenceCgroupManager{}
	manager, err := NewManager(
		cfg,
		collector,
		cgroups,
		&mockPrometheusExporter{},
		WithEnforcementStatus(cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeObservationOnly,
			Reason: cgroup.EnforcementReasonSystemdOwnsHostWorkloads,
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sample, err := manager.collectSystemMetrics()
	if err != nil {
		t.Fatalf("collectSystemMetrics() error: %v", err)
	}
	if len(cgroups.placements) != 0 || cgroups.statsReads != 0 {
		t.Fatalf("observation-only I/O accounting placements=%v reads=%d", cgroups.placements, cgroups.statsReads)
	}
	if sample.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatalf("unavailable block-I/O users = %d, want 1", sample.IOBlockIOPSUnavailableUsers)
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
