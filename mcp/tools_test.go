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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

type configurationReloaderFunc func(context.Context) error

func (f configurationReloaderFunc) Reload(ctx context.Context) error {
	return f(ctx)
}

type recordedServerLog struct {
	message string
	fields  []interface{}
}

type recordingServerLogger struct {
	warnings []recordedServerLog
}

func (*recordingServerLogger) Info(string, ...interface{})  {}
func (*recordingServerLogger) Error(string, ...interface{}) {}

func (l *recordingServerLogger) Warn(message string, fields ...interface{}) {
	l.warnings = append(l.warnings, recordedServerLog{
		message: message,
		fields:  append([]interface{}(nil), fields...),
	})
}

type stateConfigChangeHandler struct {
	manager *state.Manager
}

func (h *stateConfigChangeHandler) OnConfigChange(cfg *config.Config) error {
	h.manager.UpdateConfig(cfg)
	return nil
}

func newUserFilterTestServer(t *testing.T, reloader ConfigurationReloader) (*Server, string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	content := []byte("USER_INCLUDE_LIST=^old-include$\nUSER_EXCLUDE_LIST=^old-exclude$\n")
	if err := os.WriteFile(configPath, content, 0600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	runtimeConfig, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate() error: %v", err)
	}
	manager, err := state.NewManager(runtimeConfig, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	return &Server{
		cfg:            &config.MCPServerConfig{AllowWriteOps: true},
		stateManager:   manager,
		configReloader: reloader,
	}, configPath
}

func TestUserFilterUpdateWaitsForWatcherAcknowledgement(t *testing.T) {
	tests := []struct {
		name     string
		kind     userFilterKind
		patterns []string
		current  func(*config.Config) []string
	}{
		{
			name:     "include",
			kind:     userFilterInclude,
			patterns: []string{"^service$", "^batch-"},
			current:  (*config.Config).GetUserIncludeList,
		},
		{
			name:     "exclude",
			kind:     userFilterExclude,
			patterns: []string{"^root$", "^system-"},
			current:  (*config.Config).GetUserExcludeList,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, configPath := newUserFilterTestServer(t, nil)
			watcher, err := config.NewWatcher(configPath, server.stateManager.GetConfig(), &stateConfigChangeHandler{
				manager: server.stateManager,
			})
			if err != nil {
				t.Fatalf("NewWatcher() error: %v", err)
			}
			if err := watcher.Start(); err != nil {
				t.Fatalf("Start() error: %v", err)
			}
			t.Cleanup(func() {
				if err := watcher.Stop(); err != nil {
					t.Errorf("Stop() error: %v", err)
				}
			})
			server.configReloader = watcher

			result, err := server.updateUserFilter(context.Background(), tt.kind, tt.patterns)
			if err != nil {
				t.Fatalf("updateUserFilter() error: %v", err)
			}
			if !result.Success || !result.Persisted || !result.Applied {
				t.Fatalf("update result = %+v, want success, persisted, and applied", result)
			}
			if got := tt.current(server.stateManager.GetConfig()); !slices.Equal(got, tt.patterns) {
				t.Fatalf("runtime filters = %v, want %v", got, tt.patterns)
			}
		})
	}
}

