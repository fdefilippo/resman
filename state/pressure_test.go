package state

import (
	"errors"
	"testing"

	"github.com/fdefilippo/resman/config"
)

type processDiagnosticCollector struct {
	mockMetricsCollector
	err   error
	calls int
}

func (c *processDiagnosticCollector) DecisionProcessScanError() error {
	c.calls++
	return c.err
}

func TestDecisionProcessFailureIsAuthoritativeAndObservationCannotConsumeIt(t *testing.T) {
	failure := errors.New("process stat inaccessible")
	collector := &processDiagnosticCollector{err: failure}
	m, err := NewManager(config.DefaultConfig(), collector, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.collectSystemMetrics(); !errors.Is(err, failure) {
		t.Fatalf("authoritative process failure=%v", err)
	}
	if _, err := m.collectSystemMetricsForRefresh(); err != nil {
		t.Fatalf("observation consumed decision diagnostics: %v", err)
	}
	if collector.calls != 1 || !errors.Is(collector.err, failure) {
		t.Fatalf("decision diagnostics changed during refresh: calls=%d err=%v", collector.calls, collector.err)
	}
}

func TestSystemPressureOnlyBelongsToDecisionSample(t *testing.T) {
	cfg := config.DefaultConfig()
	m, err := NewManager(cfg, &mockMetricsCollector{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := m.collectSystemMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if decision.Pressure == nil || !decision.Pressure.At.Equal(decision.Timestamp) {
		t.Fatal("decision pressure timestamp missing")
	}
	observation, err := m.collectSystemMetricsForRefresh()
	if err != nil {
		t.Fatal(err)
	}
	if observation.Pressure != nil {
		t.Fatal("host observation attempted a new decision-owned pressure sample")
	}
	if decision.CPUEligibleCPUUsage != observation.CPUEligibleCPUUsage || decision.RAMEligibleUsageBytes != observation.RAMEligibleUsageBytes {
		t.Fatal("pressure changed attributed decision inputs")
	}
}

func TestMemoryWakeTriggerIsRetainedWithoutAddingUserMemorySource(t *testing.T) {
	if got := boundedHistoryTrigger("psi_system_memory"); got != "psi_system_memory" {
		t.Fatalf("memory trigger=%s", got)
	}
	if got := boundedHistoryTrigger("psi_user_memory"); got != "other" {
		t.Fatalf("unregistered user memory trigger=%s", got)
	}
}
