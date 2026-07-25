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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/metrics"
)

// Mock implementations for testing
type mockMetricsCollector struct {
	allUserMetrics map[int]*metrics.UserMetrics
	usernames      map[int]string
	systemLoad     float64
	systemLoadErr  error
}

func (m *mockMetricsCollector) GetTotalCores() int              { return 4 }
func (m *mockMetricsCollector) GetTotalCPUUsage() float64       { return 50.0 }
func (m *mockMetricsCollector) GetUserCPUUsage(uid int) float64 { return 10.0 }
func (m *mockMetricsCollector) GetMemoryUsage() float64         { return 1024.0 }
func (m *mockMetricsCollector) GetTotalMemoryMB() float64       { return 16384.0 }
func (m *mockMetricsCollector) GetCachedMemoryMB() float64      { return 4096.0 }
func (m *mockMetricsCollector) IsSystemUnderLoad() bool         { return false }
func (m *mockMetricsCollector) GetSystemLoad() (float64, error) {
	return m.systemLoad, m.systemLoadErr
}
func (m *mockMetricsCollector) GetAllUserMetrics() map[int]*metrics.UserMetrics {
	return m.allUserMetrics
}
func (m *mockMetricsCollector) GetDBWriter() *metrics.DBWriter { return nil }
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
func (m *mockMetricsCollector) GetUsernameFromUID(uid int) string {
	if username, ok := m.usernames[uid]; ok {
		return username
	}
	return fmt.Sprintf("user%d", uid)
}

type mockCgroupManager struct{}

