package database

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/fdefilippo/resman/internal/controlhistory"
)

func historyFixture(session string, cycle int64, at time.Time) controlhistory.Entry {
	return controlhistory.Entry{Details: controlhistory.Details{SessionID: session, CycleID: cycle, StartedAt: at.Add(-time.Second), Outcome: controlhistory.Succeeded, SampleAvailable: true}, Timestamp: at, Decision: "MAINTAIN_CURRENT_STATE", Reason: "below threshold", DurationMs: 1000}
}

func TestControlHistorySurvivesRestartDeduplicatesAndRetains(t *testing.T) {
	path := privateTestDatabasePath(t, "history.db")
	db, err := NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	entries := []controlhistory.Entry{
		historyFixture("old-session", 1, now.AddDate(0, 0, -31)),
		historyFixture("session-a", 1, now.Add(-time.Second)),
		historyFixture("session-b", 1, now),
	}
	for range 2 {
		if err := db.WriteControlHistory(t.Context(), entries); err != nil {
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
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close resource: %v", err)
		}
	}()
	got, err := db.ReadControlHistory(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].SessionID != "session-b" || got[1].Reason != "below threshold" {
		t.Fatalf("history after restart = %+v", got)
	}
	latest, err := db.ReadControlHistory(t.Context(), 1)
	if err != nil || len(latest) != 1 || latest[0].SessionID != "session-b" {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	deleted, err := db.CleanupOldData(30)
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup=(%d,%v)", deleted, err)
	}
	got, err = db.ReadControlHistory(t.Context(), 100)
	if err != nil || len(got) != 2 {
		t.Fatalf("retained=%+v err=%v", got, err)
	}
}

func TestControlHistoryBatchFailureIsAtomic(t *testing.T) {
	for _, tt := range []struct {
		name    string
		invalid func(*controlhistory.Entry)
	}{
		{"identity", func(e *controlhistory.Entry) { e.SessionID = "" }},
		{"encoding", func(e *controlhistory.Entry) { e.TotalCPUUsage = math.NaN() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, err := NewDatabaseManager(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Errorf("close resource: %v", err)
				}
			}()
			entries := []controlhistory.Entry{historyFixture("session", 1, time.Now()), historyFixture("session", 2, time.Now())}
			tt.invalid(&entries[1])
			if err := db.WriteControlHistory(t.Context(), entries); err == nil {
				t.Fatal("invalid batch succeeded")
			}
			got, err := db.ReadControlHistory(t.Context(), 100)
			if err != nil || len(got) != 0 {
				t.Fatalf("partial batch persisted: %+v %v", got, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := db.WriteControlHistory(ctx, entries[:1]); err == nil {
				t.Fatal("cancelled write succeeded")
			}
		})
	}
}

func TestSchema10MigrationPreservesObservationsAndStartsEmptyHistory(t *testing.T) {
	path := privateTestDatabasePath(t, "schema10.db")
	db, err := NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.writeSystemMetricsForTest(&SystemMetricsRecord{TotalCores: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("DROP TABLE control_history; PRAGMA user_version = 10"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close resource: %v", err)
		}
	}()
	var cores, version int
	if err := db.db.QueryRow("SELECT total_cores FROM system_metrics").Scan(&cores); err != nil || cores != 7 {
		t.Fatalf("preserved cores=%d err=%v", cores, err)
	}
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 11 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	entries, err := db.ReadControlHistory(t.Context(), 100)
	if err != nil || len(entries) != 0 {
		t.Fatalf("fabricated history=%+v err=%v", entries, err)
	}
}

func TestSchema10MigrationConflictDoesNotAdvanceVersion(t *testing.T) {
	path := privateTestDatabasePath(t, "conflict.db")
	db, err := NewDatabaseManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("DROP TABLE control_history; CREATE TABLE control_history (foreign_data TEXT); PRAGMA user_version = 10"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := NewDatabaseManager(path); err == nil {
		_ = db.Close()
		t.Fatal("conflicting table accepted")
	}
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close resource: %v", err)
		}
	}()
	var version int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 10 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}
