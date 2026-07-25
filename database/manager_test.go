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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewDatabaseManager(t *testing.T) {
	// Crea un database temporaneo
	tmpFile := "/tmp/test_metrics.db"
	defer func() { _ = os.Remove(tmpFile) }()

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Verifica health check
	if err := manager.HealthCheck(); err != nil {
		t.Errorf("Health check failed: %v", err)
	}
}

func TestNewDatabaseManagerUsesIncrementalAutoVacuum(t *testing.T) {
	manager, err := NewDatabaseManager(filepath.Join(t.TempDir(), "metrics.db"))
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

func TestNewDatabaseManagerUsesWALAndBusyTimeout(t *testing.T) {
	manager, err := NewDatabaseManager(filepath.Join(t.TempDir(), "metrics.db"))
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

func TestWriteAndReadUserMetrics(t *testing.T) {
	tmpFile := "/tmp/test_metrics_write.db"
	defer func() { _ = os.Remove(tmpFile) }()

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Scrivi metriche
	now := time.Now()
	record := &UserMetricsRecord{
		UID:              1000,
		Username:         "testuser",
		CPUUsagePercent:  45.5,
		MemoryUsageBytes: 524288000,
		ProcessCount:     15,
		CgroupPath:       "/sys/fs/cgroup/user.slice/user-1000.slice",
		CPUQuota:         "50000 100000",
		IsLimited:        true,
		Timestamp:        now,
	}

	err = manager.WriteUserMetrics(record)
	if err != nil {
		t.Errorf("Failed to write user metrics: %v", err)
	}

	// Leggi metriche
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
}

func TestWriteAndReadSystemMetrics(t *testing.T) {
	tmpFile := "/tmp/test_metrics_system.db"
	defer func() { _ = os.Remove(tmpFile) }()

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Scrivi metriche di sistema
	now := time.Now()
	record := &SystemMetricsRecord{
		TotalCPUUsagePercent: 75.2,
		TotalCores:           4,
		SystemLoad:           2.5,
		LimitsActive:         true,
		LimitedUsersCount:    3,
		Timestamp:            now,
	}

	err = manager.WriteSystemMetrics(record)
	if err != nil {
		t.Errorf("Failed to write system metrics: %v", err)
	}

	// Leggi metriche
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
}

func TestWriteMetricsBatchRollsBackWholeCycle(t *testing.T) {
	manager, err := NewDatabaseManager(filepath.Join(t.TempDir(), "metrics.db"))
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
	manager, err := NewDatabaseManager(filepath.Join(t.TempDir(), "metrics.db"))
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

func TestNewDatabaseManagerNormalizesLegacyTimestampOffsets(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metrics.db")
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
	tmpFile := "/tmp/test_metrics_summary.db"
	defer func() { _ = os.Remove(tmpFile) }()

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Scrivi multiple metriche
	now := time.Now()
	for i := 0; i < 10; i++ {
		record := &UserMetricsRecord{
			UID:              1000,
			Username:         "testuser",
			CPUUsagePercent:  float64(i * 10),
			MemoryUsageBytes: int64(500000000 + i*10000000),
			ProcessCount:     10 + i,
			IsLimited:        i%2 == 0,
			Timestamp:        now.Add(time.Duration(i) * time.Minute),
		}
		if err := manager.WriteUserMetrics(record); err != nil {
			t.Fatalf("Failed to write user metrics: %v", err)
		}
	}

	// Ottieni summary
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

	// CPU avg dovrebbe essere 45 (media di 0,10,20,30,40,50,60,70,80,90)
	if summary.CPUAvg != 45.0 {
		t.Errorf("Expected CPU avg 45.0, got %f", summary.CPUAvg)
	}

	// Memory avg dovrebbe essere 545000000 (media di 500M, 510M, ... 590M)
	if summary.MemoryAvg != 545000000.0 {
		t.Errorf("Expected Memory avg 545000000.0, got %f", summary.MemoryAvg)
	}
}

func TestCleanupOldData(t *testing.T) {
	tmpFile := "/tmp/test_metrics_cleanup.db"
	defer func() { _ = os.Remove(tmpFile) }()

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Scrivi metriche vecchie e nuove
	now := time.Now()

	// Metrica vecchia (35 giorni fa)
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

	// Metrica nuova (oggi)
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

	// Cleanup con retention di 30 giorni
	deleted, err := manager.CleanupOldData(30)
	if err != nil {
		t.Errorf("Cleanup failed: %v", err)
	}

	if deleted != 2 {
		t.Errorf("Expected to delete 2 records, got %d", deleted)
	}

	// Verifica che rimanga solo la metrica nuova
	startTime := now.AddDate(0, 0, -1)
	endTime := now.AddDate(0, 0, 1)
	records, _ := manager.GetUserHistory(1001, startTime, endTime, 100)
	if len(records) != 1 {
		t.Errorf("Expected 1 record remaining, got %d", len(records))
	}
}

func TestGetDatabaseInfo(t *testing.T) {
	tmpFile := "/tmp/test_metrics_info.db"
	defer func() { _ = os.Remove(tmpFile) }()

	manager, err := NewDatabaseManager(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create database manager: %v", err)
	}
	defer func() { _ = manager.Close() }()

	// Scrivi alcune metriche
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

	// Ottieni info
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

	// Verifica che funzioni
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
