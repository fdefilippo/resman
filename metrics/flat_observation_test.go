package metrics

import "testing"

func TestFlatSliceWithoutProcessSampleDoesNotPublishZeroUIDUsage(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	weight := uint64(3300)
	class := "root"
	coverage := "complete"
	snapshot := CPUPointsUserSnapshot{UID: 0, Username: "root", ConfiguredClass: class, LifecycleState: CPUPointsLifecycleApplied, AppliedClass: &class, AppliedWeight: &weight, ObservedWeight: &weight, AppliedToProcesses: true, ProcessCoverage: CPUPointsCoverageComplete, IOCoverage: &coverage}
	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 0, Username: "root", CPUUsagePercent: 30, CPUPoints: snapshot})
	snapshot.ProcessObservationUnavailable = true
	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 0, Username: "root", CPUPoints: snapshot})
	for _, name := range []string{"resman_user_cpu_usage_percent", "resman_user_memory_usage_bytes", "resman_user_process_count", "resman_user_cpu_points_observed_processes", "resman_user_cpu_points_enforceable_processes", "resman_user_io_read_bytes_total", "resman_user_io_write_bytes_total", "resman_user_cpu_limit_active"} {
		if hasMetricFamily(t, exporter, name) {
			t.Fatalf("unavailable process sample exported as zero: %s", name)
		}
	}
	if got := gatheredMetricValue(t, exporter, "resman_user_cpu_points_observed_weight"); got != 3300 {
		t.Fatalf("slice weight disappeared: %v", got)
	}
	snapshot.AppliedToProcesses = false
	if got := gatheredMetricValue(t, exporter, "resman_user_cpu_points_programmed_weight"); got != 3300 {
		t.Fatalf("programmed slice weight disappeared: %v", got)
	}
	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 0, Username: "root", CPUPoints: snapshot})
	if hasMetricFamily(t, exporter, "resman_user_cpu_points_applied_weight") || !hasMetricFamily(t, exporter, "resman_user_cpu_points_programmed_weight") {
		t.Fatal("programmed and currently verified weights were conflated")
	}
	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 0, Username: "root"})
	if hasMetricFamily(t, exporter, "resman_user_cpu_points_observed_weight") || hasMetricFamily(t, exporter, "resman_user_io_coverage") || hasMetricFamily(t, exporter, "resman_user_cpu_points_programmed_weight") {
		t.Fatal("departed slice retained native observations")
	}
	exporter.UpdateUserSnapshot(UserExporterMetrics{UID: 0, Username: "root", CPUPoints: snapshot})
	exporter.CleanupUserMetrics(map[int]bool{})
	if hasMetricFamily(t, exporter, "resman_user_cpu_points_observed_weight") || hasMetricFamily(t, exporter, "resman_user_io_coverage") || hasMetricFamily(t, exporter, "resman_user_cpu_points_programmed_weight") {
		t.Fatal("cleanup retained native observations")
	}
}
