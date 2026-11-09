package state

import (
	"database/sql"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/metrics"
)

func TestRawPersistenceAcknowledgesLifecycleDespiteActivityFailure(t *testing.T) {
	path := privateMetricsDatabasePath(t)
	db, err := database.NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	control, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	if _, err := control.Exec("DROP TABLE process_activity"); err != nil {
		t.Fatal(err)
	}
	// Empty procfs enumeration creates a completed empty observation without
	// scanning the host's processes. Restore fails against the missing table.
	t.Setenv("HOST_PROC", t.TempDir())
	collector, err := metrics.NewCollector(config.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(collector.Stop)
	writer := metrics.NewDBWriter(db, 3600)
	collector.SetDBWriter(writer)
	if got := collector.GetAllUserMetricsForDecision(); len(got) != 0 {
		t.Fatal("fixture enumerated host processes")
	}
	if writer.Healthy() {
		t.Fatal("failed initial activity restore was not immediately unhealthy")
	}
	exporter := &mockPrometheusExporter{}
	manager := &Manager{logger: logging.GetLogger(), metricsCollector: collector, prometheusExporter: exporter, activeUsers: map[int]bool{}, cpuPointsLifecycleEvents: map[int]cpuPointsLifecycleEvent{1000: {state: metrics.CPUPointsLifecycleApplied}}, ioWeightStatus: newDisabledIODeviceWeightStatus()}
	at := time.Now().UTC()
	user := &metrics.UserMetrics{UID: 1000, Username: "partial-persistence", ProcessCount: 1}
	sample := &SystemMetrics{Timestamp: at, UserMetrics: map[int]*metrics.UserMetrics{1000: user}, PersistenceSystem: metrics.SystemPersistenceMetrics{SampleEpochID: at.UnixNano(), IntervalEnd: at}, PersistenceUsers: map[int]metrics.UserPersistenceMetrics{1000: {Metrics: user, ConfiguredClass: "best_effort", LifecycleState: metrics.CPUPointsLifecycleApplied}}}
	manager.writeDatabaseMetrics(sample)
	if writer.ShouldWrite() || writer.Healthy() {
		t.Fatal("partial success must advance raw cadence and retain activity failure health")
	}
	if _, pending := manager.cpuPointsLifecycleEvents[1000]; pending {
		t.Fatal("committed raw lifecycle was left pending")
	}
	if got := exporter.recordedErrors(); len(got) != 1 || got[0].component != metricsDatabaseErrorComponent || got[0].errorType != metricsDatabaseWriteFailure {
		t.Fatalf("activity failure hidden: %+v", got)
	}
	manager.writeDatabaseMetrics(sample)
	if got := exporter.recordedErrors(); len(got) != 1 {
		t.Fatal("raw cadence retried and reported the same activity failure every cycle")
	}
	systemRows, err := db.GetSystemHistory(at.Add(-time.Minute), at.Add(time.Minute), 10)
	if err != nil || len(systemRows) != 1 {
		t.Fatalf("raw system commit replayed/lost: %d %v", len(systemRows), err)
	}
	userRows, err := db.GetUserHistory(1000, at.Add(-time.Minute), at.Add(time.Minute), 10)
	if err != nil || len(userRows) != 1 {
		t.Fatalf("raw user commit replayed/lost: %d %v", len(userRows), err)
	}
}
