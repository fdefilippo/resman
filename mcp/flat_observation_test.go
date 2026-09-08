package mcp

import (
	"encoding/json"
	"testing"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/systemdunit"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

func TestSliceOnlyWireObservationKeepsProcessValuesNull(t *testing.T) {
	for _, sample := range []any{
		newUserHistoryRecord(database.UserMetricsRecord{UID: 0, ProcessObservationUnavailable: true}),
		newCPUPointsUserPayload(resmanmetrics.CPUPointsUserSnapshot{UID: 0, ProcessObservationUnavailable: true}),
	} {
		data, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"cpu_usage", "memory_usage", "process_count", "enforceable_process_count", "observed_process_count"} {
			if value, exists := fields[key]; exists && value != nil {
				t.Fatalf("unavailable %s exported as %v: %s", key, value, data)
			}
		}
	}
}

func TestProcessOnlyHistoryWireKeepsSliceValuesNull(t *testing.T) {
	payload := newUserHistoryRecord(database.UserMetricsRecord{UID: 1002, Username: "service", CPUUsagePercent: 40, MemoryUsageBytes: 32 << 20, ProcessCount: 3, EligibleForCPU: true, CPUPointsLifecycleState: "eligible_inactive"})
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["cpu_usage"] != float64(40) || fields["memory_usage"] != float64(32<<20) || fields["process_count"] != float64(3) || fields["eligible_for_cpu"] != true {
		t.Fatalf("observed process lost on wire: %s", data)
	}
	for _, key := range []string{"cgroup_path", "cpu_quota", "applied_cpu_class", "applied_cpu_weight", "cpu_weight", "leaf_cpu_usage_usec_delta", "ram_cgroup_usage_bytes", "memory_high_limit", "memory_max_limit"} {
		if value, exists := fields[key]; !exists || value != nil {
			t.Fatalf("missing or fabricated %s: %s", key, data)
		}
	}
}

type unusedNativeAccountingAdapter struct{ state.SystemdCPUUnitAdapter }

func (unusedNativeAccountingAdapter) OwnedUnits() []systemdunit.UnitIdentity { return nil }

func TestNativeMCPDoesNotReadLegacyCgroupPaths(t *testing.T) {
	manager, err := state.NewManager(config.DefaultConfig(), nil, nil, nil, state.WithSystemdCPUEnforcement(unusedNativeAccountingAdapter{}))
	if err != nil {
		t.Fatal(err)
	}
	if manager.GetStatus().EnforcementMode != cgroup.EnforcementModeSystemdNative {
		t.Fatal("fixture did not select native enforcement")
	}
	// A nil legacy reader makes any accidental call fail immediately.
	server := &Server{stateManager: manager}
	payload := server.newUserMetricPayload(1000, &resmanmetrics.UserMetrics{UID: 1000, Username: "alice", CPUUsage: 25})
	if payload.CPUUsage != 25 || payload.CPUPoints != nil {
		t.Fatalf("invented native observation before the first interval: %+v", payload)
	}
}
