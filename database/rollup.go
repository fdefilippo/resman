package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/fdefilippo/resman/internal/metricrollup"
)

const rollupSchema = `
 CREATE TABLE IF NOT EXISTS metric_rollups (
  hour_start TEXT NOT NULL,
  scope TEXT NOT NULL CHECK(scope IN ('user','system')),
  uid INTEGER NOT NULL,
  username TEXT NOT NULL,
  enforcement_mode TEXT NOT NULL,
  denominator_state TEXT NOT NULL,
  total_cores INTEGER NOT NULL,
  metric TEXT NOT NULL,
  samples INTEGER NOT NULL,
  known_samples INTEGER NOT NULL,
  value_sum REAL,
  value_min REAL,
  value_max REAL,
  first_sample_at TEXT NOT NULL,
  last_sample_at TEXT NOT NULL,
  PRIMARY KEY(hour_start,scope,uid,username,enforcement_mode,denominator_state,total_cores,metric)
 );
 CREATE INDEX IF NOT EXISTS idx_metric_rollups_hour ON metric_rollups(hour_start);
 CREATE INDEX IF NOT EXISTS idx_metric_rollups_scope_uid_hour ON metric_rollups(scope,uid,hour_start);
`

// migrateMetricsSchema11To12 adds only empty observation stores, preserving old meaning.
func (m *DatabaseManager) migrateMetricsSchema11To12() error {
	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema 11 to 12 migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(strings.ReplaceAll(rollupSchema+processActivitySchema, "IF NOT EXISTS ", "")); err != nil {
		return fmt.Errorf("add observation stores: %w", err)
	}
	if _, err := tx.Exec("PRAGMA user_version = 12"); err != nil {
		return fmt.Errorf("version observation stores: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit observation stores migration: %w", err)
	}
	return nil
}

func (m *DatabaseManager) validateRollupSchema() error {
	columns, err := m.db.Query("PRAGMA table_info(metric_rollups)")
	if err != nil {
		return fmt.Errorf("inspect rollup schema: %w", err)
	}
	defer func() { _ = columns.Close() }()
	expected := []struct {
		name, typ string
		pk        int
	}{
		{"hour_start", "TEXT", 1}, {"scope", "TEXT", 2}, {"uid", "INTEGER", 3}, {"username", "TEXT", 4}, {"enforcement_mode", "TEXT", 5}, {"denominator_state", "TEXT", 6}, {"total_cores", "INTEGER", 7}, {"metric", "TEXT", 8},
		{"samples", "INTEGER", 0}, {"known_samples", "INTEGER", 0}, {"value_sum", "REAL", 0}, {"value_min", "REAL", 0}, {"value_max", "REAL", 0}, {"first_sample_at", "TEXT", 0}, {"last_sample_at", "TEXT", 0},
	}
	i := 0
	for columns.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if err := columns.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			return fmt.Errorf("scan rollup schema: %w", err)
		}
		if i >= len(expected) || name != expected[i].name || typ != expected[i].typ || pk != expected[i].pk || (i < 10 || i > 12) && nn != 1 {
			return fmt.Errorf("incompatible rollup schema")
		}
		i++
	}
	if err := columns.Err(); err != nil {
		return fmt.Errorf("read rollup schema: %w", err)
	}
	if i != len(expected) {
		return fmt.Errorf("incomplete rollup schema")
	}
	return nil
}

type rollupMetricColumn struct {
	metric metricrollup.Metric
	column string
}

var userRollupMetrics = []rollupMetricColumn{
	{metricrollup.CPUUsagePercent, "cpu_usage_percent"}, {metricrollup.MemoryUsageBytes, "memory_usage_bytes"}, {metricrollup.ProcessCount, "process_count"}, {metricrollup.RAMCgroupUsageBytes, "ram_cgroup_usage_bytes"},
	{metricrollup.UnitCPUUsageUsecDelta, "leaf_cpu_usage_usec_delta"}, {metricrollup.MemoryHighEventsDelta, "memory_high_events_delta"}, {metricrollup.MemoryOOMKillEventsDelta, "memory_oom_kill_events_delta"},
	{metricrollup.CPULimitActive, "cpu_limit_active"}, {metricrollup.RAMLimitActive, "ram_limit_active"}, {metricrollup.IOLimitActive, "io_limit_active"},
}
var systemRollupMetrics = []rollupMetricColumn{
	{metricrollup.ParentCPUUsageUsecDelta, "parent_cpu_usage_usec_delta"}, {metricrollup.ParentCPUThrottledUsecDelta, "parent_cpu_throttled_usec_delta"}, {metricrollup.CPUActivelyLimitedUsersCount, "cpu_actively_limited_users_count"},
	{metricrollup.CPULimitActive, "cpu_limits_active"}, {metricrollup.ResourceLimitsActive, "resource_limits_active"}, {metricrollup.AnyLimitsActive, "any_limits_active"},
}

