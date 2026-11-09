package database

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUserSummaryActiveLimitPercentCountsUnevenSamples(t *testing.T) {
	db, err := NewDatabaseManager(privateTestDatabasePath(t, "summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	start := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	// Equal numbers of active and inactive observations have a sample frequency
	// of 50%, despite representing very different gaps between observations.
	for i, minute := range []int{0, 1, 2, 59} {
		if err := db.writeUserMetricsForTest(&UserMetricsRecord{
			UID: 1000, Username: "uneven", CPUUsagePercent: 10,
			Timestamp: start.Add(time.Duration(minute) * time.Minute), CPULimitActive: i%2 == 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := db.GetUserSummary(1000, start, start.Add(time.Hour))
	if err != nil || summary == nil {
		t.Fatalf("summary=%+v error=%v", summary, err)
	}
	if summary.Samples != 4 || summary.CPULimitActiveTimePercent != 50 || summary.CPULimitActiveSamplePercent != 50 {
		t.Fatalf("expected count-based 50%% aliases for four uneven samples, got %+v", summary)
	}
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"cpu_limit_active_time_percent", "cpu_limit_active_sample_percent"} {
		if string(wire[field]) != "50" {
			t.Fatalf("public field %s=%s, want 50", field, wire[field])
		}
	}
}