func (m *mockCgroupManager) CreateUserCgroup(uid int) error                            { return nil }
func (m *mockCgroupManager) ApplyCPULimit(uid int, quota string) error                 { return nil }
func (m *mockCgroupManager) ApplyCPUQuota(uid int, quota string) error                 { return nil }
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
func (m *mockCgroupManager) RemoveRAMSwapLimit(uid int) error {
	return nil
}
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
func (m *mockCgroupManager) GetUserCgroupMetrics(uid int) (string, string, uint64, uint64, uint64, uint64, uint64, error) {
	return "", "", 0, 0, 0, 0, 0, nil
}
func (m *mockCgroupManager) GetPSIStats(uid int) (cgroup.PSIStats, error) {
	return cgroup.PSIStats{}, nil
}
func (m *mockCgroupManager) ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error {
	return nil
}
func (m *mockCgroupManager) CleanupUserCgroup(uid int) error            { return nil }
func (m *mockCgroupManager) MoveProcessToCgroup(pid int, uid int) error { return nil }
func (m *mockCgroupManager) MoveAllUserProcessesToSharedCgroup(uid int, path string) error {
	return nil
}
func (m *mockCgroupManager) ReleaseUserFromSharedCgroup(uid int, path, normalQuota string) error {
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
func (m *mockPrometheusExporter) UpdateUserMetrics(uid int, user string, cpu float64, cpuAvg float64, cpuEMA float64, mem uint64, proc int, limited bool, path, quota string, memoryHighEvents uint64, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64) {
}
func (m *mockPrometheusExporter) UpdateSystemMetrics(cores int, actionCores int, load float64) {}
func (m *mockPrometheusExporter) UpdateUserWorkloadPattern(uid int, username string, pattern string, confidence float64) {
}
func (m *mockPrometheusExporter) RecordControlCycleTrigger(trigger string)   {}
func (m *mockPrometheusExporter) Start(ctx context.Context) error            { return nil }
func (m *mockPrometheusExporter) Stop() error                                { return nil }
func (m *mockPrometheusExporter) CleanupUserMetrics(activeUids map[int]bool) {}
func (m *mockPrometheusExporter) IncrementLimitsActivated()                  {}
func (m *mockPrometheusExporter) IncrementLimitsDeactivated()                {}

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
		cfg:                cfg,
		limitsActive:       false,
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
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
		cfg:                cfg,
		limitsActive:       true,
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
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
		cfg:                cfg,
		limitsActive:       false,
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
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
	metricsCollector := &mockMetricsCollector{systemLoad: 2.5}
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
	if sysMetrics.SystemLoad != 2.5 {
		t.Errorf("collectSystemMetrics(): got load %f, expected 2.5", sysMetrics.SystemLoad)
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

	// Should now return the username from the metrics collector (mock returns "user1000")
	if username != "user1000" {
		t.Errorf("getUsername(): got %s, expected user1000", username)
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

type deactivateCgroupManager struct {
	mockCgroupManager
	applyCPULimitCalls       []int
	applyCPUQuotaCalls       []string
	applyCPUWeightCalls      []int
	applyRAMLimitCalls       []string
	applyIOLimitCalls        []int
	removeRAMHighCalls       []int
	removeRAMLimitCalls      []int
	removeRAMSwapLimitCalls  []int
	removeIOLimitCalls       []int
	applySharedCPULimitCalls []string
	createdUserSubgroups     []int
	movedSharedUsers         []int
	releasedUsers            []int
	releaseErrors            map[int]error
}

type blockingReleaseCgroupManager struct {
	deactivateCgroupManager
	releaseStarted chan struct{}
	releaseProceed chan struct{}
}

type flakyResourceCgroupManager struct {
	deactivateCgroupManager
	ramFailures int
	ioFailures  int
}

func (m *flakyResourceCgroupManager) ApplyRAMLimitWithHigh(uid int, maxLimit, highLimit string) error {
	m.applyRAMLimitCalls = append(m.applyRAMLimitCalls, fmt.Sprintf("%d:%s:%s", uid, maxLimit, highLimit))
	if m.ramFailures > 0 {
		m.ramFailures--
		return errors.New("RAM apply failed")
	}
	return nil
}

func (m *flakyResourceCgroupManager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	m.applyIOLimitCalls = append(m.applyIOLimitCalls, uid)
	if m.ioFailures > 0 {
		m.ioFailures--
		return errors.New("IO apply failed")
	}
	return nil
}

func (m *blockingReleaseCgroupManager) ReleaseUserFromSharedCgroup(uid int, path, normalQuota string) error {
	close(m.releaseStarted)
	<-m.releaseProceed
	return nil
}

type cleanupLockCheckingCgroupManager struct {
	mockCgroupManager
	checkLock func() error
}

func (m *cleanupLockCheckingCgroupManager) CleanupAll() error {
	return m.checkLock()
}

func (m *deactivateCgroupManager) ApplyCPULimit(uid int, quota string) error {
	m.applyCPULimitCalls = append(m.applyCPULimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) ApplyCPUQuota(uid int, quota string) error {
	m.applyCPUQuotaCalls = append(m.applyCPUQuotaCalls, fmt.Sprintf("%d:%s", uid, quota))
	return nil
}

func (m *deactivateCgroupManager) ApplyCPUWeight(uid int, weight int) error {
	m.applyCPUWeightCalls = append(m.applyCPUWeightCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) ApplyRAMLimitWithHigh(uid int, maxLimit, highLimit string) error {
	m.applyRAMLimitCalls = append(m.applyRAMLimitCalls, fmt.Sprintf("%d:%s:%s", uid, maxLimit, highLimit))
	return nil
}

func (m *deactivateCgroupManager) ApplyRAMLimitWithHighAndSwapDisabled(uid int, maxLimit, highLimit string) error {
	m.applyRAMLimitCalls = append(m.applyRAMLimitCalls, fmt.Sprintf("%d:%s:%s:swap-off", uid, maxLimit, highLimit))
	return nil
}

func (m *deactivateCgroupManager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	m.applyIOLimitCalls = append(m.applyIOLimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) RemoveRAMHigh(uid int) error {
	m.removeRAMHighCalls = append(m.removeRAMHighCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) RemoveRAMLimit(uid int) error {
	m.removeRAMLimitCalls = append(m.removeRAMLimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) RemoveRAMSwapLimit(uid int) error {
	m.removeRAMSwapLimitCalls = append(m.removeRAMSwapLimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) RemoveIOLimit(uid int) error {
	m.removeIOLimitCalls = append(m.removeIOLimitCalls, uid)
	return nil
}

func (m *deactivateCgroupManager) ApplySharedCPULimit(path string, quota string) error {
	m.applySharedCPULimitCalls = append(m.applySharedCPULimitCalls, fmt.Sprintf("%s:%s", path, quota))
	return nil
}

func (m *deactivateCgroupManager) CreateUserSubCgroup(uid int, path string) (string, error) {
	m.createdUserSubgroups = append(m.createdUserSubgroups, uid)
	return filepath.Join(path, fmt.Sprintf("user_%d", uid)), nil
}

func (m *deactivateCgroupManager) MoveAllUserProcessesToSharedCgroup(uid int, path string) error {
	m.movedSharedUsers = append(m.movedSharedUsers, uid)
	return nil
}

func (m *deactivateCgroupManager) ReleaseUserFromSharedCgroup(uid int, path, normalQuota string) error {
	m.releasedUsers = append(m.releasedUsers, uid)
	return m.releaseErrors[uid]
}

func TestDeactivateLimitsReleasesSharedCgroups(t *testing.T) {
	cfg := config.DefaultConfig()
	metricsCollector := &mockMetricsCollector{}
	cgroupManager := &deactivateCgroupManager{}
	prometheusExporter := &mockPrometheusExporter{}

	manager, err := NewManager(cfg, metricsCollector, cgroupManager, prometheusExporter)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("failed to create shared cgroup test path: %v", err)
	}

	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true
	manager.activeUsers[1001] = true
	manager.psiBoostedAt[1000] = time.Now().Add(-time.Hour)
	manager.psiBoostedAt[1001] = time.Now().Add(-time.Hour)

	if err := manager.deactivateLimits(); err != nil {
		t.Fatalf("deactivateLimits() error: %v", err)
	}

	if len(cgroupManager.applyCPULimitCalls) != 0 {
		t.Fatalf("ApplyCPULimit should not be used for shared cgroup deactivation, got calls for %v", cgroupManager.applyCPULimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.applySharedCPULimitCalls, []string{sharedPath + ":max 100000"}) {
		t.Fatalf("ApplySharedCPULimit calls = %v", cgroupManager.applySharedCPULimitCalls)
	}

	sort.Ints(cgroupManager.releasedUsers)
	if !reflect.DeepEqual(cgroupManager.releasedUsers, []int{1000, 1001}) {
		t.Fatalf("released users = %v, want [1000 1001]", cgroupManager.releasedUsers)
	}
	if manager.limitsActive {
		t.Fatal("limitsActive should be false after all shared users are released")
	}
	if manager.sharedCgroupPath != "" {
		t.Fatalf("sharedCgroupPath = %q, want empty", manager.sharedCgroupPath)
	}
	if len(manager.activeUsers) != 0 {
		t.Fatalf("activeUsers = %v, want empty", manager.activeUsers)
	}
	if len(manager.psiBoostedAt) != 0 {
		t.Fatalf("psiBoostedAt = %v, want empty", manager.psiBoostedAt)
	}

	manager.revertPSIBoosts()
	if len(cgroupManager.applyCPUWeightCalls) != 0 {
		t.Fatalf("expired PSI boosts attempted after deactivation: %v", cgroupManager.applyCPUWeightCalls)
	}
}

func TestDeactivateLimitsKeepsFailedSharedUsersActive(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &deactivateCgroupManager{
		releaseErrors: map[int]error{
			1001: errors.New("restore failed"),
		},
	}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("failed to create shared cgroup test path: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true
	manager.activeUsers[1001] = true
	manager.psiBoostedAt[1000] = time.Now()
	manager.psiBoostedAt[1001] = time.Now()

	if err := manager.deactivateLimits(); err == nil {
		t.Fatal("deactivateLimits() should report the failed user release")
	}
	if manager.activeUsers[1000] {
		t.Fatal("successfully restored user 1000 should be removed from activeUsers")
	}
	if !manager.activeUsers[1001] {
		t.Fatal("failed user 1001 should remain in activeUsers")
	}
	if !manager.limitsActive {
		t.Fatal("limitsActive should remain true while a user still needs release")
	}
	if manager.sharedCgroupPath != sharedPath {
		t.Fatalf("sharedCgroupPath = %q, want %q", manager.sharedCgroupPath, sharedPath)
	}
	if _, exists := manager.psiBoostedAt[1000]; exists {
		t.Fatal("PSI boost state for released user 1000 should be removed")
	}
	if _, exists := manager.psiBoostedAt[1001]; !exists {
		t.Fatal("PSI boost state for failed user 1001 should be retained")
	}

	cgroupManager.applySharedCPULimitCalls = nil
	metrics := &SystemMetrics{
		TotalCores: 4,
		UserCPUUsage: map[int]float64{
			1001: 10,
		},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}
	wantQuota := sharedPath + ":300000 100000"
	if !reflect.DeepEqual(cgroupManager.applySharedCPULimitCalls, []string{wantQuota}) {
		t.Fatalf("shared quota reconciliation calls = %v, want [%s]", cgroupManager.applySharedCPULimitCalls, wantQuota)
	}
}

func TestDeactivateLimitsRemovesResourcesAppliedBeforeReload(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.DisableSwap = true
	cfg.IOEnabled = true

	collector := &mockMetricsCollector{usernames: map[int]string{1000: "user1000"}}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("MkdirAll(%s) error: %v", sharedPath, err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true
	manager.applyUserResourceLimits(1000, cfg)

	manager.UpdateConfig(config.DefaultConfig())
	if err := manager.deactivateLimits(); err != nil {
		t.Fatalf("deactivateLimits() error: %v", err)
	}

	if !reflect.DeepEqual(cgroupManager.removeRAMHighCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeRAMLimitCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeRAMSwapLimitCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeIOLimitCalls, []int{1000}) {
		t.Fatalf("tracked resource cleanup calls high=%v max=%v swap=%v io=%v, want [1000] each",
			cgroupManager.removeRAMHighCalls,
			cgroupManager.removeRAMLimitCalls,
			cgroupManager.removeRAMSwapLimitCalls,
			cgroupManager.removeIOLimitCalls,
		)
	}
}

func TestReleaseIdleUsersReappliesRAMAndIOLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.IOEnabled = true

	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	metrics := &SystemMetrics{
		TotalCores:    4,
		UserCPUUsage:  map[int]float64{1000: 10},
		EligibleUsers: []int{1000},
	}

	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if !reflect.DeepEqual(cgroupManager.createdUserSubgroups, []int{1000}) {
		t.Fatalf("created subgroups = %v, want [1000]", cgroupManager.createdUserSubgroups)
	}
	if !reflect.DeepEqual(cgroupManager.movedSharedUsers, []int{1000}) {
		t.Fatalf("moved users = %v, want [1000]", cgroupManager.movedSharedUsers)
	}
	if !reflect.DeepEqual(cgroupManager.applyRAMLimitCalls, []string{"1000:1G:858993459"}) {
		t.Fatalf("RAM limit calls = %v", cgroupManager.applyRAMLimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.applyIOLimitCalls, []int{1000}) {
		t.Fatalf("IO limit calls = %v, want [1000]", cgroupManager.applyIOLimitCalls)
	}
	if !manager.activeUsers[1000] {
		t.Fatal("re-added user 1000 should be active")
	}
}

func TestReleaseIdleUsersReconcilesResourceLimitsAfterReload(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.DisableSwap = true
	cfg.IOEnabled = true

	collector := &mockMetricsCollector{usernames: map[int]string{1000: "user1000"}}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	manager.userLimitedAt[1000] = time.Now().Add(-time.Minute)
	manager.applyUserResourceLimits(1000, cfg)

	reloaded := config.DefaultConfig()
	manager.UpdateConfig(reloaded)
	metrics := &SystemMetrics{
		TotalCores:    4,
		UserCPUUsage:  map[int]float64{1000: 10},
		UserMetrics:   map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 10}},
		EligibleUsers: []int{1000},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if !reflect.DeepEqual(cgroupManager.removeRAMHighCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeRAMLimitCalls, []int{1000}) {
		t.Fatalf("RAM removal calls high=%v max=%v, want [1000] for both",
			cgroupManager.removeRAMHighCalls, cgroupManager.removeRAMLimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.removeIOLimitCalls, []int{1000}) {
		t.Fatalf("IO removal calls = %v, want [1000]", cgroupManager.removeIOLimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.removeRAMSwapLimitCalls, []int{1000}) {
		t.Fatalf("RAM swap removal calls = %v, want [1000]", cgroupManager.removeRAMSwapLimitCalls)
	}
	if !manager.activeUsers[1000] {
		t.Fatal("resource-only reload removed the user from CPU limiting")
	}
	if _, exists := manager.resourceLimits[1000]; exists {
		t.Fatal("resource limit tracking remained after successful reconciliation")
	}
}

func TestReleaseIdleUsersRetriesPartialResourceLimitApplication(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.IOEnabled = true

	collector := &mockMetricsCollector{usernames: map[int]string{1000: "user1000"}}
	cgroupManager := &flakyResourceCgroupManager{ramFailures: 1, ioFailures: 1}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	manager.userLimitedAt[1000] = time.Now()
	manager.applyUserResourceLimits(1000, cfg)

	initialState := manager.resourceLimits[1000]
	if !initialState.ram || initialState.ramApplied || !initialState.io || initialState.ioApplied {
		t.Fatalf("partial resource state = %+v, want tracked but not fully applied", initialState)
	}

	systemMetrics := &SystemMetrics{
		TotalCores:    4,
		UserCPUUsage:  map[int]float64{1000: 10},
		UserMetrics:   map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 10}},
		EligibleUsers: []int{1000},
	}
	if err := manager.releaseIdleUsers(systemMetrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if len(cgroupManager.applyRAMLimitCalls) != 2 || len(cgroupManager.applyIOLimitCalls) != 2 {
		t.Fatalf("resource apply attempts RAM=%d IO=%d, want two each",
			len(cgroupManager.applyRAMLimitCalls), len(cgroupManager.applyIOLimitCalls))
	}
	finalState := manager.resourceLimits[1000]
	if !finalState.ramApplied || !finalState.ioApplied {
		t.Fatalf("resource state after retry = %+v, want fully applied", finalState)
	}
}

func TestReleaseIdleUsersReleasesNewlyIneligibleUser(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RAMEnabled = true
	cfg.RAMQuotaPerUser = "1G"
	cfg.IOEnabled = true

	collector := &mockMetricsCollector{usernames: map[int]string{1000: "user1000"}}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	manager.userLimitedAt[1000] = time.Now()
	manager.applyUserResourceLimits(1000, cfg)

	metrics := &SystemMetrics{
		TotalCores:   4,
		UserCPUUsage: map[int]float64{1000: 10},
		UserMetrics:  map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 10}},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if manager.activeUsers[1000] {
		t.Fatal("newly ineligible user remained in activeUsers")
	}
	if !reflect.DeepEqual(cgroupManager.releasedUsers, []int{1000}) {
		t.Fatalf("released users = %v, want [1000]", cgroupManager.releasedUsers)
	}
	if !reflect.DeepEqual(cgroupManager.removeRAMHighCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeRAMLimitCalls, []int{1000}) ||
		!reflect.DeepEqual(cgroupManager.removeIOLimitCalls, []int{1000}) {
		t.Fatalf("tracked resource limits were not removed: high=%v max=%v io=%v",
			cgroupManager.removeRAMHighCalls,
			cgroupManager.removeRAMLimitCalls,
			cgroupManager.removeIOLimitCalls,
		)
	}
}

func TestReleaseIdleUsersUsesEMAAndPerUserHoldTime(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MinActiveTime = 60

	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	manager.userLimitedAt[1000] = time.Now().Add(-2 * time.Minute)

	metrics := &SystemMetrics{
		TotalCores:    4,
		UserCPUUsage:  map[int]float64{1000: 0},
		UserMetrics:   map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 5}},
		EligibleUsers: []int{1000},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers(high EMA) error: %v", err)
	}
	if !manager.activeUsers[1000] {
		t.Fatal("instantaneous idle sample released a user whose EMA was active")
	}

	metrics.UserMetrics[1000].CPUUsageEMA = 0
	manager.userLimitedAt[1000] = time.Now()
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers(hold time) error: %v", err)
	}
	if !manager.activeUsers[1000] {
		t.Fatal("user was released before its per-user minimum active time")
	}

	manager.userLimitedAt[1000] = time.Now().Add(-2 * time.Minute)
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers(expired hold) error: %v", err)
	}
	if manager.activeUsers[1000] {
		t.Fatal("idle user remained limited after EMA and hold-time conditions were met")
	}
}

func TestReleaseIdleUsersReAddDoesNotResetGlobalActivationTime(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	globalActivation := time.Now().Add(-10 * time.Minute)
	manager.limitsActive = true
	manager.limitsAppliedTime = globalActivation
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")

	metrics := &SystemMetrics{
		TotalCores:    4,
		UserCPUUsage:  map[int]float64{1000: 10},
		UserMetrics:   map[int]*metrics.UserMetrics{1000: {UID: 1000, CPUUsageEMA: 10}},
		EligibleUsers: []int{1000},
	}
	if err := manager.releaseIdleUsers(metrics); err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	if !manager.activeUsers[1000] {
		t.Fatal("active eligible user was not re-added")
	}
	if !manager.limitsAppliedTime.Equal(globalActivation) {
		t.Fatalf("global limitsAppliedTime changed from %s to %s",
			globalActivation, manager.limitsAppliedTime)
	}
	if manager.userLimitedAt[1000].Before(globalActivation) {
		t.Fatalf("per-user activation time = %s, want re-add time", manager.userLimitedAt[1000])
	}
}

func TestReleaseIdleUsersForgetsIORemediationOnlyAfterSuccessfulRelease(t *testing.T) {
	tests := []struct {
		name       string
		releaseErr error
		wantState  bool
		wantActive bool
	}{
		{
			name:      "successful release",
			wantState: false,
		},
		{
			name:       "failed release",
			releaseErr: errors.New("restore failed"),
			wantState:  true,
			wantActive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cgroupManager := &deactivateCgroupManager{
				releaseErrors: map[int]error{1000: tt.releaseErr},
			}
			manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}

			manager.limitsActive = true
			manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
			manager.activeUsers[1000] = true
			manager.psiBoostedAt[1000] = time.Now()
			manager.ioRemediation.boostStates[1000] = &IOBoostState{IsActive: true}

			metrics := &SystemMetrics{
				TotalCores:   4,
				UserCPUUsage: map[int]float64{1000: 0},
			}
			if err := manager.releaseIdleUsers(metrics); err != nil {
				t.Fatalf("releaseIdleUsers() error: %v", err)
			}

			manager.ioRemediation.mu.RLock()
			_, stateExists := manager.ioRemediation.boostStates[1000]
			manager.ioRemediation.mu.RUnlock()
			if stateExists != tt.wantState {
				t.Fatalf("IO remediation state exists = %t, want %t", stateExists, tt.wantState)
			}
			manager.mu.RLock()
			active := manager.activeUsers[1000]
			manager.mu.RUnlock()
			if active != tt.wantActive {
				t.Fatalf("activeUsers[1000] = %t, want %t", active, tt.wantActive)
			}
			manager.mu.RLock()
			_, psiStateExists := manager.psiBoostedAt[1000]
			manager.mu.RUnlock()
			if psiStateExists != tt.wantActive {
				t.Fatalf("PSI boost state exists = %t, want %t", psiStateExists, tt.wantActive)
			}
		})
	}
}

func TestReleaseIdleUsersWaitsBeforeCommittingState(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &blockingReleaseCgroupManager{
		releaseStarted: make(chan struct{}),
		releaseProceed: make(chan struct{}),
	}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	manager.limitsActive = true
	manager.sharedCgroupPath = filepath.Join(t.TempDir(), "limited")
	manager.activeUsers[1000] = true
	metrics := &SystemMetrics{
		TotalCores:   4,
		UserCPUUsage: map[int]float64{1000: 0},
	}

	releaseDone := make(chan error, 1)
	go func() {
		releaseDone <- manager.releaseIdleUsers(metrics)
	}()
	<-cgroupManager.releaseStarted

	manager.mu.RLock()
	activeWhilePending := manager.activeUsers[1000]
	manager.mu.RUnlock()
	if !activeWhilePending {
		t.Fatal("user was removed from activeUsers before cgroup release completed")
	}
	select {
	case err := <-releaseDone:
		t.Fatalf("releaseIdleUsers() returned before cgroup release completed: %v", err)
	default:
	}

	close(cgroupManager.releaseProceed)
	if err := <-releaseDone; err != nil {
		t.Fatalf("releaseIdleUsers() error: %v", err)
	}

	manager.mu.RLock()
	activeAfterRelease := manager.activeUsers[1000]
	manager.mu.RUnlock()
	if activeAfterRelease {
		t.Fatal("successfully released user remained in activeUsers")
	}
}

func TestCleanupSerializesCgroupOperations(t *testing.T) {
	cfg := config.DefaultConfig()
	cgroupManager := &cleanupLockCheckingCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	cgroupManager.checkLock = func() error {
		if manager.opMu.TryLock() {
			manager.opMu.Unlock()
			return errors.New("CleanupAll called without opMu")
		}
		return nil
	}

	if err := manager.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}
}

func TestPatternDetectionFiltersUsersAndKeepsSharedProcessesInPlace(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternMinSamples = 1
	cfg.PatternConfidenceThreshold = 0.7
	cfg.UserIncludeList = []string{"^allowed$"}

	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "allowed", CPUUsage: 0, IsLimited: false},
			1001: {UID: 1001, Username: "excluded", CPUUsage: 0, IsLimited: false},
		},
		usernames: map[int]string{1000: "allowed", 1001: "excluded"},
	}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.patternDetector.userStats[1000] = batchNightStats()
	manager.patternDetector.userStats[1001] = batchNightStats()

	run := &controlCycleContext{cfg: cfg}
	if err := manager.stageWorkloadPatternDetection(run); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.policyEngine.GetPolicy(1000); !exists {
		t.Fatal("eligible user 1000 did not receive a pattern policy")
	}
	if _, exists := manager.policyEngine.GetPolicy(1001); exists {
		t.Fatal("excluded user 1001 received a pattern policy")
	}
	if _, exists := manager.patternDetector.userStats[1001]; exists {
		t.Fatal("excluded user 1001 remained in pattern statistics")
	}
	if len(cgroupManager.applyCPULimitCalls) != 0 {
		t.Fatalf("pattern policy migrated processes through ApplyCPULimit: %v", cgroupManager.applyCPULimitCalls)
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUQuotaCalls, []string{"1000:200000 100000"}) {
		t.Fatalf("CPU quota calls = %v, want [1000:200000 100000]", cgroupManager.applyCPUQuotaCalls)
	}
}

