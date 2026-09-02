package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/limithook"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type capturingHookLogger struct {
	message string
	fields  []interface{}
}

func (l *capturingHookLogger) Info(message string, fields ...interface{}) {
	l.message = message
	l.fields = append([]interface{}(nil), fields...)
}

func (l *capturingHookLogger) Warn(message string, fields ...interface{}) {
	l.message = message
	l.fields = append([]interface{}(nil), fields...)
}

func TestPostLimitHook(t *testing.T) {
	var received limitHookEvent
	var receivedFields map[string]json.RawMessage

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s, expected POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type: got %s, expected application/json", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&receivedFields); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		payload, err := json.Marshal(receivedFields)
		if err != nil {
			t.Errorf("remarshal request body: %v", err)
		} else if err := json.Unmarshal(payload, &received); err != nil {
			t.Errorf("decode typed request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	high, max, oom, kill := uint64(153), uint64(0), uint64(0), uint64(0)
	memoryHigh, memoryMax, memorySwap := "16777216", "50331648", "0"
	swapDisabled := true
	event := limitHookEvent{
		UID:                        1000,
		Username:                   "app",
		EnforceableCPUUsagePercent: 82.5,
		CPUEligibleUsersCount:      2,
		SharedCgroup:               "/sys/fs/cgroup/resman/limited",
		Timestamp:                  time.Now().UTC(),
		LimitHookSource:            "resman",
		CPUPointsConfiguredClass:   "best_effort",
		CPUPointsLifecycleState:    resmanmetrics.CPUPointsLifecycleFailed,
		CPUPointsProcessCoverage:   resmanmetrics.CPUPointsCoverageNone,
		RAMSwapDisabled:            &swapDisabled,
		MemoryHighLimit:            &memoryHigh,
		MemoryMaxLimit:             &memoryMax,
		MemorySwapMax:              &memorySwap,
		MemoryHighEventsDelta:      &high,
		MemoryMaxEventsDelta:       &max,
		MemoryOOMEventsDelta:       &oom,
		MemoryOOMKillEventsDelta:   &kill,
	}

	if err := postLimitHook(t.Context(), server.URL, event); err != nil {
		t.Fatalf("postLimitHook() error: %v", err)
	}
	if received.UID != event.UID || received.Username != event.Username {
		t.Fatalf("received event: got uid=%d username=%q", received.UID, received.Username)
	}
	for _, required := range []string{
		"enforceable_cpu_usage_percent", "cpu_eligible_users_count",
		"cpu_points_configured_class", "cpu_points_lifecycle_state",
		"cpu_points_applied_to_processes", "cpu_points_complete_uid_workload_guaranteed",
		"cpu_points_reconciliation_degraded", "cpu_points_process_coverage",
		"ram_coverage_incomplete_process_count",
		"ram_swap_disabled", "memory_high_limit", "memory_max_limit", "memory_swap_max",
		"memory_high_events_delta", "memory_max_events_delta", "memory_oom_events_delta", "memory_oom_kill_events_delta",
	} {
		if _, ok := receivedFields[required]; !ok {
			t.Errorf("hook payload missing explicit field %q", required)
		}
	}
	if received.MemoryHighEventsDelta == nil || *received.MemoryHighEventsDelta != 153 ||
		received.MemoryMaxEventsDelta == nil || *received.MemoryMaxEventsDelta != 0 ||
		received.MemoryOOMEventsDelta == nil || *received.MemoryOOMEventsDelta != 0 ||
		received.MemoryOOMKillEventsDelta == nil || *received.MemoryOOMKillEventsDelta != 0 {
		t.Fatalf("hook conflated memory.high with max/OOM/kill: %+v", received)
	}
	for _, removed := range []string{"cpu_usage", "limited_users"} {
		if _, ok := receivedFields[removed]; ok {
			t.Errorf("hook payload retained ambiguous field %q", removed)
		}
	}
}

func TestPostLimitHookUsesItsOwnClientAndHonorsTheRequestDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer server.Close()

	originalDefault := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("http.DefaultClient must not be used")
	})}
	t.Cleanup(func() { http.DefaultClient = originalDefault })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := postLimitHook(ctx, server.URL, limitHookEvent{UID: 1000, Username: "alice"})
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("postLimitHook() error = %v, want deadline exceeded", err)
	}
}