func TestUserFilterUpdateEmitsOneBoundedLegacyCleanupNotice(t *testing.T) {
	server, configPath := newUserFilterTestServer(t, nil)
	server.configReloader = configurationReloaderFunc(func(context.Context) error {
		reloaded, err := config.LoadAndValidate(configPath)
		if err != nil {
			return err
		}
		server.stateManager.UpdateConfig(reloaded)
		return nil
	})
	logger := &recordingServerLogger{}
	server.logger = logger

	legacyNames := []string{
		"resman.conf.backup_20260821_010101",
		"resman.conf.backup_20260821_020202",
		"resman.conf.backup_20260821_030303",
		"resman.conf.backup_20260821_040404",
		"resman.conf.backup_20260821_050505",
	}
	for _, name := range legacyNames {
		if err := os.WriteFile(
			filepath.Join(filepath.Dir(configPath), name),
			[]byte("MCP_AUTH_TOKEN=cleanup-secret\n"),
			0600,
		); err != nil {
			t.Fatalf("os.WriteFile(%s) error = %v", name, err)
		}
	}

	result, err := server.updateUserFilter(context.Background(), userFilterInclude, []string{"^new$"})
	if err != nil {
		t.Fatalf("updateUserFilter() error = %v", err)
	}
	if !result.Success {
		t.Fatalf("update result = %+v, want success", result)
	}
	if len(logger.warnings) != 1 {
		t.Fatalf("cleanup warnings = %d, want exactly one: %+v", len(logger.warnings), logger.warnings)
	}
	warning := logger.warnings[0]
	fields := make(map[string]interface{}, len(warning.fields)/2)
	for index := 0; index+1 < len(warning.fields); index += 2 {
		fields[fmt.Sprint(warning.fields[index])] = warning.fields[index+1]
	}
	if fields["removed_count"] != 5 || fields["omitted_count"] != 2 {
		t.Fatalf("cleanup notice fields = %v, want removed_count=5 omitted_count=2", fields)
	}
	wantVisible := strings.Join(legacyNames[:maxLegacyArtifactNamesInCleanupNotice], ",")
	if fields["removed_basenames"] != wantVisible {
		t.Fatalf("visible cleanup basenames = %v, want %q", fields["removed_basenames"], wantVisible)
	}
	notice := warning.message + fmt.Sprint(warning.fields)
	for _, forbidden := range []string{"cleanup-secret", legacyNames[3], legacyNames[4], filepath.Dir(configPath)} {
		if strings.Contains(notice, forbidden) {
			t.Fatalf("bounded cleanup notice exposed %q: %s", forbidden, notice)
		}
	}
}

func TestUserFilterUpdateReportsPartialLegacyCleanupBeforeReturningError(t *testing.T) {
	server, configPath := newUserFilterTestServer(t, configurationReloaderFunc(func(context.Context) error {
		t.Fatal("configuration reloader called after partial persistence failure")
		return nil
	}))
	logger := &recordingServerLogger{}
	server.logger = logger
	dir := filepath.Dir(configPath)
	removedName := "resman.conf.backup_20260821_010101"
	blockedName := "resman.conf.backup_20260821_020202"
	if err := os.WriteFile(filepath.Join(dir, removedName), []byte("MCP_AUTH_TOKEN=partial-secret\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile(legacy backup) error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, blockedName), 0700); err != nil {
		t.Fatalf("os.Mkdir(blocking artifact) error = %v", err)
	}

	result, err := server.updateUserFilter(context.Background(), userFilterInclude, []string{"^new$"})
	if err == nil || !strings.Contains(err.Error(), blockedName) {
		t.Fatalf("updateUserFilter() error = %v, want named partial cleanup failure", err)
	}
	if result.Persisted || result.Applied || result.Success {
		t.Fatalf("update result = %+v, want failed persistence without runtime apply", result)
	}
	if len(logger.warnings) != 1 {
		t.Fatalf("partial cleanup warnings = %d, want exactly one: %+v", len(logger.warnings), logger.warnings)
	}
	warning := logger.warnings[0]
	fields := make(map[string]interface{}, len(warning.fields)/2)
	for index := 0; index+1 < len(warning.fields); index += 2 {
		fields[fmt.Sprint(warning.fields[index])] = warning.fields[index+1]
	}
	if fields["removed_count"] != 1 || fields["removed_basenames"] != removedName || fields["omitted_count"] != 0 {
		t.Fatalf("partial cleanup notice fields = %v, want one reported basename", fields)
	}
	if strings.Contains(warning.message+fmt.Sprint(warning.fields), "partial-secret") {
		t.Fatalf("partial cleanup notice exposed file contents: %+v", warning)
	}
}

func TestUserFilterUpdateDoesNotPublishBeforeAcknowledgement(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server, configPath := newUserFilterTestServer(t, nil)
	server.configReloader = configurationReloaderFunc(func(context.Context) error {
		close(entered)
		<-release
		reloaded, err := config.LoadAndValidate(configPath)
		if err != nil {
			return err
		}
		server.stateManager.UpdateConfig(reloaded)
		return nil
	})

	type updateOutcome struct {
		result userFilterUpdateResult
		err    error
	}
	done := make(chan updateOutcome, 1)
	go func() {
		result, err := server.updateUserFilter(context.Background(), userFilterInclude, []string{"^new$"})
		done <- updateOutcome{result: result, err: err}
	}()
	<-entered

	if got := server.stateManager.GetConfig().GetUserIncludeList(); !slices.Equal(got, []string{"^old-include$"}) {
		t.Fatalf("runtime filters before acknowledgement = %v, want old value", got)
	}
	close(release)
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("updateUserFilter() error: %v", outcome.err)
	}
	if !outcome.result.Applied {
		t.Fatalf("update result = %+v, want applied", outcome.result)
	}
}

