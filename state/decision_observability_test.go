package state

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/metrics"
)

func TestCPUDecisionEvidenceIsNormalizedAndObservationNeutral(t *testing.T) {
	for _, tt := range []struct {
		name               string
		cores              int
		requested, applied bool
		mode               cgroup.EnforcementMode
	}{
		{"native incomplete", 4, true, false, cgroup.EnforcementModeSystemdNative},
		{"CPU verified independently of resources", 4, true, true, cgroup.EnforcementModeSystemdNative},
		{"duration or attribution suppresses intent", 4, false, false, cgroup.EnforcementModeSystemdNative},
		{"unknown core count", 0, false, false, cgroup.EnforcementModeSystemdNative},
		{"declared observation", 4, false, false, cgroup.EnforcementModeObservationOnly},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exporter := &mockPrometheusExporter{}
			manager, err := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
			if err != nil {
				t.Fatal(err)
			}
			manager.enforcementStatus.Mode = tt.mode
			manager.systemdCPURequested, manager.systemdCPUComplete = tt.requested, tt.applied
			at := time.Now().Add(-time.Minute)
			run := &controlCycleContext{decision: decisionMaintain, metrics: &SystemMetrics{Timestamp: at, TotalCores: tt.cores, CPUEligibleCPUUsage: 100}}
			if err := manager.stageUpdatePrometheus(run); err != nil {
				t.Fatal(err)
			}
			got := exporter.snapshot().lastSystemSnapshot.CPUDecision
			if got == nil || got.At != at || got.ActivationRequested != tt.requested || got.ActivationApplied != tt.applied {
				t.Fatalf("decision=%+v", got)
			}
			if got.HostPercentAvailable != (tt.cores > 0) || (tt.cores > 0 && got.HostPercent != 25) {
				t.Fatalf("host percent=%+v", got)
			}
			if err := manager.RunMetricsRefresh(t.Context(), "test"); err != nil {
				t.Fatal(err)
			}
			if exporter.snapshot().lastSystemSnapshot.CPUDecision != nil {
				t.Fatal("observation renewed decision evidence")
			}
		})
	}
}

func TestCPUActivationDiagnosticsFollowDecisionGuards(t *testing.T) {
	for _, tt := range []struct {
		name               string
		eligible, observed float64
		duration           int
		underLoad          bool
		host               float64
		activate           bool
	}{
		{name: "excluded busy population", observed: 350},
		{name: "one busy core is 25 percent", eligible: 100, observed: 100},
		{name: "eligible activation", eligible: 350, observed: 350, activate: true},
		{name: "duration not elapsed", eligible: 350, observed: 350, duration: 90},
		{name: "load attributed externally", eligible: 100, observed: 350, underLoad: true, host: 100},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.CPUThresholdDuration = tt.duration
			cfg.RAMEnabled = false
			cfg.IOEnabled = false
			exporter := &mockPrometheusExporter{}
			manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
			if err != nil {
				t.Fatal(err)
			}
			sample := &SystemMetrics{Timestamp: time.Now(), TotalCores: 4, CPUEligibleCPUUsage: tt.eligible, AllUsersCPUUsage: tt.observed, SystemUnderLoad: tt.underLoad, TotalCPUUsage: tt.host, HostCPUUsageAvailable: true, CPUEligibleUsers: []int{1000}, UserMetrics: map[int]*metrics.UserMetrics{}}
			run := &controlCycleContext{cfg: cfg, metrics: sample}
			if err := manager.stageMakeDecision(run); err != nil {
				t.Fatal(err)
			}
			if err := manager.stageExecuteDecision(run); err != nil {
				t.Fatal(err)
			}
			if err := manager.stageUpdatePrometheus(run); err != nil {
				t.Fatal(err)
			}
			got := exporter.snapshot().lastSystemSnapshot.CPUDecision
			if got.ActivationRequested != tt.activate || got.ActivationApplied {
				t.Fatalf("decision=%s reason=%s evidence=%+v", run.decision, run.reason, got)
			}
		})
	}
}

func TestReadinessRequiresFreshAttemptsIncludingBlackout(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Second)
	old := now.Add(-time.Hour)
	for _, tt := range []struct {
		name     string
		snapshot CycleReadiness
		want     bool
	}{
		{"fresh core", CycleReadiness{CheckedAt: fresh, DecisionAt: fresh, CoreAvailable: true}, true},
		{"initial sample missing", CycleReadiness{CheckedAt: fresh}, false},
		{"core failed", CycleReadiness{CheckedAt: fresh, DecisionAt: fresh}, false},
		{"stale sample", CycleReadiness{CheckedAt: fresh, DecisionAt: old, CoreAvailable: true}, false},
		{"fresh blackout", CycleReadiness{CheckedAt: fresh, Suspended: true}, true},
		{"stalled blackout", CycleReadiness{CheckedAt: old, Suspended: true}, false},
		{"future timestamp", CycleReadiness{CheckedAt: now.Add(time.Hour), Suspended: true}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.snapshot.AvailableAt(now, 30*time.Second); got != tt.want {
				t.Fatalf("available=%v want=%v", got, tt.want)
			}
		})
	}
}