func TestPostLimitHookDoesNotRetryFailedRequests(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := postLimitHook(t.Context(), server.URL, limitHookEvent{UID: 1000, Username: "alice"})
	if err == nil {
		t.Fatal("postLimitHook() error = nil, want HTTP failure")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP delivery attempts = %d, want exactly one", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCleanupCancelsAndWaitsForLimitHooks(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LimitHookEnabled = true
	cfg.LimitHookTimeout = 30

	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	cfg.LimitHookScript = "/test/hook"

	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	manager.executeHookScript = func(ctx context.Context, _ hookScriptInvocation, _ limitHookEvent) error {
		close(started)
		<-ctx.Done()
		close(cancelObserved)
		<-release
		return ctx.Err()
	}

	manager.notifyUserLimited(cfg, 1000, "alice", &SystemMetrics{
		UserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {EnforceableUsage: resmanmetrics.ProcessSetMetrics{CPUUsage: 75}},
		},
	})
	<-started

	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- manager.Cleanup() }()
	<-cancelObserved
	select {
	case err := <-cleanupDone:
		t.Fatalf("Cleanup() returned before the canceled hook became quiescent: %v", err)
	default:
	}

	close(release)
	if err := <-cleanupDone; err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}

	records := exporter.recordedLimitHookExecutions()
	if len(records) != 1 {
		t.Fatalf("limit-hook terminal records = %d, want 1", len(records))
	}
	if got, want := records[0], (limitHookMetricRecord{
		hookType: resmanmetrics.LimitHookTypeScript,
		outcome:  resmanmetrics.LimitHookOutcomeCancelled,
	}); got != want {
		t.Fatalf("limit-hook terminal record = %+v, want %+v", got, want)
	}
}

func TestRunLimitHookRecordsEachTerminalOutcome(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LimitHookEnabled = true
	cfg.LimitHookURL = "https://hooks.example.test/resman"

	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	cfg.LimitHookScript = "/test/hook"
	t.Cleanup(manager.stopLimitHooks)

	manager.executeHookScript = func(context.Context, hookScriptInvocation, limitHookEvent) error { return nil }
	manager.executeHookRequest = func(context.Context, string, limitHookEvent) error {
		return errors.New("delivery failed")
	}
	event := limitHookEvent{UID: 1000, Username: "alice"}
	manager.runLimitHookJob(limitHookJob{hookType: resmanmetrics.LimitHookTypeScript, timeout: time.Second, event: event})
	manager.runLimitHookJob(limitHookJob{hookType: resmanmetrics.LimitHookTypeHTTP, endpoint: cfg.LimitHookURL, timeout: time.Second, event: event})

	records := exporter.recordedLimitHookExecutions()
	want := []limitHookMetricRecord{
		{hookType: resmanmetrics.LimitHookTypeScript, outcome: resmanmetrics.LimitHookOutcomeSuccess},
		{hookType: resmanmetrics.LimitHookTypeHTTP, outcome: resmanmetrics.LimitHookOutcomeFailure},
	}
	if len(records) != len(want) {
		t.Fatalf("limit-hook terminal records = %+v, want %+v", records, want)
	}
	for i := range want {
		if records[i] != want[i] {
			t.Fatalf("limit-hook terminal record %d = %+v, want %+v", i, records[i], want[i])
		}
	}
}