func TestUserFilterUpdateReportsWatcherFailureAfterPersistence(t *testing.T) {
	reloadErr := errors.New("watcher apply failed")
	server, configPath := newUserFilterTestServer(t, configurationReloaderFunc(func(context.Context) error {
		return reloadErr
	}))

	result, err := server.updateUserFilter(context.Background(), userFilterExclude, []string{"^new$"})
	if !errors.Is(err, reloadErr) {
		t.Fatalf("updateUserFilter() error = %v, want watcher error", err)
	}
	if !result.Persisted || result.Applied || result.Success {
		t.Fatalf("update result = %+v, want persisted but not applied or successful", result)
	}
	persisted, loadErr := config.LoadAndValidate(configPath)
	if loadErr != nil {
		t.Fatalf("LoadAndValidate() error: %v", loadErr)
	}
	if got := persisted.GetUserExcludeList(); !slices.Equal(got, []string{"^new$"}) {
		t.Fatalf("persisted filters = %v, want [^new$]", got)
	}
	if got := server.stateManager.GetConfig().GetUserExcludeList(); !slices.Equal(got, []string{"^old-exclude$"}) {
		t.Fatalf("runtime filters = %v, want old value", got)
	}
}

func TestUserFilterUpdateRejectsSymlinkedConfigBeforeWatcherReload(t *testing.T) {
	server, configPath := newUserFilterTestServer(t, configurationReloaderFunc(func(context.Context) error {
		t.Fatal("configuration reloader called after persistence rejected a symbolic link")
		return nil
	}))
	targetPath := filepath.Join(filepath.Dir(configPath), "managed-target.conf")
	if err := os.Rename(configPath, targetPath); err != nil {
		t.Fatalf("os.Rename(config target) error = %v", err)
	}
	if err := os.Symlink(targetPath, configPath); err != nil {
		t.Fatalf("os.Symlink(config) error = %v", err)
	}
	original, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("os.ReadFile(target) error = %v", err)
	}

	result, err := server.updateUserFilter(context.Background(), userFilterInclude, []string{"^new$"})
	if err == nil || !strings.Contains(err.Error(), configPath) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("updateUserFilter() error = %v, want named symbolic-link rejection", err)
	}
	if result.Persisted || result.Applied || result.Success {
		t.Fatalf("rejected update result = %+v, want no persisted or applied state", result)
	}
	if got := server.stateManager.GetConfig().GetUserIncludeList(); !slices.Equal(got, []string{"^old-include$"}) {
		t.Fatalf("runtime filters = %v, want unchanged old value", got)
	}
	content, readErr := os.ReadFile(targetPath)
	if readErr != nil || string(content) != string(original) {
		t.Fatalf("symlink target changed: content=%q error=%v, want %q", content, readErr, original)
	}
}

func TestUserFilterUpdateRejectsConcurrentEdits(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server, configPath := newUserFilterTestServer(t, nil)
	server.configReloader = configurationReloaderFunc(func(context.Context) error {
		close(entered)
		<-release
		reloaded, err := config.LoadAndValidate(configPath)
		if err != nil {
			return err
		}
		server.stateManager.UpdateConfig(reloaded)
		return nil
	})

	firstDone := make(chan error, 1)
	go func() {
		_, err := server.updateUserFilter(context.Background(), userFilterInclude, []string{"^first$"})
		firstDone <- err
	}()
	<-entered

	result, err := server.updateUserFilter(context.Background(), userFilterInclude, []string{"^second$"})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent update error = %v, want explicit rejection", err)
	}
	if result.Persisted || result.Applied || result.Success {
		t.Fatalf("rejected update result = %+v, want no side effects", result)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first update error: %v", err)
	}

	persisted, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate() error: %v", err)
	}
	if got := persisted.GetUserIncludeList(); !slices.Equal(got, []string{"^first$"}) {
		t.Fatalf("persisted filters = %v, want first update", got)
	}
}

