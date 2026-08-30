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
package metrics

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/shirou/gopsutil/v3/cpu"
)

func TestReportableProcFSFailureIgnoresProcessExitOnly(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error", err: nil, want: false},
		{name: "process exited", err: fmt.Errorf("read procfs: %w", fs.ErrNotExist), want: false},
		{name: "permission denied", err: fmt.Errorf("read procfs: %w", fs.ErrPermission), want: true},
		{name: "malformed sample", err: errors.New("required counter missing"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reportableProcFSFailure(tt.err); got != tt.want {
				t.Fatalf("reportableProcFSFailure(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func TestProcFSFailureSummaryAggregatesPerAccessInsteadOfPerPID(t *testing.T) {
	firstErr := errors.New("first failure")
	summary := procFSFailureSummary{access: procFSAccessIODecision}
	summary.record(101, firstErr)
	summary.record(202, errors.New("second failure"))
	if summary.count != 2 || summary.firstPID != 101 || !errors.Is(summary.firstErr, firstErr) {
		t.Fatalf("summary = %+v, want count 2 and first PID/error only", summary)
	}
}

func TestUserMetricsStruct(t *testing.T) {
	um := &UserMetrics{
		UID:          1000,
		Username:     "testuser",
		CPUUsage:     25.5,
		MemoryUsage:  104857600,
		ProcessCount: 10,
	}

	if um.UID != 1000 {
		t.Errorf("UID: got %d, expected 1000", um.UID)
	}
	if um.Username != "testuser" {
		t.Errorf("Username: got %s, expected testuser", um.Username)
	}
	if um.CPUUsage != 25.5 {
		t.Errorf("CPUUsage: got %f, expected 25.5", um.CPUUsage)
	}
	if um.MemoryUsage != 104857600 {
		t.Errorf("MemoryUsage: got %d, expected 104857600", um.MemoryUsage)
	}
	if um.ProcessCount != 10 {
		t.Errorf("ProcessCount: got %d, expected 10", um.ProcessCount)
	}
}

func TestAddProcessSampleSeparatesObservedAndEnforceableUsage(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ProcessExcludeList = []string{"^systemd$"}
	data := &userData{}

	excluded := processUsage{
		cpuUsage: 90, cpuUsageAvg: 80, processCount: 1, memoryUsage: 900,
		ioReadBytes: 9000, ioWriteBytes: 8000, ioReadOps: 90, ioWriteOps: 80,
	}
	included := processUsage{
		cpuUsage: 10, cpuUsageAvg: 8, processCount: 1, memoryUsage: 100,
		ioReadBytes: 1000, ioWriteBytes: 2000, ioReadOps: 10, ioWriteOps: 20,
	}
	if selection := addProcessSample(data, cfg, "/usr/lib/systemd/systemd", "systemd", excluded); selection.Enforceable {
		t.Fatalf("excluded process selection = %+v, want enforceable=false", selection)
	}
	if selection := addProcessSample(data, cfg, "/usr/bin/stress", "stress", included); !selection.Enforceable {
		t.Fatalf("included process selection = %+v, want enforceable=true", selection)
	}

	if data.observed.cpuUsage != 100 || data.observed.memoryUsage != 1000 ||
		data.observed.processCount != 2 || data.observed.ioReadBytes != 10000 ||
		data.observed.ioWriteBytes != 10000 || data.observed.ioReadOps != 100 ||
		data.observed.ioWriteOps != 100 {
		t.Fatalf("observed usage = %+v, want both process samples", data.observed)
	}
	if data.enforceable != included {
		t.Fatalf("enforceable usage = %+v, want only included sample %+v", data.enforceable, included)
	}
}

func TestAddProcessSampleKeepsUntrustedCommFallbackEnforceable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ProcessExcludeList = []string{"^systemd$"}
	data := &userData{}
	sample := processUsage{
		cpuUsage:                               50,
		memoryUsage:                            4096,
		processCount:                           1,
		executableIdentityUnavailableProcesses: 1,
		ioUnavailableProcesses:                 1,
	}

	selection := addProcessSample(data, cfg, "", "systemd", sample)
	if !selection.Enforceable || selection.IdentityTrusted {
		t.Fatalf("selection = %+v, want fail-closed enforceable untrusted identity", selection)
	}
	if data.observed != sample || data.enforceable != sample {
		t.Fatalf("usage observed=%+v enforceable=%+v, want sample in both", data.observed, data.enforceable)
	}
	metrics := processSetMetrics(data.enforceable, 0)
	if metrics.ExecutableIdentityUnavailableProcesses != 1 || metrics.IOUnavailableProcesses != 1 {
		t.Fatalf("coverage metrics = %+v, want one unavailable identity and I/O sample", metrics)
	}
}

func TestAddProcessSampleKeepsExcludedProcFSCoverageOutOfDecisionAggregate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ProcessExcludeList = []string{"^systemd$"}
	data := &userData{}
	sample := processUsage{
		processCount:           1,
		ioUnavailableProcesses: 1,
	}

	selection := addProcessSample(data, cfg, "/usr/lib/systemd/systemd", "systemd", sample)
	if selection.Enforceable || !selection.IdentityTrusted {
		t.Fatalf("selection = %+v, want trusted exclusion", selection)
	}
	if data.observed.ioUnavailableProcesses != 1 {
		t.Fatalf("observed unavailable I/O processes = %d, want 1", data.observed.ioUnavailableProcesses)
	}
	if data.enforceable.ioUnavailableProcesses != 0 {
		t.Fatalf("decision unavailable I/O processes = %d, want 0 for excluded process", data.enforceable.ioUnavailableProcesses)
	}
}

func TestUpdateConfigResetsOnlyEnforceableEMAWhenProcessPolicyChanges(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)
	for _, state := range []*userMetricsSamplingState{collector.observationState, collector.decisionState} {
		state.ema.values[1000] = 90
		state.ema.enforceableValues[1000] = 80
	}

	reloaded := config.DefaultConfig()
	reloaded.ProcessExcludeList = []string{"^stress$"}
	collector.UpdateConfig(reloaded)

	for name, state := range map[string]*userMetricsSamplingState{
		"observation": collector.observationState,
		"decision":    collector.decisionState,
	} {
		if got := state.ema.values[1000]; got != 90 {
			t.Fatalf("%s observed EMA after reload = %.1f, want 90", name, got)
		}
		if len(state.ema.enforceableValues) != 0 {
			t.Fatalf("%s enforceable EMA after process-policy reload = %v, want empty", name, state.ema.enforceableValues)
		}
	}

	data := &userData{}
	excluded := processUsage{cpuUsage: 90, memoryUsage: 900, processCount: 1}
	included := processUsage{cpuUsage: 10, memoryUsage: 100, processCount: 1}
	activeConfig := collector.getConfig()
	if selection := addProcessSample(data, activeConfig, "/usr/bin/stress", "stress", excluded); selection.Enforceable {
		t.Fatalf("reloaded exclusion selection = %+v, want enforceable=false", selection)
	}
	if selection := addProcessSample(data, activeConfig, "/usr/bin/worker", "worker", included); !selection.Enforceable {
		t.Fatalf("reloaded included selection = %+v, want enforceable=true", selection)
	}
	if data.observed.cpuUsage != 100 || data.enforceable != included {
		t.Fatalf("usage after policy reload: observed=%+v enforceable=%+v", data.observed, data.enforceable)
	}
}

func TestParseProcessIOSeparatesStorageBytesFromSyscalls(t *testing.T) {
	data := []byte(`rchar: 999999
wchar: 888888
syscr: 123
syscw: 45
read_bytes: 4096
write_bytes: 8192
cancelled_write_bytes: 1024
`)

	counters, err := parseProcessIO(data)
	if err != nil {
		t.Fatalf("parseProcessIO() error: %v", err)
	}
	if counters.readBytes != 4096 {
		t.Errorf("readBytes = %d, want 4096", counters.readBytes)
	}
	if counters.writeBytes != 8192 {
		t.Errorf("writeBytes = %d, want 8192", counters.writeBytes)
	}
	if counters.readOps != 123 {
		t.Errorf("readSyscalls = %d, want 123", counters.readOps)
	}
	if counters.writeOps != 45 {
		t.Errorf("writeSyscalls = %d, want 45", counters.writeOps)
	}
}

func TestParseProcessIORejectsIncompleteOrInvalidDecisionSamples(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "missing storage counter", data: "syscr: 1\nsyscw: 2\nwrite_bytes: 3\n"},
		{name: "invalid syscall counter", data: "syscr: nope\nsyscw: 2\nread_bytes: 3\nwrite_bytes: 4\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseProcessIO([]byte(tt.data)); err == nil {
				t.Fatal("parseProcessIO() error = nil, want incomplete coverage error")
			}
		})
	}
}

