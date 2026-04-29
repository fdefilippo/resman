package state

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
