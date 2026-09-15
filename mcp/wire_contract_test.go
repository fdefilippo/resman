package mcp

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

func TestMCPWireDTOJSONContracts(t *testing.T) {
	matchCount := 1
	tests := []struct {
		name  string
		value any
		keys  []string
	}{
		{
			name: "system status",
			value: newSystemStatusPayload("host", "role", resmanmetrics.ObservationMetrics{}, state.RuntimeStatus{
				EnforcementMode: cgroup.EnforcementModeObservationOnly,
			}),
			keys: []string{
				"actively_limited_users_count", "any_limits_active", "cpu_limits_active", "cpu_limits_applied_time",
				"applied_enforcement_action", "cpu_points", "enforcement_block_reason", "enforcement_mode", "enforcement_reason", "hostname", "memory_usage_mb",
				"observed_users_count", "observed_users_cpu_usage",
				"io_device_weight",
				"requested_policy_intent",
				"resource_limits_active", "resource_limits_applied_time", "server_role",
				"system_under_load", "total_cores", "total_cpu_usage", "total_cpu_usage_available",
				"total_cpu_usage_unavailable_reason",
			},
		},
		{
			name: "limits status",
			value: newLimitsStatusPayload("host", "role", state.RuntimeStatus{
				EnforcementMode: cgroup.EnforcementModeObservationOnly,
			}),
			keys: []string{
				"actively_limited_users", "actively_limited_users_count", "any_limits_active", "cpu_actively_limited_users",
				"cpu_actively_limited_users_count", "cpu_limits_active", "cpu_limits_applied_time", "cpu_point_users",
				"applied_enforcement_action", "cpu_points", "enforcement_block_reason", "enforcement_mode", "enforcement_reason", "hostname",
				"io_device_weight",
				"requested_policy_intent",
				"resource_limits_active", "resource_limits_applied_time", "server_role",
			},
		},
		{
			name:  "active users",
			value: activeUsersPayload{Users: []activeUserPayload{{UID: 1000, Username: "alice"}}},
			keys:  []string{"hostname", "server_role", "users"},
		},
		{
			name:  "user metric",
			value: UserMetric{},
			keys: []string{
				"cpu_limit_active", "cpu_limit_requested", "cpu_usage", "eligible_for_cpu",
				"eligible_for_io", "eligible_for_ram", "io_limit_active", "io_limit_requested", "memory_usage",
				"process_count", "ram_limit_active", "ram_limit_requested", "uid", "username",
			},
		},
		{
			name:  "resource policy configuration",
			value: newResourcePolicyConfigurationPayload("host", config.DefaultConfig()),
			keys: []string{
				"cpu_best_effort_points", "cpu_points_file", "cpu_release_threshold", "cpu_reserve_points", "cpu_threshold", "cpu_threshold_duration",
				"disable_swap", "enable_prometheus", "hostname", "ignore_system_load", "io_device_filter",
				"io_default_weight", "io_root_weight", "io_user_weight_file", "io_weight_devices",
				"io_enabled", "io_read_bps", "io_read_iops", "io_release_threshold", "io_threshold",
				"io_threshold_duration", "io_write_bps", "io_write_iops", "polling_interval",
				"prometheus_port", "ram_enabled", "ram_high_ratio", "ram_quota_per_user", "ram_release_threshold",
				"ram_threshold", "server_role", "system_uid_max", "system_uid_min",
			},
		},
		{
			name:  "CPU report",
			value: cpuReportPayload{},
			keys: []string{
				"avg_cpu", "cpu_actively_limited_users_count", "cpu_limits_active", "cpu_points", "hostname",
				"observed_users_count", "peak_cpu", "report", "server_role", "total_cpu", "total_cpu_available",
				"total_cpu_unavailable_reason",
			},
		},
		{
			name:  "memory report",
			value: memoryReportPayload{},
			keys:  []string{"avg_memory_mb", "hostname", "observed_users_count", "peak_memory_mb", "ram_actively_limited_users_count", "report", "resource_limits_active", "server_role", "total_memory_mb"},
		},
		{
			name:  "limit action",
			value: limitActionResult{},
			keys:  []string{"message", "success"},
		},
		{
			name:  "user filters",
			value: userFiltersPayload{},
			keys:  []string{"config_file", "user_exclude_list", "user_include_list"},
		},
		{
			name: "valid user filter",
			value: func() validateUserFilterResult {
				matches := []string{"alice"}
				return validateUserFilterResult{Valid: true, Pattern: ".*", Type: "include", TestMatches: &matches, MatchCount: &matchCount}
			}(),
			keys: []string{"match_count", "pattern", "test_matches", "type", "valid"},
		},
		{
			name:  "invalid user filter",
			value: validateUserFilterResult{Error: "invalid"},
			keys:  []string{"error", "valid"},
		},
		{
			name:  "user history",
			value: getUserHistoryResult{Records: []userHistoryRecord{{}}},
			keys:  []string{"count", "end_time", "records", "start_time"},
		},
		{
			name:  "system history",
			value: getSystemHistoryResult{Records: []systemHistoryRecord{{}}},
			keys:  []string{"count", "end_time", "records", "start_time"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertExactJSONKeys(t, tt.value, tt.keys)
		})
	}
	assertExactJSONKeys(t, newIODeviceWeightPayload(state.IODeviceWeightStatus{}), []string{
		"authority_coverage", "classification_attempts", "complete_users", "effect_qualification_provenance", "effect_qualified", "functionally_accepted",
		"mechanism", "observed_delivery", "partial_users", "probe_attempts", "programmed", "programmed_state",
		"read_back", "read_back_state", "reason", "selector", "sibling_slices", "state", "total_points",
		"unavailable_users", "values",
	})
	assertExactJSONKeys(t, ioDeviceWeightValuePayload{}, []string{
		"class", "coverage", "device", "mechanism", "nominal_share", "programmed", "read_back", "requested_value", "uid",
	})

	assertExactNestedJSONKeys(t, getUserHistoryResult{Records: []userHistoryRecord{{}}}, "records", []string{
		"cpu_authority_coverage", "io_coverage",
		"applied_cpu_class", "applied_cpu_weight", "cgroup_path", "configured_cpu_class", "configured_guarantee_points",
		"cpu_limit_active", "cpu_limit_requested", "cpu_points_lifecycle_state", "cpu_quota", "cpu_usage", "cpu_weight",
		"eligible_for_cpu", "eligible_for_io", "eligible_for_ram", "enforceable_process_count", "interval_end", "interval_start",
		"io_limit_active", "io_limit_requested", "leaf_cpu_usage_usec_delta", "memory_high_events_delta", "memory_high_limit",
		"memory_max_events_delta", "memory_max_limit", "memory_oom_events_delta",
		"memory_oom_kill_events_delta", "memory_swap_max", "memory_usage", "process_count", "ram_cgroup_usage_bytes", "ram_coverage",
		"ram_coverage_incomplete_process_count", "ram_limit_active", "ram_limit_requested", "ram_swap_disabled",
		"sample_epoch_id", "timestamp", "uid", "username",
	})
	assertExactNestedJSONKeys(t, getSystemHistoryResult{Records: []systemHistoryRecord{{}}}, "records", []string{
		"denominator_state", "enforcement_mode",
		"io_device_weight_state", "io_device_weight_reason", "io_device_weight_selector",
		"io_device_weight_mechanism",
		"io_device_weight_classification_attempts", "io_device_weight_probe_attempts",
		"io_device_weight_programmed", "io_device_weight_programmed_state", "io_device_weight_read_back", "io_device_weight_read_back_state",
		"io_device_weight_functionally_accepted", "io_device_weight_effect_qualification_provenance", "io_device_weight_effect_qualified",
		"io_device_weight_authority_coverage", "io_device_weight_complete_users", "io_device_weight_partial_users",
		"io_device_weight_unavailable_users", "io_device_weight_sibling_slices", "io_device_weight_total_points",
		"io_device_weight_requested_at", "io_device_weight_next_retry_at", "io_device_weight_values",
		"io_device_weight_observed_delivery",
		"actively_limited_users_count", "any_limits_active", "applied_guarantee_points", "configured_root_points",
		"programmed_best_effort_weight", "configured_best_effort_points", "cpu_actively_limited_users_count", "cpu_capacity_available",
		"cpu_limits_active", "cpu_points_degraded", "observed_sibling_weight_sum", "programmed_sibling_weight_sum",
		"interval_end", "interval_start", "nominal_parent_pool_points", "online_cpus", "parent_cpu_periods_delta",
		"parent_cpu_quota", "parent_cpu_throttled_periods_delta", "parent_cpu_throttled_usec_delta", "parent_cpu_usage_usec_delta",
		"programmed_guarantee_weight", "programmed_parent_period_usec", "programmed_parent_quota_usec", "resource_limits_active",
		"sample_epoch_id", "system_load", "timestamp", "total_cores", "total_cpu_usage",
	})
	assertExactNestedJSONKeys(t, activeUsersPayload{Users: []activeUserPayload{{UID: 1000, Username: "alice"}}}, "users", []string{"uid", "username"})
}