func TestParseUserFilterPatternsRejectsRemovedAndUnknownParameters(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "removed reload", args: map[string]any{"patterns": []any{".*"}, "reload": true}, want: "was removed"},
		{name: "unknown", args: map[string]any{"patterns": []any{".*"}, "future": true}, want: "unknown parameter"},
		{name: "non-string item", args: map[string]any{"patterns": []any{42}}, want: "must be a string"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseUserFilterPatterns(tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parseUserFilterPatterns() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMetricsDatabaseInfoUsesEffectiveRuntimeRetention(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MetricsDBRetentionDays = 11
	manager, err := state.NewManager(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	dbManager, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	defer func() { _ = dbManager.Close() }()
	server := &Server{stateManager: manager, dbManager: dbManager}
	_, result, err := server.handleGetMetricsDatabaseInfo(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("handleGetMetricsDatabaseInfo() error: %v", err)
	}
	if result.RetentionDays != 11 {
		t.Fatalf("effective retention = %d, want 11", result.RetentionDays)
	}

	reloaded := config.DefaultConfig()
	reloaded.MetricsDBRetentionDays = 23
	manager.UpdateConfig(reloaded)
	_, result, err = server.handleGetMetricsDatabaseInfo(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("handleGetMetricsDatabaseInfo() after reload error: %v", err)
	}
	if result.RetentionDays != 23 {
		t.Fatalf("effective retention after reload = %d, want 23", result.RetentionDays)
	}
}

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
	metrics := resmanmetrics.ObservationMetrics{
		TotalCores:    4,
		MemoryUsageMB: 1024,
		TotalMemoryMB: 16384,
	}

	if got := totalCPUCapacityPercent(metrics); got != 400 {
		t.Errorf("totalCPUCapacityPercent() = %.1f, want 400.0", got)
	}
	if got := totalSystemMemoryMB(metrics); got != 16384 {
		t.Errorf("totalSystemMemoryMB() = %.1f, want 16384.0", got)
	}
}

func TestStatusPayloadsKeepObservationAndRuntimeContractsDistinct(t *testing.T) {
	observation := resmanmetrics.ObservationMetrics{
		TotalCores:            8,
		TotalCPUUsage:         71.5,
		ObservedUsersCPUUsage: 54.25,
		ObservedUsersCount:    7,
		MemoryUsageMB:         2048,
		SystemUnderLoad:       true,
	}
	runtime := state.RuntimeStatus{
		AnyLimitsActive:              true,
		CPULimitsActive:              true,
		ResourceLimitsActive:         true,
		ActivelyLimitedUsers:         []int{1001, 1003},
		ActivelyLimitedUsersCount:    2,
		CPUActivelyLimitedUsers:      []int{1001},
		CPUActivelyLimitedUsersCount: 1,
		SharedCgroupPath:             "/sys/fs/cgroup/resman/limited",
		SharedCgroupActive:           true,
	}

	tests := []struct {
		name    string
		payload any
		want    map[string]any
	}{
		{
			name:    "system status",
			payload: newSystemStatusPayload("host-a", "worker", observation, runtime),
			want: map[string]any{
				"observed_users_cpu_usage":     54.25,
				"observed_users_count":         float64(7),
				"actively_limited_users_count": float64(2),
				"cpu_limits_active":            true,
				"resource_limits_active":       true,
			},
		},
		{
			name:    "limits status",
			payload: newLimitsStatusPayload("host-a", "worker", runtime),
			want: map[string]any{
				"actively_limited_users_count":     float64(2),
				"cpu_actively_limited_users_count": float64(1),
				"cpu_limits_active":                true,
				"resource_limits_active":           true,
			},
		},
	}

	legacyFields := []string{"total_user_cpu_usage", "user_cpu_usage", "active_users_count", "active_users", "limits_active", "limits_applied_time"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("%s = %#v, want %#v", key, got[key], want)
				}
			}
			for _, key := range legacyFields {
				if _, exists := got[key]; exists {
					t.Errorf("payload contains removed field %q: %s", key, encoded)
				}
			}
		})
	}
}

