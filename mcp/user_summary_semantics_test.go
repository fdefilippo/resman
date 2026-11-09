package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/fdefilippo/resman/database"
)

func TestUserSummarySamplePercentAliasHTTPAndStdio(t *testing.T) {
	db, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	start := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	for i, minute := range []int{0, 1, 2, 59} {
		if err := writeUserHistoryFixture(db, &database.UserMetricsRecord{
			UID: 1000, Username: "uneven", CPUUsagePercent: 10,
			Timestamp: start.Add(time.Duration(minute) * time.Minute), CPULimitActive: i%2 == 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := newProtocolTestServer(t)
	server.dbManager = db
	fields := map[string]any{"name": "get_user_summary", "arguments": map[string]any{
		"uid": 1000, "startTime": start.Format(time.RFC3339), "endTime": start.Add(time.Hour).Format(time.RFC3339),
	}}
	viaHTTP := callStatusOverHTTP(t, server, "tools/call", "get_user_summary", fields)
	viaStdio := callStatusOverStdio(t, server, "tools/call", fields)
	if !reflect.DeepEqual(viaHTTP, viaStdio) {
		t.Fatalf("wire differs: HTTP=%v stdio=%v", viaHTTP, viaStdio)
	}
	body, err := json.Marshal(viaHTTP)
	if err != nil {
		t.Fatal(err)
	}
	var result GetUserSummaryResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Samples != 4 || result.CPULimitActiveTimePercent != 50 || result.CPULimitActiveSamplePercent != 50 {
		t.Fatalf("expected both count-based public fields to equal 50%%, got %+v", result)
	}
}
