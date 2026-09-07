package app

import (
	"context"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

type blackoutCollector struct {
	state.MetricsCollector
	observations, decisions int
}

func (*blackoutCollector) GetTotalCores() int              { return 4 }
func (*blackoutCollector) GetMemoryUsage() float64         { return 64 }
func (*blackoutCollector) GetTotalMemoryMB() float64       { return 4096 }
func (*blackoutCollector) GetCachedMemoryMB() float64      { return 0 }
func (*blackoutCollector) IsSystemUnderLoad() bool         { return true }
func (*blackoutCollector) GetSystemLoad() (float64, error) { return 2, nil }
func (c *blackoutCollector) GetObservationHostCPUUsage() metrics.HostCPUUsageSample {
	c.observations++
	return metrics.HostCPUUsageSample{Available: true, UsagePercent: float64(c.observations * 10)}
}
func (*blackoutCollector) GetAllUserMetrics() map[int]*metrics.UserMetrics {
	return map[int]*metrics.UserMetrics{1006: {UID: 1006, Username: "alice", CPUUsage: 300, ProcessCount: 6}}
}
func (c *blackoutCollector) GetDecisionHostCPUUsage() metrics.HostCPUUsageSample {
	c.decisions++
	return metrics.HostCPUUsageSample{}
}
func (c *blackoutCollector) GetAllUserMetricsForDecision() map[int]*metrics.UserMetrics {
	c.decisions++
	return nil
}

type blackoutCgroups struct{ shutdownCgroupManager }

func (*blackoutCgroups) EnforcementStatus() cgroup.EnforcementStatus {
	return cgroup.EnforcementStatus{Mode: cgroup.EnforcementModeObservationOnlySystemd}
}

type blackoutExporter struct {
	state.PrometheusExporter
	samples    chan metrics.SystemExporterMetrics
	userWrites int
}

func (e *blackoutExporter) UpdateSystemSnapshot(s metrics.SystemExporterMetrics)    { e.samples <- s }
func (e *blackoutExporter) UpdateUserSnapshot(metrics.UserExporterMetrics)          { e.userWrites++ }
func (*blackoutExporter) RecordControlCycleTrigger(string)                          {}
func (*blackoutExporter) RecordControlCycleDuration(time.Duration)                  {}
func (*blackoutExporter) RecordMetricsCollectionDuration(time.Duration)             {}
func (*blackoutExporter) ObserveObservationHostCPUUsage(metrics.HostCPUUsageSample) {}
func (*blackoutExporter) Stop() error                                               { return nil }
func (*blackoutExporter) ObserveLimitHookExecutor(int, int, int)                    {}
func (*blackoutExporter) RecordProcessRestoreResult(cgroup.ProcessRestoreResult)    {}

func TestApplicationPollingBlackoutSchedulesIndependentObservation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.BlackoutSpec = "* 00-24"
	cfg.PSIEventDriven = false
	cfg.PollingInterval = 1
	cfg.MetricsRefreshInterval = 1
	var err error
	cfg.BlackoutTimeframes, err = config.ParseTimeframe(cfg.BlackoutSpec)
	if err != nil {
		t.Fatal(err)
	}
	collector := &blackoutCollector{}
	exporter := &blackoutExporter{samples: make(chan metrics.SystemExporterMetrics, 8)}
	manager, err := state.NewManager(cfg, collector, &blackoutCgroups{}, exporter,
		state.WithEnforcementStatus(cgroup.EnforcementStatus{Mode: cgroup.EnforcementModeObservationOnlySystemd}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &App{cfg: cfg, ctx: ctx, logger: &shutdownLogger{}, stateManager: manager,
		prometheusExporter: &metrics.PrometheusExporter{}}
	done := make(chan error, 1)
	go func() { done <- a.runControlLoop() }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("application loop did not stop")
		}
		if collector.decisions != 0 || exporter.userWrites != 0 {
			t.Errorf("blackout advanced decision sampling or decision-owned user series: %d/%d", collector.decisions, exporter.userWrites)
		}
	}()
	for i := 1; i <= 2; i++ {
		select {
		case sample := <-exporter.samples:
			if !sample.TotalCPUUsageAvailable || sample.TotalCPUUsage != float64(i*10) || sample.SystemLoad != 2 ||
				sample.EnforcementMode != cgroup.EnforcementModeObservationOnlySystemd || sample.AnyLimitsActive {
				t.Fatalf("stale or fabricated blackout observation: %+v", sample)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("application scheduler did not publish blackout observation")
		}
	}
}
