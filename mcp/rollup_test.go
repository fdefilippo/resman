package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/metricrollup"
)

func TestMetricRollupsRoundTripHTTPAndStdio(t *testing.T) {
	db, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	old := time.Now().UTC().AddDate(0, 0, -35).Truncate(time.Hour)
	if err := writeUserHistoryFixture(db, &database.UserMetricsRecord{Timestamp: old.Add(10 * time.Minute), UID: 1000, Username: "past-user", CPUUsagePercent: 42, MemoryUsageBytes: 512, ProcessCount: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CleanupOldDataWithRollupRetention(30, 365); err != nil {
		t.Fatal(err)
	}
	server := newProtocolTestServer(t)
	server.dbManager = db
	fields := map[string]any{"name": "get_metrics_rollups", "arguments": map[string]any{"scope": "user", "uid": 1000, "start_time": old.Format(time.RFC3339), "end_time": old.Add(time.Hour).Format(time.RFC3339), "limit": 1000}}
	viaHTTP := callStatusOverHTTP(t, server, "tools/call", "get_metrics_rollups", fields)
	viaStdio := callStatusOverStdio(t, server, "tools/call", fields)
	if !reflect.DeepEqual(viaHTTP, viaStdio) {
		t.Fatalf("wire differs: HTTP=%v stdio=%v", viaHTTP, viaStdio)
	}
	body, err := json.Marshal(viaHTTP)
	if err != nil {
		t.Fatal(err)
	}
	var result GetMetricRollupResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.RawSamplesIncluded || result.Averaging != "sample_weighted" || len(result.Entries) != 10 {
		t.Fatalf("result=%+v", result)
	}
	var found bool
	for _, e := range result.Entries {
		if e.Metric == metricrollup.CPUUsagePercent {
			found = true
			if e.KnownSamples != 1 || e.SampleAverage == nil || *e.SampleAverage != 42 {
				t.Fatalf("CPU=%+v", e)
			}
		}
	}
	if !found {
		t.Fatal("CPU aggregate absent")
	}
}
func TestMetricRollupsRejectUnavailableInvalidRangeAndScope(t *testing.T) {
	if _, _, err := (&Server{}).handleGetMetricRollups(t.Context(), nil, GetMetricRollupArgs{Scope: metricrollup.User}); err == nil {
		t.Fatal("disabled DB hidden")
	}
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	server := &Server{dbManager: db}
	for _, args := range []GetMetricRollupArgs{{Scope: "invalid"}, {Scope: metricrollup.User, StartTime: "invalid"}, {Scope: metricrollup.User, StartTime: "2026-11-09T00:00:00Z", EndTime: "2026-11-08T00:00:00Z"}} {
		if _, _, err := server.handleGetMetricRollups(t.Context(), nil, args); err == nil {
			t.Fatalf("invalid arguments accepted: %+v", args)
		}
	}
}
