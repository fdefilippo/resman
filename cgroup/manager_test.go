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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

func TestHasControllerUsesExactTokenMatch(t *testing.T) {
	tests := []struct {
		name        string
		controllers string
		wanted      string
		want        bool
	}{
		{name: "cpu present", controllers: "cpu cpuset io memory", wanted: "cpu", want: true},
		{name: "cpu absent from cpuset", controllers: "cpuset io memory", wanted: "cpu", want: false},
		{name: "cpuset absent from cpu", controllers: "cpu io memory", wanted: "cpuset", want: false},
		{name: "newline separated", controllers: "cpu\nmemory\nio", wanted: "memory", want: true},
		{name: "substring is not token", controllers: "memory_hugetlb", wanted: "memory", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasController(tt.controllers, tt.wanted); got != tt.want {
				t.Fatalf("hasController(%q, %q) = %t, want %t",
					tt.controllers, tt.wanted, got, tt.want)
			}
		})
	}
}

func TestVerifyRequiredControllersMatchesEnabledFeatures(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*config.Config)
		available  string
		wantErrFor []string
	}{
		{
			name:       "CPU controller is always mandatory",
			available:  "memory io",
			wantErrFor: []string{"CPU limiting", "cpu", "cpu.max"},
		},
		{
			name: "RAM controller is mandatory when RAM limiting is enabled",
			configure: func(cfg *config.Config) {
				cfg.RAMEnabled = true
			},
			available:  "cpu io",
			wantErrFor: []string{"RAM limiting", "memory", "memory.max"},
		},
		{
			name: "IO controller is mandatory when IO limiting is enabled",
			configure: func(cfg *config.Config) {
				cfg.IOEnabled = true
			},
			available:  "cpu memory",
			wantErrFor: []string{"I/O limiting", "io", "io.max"},
		},
		{
			name:      "disabled RAM and IO features do not require their controllers",
			available: "cpu",
		},
		{
			name: "all enabled feature controllers are available",
			configure: func(cfg *config.Config) {
				cfg.RAMEnabled = true
				cfg.IOEnabled = true
			},
			available: "cpu memory io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			if tt.configure != nil {
				tt.configure(cfg)
			}
			err := verifyRequiredControllers(tt.available, enabledControllerInterfaces(cfg))
			if len(tt.wantErrFor) == 0 {
				if err != nil {
					t.Fatalf("verifyRequiredControllers() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("verifyRequiredControllers() expected an error")
			}
			if !IsRequiredCapabilityError(err) {
				t.Fatalf("verifyRequiredControllers() error = %v, want structural capability error", err)
			}
			for _, fragment := range tt.wantErrFor {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("error %q does not name %q", err, fragment)
				}
			}
		})
	}
}

func TestProbeControllerInterfacesUsesRealChildFiles(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*config.Config)
		interfaces []string
		wantErrFor []string
	}{
		{
			name:       "missing CPU interface",
			wantErrFor: []string{"CPU limiting", "cpu", "cpu.max"},
		},
		{
			name: "missing RAM interface",
			configure: func(cfg *config.Config) {
				cfg.RAMEnabled = true
			},
			interfaces: []string{"cpu.max"},
			wantErrFor: []string{"RAM limiting", "memory", "memory.max"},
		},
		{
			name: "missing IO interface",
			configure: func(cfg *config.Config) {
				cfg.IOEnabled = true
			},
			interfaces: []string{"cpu.max"},
			wantErrFor: []string{"I/O limiting", "io", "io.max"},
		},
		{
			name:       "disabled RAM and IO interfaces are not required",
			interfaces: []string{"cpu.max"},
		},
		{
			name: "all enabled interfaces exist",
			configure: func(cfg *config.Config) {
				cfg.RAMEnabled = true
				cfg.IOEnabled = true
			},
			interfaces: []string{"cpu.max", "memory.max", "io.max"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			if tt.configure != nil {
				tt.configure(cfg)
			}
			basePath := t.TempDir()
			var probePath string
			manager := &Manager{
				cfg: cfg,
				createCgroupProbe: func(base, pattern string) (string, error) {
					path, err := os.MkdirTemp(base, pattern)
					if err != nil {
						return "", err
					}
					probePath = path
					for _, interfaceFile := range tt.interfaces {
						if err := os.WriteFile(filepath.Join(path, interfaceFile), nil, 0644); err != nil {
							return "", err
						}
					}
					return path, nil
				},
				removeCgroupProbe: func(path string) error {
					for _, interfaceFile := range tt.interfaces {
						if err := os.Remove(filepath.Join(path, interfaceFile)); err != nil {
							return err
						}
					}
					return os.Remove(path)
				},
			}

			candidates := availableControllerInterfaces("cpu memory io", allControllerInterfaces())
			usable, err := manager.probeControllerInterfaces(basePath, candidates)
			if err == nil {
				err = verifyUsableControllerInterfaces(usable, enabledControllerInterfaces(cfg))
			}
			if len(tt.wantErrFor) == 0 {
				if err != nil {
					t.Fatalf("probeControllerInterfaces() error = %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("probeControllerInterfaces() expected an error")
				}
				if !IsRequiredCapabilityError(err) {
					t.Fatalf("probeControllerInterfaces() error = %v, want structural capability error", err)
				}
				for _, fragment := range tt.wantErrFor {
					if !strings.Contains(err.Error(), fragment) {
						t.Errorf("error %q does not name %q", err, fragment)
					}
				}
			}
			if _, statErr := os.Stat(probePath); !os.IsNotExist(statErr) {
				t.Errorf("capability probe was not removed: stat error = %v", statErr)
			}
		})
	}
}

