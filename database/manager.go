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
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/fdefilippo/resman/internal/operationgate"
	"github.com/fdefilippo/resman/logging"
	_ "github.com/mattn/go-sqlite3"
)

// UserMetricsRecord represents one persisted user metrics sample.
type UserMetricsRecord struct {
	UID               int
	Username          string
	CPUUsagePercent   float64
	MemoryUsageBytes  int64
	ProcessCount      int
	CgroupPath        string
	CPUQuota          string
	EligibleForCPU    bool
	EligibleForRAM    bool
	EligibleForIO     bool
	CPULimitRequested bool
	CPULimitActive    bool
	RAMLimitRequested bool
	RAMLimitActive    bool
	IOLimitRequested  bool
	IOLimitActive     bool
	Timestamp         time.Time
}

// SystemMetricsRecord represents one persisted system metrics sample.
type SystemMetricsRecord struct {
	TotalCPUUsagePercent         float64
	TotalCores                   int
	SystemLoad                   float64
	CPULimitsActive              bool
	ResourceLimitsActive         bool
	AnyLimitsActive              bool
	CPUActivelyLimitedUsersCount int
	ActivelyLimitedUsersCount    int
	Timestamp                    time.Time
}

// UserSummary contains aggregate metrics for one user and time range.
type UserSummary struct {
	UID                       int     `json:"uid"`
	Username                  string  `json:"username"`
	PeriodStart               string  `json:"period_start"`
	PeriodEnd                 string  `json:"period_end"`
	CPUAvg                    float64 `json:"cpu_avg"`
	CPUMin                    float64 `json:"cpu_min"`
	CPUMax                    float64 `json:"cpu_max"`
	MemoryAvg                 float64 `json:"memory_avg"`
	MemoryMin                 float64 `json:"memory_min"`
	MemoryMax                 float64 `json:"memory_max"`
	ProcessCountAvg           float64 `json:"process_count_avg"`
	ProcessCountMin           float64 `json:"process_count_min"`
	ProcessCountMax           float64 `json:"process_count_max"`
	CPULimitActiveTimePercent float64 `json:"cpu_limit_active_time_percent"`
	Samples                   int     `json:"samples"`
}

// DatabaseInfo describes the metrics database.
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

// DatabaseManager manages the SQLite metrics database.
type DatabaseManager struct {
	db              *sql.DB
	dbGate          operationgate.Gate
	dbPath          string
	beforeOperation func()
}

const (
	metricsSchemaVersion   = 3
	insertUserMetricsQuery = `
    INSERT INTO user_metrics (timestamp, uid, username, cpu_usage_percent, memory_usage_bytes,
							  process_count, cgroup_path, cpu_quota, eligible_for_cpu,
							  eligible_for_ram, eligible_for_io, cpu_limit_requested,
							  cpu_limit_active, ram_limit_requested, ram_limit_active,
							  io_limit_requested, io_limit_active)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `
	insertSystemMetricsQuery = `
	INSERT INTO system_metrics (timestamp, total_cpu_usage_percent, total_cores,
								system_load, cpu_limits_active, resource_limits_active,
								any_limits_active, cpu_actively_limited_users_count,
								actively_limited_users_count)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `
)

// NewDatabaseManager creates a metrics database manager.
func NewDatabaseManager(dbPath string) (*DatabaseManager, error) {
	storage, err := prepareSQLiteStorage(dbPath)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database at %s: %w", dbPath, err)
	}

	// Configure SQLite for serialized writes and bounded connection reuse.
	db.SetMaxOpenConns(1) // SQLite does not support concurrent writers here.
	db.SetMaxIdleConns(1)
	if lifetime := connectionMaxLifetime(dbPath); lifetime > 0 {
		db.SetConnMaxLifetime(lifetime)
	}

	manager := &DatabaseManager{
		db:     db,
		dbPath: dbPath,
	}

	// Initialize or validate the schema before publishing the manager.
	if err := manager.InitSchema(); err != nil {
		startupErr := fmt.Errorf("failed to initialize database schema at %s: %w", dbPath, err)
		if secureErr := storage.secureRuntimeArtifacts(); secureErr != nil {
			startupErr = errors.Join(startupErr, secureErr)
		}
		if closeErr := db.Close(); closeErr != nil {
			startupErr = errors.Join(startupErr, fmt.Errorf("failed to close database after startup rejection: %w", closeErr))
		}
		return nil, startupErr
	}
	if err := storage.secureRuntimeArtifacts(); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to close database after storage protection failure: %w", closeErr))
		}
		return nil, err
	}

	return manager, nil
}

