package state

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/limithook"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type limitHookTestLogger struct{ warnings atomic.Int64 }

func (*limitHookTestLogger) Debug(string, ...interface{}) {}
func (*limitHookTestLogger) Info(string, ...interface{})  {}
func (*limitHookTestLogger) Error(string, ...interface{}) {}
func (l *limitHookTestLogger) Warn(message string, _ ...interface{}) {
	if message == "Limit hook queue saturated" {
		l.warnings.Add(1)
	}
}
func (*limitHookTestLogger) DebugChecked(string, ...interface{}) error { return nil }
func (*limitHookTestLogger) InfoChecked(string, ...interface{}) error  { return nil }

func TestLimitHookExecutorBoundsConcurrencyQueueAndGoroutinesWithoutBlockingDispatch(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LimitHookMaxConcurrency = 2
	cfg.LimitHookQueueCapacity = 3
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	release := make(chan struct{})
	started := make(chan struct{}, cfg.LimitHookMaxConcurrency)
	var active atomic.Int64
	var maximum atomic.Int64
	var startSignals atomic.Int64
	manager.executeHookScript = func(context.Context, hookScriptInvocation, limitHookEvent) error {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		if startSignals.Add(1) <= int64(cfg.LimitHookMaxConcurrency) {
			started <- struct{}{}
		}
		<-release
		active.Add(-1)
		return nil
	}

	baselineGoroutines := runtime.NumGoroutine()
	startedAt := time.Now()
	for uid := 1000; uid < 1100; uid++ {
		manager.enqueueLimitHookJob(limitHookJob{
			hookType: resmanmetrics.LimitHookTypeScript,
			timeout:  time.Minute,
			event:    limitHookEvent{UID: uid, Username: "burst"},
		})
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("100 non-blocking hook admissions took %s", elapsed)
	}
	for range cfg.LimitHookMaxConcurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("bounded workers did not start")
		}
	}
	if got := maximum.Load(); got > int64(cfg.LimitHookMaxConcurrency) {
		t.Fatalf("maximum active deliveries = %d, limit = %d", got, cfg.LimitHookMaxConcurrency)
	}
	if got := len(manager.hookQueue); got > cfg.LimitHookQueueCapacity {
		t.Fatalf("queued deliveries = %d, capacity = %d", got, cfg.LimitHookQueueCapacity)
	}
	if delta := runtime.NumGoroutine() - baselineGoroutines; delta > cfg.LimitHookMaxConcurrency+2 {
		t.Fatalf("goroutine growth = %d for 100 events, want only the fixed worker pool", delta)
	}

	close(release)
	manager.stopLimitHooks()
	records := exporter.recordedLimitHookExecutions()
	if len(records) != 100 {
		t.Fatalf("terminal hook records = %d, want exactly one for each of 100 deliveries", len(records))
	}
	var saturated int
	for _, record := range records {
		if record.outcome == resmanmetrics.LimitHookOutcomeSaturated {
			saturated++
		}
	}
	if saturated == 0 {
		t.Fatal("burst produced no saturation outcomes")
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if exporter.limitHookInFlight != 0 || exporter.limitHookQueued != 0 || exporter.limitHookCapacity != cfg.LimitHookQueueCapacity {
		t.Fatalf("final executor metrics = in_flight=%d queued=%d capacity=%d", exporter.limitHookInFlight, exporter.limitHookQueued, exporter.limitHookCapacity)
	}
}

