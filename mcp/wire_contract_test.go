package mcp

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

func TestMCPWireDTOJSONContracts(t *testing.T) {
	matchCount := 1
	tests := []struct {
		name  string
		value any
		keys  []string
	}{
		{
			name:  "active users",
			value: activeUsersPayload{Users: []activeUserPayload{{UID: 1000, Username: "alice"}}},
			keys:  []string{"hostname", "server_role", "users"},
		},
		{
			name: "user metric",
			value: UserMetric{
				CgroupMemoryCurrentBytes: 1, MemoryMax: "max", MemoryHigh: "1024", MemoryHighEvents: 1,
				IOReadBytes: 1, IOWriteBytes: 1, IOReadOps: 1, IOWriteOps: 1,
			},
			keys: []string{
				"cgroup_memory_current_bytes", "cpu_limit_active", "cpu_limit_requested", "cpu_usage", "eligible_for_cpu",
				"eligible_for_io", "eligible_for_ram", "io_limit_active", "io_limit_requested", "io_read_bytes", "io_read_ops",
				"io_write_bytes", "io_write_ops", "memory_high", "memory_high_events", "memory_max", "memory_usage",
				"process_count", "ram_limit_active", "ram_limit_requested", "uid", "username",
			},
		},
		{
			name:  "resource policy configuration",
			value: newResourcePolicyConfigurationPayload("host", config.DefaultConfig()),
			keys: []string{
				"cpu_quota_normal", "cpu_release_threshold", "cpu_threshold", "cpu_threshold_duration",
				"disable_swap", "enable_prometheus", "hostname", "ignore_system_load", "io_device_filter",
				"io_enabled", "io_read_bps", "io_read_iops", "io_release_threshold", "io_threshold",
				"io_threshold_duration", "io_write_bps", "io_write_iops", "min_system_cores", "polling_interval",
				"prometheus_port", "ram_enabled", "ram_high_ratio", "ram_quota_per_user", "ram_release_threshold",
				"ram_threshold", "server_role", "system_uid_max", "system_uid_min",
			},
		},
		{
			name:  "CPU report",
			value: cpuReportPayload{},
			keys:  []string{"avg_cpu", "cpu_actively_limited_users_count", "cpu_limits_active", "hostname", "observed_users_count", "peak_cpu", "report", "server_role", "total_cpu"},
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

	assertExactNestedJSONKeys(t, getUserHistoryResult{Records: []userHistoryRecord{{}}}, "records", []string{
		"cgroup_path", "cpu_limit_active", "cpu_limit_requested", "cpu_quota", "cpu_usage", "eligible_for_cpu",
		"eligible_for_io", "eligible_for_ram", "io_limit_active", "io_limit_requested", "memory_usage", "process_count",
		"ram_limit_active", "ram_limit_requested", "timestamp", "uid", "username",
	})
	assertExactNestedJSONKeys(t, getSystemHistoryResult{Records: []systemHistoryRecord{{}}}, "records", []string{
		"actively_limited_users_count", "any_limits_active", "cpu_actively_limited_users_count", "cpu_limits_active",
		"resource_limits_active", "system_load", "timestamp", "total_cores", "total_cpu_usage",
	})
	assertExactNestedJSONKeys(t, activeUsersPayload{Users: []activeUserPayload{{UID: 1000, Username: "alice"}}}, "users", []string{"uid", "username"})
}

func TestMCPWireProjectionsPreserveTypedContracts(t *testing.T) {
	active := newActiveUsersPayload("host", "worker", []int{1000}, map[int]*resmanmetrics.UserMetrics{
		1000: {Username: "alice"},
	})
	if !reflect.DeepEqual(active.Users, []activeUserPayload{{UID: 1000, Username: "alice"}}) {
		t.Fatalf("active users projection = %+v", active)
	}

	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	userRecord := newUserHistoryRecord(database.UserMetricsRecord{Timestamp: now, UID: 1000, Username: "alice", MemoryUsageBytes: 42})
	if userRecord.Timestamp != now.Format(time.RFC3339) || userRecord.MemoryUsage != 42 {
		t.Fatalf("user history projection = %+v", userRecord)
	}
	systemRecord := newSystemHistoryRecord(database.SystemMetricsRecord{
		Timestamp: now, CPULimitsActive: true, ResourceLimitsActive: true,
		AnyLimitsActive: true, CPUActivelyLimitedUsersCount: 2, ActivelyLimitedUsersCount: 3,
	})
	if systemRecord.Timestamp != now.Format(time.RFC3339) || systemRecord.CPUActivelyLimitedUsersCount != 2 || systemRecord.ActivelyLimitedUsersCount != 3 {
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
