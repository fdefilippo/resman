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
// database/manager.go
package database

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fdefilippo/resman/logging"
	_ "github.com/mattn/go-sqlite3"
)

// UserMetricsRecord rappresenta un record delle metriche utente
type UserMetricsRecord struct {
	UID              int
	Username         string
	CPUUsagePercent  float64
	MemoryUsageBytes int64
	ProcessCount     int
	CgroupPath       string
	CPUQuota         string
	IsLimited        bool
	Timestamp        time.Time
}

// SystemMetricsRecord rappresenta un record delle metriche di sistema
type SystemMetricsRecord struct {
	TotalCPUUsagePercent float64
	TotalCores           int
	SystemLoad           float64
	LimitsActive         bool
	LimitedUsersCount    int
	Timestamp            time.Time
}

// UserSummary rappresenta le statistiche aggregate per utente
type UserSummary struct {
	UID                int     `json:"uid"`
	Username           string  `json:"username"`
	PeriodStart        string  `json:"period_start"`
	PeriodEnd          string  `json:"period_end"`
	CPUAvg             float64 `json:"cpu_avg"`
	CPUMin             float64 `json:"cpu_min"`
	CPUMax             float64 `json:"cpu_max"`
	MemoryAvg          float64 `json:"memory_avg"`
	MemoryMin          float64 `json:"memory_min"`
	MemoryMax          float64 `json:"memory_max"`
	ProcessCountAvg    float64 `json:"process_count_avg"`
	ProcessCountMin    float64 `json:"process_count_min"`
	ProcessCountMax    float64 `json:"process_count_max"`
	LimitedTimePercent float64 `json:"limited_time_percent"`
	Samples            int     `json:"samples"`
}

// DatabaseInfo rappresenta le informazioni sul database
type DatabaseInfo struct {
	Path               string  `json:"path"`
	SizeBytes          int64   `json:"size_bytes"`
	SizeMB             float64 `json:"size_mb"`
	UserMetricsCount   int64   `json:"user_metrics_count"`
	SystemMetricsCount int64   `json:"system_metrics_count"`
	OldestRecord       string  `json:"oldest_record"`
	NewestRecord       string  `json:"newest_record"`
	RetentionDays      int     `json:"retention_days"`
	UsersTracked       int64   `json:"users_tracked"`
}

// DatabaseManager gestisce il database SQLite delle metriche
type DatabaseManager struct {
	db     *sql.DB
	mu     sync.RWMutex
	dbPath string
}

const (
	insertUserMetricsQuery = `
    INSERT INTO user_metrics (timestamp, uid, username, cpu_usage_percent, memory_usage_bytes,
                              process_count, cgroup_path, cpu_quota, is_limited)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `
	insertSystemMetricsQuery = `
    INSERT INTO system_metrics (timestamp, total_cpu_usage_percent, total_cores,
                                system_load, limits_active, limited_users_count)
    VALUES (?, ?, ?, ?, ?, ?)
    `
)

// NewDatabaseManager crea un nuovo DatabaseManager
func NewDatabaseManager(dbPath string) (*DatabaseManager, error) {
	// Assicura che la directory esista
	dir := filepath.Dir(dbPath)
	if dir != ":" { // Skip per :memory:
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database at %s: %w", dbPath, err)
	}

	// Configura il database per performance migliori
	db.SetMaxOpenConns(1) // SQLite non supporta connessioni multiple in scrittura
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)

	manager := &DatabaseManager{
		db:     db,
		dbPath: dbPath,
	}

	// Inizializza lo schema
	if err := manager.InitSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize database schema at %s: %w", dbPath, err)
	}

	return manager, nil
}

func sqliteDSN(dbPath string) string {
	if dbPath == ":memory:" {
		return dbPath
	}

	query := url.Values{}
	query.Set("_auto_vacuum", "incremental")
	query.Set("_busy_timeout", "5000")
	query.Set("_journal_mode", "WAL")
	query.Set("_loc", "UTC")
	dsn := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(dbPath),
		RawQuery: query.Encode(),
	}
	return dsn.String()
}