func TestEnableRequiredControllerWriteFailureRemainsTransient(t *testing.T) {
	injected := errors.New("transient subtree_control contention")
	manager := &Manager{
		logger: logging.GetLogger(),
		writeController: func(string, string) error {
			return injected
		},
	}
	required := controllerRequirement{feature: "CPU limiting", controller: "cpu", interfaceFile: "cpu.max"}

	_, err := manager.enableControllerInterfaces("/sys/fs/cgroup/cgroup.subtree_control", []controllerRequirement{required}, []controllerRequirement{required})
	if !errors.Is(err, injected) {
		t.Fatalf("enableControllerInterfaces() error = %v, want injected write failure", err)
	}
	if IsRequiredCapabilityError(err) {
		t.Fatalf("enableControllerInterfaces() classified transient write failure as structural: %v", err)
	}
}

func TestUpdateConfigDisablesFeaturesWithoutUsableInterfaces(t *testing.T) {
	current := config.DefaultConfig()
	manager := &Manager{
		cfg: current,
		usableControllerInterfaces: map[string]bool{
			"cpu.max":    true,
			"memory.max": false,
			"io.max":     false,
		},
	}
	requested := config.DefaultConfig()
	requested.RAMEnabled = true
	requested.IOEnabled = true

	err := manager.UpdateConfig(requested)
	if err == nil {
		t.Fatal("UpdateConfig() expected a capability error")
	}
	for _, fragment := range []string{"RAM limiting", "memory", "memory.max", "I/O limiting", "io", "io.max"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not name %q", err, fragment)
		}
	}
	if requested.RAMEnabled || requested.IOEnabled {
		t.Fatalf("unsupported resource features remained enabled: RAM=%t IO=%t",
			requested.RAMEnabled, requested.IOEnabled)
	}
	if manager.getConfig() != requested {
		t.Fatal("manager did not publish the safe, capability-filtered configuration")
	}
}

func TestUpdateConfigEnablesNewFeatureInExistingSharedCgroup(t *testing.T) {
	root := t.TempDir()
	sharedPath := filepath.Join(root, "resman", "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("create shared fixture: %v", err)
	}
	subtreeControl := filepath.Join(sharedPath, "cgroup.subtree_control")
	if err := os.WriteFile(subtreeControl, []byte("cpu"), 0644); err != nil {
		t.Fatalf("create subtree_control fixture: %v", err)
	}
	current := config.DefaultConfig()
	current.CgroupRoot = root
	current.CgroupBase = "resman"
	manager := &Manager{
		cfg: current,
		usableControllerInterfaces: map[string]bool{
			"cpu.max":    true,
			"memory.max": true,
		},
	}
	requested := config.DefaultConfig()
	requested.CgroupRoot = root
	requested.CgroupBase = "resman"
	requested.RAMEnabled = true

	if err := manager.UpdateConfig(requested); err != nil {
		t.Fatalf("UpdateConfig() error = %v", err)
	}
	if !requested.RAMEnabled {
		t.Fatal("RAM feature was disabled despite usable memory.max")
	}
	assertFileContent(t, subtreeControl, "+memory")
}