func TestLimitHookOutcomeUsesBoundedTerminalValues(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want resmanmetrics.LimitHookOutcome
	}{
		{name: "success", want: resmanmetrics.LimitHookOutcomeSuccess},
		{name: "failure", err: errors.New("delivery failed"), want: resmanmetrics.LimitHookOutcomeFailure},
		{name: "timeout", err: fmt.Errorf("hook: %w", context.DeadlineExceeded), want: resmanmetrics.LimitHookOutcomeTimeout},
		{name: "cancelled", err: fmt.Errorf("hook: %w", context.Canceled), want: resmanmetrics.LimitHookOutcomeCancelled},
		{name: "saturated", err: fmt.Errorf("hook: %w", errLimitHookSaturated), want: resmanmetrics.LimitHookOutcomeSaturated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := limitHookOutcome(tt.err); got != tt.want {
				t.Fatalf("limitHookOutcome(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestPostLimitHookFailureRedactsEndpointSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	server.Close()

	endpoint := strings.Replace(server.URL, "http://", "http://hook-user:password-canary@", 1) +
		"/path-canary?token=query-canary#fragment-canary"
	err := postLimitHook(t.Context(), endpoint, limitHookEvent{UID: 1000, Username: "app"})
	if err == nil {
		t.Fatal("postLimitHook() error = nil, want transport failure")
	}
	if errors.Unwrap(err) != nil {
		t.Fatalf("postLimitHook() retained an unbounded transport cause: %v", err)
	}

	secrets := []string{
		"hook-user", "password-canary", "path-canary", "query-canary", "fragment-canary",
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		for _, secret := range secrets {
			if strings.Contains(cause.Error(), secret) {
				t.Fatalf("postLimitHook() error chain exposed %q: %v", secret, cause)
			}
		}
	}
	if got, want := hookEndpointForLog(endpoint), server.URL; got != want {
		t.Fatalf("hookEndpointForLog() = %q, want %q", got, want)
	}

	logger := &capturingHookLogger{}
	reportLimitHookURLFailure(
		logger,
		limitHookEvent{UID: 1000, Username: "app"},
		endpoint,
		resmanmetrics.LimitHookOutcomeFailure,
		err,
	)
	logged := fmt.Sprint(logger.message, logger.fields)
	for _, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Fatalf("limit-hook warning exposed %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, server.URL) {
		t.Fatalf("limit-hook warning omitted safe endpoint %q: %s", server.URL, logged)
	}
}

func TestPostLimitHookInvalidEndpointDoesNotRetainSecrets(t *testing.T) {
	endpoint := "http://hook-user:password-canary@[invalid/path-canary?token=query-canary#fragment-canary"
	err := postLimitHook(t.Context(), endpoint, limitHookEvent{UID: 1000, Username: "app"})
	if err == nil {
		t.Fatal("postLimitHook() error = nil, want invalid endpoint failure")
	}

	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		for _, secret := range []string{
			"hook-user", "password-canary", "path-canary", "query-canary", "fragment-canary",
		} {
			if strings.Contains(cause.Error(), secret) {
				t.Fatalf("postLimitHook() error chain exposed %q: %v", secret, cause)
			}
		}
	}
	if !strings.Contains(err.Error(), "<invalid-hook-endpoint>") {
		t.Fatalf("postLimitHook() error = %q, want bounded invalid-endpoint context", err)
	}
}

func TestRunLimitHookScript(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		t.Fatalf("secure temporary hook directory: %v", err)
	}
	outputPath := filepath.Join(tmpDir, "hook.out")
	scriptPath := filepath.Join(tmpDir, "hook.sh")

	script := "#!/bin/sh\nprintf '%s:%s:%s:%s:%s:%s:%s:%s:%s:%s' \"$RESMAN_LIMIT_UID\" \"$RESMAN_LIMIT_USERNAME\" \"$RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT\" \"$RESMAN_LIMIT_CPU_ELIGIBLE_USERS_COUNT\" \"$RESMAN_LIMIT_CPU_POINTS_CONFIGURED_CLASS\" \"$RESMAN_LIMIT_CPU_POINTS_PROCESS_COVERAGE\" \"$RESMAN_LIMIT_MEMORY_HIGH_EVENTS_DELTA\" \"$RESMAN_LIMIT_MEMORY_MAX_EVENTS_DELTA\" \"$RESMAN_LIMIT_MEMORY_OOM_EVENTS_DELTA\" \"$RESMAN_LIMIT_MEMORY_OOM_KILL_EVENTS_DELTA\" > \"" + outputPath + "\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	high, zero := uint64(153), uint64(0)
	event := limitHookEvent{
		UID:                        1000,
		Username:                   "app",
		EnforceableCPUUsagePercent: 82.5,
		CPUEligibleUsersCount:      2,
		Timestamp:                  time.Now().UTC(),
		LimitHookSource:            "resman",
		CPUPointsConfiguredClass:   "guaranteed",
		CPUPointsProcessCoverage:   resmanmetrics.CPUPointsCoveragePartial,
		MemoryHighEventsDelta:      &high,
		MemoryMaxEventsDelta:       &zero,
		MemoryOOMEventsDelta:       &zero,
		MemoryOOMKillEventsDelta:   &zero,
	}

	if err := runLimitHookScript(t.Context(), testHookScriptInvocation(scriptPath), event); err != nil {
		t.Fatalf("runLimitHookScript() error: %v", err)
	}

	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read hook output: %v", err)
	}
	if string(output) != "1000:app:82.50:2:guaranteed:partial:153:0:0:0" {
		t.Fatalf("script output: got %q, expected distinct high/max/OOM/kill fields", string(output))
	}
}

