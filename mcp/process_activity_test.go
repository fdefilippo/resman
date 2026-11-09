package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/processactivity"
	"github.com/fdefilippo/resman/metrics"
)

func TestProcessActivityRoundTripsWithoutCollectingOverHTTPAndStdio(t *testing.T) {
	db, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	entry := processactivity.Entry{Identity: processactivity.Identity{BootID: "previous-boot", PID: 42, StartTicks: "9007199254740993"}, UID: 1000, LastSeenAt: time.Now().UTC(), RuntimeAvailable: true, RuntimeSeconds: 604800, CPUCoveredWallSeconds: 3600, CoveredCPUSeconds: 2880, CPUAverageAvailable: true, AverageCPUPercent: 80, Executable: "/bin/job", ExecutableAvailable: true}
	if err := db.UpsertProcessActivity(t.Context(), []processactivity.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.MetricsDBEnabled = true
	c, err := metrics.NewCollector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	c.SetDBWriter(metrics.NewDBWriter(db, 30))
	server := newProtocolTestServer(t)
	server.metricsCollector = c
	fields := map[string]any{"name": "get_process_activity", "arguments": map[string]any{"min_runtime_seconds": 604800, "min_average_cpu_percent": 80, "min_covered_seconds": 3600, "limit": 1}}
	httpResult := callStatusOverHTTP(t, server, "tools/call", "get_process_activity", fields)
	stdioResult := callStatusOverStdio(t, server, "tools/call", fields)
	if !reflect.DeepEqual(httpResult, stdioResult) {
		t.Fatalf("transports differ: HTTP=%v stdio=%v", httpResult, stdioResult)
	}
	body, err := json.Marshal(httpResult)
	if err != nil {
		t.Fatal(err)
	}
	var got processactivity.Result
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Persistence != processactivity.PersistenceAvailable || len(got.Entries) != 1 || got.Entries[0].StartTicks != entry.StartTicks || got.Entries[0].Active || !got.SampledAt.IsZero() {
		t.Fatalf("result=%+v", got)
	}
	if result, err := c.QueryProcessActivity(t.Context(), processactivity.Query{ActiveOnly: true}); err != nil || len(result.Entries) != 0 {
		t.Fatalf("old boot falsely live: %+v %v", result, err)
	}
	c.GetDBWriter().SetEnabled(false)
	if result, err := c.QueryProcessActivity(t.Context(), processactivity.Query{}); err != nil || result.Persistence != processactivity.PersistenceUnavailable {
		t.Fatalf("unavailable=%+v err=%v", result, err)
	}
}
