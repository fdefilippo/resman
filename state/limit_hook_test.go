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
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
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

	event := limitHookEvent{
		UID:                        1000,
		Username:                   "app",
		EnforceableCPUUsagePercent: 82.5,
		CPUEligibleUsersCount:      2,
		SharedCgroup:               "/sys/fs/cgroup/resman/limited",
		Timestamp:                  time.Now().UTC(),
		LimitHookSource:            "resman",
	}

	if err := postLimitHook(t.Context(), server.URL, event); err != nil {
		t.Fatalf("postLimitHook() error: %v", err)
	}
	if received.UID != event.UID || received.Username != event.Username {
		t.Fatalf("received event: got uid=%d username=%q", received.UID, received.Username)
	}
	for _, required := range []string{"enforceable_cpu_usage_percent", "cpu_eligible_users_count"} {
		if _, ok := receivedFields[required]; !ok {
			t.Errorf("hook payload missing explicit field %q", required)
		}
	}
	for _, removed := range []string{"cpu_usage", "limited_users"} {
		if _, ok := receivedFields[removed]; ok {
			t.Errorf("hook payload retained ambiguous field %q", removed)
		}
	}
}

func TestCleanupCancelsAndWaitsForLimitHooks(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LimitHookEnabled = true
	cfg.LimitHookScript = "/test/hook"
	cfg.LimitHookTimeout = 30

	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	manager.executeHookScript = func(ctx context.Context, _ string, _ limitHookEvent) error {
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
	cfg.LimitHookScript = "/test/hook"
	cfg.LimitHookURL = "https://hooks.example.test/resman"

	exporter := &mockPrometheusExporter{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	t.Cleanup(manager.stopLimitHooks)

	manager.executeHookScript = func(context.Context, string, limitHookEvent) error { return nil }
	manager.executeHookRequest = func(context.Context, string, limitHookEvent) error {
		return errors.New("delivery failed")
	}
	manager.runLimitHook(context.Background(), cfg, limitHookEvent{UID: 1000, Username: "alice"})

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
	outputPath := filepath.Join(tmpDir, "hook.out")
	scriptPath := filepath.Join(tmpDir, "hook.sh")

	script := "#!/bin/sh\nprintf '%s:%s:%s:%s' \"$RESMAN_LIMIT_UID\" \"$RESMAN_LIMIT_USERNAME\" \"$RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT\" \"$RESMAN_LIMIT_CPU_ELIGIBLE_USERS_COUNT\" > \"" + outputPath + "\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	event := limitHookEvent{
		UID:                        1000,
		Username:                   "app",
		EnforceableCPUUsagePercent: 82.5,
		CPUEligibleUsersCount:      2,
		Timestamp:                  time.Now().UTC(),
		LimitHookSource:            "resman",
	}

	if err := runLimitHookScript(t.Context(), scriptPath, event); err != nil {
		t.Fatalf("runLimitHookScript() error: %v", err)
	}

	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read hook output: %v", err)
	}
	if string(output) != "1000:app:82.50:2" {
		t.Fatalf("script output: got %q, expected %q", string(output), "1000:app:82.50:2")
	}
}

func TestRunLimitHookScriptFailureDoesNotReturnProcessOutput(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "failing-hook.sh")
	script := "#!/bin/sh\nprintf '%s' 'stdout-canary'\nprintf '%s' 'stderr-canary' >&2\nexit 7\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	err := runLimitHookScript(t.Context(), scriptPath, limitHookEvent{UID: 1000, Username: "app"})
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
