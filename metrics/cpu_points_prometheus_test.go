package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
)

func TestCPUPointsPrometheusSystemSnapshotUsesEffectiveParentIntervalAndDeletesStaleValues(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	start := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Second)
	online, quota, period := uint64(4), uint64(360000), uint64(100000)
	siblingWeight, bestEffortWeight := uint64(800), uint64(100)
	parentUsage, observedWeight, rootPoints := uint64(9_000_000), uint64(800), uint64(100)
	periods, throttled, throttledUsec := uint64(300), uint64(190), uint64(4_590_000)

	first := CPUPointsSystemSnapshot{
		SampleEpochID: 1, IntervalStart: &start, IntervalEnd: end,
		ReservePoints: 100, NominalParentPoolPoints: 900, ConfiguredBestEffortPoints: 100,
		CapacityAvailable: true, OnlineCPUs: &online, ProgrammedParentQuotaUsec: &quota, ProgrammedParentPeriodUsec: &period,
		AppliedGuaranteePoints: 600, ProgrammedGuaranteeWeight: 600,
		ProgrammedSiblingWeightSum: &siblingWeight, ProgrammedBestEffortWeight: &bestEffortWeight,
		ParentCPUUsageUsecDelta: &parentUsage, ObservedSiblingWeightSum: &observedWeight,
		ConfiguredRootPoints: &rootPoints, ParentCPUPeriodsDelta: &periods,
		ParentCPUThrottledPeriodsDelta: &throttled, ParentCPUThrottledUsecDelta: &throttledUsec,
		DeliveryState: CPUPointsDeliveryThrottledParent, DenominatorState: CPUPointsDenominatorComplete,
	}
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{CPUPoints: &first})

	want := map[string]float64{
		"resman_cpu_points_reserve": 100, "resman_cpu_points_nominal_parent_pool": 900,
		"resman_cpu_points_best_effort_entitlement": 100, "resman_cpu_points_online_cpus": 4,
		"resman_cpu_points_parent_quota_microseconds": 360000, "resman_cpu_points_parent_period_microseconds": 100000,
		"resman_cpu_points_capacity_available":                  1,
		"resman_cpu_points_reconciliation_degraded":             0,
		"resman_cpu_points_applied_guarantee_total":             600,
		"resman_cpu_points_programmed_guaranteed_weight_sum":    600,
		"resman_cpu_points_programmed_sibling_weight_sum":       800,
		"resman_cpu_points_programmed_best_effort_weight":       100,
		"resman_cpu_points_parent_usage_microseconds_delta":     9_000_000,
		"resman_cpu_points_observed_sibling_weight_sum":         800,
		"resman_cpu_points_root_entitlement":                    100,
		"resman_cpu_points_parent_periods_delta":                300,
		"resman_cpu_points_parent_throttled_periods_delta":      190,
		"resman_cpu_points_parent_throttled_microseconds_delta": 4_590_000,
		"resman_cpu_points_observation_interval_seconds":        30,
	}
	for name, value := range want {
		if got := gatheredMetricValue(t, exporter, name); got != value {
			t.Errorf("%s = %v, want %v", name, got, value)
		}
	}
	if hasMetricFamily(t, exporter, "resman_cpu_action_cores") {
		t.Fatal("removed resman_cpu_action_cores alias remains registered")
	}
	if help := gatheredMetricHelp(t, exporter, "resman_cpu_points_parent_usage_microseconds_delta"); !strings.Contains(help, "allocation denominator") {
		t.Fatalf("effective-parent help = %q", help)
	}
	if help := gatheredMetricHelp(t, exporter, "resman_cpu_points_parent_quota_microseconds"); !strings.Contains(help, "not a delivered guarantee") {
		t.Fatalf("nominal-quota help = %q", help)
	}
	assertGaugeLabelValue(t, exporter, "resman_cpu_points_delivery_state", "state", "throttled_parent", 1)
	assertGaugeLabelValue(t, exporter, "resman_cpu_points_denominator_state", "state", "complete", 1)

	// Observation-only refreshes have no CPU Points pointer and must not erase
	// or advance the decision-owned interval.
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{TotalCPUUsage: 42, TotalCPUUsageAvailable: true})
	if got := gatheredMetricValue(t, exporter, "resman_cpu_points_parent_usage_microseconds_delta"); got != 9_000_000 {
		t.Fatalf("observation refresh replaced CPU Points interval: %v", got)
	}

	unavailableSnapshot := CPUPointsSystemSnapshot{
		ReservePoints: 100, NominalParentPoolPoints: 900, ConfiguredBestEffortPoints: 100,
		DeliveryState: CPUPointsDeliveryUnavailable, DenominatorState: CPUPointsDenominatorUnavailable,
	}
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{CPUPoints: &unavailableSnapshot})
	for _, name := range []string{
		"resman_cpu_points_online_cpus", "resman_cpu_points_parent_quota_microseconds",
		"resman_cpu_points_parent_usage_microseconds_delta", "resman_cpu_points_observation_interval_seconds",
	} {
		if hasMetricFamily(t, exporter, name) {
			t.Errorf("stale optional metric %s remains after unavailable snapshot", name)
		}
	}
	assertGaugeLabelValue(t, exporter, "resman_cpu_points_delivery_state", "state", "unavailable", 1)
}

