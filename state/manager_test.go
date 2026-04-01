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
package state

import (
	"context"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/metrics"
)

// Mock implementations for testing
type mockMetricsCollector struct{}

func (m *mockMetricsCollector) GetTotalCores() int                              { return 4 }
func (m *mockMetricsCollector) GetTotalCPUUsage() float64                       { return 50.0 }
func (m *mockMetricsCollector) GetUserCPUUsage(uid int) float64                 { return 10.0 }
func (m *mockMetricsCollector) GetMemoryUsage() float64                         { return 1024.0 }
func (m *mockMetricsCollector) GetTotalMemoryMB() float64                       { return 16384.0 }
func (m *mockMetricsCollector) GetCachedMemoryMB() float64                      { return 4096.0 }
func (m *mockMetricsCollector) IsSystemUnderLoad() bool                         { return false }
func (m *mockMetricsCollector) GetAllUserMetrics() map[int]*metrics.UserMetrics { return nil }
func (m *mockMetricsCollector) GetDBWriter() *metrics.DBWriter                  { return nil }
func (m *mockMetricsCollector) WriteMetricsToDatabase(userMetrics map[int]*metrics.UserMetrics, totalCPUUsage float64, totalCores int, systemLoad float64, limitsActive bool, limitedUsersCount int) {
}

// ALL USERS metrics
func (m *mockMetricsCollector) GetAllUsers() []int             { return []int{1000, 1001, 1002} }
func (m *mockMetricsCollector) GetAllUsersCPUUsage() float64   { return 40.0 }
func (m *mockMetricsCollector) GetAllUsersMemoryUsage() uint64 { return 2000000000 }

// LIMITED USERS metrics
func (m *mockMetricsCollector) GetLimitedUsers() []int             { return []int{1000, 1001} }
func (m *mockMetricsCollector) GetLimitedUsersCPUUsage() float64   { return 30.0 }
func (m *mockMetricsCollector) GetLimitedUsersMemoryUsage() uint64 { return 1500000000 }

type mockCgroupManager struct{}

func (m *mockCgroupManager) CreateUserCgroup(uid int) error                            { return nil }
func (m *mockCgroupManager) ApplyCPULimit(uid int, quota string) error                 { return nil }
func (m *mockCgroupManager) ApplyCPUWeight(uid int, weight int) error                  { return nil }
func (m *mockCgroupManager) RemoveCPULimit(uid int) error                              { return nil }
func (m *mockCgroupManager) ApplyRAMLimit(uid int, limit string) error                 { return nil }
func (m *mockCgroupManager) ApplyRAMLimitWithSwapDisabled(uid int, limit string) error { return nil }
func (m *mockCgroupManager) ApplyRAMHigh(uid int, limit string) error                  { return nil }
func (m *mockCgroupManager) ApplyRAMLimitWithHigh(uid int, maxLimit, highLimit string) error {
	return nil
}
func (m *mockCgroupManager) ApplyRAMLimitWithHighAndSwapDisabled(uid int, maxLimit, highLimit string) error {
	return nil
}
func (m *mockCgroupManager) RemoveRAMLimit(uid int) error { return nil }
func (m *mockCgroupManager) RemoveRAMHigh(uid int) error  { return nil }
func (m *mockCgroupManager) GetCgroupRAMUsage(uid int) (uint64, error) {
	return 0, nil
}
func (m *mockCgroupManager) GetMemoryHighEvents(uid int) (uint64, error) {
	return 0, nil
}
func (m *mockCgroupManager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	return nil
}
func (m *mockCgroupManager) RemoveIOLimit(uid int) error { return nil }
func (m *mockCgroupManager) GetIOStats(uid int) (uint64, uint64, uint64, uint64, error) {
	return 0, 0, 0, 0, nil
}
func (m *mockCgroupManager) CleanupUserCgroup(uid int) error            { return nil }
func (m *mockCgroupManager) MoveProcessToCgroup(pid int, uid int) error { return nil }
func (m *mockCgroupManager) MoveAllUserProcessesToSharedCgroup(uid int, path string) error {
	return nil
}
func (m *mockCgroupManager) CreateSharedCgroup() (string, error)                      { return "", nil }
func (m *mockCgroupManager) ApplySharedCPULimit(path string, quota string) error      { return nil }
func (m *mockCgroupManager) CreateUserSubCgroup(uid int, path string) (string, error) { return "", nil }
func (m *mockCgroupManager) CleanupAll() error                                        { return nil }
func (m *mockCgroupManager) GetCgroupInfo(uid int) (map[string]string, error)         { return nil, nil }
func (m *mockCgroupManager) GetCreatedCgroups() []int                                 { return nil }

type mockPrometheusExporter struct{}

func (m *mockPrometheusExporter) UpdateMetrics(metrics map[string]float64) {}
func (m *mockPrometheusExporter) UpdateUserMetrics(uid int, user string, cpu float64, mem uint64, proc int, limited bool, path, quota string, memoryHighEvents uint64, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64) {
}
func (m *mockPrometheusExporter) UpdateSystemMetrics(cores int, actionCores int, load float64) {}
func (m *mockPrometheusExporter) Start(ctx context.Context) error                              { return nil }
func (m *mockPrometheusExporter) Stop() error                                                  { return nil }
func (m *mockPrometheusExporter) CleanupUserMetrics(activeUids map[int]bool)                   {}
func (m *mockPrometheusExporter) IncrementLimitsActivated()                                    {}
func (m *mockPrometheusExporter) IncrementLimitsDeactivated()                                  {}