func TestWriteMetricsToDatabasePreservesRuntimeState(t *testing.T) {
	dbManager, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatalf("NewDatabaseManager() error: %v", err)
	}
	t.Cleanup(func() {
		if err := dbManager.Close(); err != nil {
			t.Errorf("DatabaseManager.Close() error: %v", err)
		}
	})

	collector := &Collector{
		dbWriter: NewDBWriter(dbManager, 0),
	}
	if err := collector.WriteMetricsToDatabase(
		map[int]*UserMetrics{
			1000: {
				UID:               1000,
				Username:          "limited-user",
				CPUUsage:          75,
				MemoryUsage:       1024,
				ProcessCount:      3,
				EligibleForCPU:    true,
				EligibleForRAM:    true,
				EligibleForIO:     true,
				CPULimitRequested: true,
				CPULimitActive:    true,
				RAMLimitRequested: true,
				RAMLimitActive:    true,
				IOLimitRequested:  true,
				IOLimitActive:     true,
			},
		},
		SystemPersistenceMetrics{
			TotalCPUUsagePercent:         50,
			TotalCores:                   4,
			SystemLoad:                   2.5,
			CPULimitsActive:              true,
			AnyLimitsActive:              true,
			CPUActivelyLimitedUsersCount: 1,
			ActivelyLimitedUsersCount:    1,
		},
	); err != nil {
		t.Fatalf("WriteMetricsToDatabase() error: %v", err)
	}

	start := time.Now().Add(-time.Minute)
	end := time.Now().Add(time.Minute)
	userHistory, err := dbManager.GetUserHistory(1000, start, end, 1)
	if err != nil {
		t.Fatalf("GetUserHistory() error: %v", err)
	}
	if len(userHistory) != 1 || !userHistory[0].CPULimitActive || !userHistory[0].EligibleForRAM {
		t.Fatalf("user history = %+v, want one explicit enforcement record", userHistory)
	}

	systemHistory, err := dbManager.GetSystemHistory(start, end, 1)
	if err != nil {
		t.Fatalf("GetSystemHistory() error: %v", err)
	}
	if len(systemHistory) != 1 || systemHistory[0].SystemLoad != 2.5 {
		t.Fatalf("system history = %+v, want system load 2.5", systemHistory)
	}
	if !userHistory[0].Timestamp.Equal(systemHistory[0].Timestamp) {
		t.Fatalf("batch timestamps differ: user=%s system=%s",
			userHistory[0].Timestamp, systemHistory[0].Timestamp)
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

func TestNewCollector(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)

	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)
	if collector == nil {
		t.Fatal("NewCollector() returned nil")
	}
	if collector.cfg != cfg {
		t.Error("collector.cfg not set correctly")
	}
	if collector.cache == nil {
		t.Error("collector.cache not initialized")
	}
	if collector.now == nil {
		t.Error("collector clock not initialized")
	}
}

