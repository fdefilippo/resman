package mcp

import (
	"context"
	"encoding/json"
	"strings"
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
	if payload.CPUUsage != 25 || payload.CPUPoints != nil || payload.CgroupMemoryCurrentBytes != 0 {
		t.Fatalf("invented native observation before the first interval: %+v", payload)
	}
	_, _, err = server.handleGetCgroupInfo(context.Background(), nil, GetCgroupInfoArgs{UID: 1000})
	if err == nil || !strings.Contains(err.Error(), "get_limits_status") {
		t.Fatalf("legacy inspection did not explain its native replacement: %v", err)
	}
}
