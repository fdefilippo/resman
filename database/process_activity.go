package database

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fdefilippo/resman/internal/processactivity"
)

const processActivitySchema = `
CREATE TABLE IF NOT EXISTS process_activity (
 boot_id TEXT NOT NULL,
 pid INTEGER NOT NULL CHECK(pid > 0),
 start_ticks TEXT NOT NULL,
 last_seen_at TEXT NOT NULL,
 uid INTEGER NOT NULL,
 runtime_seconds REAL NOT NULL,
 average_cpu_percent REAL NOT NULL,
 cpu_covered_seconds REAL NOT NULL,
 payload TEXT NOT NULL,
 PRIMARY KEY(boot_id,pid,start_ticks)
);
CREATE INDEX IF NOT EXISTS idx_process_activity_last_seen ON process_activity(last_seen_at);
CREATE INDEX IF NOT EXISTS idx_process_activity_uid ON process_activity(uid,last_seen_at);
`

func (m *DatabaseManager) validateProcessActivitySchema() error {
	rows, err := m.db.Query("PRAGMA table_info(process_activity)")
	if err != nil {
		return fmt.Errorf("inspect process activity schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	expected := []string{"boot_id", "pid", "start_ticks", "last_seen_at", "uid", "runtime_seconds", "average_cpu_percent", "cpu_covered_seconds", "payload"}
	count := 0
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("inspect process activity column: %w", err)
		}
		expectedTypes := []string{"TEXT", "INTEGER", "TEXT", "TEXT", "INTEGER", "REAL", "REAL", "REAL", "TEXT"}
		expectedPK := 0
		if count < 3 {
			expectedPK = count + 1
		}
		if count >= len(expected) || name != expected[count] || notnull != 1 || typ != expectedTypes[count] || pk != expectedPK {
			return fmt.Errorf("incompatible process activity schema")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect process activity schema: %w", err)
	}
	if count != len(expected) {
		return fmt.Errorf("incomplete process activity schema")
	}
	return nil
}

// UpsertProcessActivity atomically replaces latest summaries without replaying deltas.
func (m *DatabaseManager) UpsertProcessActivity(ctx context.Context, entries []processactivity.Entry) error {
	if len(entries) > processactivity.MaxEntries {
		return fmt.Errorf("process activity batch exceeds %d", processactivity.MaxEntries)
	}
	leave, err := m.dbGate.EnterContext(ctx)
	if err != nil {
		return fmt.Errorf("enter process activity write: %w", err)
	}
	defer leave()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin process activity: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range entries {
		if !e.Valid() || e.LastSeenAt.IsZero() {
			return fmt.Errorf("invalid process activity identity or timestamp")
		}
		body, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode process activity: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO process_activity(boot_id,pid,start_ticks,last_seen_at,uid,runtime_seconds,average_cpu_percent,cpu_covered_seconds,payload) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(boot_id,pid,start_ticks) DO UPDATE SET last_seen_at=excluded.last_seen_at,uid=excluded.uid,runtime_seconds=excluded.runtime_seconds,average_cpu_percent=excluded.average_cpu_percent,cpu_covered_seconds=excluded.cpu_covered_seconds,payload=excluded.payload WHERE excluded.last_seen_at >= process_activity.last_seen_at`, e.BootID, e.PID, e.StartTicks, e.LastSeenAt.UTC().Format(historyTimeLayout), e.UID, e.RuntimeSeconds, e.AverageCPUPercent, e.CPUCoveredWallSeconds, string(body))
		if err != nil {
			return fmt.Errorf("upsert process activity: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit process activity: %w", err)
	}
	return nil
}

// QueryProcessActivity filters before limiting, so a UID query cannot miss older matches.
// Durable records alone never assert that a process is active in the current scan.
func (m *DatabaseManager) QueryProcessActivity(ctx context.Context, q processactivity.Query) ([]processactivity.Entry, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	leave, err := m.dbGate.EnterContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("enter process activity query: %w", err)
	}
	defer leave()
	sqlQuery := `SELECT boot_id,pid,start_ticks,payload FROM process_activity WHERE runtime_seconds >= ? AND cpu_covered_seconds >= ? AND (? = 0 OR (cpu_covered_seconds > 0 AND average_cpu_percent >= ?))`
	args := []any{q.MinRuntimeSeconds, q.MinCoveredSeconds, q.MinAverageCPUPercent, q.MinAverageCPUPercent}
	if q.UID != nil {
		sqlQuery += " AND uid = ?"
		args = append(args, *q.UID)
	}
	sqlQuery += " ORDER BY last_seen_at DESC,boot_id,pid,start_ticks LIMIT ?"
	args = append(args, q.BoundedLimit()+processactivity.MaxEntries)
	rows, err := m.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query process activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := make([]processactivity.Entry, 0)
	for rows.Next() {
		var boot, ticks, body string
		var pid int
		if err := rows.Scan(&boot, &pid, &ticks, &body); err != nil {
			return nil, fmt.Errorf("scan process activity: %w", err)
		}
		var e processactivity.Entry
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("decode process activity: %w", err)
		}
		if e.BootID != boot || e.PID != pid || e.StartTicks != ticks {
			return nil, fmt.Errorf("process activity identity mismatch")
		}
		e.Active = false
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read process activity: %w", err)
	}
	return entries, nil
}

// RestoreProcessActivity returns a bounded same-boot restart snapshot, not a baseline.
func (m *DatabaseManager) RestoreProcessActivity(ctx context.Context, boot string) ([]processactivity.Entry, error) {
	leave, err := m.dbGate.EnterContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("enter process activity restore: %w", err)
	}
	defer leave()
	rows, err := m.db.QueryContext(ctx, `SELECT boot_id,pid,start_ticks,payload FROM process_activity WHERE boot_id = ? ORDER BY last_seen_at DESC LIMIT ?`, boot, processactivity.MaxEntries)
	if err != nil {
		return nil, fmt.Errorf("restore process activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := make([]processactivity.Entry, 0)
	for rows.Next() {
		var body, rowBoot, rowTicks string
		var rowPID int
		if err := rows.Scan(&rowBoot, &rowPID, &rowTicks, &body); err != nil {
			return nil, fmt.Errorf("scan restored process activity: %w", err)
		}
		var e processactivity.Entry
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("decode restored process activity: %w", err)
		}
		if e.BootID != boot || rowBoot != boot || e.PID != rowPID || e.StartTicks != rowTicks {
			return nil, fmt.Errorf("restored process boot mismatch")
		}
		e.Active = false
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read restored process activity: %w", err)
	}
	return entries, nil
}