func TestUsernameCacheTTLLifecycleIsIndependentOfMetricsDatabase(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MetricsDBEnabled = false
	cfg.UsernameCacheTTL = 17
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)
	if got, want := collector.GetUsernameCacheTTL(), 17*time.Minute; got != want {
		t.Fatalf("initial username cache TTL = %s, want %s", got, want)
	}

	reloaded := config.DefaultConfig()
	reloaded.MetricsDBEnabled = false
	reloaded.UsernameCacheTTL = 9
	collector.UpdateConfig(reloaded)
	if got, want := collector.GetUsernameCacheTTL(), 9*time.Minute; got != want {
		t.Fatalf("reloaded username cache TTL = %s, want %s", got, want)
	}
}

func TestUpdateProcessCPUSampleCountsConsecutiveDelta(t *testing.T) {
	state := newUserMetricsSamplingState()
	now := time.Now()

	if got := updateProcessCPUSample(state, 42, 1000, cpu.TimesStat{User: 1, System: 1}, now); got != 0 {
		t.Fatalf("first sample = %f, want 0", got)
	}
	got := updateProcessCPUSample(state, 42, 1000, cpu.TimesStat{User: 2.5, System: 1.5}, now.Add(time.Second))
	if got != 200 {
		t.Fatalf("second sample = %f, want 200", got)
	}
}

func TestUpdateProcessCPUSampleWaitsForReliableElapsedTime(t *testing.T) {
	state := newUserMetricsSamplingState()
	now := time.Now()

	updateProcessCPUSample(state, 42, 1000, cpu.TimesStat{User: 1}, now)
	if got := updateProcessCPUSample(state, 42, 1000, cpu.TimesStat{User: 1.5}, now.Add(500*time.Millisecond)); got != 0 {
		t.Fatalf("sub-second CPU delta = %f, want 0", got)
	}
	got := updateProcessCPUSample(state, 42, 1000, cpu.TimesStat{User: 2}, now.Add(time.Second))
	if got != 100 {
		t.Fatalf("CPU delta after one second = %f, want 100", got)
	}
}

func TestUpdateProcessCPUSampleResetsReusedPID(t *testing.T) {
	state := newUserMetricsSamplingState()
	now := time.Now()
	updateProcessCPUSample(state, 42, 1000, cpu.TimesStat{User: 1}, now)

	got := updateProcessCPUSample(state, 42, 2000, cpu.TimesStat{User: 20}, now.Add(time.Second))
	if got != 0 {
		t.Fatalf("first sample for reused PID = %f, want 0", got)
	}
	got = updateProcessCPUSample(state, 42, 2000, cpu.TimesStat{User: 21}, now.Add(2*time.Second))
	if got != 100 {
		t.Fatalf("second sample for reused PID = %f, want 100", got)
	}
}

func TestUpdateProcessCPUSampleRetainsBaselinesAboveLegacyLimit(t *testing.T) {
	state := newUserMetricsSamplingState()
	now := time.Now()
	const processCount = 6000
	for pid := int32(1); pid <= processCount; pid++ {
		updateProcessCPUSample(state, pid, int64(pid), cpu.TimesStat{User: 1}, now)
	}

	if got := len(state.process.prevProcCPU); got != processCount {
		t.Fatalf("process cache size = %d, want %d", got, processCount)
	}
	got := updateProcessCPUSample(state, 1, 1, cpu.TimesStat{User: 2}, now.Add(time.Second))
	if got != 100 {
		t.Fatalf("CPU delta after cache growth = %f, want 100", got)
	}
}

func TestObservationSamplesDoNotAdvanceDecisionTemporalState(t *testing.T) {
	tests := []struct {
		name             string
		observationCount int
	}{
		{name: "no observation refresh", observationCount: 0},
		{name: "one observation refresh", observationCount: 1},
		{name: "three observation refreshes", observationCount: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decisionState := newUserMetricsSamplingState()
			observationState := newUserMetricsSamplingState()
			startedAt := time.Unix(1_700_000_000, 0)
			initialIO := processIOCounters{readBytes: 100, writeBytes: 200, readOps: 10, writeOps: 20}

			updateProcessCPUSample(decisionState, 42, 1000, cpu.TimesStat{User: 1}, startedAt)
			updateProcessIOSample(decisionState, 42, 1000, initialIO, startedAt)
			calculateEMA(decisionState, 1000, 10)
			for i := 0; i < tt.observationCount; i++ {
				sampledAt := startedAt.Add(time.Duration(i+1) * time.Second)
				updateProcessCPUSample(
					observationState,
					42,
					1000,
					cpu.TimesStat{User: float64(i + 2)},
					sampledAt,
				)
				updateProcessIOSample(
					observationState,
					42,
					1000,
					processIOCounters{readBytes: uint64(1000 + i), writeBytes: uint64(2000 + i)},
					sampledAt,
				)
				calculateEMA(observationState, 1000, float64(90+i))
			}

			cpuUsage := updateProcessCPUSample(
				decisionState,
				42,
				1000,
				cpu.TimesStat{User: 5},
				startedAt.Add(4*time.Second),
			)
			ioDelta := updateProcessIOSample(
				decisionState,
				42,
				1000,
				processIOCounters{readBytes: 500, writeBytes: 800, readOps: 50, writeOps: 80},
				startedAt.Add(4*time.Second),
			)
			ema := calculateEMA(decisionState, 1000, 80)

			if cpuUsage != 100 {
				t.Fatalf("decision CPU usage after %d observation refreshes = %.1f, want 100", tt.observationCount, cpuUsage)
			}
			if ema != 31 {
				t.Fatalf("decision EMA after %d observation refreshes = %.1f, want 31", tt.observationCount, ema)
			}
			if ioDelta != (ProcessIODelta{ReadBytes: 400, WriteBytes: 600}) {
				t.Fatalf("decision I/O delta after %d observation refreshes = %+v", tt.observationCount, ioDelta)
			}
		})
	}
}

