package state

import (
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
)

type capturingHookLogger struct {
	message string
	fields  []interface{}
}

func (l *capturingHookLogger) Warn(message string, fields ...interface{}) {
	l.message = message
	l.fields = append([]interface{}(nil), fields...)
}

func TestPostLimitHook(t *testing.T) {
	var received limitHookEvent

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s, expected POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type: got %s, expected application/json", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	event := limitHookEvent{
		UID:             1000,
		Username:        "app",
		CPUUsage:        82.5,
		LimitedUsers:    2,
		SharedCgroup:    "/sys/fs/cgroup/resman/limited",
		Timestamp:       time.Now().UTC(),
		LimitHookSource: "resman",
	}

	if err := postLimitHook(t.Context(), server.URL, event); err != nil {
		t.Fatalf("postLimitHook() error: %v", err)
	}
	if received.UID != event.UID || received.Username != event.Username {
		t.Fatalf("received event: got uid=%d username=%q", received.UID, received.Username)
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
	reportLimitHookURLFailure(logger, limitHookEvent{UID: 1000, Username: "app"}, endpoint, err)
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

	script := "#!/bin/sh\nprintf '%s:%s' \"$RESMAN_LIMIT_UID\" \"$RESMAN_LIMIT_USERNAME\" > \"" + outputPath + "\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	event := limitHookEvent{
		UID:             1000,
		Username:        "app",
		Timestamp:       time.Now().UTC(),
		LimitHookSource: "resman",
	}

	if err := runLimitHookScript(t.Context(), scriptPath, event); err != nil {
		t.Fatalf("runLimitHookScript() error: %v", err)
	}

	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read hook output: %v", err)
	}
	if string(output) != "1000:app" {
		t.Fatalf("script output: got %q, expected %q", string(output), "1000:app")
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
	reportLimitHookScriptFailure(logger, limitHookEvent{UID: 1000, Username: "app"}, err)
	logged := fmt.Sprint(logger.message, logger.fields)
	for _, secret := range []string{"stdout-canary", "stderr-canary", scriptPath} {
		if strings.Contains(logged, secret) {
			t.Fatalf("limit-hook warning exposed %q: %s", secret, logged)
		}
	}
}
