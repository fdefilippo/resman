package state

import (
	"testing"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/metrics"
)

// publishedRefusal runs one activation cycle on a host that observes for the
// given reason and returns everything the daemon published about it.
func publishedRefusal(t *testing.T, reason string) (map[string]interface{}, RuntimeStatus, cgroup.EnforcementCycleState) {
	t.Helper()
	logger := &failingCompletionLogger{}
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(
		config.DefaultConfig(),
		&mockMetricsCollector{},
		&mockCgroupManager{},
		exporter,
		WithEnforcementStatus(cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeObservationOnly,
			Reason: reason,
		}),
	)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.logger = logger
	run := &controlCycleContext{
		cfg:      config.DefaultConfig(),
		cycleID:  1,
		trigger:  ControlCycleTriggerManual,
		decision: decisionActivate,
		reason:   "test decision",
		metrics: &SystemMetrics{
			CPUEligibleUsers: []int{1000},
			UserMetrics: map[int]*metrics.UserMetrics{
				1000: {EnforceableUsage: metrics.ProcessSetMetrics{ProcessCount: 3}},
			},
		},
	}
	if err := manager.stageExecuteDecision(run); err != nil {
		t.Fatalf("stageExecuteDecision() error: %v", err)
	}
	if err := manager.stageUpdatePrometheus(run); err != nil {
		t.Fatalf("stageUpdatePrometheus() error: %v", err)
	}
	if err := manager.stageLogCompletion(run); err != nil {
		t.Fatalf("stageLogCompletion() error: %v", err)
	}
	return logFieldsByKey(t, logger.fields), manager.GetStatus(),
		exporter.snapshot().lastSystemSnapshot.EnforcementCycleState
}

func TestUnavailableMandatoryCapabilityIsPublishedOnEverySurface(t *testing.T) {
	fields, status, prometheus := publishedRefusal(t, cgroup.EnforcementReasonMandatoryCapabilityUnavailable)

	if fields["enforcement_mode"] != cgroup.EnforcementModeObservationOnly {
		t.Fatalf("logged enforcement_mode = %#v", fields["enforcement_mode"])
	}
	if fields["enforcement_block_reason"] != cgroup.EnforcementBlockReasonMandatoryCapability {
		t.Fatalf("logged enforcement_block_reason = %#v, want %q",
			fields["enforcement_block_reason"], cgroup.EnforcementBlockReasonMandatoryCapability)
	}
	if status.EnforcementReason != cgroup.EnforcementReasonMandatoryCapabilityUnavailable {
		t.Fatalf("MCP enforcement_reason = %q, want %q",
			status.EnforcementReason, cgroup.EnforcementReasonMandatoryCapabilityUnavailable)
	}
	if status.EnforcementBlockReason != cgroup.EnforcementBlockReasonMandatoryCapability {
		t.Fatalf("MCP enforcement_block_reason = %q", status.EnforcementBlockReason)
	}
	if prometheus.BlockReason != cgroup.EnforcementBlockReasonMandatoryCapability {
		t.Fatalf("Prometheus block reason = %q", prometheus.BlockReason)
	}
	// The limit was requested and refused: both halves stay visible, so an
	// operator still sees what the policy wanted on this host.
	if fields["requested_policy_intent"] != cgroup.EnforcementPolicyIntentActivate ||
		fields["applied_enforcement_action"] != cgroup.AppliedEnforcementActionNone {
		t.Fatalf("intent projection = requested=%#v applied=%#v",
			fields["requested_policy_intent"], fields["applied_enforcement_action"])
	}
	if prometheus.RequestedIntent != cgroup.EnforcementPolicyIntentActivate {
		t.Fatalf("Prometheus requested intent = %q, want the refused activation", prometheus.RequestedIntent)
	}
}

func TestDeclaredObservationIsPublishedAsADeclarationNotAFailure(t *testing.T) {
	fields, status, prometheus := publishedRefusal(t, cgroup.EnforcementReasonOperatorObservationOnly)

	if fields["enforcement_block_reason"] != cgroup.EnforcementBlockReasonOperatorObservation {
		t.Fatalf("logged enforcement_block_reason = %#v, want %q",
			fields["enforcement_block_reason"], cgroup.EnforcementBlockReasonOperatorObservation)
	}
	if status.EnforcementReason != cgroup.EnforcementReasonOperatorObservationOnly ||
		status.EnforcementBlockReason != cgroup.EnforcementBlockReasonOperatorObservation {
		t.Fatalf("MCP projection = reason=%q block=%q", status.EnforcementReason, status.EnforcementBlockReason)
	}
	if prometheus.BlockReason != cgroup.EnforcementBlockReasonOperatorObservation {
		t.Fatalf("Prometheus block reason = %q", prometheus.BlockReason)
	}
	if prometheus.BlockReason == cgroup.EnforcementBlockReasonMandatoryCapability {
		t.Fatal("a declared observation was published as a host inability")
	}
}
