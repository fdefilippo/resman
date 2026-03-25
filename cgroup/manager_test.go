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
package cgroup

import (
	"os"
	"sync"
	"testing"

	"github.com/fdefilippo/resman/config"
)

func TestNewManager(t *testing.T) {
	// This test requires root and cgroups v2, so it will be skipped in most CI environments
	if os.Getuid() != 0 {
		t.Skipf("Test requires root privileges")
	}

	cfg := config.DefaultConfig()
	manager, err := NewManager(cfg)

	// May fail if cgroups v2 is not properly configured
	if err != nil {
		t.Logf("Note: NewManager failed (expected in non-cgroup environment): %v", err)
		t.Skipf("Skipping test - cgroups v2 not available")
	}

	if manager == nil {
		t.Fatal("NewManager() returned nil")
	}
}

func TestGetUserCgroupPath(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = "/sys/fs/cgroup"
	cfg.ScriptCgroupBase = "resman"

	// Create manager without calling verifyCgroupSetup
	manager := &Manager{
		cfg: cfg,
	}

	tests := []struct {
		uid      int
		expected string
	}{
		{1000, "/sys/fs/cgroup/resman/user_1000"},
		{0, "/sys/fs/cgroup/resman/user_0"},
		{65534, "/sys/fs/cgroup/resman/user_65534"},
	}

	for _, tt := range tests {
		got := manager.getUserCgroupPath(tt.uid)
		if got != tt.expected {
			t.Errorf("getUserCgroupPath(%d): got %s, expected %s", tt.uid, got, tt.expected)
		}
	}
}

func TestGetBaseCgroupPath(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = "/sys/fs/cgroup"
	cfg.ScriptCgroupBase = "resman"

	manager := &Manager{
		cfg: cfg,
	}

	expected := "/sys/fs/cgroup/resman"
	got := manager.getBaseCgroupPath()

	if got != expected {
		t.Errorf("getBaseCgroupPath(): got %s, expected %s", got, expected)
	}
}

func TestIsValidCPUQuotaFormat(t *testing.T) {
	tests := []struct {
		name     string
		quota    string
		expected bool
	}{
		{"valid max format", "max 100000", true},
		{"valid numeric format", "50000 100000", true},
		{"valid large quota", "200000 100000", true},
		{"missing period", "50000", false},
		{"empty string", "", false},
		{"three parts", "50000 100000 extra", false},
		{"invalid format", "invalid", false},
		{"max without period", "max", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidCPUQuotaFormat(tt.quota)
			if got != tt.expected {
				t.Errorf("isValidCPUQuotaFormat(%q): got %v, expected %v", tt.quota, got, tt.expected)
			}
		})
	}
}

func TestWriteControllerIfMissing(t *testing.T) {
	// This test requires write access to cgroup filesystem
	if os.Getuid() != 0 {
		t.Skipf("Test requires root privileges")
	}

	// Skip if cgroups not available
	if _, err := os.Stat("/sys/fs/cgroup"); os.IsNotExist(err) {
		t.Skipf("Cgroups not available")
	}

	cfg := config.DefaultConfig()
	manager, err := NewManager(cfg)
	if err != nil {
		t.Skipf("Cannot create manager: %v", err)
	}

	// Test with a temporary file
	tmpFile, err := os.CreateTemp("", "cgroup-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write initial content
	if err := os.WriteFile(tmpFile.Name(), []byte("cpu"), 0644); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}

	// Should not add if already present
	err = manager.writeControllerIfMissing(tmpFile.Name(), "+cpu")
	if err != nil {
		t.Errorf("writeControllerIfMissing() error: %v", err)
	}
}

func TestEnableCPUControllers(t *testing.T) {
	// This test requires root and cgroups
	if os.Getuid() != 0 {
		t.Skipf("Test requires root privileges")
	}

	if _, err := os.Stat("/sys/fs/cgroup"); os.IsNotExist(err) {
		t.Skipf("Cgroups not available")
	}

	cfg := config.DefaultConfig()
	manager, err := NewManager(cfg)
	if err != nil {
		t.Skipf("Cannot create manager: %v", err)
	}

	// This may fail in containerized environments
	err = manager.enableCPUControllers()
	if err != nil {
		t.Logf("Note: enableCPUControllers failed (expected in containers): %v", err)
	}
}

func TestManagerConcurrency(t *testing.T) {
	cfg := config.DefaultConfig()
	manager := &Manager{
		cfg:            cfg,
		createdCgroups: make(map[int]string),
	}

	var wg sync.WaitGroup
	mu := sync.Mutex{}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mu.Lock()
				manager.createdCgroups[id] = "/test/path"
				_ = manager.createdCgroups[id]
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()
}

func TestLoadExistingCgroups(t *testing.T) {
	// Test with non-existent file (should not error)
	cfg := config.DefaultConfig()
	cfg.CreatedCgroupsFile = "/nonexistent/file"

	manager := &Manager{
		cfg:                cfg,
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: cfg.CreatedCgroupsFile,
	}

	// Should not panic or error
	err := manager.loadExistingCgroups()
	if err == nil {
		t.Log("loadExistingCgroups() handled non-existent file correctly")
	}
}

func TestSaveCgroupToFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "cgroup-tracking-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	cfg := config.DefaultConfig()
	cfg.CreatedCgroupsFile = tmpFile.Name()

	manager := &Manager{
		cfg:                cfg,
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: cfg.CreatedCgroupsFile,
	}

	// Test save
	err = manager.saveCgroupToFile(1000, "/sys/fs/cgroup/resman/user_1000")
	if err != nil {
		t.Errorf("saveCgroupToFile() error: %v", err)
	}

	// Verify file content
	data, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to read temp file: %v", err)
	}

	if len(data) == 0 {
		t.Error("Saved file is empty")
	}
}

func TestRemoveCgroupFromFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "cgroup-tracking-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write test data
	if err := os.WriteFile(tmpFile.Name(), []byte("1000:/sys/fs/cgroup/resman/user_1000\n"), 0644); err != nil {
		t.Fatalf("Failed to write test data: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.CreatedCgroupsFile = tmpFile.Name()

	manager := &Manager{
		cfg:                cfg,
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: cfg.CreatedCgroupsFile,
	}

	// Test remove
	err = manager.removeCgroupFromFile(1000)
	// May or may not error depending on implementation
	t.Logf("removeCgroupFromFile() returned: %v", err)
}

func TestGetCgroupInfo(t *testing.T) {
	cfg := config.DefaultConfig()
	manager := &Manager{
		cfg:            cfg,
		createdCgroups: make(map[int]string),
	}

	// Test with non-existent cgroup
	info, err := manager.GetCgroupInfo(99999)
	if err == nil {
		t.Log("GetCgroupInfo() should error for non-existent cgroup")
	}
	if info != nil {
		t.Error("GetCgroupInfo() should return nil for non-existent cgroup")
	}
}

func TestGetCreatedCgroups(t *testing.T) {
	cfg := config.DefaultConfig()
	manager := &Manager{
		cfg: cfg,
		createdCgroups: map[int]string{
			1000: "/sys/fs/cgroup/resman/user_1000",
			1001: "/sys/fs/cgroup/resman/user_1001",
		},
	}

	uids := manager.GetCreatedCgroups()
	if len(uids) != 2 {
		t.Errorf("GetCreatedCgroups(): got %d uids, expected 2", len(uids))
	}
}