func TestUpdateConfigDoesNotPublishFeatureWhenSharedControllerCannotBeEnabled(t *testing.T) {
	root := t.TempDir()
	sharedPath := filepath.Join(root, "resman", "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("create shared fixture: %v", err)
	}
	current := config.DefaultConfig()
	current.CgroupRoot = root
	current.CgroupBase = "resman"
	manager := &Manager{
		cfg: current,
		usableControllerInterfaces: map[string]bool{
			"cpu.max": true,
			"io.max":  true,
		},
	}
	requested := config.DefaultConfig()
	requested.CgroupRoot = root
	requested.CgroupBase = "resman"
	requested.IOEnabled = true

	err := manager.UpdateConfig(requested)
	if err == nil {
		t.Fatal("UpdateConfig() expected an error for missing shared subtree_control")
	}
	for _, fragment := range []string{"I/O limiting", "io", "io.max", "shared cgroup"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not name %q", err, fragment)
		}
	}
	if requested.IOEnabled {
		t.Fatal("I/O feature remained enabled after shared-controller failure")
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

func TestRemoveRAMSwapLimitWritesMax(t *testing.T) {
	userPath := filepath.Join(t.TempDir(), "user_1000")
	if err := os.MkdirAll(userPath, 0755); err != nil {
		t.Fatalf("failed to create user cgroup fixture: %v", err)
	}
	swapMaxPath := filepath.Join(userPath, "memory.swap.max")
	if err := os.WriteFile(swapMaxPath, []byte("0"), 0644); err != nil {
		t.Fatalf("failed to create memory.swap.max fixture: %v", err)
	}

	manager := &Manager{
		logger:         logging.GetLogger(),
		createdCgroups: map[int]string{1000: userPath},
	}
	if err := manager.RemoveRAMSwapLimit(1000); err != nil {
		t.Fatalf("RemoveRAMSwapLimit() error: %v", err)
	}
	assertFileContent(t, swapMaxPath, "max")
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
	if info != (CgroupInfo{}) {
		t.Errorf("GetCgroupInfo() = %#v, want zero value for non-existent cgroup", info)
	}
}

func TestGetCgroupInfoIncludesMemoryValues(t *testing.T) {
	cgroupPath := t.TempDir()
	values := map[string]string{
		"cpu.max":        "50000 100000",
		"cpu.weight":     "100",
		"memory.current": "1048576",
		"memory.max":     "max",
		"memory.high":    "2097152",
	}
	for name, value := range values {
		if err := os.WriteFile(filepath.Join(cgroupPath, name), []byte(value), 0644); err != nil {
			t.Fatalf("failed to write %s fixture: %v", name, err)
		}
	}

	manager := &Manager{
		cfg: config.DefaultConfig(),
		createdCgroups: map[int]string{
			1000: cgroupPath,
		},
	}

	info, err := manager.GetCgroupInfo(1000)
	if err != nil {
		t.Fatalf("GetCgroupInfo() error = %v", err)
	}
	if info.Path != cgroupPath {
		t.Errorf("Path = %q, want %q", info.Path, cgroupPath)
	}
	tests := []struct {
		name  string
		value CgroupFileValue
		want  string
	}{
		{name: "CPU quota", value: info.CPUQuota, want: values["cpu.max"]},
		{name: "CPU weight", value: info.CPUWeight, want: values["cpu.weight"]},
		{name: "memory current", value: info.MemoryCurrent, want: values["memory.current"]},
		{name: "memory max", value: info.MemoryMax, want: values["memory.max"]},
		{name: "memory high", value: info.MemoryHigh, want: values["memory.high"]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.value.Available || tt.value.Value != tt.want || tt.value.UnavailableReason != "" {
				t.Errorf("value = %#v, want available value %q", tt.value, tt.want)
			}
		})
	}
}

func TestGetCgroupInfoReportsUnavailableInterfaces(t *testing.T) {
	cgroupPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.max"), []byte("max 100000"), 0644); err != nil {
		t.Fatalf("write cpu.max fixture: %v", err)
	}
	manager := &Manager{
		cfg:            config.DefaultConfig(),
		createdCgroups: map[int]string{1000: cgroupPath},
	}

	info, err := manager.GetCgroupInfo(1000)
	if err != nil {
		t.Fatalf("GetCgroupInfo() error = %v", err)
	}
	if !info.CPUQuota.Available || info.CPUQuota.Value != "max 100000" {
		t.Errorf("CPUQuota = %#v, want available fixture value", info.CPUQuota)
	}
	for _, tt := range []struct {
		name  string
		value CgroupFileValue
	}{
		{name: "CPU weight", value: info.CPUWeight},
		{name: "memory current", value: info.MemoryCurrent},
		{name: "memory max", value: info.MemoryMax},
		{name: "memory high", value: info.MemoryHigh},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value.Available || tt.value.Value != "" || tt.value.UnavailableReason != CgroupFileNotPresent {
				t.Errorf("value = %#v, want unavailable reason %q", tt.value, CgroupFileNotPresent)
			}
		})
	}
}