func connectionMaxLifetime(dbPath string) time.Duration {
	if dbPath == ":memory:" {
		return 0
	}
	return time.Hour
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

// InitSchema creates or validates the current metrics schema.
func (m *DatabaseManager) InitSchema() error {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

	version, err := m.schemaVersion()
	if err != nil {
		return err
	}
	hasMetricsTables, err := m.hasMetricsTables()
	if err != nil {
		return err
	}
	if version == 0 && hasMetricsTables {
		return m.incompatibleSchemaError("legacy unversioned schema")
	}
	if version != 0 && version != metricsSchemaVersion {
		return m.incompatibleSchemaError(fmt.Sprintf("schema version %d", version))
	}

	if err := m.ensureIncrementalAutoVacuum(); err != nil {
		return err
	}

	schema := `
    -- Per-user metrics and explicit policy/runtime state.
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
        eligible_for_cpu BOOLEAN NOT NULL,
        eligible_for_ram BOOLEAN NOT NULL,
        eligible_for_io BOOLEAN NOT NULL,
        cpu_limit_requested BOOLEAN NOT NULL,
        cpu_limit_active BOOLEAN NOT NULL,
        ram_limit_requested BOOLEAN NOT NULL,
        ram_limit_active BOOLEAN NOT NULL,
        io_limit_requested BOOLEAN NOT NULL,
        io_limit_active BOOLEAN NOT NULL
    );

    -- System-wide observation and explicit enforcement state.
    CREATE TABLE IF NOT EXISTS system_metrics (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
        total_cpu_usage_percent REAL NOT NULL,
        total_cores INTEGER NOT NULL,
        system_load REAL,
		cpu_limits_active BOOLEAN NOT NULL,
		resource_limits_active BOOLEAN NOT NULL,
		any_limits_active BOOLEAN NOT NULL,
		cpu_actively_limited_users_count INTEGER NOT NULL,
		actively_limited_users_count INTEGER NOT NULL
    );

    -- Query indexes.
    CREATE INDEX IF NOT EXISTS idx_user_metrics_timestamp ON user_metrics(timestamp);
    CREATE INDEX IF NOT EXISTS idx_user_metrics_uid ON user_metrics(uid);
    CREATE INDEX IF NOT EXISTS idx_user_metrics_uid_timestamp ON user_metrics(uid, timestamp);
    CREATE INDEX IF NOT EXISTS idx_user_metrics_username_timestamp ON user_metrics(username, timestamp);
    CREATE INDEX IF NOT EXISTS idx_system_metrics_timestamp ON system_metrics(timestamp);
    `

	if _, err := m.db.Exec(schema); err != nil {
		return err
	}
	if version == 0 {
		if _, err := m.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", metricsSchemaVersion)); err != nil {
			return fmt.Errorf("failed to record metrics schema version %d: %w", metricsSchemaVersion, err)
		}
	}
	if err := m.validateUserMetricsSchema(); err != nil {
		return err
	}
	if err := m.validateSystemMetricsSchema(); err != nil {
		return err
	}
	return m.normalizeStoredTimestamps()
}

func (m *DatabaseManager) schemaVersion() (int, error) {
	var version int
	if err := m.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("failed to read metrics database schema version: %w", err)
	}
	return version, nil
}