func TestPatternHistorySurvivesTemporaryProcessAbsence(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternMinSamples = 1
	cfg.UserIncludeList = []string{"^batch$"}

	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{},
		usernames:      map[int]string{1000: "batch"},
	}
	manager, err := NewManager(cfg, collector, &deactivateCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	stats := batchNightStats()
	manager.patternDetector.userStats[1000] = stats
	manager.policyEngine.ApplyPolicy(1000, PatternBatchNight, cfg)

	if err := manager.stageWorkloadPatternDetection(&controlCycleContext{cfg: cfg}); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	retained, exists := manager.patternDetector.userStats[1000]
	if !exists {
		t.Fatal("pattern history was removed while the configured user had no live processes")
	}
	if len(retained.Buckets) != 1 || retained.Buckets[0].SampleCount != 30 {
		t.Fatalf("retained buckets = %+v, want one bucket with 30 samples", retained.Buckets)
	}
	if _, exists := manager.policyEngine.GetPolicy(1000); !exists {
		t.Fatal("pattern policy was removed while retained history was still valid")
	}
}

func TestPatternHistoryExpiryRemovesPolicy(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternHistoryHours = 1
	cfg.UserIncludeList = []string{"^batch$"}

	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{},
		usernames:      map[int]string{1000: "batch"},
	}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	stats := batchNightStats()
	stats.Buckets[0].Hour = patternHourStart(time.Now().Add(-2 * time.Hour))
	manager.patternDetector.userStats[1000] = stats
	manager.policyEngine.ApplyPolicy(1000, PatternBatchNight, cfg)
	manager.activeUsers[1000] = true

	if err := manager.stageWorkloadPatternDetection(&controlCycleContext{cfg: cfg}); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.patternDetector.userStats[1000]; exists {
		t.Fatal("expired pattern history was retained")
	}
	if _, exists := manager.policyEngine.GetPolicy(1000); exists {
		t.Fatal("policy survived after its pattern history expired")
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUQuotaCalls, []string{"1000:max 100000"}) {
		t.Fatalf("CPU quota calls = %v, want [1000:max 100000]", cgroupManager.applyCPUQuotaCalls)
	}
}

