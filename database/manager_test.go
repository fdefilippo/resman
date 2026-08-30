/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// database/manager_test.go
package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewDatabaseManager(t *testing.T) {
	tmpFile := privateTestDatabasePath(t, "metrics.db")

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Verify the health check.
	if err := manager.HealthCheck(); err != nil {
		t.Errorf("Health check failed: %v", err)
	}
}

func TestDatabasePathRemainsAvailableWhileWriteBlocks(t *testing.T) {
	dbPath := privateTestDatabasePath(t, "metrics.db")
	manager, err := NewDatabaseManager(dbPath)
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	started := make(chan struct{})
	release := make(chan struct{})
	manager.beforeOperation = func() {
		close(started)
		<-release
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- manager.WriteMetricsBatch(nil, nil) }()
	<-started

	pathDone := make(chan string, 1)
	go func() { pathDone <- manager.Path() }()
	select {
	case got := <-pathDone:
		if got != dbPath {
			t.Fatalf("Path() = %q, want %q", got, dbPath)
		}
	case <-time.After(time.Second):
		close(release)
		<-writeDone
		t.Fatal("Path() blocked behind database I/O")
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatalf("WriteMetricsBatch() error: %v", err)
	}
}

func TestNewDatabaseManagerUsesIncrementalAutoVacuum(t *testing.T) {
	manager, err := NewDatabaseManager(privateTestDatabasePath(t, "metrics.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	var mode int
	if err := manager.db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		t.Fatalf("failed to read auto_vacuum mode: %v", err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum mode = %d, want 2 (INCREMENTAL)", mode)
	}
}

func TestNewDatabaseManagerMigratesLegacyAutoVacuumDatabase(t *testing.T) {
	dbPath := privateTestDatabasePath(t, "legacy.db")
	legacyDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE legacy_data (
			id INTEGER PRIMARY KEY,
			value TEXT NOT NULL
		);
		INSERT INTO legacy_data(value) VALUES ('preserved');
	`); err != nil {
		_ = legacyDB.Close()
		t.Fatalf("failed to create legacy database: %v", err)
	}
	var legacyMode int
	if err := legacyDB.QueryRow("PRAGMA auto_vacuum").Scan(&legacyMode); err != nil {
		_ = legacyDB.Close()
		t.Fatalf("failed to read legacy auto_vacuum mode: %v", err)
	}
	if legacyMode != 0 {
		_ = legacyDB.Close()
		t.Fatalf("legacy auto_vacuum mode = %d, want 0 (NONE)", legacyMode)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("failed to close legacy database: %v", err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", dbPath, err)
	}

	manager, err := NewDatabaseManager(dbPath)
	if err != nil {
		t.Fatalf("NewDatabaseManager() migration error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	var mode int
	if err := manager.db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		t.Fatalf("failed to read migrated auto_vacuum mode: %v", err)
	}
	if mode != 2 {
		t.Fatalf("migrated auto_vacuum mode = %d, want 2 (INCREMENTAL)", mode)
	}

	var value string
	if err := manager.db.QueryRow("SELECT value FROM legacy_data WHERE id = 1").Scan(&value); err != nil {
		t.Fatalf("failed to read data after auto-vacuum migration: %v", err)
	}
	if value != "preserved" {
		t.Fatalf("legacy value after migration = %q, want preserved", value)
	}
}

func TestNewDatabaseManagerRejectsAmbiguousLegacyMetricsSchema(t *testing.T) {
	dbPath := privateTestDatabasePath(t, "legacy-metrics.db")
	legacyDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE user_metrics (
			id INTEGER PRIMARY KEY,
			uid INTEGER NOT NULL,
			username TEXT NOT NULL,
			is_limited BOOLEAN DEFAULT FALSE
		);
		INSERT INTO user_metrics(uid, username, is_limited) VALUES (1000, 'ambiguous', 1);
	`); err != nil {
		_ = legacyDB.Close()
		t.Fatalf("failed to create legacy metrics database: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("legacy database close error: %v", err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", dbPath, err)
	}

	manager, err := NewDatabaseManager(dbPath)
	if manager != nil {
		_ = manager.Close()
		t.Fatal("NewDatabaseManager() returned a manager for an incompatible schema")
	}
	if err == nil {
		t.Fatal("NewDatabaseManager() accepted an ambiguous legacy schema")
	}
	for _, fragment := range []string{dbPath, "legacy unversioned schema", "delete or move", "schema version 3"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("NewDatabaseManager() error = %q, want fragment %q", err, fragment)
		}
	}

	legacyDB, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("reopen legacy database error: %v", err)
	}
	defer func() { _ = legacyDB.Close() }()
	var username string
	var limited bool
	if err := legacyDB.QueryRow("SELECT username, is_limited FROM user_metrics WHERE uid = 1000").Scan(&username, &limited); err != nil {
		t.Fatalf("legacy row was not preserved: %v", err)
	}
	if username != "ambiguous" || !limited {
		t.Fatalf("legacy row changed after rejection: username=%q is_limited=%t", username, limited)
	}
}