// InitSchema crea le tabelle se non esistono
func (m *DatabaseManager) InitSchema() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureIncrementalAutoVacuum(); err != nil {
		return err
	}

	schema := `
    -- Tabella per le metriche degli utenti
    CREATE TABLE IF NOT EXISTS user_metrics (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
        uid INTEGER NOT NULL,
        username TEXT NOT NULL,
        cpu_usage_percent REAL NOT NULL,
        memory_usage_bytes INTEGER NOT NULL,
        process_count INTEGER NOT NULL,
        cgroup_path TEXT,
        cpu_quota TEXT,
        is_limited BOOLEAN DEFAULT FALSE
    );

    -- Tabella per le metriche di sistema
    CREATE TABLE IF NOT EXISTS system_metrics (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
        total_cpu_usage_percent REAL NOT NULL,
        total_cores INTEGER NOT NULL,
        system_load REAL,
        limits_active BOOLEAN DEFAULT FALSE,
        limited_users_count INTEGER
    );

    -- Indici per performance
    CREATE INDEX IF NOT EXISTS idx_user_metrics_timestamp ON user_metrics(timestamp);
    CREATE INDEX IF NOT EXISTS idx_user_metrics_uid ON user_metrics(uid);
    CREATE INDEX IF NOT EXISTS idx_user_metrics_uid_timestamp ON user_metrics(uid, timestamp);
    CREATE INDEX IF NOT EXISTS idx_user_metrics_username_timestamp ON user_metrics(username, timestamp);
    CREATE INDEX IF NOT EXISTS idx_system_metrics_timestamp ON system_metrics(timestamp);
    `

	if _, err := m.db.Exec(schema); err != nil {
		return err
	}
	return m.normalizeStoredTimestamps()
}

func (m *DatabaseManager) ensureIncrementalAutoVacuum() error {
	if m.dbPath == ":memory:" {
		return nil
	}

	mode, err := m.autoVacuumMode()
	if err != nil {
		return err
	}
	if mode == 2 {
		return nil
	}

	if _, err := m.db.Exec("PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("failed to request incremental auto-vacuum: %w", err)
	}
	mode, err = m.autoVacuumMode()
	if err != nil {
		return err
	}
	if mode == 2 {
		return nil
	}

	logger := logging.GetLogger()
	logger.Info("Migrating metrics database to incremental auto-vacuum",
		"path", m.dbPath,
		"previous_mode", mode,
	)
	if _, err := m.db.Exec("VACUUM"); err != nil {
		return fmt.Errorf("failed to migrate database %s to incremental auto-vacuum: %w", m.dbPath, err)
	}

	mode, err = m.autoVacuumMode()
	if err != nil {
		return err
	}
	if mode != 2 {
		return fmt.Errorf("database %s auto-vacuum mode is %d after migration, want 2", m.dbPath, mode)
	}
	logger.Info("Metrics database auto-vacuum migration completed",
		"path", m.dbPath,
	)
	return nil
}

func (m *DatabaseManager) autoVacuumMode() (int, error) {
	var mode int
	if err := m.db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return 0, fmt.Errorf("failed to read database auto-vacuum mode: %w", err)
	}
	return mode, nil
}