func TestObservationAndDecisionSamplesUseIndependentCacheEntries(t *testing.T) {
	collector := &Collector{
		cfg:   config.DefaultConfig(),
		cache: make(map[string]metricCacheEntry),
	}
	observationState := newUserMetricsSamplingState()
	decisionState := newUserMetricsSamplingState()
	var calls atomic.Int32
	collect := func(state *userMetricsSamplingState) map[int]*UserMetrics {
		calls.Add(1)
		uid := 1000
		if state == decisionState {
			uid = 1001
		}
		return map[int]*UserMetrics{uid: {UID: uid}}
	}

	observation := collector.getAllUserMetricsCached(
		observationUserMetricsCacheKey,
		observationState,
		collect,
	)
	decision := collector.getAllUserMetricsCached(
		decisionUserMetricsCacheKey,
		decisionState,
		collect,
	)
	collector.getAllUserMetricsCached(observationUserMetricsCacheKey, observationState, collect)

	if _, ok := observation[1000]; !ok {
		t.Fatalf("observation sample = %v, want UID 1000", observation)
	}
	if _, ok := decision[1001]; !ok {
		t.Fatalf("decision sample = %v, want UID 1001", decision)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("collection calls = %d, want one per sampling purpose", got)
	}
}

func TestCalculateProcessCPUAveragePreservesMulticoreUsage(t *testing.T) {
	if got := calculateProcessCPUAverage(4, 1); got != 400 {
		t.Fatalf("multicore average CPU = %f, want 400", got)
	}
	if got := calculateProcessCPUAverage(-1, 1); got != 0 {
		t.Fatalf("negative average CPU = %f, want 0", got)
	}
}

func TestUsernameCacheEvictsUIDZeroWhenOldest(t *testing.T) {
	collector, err := NewCollector(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)

	now := time.Now()
	for uid := 0; uid < MAX_USERNAME_CACHE_SIZE; uid++ {
		collector.usernameCache[uid] = "cached"
		collector.usernameCacheTime[uid] = now
	}
	collector.usernameCacheTime[0] = now.Add(-time.Hour)

	collector.cacheUsername(MAX_USERNAME_CACHE_SIZE, "new-user")

	if _, exists := collector.usernameCache[0]; exists {
		t.Fatal("oldest root cache entry was not evicted")
	}
	if got := len(collector.usernameCache); got != MAX_USERNAME_CACHE_SIZE {
		t.Fatalf("username cache size = %d, want %d", got, MAX_USERNAME_CACHE_SIZE)
	}
	if got := collector.usernameCache[MAX_USERNAME_CACHE_SIZE]; got != "new-user" {
		t.Fatalf("new cache entry = %q, want new-user", got)
	}
}

func TestUsernameCacheTTLConcurrentAccess(t *testing.T) {
	collector, err := NewCollector(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)
	collector.cacheUsername(1000, "testuser")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			collector.SetUsernameCacheTTL(time.Duration(i+1) * time.Second)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = collector.GetUsernameCacheTTL()
			_, _ = collector.getCachedUsername(1000)
		}
	}()
	wg.Wait()
}

func TestUnknownUsernameIsNegativelyCached(t *testing.T) {
	collector, err := NewCollector(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)

	const uid = 2147483646
	if got := collector.getUsername(uid); got != "2147483646" {
		t.Fatalf("unknown username = %q, want numeric UID", got)
	}
	if got, ok := collector.getCachedUsername(uid); !ok || got != "2147483646" {
		t.Fatalf("negative username cache = (%q, %v), want numeric UID and hit", got, ok)
	}
}

func TestRetainProcessBaselinesCompactsExitedProcesses(t *testing.T) {
	state := newUserMetricsSamplingState()
	now := time.Now()
	const processCount = 6000
	for pid := int32(1); pid <= processCount; pid++ {
		updateProcessCPUSample(state, pid, int64(pid), cpu.TimesStat{User: 1}, now)
		updateProcessIOSample(state, pid, int64(pid), processIOCounters{writeBytes: uint64(pid)}, now)
	}

	removed := retainProcessBaselines(state, map[int32]struct{}{
		1:            {},
		processCount: {},
	})
	if removed != (processBaselinePruneResult{cpu: processCount - 2, io: processCount - 2}) {
		t.Fatalf("removed process baselines = %+v, want %d CPU and I/O", removed, processCount-2)
	}
	if got := len(state.process.prevProcCPU); got != 2 {
		t.Fatalf("process cache size = %d, want 2", got)
	}
	if got := len(state.process.prevProcTime); got != 2 {
		t.Fatalf("process timestamp cache size = %d, want 2", got)
	}
	if got := len(state.process.procStartTime); got != 2 {
		t.Fatalf("process start-time cache size = %d, want 2", got)
	}
	if got := len(state.process.prevProcIO); got != 2 {
		t.Fatalf("process I/O cache size = %d, want 2", got)
	}

	if got := updateProcessCPUSample(state, 1, 1, cpu.TimesStat{User: 2}, now.Add(time.Second)); got != 100 {
		t.Fatalf("retained process CPU delta = %f, want 100", got)
	}
	if got := updateProcessCPUSample(state, 2, 2, cpu.TimesStat{User: 2}, now.Add(time.Second)); got != 0 {
		t.Fatalf("removed process CPU delta = %f, want 0", got)
	}
	if got := updateProcessIOSample(state, 1, 1, processIOCounters{writeBytes: 2}, now.Add(time.Second)); got.WriteBytes != 1 {
		t.Fatalf("retained process I/O delta = %+v, want write delta 1", got)
	}
	if got := updateProcessIOSample(state, 2, 2, processIOCounters{writeBytes: 2}, now.Add(time.Second)); got != (ProcessIODelta{}) {
		t.Fatalf("removed process I/O delta = %+v, want zero", got)
	}
}