func TestCPUPointsPrometheusUserSnapshotDistinguishesPartialGuaranteeAndRemovesBestEffortStaleSeries(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	guarantee, weight := uint64(300), uint64(300)
	leafUsage, ramCurrent, high, max, oom, kills := uint64(100), uint64(64<<20), uint64(153), uint64(0), uint64(0), uint64(0)
	ramCoverage := "partial"
	appliedClass := "guaranteed"
	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 1000, Username: "alice", ProcessCount: 3, CPUPoints: CPUPointsUserSnapshot{
		UID: 1000, Username: "alice", ConfiguredClass: "guaranteed", ConfiguredGuaranteePoints: &guarantee,
		CPUEnforcementRequested: true, LifecycleState: CPUPointsLifecycleApplied,
		AppliedClass: &appliedClass, AppliedWeight: &weight, AppliedToProcesses: true,
		CompleteUIDWorkloadGuaranteed: false, ReconciliationDegraded: true, ProcessCoverage: CPUPointsCoveragePartial,
		ObservedProcessCount: 3, EnforceableProcessCount: 2, PIDNamespaceMismatchCount: 1,
		LeafCPUUsageUsecDelta: &leafUsage,
		RAMCgroupUsageBytes:   &ramCurrent, RAMCoverage: &ramCoverage, RAMCoverageIncompleteProcessCount: 1,
		MemoryHighEventsDelta: &high, MemoryMaxEventsDelta: &max, MemoryOOMEventsDelta: &oom, MemoryOOMKillEventsDelta: &kills,
	}})
	want := map[string]float64{
		"resman_user_cpu_points_configured_guarantee":                300,
		"resman_user_cpu_points_enforcement_requested":               1,
		"resman_user_cpu_points_applied_weight":                      300,
		"resman_user_cpu_points_applied_to_processes":                1,
		"resman_user_cpu_points_complete_uid_workload_guaranteed":    0,
		"resman_user_cpu_points_reconciliation_degraded":             1,
		"resman_user_cpu_points_observed_processes":                  3,
		"resman_user_cpu_points_enforceable_processes":               2,
		"resman_user_cpu_points_pid_namespace_mismatch_processes":    1,
		"resman_user_cpu_points_pid_namespace_unavailable_processes": 0,
		"resman_user_cpu_points_leaf_usage_microseconds_delta":       100,
		"resman_user_ram_cgroup_memory_current_bytes":                64 << 20,
		"resman_user_ram_cgroup_coverage_incomplete_processes":       1,
		"resman_user_memory_high_events_delta":                       153,
		"resman_user_memory_max_events_delta":                        0,
		"resman_user_memory_oom_events_delta":                        0,
		"resman_user_memory_oom_kill_events_delta":                   0,
	}
	for name, value := range want {
		if got := gatheredMetricValue(t, exporter, name); got != value {
			t.Errorf("%s = %v, want %v", name, got, value)
		}
	}
	assertGaugeLabelValue(t, exporter, "resman_user_cpu_points_configured_class", "class", "guaranteed", 1)
	assertGaugeLabelValue(t, exporter, "resman_user_cpu_points_lifecycle_state", "state", "applied", 1)
	assertGaugeLabelValue(t, exporter, "resman_user_cpu_points_applied_class", "class", "guaranteed", 1)
	assertGaugeLabelValue(t, exporter, "resman_user_cpu_points_process_coverage", "coverage", "partial", 1)
	assertGaugeLabelValue(t, exporter, "resman_user_ram_cgroup_coverage", "coverage", "partial", 1)
	if help := gatheredMetricHelp(t, exporter, "resman_user_memory_high_events_delta"); !strings.Contains(help, "does not promise a kill") {
		t.Fatalf("memory.high help = %q", help)
	}

	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 1000, Username: "alice", CPUPoints: CPUPointsUserSnapshot{
		UID: 1000, Username: "alice", ConfiguredClass: "best_effort",
		LifecycleState: CPUPointsLifecycleReleased, ProcessCoverage: CPUPointsCoverageNone,
	}})
	for _, name := range []string{
		"resman_user_cpu_points_configured_guarantee", "resman_user_cpu_points_applied_weight",
		"resman_user_cpu_points_leaf_usage_microseconds_delta", "resman_user_ram_cgroup_memory_current_bytes",
		"resman_user_memory_high_events_delta",
	} {
		if hasMetricFamily(t, exporter, name) {
			t.Errorf("best-effort/released snapshot retained stale series %s", name)
		}
	}
	assertGaugeLabelValue(t, exporter, "resman_user_cpu_points_configured_class", "class", "best_effort", 1)
	assertGaugeLabelValue(t, exporter, "resman_user_cpu_points_lifecycle_state", "state", "released", 1)
	for _, stale := range []struct {
		name, label, value string
	}{
		{"resman_user_cpu_points_configured_class", "class", "guaranteed"},
		{"resman_user_cpu_points_applied_class", "class", "guaranteed"},
		{"resman_user_cpu_points_lifecycle_state", "state", "applied"},
		{"resman_user_cpu_points_process_coverage", "coverage", "partial"},
		{"resman_user_ram_cgroup_coverage", "coverage", "partial"},
	} {
		if hasGaugeLabelValue(t, exporter, stale.name, stale.label, stale.value) {
			t.Errorf("state transition retained stale %s{%s=%q}", stale.name, stale.label, stale.value)
		}
	}
}