// compactMetricRows consumes exactly the rows deleted by retention in the same transaction.
func compactMetricRows(tx *sql.Tx, cutoff time.Time) error {
	for _, family := range []struct {
		table, scope, dimensions string
		metrics                  []rollupMetricColumn
	}{
		{"user_metrics", "user", "uid,username,'','',0", userRollupMetrics},
		{"system_metrics", "system", "-1,'',enforcement_mode,denominator_state,total_cores", systemRollupMetrics},
	} {
		for _, metric := range family.metrics {
			// All identifiers and metric names below are fixed producer-owned inventory.
			statement := fmt.Sprintf(`INSERT INTO metric_rollups(hour_start,scope,uid,username,enforcement_mode,denominator_state,total_cores,metric,samples,known_samples,value_sum,value_min,value_max,first_sample_at,last_sample_at)
    SELECT replace(substr(CAST(timestamp AS TEXT),1,13),' ','T')||':00:00Z','%s',%s,'%s',COUNT(*),COUNT(%s),SUM(CAST(%s AS REAL)),MIN(%s),MAX(%s),MIN(CAST(timestamp AS TEXT)),MAX(CAST(timestamp AS TEXT))
    FROM %s WHERE timestamp < ? GROUP BY 1,2,3,4,5,6,7,8
    ON CONFLICT(hour_start,scope,uid,username,enforcement_mode,denominator_state,total_cores,metric) DO UPDATE SET
     samples=metric_rollups.samples+excluded.samples, known_samples=metric_rollups.known_samples+excluded.known_samples,
     value_sum=CASE WHEN metric_rollups.value_sum IS NULL AND excluded.value_sum IS NULL THEN NULL ELSE COALESCE(metric_rollups.value_sum,0)+COALESCE(excluded.value_sum,0) END,
     value_min=CASE WHEN metric_rollups.value_min IS NULL THEN excluded.value_min WHEN excluded.value_min IS NULL THEN metric_rollups.value_min ELSE MIN(metric_rollups.value_min,excluded.value_min) END,
     value_max=CASE WHEN metric_rollups.value_max IS NULL THEN excluded.value_max WHEN excluded.value_max IS NULL THEN metric_rollups.value_max ELSE MAX(metric_rollups.value_max,excluded.value_max) END,
     first_sample_at=MIN(metric_rollups.first_sample_at,excluded.first_sample_at),last_sample_at=MAX(metric_rollups.last_sample_at,excluded.last_sample_at)`, family.scope, family.dimensions, metric.metric, metric.column, metric.column, metric.column, metric.column, family.table)
			if _, err := tx.Exec(statement, cutoff); err != nil {
				return fmt.Errorf("compact %s %s: %w", family.scope, metric.metric, err)
			}
		}
	}
	return nil
}

// GetMetricRollups returns retained compacted observations; raw rows are not implicitly mixed in.
func (m *DatabaseManager) GetMetricRollups(ctx context.Context, q metricrollup.Query) ([]metricrollup.Entry, error) {
	if q.Scope != metricrollup.User && q.Scope != metricrollup.System {
		return nil, fmt.Errorf("rollup scope must be user or system")
	}
	if q.Start.IsZero() || q.End.IsZero() || !q.Start.Before(q.End) {
		return nil, fmt.Errorf("rollup time range must be increasing and explicit")
	}
	if q.UID != nil && (q.Scope != metricrollup.User || *q.UID < 0) {
		return nil, fmt.Errorf("rollup UID requires nonnegative user scope")
	}
	leave, err := m.dbGate.EnterContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("enter metric rollup query: %w", err)
	}
	defer leave()
	query := `SELECT hour_start,scope,uid,username,enforcement_mode,denominator_state,total_cores,metric,samples,known_samples,value_sum,value_min,value_max,first_sample_at,last_sample_at FROM metric_rollups WHERE scope=? AND hour_start>=? AND hour_start<?`
	endExclusive := q.End.UTC().Truncate(time.Hour)
	if !endExclusive.Equal(q.End.UTC()) {
		endExclusive = endExclusive.Add(time.Hour)
	}
	args := []any{q.Scope, q.Start.UTC().Truncate(time.Hour).Format(time.RFC3339), endExclusive.Format(time.RFC3339)}
	if q.UID != nil {
		query += " AND uid=?"
		args = append(args, *q.UID)
	}
	query += " ORDER BY hour_start DESC,uid,username,enforcement_mode,denominator_state,total_cores,metric LIMIT ?"
	args = append(args, metricrollup.Limit(q.Limit))
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query metric rollups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := make([]metricrollup.Entry, 0)
	for rows.Next() {
		var e metricrollup.Entry
		var sum, minValue, maxValue sql.NullFloat64
		if err := rows.Scan(&e.HourStart, &e.Scope, &e.UID, &e.Username, &e.EnforcementMode, &e.DenominatorState, &e.TotalCores, &e.Metric, &e.Samples, &e.KnownSamples, &sum, &minValue, &maxValue, &e.FirstSampleAt, &e.LastSampleAt); err != nil {
			return nil, fmt.Errorf("scan metric rollup: %w", err)
		}
		for _, timestamp := range []*string{&e.FirstSampleAt, &e.LastSampleAt} {
			at, parseErr := time.Parse("2006-01-02 15:04:05.999999999Z07:00", *timestamp)
			if parseErr != nil {
				return nil, fmt.Errorf("decode rollup sample timestamp: %w", parseErr)
			}
			*timestamp = at.UTC().Format(time.RFC3339Nano)
		}
		e.UnknownSamples = e.Samples - e.KnownSamples
		if sum.Valid {
			e.Sum = &sum.Float64
		}
		if minValue.Valid {
			e.Minimum = &minValue.Float64
		}
		if maxValue.Valid {
			e.Maximum = &maxValue.Float64
		}
		if e.KnownSamples > 0 && sum.Valid {
			average := sum.Float64 / float64(e.KnownSamples)
			e.SampleAverage = &average
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read metric rollups: %w", err)
	}
	return entries, nil
}