func (m *DatabaseManager) hasMetricsTables() (bool, error) {
	var count int
	if err := m.db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN ('user_metrics', 'system_metrics')
	`).Scan(&count); err != nil {
		return false, fmt.Errorf("failed to inspect metrics database tables: %w", err)
	}
	return count > 0, nil
}

func (m *DatabaseManager) validateUserMetricsSchema() error {
	rows, err := m.db.Query("PRAGMA table_info(user_metrics)")
	if err != nil {
		return fmt.Errorf("failed to inspect user_metrics schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("failed to scan user_metrics schema: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed while inspecting user_metrics schema: %w", err)
	}
	if columns["is_limited"] {
		return m.incompatibleSchemaError("ambiguous is_limited column")
	}
	for _, required := range []string{
		"eligible_for_cpu", "eligible_for_ram", "eligible_for_io",
		"cpu_limit_requested", "cpu_limit_active", "ram_limit_requested",
		"ram_limit_active", "io_limit_requested", "io_limit_active",
	} {
		if !columns[required] {
			return m.incompatibleSchemaError(fmt.Sprintf("missing required column %s", required))
		}
	}
	return nil
}

func (m *DatabaseManager) validateSystemMetricsSchema() error {
	rows, err := m.db.Query("PRAGMA table_info(system_metrics)")
	if err != nil {
		return fmt.Errorf("failed to inspect system_metrics schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("failed to scan system_metrics schema: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed while inspecting system_metrics schema: %w", err)
	}
	for _, ambiguous := range []string{"limits_active", "limited_users_count"} {
		if columns[ambiguous] {
			return m.incompatibleSchemaError(fmt.Sprintf("ambiguous %s column", ambiguous))
		}
	}
	for _, required := range []string{
		"cpu_limits_active", "resource_limits_active", "any_limits_active",
		"cpu_actively_limited_users_count", "actively_limited_users_count",
	} {
		if !columns[required] {
			return m.incompatibleSchemaError(fmt.Sprintf("missing required column %s", required))
		}
	}
	return nil
}

func (m *DatabaseManager) incompatibleSchemaError(found string) error {
	return fmt.Errorf(
		"incompatible metrics database schema at %s: found %s; delete or move the database and restart to recreate schema version %d",
		m.dbPath,
		found,
		metricsSchemaVersion,
	)
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

// WriteUserMetrics inserts one per-user metrics record.
func (m *DatabaseManager) WriteUserMetrics(record *UserMetricsRecord) error {
	return m.WriteMetricsBatch(nil, []*UserMetricsRecord{record})
}

// WriteSystemMetrics inserts one system metrics record.
func (m *DatabaseManager) WriteSystemMetrics(record *SystemMetricsRecord) error {
	return m.WriteMetricsBatch(record, nil)
}

// WriteMetricsBatch writes one complete collection cycle in a single transaction.
func (m *DatabaseManager) WriteMetricsBatch(system *SystemMetricsRecord, users []*UserMetricsRecord) error {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()
	if m.beforeOperation != nil {
		m.beforeOperation()
	}

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
			system.CPULimitsActive,
			system.ResourceLimitsActive,
			system.AnyLimitsActive,
			system.CPUActivelyLimitedUsersCount,
			system.ActivelyLimitedUsersCount,
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
				record.EligibleForCPU,
				record.EligibleForRAM,
				record.EligibleForIO,
				record.CPULimitRequested,
				record.CPULimitActive,
				record.RAMLimitRequested,
				record.RAMLimitActive,
				record.IOLimitRequested,
				record.IOLimitActive,
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

// GetUserHistory returns persisted metrics for one user and time range.
func (m *DatabaseManager) GetUserHistory(uid int, startTime, endTime time.Time, limit int) ([]UserMetricsRecord, error) {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

	query := `
    SELECT timestamp, uid, username, cpu_usage_percent, memory_usage_bytes,
		   process_count, cgroup_path, cpu_quota, eligible_for_cpu,
		   eligible_for_ram, eligible_for_io, cpu_limit_requested,
		   cpu_limit_active, ram_limit_requested, ram_limit_active,
		   io_limit_requested, io_limit_active
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
			&r.CPUQuota, &r.EligibleForCPU, &r.EligibleForRAM, &r.EligibleForIO,
			&r.CPULimitRequested, &r.CPULimitActive, &r.RAMLimitRequested,
			&r.RAMLimitActive, &r.IOLimitRequested, &r.IOLimitActive)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user history record for UID %d: %w", uid, err)
		}
		records = append(records, r)
	}

	return records, rows.Err()
}

