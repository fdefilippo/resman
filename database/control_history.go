package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/fdefilippo/resman/internal/controlhistory"
)

const historyTimeLayout = "2006-01-02T15:04:05.000000000Z"
const controlHistorySchema = `
 CREATE TABLE IF NOT EXISTS control_history (
  session_id TEXT NOT NULL,
  cycle_id INTEGER NOT NULL CHECK (cycle_id > 0),
  finished_at TEXT NOT NULL,
  payload TEXT NOT NULL,
  PRIMARY KEY (session_id, cycle_id)
 );
 CREATE INDEX IF NOT EXISTS idx_control_history_finished ON control_history(finished_at);
`

// migrateMetricsSchema10To11 preserves observations and adds empty decision history.
// No historical decision or reason can be reconstructed from observation rows.
func (m *DatabaseManager) migrateMetricsSchema10To11() error {
	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema 10 to 11 migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(strings.ReplaceAll(controlHistorySchema, "IF NOT EXISTS ", "")); err != nil {
		return fmt.Errorf("add control history: %w", err)
	}
	if _, err = tx.Exec("PRAGMA user_version = 11"); err != nil {
		return fmt.Errorf("version control history schema: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit control history migration: %w", err)
	}
	return nil
}

// WriteControlHistory stores a bounded retry batch atomically and idempotently.
func (m *DatabaseManager) WriteControlHistory(ctx context.Context, entries []controlhistory.Entry) error {
	if len(entries) > controlhistory.MaxQueryEntries {
		return fmt.Errorf("control history batch exceeds %d entries", controlhistory.MaxQueryEntries)
	}
	leave := m.dbGate.Enter()
	defer leave()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin control history batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range entries {
		if e.SessionID == "" || e.CycleID <= 0 || e.Timestamp.IsZero() {
			return fmt.Errorf("invalid control history identity or timestamp")
		}
		body, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode control history: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO control_history(session_id,cycle_id,finished_at,payload) VALUES(?,?,?,?) ON CONFLICT(session_id,cycle_id) DO NOTHING`, e.SessionID, e.CycleID, e.Timestamp.UTC().Format(historyTimeLayout), string(body)); err != nil {
			return fmt.Errorf("insert control history: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit control history batch: %w", err)
	}
	return nil
}

// ReadControlHistory returns the most recent bounded window in chronological order.
func (m *DatabaseManager) ReadControlHistory(ctx context.Context, limit int) ([]controlhistory.Entry, error) {
	leave := m.dbGate.Enter()
	defer leave()
	rows, err := m.db.QueryContext(ctx, `SELECT session_id,cycle_id,payload FROM control_history ORDER BY finished_at DESC, session_id DESC, cycle_id DESC LIMIT ?`, controlhistory.QueryLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("query control history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := make([]controlhistory.Entry, 0)
	for rows.Next() {
		var session, body string
		var cycle int64
		if err := rows.Scan(&session, &cycle, &body); err != nil {
			return nil, fmt.Errorf("scan control history: %w", err)
		}
		var e controlhistory.Entry
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("decode control history: %w", err)
		}
		if e.SessionID != session || e.CycleID != cycle {
			return nil, fmt.Errorf("control history identity mismatch")
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read control history rows: %w", err)
	}
	return controlhistory.Merge(entries, nil, limit), nil
}

func historyRetentionCutoff(now time.Time, days int) string {
	return now.UTC().AddDate(0, 0, -days).Format(historyTimeLayout)
}

func (m *DatabaseManager) validateControlHistorySchema() error {
	rows, err := m.db.Query("PRAGMA table_info(control_history)")
	if err != nil {
		return fmt.Errorf("inspect control history schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	expected := []struct {
		name, typ string
		pk        int
	}{{"session_id", "TEXT", 1}, {"cycle_id", "INTEGER", 2}, {"finished_at", "TEXT", 0}, {"payload", "TEXT", 0}}
	count := 0
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("inspect control history column: %w", err)
		}
		if count >= len(expected) || name != expected[count].name || typ != expected[count].typ || pk != expected[count].pk || notNull != 1 {
			return fmt.Errorf("incompatible control history schema")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect control history columns: %w", err)
	}
	if count != len(expected) {
		return fmt.Errorf("incomplete control history schema")
	}
	return nil
}
