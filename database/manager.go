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
	"os"
	"path/filepath"
	"sync"
	"time"

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

// NewDatabaseManager crea un nuovo DatabaseManager
func NewDatabaseManager(dbPath string) (*DatabaseManager, error) {
	// Assicura che la directory esista
	dir := filepath.Dir(dbPath)
	if dir != ":" { // Skip per :memory:
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite3", dbPath)
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
		db.Close()
		return nil, fmt.Errorf("failed to initialize database schema at %s: %w", dbPath, err)
	}

	return manager, nil
}

// InitSchema crea le tabelle se non esistono
func (m *DatabaseManager) InitSchema() error {
	m.mu.Lock()
	defer m.mu.Unlock()

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
    CREATE INDEX IF NOT EXISTS idx_system_metrics_timestamp ON system_metrics(timestamp);
    `

	_, err := m.db.Exec(schema)
	return err
}

// WriteUserMetrics inserisce un record delle metriche utente
func (m *DatabaseManager) WriteUserMetrics(record *UserMetricsRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	query := `
    INSERT INTO user_metrics (timestamp, uid, username, cpu_usage_percent, memory_usage_bytes, 
                              process_count, cgroup_path, cpu_quota, is_limited)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `

	_, err := m.db.Exec(query,
		record.Timestamp,
		record.UID,
		record.Username,
		record.CPUUsagePercent,
		record.MemoryUsageBytes,
		record.ProcessCount,
		record.CgroupPath,
		record.CPUQuota,
		record.IsLimited,
	)

	return err
}

// WriteSystemMetrics inserisce un record delle metriche di sistema
func (m *DatabaseManager) WriteSystemMetrics(record *SystemMetricsRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	query := `
    INSERT INTO system_metrics (timestamp, total_cpu_usage_percent, total_cores, 
                                system_load, limits_active, limited_users_count)
    VALUES (?, ?, ?, ?, ?, ?)
    `

	_, err := m.db.Exec(query,
		record.Timestamp,
		record.TotalCPUUsagePercent,
		record.TotalCores,
		record.SystemLoad,
		record.LimitsActive,
		record.LimitedUsersCount,
	)

	return err
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

	rows, err := m.db.Query(query, uid, startTime, endTime, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query user history for UID %d (time range %s to %s): %w", uid, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339), err)
	}
	defer rows.Close()

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

	rows, err := m.db.Query(query, startTime, endTime, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query system history (time range %s to %s): %w", startTime.Format(time.RFC3339), endTime.Format(time.RFC3339), err)
	}
	defer rows.Close()

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
	err := m.db.QueryRow(query, uid, startTime, endTime).Scan(
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

	cutoff := time.Now().AddDate(0, 0, -retentionDays)

	// Rimuovi user metrics vecchi
	result, err := m.db.Exec("DELETE FROM user_metrics WHERE timestamp < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to delete user metrics older than %s (retention %d days): %w", cutoff.Format(time.RFC3339), retentionDays, err)
	}

	userDeleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected for user metrics deletion: %w", err)
	}

	// Rimuovi system metrics vecchi
	_, err = m.db.Exec("DELETE FROM system_metrics WHERE timestamp < ?", cutoff)
	if err != nil {
		return userDeleted, fmt.Errorf("failed to delete system metrics older than %s (retention %d days): %w", cutoff.Format(time.RFC3339), retentionDays, err)
	}

	// Vacuum per recuperare spazio
	_, err = m.db.Exec("VACUUM")
	if err != nil {
		return userDeleted, fmt.Errorf("failed to vacuum database after cleanup: %w", err)
	}

	return userDeleted, nil
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
