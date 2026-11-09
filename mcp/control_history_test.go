package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/controlhistory"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

func TestDurableControlHistoryRoundTripsThroughHTTPAndStdio(t *testing.T) {
	db, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close resource: %v", err)
		}
	}()
	entry := controlhistory.Entry{Details: controlhistory.Details{
		SessionID: "previous-daemon", CycleID: 7, StartedAt: time.Now().UTC(), Outcome: controlhistory.Failed,
		SampleAvailable: false, ExecutionAttempted: false, FailedStage: "collect_metrics", FailureKind: "stage_error",
	}, Timestamp: time.Now().UTC(), DurationMs: 15}
	if err := db.WriteControlHistory(t.Context(), []controlhistory.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.MetricsDBEnabled = true
	collector, err := metrics.NewCollector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Stop()
	collector.SetDBWriter(metrics.NewDBWriter(db, 30))
	manager, err := state.NewManager(cfg, collector, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := newProtocolTestServer(t)
	server.stateManager = manager
	fields := map[string]any{"name": "get_control_history", "arguments": map[string]any{"limit": 100}}
	httpResult := callStatusOverHTTP(t, server, "tools/call", "get_control_history", fields)
	stdioResult := callStatusOverStdio(t, server, "tools/call", fields)
	if !reflect.DeepEqual(httpResult, stdioResult) {
		t.Fatalf("HTTP/stdout history differs: HTTP=%v stdio=%v", httpResult, stdioResult)
	}
	body, err := json.Marshal(httpResult)
	if err != nil {
		t.Fatal(err)
	}
	var result GetControlHistoryResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.PersistenceStatus != controlhistory.PersistenceAvailable || len(result.Entries) != 1 || result.Entries[0].SessionID != entry.SessionID || result.Entries[0].Outcome != controlhistory.Failed || result.Entries[0].SampleAvailable {
		t.Fatalf("result=%+v", result)
	}
	collector.GetDBWriter().SetEnabled(false)
	resultDirect := manager.QueryControlHistory(t.Context(), 100)
	if resultDirect.Persistence != controlhistory.PersistenceUnavailable || len(resultDirect.Entries) != 0 {
		t.Fatalf("unavailable DB=%+v", resultDirect)
	}
}