func TestPerProcessIODeltasRemainNonzeroAcrossProcessChurn(t *testing.T) {
	state := newUserMetricsSamplingState()
	startedAt := time.Unix(1_700_000_000, 0)
	const (
		writerPID   int32 = 10
		writerStart int64 = 1000
	)

	updateProcessIOSample(state, writerPID, writerStart, processIOCounters{writeBytes: 100}, startedAt)
	previousChurnPID := int32(20)
	updateProcessIOSample(state, previousChurnPID, 2000, processIOCounters{writeBytes: 10_000}, startedAt)
	retainProcessBaselines(state, map[int32]struct{}{writerPID: {}, previousChurnPID: {}})

	for cycle := 1; cycle <= 4; cycle++ {
		sampledAt := startedAt.Add(time.Duration(cycle) * time.Second)
		writerDelta := updateProcessIOSample(
			state,
			writerPID,
			writerStart,
			processIOCounters{writeBytes: uint64(100 + cycle*100)},
			sampledAt,
		)
		churnPID := int32(20 + cycle)
		churnDelta := updateProcessIOSample(
			state,
			churnPID,
			int64(2000+cycle),
			processIOCounters{writeBytes: uint64(20_000 + cycle*1000)},
			sampledAt,
		)

		if total := writerDelta.WriteBytes + churnDelta.WriteBytes; total != 100 {
			t.Fatalf("cycle %d write delta = %d, want sustained writer delta 100", cycle, total)
		}
		removed := retainProcessBaselines(state, map[int32]struct{}{writerPID: {}, churnPID: {}})
		if removed.io != 1 {
			t.Fatalf("cycle %d removed I/O baselines = %d, want exited process 1", cycle, removed.io)
		}
	}
}

func TestUpdateProcessIOSampleGuardsCounterResetAndPIDReuse(t *testing.T) {
	state := newUserMetricsSamplingState()
	now := time.Unix(1_700_000_000, 0)
	updateProcessIOSample(
		state,
		42,
		1000,
		processIOCounters{readBytes: 100, writeBytes: 200, readOps: 30, writeOps: 40},
		now,
	)

	reset := updateProcessIOSample(
		state,
		42,
		1000,
		processIOCounters{readBytes: 150, writeBytes: 20, readOps: 50, writeOps: 80},
		now.Add(time.Second),
	)
	if reset != (ProcessIODelta{ReadBytes: 50}) {
		t.Fatalf("counter-reset delta = %+v, want only monotonic dimensions", reset)
	}

	reused := updateProcessIOSample(
		state,
		42,
		2000,
		processIOCounters{readBytes: 10_000, writeBytes: 10_000},
		now.Add(2*time.Second),
	)
	if reused != (ProcessIODelta{}) {
		t.Fatalf("reused PID delta = %+v, want fresh baseline", reused)
	}

	discardProcessIOBaseline(state, 42)
	afterMissingRead := updateProcessIOSample(
		state,
		42,
		2000,
		processIOCounters{readBytes: 20_000, writeBytes: 20_000},
		now.Add(3*time.Second),
	)
	if afterMissingRead != (ProcessIODelta{}) {
		t.Fatalf("delta after missing read = %+v, want fresh baseline", afterMissingRead)
	}
}

