package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
)

func TestProcessIDsForUIDReusesSingleScan(t *testing.T) {
	manager := &Manager{cfg: config.DefaultConfig()}
	scanCalls := 0
	manager.scanProcessIDs = func() (map[int][]int, error) {
		scanCalls++
		return map[int][]int{
			1000: {101, 102},
			1001: {201},
		}, nil
	}

	first, err := manager.processIDsForUID(1000)
	if err != nil {
		t.Fatalf("processIDsForUID(1000) error: %v", err)
	}
	second, err := manager.processIDsForUID(1001)
	if err != nil {
		t.Fatalf("processIDsForUID(1001) error: %v", err)
	}

	if scanCalls != 1 {
		t.Fatalf("process scan calls = %d, want 1", scanCalls)
	}
	if !reflect.DeepEqual(first, []int{101, 102}) || !reflect.DeepEqual(second, []int{201}) {
		t.Fatalf("unexpected cached process index: first=%v second=%v", first, second)
	}

	first[0] = 999
	again, err := manager.processIDsForUID(1000)
	if err != nil {
		t.Fatalf("second processIDsForUID(1000) error: %v", err)
	}
	if again[0] != 101 {
		t.Fatalf("caller modified cached PID slice: %v", again)
	}
}

func TestGetProcessInfoCachesUsernameLookup(t *testing.T) {
	procRoot := t.TempDir()
	cfg := config.DefaultConfig()
	manager := &Manager{
		cfg:           cfg,
		procRoot:      procRoot,
		usernameCache: make(map[string]cachedUsername),
	}

	lookupCalls := 0
	manager.resolveUsername = func(uid string) (string, error) {
		lookupCalls++
		if uid != "1000" {
			return "", fmt.Errorf("unexpected UID %s", uid)
		}
		return "alice", nil
	}

	for _, pid := range []int{101, 102} {
		processPath := filepath.Join(procRoot, fmt.Sprintf("%d", pid))
		if err := os.MkdirAll(processPath, 0755); err != nil {
			t.Fatalf("failed to create fake process path: %v", err)
		}
		if err := os.WriteFile(filepath.Join(processPath, "comm"), []byte("worker\n"), 0644); err != nil {
			t.Fatalf("failed to write comm: %v", err)
		}
		if err := os.WriteFile(filepath.Join(processPath, "cmdline"), []byte("/usr/bin/worker\x00--serve\x00"), 0644); err != nil {
			t.Fatalf("failed to write cmdline: %v", err)
		}
		status := "Name:\tworker\nState:\tS (sleeping)\nUid:\t1000\t1000\t1000\t1000\n"
		if err := os.WriteFile(filepath.Join(processPath, "status"), []byte(status), 0644); err != nil {
			t.Fatalf("failed to write status: %v", err)
		}

		info, err := manager.getProcessInfo(pid)
		if err != nil {
			t.Fatalf("getProcessInfo(%d) error: %v", pid, err)
		}
		if info["username"] != "alice" || info["state"] != "S" {
			t.Fatalf("getProcessInfo(%d) = %v", pid, info)
		}
		if got := processNameFromInfo(pid, info); got != fmt.Sprintf("worker[%d]", pid) {
			t.Fatalf("processNameFromInfo(%d) = %q", pid, got)
		}
	}

	if lookupCalls != 1 {
		t.Fatalf("username lookup calls = %d, want 1", lookupCalls)
	}
}

func TestProcessIDsForUIDRefreshesExpiredScan(t *testing.T) {
	manager := &Manager{cfg: config.DefaultConfig()}
	scanCalls := 0
	manager.scanProcessIDs = func() (map[int][]int, error) {
		scanCalls++
		return map[int][]int{1000: {scanCalls}}, nil
	}

	if _, err := manager.processIDsForUID(1000); err != nil {
		t.Fatalf("initial processIDsForUID() error: %v", err)
	}
	manager.processScan.createdAt = time.Now().Add(-2 * processScanCacheTTL)
	refreshed, err := manager.processIDsForUID(1000)
	if err != nil {
		t.Fatalf("refreshed processIDsForUID() error: %v", err)
	}
	if scanCalls != 2 || !reflect.DeepEqual(refreshed, []int{2}) {
		t.Fatalf("scanCalls=%d refreshed=%v, want 2 and [2]", scanCalls, refreshed)
	}
}
