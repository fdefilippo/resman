package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

func TestHookRetryUsesStableIdentityAndOneTerminalResult(t *testing.T) {
	for _, tt := range []struct {
		name                        string
		status, attempts, wantCalls int
		success                     bool
	}{
		{"transient success", 503, 3, 3, true}, {"ordinary client failure", 400, 3, 1, false}, {"default remains one shot", 503, 1, 1, false}, {"exhausted", 503, 2, 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1))
				var event limitHookEvent
				if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
					t.Errorf("event decode: %v", err)
				}
				if event.EventID != "stable-event" || r.Header.Get("Idempotency-Key") != event.EventID {
					t.Errorf("identity changed: event=%q key=%q", event.EventID, r.Header.Get("Idempotency-Key"))
				}
				if tt.success && n == tt.wantCalls {
					w.WriteHeader(http.StatusNoContent)
				} else {
					w.WriteHeader(tt.status)
				}
			}))
			defer server.Close()
			exporter := &mockPrometheusExporter{}
			cfg := config.DefaultConfig()
			m, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
			if err != nil {
				t.Fatal(err)
			}
			m.runLimitHookJob(limitHookJob{hookType: resmanmetrics.LimitHookTypeHTTP, endpoint: server.URL, httpMaxAttempts: tt.attempts, timeout: 2 * time.Second, event: limitHookEvent{EventID: "stable-event"}})
			if int(calls.Load()) != tt.wantCalls {
				t.Fatalf("attempts=%d want%d", calls.Load(), tt.wantCalls)
			}
			results := exporter.recordedLimitHookExecutions()
			if len(results) != 1 {
				t.Fatalf("terminal outcomes=%+v", results)
			}
			wantFailures := tt.wantCalls
			if tt.success {
				wantFailures--
			}
			if len(exporter.errors) != wantFailures {
				t.Fatalf("first/terminal failures not counted: %+v", exporter.errors)
			}
			if (results[0].outcome == resmanmetrics.LimitHookOutcomeSuccess) != tt.success {
				t.Fatalf("terminal=%+v", results[0])
			}
		})
	}
}
func TestHookRetryCancellationStopsBackoffAndDoesNotReplayScript(t *testing.T) {
	for _, script := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP cancellation", true: "script single attempt"}[script], func(t *testing.T) {
			exporter := &mockPrometheusExporter{}
			cfg := config.DefaultConfig()
			m, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
			if err != nil {
				t.Fatal(err)
			}
			var calls int
			invoke := func(ctx context.Context) error {
				calls++
				if !script {
					m.hookCancel()
				}
				return &hookHTTPStatusError{status: 503}
			}
			m.executeHookRequest = func(ctx context.Context, _ string, _ limitHookEvent) error { return invoke(ctx) }
			m.executeHookScript = func(ctx context.Context, _ hookScriptInvocation, _ limitHookEvent) error { return invoke(ctx) }
			typ := resmanmetrics.LimitHookTypeHTTP
			if script {
				typ = resmanmetrics.LimitHookTypeScript
			}
			started := time.Now()
			m.runLimitHookJob(limitHookJob{hookType: typ, httpMaxAttempts: 5, timeout: time.Minute})
			if calls != 1 || time.Since(started) > time.Second {
				t.Fatalf("calls=%d elapsed=%s", calls, time.Since(started))
			}
			result := exporter.recordedLimitHookExecutions()
			if len(result) != 1 {
				t.Fatalf("terminal=%+v", result)
			}
			if !script && result[0].outcome != resmanmetrics.LimitHookOutcomeCancelled {
				t.Fatalf("cancelled=%+v", result)
			}
		})
	}
}
func TestHookRetryBudgetAndSecretSafeClassification(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.GetBody != nil {
			t.Error("transport can replay POST outside worker accounting")
		}
		return nil, io.ErrUnexpectedEOF
	})}
	err := postLimitHookWithClient(t.Context(), client, "https://user:private@hooks.example/private-path?token=canary", limitHookEvent{EventID: "key"})
	if !limitHookRetryable(err) || calls.Load() != 1 {
		t.Fatalf("retry classification=%v calls=%d", err, calls.Load())
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		for _, secret := range []string{"private", "canary", "token="} {
			if strings.Contains(current.Error(), secret) {
				t.Fatalf("credential retained in error chain: %v", current)
			}
		}
	}
	if limitHookRetryable(&hookHTTPStatusError{status: 401}) || limitHookRetryable(newSanitizedHookError("create", "https://host/", errors.New("invalid"))) {
		t.Fatal("permanent failure classified retryable")
	}
	cfg := config.DefaultConfig()
	exporter := &mockPrometheusExporter{}
	m, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, exporter)
	if err != nil {
		t.Fatal(err)
	}
	m.executeHookRequest = func(context.Context, string, limitHookEvent) error { return &hookHTTPStatusError{status: 503} }
	m.runLimitHookJob(limitHookJob{hookType: resmanmetrics.LimitHookTypeHTTP, httpMaxAttempts: 5, timeout: 10 * time.Millisecond})
	results := exporter.recordedLimitHookExecutions()
	if len(results) != 1 || results[0].outcome != resmanmetrics.LimitHookOutcomeTimeout {
		t.Fatalf("budget=%+v", results)
	}
}

func TestHookOneShotTransportPreserves307And308Redirects(t *testing.T) {
	for _, code := range []int{307, 308} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var received atomic.Int64
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var event limitHookEvent
				if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
					t.Errorf("redirect body: %v", err)
				}
				if r.Method != http.MethodPost || event.EventID != "redirect-event" {
					t.Errorf("redirect changed request: method=%s event=%+v", r.Method, event)
				}
				received.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer destination.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(code)
			}))
			defer source.Close()
			if err := postLimitHook(t.Context(), source.URL, limitHookEvent{EventID: "redirect-event"}); err != nil {
				t.Fatal(err)
			}
			if received.Load() != 1 {
				t.Fatalf("redirect destination calls=%d", received.Load())
			}
		})
	}
}

func TestHookDispatchAssignsOneIdentityBeforeIndependentMechanismAdmission(t *testing.T) {
	cfg := config.DefaultConfig()
	m, err := NewManager(cfg, &mockMetricsCollector{}, &mockCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.LimitHookScript = "/unused-test-hook"
	cfg.LimitHookURL = "https://hooks.example"
	cfg.LimitHookHTTPMaxAttempts = 2
	identities := make(chan string, 3)
	var httpCalls atomic.Int64
	m.executeHookScript = func(_ context.Context, _ hookScriptInvocation, event limitHookEvent) error {
		identities <- event.EventID
		return nil
	}
	m.executeHookRequest = func(_ context.Context, _ string, event limitHookEvent) error {
		identities <- event.EventID
		if httpCalls.Add(1) == 1 {
			return &hookHTTPStatusError{status: 503}
		}
		return nil
	}
	m.dispatchLimitHookJobs(cfg, limitHookEvent{UID: 1000})
	var received []string
	for range 3 {
		select {
		case id := <-identities:
			received = append(received, id)
		case <-time.After(time.Second):
			t.Fatal("mechanism attempt absent")
		}
	}
	m.stopLimitHooks()
	if received[0] == "" || received[0] != received[1] || received[0] != received[2] || httpCalls.Load() != 2 {
		t.Fatalf("identities=%v calls=%d", received, httpCalls.Load())
	}
}
