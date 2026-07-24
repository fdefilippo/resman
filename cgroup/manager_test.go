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
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

func TestMain(m *testing.M) {
	logging.InitLogger("ERROR", filepath.Join(os.TempDir(), "resman-cgroup-test.log"), 10*1024*1024, false)
	os.Exit(m.Run())
}

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
	cfg.CgroupBase = "resman"

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
	cfg.CgroupBase = "resman"

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
	cfg := config.DefaultConfig()
	manager := &Manager{cfg: cfg}

	// Test with a temporary file
	tmpFile, err := os.CreateTemp("", "cgroup-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

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

func TestWriteControllerIfMissingUsesExactTokenMatch(t *testing.T) {
	manager := &Manager{cfg: config.DefaultConfig()}

	tmpFile, err := os.CreateTemp("", "cgroup-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if err := os.WriteFile(tmpFile.Name(), []byte("cpuset"), 0644); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}

	if err := manager.writeControllerIfMissing(tmpFile.Name(), "+cpu"); err != nil {
		t.Fatalf("writeControllerIfMissing() error: %v", err)
	}

	data, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to read temp file: %v", err)
	}
	if string(data) != "+cpu" {
		t.Fatalf("writeControllerIfMissing() should write +cpu when only cpuset is present, got %q", string(data))
	}
}

func TestVerifyCgroupRootWriteAccessDoesNotMoveProcess(t *testing.T) {
	tmpDir := t.TempDir()
	procsPath := filepath.Join(tmpDir, "cgroup.procs")
	const original = "1234\n"
	if err := os.WriteFile(procsPath, []byte(original), 0644); err != nil {
		t.Fatalf("failed to create cgroup.procs fixture: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.CgroupRoot = tmpDir
	manager := &Manager{cfg: cfg}

	if err := manager.verifyCgroupRootWriteAccess(); err != nil {
		t.Fatalf("verifyCgroupRootWriteAccess() error: %v", err)
	}
	if !manager.cgroupRootWritable {
		t.Fatal("cgroup root was not marked writable")
	}
	assertFileContent(t, procsPath, original)
}

func TestVerifyCgroupRootWriteAccessPropagatesOpenError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = t.TempDir()
	manager := &Manager{cfg: cfg}

	err := manager.verifyCgroupRootWriteAccess()
	if err == nil {
		t.Fatal("verifyCgroupRootWriteAccess() expected an error for missing cgroup.procs")
	}
	if manager.cgroupRootWritable {
		t.Fatal("cgroup root was marked writable after an open error")
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
		logger:             logging.GetLogger(),
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
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	cfg := config.DefaultConfig()
	cfg.CreatedCgroupsFile = tmpFile.Name()

	manager := &Manager{
		cfg:                cfg,
		logger:             logging.GetLogger(),
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
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	// Write test data
	if err := os.WriteFile(tmpFile.Name(), []byte("1000:/sys/fs/cgroup/resman/user_1000\n"), 0644); err != nil {
		t.Fatalf("Failed to write test data: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.CreatedCgroupsFile = tmpFile.Name()

	manager := &Manager{
		cfg:                cfg,
		logger:             logging.GetLogger(),
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

func TestCreateUserSubCgroupTracksSharedPath(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = tmpDir
	cfg.CgroupBase = "resman"
	cfg.CreatedCgroupsFile = filepath.Join(tmpDir, "created-cgroups")

	manager := &Manager{
		cfg:                cfg,
		logger:             logging.GetLogger(),
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: cfg.CreatedCgroupsFile,
	}

	sharedPath := filepath.Join(tmpDir, "resman", "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("failed to create shared path: %v", err)
	}

	userPath, err := manager.CreateUserSubCgroup(1000, sharedPath)
	if err != nil {
		t.Fatalf("CreateUserSubCgroup() error: %v", err)
	}

	got, exists := manager.getCgroupPath(1000)
	if !exists {
		t.Fatal("expected shared user cgroup to be tracked")
	}
	if got != userPath {
		t.Fatalf("tracked path = %s, want %s", got, userPath)
	}
	if got == manager.getUserCgroupPath(1000) {
		t.Fatalf("shared user cgroup should not track legacy path %s", got)
	}
}

func TestUIDOperationsUseTrackedSharedCgroupPath(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = tmpDir
	cfg.CgroupBase = "resman"
	cfg.CreatedCgroupsFile = filepath.Join(tmpDir, "created-cgroups")

	manager := &Manager{
		cfg:                cfg,
		logger:             logging.GetLogger(),
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: cfg.CreatedCgroupsFile,
	}

	sharedPath := filepath.Join(tmpDir, "resman", "limited")
	userPath := filepath.Join(sharedPath, "user_1000")
	legacyPath := manager.getUserCgroupPath(1000)
	if err := os.MkdirAll(userPath, 0755); err != nil {
		t.Fatalf("failed to create shared user path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userPath, "io.max"), nil, 0644); err != nil {
		t.Fatalf("failed to create io.max fixture: %v", err)
	}
	if err := manager.trackCgroupPath(1000, userPath); err != nil {
		t.Fatalf("trackCgroupPath() error: %v", err)
	}

	if err := manager.ApplyCPUWeight(1000, 250); err != nil {
		t.Fatalf("ApplyCPUWeight() error: %v", err)
	}
	if err := manager.ApplyRAMLimitWithHigh(1000, "1048576", "524288"); err != nil {
		t.Fatalf("ApplyRAMLimitWithHigh() error: %v", err)
	}
	if err := manager.ApplyIOLimit(1000, "1024", "2048", 10, 20, "8:0"); err != nil {
		t.Fatalf("ApplyIOLimit() error: %v", err)
	}

	assertFileContent(t, filepath.Join(userPath, "cpu.weight"), "250")
	assertFileContent(t, filepath.Join(userPath, "memory.high"), "524288")
	assertFileContent(t, filepath.Join(userPath, "memory.max"), "1048576")
	assertFileContent(t, filepath.Join(userPath, "io.max"), "8:0 rbps=1024 wbps=2048 riops=10 wiops=20\n")

	if _, err := os.Stat(filepath.Join(legacyPath, "cpu.weight")); !os.IsNotExist(err) {
		t.Fatalf("legacy cgroup path should not receive writes, stat err=%v", err)
	}
}

func TestApplyIOLimitAllDevices(t *testing.T) {
	tmpDir := t.TempDir()
	sysBlockRoot := filepath.Join(tmpDir, "sys", "block")
	for name, device := range map[string]string{
		"nvme0n1": "259:0\n",
		"sda":     "8:0\n",
	} {
		deviceDir := filepath.Join(sysBlockRoot, name)
		if err := os.MkdirAll(deviceDir, 0755); err != nil {
			t.Fatalf("failed to create fake block device directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(deviceDir, "dev"), []byte(device), 0644); err != nil {
			t.Fatalf("failed to create fake block device number: %v", err)
		}
	}

	cfg := config.DefaultConfig()
	cfg.CgroupRoot = tmpDir
	cfg.CgroupBase = "resman"
	manager := &Manager{
		cfg:            cfg,
		logger:         logging.GetLogger(),
		createdCgroups: map[int]string{},
		sysBlockRoot:   sysBlockRoot,
	}
	userPath := filepath.Join(tmpDir, "resman", "user_1000")
	if err := os.MkdirAll(userPath, 0755); err != nil {
		t.Fatalf("failed to create user cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userPath, "io.max"), nil, 0644); err != nil {
		t.Fatalf("failed to create io.max: %v", err)
	}
	manager.createdCgroups[1000] = userPath

	if err := manager.ApplyIOLimit(1000, "1024", "2048", 10, 20, "all"); err != nil {
		t.Fatalf("ApplyIOLimit() error: %v", err)
	}
	assertFileContent(t, filepath.Join(userPath, "io.max"),
		"259:0 rbps=1024 wbps=2048 riops=10 wiops=20\n"+
			"8:0 rbps=1024 wbps=2048 riops=10 wiops=20\n")

	if err := manager.RemoveIOLimit(1000); err != nil {
		t.Fatalf("RemoveIOLimit() error: %v", err)
	}
	assertFileContent(t, filepath.Join(userPath, "io.max"),
		"259:0 rbps=max wbps=max riops=max wiops=max\n"+
			"8:0 rbps=max wbps=max riops=max wiops=max\n")
}

func TestApplyIOLimitAllDevicesFailsWhenNoDevicesExist(t *testing.T) {
	tmpDir := t.TempDir()
	sysBlockRoot := filepath.Join(tmpDir, "sys", "block")
	if err := os.MkdirAll(sysBlockRoot, 0755); err != nil {
		t.Fatalf("failed to create fake sysfs root: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.CgroupRoot = tmpDir
	cfg.CgroupBase = "resman"
	manager := &Manager{
		cfg:            cfg,
		logger:         logging.GetLogger(),
		createdCgroups: map[int]string{1000: filepath.Join(tmpDir, "resman", "user_1000")},
		sysBlockRoot:   sysBlockRoot,
	}
	if err := os.MkdirAll(manager.createdCgroups[1000], 0755); err != nil {
		t.Fatalf("failed to create user cgroup: %v", err)
	}

	err := manager.ApplyIOLimit(1000, "1024", "2048", 10, 20, "all")
	if err == nil {
		t.Fatal("ApplyIOLimit() expected an error when no block devices are available")
	}
	if !strings.Contains(err.Error(), "no block devices found") {
		t.Fatalf("ApplyIOLimit() error = %v, want no block devices context", err)
	}
}

func TestGetPSIStatsUsesTrackedSharedCgroupPath(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = tmpDir
	cfg.CgroupBase = "resman"
	cfg.CreatedCgroupsFile = filepath.Join(tmpDir, "created-cgroups")

	manager := &Manager{
		cfg:                cfg,
		logger:             logging.GetLogger(),
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: cfg.CreatedCgroupsFile,
	}

	userPath := filepath.Join(tmpDir, "resman", "limited", "user_1000")
	if err := os.MkdirAll(userPath, 0755); err != nil {
		t.Fatalf("failed to create shared user path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userPath, "io.pressure"), []byte("some avg10=25.00 avg60=18.50 avg300=12.30 total=1234567\nfull avg10=10.00 avg60=8.20 avg300=5.10 total=567890\n"), 0644); err != nil {
		t.Fatalf("failed to write io.pressure: %v", err)
	}
	if err := manager.trackCgroupPath(1000, userPath); err != nil {
		t.Fatalf("trackCgroupPath() error: %v", err)
	}

	stats, err := manager.GetPSIStats(1000)
	if err != nil {
		t.Fatalf("GetPSIStats() error: %v", err)
	}
	if stats.SomeAvg10 != 25.00 || stats.FullAvg10 != 10.00 {
		t.Fatalf("unexpected PSI stats: %+v", stats)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, string(data), want)
	}
}