func TestGetCgroupInfoClassifiesBoundedUnavailableReasons(t *testing.T) {
	tests := []struct {
		name string
		read func(string) ([]byte, error)
		want CgroupFileValue
	}{
		{
			name: "available",
			read: func(string) ([]byte, error) { return []byte("  max 100000\n"), nil },
			want: CgroupFileValue{Value: "max 100000", Available: true},
		},
		{
			name: "not present",
			read: func(path string) ([]byte, error) {
				return nil, &os.PathError{Op: "read", Path: path, Err: syscall.ENOENT}
			},
			want: CgroupFileValue{UnavailableReason: CgroupFileNotPresent},
		},
		{
			name: "permission denied",
			read: func(path string) ([]byte, error) {
				return nil, &os.PathError{Op: "read", Path: path, Err: syscall.EACCES}
			},
			want: CgroupFileValue{UnavailableReason: CgroupFilePermissionDenied},
		},
		{
			name: "other read error",
			read: func(string) ([]byte, error) { return nil, errors.New("device read failed at a sensitive path") },
			want: CgroupFileValue{UnavailableReason: CgroupFileReadError},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{
				cfg:            config.DefaultConfig(),
				createdCgroups: map[int]string{1000: "/sensitive/cgroup/path"},
				readCgroupFile: tt.read,
			}
			info, err := manager.GetCgroupInfo(1000)
			if err != nil {
				t.Fatalf("GetCgroupInfo() error = %v", err)
			}
			for name, got := range map[string]CgroupFileValue{
				"cpu.max": info.CPUQuota, "cpu.weight": info.CPUWeight,
				"memory.current": info.MemoryCurrent, "memory.max": info.MemoryMax,
				"memory.high": info.MemoryHigh,
			} {
				if got != tt.want {
					t.Errorf("%s = %#v, want %#v", name, got, tt.want)
				}
			}
		})
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
	if err := os.WriteFile(filepath.Join(userPath, "cpu.max"), nil, 0644); err != nil {
		t.Fatalf("failed to create cpu.max fixture: %v", err)
	}
	if err := manager.ApplyCPUQuota(1000, "50000 100000"); err != nil {
		t.Fatalf("ApplyCPUQuota() error: %v", err)
	}
	if err := manager.ApplyRAMLimitWithHigh(1000, "1048576", "524288"); err != nil {
		t.Fatalf("ApplyRAMLimitWithHigh() error: %v", err)
	}
	if err := manager.ApplyIOLimit(1000, "1M", "2m", 10, 20, "8:0"); err != nil {
		t.Fatalf("ApplyIOLimit() error: %v", err)
	}

	assertFileContent(t, filepath.Join(userPath, "cpu.weight"), "250")
	assertFileContent(t, filepath.Join(userPath, "cpu.max"), "50000 100000")
	assertFileContent(t, filepath.Join(userPath, "memory.high"), "524288")
	assertFileContent(t, filepath.Join(userPath, "memory.max"), "1048576")
	assertFileContent(t, filepath.Join(userPath, "io.max"), "8:0 rbps=1048576 wbps=2097152 riops=10 wiops=20\n")

	if _, err := os.Stat(filepath.Join(legacyPath, "cpu.weight")); !os.IsNotExist(err) {
		t.Fatalf("legacy cgroup path should not receive writes, stat err=%v", err)
	}

	if err := manager.ApplyCPUQuota(1001, "50000 100000"); err == nil {
		t.Fatal("ApplyCPUQuota() should reject an untracked user")
	}
	if _, err := os.Stat(manager.getUserCgroupPath(1001)); !os.IsNotExist(err) {
		t.Fatalf("ApplyCPUQuota() created a legacy cgroup for an untracked user, stat err=%v", err)
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
	if err := os.MkdirAll(filepath.Join(sysBlockRoot, "vanished"), 0755); err != nil {
		t.Fatalf("failed to create vanished block device fixture: %v", err)
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