func TestExtractCgroupMemoryMetrics(t *testing.T) {
	current, hasCurrent, max, high := extractCgroupMemoryMetrics(cgroup.CgroupInfo{
		MemoryCurrent: cgroup.CgroupFileValue{Value: "1048576", Available: true},
		MemoryMax:     cgroup.CgroupFileValue{Value: "max", Available: true},
		MemoryHigh:    cgroup.CgroupFileValue{Value: "2097152", Available: true},
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

func TestUserMetricJSONUsesExplicitLimitSemantics(t *testing.T) {
	limitState := state.UserLimitState{
		EligibleForCPU:    true,
		EligibleForRAM:    false,
		EligibleForIO:     true,
		CPULimitRequested: true,
		CPULimitActive:    false,
		RAMLimitRequested: true,
		RAMLimitActive:    true,
		IOLimitRequested:  false,
		IOLimitActive:     false,
	}
	metric := newUserMetric(1000, &resmanmetrics.UserMetrics{
		Username:       "alice",
		EligibleForCPU: false,
		CPULimitActive: true,
	}, limitState)
	data, err := json.Marshal(metric)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	payload := string(data)
	for _, field := range []string{
		"eligible_for_cpu", "eligible_for_ram", "eligible_for_io",
		"cpu_limit_requested", "cpu_limit_active", "ram_limit_requested",
		"ram_limit_active", "io_limit_requested", "io_limit_active",
	} {
		if !strings.Contains(payload, `"`+field+`"`) {
			t.Errorf("JSON payload %s does not contain field %q", payload, field)
		}
	}
	if strings.Contains(payload, `"is_limited"`) {
		t.Errorf("JSON payload %s contains removed ambiguous field is_limited", payload)
	}
	if !metric.EligibleForCPU || metric.CPULimitActive {
		t.Fatalf("tool metric re-derived state from collector sample: %+v", metric)
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
	dbManager, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
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

func TestGetUserHistoryReturnsPersistedExplicitLimitState(t *testing.T) {
	dbManager, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error = %v", err)
	}
	defer func() { _ = dbManager.Close() }()

	now := time.Now().UTC()
	if err := dbManager.WriteUserMetrics(&database.UserMetricsRecord{
		Timestamp:         now,
		UID:               1000,
		Username:          "offline-user",
		ProcessCount:      1,
		EligibleForCPU:    true,
		EligibleForRAM:    false,
		EligibleForIO:     true,
		CPULimitRequested: true,
		CPULimitActive:    false,
		RAMLimitRequested: true,
		RAMLimitActive:    true,
	}); err != nil {
		t.Fatalf("WriteUserMetrics() error = %v", err)
	}

	server := &Server{dbManager: dbManager}
	uid := 1000
	_, result, err := server.handleGetUserHistory(
		context.Background(),
		nil,
		GetHistoryArgs{UID: &uid, Hours: 1},
	)
	if err != nil {
		t.Fatalf("handleGetUserHistory() error = %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("history records = %d, want 1", len(result.Records))
	}
	record := result.Records[0]
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal(history record) error = %v", err)
	}
	if strings.Contains(string(encoded), `"is_limited"`) {
		t.Fatalf("history record contains removed is_limited field: %s", encoded)
	}
	if !record.CPULimitRequested || record.CPULimitActive ||
		!record.RAMLimitRequested || !record.RAMLimitActive {
		t.Fatalf("history record lost requested/active distinctions: %+v", record)
	}
}

func privateMetricsDatabasePath(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", root, err)
	}
	dir := filepath.Join(root, "database")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("os.Mkdir(%s) error = %v", dir, err)
	}
	return filepath.Join(dir, "metrics.db")
}
