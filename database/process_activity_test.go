package database

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/internal/processactivity"
)

func processActivityEntry(pid int, at time.Time) processactivity.Entry {
	return processactivity.Entry{Identity: processactivity.Identity{BootID: "boot-a", PID: pid, StartTicks: strconv.FormatUint(9007199254740993+uint64(pid), 10)}, UID: 1000, FirstObservedAt: at, ActivitySince: at, LastSeenAt: at, RuntimeAvailable: true, RuntimeSeconds: 604800, CPUAvailable: true, CPUAverageAvailable: true, AverageCPUPercent: 80, CPUCoveredWallSeconds: 3600, CoveredCPUSeconds: 2880, Samples: 3, Active: true}
}
func TestProcessActivitySurvivesRestartAndFiltersBeforeLimit(t *testing.T) {
	path := privateTestDatabasePath(t, "activity.db")
	db, err := NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	entries := []processactivity.Entry{processActivityEntry(1, now.Add(-time.Hour)), processActivityEntry(2, now)}
	entries[1].UID = 1001
	for range 2 {
		if err := db.UpsertProcessActivity(t.Context(), entries); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	uid := 1000
	got, err := db.QueryProcessActivity(t.Context(), processactivity.Query{UID: &uid, MinAverageCPUPercent: 70, Limit: 1})
	if err != nil || len(got) != 1 || got[0].PID != 1 || got[0].Active || got[0].StartTicks != entries[0].StartTicks {
		t.Fatalf("query=%+v err=%v", got, err)
	}
	restored, err := db.RestoreProcessActivity(t.Context(), "boot-a")
	if err != nil || len(restored) != 2 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	other, err := db.RestoreProcessActivity(t.Context(), "new-boot")
	if err != nil || len(other) != 0 {
		t.Fatalf("foreign boot=%+v err=%v", other, err)
	}
}
func TestProcessActivityBatchAndIdentityValidation(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	entries := []processactivity.Entry{processActivityEntry(1, time.Now()), processActivityEntry(2, time.Now())}
	entries[1].AverageCPUPercent = math.NaN()
	if err := db.UpsertProcessActivity(t.Context(), entries); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if got, err := db.QueryProcessActivity(t.Context(), processactivity.Query{}); err != nil || len(got) != 0 {
		t.Fatalf("partial=%+v err=%v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := db.UpsertProcessActivity(ctx, entries[:1]); err == nil {
		t.Fatal("cancelled batch committed")
	}
	if err := db.UpsertProcessActivity(t.Context(), entries[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE process_activity SET pid=9 WHERE pid=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.QueryProcessActivity(t.Context(), processactivity.Query{}); err == nil {
		t.Fatal("query identity mismatch ignored")
	}
	if _, err := db.RestoreProcessActivity(t.Context(), "boot-a"); err == nil {
		t.Fatal("restore identity mismatch ignored")
	}
}
func TestProcessActivityValidationRejectsMissingIdentityPrimaryKey(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.db.Exec("DROP TABLE process_activity"); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(processActivitySchema, ",\n PRIMARY KEY(boot_id,pid,start_ticks)", "", 1)
	if _, err := db.db.Exec(bad); err != nil {
		t.Fatal(err)
	}
	if err := db.validateProcessActivitySchema(); err == nil {
		t.Fatal("missing identity constraint accepted")
	}
}
func TestProcessActivityOverfetchAllowsMemoryInvalidation(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now()
	old := processActivityEntry(1, now.Add(-time.Hour))
	recent := processActivityEntry(2, now)
	if err := db.UpsertProcessActivity(t.Context(), []processactivity.Entry{old, recent}); err != nil {
		t.Fatal(err)
	}
	q := processactivity.Query{Limit: 1, MinAverageCPUPercent: 80}
	durable, err := db.QueryProcessActivity(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	recent.LastSeenAt = recent.LastSeenAt.Add(time.Second)
	recent.CPUAverageAvailable = false
	recent.CPUCoveredWallSeconds = 0
	recent.AverageCPUPercent = 0
	got := processactivity.Merge(durable, []processactivity.Entry{recent}, q)
	if len(got) != 1 || got[0].PID != 1 {
		t.Fatalf("older eligible match hidden: %+v; raw ticks=%s", got, old.StartTicks)
	}
}

func TestProcessActivityPersistsCanonicalZeroStartTicks(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	entry := processActivityEntry(42, time.Now())
	entry.StartTicks = "0"
	if err := db.UpsertProcessActivity(t.Context(), []processactivity.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	got, err := db.RestoreProcessActivity(t.Context(), entry.BootID)
	if err != nil || len(got) != 1 || got[0].StartTicks != "0" {
		t.Fatalf("zero ticks lost: %+v %v", got, err)
	}
}