func (m *DatabaseManager) normalizeStoredTimestamps() error {
	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to start timestamp normalization transaction: %w", err)
	}
	rollback := func() {
		_ = tx.Rollback()
	}

	updates := []struct {
		table string
		query string
	}{
		{
			table: "user_metrics",
			query: `
			UPDATE user_metrics
			SET timestamp = strftime('%Y-%m-%d %H:%M:%f+00:00', timestamp)
			WHERE substr(CAST(timestamp AS TEXT), -6) != '+00:00'
			  AND strftime('%Y-%m-%d %H:%M:%f+00:00', timestamp) IS NOT NULL
		`,
		},
		{
			table: "system_metrics",
			query: `
			UPDATE system_metrics
			SET timestamp = strftime('%Y-%m-%d %H:%M:%f+00:00', timestamp)
			WHERE substr(CAST(timestamp AS TEXT), -6) != '+00:00'
			  AND strftime('%Y-%m-%d %H:%M:%f+00:00', timestamp) IS NOT NULL
		`,
		},
	}
	for _, update := range updates {
		if _, err := tx.Exec(update.query); err != nil {
			rollback()
			return fmt.Errorf("failed to normalize timestamps in %s: %w", update.table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit timestamp normalization: %w", err)
	}
	return nil
}

// WriteUserMetrics inserisce un record delle metriche utente
func (m *DatabaseManager) WriteUserMetrics(record *UserMetricsRecord) error {
	return m.WriteMetricsBatch(nil, []*UserMetricsRecord{record})
}

// WriteSystemMetrics inserisce un record delle metriche di sistema
func (m *DatabaseManager) WriteSystemMetrics(record *SystemMetricsRecord) error {
	return m.WriteMetricsBatch(record, nil)
}

// WriteMetricsBatch writes one complete collection cycle in a single transaction.
func (m *DatabaseManager) WriteMetricsBatch(system *SystemMetricsRecord, users []*UserMetricsRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to start metrics batch transaction: %w", err)
	}
	rollback := func() {
		_ = tx.Rollback()
	}

	if system != nil {
		if _, err := tx.Exec(
			insertSystemMetricsQuery,
			system.Timestamp.UTC(),
			system.TotalCPUUsagePercent,
			system.TotalCores,
			system.SystemLoad,
			system.LimitsActive,
			system.LimitedUsersCount,
		); err != nil {
			rollback()
			return fmt.Errorf("failed to insert system metrics batch record: %w", err)
		}
	}

	if len(users) > 0 {
		stmt, err := tx.Prepare(insertUserMetricsQuery)
		if err != nil {
			rollback()
			return fmt.Errorf("failed to prepare user metrics batch insert: %w", err)
		}
		for _, record := range users {
			if record == nil {
				_ = stmt.Close()
				rollback()
				return fmt.Errorf("user metrics batch contains a nil record")
			}
			if _, err := stmt.Exec(
				record.Timestamp.UTC(),
				record.UID,
				record.Username,
				record.CPUUsagePercent,
				record.MemoryUsageBytes,
				record.ProcessCount,
				record.CgroupPath,
				record.CPUQuota,
				record.IsLimited,
			); err != nil {
				_ = stmt.Close()
				rollback()
				return fmt.Errorf("failed to insert user metrics batch record for UID %d: %w", record.UID, err)
			}
		}
		if err := stmt.Close(); err != nil {
			rollback()
			return fmt.Errorf("failed to close user metrics batch statement: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit metrics batch transaction: %w", err)
	}
	return nil
}

// GetUserHistory recupera lo storico delle metriche per un utente
func (m *DatabaseManager) GetUserHistory(uid int, startTime, endTime time.Time, limit int) ([]UserMetricsRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	query := `
    SELECT timestamp, uid, username, cpu_usage_percent, memory_usage_bytes,
           process_count, cgroup_path, cpu_quota, is_limited
    FROM user_metrics
    WHERE uid = ? AND timestamp BETWEEN ? AND ?
    ORDER BY timestamp DESC
    LIMIT ?
    `

	rows, err := m.db.Query(query, uid, startTime.UTC(), endTime.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query user history for UID %d (time range %s to %s): %w", uid, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var records []UserMetricsRecord
	for rows.Next() {
		var r UserMetricsRecord
		err := rows.Scan(&r.Timestamp, &r.UID, &r.Username, &r.CPUUsagePercent,
			&r.MemoryUsageBytes, &r.ProcessCount, &r.CgroupPath,
			&r.CPUQuota, &r.IsLimited)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user history record for UID %d: %w", uid, err)
		}
		records = append(records, r)
	}

	return records, rows.Err()
}

// ResolveUserUID finds the unique UID associated with a username in a time range.
func (m *DatabaseManager) ResolveUserUID(username string, startTime, endTime time.Time) (int, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rows, err := m.db.Query(`
    SELECT DISTINCT uid
    FROM user_metrics
    WHERE username = ? AND timestamp BETWEEN ? AND ?
    LIMIT 2
    `, username, startTime.UTC(), endTime.UTC())
	if err != nil {
		return 0, false, fmt.Errorf("failed to resolve username %q in metrics history: %w", username, err)
	}
	defer func() { _ = rows.Close() }()

	uids := make([]int, 0, 2)
	for rows.Next() {
		var uid int
		if err := rows.Scan(&uid); err != nil {
			return 0, false, fmt.Errorf("failed to scan UID for username %q: %w", username, err)
		}
		uids = append(uids, uid)
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("failed while resolving username %q: %w", username, err)
	}

	switch len(uids) {
	case 0:
		return 0, false, nil
	case 1:
		return uids[0], true, nil
	default:
		return 0, false, fmt.Errorf("username %q maps to multiple UIDs in the requested time range; specify uid explicitly", username)
	}
}

// GetSystemHistory recupera lo storico delle metriche di sistema
func (m *DatabaseManager) GetSystemHistory(startTime, endTime time.Time, limit int) ([]SystemMetricsRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	query := `
    SELECT timestamp, total_cpu_usage_percent, total_cores, system_load,
           limits_active, limited_users_count
    FROM system_metrics
    WHERE timestamp BETWEEN ? AND ?
    ORDER BY timestamp DESC
    LIMIT ?
    `

	rows, err := m.db.Query(query, startTime.UTC(), endTime.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query system history (time range %s to %s): %w", startTime.Format(time.RFC3339), endTime.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var records []SystemMetricsRecord
	for rows.Next() {
		var r SystemMetricsRecord
		err := rows.Scan(&r.Timestamp, &r.TotalCPUUsagePercent, &r.TotalCores,
			&r.SystemLoad, &r.LimitsActive, &r.LimitedUsersCount)
		if err != nil {
			return nil, fmt.Errorf("failed to scan system history record: %w", err)
		}
		records = append(records, r)
	}

	return records, rows.Err()
}

// GetUserSummary recupera le statistiche aggregate per un utente
func (m *DatabaseManager) GetUserSummary(uid int, startTime, endTime time.Time) (*UserSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	query := `
    SELECT
        uid,
        username,
        MIN(timestamp) as period_start,
        MAX(timestamp) as period_end,
        AVG(cpu_usage_percent) as cpu_avg,
        MIN(cpu_usage_percent) as cpu_min,
        MAX(cpu_usage_percent) as cpu_max,
        AVG(memory_usage_bytes) as memory_avg,
        MIN(memory_usage_bytes) as memory_min,
        MAX(memory_usage_bytes) as memory_max,
        AVG(process_count) as process_count_avg,
        MIN(process_count) as process_count_min,
        MAX(process_count) as process_count_max,
        CAST(SUM(CASE WHEN is_limited THEN 1 ELSE 0 END) AS FLOAT) / COUNT(*) * 100 as limited_time_percent,
        COUNT(*) as samples
    FROM user_metrics
    WHERE uid = ? AND timestamp BETWEEN ? AND ?
    GROUP BY uid
    `

	var summary UserSummary
	err := m.db.QueryRow(query, uid, startTime.UTC(), endTime.UTC()).Scan(
		&summary.UID, &summary.Username, &summary.PeriodStart, &summary.PeriodEnd,
		&summary.CPUAvg, &summary.CPUMin, &summary.CPUMax,
		&summary.MemoryAvg, &summary.MemoryMin, &summary.MemoryMax,
		&summary.ProcessCountAvg, &summary.ProcessCountMin, &summary.ProcessCountMax,
		&summary.LimitedTimePercent, &summary.Samples,
	)

	if err == sql.ErrNoRows {
		return nil, nil // No data for this user in the time range
	}

	if err != nil {
		return nil, fmt.Errorf("failed to query user summary for UID %d (time range %s to %s): %w", uid, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339), err)
	}

	return &summary, err
}

// GetDatabaseInfo recupera le informazioni sul database
func (m *DatabaseManager) GetDatabaseInfo(retentionDays int) (*DatabaseInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info := &DatabaseInfo{
		Path:          m.dbPath,
		RetentionDays: retentionDays,
	}

	// Dimensione del file
	if m.dbPath != ":memory:" {
		fileInfo, err := os.Stat(m.dbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to stat database file at %s: %w", m.dbPath, err)
		}
		info.SizeBytes = fileInfo.Size()
		info.SizeMB = float64(info.SizeBytes) / 1024 / 1024
	}

	// Count user metrics
	err := m.db.QueryRow("SELECT COUNT(*) FROM user_metrics").Scan(&info.UserMetricsCount)
	if err != nil {
		return nil, fmt.Errorf("failed to count user metrics: %w", err)
	}

	// Count system metrics
	err = m.db.QueryRow("SELECT COUNT(*) FROM system_metrics").Scan(&info.SystemMetricsCount)
	if err != nil {
		return nil, fmt.Errorf("failed to count system metrics: %w", err)
	}

	// Oldest record
	var oldest sql.NullTime
	err = m.db.QueryRow("SELECT MIN(timestamp) FROM user_metrics").Scan(&oldest)
	if err == nil && oldest.Valid {
		info.OldestRecord = oldest.Time.Format(time.RFC3339)
	}

	// Newest record
	var newest sql.NullTime
	err = m.db.QueryRow("SELECT MAX(timestamp) FROM user_metrics").Scan(&newest)
	if err == nil && newest.Valid {
		info.NewestRecord = newest.Time.Format(time.RFC3339)
	}

	// Unique users tracked
	err = m.db.QueryRow("SELECT COUNT(DISTINCT uid) FROM user_metrics").Scan(&info.UsersTracked)
	if err != nil {
		return nil, fmt.Errorf("failed to count unique users: %w", err)
	}

	return info, nil
}

// CleanupOldData rimuove i dati più vecchi di retentionDays
func (m *DatabaseManager) CleanupOldData(retentionDays int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	tx, err := m.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to start retention transaction: %w", err)
	}
	rollback := func() {
		_ = tx.Rollback()
	}

	// Rimuovi user metrics vecchi
	result, err := tx.Exec("DELETE FROM user_metrics WHERE timestamp < ?", cutoff)
	if err != nil {
		rollback()
		return 0, fmt.Errorf("failed to delete user metrics older than %s (retention %d days): %w", cutoff.Format(time.RFC3339), retentionDays, err)
	}

	userDeleted, err := result.RowsAffected()
	if err != nil {
		rollback()
		return 0, fmt.Errorf("failed to get rows affected for user metrics deletion: %w", err)
	}

	// Rimuovi system metrics vecchi
	result, err = tx.Exec("DELETE FROM system_metrics WHERE timestamp < ?", cutoff)
	if err != nil {
		rollback()
		return 0, fmt.Errorf("failed to delete system metrics older than %s (retention %d days): %w", cutoff.Format(time.RFC3339), retentionDays, err)
	}
	systemDeleted, err := result.RowsAffected()
	if err != nil {
		rollback()
		return 0, fmt.Errorf("failed to get rows affected for system metrics deletion: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit retention cleanup: %w", err)
	}

	totalDeleted := userDeleted + systemDeleted
	if totalDeleted > 0 {
		if _, err := m.db.Exec("PRAGMA incremental_vacuum(1000)"); err != nil {
			return totalDeleted, fmt.Errorf("failed to reclaim database pages after deleting %d records: %w", totalDeleted, err)
		}
	}
	return totalDeleted, nil
}

// Close chiude la connessione al database
func (m *DatabaseManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.db != nil {
		if err := m.db.Close(); err != nil {
			return fmt.Errorf("failed to close database connection at %s: %w", m.dbPath, err)
		}
		return nil
	}
	return nil
}

// HealthCheck verifica che il database sia accessibile
func (m *DatabaseManager) HealthCheck() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.db.Ping(); err != nil {
		return fmt.Errorf("database health check failed at %s: %w", m.dbPath, err)
	}
	return nil
}