func TestUpdateFallbackCPUSampleUsesSamplingCadence(t *testing.T) {
	normalConfig := func(cacheTTL int) *config.Config {
		cfg := config.DefaultConfig()
		cfg.PollingInterval = 30
		cfg.MetricsCacheTTL = cacheTTL
		return cfg
	}
	psiConfig := func() *config.Config {
		cfg := config.DefaultConfig()
		cfg.PSIEventDriven = true
		cfg.PSIFallbackInterval = 300
		return cfg
	}
	configuredButInactivePSI := func() *config.Config {
		cfg := config.DefaultConfig()
		cfg.PSIEventDriven = true
		cfg.PollingInterval = 30
		cfg.PSIFallbackInterval = 10
		return cfg
	}

	tests := []struct {
		name              string
		cfg               *config.Config
		gap               time.Duration
		secondTotal       uint64
		secondIdle        uint64
		want              float64
		wantRecovery      bool
		effectiveInterval time.Duration
	}{
		{
			name:        "nil config uses default cadence",
			gap:         60 * time.Second,
			secondTotal: 1100,
			secondIdle:  850,
			want:        50,
		},
		{
			name:        "normal refresh jitter",
			cfg:         normalConfig(15),
			gap:         31 * time.Second,
			secondTotal: 1100,
			secondIdle:  850,
			want:        50,
		},
		{
			name:        "short cache TTL does not narrow sample window",
			cfg:         normalConfig(1),
			gap:         31 * time.Second,
			secondTotal: 1100,
			secondIdle:  850,
			want:        50,
		},
		{
			name:        "exact normal stale boundary remains valid",
			cfg:         normalConfig(15),
			gap:         60 * time.Second,
			secondTotal: 1100,
			secondIdle:  850,
			want:        50,
		},
		{
			name:         "normal long gap resets baseline",
			cfg:          normalConfig(3600),
			gap:          60*time.Second + time.Nanosecond,
			secondTotal:  1100,
			secondIdle:   850,
			wantRecovery: true,
		},
		{
			name:        "configured but inactive PSI uses polling cadence",
			cfg:         configuredButInactivePSI(),
			gap:         31 * time.Second,
			secondTotal: 1100,
			secondIdle:  850,
			want:        50,
		},
		{
			name:              "PSI heartbeat jitter",
			cfg:               psiConfig(),
			gap:               301 * time.Second,
			secondTotal:       1100,
			secondIdle:        850,
			want:              50,
			effectiveInterval: 300 * time.Second,
		},
		{
			name:              "exact PSI stale boundary remains valid",
			cfg:               psiConfig(),
			gap:               600 * time.Second,
			secondTotal:       1100,
			secondIdle:        850,
			want:              50,
			effectiveInterval: 300 * time.Second,
		},
		{
			name:              "PSI long gap resets baseline",
			cfg:               psiConfig(),
			gap:               600*time.Second + time.Nanosecond,
			secondTotal:       1100,
			secondIdle:        850,
			wantRecovery:      true,
			effectiveInterval: 300 * time.Second,
		},
		{
			name:         "counter regression resets baseline",
			cfg:          normalConfig(15),
			gap:          30 * time.Second,
			secondTotal:  100,
			secondIdle:   80,
			wantRecovery: true,
		},
		{
			name:         "clock regression resets baseline",
			cfg:          normalConfig(15),
			gap:          -time.Second,
			secondTotal:  1100,
			secondIdle:   850,
			wantRecovery: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collector := &Collector{cfg: tt.cfg}
			if tt.effectiveInterval > 0 {
				collector.SetFallbackCPUSamplingInterval(tt.effectiveInterval)
			}
			now := time.Now()
			if got := collector.updateFallbackCPUSampleAt(1000, 800, now); got != 0 {
				t.Fatalf("first fallback CPU sample = %f, want 0", got)
			}

			secondAt := now.Add(tt.gap)
			if got := collector.updateFallbackCPUSampleAt(tt.secondTotal, tt.secondIdle, secondAt); got != tt.want {
				t.Fatalf("second fallback CPU sample = %f, want %f", got, tt.want)
			}
			if tt.wantRecovery {
				got := collector.updateFallbackCPUSampleAt(tt.secondTotal+100, tt.secondIdle+50, secondAt.Add(time.Second))
				if got != 50 {
					t.Fatalf("fallback CPU sample after baseline reset = %f, want 50", got)
				}
			}
		})
	}
}