func TestCPUPointsWireContractKeepsPolicyDeliveryAndLifecycleStatesDistinct(t *testing.T) {
	start := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Second)
	online, quota, period := uint64(4), uint64(360000), uint64(100000)
	parent, guaranteed, bestEffort := uint64(9000), uint64(0), uint64(9000)
	guaranteedWeight, bestEffortWeight := uint64(300), uint64(100)
	periods, throttled, throttledUsec := uint64(30), uint64(4), uint64(500)
	system := newCPUPointsSystemPayload(resmanmetrics.CPUPointsSystemSnapshot{
		SampleEpochID: 1, IntervalStart: &start, IntervalEnd: end,
		ReservePoints: 100, NominalParentPoolPoints: 900, ConfiguredBestEffortPoints: 100,
		CapacityAvailable: true, OnlineCPUs: &online, ProgrammedParentQuotaUsec: &quota,
		ProgrammedParentPeriodUsec: &period, ParentCPUUsageUsecDelta: &parent,
		ObservedSiblingWeightSum: &guaranteed, ConfiguredRootPoints: &bestEffort,
		ProgrammedSiblingWeightSum: &guaranteedWeight, ProgrammedBestEffortWeight: &bestEffortWeight,
		ParentCPUPeriodsDelta: &periods, ParentCPUThrottledPeriodsDelta: &throttled, ParentCPUThrottledUsecDelta: &throttledUsec,
		DeliveryState:    resmanmetrics.CPUPointsDeliveryThrottledParent,
		DenominatorState: resmanmetrics.CPUPointsDenominatorComplete,
	})
	assertExactJSONKeys(t, system, []string{
		"enforcement_mode",
		"applied_guarantee_points", "configured_root_points", "programmed_best_effort_weight",
		"capacity_available", "configured_best_effort_points", "delivery_state", "observed_sibling_weight_sum",
		"programmed_sibling_weight_sum", "interval_end", "interval_start", "denominator_state", "nominal_parent_pool_points",
		"online_cpus", "parent_cpu_periods_delta", "parent_cpu_throttled_periods_delta", "parent_cpu_throttled_usec_delta",
		"parent_cpu_usage_usec_delta", "programmed_guarantee_weight", "programmed_parent_period_usec",
		"programmed_parent_quota_usec", "reconciliation_degraded", "reserve_points", "sample_epoch_id",
	})
	encoded, err := json.Marshal(system)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, fragment := range []string{`"delivery_state":"throttled_parent"`, `"denominator_state":"complete"`, `"online_cpus":4`} {
		if !strings.Contains(text, fragment) {
			t.Errorf("system CPU Points payload %s lacks %s", text, fragment)
		}
	}
	for _, forbidden := range []string{"default_points", "ceiling", "action_cores", "delivered_guarantee_points"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("system CPU Points payload contains removed or misleading field %q: %s", forbidden, text)
		}
	}

	guarantee, weight := uint64(300), uint64(300)
	leafUsage, ramCurrent, high, max, oom, kill := uint64(100), uint64(64<<20), uint64(4), uint64(0), uint64(0), uint64(0)
	ramCoverage, memoryHigh, memoryMax, memorySwap := "partial", "16777216", "50331648", "0"
	swapDisabled := true
	class := "guaranteed"
	users := []cpuPointsUserPayload{
		newCPUPointsUserPayload(resmanmetrics.CPUPointsUserSnapshot{
			UID: 1000, Username: "alice", ConfiguredClass: "guaranteed", ConfiguredGuaranteePoints: &guarantee,
			LifecycleState: resmanmetrics.CPUPointsLifecycleApplied, AppliedClass: &class, AppliedWeight: &weight,
			AppliedToProcesses: true, ProcessCoverage: resmanmetrics.CPUPointsCoveragePartial,
			ObservedProcessCount: 3, EnforceableProcessCount: 2,
			LeafCPUUsageUsecDelta: &leafUsage, RAMCgroupUsageBytes: &ramCurrent, RAMCoverage: &ramCoverage,
			RAMCoverageIncompleteProcessCount: 1, RAMSwapDisabled: &swapDisabled,
			MemoryHighLimit: &memoryHigh, MemoryMaxLimit: &memoryMax, MemorySwapMax: &memorySwap,
			MemoryHighEventsDelta: &high, MemoryMaxEventsDelta: &max, MemoryOOMEventsDelta: &oom, MemoryOOMKillEventsDelta: &kill,
		}),
		newCPUPointsUserPayload(resmanmetrics.CPUPointsUserSnapshot{UID: 1001, Username: "bob", ConfiguredClass: "best_effort", LifecycleState: resmanmetrics.CPUPointsLifecycleFailed, ProcessCoverage: resmanmetrics.CPUPointsCoverageNone}),
		newCPUPointsUserPayload(resmanmetrics.CPUPointsUserSnapshot{UID: 1002, Username: "carol", ConfiguredClass: "best_effort", LifecycleState: resmanmetrics.CPUPointsLifecycleReleased, ProcessCoverage: resmanmetrics.CPUPointsCoverageNone}),
	}
	assertExactJSONKeys(t, users[0], []string{
		"applied_class", "applied_to_processes", "applied_weight", "complete_uid_workload_guaranteed",
		"configured_class", "configured_guarantee_points", "cpu_enforcement_requested", "enforceable_process_count",
		"leaf_cpu_usage_usec_delta", "lifecycle_state", "memory_high_events_delta", "memory_high_limit",
		"memory_max_events_delta", "memory_max_limit", "memory_oom_events_delta", "memory_oom_kill_events_delta",
		"memory_swap_max", "observed_process_count",
		"process_coverage", "ram_cgroup_memory_current_bytes", "ram_coverage", "ram_coverage_incomplete_process_count",
		"ram_swap_disabled", "reconciliation_degraded", "uid", "username",
	})
	encoded, err = json.Marshal(users)
	if err != nil {
		t.Fatal(err)
	}
	text = string(encoded)
	for _, fragment := range []string{`"configured_class":"guaranteed"`, `"configured_class":"best_effort"`, `"lifecycle_state":"failed"`, `"lifecycle_state":"released"`, `"process_coverage":"partial"`} {
		if !strings.Contains(text, fragment) {
			t.Errorf("user CPU Points payload %s lacks %s", text, fragment)
		}
	}
	if strings.Count(text, `"configured_guarantee_points"`) != 1 {
		t.Fatalf("best-effort users received a fabricated guarantee: %s", text)
	}

	unavailable := newCPUPointsSystemPayload(resmanmetrics.CPUPointsSystemSnapshot{
		DeliveryState:    resmanmetrics.CPUPointsDeliveryUnavailable,
		DenominatorState: resmanmetrics.CPUPointsDenominatorUnavailable,
	})
	encoded, err = json.Marshal(unavailable)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"delivery_state":"unavailable"`) {
		t.Fatalf("unavailable state is not explicit: %s", encoded)
	}
}

func TestMCPWireProjectionsPreserveTypedContracts(t *testing.T) {
	active := newActiveUsersPayload("host", "worker", []int{1000}, map[int]*resmanmetrics.UserMetrics{
		1000: {Username: "alice"},
	})
	if !reflect.DeepEqual(active.Users, []activeUserPayload{{UID: 1000, Username: "alice"}}) {
		t.Fatalf("active users projection = %+v", active)
	}

	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	start := now.Add(-30 * time.Second)
	guarantee, weight, delta := uint64(300), uint64(300), uint64(90)
	class := "guaranteed"
	userRecord := newUserHistoryRecord(database.UserMetricsRecord{
		Timestamp: now, SampleEpochID: 7, IntervalStart: &start, IntervalEnd: now,
		UID: 1000, Username: "alice", MemoryUsageBytes: 42,
		ConfiguredGuaranteePoints: &guarantee, ConfiguredCPUClass: class,
		CPUPointsLifecycleState: "applied", AppliedCPUClass: &class,
		AppliedCPUWeight: &weight, LeafCPUUsageUsecDelta: &delta,
	})
	if userRecord.Timestamp != now.Format(time.RFC3339) || (userRecord.MemoryUsage == nil || *userRecord.MemoryUsage != 42) ||
		userRecord.IntervalStart == nil || *userRecord.IntervalStart != start.Format(time.RFC3339) ||
		userRecord.ConfiguredGuaranteePoints == nil || *userRecord.ConfiguredGuaranteePoints != 300 ||
		userRecord.AppliedCPUClass == nil || *userRecord.AppliedCPUClass != class {
		t.Fatalf("user history projection = %+v", userRecord)
	}
	bestEffort := newUserHistoryRecord(database.UserMetricsRecord{Timestamp: now, IntervalEnd: now, ConfiguredCPUClass: "best_effort"})
	if bestEffort.ConfiguredGuaranteePoints != nil || bestEffort.AppliedCPUClass != nil {
		t.Fatalf("best-effort projection fabricated allocation = %+v", bestEffort)
	}
	systemRecord := newSystemHistoryRecord(database.SystemMetricsRecord{
		Timestamp: now, SampleEpochID: 7, IntervalStart: &start, IntervalEnd: now,
		IODeviceWeightState: "refused_observation", IODeviceWeightReason: "ambiguous_topology",
		IODeviceWeightSelector: "8:0", IODeviceWeightMechanism: "bfq", IODeviceWeightClassificationAttempts: 4,
		IODeviceWeightProbeAttempts: 1, IODeviceWeightProgrammed: true,
		IODeviceWeightProgrammedState: "confirmed", IODeviceWeightReadBackState: "failed",
		IODeviceWeightAuthorityCoverage: "partial", IODeviceWeightCompleteUsers: 3,
		IODeviceWeightPartialUsers: 2, IODeviceWeightUnavailableUsers: 1,
		IODeviceWeightSiblingSlices: 6, IODeviceWeightTotalPoints: 1500,
		IODeviceWeightRequestedAt: &start, IODeviceWeightNextRetryAt: &now,
		IODeviceWeightValuesJSON: "[{\"uid\":1000}]", IODeviceWeightObservedDelivery: "not_measured",
		CPULimitsActive: true, ResourceLimitsActive: true,
		AnyLimitsActive: true, CPUActivelyLimitedUsersCount: 2, ActivelyLimitedUsersCount: 3,
		ParentCPUUsageUsecDelta: &delta,
	})
	if systemRecord.Timestamp != now.Format(time.RFC3339) || systemRecord.SampleEpochID != 7 ||
		systemRecord.CPUActivelyLimitedUsersCount != 2 || systemRecord.ActivelyLimitedUsersCount != 3 ||
		systemRecord.IODeviceWeightState != "refused_observation" || systemRecord.IODeviceWeightReason != "ambiguous_topology" ||
		systemRecord.IODeviceWeightClassificationAttempts != 4 || systemRecord.IODeviceWeightProbeAttempts != 1 ||
		!systemRecord.IODeviceWeightProgrammed || systemRecord.IODeviceWeightPartialUsers != 2 ||
		systemRecord.IODeviceWeightMechanism != "bfq" || systemRecord.IODeviceWeightProgrammedState != "confirmed" ||
		systemRecord.IODeviceWeightAuthorityCoverage != "partial" || systemRecord.IODeviceWeightSiblingSlices != 6 ||
		string(systemRecord.IODeviceWeightValues) != "[{\"uid\":1000}]" ||
		systemRecord.ParentCPUUsageUsecDelta == nil || *systemRecord.ParentCPUUsageUsecDelta != 90 {
		t.Fatalf("system history projection = %+v", systemRecord)
	}
}

func TestProductionMCPOutputsDoNotUseUntypedMapLiterals(t *testing.T) {
	for _, path := range []string{"tools.go", "resources.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		positions, err := untypedOutputMapLiteralPositions(path, source)
		if err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		if len(positions) > 0 {
			t.Errorf("%s contains untyped production output map literals outside InputSchema at %v", path, positions)
		}
	}

	probe := []byte(`package mcp
