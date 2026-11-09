package database

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/fdefilippo/resman/internal/metricrollup"
)

func rollupUserFixture(t *testing.T, db *DatabaseManager, at time.Time, uid int, name string, cpu float64, unavailable bool) {
	t.Helper()
	if err := db.writeUserMetricsForTest(&UserMetricsRecord{UID: uid, Username: name, CPUUsagePercent: cpu, MemoryUsageBytes: 100, ProcessCount: 2, Timestamp: at, ProcessObservationUnavailable: unavailable}); err != nil {
		t.Fatal(err)
	}
}
func findRollup(t *testing.T, entries []metricrollup.Entry, name metricrollup.Metric) metricrollup.Entry {
	t.Helper()
	for _, e := range entries {
		if e.Metric == name {
			return e
		}
	}
	t.Fatalf("metric %s absent from %+v", name, entries)
	return metricrollup.Entry{}
}
func queryUserRollups(t *testing.T, db *DatabaseManager, start, end time.Time) []metricrollup.Entry {
	t.Helper()
	entries, err := db.GetMetricRollups(t.Context(), metricrollup.Query{Scope: metricrollup.User, Start: start, End: end, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
func TestRollupCompactionIsIdempotentLateSafeAndAvailabilityAware(t *testing.T) {
	path := privateTestDatabasePath(t, "rollups.db")
	db, err := NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -35).Truncate(time.Hour)
	rollupUserFixture(t, db, old.Add(10*time.Minute), 1000, "worker", 0, false)
	rollupUserFixture(t, db, old.Add(20*time.Minute), 1000, "worker", 60, false)
	rollupUserFixture(t, db, old.Add(30*time.Minute), 1000, "worker", 999, true)
	for range 2 {
		if _, err := db.CleanupOldDataWithRollupRetention(30, 365); err != nil {
			t.Fatal(err)
		}
	}
	entries := queryUserRollups(t, db, old, old.Add(time.Hour))
	cpu := findRollup(t, entries, metricrollup.CPUUsagePercent)
	if cpu.Samples != 3 || cpu.KnownSamples != 2 || cpu.UnknownSamples != 1 || cpu.Sum == nil || *cpu.Sum != 60 || cpu.SampleAverage == nil || *cpu.SampleAverage != 30 || *cpu.Minimum != 0 || *cpu.Maximum != 60 {
		t.Fatalf("coverage=%+v", cpu)
	}
	rollupUserFixture(t, db, old.Add(40*time.Minute), 1000, "worker", 90, false)
	if _, err := db.CleanupOldDataWithRollupRetention(30, 365); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	entries = queryUserRollups(t, db, old, old.Add(time.Hour))
	cpu = findRollup(t, entries, metricrollup.CPUUsagePercent)
	if cpu.Samples != 4 || cpu.KnownSamples != 3 || *cpu.Sum != 150 || *cpu.SampleAverage != 50 {
		t.Fatalf("late/reopened=%+v", cpu)
	}
	raw, err := db.GetUserHistory(1000, old, old.Add(time.Hour), 100)
	if err != nil || len(raw) != 0 {
		t.Fatalf("raw=%v err=%v", raw, err)
	}
}
func TestRollupHourIdentityUnknownAndRetention(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	old := time.Now().UTC().AddDate(0, 0, -35).Truncate(time.Hour)
	rollupUserFixture(t, db, old.Add(-time.Nanosecond), 1, "previous-name", 10, false)
	rollupUserFixture(t, db, old, 1, "new-name", 20, false)
	rollupUserFixture(t, db, old.Add(time.Minute), 2, "unknown", 999, true)
	if _, err := db.CleanupOldDataWithRollupRetention(30, 60); err != nil {
		t.Fatal(err)
	}
	entries := queryUserRollups(t, db, old.Add(-time.Hour), old.Add(time.Hour))
	var cpus []metricrollup.Entry
	for _, e := range entries {
		if e.Metric == metricrollup.CPUUsagePercent {
			cpus = append(cpus, e)
		}
	}
	if len(cpus) != 3 {
		t.Fatalf("identity/hour buckets=%+v", cpus)
	}
	for _, e := range cpus {
		if e.Username == "previous-name" && (e.HourStart != old.Add(-time.Hour).Format(time.RFC3339) || e.FirstSampleAt != old.Add(-time.Nanosecond).Format(time.RFC3339Nano)) {
			t.Fatalf("nanosecond rounded into wrong hour: %+v", e)
		}
		if e.Username == "new-name" && e.HourStart != old.Format(time.RFC3339) {
			t.Fatalf("hour boundary misplaced: %+v", e)
		}
		if e.UID == 2 && (e.KnownSamples != 0 || e.Sum != nil || e.SampleAverage != nil || e.Minimum != nil || e.Maximum != nil) {
			t.Fatalf("unknown fabricated=%+v", e)
		}
	}
	partial, err := db.GetMetricRollups(t.Context(), metricrollup.Query{Scope: metricrollup.User, Start: old, End: old.Add(time.Nanosecond), Limit: 1000})
	if err != nil || len(partial) != 20 {
		t.Fatalf("partial hour=%d err=%v", len(partial), err)
	}
	if _, err := db.CleanupOldDataWithRollupRetention(30, 31); err != nil {
		t.Fatal(err)
	}
	if entries := queryUserRollups(t, db, old.Add(-time.Hour), old.Add(time.Hour)); len(entries) != 0 {
		t.Fatalf("expired rollups=%+v", entries)
	}
}
func TestRollupCompactionAndRawDeletionRollBackTogether(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	old := time.Now().UTC().AddDate(0, 0, -35)
	rollupUserFixture(t, db, old, 1000, "worker", 50, false)
	if _, err := db.db.Exec(`CREATE TRIGGER refuse_retention BEFORE DELETE ON user_metrics BEGIN SELECT RAISE(ABORT,'test refusal'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CleanupOldDataWithRollupRetention(30, 365); err == nil {
		t.Fatal("retention failure hidden")
	}
	if entries := queryUserRollups(t, db, old.Add(-time.Hour), old.Add(time.Hour)); len(entries) != 0 {
		t.Fatalf("partial compaction=%+v", entries)
	}
	raw, err := db.GetUserHistory(1000, old.Add(-time.Hour), old.Add(time.Hour), 100)
	if err != nil || len(raw) != 1 {
		t.Fatalf("raw lost=%+v err=%v", raw, err)
	}
}
func TestRollupSystemDimensionsAndBoundedQuery(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	old := time.Now().UTC().AddDate(0, 0, -35)
	for _, cores := range []int{4, 8} {
		if err := db.writeSystemMetricsForTest(&SystemMetricsRecord{Timestamp: old, TotalCores: cores, EnforcementMode: "observation_only", DenominatorState: "unavailable"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CleanupOldDataWithRollupRetention(30, 365); err != nil {
		t.Fatal(err)
	}
	q := metricrollup.Query{Scope: metricrollup.System, Start: old.Add(-time.Hour), End: old.Add(time.Hour), Limit: 1}
	entries, err := db.GetMetricRollups(t.Context(), q)
	if err != nil || len(entries) != 1 {
		t.Fatalf("limit=%+v %v", entries, err)
	}
	q.Limit = 1000
	entries, err = db.GetMetricRollups(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	dimensions := map[int]bool{}
	for _, e := range entries {
		dimensions[e.TotalCores] = true
		if e.Metric == metricrollup.CPUUsagePercent {
			t.Fatal("host CPU availability invented")
		}
	}
	if len(dimensions) != 2 {
		t.Fatalf("core denominators mixed: %+v", entries)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := db.GetMetricRollups(ctx, q); err == nil {
		t.Fatal("cancelled query accepted")
	}
}
func TestSchema11MigrationAddsEmptyObservationStoresOrRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserves", true: "conflict"}[conflict], func(t *testing.T) {
			path := privateTestDatabasePath(t, "schema11.db")
			db, err := NewDatabaseManager(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.writeSystemMetricsForTest(&SystemMetricsRecord{TotalCores: 7}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.db.Exec("DROP TABLE metric_rollups;DROP TABLE process_activity; PRAGMA user_version=11"); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := db.db.Exec("CREATE TABLE process_activity(foreign_data TEXT)"); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			opened, openErr := NewDatabaseManager(path)
			if conflict {
				if openErr == nil {
					_ = opened.Close()
					t.Fatal("foreign store accepted")
				}
				raw, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = raw.Close() }()
				var version, count int
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 11 {
					t.Fatalf("version=%d err=%v", version, err)
				}
				if err := raw.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='metric_rollups'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial table=%d err=%v", count, err)
				}
			} else {
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer func() { _ = opened.Close() }()
				var cores int
				if err := opened.db.QueryRow("SELECT total_cores FROM system_metrics").Scan(&cores); err != nil || cores != 7 {
					t.Fatalf("cores=%d err=%v", cores, err)
				}
				if got := queryUserRollups(t, opened, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); len(got) != 0 {
					t.Fatalf("fabricated rollups=%+v", got)
				}
			}
		})
	}
}

func TestRollupUsesUTCAndKeepsRawWindowWhenConfiguredRollupIsShorter(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	utc := time.Now().UTC().AddDate(0, 0, -35).Truncate(time.Hour)
	rollupUserFixture(t, db, utc.In(time.FixedZone("fixture-east", 7200)), 1000, "zone-user", 10, false)
	// Effective rollup retention cannot be shorter than the caller's raw retention.
	if _, err := db.CleanupOldDataWithRollupRetention(30, 1); err != nil {
		t.Fatal(err)
	}
	entries := queryUserRollups(t, db, utc.Add(-time.Hour), utc.Add(time.Hour))
	if len(entries) != 0 {
		t.Fatalf("35-day data retained beyond effective30 days: %+v", entries)
	}
	rollupUserFixture(t, db, utc.In(time.FixedZone("fixture-east", 7200)), 1000, "zone-user", 10, false)
	if _, err := db.CleanupOldDataWithRollupRetention(30, 365); err != nil {
		t.Fatal(err)
	}
	entries = queryUserRollups(t, db, utc.Add(-time.Hour), utc.Add(time.Hour))
	for _, e := range entries {
		if e.HourStart != utc.Format(time.RFC3339) {
			t.Fatalf("local timezone changed UTC hour: %+v", e)
		}
	}
	if len(entries) != 10 {
		t.Fatalf("UTC entries=%d", len(entries))
	}
}