func TestFallbackCPUSampleMaxGapDerivesFromDecisionCadence(t *testing.T) {
	normalConfig := config.DefaultConfig()
	normalConfig.PollingInterval = 45
	normalConfig.MetricsCacheTTL = 3600

	psiConfig := config.DefaultConfig()
	psiConfig.PSIEventDriven = true
	psiConfig.PSIFallbackInterval = 120
	psiConfig.MetricsCacheTTL = 1

	tests := []struct {
		name              string
		cfg               *config.Config
		effectiveInterval time.Duration
		want              time.Duration
	}{
		{name: "nil config", want: 60 * time.Second},
		{name: "custom polling interval", cfg: normalConfig, want: 90 * time.Second},
		{name: "configured but inactive PSI uses polling interval", cfg: psiConfig, want: 60 * time.Second},
		{name: "active PSI uses effective fallback interval", cfg: psiConfig, effectiveInterval: 120 * time.Second, want: 240 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collector := &Collector{cfg: tt.cfg}
			if tt.effectiveInterval > 0 {
				collector.SetFallbackCPUSamplingInterval(tt.effectiveInterval)
			}
			if got := collector.fallbackCPUSampleMaxGap(); got != tt.want {
				t.Fatalf("Collector.fallbackCPUSampleMaxGap() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseProcessPSS(t *testing.T) {
	input := "Rss:                4096 kB\nPss:                1536 kB\nPss_Anon:           1024 kB\n"
	got, err := parseProcessPSS(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseProcessPSS() error: %v", err)
	}
	if want := uint64(1536 * 1024); got != want {
		t.Fatalf("parseProcessPSS() = %d, want %d", got, want)
	}
}

func TestRetainEMAUsersRemovesInactiveUIDs(t *testing.T) {
	state := newUserMetricsSamplingState()
	state.ema.values[1000] = 10
	state.ema.values[1001] = 20
	retainEMAUsers(state, map[int]*UserMetrics{1001: {UID: 1001}})

	if _, exists := state.ema.values[1000]; exists {
		t.Fatal("EMA for inactive UID 1000 was not removed")
	}
	if got := state.ema.values[1001]; got != 20 {
		t.Fatalf("EMA for active UID 1001 = %f, want 20", got)
	}
}

func TestGetTotalCores(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	cores := collector.GetTotalCores()
	if cores < 1 {
		t.Errorf("GetTotalCores() returned %d, expected >= 1", cores)
	}
}

func TestGetTotalCPUUsage(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	usage := collector.GetTotalCPUUsage()
	// CPU usage should be between 0 and 100+ (can exceed 100 on multi-core)
	if usage < 0 {
		t.Errorf("GetTotalCPUUsage() returned %f, expected >= 0", usage)
	}
}

func TestGetMemoryUsage(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	usage := collector.GetMemoryUsage()
	if usage < 0 {
		t.Errorf("GetMemoryUsage() returned %f, expected >= 0", usage)
	}
}

func TestGetAllUsers(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	users := collector.GetAllUsers()
	// Should return at least root user
	if len(users) < 1 {
		t.Errorf("GetAllUsers() returned %d users, expected >= 1", len(users))
	}

	// Verify all users are in valid range
	for _, uid := range users {
		if uid < cfg.SystemUIDMin || uid > cfg.SystemUIDMax {
			t.Errorf("GetAllUsers() returned invalid UID %d", uid)
		}
	}
}

func TestGetAllUserMetrics(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	userMetrics := collector.GetAllUserMetrics()
	if userMetrics == nil {
		t.Error("GetAllUserMetrics() returned nil")
	}

	// Verify structure of returned metrics
	for uid, metrics := range userMetrics {
		if metrics == nil {
			t.Errorf("GetAllUserMetrics() returned nil metrics for UID %d", uid)
			continue
		}
		if metrics.UID != uid {
			t.Errorf("Metrics UID mismatch: got %d, expected %d", metrics.UID, uid)
		}
		if metrics.CPUUsage < 0 {
			t.Errorf("CPUUsage for UID %d is negative: %f", uid, metrics.CPUUsage)
		}
		if metrics.ProcessCount < 0 {
			t.Errorf("ProcessCount for UID %d is negative: %d", uid, metrics.ProcessCount)
		}
	}
}

func TestGetAllUserMetricsCoalescesConcurrentScans(t *testing.T) {
	cfg := config.DefaultConfig()
	collector := &Collector{
		cfg:   cfg,
		cache: make(map[string]metricCacheEntry),
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	state := newUserMetricsSamplingState()
	collect := func(*userMetricsSamplingState) map[int]*UserMetrics {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return map[int]*UserMetrics{1000: {UID: 1000}}
	}

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	results := make(chan map[int]*UserMetrics, callers)
	for range callers {
		go func() {
			defer wg.Done()
			results <- collector.getAllUserMetricsCached(decisionUserMetricsCacheKey, state, collect)
		}()
	}

	<-started
	close(release)
	wg.Wait()
	close(results)

	if got := calls.Load(); got != 1 {
		t.Fatalf("full metric scans = %d, want 1", got)
	}
	for result := range results {
		if _, ok := result[1000]; !ok {
			t.Fatal("coalesced scan did not return the cached result")
		}
	}
}

func TestDerivedUserMetricsDoNotExtendCacheLifetime(t *testing.T) {
	cfg := config.DefaultConfig()
	collector := &Collector{
		cfg:   cfg,
		cache: make(map[string]metricCacheEntry),
	}

	collector.setInCache(observationUserMetricsCacheKey, map[int]*UserMetrics{
		1000: {UID: 1000, CPUUsage: 10},
	}, collector.metricsCacheTTL())
	if got := collector.GetUserCPUUsage(1000); got != 10 {
		t.Fatalf("initial user CPU = %f, want 10", got)
	}

	collector.setInCache(observationUserMetricsCacheKey, map[int]*UserMetrics{
		1000: {UID: 1000, CPUUsage: 25},
	}, collector.metricsCacheTTL())
	if got := collector.GetUserCPUUsage(1000); got != 25 {
		t.Fatalf("updated user CPU = %f, want 25", got)
	}
}

func TestGetUserMemoryUsage(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	// Test with a valid UID (root = 0, might not be in range)
	// Test with current user if available
	currentUID := os.Getuid()

	// GetUserMemoryUsage returns a uint64, so the result is non-negative by
	// construction; exercise the call path and ensure it does not panic.
	_ = collector.GetUserMemoryUsage(currentUID)
}

func TestGetUserProcessCount(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	currentUID := os.Getuid()
	count := collector.GetUserProcessCount(currentUID)

	// Process count should be non-negative
	if count < 0 {
		t.Errorf("GetUserProcessCount() returned %d, expected >= 0", count)
	}
}

func TestCacheFunctions(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)

	// Test set and get from cache
	collector.setInCache("test_key", "test_value", time.Second)

	val, valid := collector.getFromCache("test_key")
	if !valid {
		t.Error("getFromCache() returned invalid for existing key")
	}
	if val != "test_value" {
		t.Errorf("getFromCache() returned %v, expected test_value", val)
	}

	// Test non-existent key
	val, valid = collector.getFromCache("nonexistent_key")
	if valid {
		t.Error("getFromCache() should return invalid for non-existent key")
	}
	if val != nil {
		t.Errorf("getFromCache() returned %v for non-existent key, expected nil", val)
	}
}

func TestMetricCacheHonorsEachEntryTTLAtExactBoundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	collector := &Collector{
		cache: make(map[string]metricCacheEntry),
		now:   func() time.Time { return now },
	}

	collector.setInCache("short", "short-value", 10*time.Second)
	collector.setInCache("long", "long-value", time.Hour)

	now = now.Add(5 * time.Minute)
	collector.cleanupCacheAt(now)
	if value, valid := collector.getFromCache("short"); valid || value != nil {
		t.Fatalf("short cache entry after cleanup = (%v, %v), want expired", value, valid)
	}
	if value, valid := collector.getFromCache("long"); !valid || value != "long-value" {
		t.Fatalf("one-hour cache entry after five minutes = (%v, %v), want retained", value, valid)
	}

	now = time.Unix(1_700_000_000, 0).Add(time.Hour)
	collector.cleanupCacheAt(now)
	if value, valid := collector.getFromCache("long"); !valid || value != "long-value" {
		t.Fatalf("cache entry at exact TTL = (%v, %v), want retained", value, valid)
	}

	now = now.Add(time.Nanosecond)
	collector.cleanupCacheAt(now)
	if value, valid := collector.getFromCache("long"); valid || value != nil {
		t.Fatalf("cache entry beyond TTL = (%v, %v), want expired", value, valid)
	}
}

func TestCleanupPreservesActiveProcessBaselinesAcrossLongSamplingCadences(t *testing.T) {
	tests := []struct {
		name string
		gap  time.Duration
	}{
		{name: "default PSI cadence plus scheduler jitter", gap: 301 * time.Second},
		{name: "accepted interval above five minutes", gap: 10 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newUserMetricsSamplingState()
			startedAt := time.Unix(1_700_000_000, 0)
			const (
				pid       int32 = 42
				startTime int64 = 1000
			)
			updateProcessCPUSample(state, pid, startTime, cpu.TimesStat{User: 1}, startedAt)
			updateProcessIOSample(state, pid, startTime, processIOCounters{writeBytes: 100}, startedAt)

			collector := &Collector{
				cache:             make(map[string]metricCacheEntry),
				observationState:  newUserMetricsSamplingState(),
				decisionState:     state,
				usernameCache:     make(map[int]string),
				usernameCacheTime: make(map[int]time.Time),
			}
			collector.cleanupCacheAt(startedAt.Add(tt.gap))

			cpuUsage := updateProcessCPUSample(
				state,
				pid,
				startTime,
				cpu.TimesStat{User: 1 + tt.gap.Seconds()},
				startedAt.Add(tt.gap),
			)
			if cpuUsage != 100 {
				t.Fatalf("CPU usage after %s = %f, want sustained 100", tt.gap, cpuUsage)
			}
			ioDelta := updateProcessIOSample(
				state,
				pid,
				startTime,
				processIOCounters{writeBytes: 200},
				startedAt.Add(tt.gap),
			)
			if ioDelta.WriteBytes != 100 {
				t.Fatalf("I/O delta after %s = %+v, want write delta 100", tt.gap, ioDelta)
			}
		})
	}
}

func TestReloadedCacheTTLAndCadencePreserveTheirOwningState(t *testing.T) {
	initial := config.DefaultConfig()
	initial.MetricsCacheTTL = 15
	initial.PollingInterval = 30
	collector, err := NewCollector(initial)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)

	startedAt := time.Unix(1_700_000_000, 0)
	now := startedAt
	collector.now = func() time.Time { return now }
	const (
		pid       int32 = 42
		startTime int64 = 1000
	)
	updateProcessCPUSample(collector.decisionState, pid, startTime, cpu.TimesStat{User: 1}, startedAt)
	collector.setInCache("old-epoch", "old", collector.metricsCacheTTL())

	reloaded := config.DefaultConfig()
	reloaded.MetricsCacheTTL = 3600
	reloaded.PollingInterval = 900
	collector.UpdateConfig(reloaded)
	collector.SetFallbackCPUSamplingInterval(15 * time.Minute)
	if value, valid := collector.getFromCache("old-epoch"); valid || value != nil {
		t.Fatalf("pre-reload cache entry = (%v, %v), want invalidated", value, valid)
	}

	collector.setInCache("new-epoch", "new", collector.metricsCacheTTL())
	now = startedAt.Add(15*time.Minute + time.Second)
	collector.cleanupCacheAt(now)
	if value, valid := collector.getFromCache("new-epoch"); !valid || value != "new" {
		t.Fatalf("reloaded one-hour cache entry = (%v, %v), want retained", value, valid)
	}
	if got := updateProcessCPUSample(
		collector.decisionState,
		pid,
		startTime,
		cpu.TimesStat{User: 902},
		now,
	); got != 100 {
		t.Fatalf("decision CPU usage after cadence reload = %f, want sustained 100", got)
	}
}

func TestClearCache(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)

	collector.setInCache("key1", "value1", time.Second)
	collector.setInCache("key2", "value2", time.Second)

	collector.ClearCache()

	val, valid := collector.getFromCache("key1")
	if valid {
		t.Errorf("ClearCache() did not clear key1: got %v", val)
	}

	val, valid = collector.getFromCache("key2")
	if valid {
		t.Errorf("ClearCache() did not clear key2: got %v", val)
	}
}