var schema = Tool{InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}
var output = map[string]any{"status": true}
`)
	positions, err := untypedOutputMapLiteralPositions("probe.go", probe)
	if err != nil {
		t.Fatalf("scan probe: %v", err)
	}
	if len(positions) != 1 || positions[0].Line != 3 {
		t.Fatalf("probe output map positions = %v, want line 3 only", positions)
	}
}

func untypedOutputMapLiteralPositions(filename string, source []byte) ([]token.Position, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filename, source, 0)
	if err != nil {
		return nil, err
	}

	type sourceRange struct{ start, end token.Pos }
	var schemaRanges []sourceRange
	ast.Inspect(parsed, func(node ast.Node) bool {
		field, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		name, ok := field.Key.(*ast.Ident)
		if ok && name.Name == "InputSchema" {
			schemaRanges = append(schemaRanges, sourceRange{start: field.Value.Pos(), end: field.Value.End()})
			return false
		}
		return true
	})

	var positions []token.Position
	ast.Inspect(parsed, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok || !isStringAnyMapType(literal.Type) {
			return true
		}
		for _, allowed := range schemaRanges {
			if literal.Pos() >= allowed.start && literal.End() <= allowed.end {
				return true
			}
		}
		positions = append(positions, files.Position(literal.Pos()))
		return true
	})
	return positions, nil
}

func isStringAnyMapType(expression ast.Expr) bool {
	mapType, ok := expression.(*ast.MapType)
	if !ok {
		return false
	}
	key, keyOK := mapType.Key.(*ast.Ident)
	value, valueOK := mapType.Value.(*ast.Ident)
	return keyOK && valueOK && key.Name == "string" && (value.Name == "any" || value.Name == "interface")
}

func assertExactJSONKeys(t *testing.T, value any, want []string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	got := sortedMapKeys(payload)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("JSON keys = %v, want %v; payload: %s", got, want, encoded)
	}
}

func assertExactNestedJSONKeys(t *testing.T, value any, field string, want []string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(payload[field], &records); err != nil || len(records) != 1 {
		t.Fatalf("decode %s records: %v; payload: %s", field, err, encoded)
	}
	got := sortedMapKeys(records[0])
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("nested JSON keys = %v, want %v; payload: %s", got, want, encoded)
	}
}
