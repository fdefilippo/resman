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
package mcp

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/database"
)

func TestResolveHistoryTimeRangeRejectsInvalidExplicitTimes(t *testing.T) {
	now := time.Date(2026, time.July, 25, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		args       GetHistoryArgs
		errorField string
	}{
		{
			name:       "invalid start time",
			args:       GetHistoryArgs{StartTime: "2026-07-24 08:00:00"},
			errorField: "startTime",
		},
		{
			name:       "invalid end time",
			args:       GetHistoryArgs{EndTime: "2026-07-25 08:00:00"},
			errorField: "endTime",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := resolveHistoryTimeRange(tt.args, now)
			if err == nil {
				t.Fatal("resolveHistoryTimeRange() accepted an invalid explicit time")
			}
			if !strings.Contains(err.Error(), tt.errorField) {
				t.Fatalf("resolveHistoryTimeRange() error = %q, want field %q", err, tt.errorField)
			}
		})
	}
}

func TestResolveHistoryTimeRangeAppliesExplicitTimes(t *testing.T) {
	now := time.Date(2026, time.July, 25, 10, 0, 0, 0, time.UTC)
	explicitStart := "2026-07-24T08:00:00Z"
	explicitEnd := "2026-07-24T18:00:00Z"

	start, end, err := resolveHistoryTimeRange(GetHistoryArgs{
		Period:    "last_7_days",
		StartTime: explicitStart,
		EndTime:   explicitEnd,
	}, now)
	if err != nil {
		t.Fatalf("resolveHistoryTimeRange() error = %v", err)
	}

	wantStart, err := time.Parse(time.RFC3339, explicitStart)
	if err != nil {
		t.Fatalf("time.Parse() error = %v", err)
	}
	wantEnd, err := time.Parse(time.RFC3339, explicitEnd)
	if err != nil {
		t.Fatalf("time.Parse() error = %v", err)
	}
	if !start.Equal(wantStart) {
		t.Errorf("start = %s, want %s", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %s, want %s", end, wantEnd)
	}
}

func TestNormalizeHistoryLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "negative uses default", limit: -1, want: defaultHistoryLimit},
		{name: "zero uses default", limit: 0, want: defaultHistoryLimit},
		{name: "positive is retained", limit: 25, want: 25},
		{name: "maximum is retained", limit: maxHistoryLimit, want: maxHistoryLimit},
		{name: "above maximum is capped", limit: maxHistoryLimit + 1, want: maxHistoryLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeHistoryLimit(tt.limit); got != tt.want {
				t.Errorf("normalizeHistoryLimit(%d) = %d, want %d", tt.limit, got, tt.want)
			}
		})
	}
}

func TestReportMetricExtraction(t *testing.T) {
	metrics := map[string]any{
		"total_cores":     4,
		"memory_usage_mb": 1024.0,
		"total_memory_mb": 16384.0,
	}

	if got := totalCPUCapacityPercent(metrics); got != 400 {
		t.Errorf("totalCPUCapacityPercent() = %.1f, want 400.0", got)
	}
	if got := totalSystemMemoryMB(metrics); got != 16384 {
		t.Errorf("totalSystemMemoryMB() = %.1f, want 16384.0", got)
	}
}

func TestExtractCgroupMemoryMetrics(t *testing.T) {
	current, hasCurrent, max, high := extractCgroupMemoryMetrics(map[string]string{
		"memory.current": "1048576",
		"memory.max":     "max",
		"memory.high":    "2097152",
	})

	if !hasCurrent || current != 1048576 {
		t.Errorf("current = %d, present = %t; want 1048576, true", current, hasCurrent)
	}
	if max != "max" {
		t.Errorf("max = %q, want %q", max, "max")
	}
	if high != "2097152" {
		t.Errorf("high = %q, want %q", high, "2097152")
	}
}

func TestUserMetricJSONUsesExplicitCgroupMemoryFields(t *testing.T) {
	data, err := json.Marshal(UserMetric{
		CgroupMemoryCurrentBytes: 1048576,
		MemoryMax:                "max",
		MemoryHigh:               "2097152",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	payload := string(data)
	for _, field := range []string{"cgroup_memory_current_bytes", "memory_max", "memory_high"} {
		if !strings.Contains(payload, `"`+field+`"`) {
			t.Errorf("JSON payload %s does not contain field %q", payload, field)
		}
	}
	if strings.Contains(payload, `"memory_max_bytes"`) {
		t.Errorf("JSON payload %s contains obsolete field memory_max_bytes", payload)
	}
}

func TestActivationResultReflectsRuntimeState(t *testing.T) {
	tests := []struct {
		name         string
		force        bool
		limitsActive bool
		err          error
		wantSuccess  bool
		wantMessage  string
	}{
		{
			name:         "control cycle activates limits",
			limitsActive: true,
			wantSuccess:  true,
			wantMessage:  "Limits activated successfully",
		},
		{
			name:        "control cycle maintains current state",
			wantMessage: "activation conditions were not met",
		},
		{
			name:        "forced activation leaves limits inactive",
			force:       true,
			wantMessage: "limits are not active",
		},
		{
			name:        "activation fails",
			err:         errors.New("cgroup unavailable"),
			wantMessage: "cgroup unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := activationResult(tt.force, tt.limitsActive, tt.err)
			if result.Success != tt.wantSuccess {
				t.Errorf("Success = %t, want %t", result.Success, tt.wantSuccess)
			}
			if !strings.Contains(result.Message, tt.wantMessage) {
				t.Errorf("Message = %q, want substring %q", result.Message, tt.wantMessage)
			}
		})
	}
}

func TestResolveHistoricalUIDUsesDatabaseForInactiveUser(t *testing.T) {
	dbManager, err := database.NewDatabaseManager(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error = %v", err)
	}
	defer func() { _ = dbManager.Close() }()

	now := time.Now().UTC()
	if err := dbManager.WriteUserMetrics(&database.UserMetricsRecord{
		Timestamp:    now,
		UID:          1000,
		Username:     "offline-user",
		ProcessCount: 1,
	}); err != nil {
		t.Fatalf("WriteUserMetrics() error = %v", err)
	}

	server := &Server{dbManager: dbManager}
	uid, err := server.resolveHistoricalUID(
		GetHistoryArgs{Username: "offline-user"},
		now.Add(-time.Hour),
		now.Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("resolveHistoricalUID() error = %v", err)
	}
	if uid != 1000 {
		t.Errorf("resolveHistoricalUID() = %d, want 1000", uid)
	}
}