func TestLimitHookSnapshotReportsAppliedHostSubsetWithoutCallingTheCompleteUIDGuaranteed(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	weight, err := cpupoints.NewKernelCPUWeight(300)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		cpuPointsPolicy: policy, cpuAllocations: map[int]cpuPointsAllocation{1000: {class: cpupoints.AllocationClassGuaranteed, weight: weight}},
		cpuPointsLifecycleEvents: map[int]cpuPointsLifecycleEvent{1000: {state: resmanmetrics.CPUPointsLifecycleApplied, pidNamespaceMismatches: 1}},
		resourceLimits:           map[int]userResourceLimitState{}, ramCoverage: map[int]ramCoverageState{},
	}
	sample := &SystemMetrics{UserMetrics: map[int]*resmanmetrics.UserMetrics{1000: {
		UID: 1000, Username: "alice", ProcessCount: 3, CPULimitRequested: true,
		EnforceableUsage: resmanmetrics.ProcessSetMetrics{ProcessCount: 2},
	}}}
	snapshot := manager.limitHookCPUPointsSnapshot(1000, "alice", sample)
	if !snapshot.AppliedToProcesses || snapshot.CompleteUIDWorkloadGuaranteed || snapshot.ProcessCoverage != resmanmetrics.CPUPointsCoveragePartial {
		t.Fatalf("limit-hook CPU Points snapshot = %+v", snapshot)
	}
}

func TestRunLimitHookScriptFailureDoesNotReturnProcessOutput(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		t.Fatalf("secure temporary hook directory: %v", err)
	}
	scriptPath := filepath.Join(tmpDir, "failing-hook.sh")
	script := "#!/bin/sh\nprintf '%s' 'stdout-canary'\nprintf '%s' 'stderr-canary' >&2\nexit 7\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	err := runLimitHookScript(t.Context(), testHookScriptInvocation(scriptPath), limitHookEvent{UID: 1000, Username: "app"})
	if err == nil {
		t.Fatal("runLimitHookScript() error = nil, want exit failure")
	}
	for _, secret := range []string{"stdout-canary", "stderr-canary"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("runLimitHookScript() error exposed %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "exited with status 7") {
		t.Fatalf("runLimitHookScript() error = %q, want actionable exit status", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runLimitHookScript() did not preserve exec.ExitError: %v", err)
	}

	logger := &capturingHookLogger{}
	reportLimitHookScriptFailure(
		logger,
		limitHookEvent{UID: 1000, Username: "app"},
		resmanmetrics.LimitHookOutcomeFailure,
		err,
	)
	logged := fmt.Sprint(logger.message, logger.fields)
	for _, secret := range []string{"stdout-canary", "stderr-canary", scriptPath} {
		if strings.Contains(logged, secret) {
			t.Fatalf("limit-hook warning exposed %q: %s", secret, logged)
		}
	}
}

func testHookScriptInvocation(path string) hookScriptInvocation {
	return hookScriptInvocation{
		path: path,
		identity: limithook.ScriptIdentity{
			Username: strconv.Itoa(os.Getuid()),
			Group:    strconv.Itoa(os.Getgid()),
			UID:      uint32(os.Getuid()),
			GID:      uint32(os.Getgid()),
		},
	}
}