func TestNewDatabaseManagerRejectsVersionTwoWithoutMigration(t *testing.T) {
	dbPath := privateTestDatabasePath(t, "version-two.db")
	legacyDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}
	if _, err := legacyDB.Exec("PRAGMA user_version = 2"); err != nil {
		_ = legacyDB.Close()
		t.Fatalf("set schema version 2: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("legacy database close error: %v", err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", dbPath, err)
	}

	manager, err := NewDatabaseManager(dbPath)
	if manager != nil {
		_ = manager.Close()
		t.Fatal("NewDatabaseManager() returned a manager for schema version 2")
	}
	if err == nil {
		t.Fatal("NewDatabaseManager() migrated schema version 2")
	}
	for _, fragment := range []string{dbPath, "schema version 2", "delete or move", "schema version 3"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("NewDatabaseManager() error = %q, want fragment %q", err, fragment)
		}
	}
}

func TestNewDatabaseManagerUsesWALAndBusyTimeout(t *testing.T) {
	manager, err := NewDatabaseManager(privateTestDatabasePath(t, "metrics.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	var journalMode string
	if err := manager.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("failed to read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := manager.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("failed to read busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}
}

func TestNewDatabaseManagerCreatesRestrictiveStateDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", root, err)
	}
	dir := filepath.Join(root, "nested", "resman")
	manager, err := NewDatabaseManager(filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("DatabaseManager.Close() error: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("os.Stat(%s) error: %v", dir, err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Fatalf("state directory mode = %04o, want 0700", got)
	}
}

func TestWriteAndReadUserMetrics(t *testing.T) {
	tmpFile := privateTestDatabasePath(t, "metrics.db")

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Write metrics.
	now := time.Now()
	record := &UserMetricsRecord{
		UID:               1000,
		Username:          "testuser",
		CPUUsagePercent:   45.5,
		MemoryUsageBytes:  524288000,
		ProcessCount:      15,
		CgroupPath:        "/sys/fs/cgroup/user.slice/user-1000.slice",
		CPUQuota:          "50000 100000",
		EligibleForCPU:    true,
		EligibleForRAM:    false,
		EligibleForIO:     true,
		CPULimitRequested: true,
		CPULimitActive:    false,
		RAMLimitRequested: true,
		RAMLimitActive:    true,
		IOLimitRequested:  true,
		IOLimitActive:     false,
		Timestamp:         now,
	}

	err = manager.WriteUserMetrics(record)
	if err != nil {
		t.Errorf("Failed to write user metrics: %v", err)
	}

	// Read metrics.
	startTime := now.Add(-1 * time.Hour)
	endTime := now.Add(1 * time.Hour)
	records, err := manager.GetUserHistory(1000, startTime, endTime, 100)
	if err != nil {
		t.Errorf("Failed to read user history: %v", err)
	}

	if len(records) != 1 {
		t.Errorf("Expected 1 record, got %d", len(records))
	}

	if records[0].CPUUsagePercent != 45.5 {
		t.Errorf("Expected CPU usage 45.5, got %f", records[0].CPUUsagePercent)
	}
	if !records[0].EligibleForCPU || records[0].EligibleForRAM || !records[0].EligibleForIO {
		t.Errorf("eligibility state was not preserved: %+v", records[0])
	}
	if !records[0].CPULimitRequested || records[0].CPULimitActive ||
		!records[0].RAMLimitRequested || !records[0].RAMLimitActive ||
		!records[0].IOLimitRequested || records[0].IOLimitActive {
		t.Errorf("intent/observed state was not preserved: %+v", records[0])
	}
}

func TestWriteAndReadSystemMetrics(t *testing.T) {
	tmpFile := privateTestDatabasePath(t, "metrics.db")

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Write system metrics.
	now := time.Now()
	record := &SystemMetricsRecord{
		TotalCPUUsagePercent:         75.2,
		TotalCores:                   4,
		SystemLoad:                   2.5,
		CPULimitsActive:              true,
		ResourceLimitsActive:         true,
		AnyLimitsActive:              true,
		CPUActivelyLimitedUsersCount: 2,
		ActivelyLimitedUsersCount:    3,
		Timestamp:                    now,
	}

	err = manager.WriteSystemMetrics(record)
	if err != nil {
		t.Errorf("Failed to write system metrics: %v", err)
	}

	// Read metrics.
	startTime := now.Add(-1 * time.Hour)
	endTime := now.Add(1 * time.Hour)
	records, err := manager.GetSystemHistory(startTime, endTime, 100)
	if err != nil {
		t.Errorf("Failed to read system history: %v", err)
	}

	if len(records) != 1 {
		t.Errorf("Expected 1 record, got %d", len(records))
	}

	if records[0].TotalCores != 4 {
		t.Errorf("Expected 4 cores, got %d", records[0].TotalCores)
	}
	if !records[0].CPULimitsActive || !records[0].ResourceLimitsActive || !records[0].AnyLimitsActive {
		t.Errorf("system enforcement state = %+v, want all active", records[0])
	}
	if records[0].CPUActivelyLimitedUsersCount != 2 || records[0].ActivelyLimitedUsersCount != 3 {
		t.Errorf("system enforcement counts = CPU %d, any %d; want 2, 3", records[0].CPUActivelyLimitedUsersCount, records[0].ActivelyLimitedUsersCount)
	}
}

func TestWriteMetricsBatchRollsBackWholeCycle(t *testing.T) {
	manager, err := NewDatabaseManager(privateTestDatabasePath(t, "metrics.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	if _, err := manager.db.Exec(`
		CREATE TRIGGER reject_uid_1001
		BEFORE INSERT ON user_metrics
		WHEN NEW.uid = 1001
		BEGIN
			SELECT RAISE(ABORT, 'rejected test UID');
		END;
	`); err != nil {
		t.Fatalf("failed to create rejection trigger: %v", err)
	}

	now := time.Now()
	err = manager.WriteMetricsBatch(
		&SystemMetricsRecord{Timestamp: now, TotalCores: 4},
		[]*UserMetricsRecord{
			{Timestamp: now, UID: 1000, Username: "accepted", ProcessCount: 1},
			{Timestamp: now, UID: 1001, Username: "rejected", ProcessCount: 1},
		},
	)
	if err == nil {
		t.Fatal("WriteMetricsBatch() expected an error")
	}

	for _, table := range []string{"system_metrics", "user_metrics"} {
		var count int
		if err := manager.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("failed to count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows after rollback = %d, want 0", table, count)
		}
	}
}

func TestDatabaseTimeRangesCompareUTCInstants(t *testing.T) {
	manager, err := NewDatabaseManager(privateTestDatabasePath(t, "metrics.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	local := time.FixedZone("UTC+2", 2*60*60)
	outside := time.Date(2026, 7, 24, 9, 0, 0, 0, local) // 07:00 UTC
	inside := time.Date(2026, 7, 24, 11, 0, 0, 0, local) // 09:00 UTC
	for _, record := range []*UserMetricsRecord{
		{Timestamp: outside, UID: 1000, Username: "test", CPUUsagePercent: 7, ProcessCount: 1},
		{Timestamp: inside, UID: 1000, Username: "test", CPUUsagePercent: 9, ProcessCount: 1},
	} {
		if err := manager.WriteUserMetrics(record); err != nil {
			t.Fatalf("WriteUserMetrics() error: %v", err)
		}
	}

	start := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	records, err := manager.GetUserHistory(1000, start, end, 10)
	if err != nil {
		t.Fatalf("GetUserHistory() error: %v", err)
	}
	if len(records) != 1 || records[0].CPUUsagePercent != 9 {
		t.Fatalf("UTC user history = %+v, want only the 09:00 UTC record", records)
	}
	if records[0].Timestamp.Location() != time.UTC {
		t.Fatalf("record location = %s, want UTC", records[0].Timestamp.Location())
	}
}

func TestResolveUserUIDFromHistoricalMetrics(t *testing.T) {
	manager, err := NewDatabaseManager(privateTestDatabasePath(t, "metrics.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	now := time.Now().UTC()
	for _, record := range []*UserMetricsRecord{
		{Timestamp: now.Add(-2 * time.Hour), UID: 1000, Username: "offline-user", ProcessCount: 1},
		{Timestamp: now.Add(-time.Hour), UID: 1000, Username: "offline-user", ProcessCount: 1},
	} {
		if err := manager.WriteUserMetrics(record); err != nil {
			t.Fatalf("WriteUserMetrics() error: %v", err)
		}
	}

	uid, found, err := manager.ResolveUserUID("offline-user", now.Add(-3*time.Hour), now)
	if err != nil {
		t.Fatalf("ResolveUserUID() error = %v", err)
	}
	if !found || uid != 1000 {
		t.Fatalf("ResolveUserUID() = %d, %t; want 1000, true", uid, found)
	}

	_, found, err = manager.ResolveUserUID("missing", now.Add(-3*time.Hour), now)
	if err != nil {
		t.Fatalf("ResolveUserUID() missing user error = %v", err)
	}
	if found {
		t.Fatal("ResolveUserUID() found an unknown username")
	}
}

func TestResolveUserUIDRejectsAmbiguousHistoricalUsername(t *testing.T) {
	manager, err := NewDatabaseManager(privateTestDatabasePath(t, "metrics.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	now := time.Now().UTC()
	for _, uid := range []int{1000, 2000} {
		if err := manager.WriteUserMetrics(&UserMetricsRecord{
			Timestamp:    now,
			UID:          uid,
			Username:     "reused-name",
			ProcessCount: 1,
		}); err != nil {
			t.Fatalf("WriteUserMetrics() error: %v", err)
		}
	}

	if _, _, err := manager.ResolveUserUID("reused-name", now.Add(-time.Hour), now.Add(time.Hour)); err == nil {
		t.Fatal("ResolveUserUID() accepted a username associated with multiple UIDs")
	}
}

func TestNewDatabaseManagerNormalizesLegacyTimestampOffsets(t *testing.T) {
	dbPath := privateTestDatabasePath(t, "metrics.db")
	manager, err := NewDatabaseManager(dbPath)
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	if err := manager.WriteUserMetrics(&UserMetricsRecord{
		Timestamp:    time.Now(),
		UID:          1000,
		Username:     "legacy",
		ProcessCount: 1,
	}); err != nil {
		t.Fatalf("WriteUserMetrics() error: %v", err)
	}
	if _, err := manager.db.Exec(
		"UPDATE user_metrics SET timestamp = ? WHERE uid = ?",
		"2026-07-24 09:00:00.000000000+02:00",
		1000,
	); err != nil {
		t.Fatalf("failed to install legacy timestamp fixture: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	manager, err = NewDatabaseManager(dbPath)
	if err != nil {
		t.Fatalf("reopen NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = manager.Close() }()

	var stored string
	if err := manager.db.QueryRow(
		"SELECT CAST(timestamp AS TEXT) FROM user_metrics WHERE uid = ?",
		1000,
	).Scan(&stored); err != nil {
		t.Fatalf("failed to read normalized timestamp: %v", err)
	}
	if stored != "2026-07-24 07:00:00.000+00:00" {
		t.Fatalf("normalized timestamp = %q, want UTC equivalent", stored)
	}
}

func TestGetUserSummary(t *testing.T) {
	tmpFile := privateTestDatabasePath(t, "metrics.db")

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Write multiple metrics samples.
	now := time.Now()
	for i := 0; i < 10; i++ {
		record := &UserMetricsRecord{
			UID:              1000,
			Username:         "testuser",
			CPUUsagePercent:  float64(i * 10),
			MemoryUsageBytes: int64(500000000 + i*10000000),
			ProcessCount:     10 + i,
			CPULimitActive:   i%2 == 0,
			Timestamp:        now.Add(time.Duration(i) * time.Minute),
		}
		if err := manager.WriteUserMetrics(record); err != nil {
			t.Fatalf("Failed to write user metrics: %v", err)
		}
	}

	// Read the aggregate summary.
	startTime := now.Add(-1 * time.Hour)
	endTime := now.Add(1 * time.Hour)
	summary, err := manager.GetUserSummary(1000, startTime, endTime)
	if err != nil {
		t.Errorf("Failed to get user summary: %v", err)
	}

	if summary == nil {
		t.Fatal("Expected summary, got nil")
	}

	if summary.Samples != 10 {
		t.Errorf("Expected 10 samples, got %d", summary.Samples)
	}

	// CPU average should be 45 (mean of 0,10,20,30,40,50,60,70,80,90).
	if summary.CPUAvg != 45.0 {
		t.Errorf("Expected CPU avg 45.0, got %f", summary.CPUAvg)
	}

	// Memory average should be 545000000 (mean of 500M, 510M, ... 590M).
	if summary.MemoryAvg != 545000000.0 {
		t.Errorf("Expected Memory avg 545000000.0, got %f", summary.MemoryAvg)
	}
	if summary.CPULimitActiveTimePercent != 50 {
		t.Errorf("CPU limit active time = %f, want 50", summary.CPULimitActiveTimePercent)
	}
}

func TestCleanupOldData(t *testing.T) {
	tmpFile := privateTestDatabasePath(t, "metrics.db")

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Write old and current metrics.
	now := time.Now()

	// Old metric (35 days ago).
	oldRecord := &UserMetricsRecord{
		UID:              1000,
		Username:         "olduser",
		CPUUsagePercent:  50.0,
		MemoryUsageBytes: 500000000,
		ProcessCount:     10,
		Timestamp:        now.AddDate(0, 0, -35),
	}
	if err := manager.WriteUserMetrics(oldRecord); err != nil {
		t.Fatalf("Failed to write old user metrics: %v", err)
	}

	// Current metric (today).
	newRecord := &UserMetricsRecord{
		UID:              1001,
		Username:         "newuser",
		CPUUsagePercent:  60.0,
		MemoryUsageBytes: 600000000,
		ProcessCount:     12,
		Timestamp:        now,
	}
	if err := manager.WriteUserMetrics(newRecord); err != nil {
		t.Fatalf("Failed to write new user metrics: %v", err)
	}
	if err := manager.WriteSystemMetrics(&SystemMetricsRecord{
		TotalCPUUsagePercent: 80,
		TotalCores:           4,
		Timestamp:            now.AddDate(0, 0, -35),
	}); err != nil {
		t.Fatalf("Failed to write old system metrics: %v", err)
	}

	// Apply 30-day retention.
	deleted, err := manager.CleanupOldData(30)
	if err != nil {
		t.Errorf("Cleanup failed: %v", err)
	}

	if deleted != 2 {
		t.Errorf("Expected to delete 2 records, got %d", deleted)
	}

	// Verify that only the current metric remains.
	startTime := now.AddDate(0, 0, -1)
	endTime := now.AddDate(0, 0, 1)
	records, _ := manager.GetUserHistory(1001, startTime, endTime, 100)
	if len(records) != 1 {
		t.Errorf("Expected 1 record remaining, got %d", len(records))
	}
}

func TestGetDatabaseInfo(t *testing.T) {
	tmpFile := privateTestDatabasePath(t, "metrics.db")

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Write sample metrics.
	now := time.Now()
	for i := 0; i < 5; i++ {
		record := &UserMetricsRecord{
			UID:              1000 + i,
			Username:         "user",
			CPUUsagePercent:  float64(i * 10),
			MemoryUsageBytes: 500000000,
			ProcessCount:     10,
			Timestamp:        now,
		}
		if err := manager.WriteUserMetrics(record); err != nil {
			t.Fatalf("Failed to write user metrics: %v", err)
		}
	}

	// Read database information.
	info, err := manager.GetDatabaseInfo(30)
	if err != nil {
		t.Errorf("Failed to get database info: %v", err)
	}

	if info.UserMetricsCount != 5 {
		t.Errorf("Expected 5 user metrics, got %d", info.UserMetricsCount)
	}

	if info.UsersTracked != 5 {
		t.Errorf("Expected 5 users tracked, got %d", info.UsersTracked)
	}
}

func TestInMemoryDatabase(t *testing.T) {
	manager, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatalf("Failed to create in-memory database: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Verify the database remains healthy.
	err = manager.WriteUserMetrics(&UserMetricsRecord{
		UID:              1000,
		Username:         "test",
		CPUUsagePercent:  50.0,
		MemoryUsageBytes: 500000000,
		ProcessCount:     10,
		Timestamp:        time.Now(),
	})

	if err != nil {
		t.Errorf("Failed to write to in-memory database: %v", err)
	}
}

func TestConnectionMaxLifetime(t *testing.T) {
	tests := []struct {
		name   string
		dbPath string
		want   time.Duration
	}{
		{name: "in-memory database", dbPath: ":memory:", want: 0},
		{name: "file database", dbPath: privateTestDatabasePath(t, "metrics.db"), want: time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectionMaxLifetime(tt.dbPath); got != tt.want {
				t.Fatalf("connectionMaxLifetime(%q) = %s, want %s", tt.dbPath, got, tt.want)
			}
		})
	}
}

func privateTestDatabasePath(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", root, err)
	}
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("os.Mkdir(%s) error = %v", dir, err)
	}
	return filepath.Join(dir, name)
}