func TestNewManager(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, err := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	if manager == nil {
		t.Fatal("NewManager() returned nil")
	}
}

func TestNewManagerNilConfig(t *testing.T) {
	_, err := NewManager(nil, nil, nil, nil)

	if err == nil {
		t.Error("NewManager() should error with nil config")
	}
}

func TestMakeDecision(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 75
	cfg.CPUReleaseThreshold = 40
	cfg.MinActiveTime = 60
	cfg.CPUThresholdDuration = 0 // Disable time window for immediate activation

	manager := &Manager{
		cfg:              cfg,
		limitsActive:     false,
		thresholdTracker: &ThresholdTracker{},
	}

	metrics := &SystemMetrics{
		LimitedUsersCPUUsage: 80.0, // Above threshold
		TotalCores:           4,
		SystemUnderLoad:      false,
	}

	decision, reason := manager.makeDecision(metrics)

	if decision != "ACTIVATE_LIMITS" {
		t.Errorf("makeDecision(): got %s, expected ACTIVATE_LIMITS", decision)
	}
	if reason == "" {
		t.Error("makeDecision() should return a reason")
	}
}

func TestMakeDecisionDeactivate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 75
	cfg.CPUReleaseThreshold = 40

	manager := &Manager{
		cfg:              cfg,
		limitsActive:     true,
		thresholdTracker: &ThresholdTracker{},
	}

	metrics := &SystemMetrics{
		LimitedUsersCPUUsage: 30.0, // Below release threshold
		SystemUnderLoad:      false,
	}

	decision, reason := manager.makeDecision(metrics)

	if decision != "DEACTIVATE_LIMITS" {
		t.Errorf("makeDecision(): got %s, expected DEACTIVATE_LIMITS", decision)
	}
	if reason == "" {
		t.Error("makeDecision() should return a reason")
	}
}

func TestMakeDecisionMaintain(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 75
	cfg.CPUReleaseThreshold = 40

	manager := &Manager{
		cfg:              cfg,
		limitsActive:     false,
		thresholdTracker: &ThresholdTracker{},
	}

	metrics := &SystemMetrics{
		LimitedUsersCPUUsage: 50.0, // Between thresholds
		SystemUnderLoad:      false,
	}

	decision, _ := manager.makeDecision(metrics)

	if decision != "MAINTAIN_CURRENT_STATE" {
		t.Errorf("makeDecision(): got %s, expected MAINTAIN_CURRENT_STATE", decision)
	}
}

func TestBoolToFloat(t *testing.T) {
	tests := []struct {
		input    bool
		expected float64
	}{
		{true, 1.0},
		{false, 0.0},
	}

	for _, tt := range tests {
		got := boolToFloat(tt.input)
		if got != tt.expected {
			t.Errorf("boolToFloat(%v): got %f, expected %f", tt.input, got, tt.expected)
		}
	}
}

func TestGetStatus(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	status := manager.GetStatus()

	if status == nil {
		t.Fatal("GetStatus() returned nil")
	}

	if _, ok := status["limits_active"]; !ok {
		t.Error("GetStatus() should include limits_active")
	}
}

func TestCollectSystemMetrics(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	sysMetrics, err := manager.collectSystemMetrics()

	if err != nil {
		t.Fatalf("collectSystemMetrics() error: %v", err)
	}

	if sysMetrics.TotalCores != 4 {
		t.Errorf("collectSystemMetrics(): got %d cores, expected 4", sysMetrics.TotalCores)
	}
}

func TestIsUserLimited(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	// Initially no users should be limited
	if manager.isUserLimited(1000) {
		t.Error("isUserLimited() should return false initially")
	}

	// Add user to activeUsers
	manager.activeUsers[1000] = true

	if !manager.isUserLimited(1000) {
		t.Error("isUserLimited() should return true after adding user")
	}
}

func TestGetUsername(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	username := manager.getUsername(1000)

	// Should return UID as string if username lookup fails
	if username != "1000" {
		t.Errorf("getUsername(): got %s, expected 1000", username)
	}
}

func TestGetLoadAverage(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	load, err := manager.getLoadAverage()

	// May fail in some environments
	if err != nil {
		t.Logf("getLoadAverage() error (expected in some environments): %v", err)
	} else if load < 0 {
		t.Errorf("getLoadAverage(): got %f, expected >= 0", load)
	}
}

func TestForceActivateLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	err := manager.ForceActivateLimits()
	if err != nil {
		t.Logf("ForceActivateLimits() error: %v", err)
	}
}

func TestForceDeactivateLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	err := manager.ForceDeactivateLimits()
	if err != nil {
		t.Logf("ForceDeactivateLimits() error: %v", err)
	}
}

func TestCleanup(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &mockCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, _ := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)

	err := manager.Cleanup()
	if err != nil {
		t.Logf("Cleanup() error: %v", err)
	}
}