func TestIsValidUserUID(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	tests := []struct {
		uid      int
		expected bool
	}{
		{500, false},                  // System user
		{999, false},                  // Still system user
		{1000, true},                  // Valid user
		{1001, true},                  // Valid user
		{-1, false},                   // Negative
		{cfg.SystemUIDMax, true},      // Max valid (dynamic)
		{cfg.SystemUIDMax + 1, false}, // Above max (dynamic)
	}

	for _, tt := range tests {
		got := collector.isMonitoredUserUID(tt.uid)
		if got != tt.expected {
			t.Errorf("isMonitoredUserUID(%d): got %v, expected %v (SystemUIDMax=%d)", tt.uid, got, tt.expected, cfg.SystemUIDMax)
		}
	}
}

func TestGetSystemLoad(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	load, err := collector.GetSystemLoad()
	if err != nil {
		t.Errorf("GetSystemLoad() error: %v", err)
	}
	if load < 0 {
		t.Errorf("GetSystemLoad() returned %f, expected >= 0", load)
	}
}

func TestIsSystemUnderLoad(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	// This test depends on actual system state
	underLoad := collector.IsSystemUnderLoad()
	// Just verify it returns without error
	_ = underLoad
}

func TestGetObservationMetricsReturnsTypedSnapshot(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}

	observation := collector.GetObservationMetrics()
	if observation.TotalCores <= 0 {
		t.Errorf("TotalCores = %d, want positive value", observation.TotalCores)
	}
	if observation.ObservedUsersCount < 0 {
		t.Errorf("ObservedUsersCount = %d, want non-negative value", observation.ObservedUsersCount)
	}
}

func TestCollectorConcurrency(t *testing.T) {
	cfg := config.DefaultConfig()
	collector, err := NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error: %v", err)
	}
	t.Cleanup(collector.Stop)

	// Test concurrent access to cache
	done := make(chan bool)

	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 100; j++ {
				collector.setInCache("key", id, time.Second)
				collector.getFromCache("key")
			}
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}