func TestCPUPointsPrometheusCleanupRemovesEveryInactiveUserSeries(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	guarantee, weight, delta, ramCurrent := uint64(300), uint64(300), uint64(10), uint64(32<<20)
	class, ramCoverage := "guaranteed", "complete"
	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 1000, Username: "alice", CPUPoints: CPUPointsUserSnapshot{
		UID: 1000, Username: "alice", ConfiguredClass: class, ConfiguredGuaranteePoints: &guarantee,
		CPUEnforcementRequested: true, LifecycleState: CPUPointsLifecycleApplied,
		AppliedClass: &class, AppliedWeight: &weight, AppliedToProcesses: true,
		CompleteUIDWorkloadGuaranteed: true, ProcessCoverage: CPUPointsCoverageComplete,
		ObservedProcessCount: 2, EnforceableProcessCount: 2, LeafCPUUsageUsecDelta: &delta,
		RAMCgroupUsageBytes: &ramCurrent, RAMCoverage: &ramCoverage,
		MemoryHighEventsDelta: &delta, MemoryMaxEventsDelta: &delta,
		MemoryOOMEventsDelta: &delta, MemoryOOMKillEventsDelta: &delta,
	}})

	exporter.CleanupUserMetrics(map[int]bool{})
	for _, name := range []string{
		"resman_user_cpu_points_configured_class",
		"resman_user_cpu_points_configured_guarantee",
		"resman_user_cpu_points_enforcement_requested",
		"resman_user_cpu_points_lifecycle_state",
		"resman_user_cpu_points_applied_class",
		"resman_user_cpu_points_applied_weight",
		"resman_user_cpu_points_applied_to_processes",
		"resman_user_cpu_points_complete_uid_workload_guaranteed",
		"resman_user_cpu_points_reconciliation_degraded",
		"resman_user_cpu_points_process_coverage",
		"resman_user_cpu_points_observed_processes",
		"resman_user_cpu_points_enforceable_processes",
		"resman_user_cpu_points_pid_namespace_mismatch_processes",
		"resman_user_cpu_points_pid_namespace_unavailable_processes",
		"resman_user_cpu_points_leaf_usage_microseconds_delta",
		"resman_user_ram_cgroup_memory_current_bytes",
		"resman_user_ram_cgroup_coverage",
		"resman_user_ram_cgroup_coverage_incomplete_processes",
		"resman_user_memory_high_events_delta",
		"resman_user_memory_max_events_delta",
		"resman_user_memory_oom_events_delta",
		"resman_user_memory_oom_kill_events_delta",
	} {
		if hasMetricFamily(t, exporter, name) {
			t.Errorf("inactive-user cleanup retained %s", name)
		}
	}
}