// ResolveUserUID finds the unique UID associated with a username in a time range.
func (m *DatabaseManager) ResolveUserUID(username string, startTime, endTime time.Time) (int, bool, error) {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

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

// GetSystemHistory returns persisted system metrics for a time range.
func (m *DatabaseManager) GetSystemHistory(startTime, endTime time.Time, limit int) ([]SystemMetricsRecord, error) {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

	query := `
    SELECT timestamp, total_cpu_usage_percent, total_cores, system_load,
		   cpu_limits_active, resource_limits_active, any_limits_active,
		   cpu_actively_limited_users_count, actively_limited_users_count
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
			&r.SystemLoad, &r.CPULimitsActive, &r.ResourceLimitsActive,
			&r.AnyLimitsActive, &r.CPUActivelyLimitedUsersCount,
			&r.ActivelyLimitedUsersCount)
		if err != nil {
			return nil, fmt.Errorf("failed to scan system history record: %w", err)
		}
		records = append(records, r)
	}

	return records, rows.Err()
}

// GetUserSummary returns aggregate persisted metrics for one user and time range.
func (m *DatabaseManager) GetUserSummary(uid int, startTime, endTime time.Time) (*UserSummary, error) {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

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
		CAST(SUM(CASE WHEN cpu_limit_active THEN 1 ELSE 0 END) AS FLOAT) / COUNT(*) * 100 as cpu_limit_active_time_percent,
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
		&summary.CPULimitActiveTimePercent, &summary.Samples,
	)

	if err == sql.ErrNoRows {
		return nil, nil // No data for this user in the time range
	}

	if err != nil {
		return nil, fmt.Errorf("failed to query user summary for UID %d (time range %s to %s): %w", uid, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339), err)
	}

	return &summary, err
}

// GetDatabaseInfo returns database size and retained metrics statistics.
func (m *DatabaseManager) GetDatabaseInfo(retentionDays int) (*DatabaseInfo, error) {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

	info := &DatabaseInfo{
		Path:          m.dbPath,
		RetentionDays: retentionDays,
	}

	// Read the file size.
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

// CleanupOldData removes records older than retentionDays.
func (m *DatabaseManager) CleanupOldData(retentionDays int) (int64, error) {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	tx, err := m.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to start retention transaction: %w", err)
	}
	rollback := func() {
		_ = tx.Rollback()
	}

	// Remove old user metrics.
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

	// Remove old system metrics.
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

// Close closes the database connection after preceding operations finish.
func (m *DatabaseManager) Close() error {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

	if m.db != nil {
		if err := m.db.Close(); err != nil {
			return fmt.Errorf("failed to close database connection at %s: %w", m.dbPath, err)
		}
		return nil
	}
	return nil
}

// HealthCheck verifies that the database is reachable.
func (m *DatabaseManager) HealthCheck() error {
	leaveOperation := m.dbGate.Enter()
	defer leaveOperation()

	if err := m.db.Ping(); err != nil {
		return fmt.Errorf("database health check failed at %s: %w", m.dbPath, err)
	}
	return nil
}

// Path returns the configured database path without waiting for database I/O.
func (m *DatabaseManager) Path() string {
	return m.dbPath
}