func TestPatternPolicyIsRevertedWhenClassificationDecays(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = true
	cfg.PatternMinSamples = 1

	collector := &mockMetricsCollector{
		allUserMetrics: map[int]*metrics.UserMetrics{
			1000: {UID: 1000, Username: "user1000", IsLimited: true},
		},
	}
	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, collector, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.policyEngine.ApplyPolicy(1000, PatternBatchNight, cfg)
	noon := mostRecentHour(12)
	manager.patternDetector.userStats[1000] = &UserHourlyStats{Buckets: []hourlyPatternBucket{{
		Hour:        noon,
		CPUSum:      0,
		SampleCount: 30,
	}}}

	if err := manager.stageWorkloadPatternDetection(&controlCycleContext{cfg: cfg}); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.policyEngine.GetPolicy(1000); exists {
		t.Fatal("unknown classification did not remove the existing policy")
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUQuotaCalls, []string{"1000:max 100000"}) {
		t.Fatalf("CPU quota calls = %v, want [1000:max 100000]", cgroupManager.applyCPUQuotaCalls)
	}
}

func TestPatternPolicyIsRevertedWhenAutodetectIsDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutodetectPatterns = false

	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.policyEngine.ApplyPolicy(1000, PatternBatchNight, cfg)

	if err := manager.stageWorkloadPatternDetection(&controlCycleContext{cfg: cfg}); err != nil {
		t.Fatalf("stageWorkloadPatternDetection() error: %v", err)
	}

	if _, exists := manager.policyEngine.GetPolicy(1000); exists {
		t.Fatal("disabled autodetect did not clear the existing policy")
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUQuotaCalls, []string{"1000:max 100000"}) {
		t.Fatalf("CPU quota calls = %v, want [1000:max 100000]", cgroupManager.applyCPUQuotaCalls)
	}
}