func TestLimitHookScriptAndHTTPReceiveIndependentFullDeadlines(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LimitHookMaxConcurrency = 1
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	requestStarted := make(chan time.Duration, 1)
	manager.executeHookScript = func(ctx context.Context, _ hookScriptInvocation, _ limitHookEvent) error {
		<-ctx.Done()
		return ctx.Err()
	}
	manager.executeHookRequest = func(ctx context.Context, _ string, _ limitHookEvent) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			requestStarted <- 0
			return nil
		}
		requestStarted <- time.Until(deadline)
		return nil
	}

	for _, hookType := range []resmanmetrics.LimitHookType{resmanmetrics.LimitHookTypeScript, resmanmetrics.LimitHookTypeHTTP} {
		manager.enqueueLimitHookJob(limitHookJob{
			hookType: hookType,
			endpoint: "https://hooks.example.test",
			timeout:  80 * time.Millisecond,
			event:    limitHookEvent{UID: 1000, Username: "alice"},
		})
	}
	select {
	case remaining := <-requestStarted:
		if remaining < 60*time.Millisecond {
			t.Fatalf("HTTP delivery inherited only %s after the script timeout", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP delivery did not run after the script timed out")
	}
	manager.stopLimitHooks()
}

func TestStopLimitHooksCancelsRunningAndQueuedJobsAndRecordsEachOnce(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LimitHookMaxConcurrency = 1
	cfg.LimitHookQueueCapacity = 2
	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	started := make(chan struct{})
	var startedOnce atomic.Bool
	manager.executeHookScript = func(ctx context.Context, _ hookScriptInvocation, _ limitHookEvent) error {
		if startedOnce.CompareAndSwap(false, true) {
			close(started)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	job := limitHookJob{hookType: resmanmetrics.LimitHookTypeScript, timeout: time.Minute}
	manager.enqueueLimitHookJob(job)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first hook job did not start")
	}
	manager.enqueueLimitHookJob(job)
	manager.enqueueLimitHookJob(job)

	manager.stopLimitHooks()
	records := exporter.recordedLimitHookExecutions()
	if len(records) != 3 {
		t.Fatalf("terminal hook records = %d, want one for each accepted job", len(records))
	}
	for index, record := range records {
		if record.outcome != resmanmetrics.LimitHookOutcomeCancelled {
			t.Fatalf("terminal hook record %d = %+v, want cancellation", index, record)
		}
	}
}

func TestLimitHookSaturationDiagnosticsAreRateLimited(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LimitHookMaxConcurrency = 1
	cfg.LimitHookQueueCapacity = 1
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	logger := &limitHookTestLogger{}
	manager.logger = logger
	release := make(chan struct{})
	started := make(chan struct{})
	manager.executeHookScript = func(context.Context, hookScriptInvocation, limitHookEvent) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return nil
	}
	job := limitHookJob{hookType: resmanmetrics.LimitHookTypeScript, timeout: time.Minute}
	manager.enqueueLimitHookJob(job)
	<-started
	manager.enqueueLimitHookJob(job)
	for range 20 {
		manager.enqueueLimitHookJob(job)
	}
	if got := logger.warnings.Load(); got != 1 {
		t.Fatalf("saturation warnings = %d, want one rate-limited diagnostic", got)
	}
	close(release)
	manager.stopLimitHooks()
}

func TestRunLimitHookScriptUsesAllowlistedEnvironmentAndKillsTheProcessGroup(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	environmentPath := tmpDir + "/environment"
	childPath := tmpDir + "/child.pid"
	scriptPath := tmpDir + "/hook.sh"
	script := "#!/bin/sh\n/usr/bin/env > \"" + environmentPath + "\"\nsleep 30 &\necho $! > \"" + childPath + "\"\nwait\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_AUTH_TOKEN", "secret-canary")
	t.Setenv("RESMAN_PARENT_SECRET", "parent-canary")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := runLimitHookScript(ctx, testHookScriptInvocation(scriptPath), limitHookEvent{UID: 1000, Username: "alice", Timestamp: time.Now().UTC()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runLimitHookScript() error = %v, want deadline exceeded", err)
	}
	environment, readErr := os.ReadFile(environmentPath)
	if readErr != nil {
		t.Fatalf("read script environment: %v", readErr)
	}
	for _, forbidden := range []string{"MCP_AUTH_TOKEN", "secret-canary", "RESMAN_PARENT_SECRET", "parent-canary"} {
		if strings.Contains(string(environment), forbidden) {
			t.Fatalf("script environment retained parent secret %q", forbidden)
		}
	}
	for _, required := range []string{"PATH=", "PWD=/", "RESMAN_LIMIT_UID=1000", "RESMAN_LIMIT_USERNAME=alice"} {
		if !strings.Contains(string(environment), required) {
			t.Fatalf("script environment omitted %q: %s", required, environment)
		}
	}
	childData, readErr := os.ReadFile(childPath)
	if readErr != nil {
		t.Fatalf("read child PID: %v", readErr)
	}
	childPID, parseErr := strconv.Atoi(strings.TrimSpace(string(childData)))
	if parseErr != nil {
		t.Fatalf("parse child PID: %v", parseErr)
	}
	if killErr := syscall.Kill(childPID, 0); !errors.Is(killErr, syscall.ESRCH) {
		t.Fatalf("hook child PID %d survived timeout: %v", childPID, killErr)
	}
}

func TestDispatchCarriesTheResolvedRestartRequiredScriptIdentity(t *testing.T) {
	cfg := config.DefaultConfig()
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	identity := limithook.ScriptIdentity{Username: "hook-user", Group: "hook-group", UID: 1234, GID: 5678}
	manager.hookScriptIdentity = identity
	invocation := make(chan hookScriptInvocation, 1)
	manager.executeHookScript = func(_ context.Context, got hookScriptInvocation, _ limitHookEvent) error {
		invocation <- got
		return nil
	}
	manager.enqueueLimitHookJob(limitHookJob{
		hookType: resmanmetrics.LimitHookTypeScript,
		script: hookScriptInvocation{
			path:           "/trusted/hook",
			identity:       manager.hookScriptIdentity,
			dropPrivileges: true,
		},
		timeout: time.Second,
	})
	select {
	case got := <-invocation:
		if !got.dropPrivileges || got.identity != identity {
			t.Fatalf("script invocation = %+v, want resolved identity %+v with privilege drop", got, identity)
		}
	case <-time.After(time.Second):
		t.Fatal("script delivery did not start")
	}
	manager.stopLimitHooks()
}