func TestCPUPointsPrometheusRejectsUnboundedSnapshotLabels(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	invalidClass := "uid-1000-dynamic"
	invalidCoverage := "pid-42-missing"
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{CPUPoints: &CPUPointsSystemSnapshot{
		DeliveryState:    CPUPointsDeliveryState("kernel-error-text"),
		DenominatorState: CPUPointsDenominatorState("user-derived-runnable-points"),
	}})
	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 1000, Username: "alice", CPUPoints: CPUPointsUserSnapshot{
		ConfiguredClass: invalidClass, LifecycleState: CPUPointsLifecycleState("pid-42-failed"),
		AppliedClass: &invalidClass, AppliedToProcesses: true,
		ProcessCoverage: CPUPointsProcessCoverage(invalidCoverage), RAMCoverage: &invalidCoverage,
	}})

	for _, check := range []struct {
		name, label, value string
	}{
		{"resman_cpu_points_delivery_state", "state", "kernel-error-text"},
		{"resman_cpu_points_denominator_state", "state", "user-derived-runnable-points"},
		{"resman_user_cpu_points_configured_class", "class", invalidClass},
		{"resman_user_cpu_points_applied_class", "class", invalidClass},
		{"resman_user_cpu_points_lifecycle_state", "state", "pid-42-failed"},
		{"resman_user_cpu_points_process_coverage", "coverage", invalidCoverage},
		{"resman_user_ram_cgroup_coverage", "coverage", invalidCoverage},
	} {
		if hasGaugeLabelValue(t, exporter, check.name, check.label, check.value) {
			t.Errorf("unbounded label escaped into %s{%s=%q}", check.name, check.label, check.value)
		}
	}
}

func newCPUPointsTestExporter(t *testing.T) *PrometheusExporter {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}
	return exporter
}

func hasMetricFamily(t *testing.T, exporter *PrometheusExporter, name string) bool {
	t.Helper()
	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return true
		}
	}
	return false
}

func assertGaugeLabelValue(t *testing.T, exporter *PrometheusExporter, name, labelName, labelValue string, want float64) {
	t.Helper()
	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					if got := metric.GetGauge().GetValue(); got != want {
						t.Fatalf("%s{%s=%q} = %v, want %v", name, labelName, labelValue, got, want)
					}
					return
				}
			}
		}
	}
	t.Fatalf("metric %s{%s=%q} not found", name, labelName, labelValue)
}

func hasGaugeLabelValue(t *testing.T, exporter *PrometheusExporter, name, labelName, labelValue string) bool {
	t.Helper()
	families, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					return true
				}
			}
		}
	}
	return false
}