func TestIORemediationUsesOnlyActiveIOEligibleUsers(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IOEnabled = true
	cfg.IORemediationEnabled = true
	cfg.IOStarvationCheckInterval = 0
	cfg.IOUserIncludeList = []string{"^allowed$"}

	collector := &mockMetricsCollector{
		usernames: map[int]string{
			1000: "allowed",
			1001: "excluded",
		},
	}
	manager, err := NewManager(cfg, collector, &deactivateCgroupManager{}, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	manager.activeUsers[1000] = true
	manager.activeUsers[1001] = true

	if err := manager.stageIORemediation(&controlCycleContext{cfg: cfg}); err != nil {
		t.Fatalf("stageIORemediation() error: %v", err)
	}

	if _, exists := manager.ioRemediation.boostStates[1000]; !exists {
		t.Fatal("active IO-eligible user was not checked")
	}
	if _, exists := manager.ioRemediation.boostStates[1001]; exists {
		t.Fatal("IO-excluded user was checked for remediation")
	}
}

func batchNightStats() *UserHourlyStats {
	return &UserHourlyStats{Buckets: []hourlyPatternBucket{{
		Hour:        mostRecentHour(23),
		CPUSum:      3000,
		SampleCount: 30,
	}}}
}

func mostRecentHour(hour int) time.Time {
	now := time.Now()
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if candidate.After(now) {
		candidate = candidate.Add(-24 * time.Hour)
	}
	return candidate
}

func TestBlackoutDeactivatesLimitsAndResetsBoosts(t *testing.T) {
	cfg := config.DefaultConfig()
	timeframes, err := config.ParseTimeframe("* 00-24")
	if err != nil {
		t.Fatalf("ParseTimeframe() error: %v", err)
	}
	cfg.BlackoutSpec = "* 00-24"
	cfg.BlackoutTimeframes = timeframes

	cgroupManager := &deactivateCgroupManager{}
	manager, err := NewManager(cfg, &mockMetricsCollector{}, cgroupManager, &mockPrometheusExporter{})
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	sharedPath := filepath.Join(t.TempDir(), "limited")
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		t.Fatalf("failed to create shared cgroup test path: %v", err)
	}
	manager.limitsActive = true
	manager.sharedCgroupPath = sharedPath
	manager.activeUsers[1000] = true
	manager.psiBoostedAt[1000] = time.Now()
	manager.ioRemediation.boostStates[1000] = &IOBoostState{
		IsActive:        true,
		StartTime:       time.Now(),
		StarvationStart: time.Now(),
	}

	run := &controlCycleContext{
		cfg:     cfg,
		cycleID: 123,
		trigger: ControlCycleTriggerTicker,
	}
	if err := manager.stageCheckBlackout(run); err != nil {
		t.Fatalf("stageCheckBlackout() error: %v", err)
	}

	if !run.stopWithoutError {
		t.Fatal("blackout did not suspend the control cycle")
	}
	if manager.limitsActive || len(manager.activeUsers) != 0 {
		t.Fatalf("limits remained active during blackout: active=%v users=%v", manager.limitsActive, manager.activeUsers)
	}
	if !reflect.DeepEqual(cgroupManager.applyCPUWeightCalls, []int{1000}) {
		t.Fatalf("ApplyCPUWeight calls = %v, want [1000]", cgroupManager.applyCPUWeightCalls)
	}
	if !reflect.DeepEqual(cgroupManager.releasedUsers, []int{1000}) {
		t.Fatalf("released users = %v, want [1000]", cgroupManager.releasedUsers)
	}
	if len(manager.psiBoostedAt) != 0 {
		t.Fatalf("PSI boost state remained after blackout: %v", manager.psiBoostedAt)
	}
	if _, exists := manager.ioRemediation.boostStates[1000]; exists {
		t.Fatal("IO boost state remained after blackout released the user cgroup")
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
