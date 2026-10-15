/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package state

import (
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/metrics"
)

// TestCPUThresholdsArePercentOfHost pins resman-fz0: CPU_THRESHOLD and
// CPU_RELEASE_THRESHOLD are percentages of the host, while the eligible sum is
// an aggregate per-core percentage. The 16-CPU rows reproduce vmgclalpr2400,
// where a per-core comparison activated at 390.1 >= 75 with the host at 24%
// and then held limits for hours because 45 users never fell below 0.4 CPU.
func TestCPUThresholdsArePercentOfHost(t *testing.T) {
	tests := []struct {
		name             string
		cores            int
		eligibleCPU      float64
		userEMA          float64
		active           bool
		wantDecision     string
		wantReasonSignal string
	}{
		{
			name:             "production activation sample stays below 75% of the host",
			cores:            16,
			eligibleCPU:      390.1,
			wantDecision:     "MAINTAIN_CURRENT_STATE",
			wantReasonSignal: "All resources within normal range",
		},
		{
			name:             "activation threshold reached exactly",
			cores:            16,
			eligibleCPU:      1200,
			wantDecision:     "ACTIVATE_LIMITS",
			wantReasonSignal: "CPU 75.0% of host >= 75%",
		},
		{
			name:         "just below the activation threshold",
			cores:        16,
			eligibleCPU:  1199,
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
		{
			name:             "single CPU host keeps the identity",
			cores:            1,
			eligibleCPU:      75,
			wantDecision:     "ACTIVATE_LIMITS",
			wantReasonSignal: "CPU 75.0% of host >= 75%",
		},
		{
			name:         "zero eligible usage",
			cores:        16,
			eligibleCPU:  0,
			wantDecision: "MAINTAIN_CURRENT_STATE",
		},
		{
			name:             "unknown core count cannot activate on CPU",
			cores:            0,
			eligibleCPU:      1600,
			wantDecision:     "MAINTAIN_CURRENT_STATE",
			wantReasonSignal: "online CPU count is unavailable",
		},
		{
			name:             "production limited sample releases at 10.6% of the host",
			cores:            16,
			eligibleCPU:      168.9,
			userEMA:          30,
			active:           true,
			wantDecision:     "DEACTIVATE_LIMITS",
			wantReasonSignal: "CPU 10.6% of host < 40%",
		},
		{
			name:             "release threshold reached exactly holds limits",
			cores:            16,
			eligibleCPU:      640,
			active:           true,
			wantDecision:     "MAINTAIN_CURRENT_STATE",
			wantReasonSignal: "still above release threshold",
		},
		{
			name:             "per-user EMA compared in host percent",
			cores:            16,
			eligibleCPU:      600,
			userEMA:          600, // 37.5% of the host, below 40%
			active:           true,
			wantDecision:     "DEACTIVATE_LIMITS",
			wantReasonSignal: "CPU 37.5% of host < 40%",
		},
		{
			name:             "per-user EMA at the host release threshold holds limits",
			cores:            16,
			eligibleCPU:      600,
			userEMA:          640,
			active:           true,
			wantDecision:     "MAINTAIN_CURRENT_STATE",
			wantReasonSignal: "waiting for stability",
		},
		{
			name:             "unknown core count cannot release on CPU",
			cores:            0,
			eligibleCPU:      0,
			active:           true,
			wantDecision:     "MAINTAIN_CURRENT_STATE",
			wantReasonSignal: "online CPU count is unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.CPUThreshold = 75
			cfg.CPUReleaseThreshold = 40
			cfg.CPUThresholdDuration = 0
			cfg.MinActiveTime = 0
			cfg.PollingInterval = 30
			cfg.IgnoreSystemLoad = true
			userMetrics := map[int]*metrics.UserMetrics{
				1000: {UID: 1000, EnforceableUsage: metrics.ProcessSetMetrics{CPUUsageEMA: tt.userEMA}},
			}
			tracker := newUserStabilityTracker()
			// An already elapsed cool-down isolates the threshold comparison.
			tracker.belowThresholdSince[1000] = time.Now().Add(-time.Hour)
			manager := &Manager{
				cfg:                cfg,
				limitsActive:       tt.active,
				limitsAppliedTime:  time.Now().Add(-time.Hour),
				activeUsers:        map[int]bool{1000: tt.active},
				thresholdTracker:   &ThresholdTracker{},
				ioThresholdTracker: &ThresholdTracker{},
				stabilityTracker:   tracker,
			}
			if !tt.active {
				manager.activeUsers = map[int]bool{}
			}

			decision, reason := manager.makeDecision(&SystemMetrics{
				TotalCores:          tt.cores,
				CPUEligibleCPUUsage: tt.eligibleCPU,
				UserMetrics:         userMetrics,
			})
			if decision != tt.wantDecision {
				t.Fatalf("makeDecision() = %s (%s), want %s", decision, reason, tt.wantDecision)
			}
			if tt.wantReasonSignal != "" && !strings.Contains(reason, tt.wantReasonSignal) {
				t.Fatalf("reason %q does not contain %q", reason, tt.wantReasonSignal)
			}
		})
	}
}

// TestCPUThresholdDurationTracksHostPercent proves that the activation duration
// guard is fed the host percentage, not the per-core sum: a sum far above the
// threshold number but below it as a host share never starts the guard.
func TestCPUThresholdDurationTracksHostPercent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 75
	cfg.CPUReleaseThreshold = 40
	cfg.CPUThresholdDuration = 90
	cfg.IgnoreSystemLoad = true
	manager := &Manager{
		cfg:                cfg,
		thresholdTracker:   &ThresholdTracker{},
		ioThresholdTracker: &ThresholdTracker{},
	}

	decision, reason := manager.makeDecision(&SystemMetrics{TotalCores: 16, CPUEligibleCPUUsage: 779.2})
	if decision != "MAINTAIN_CURRENT_STATE" || strings.Contains(reason, "waiting") {
		t.Fatalf("makeDecision() = %s (%s), want maintenance without a duration guard", decision, reason)
	}
	if !manager.thresholdTracker.firstOverThresholdTime.IsZero() {
		t.Fatal("duration guard started for 48.7% of the host against a 75% threshold")
	}

	decision, reason = manager.makeDecision(&SystemMetrics{TotalCores: 16, CPUEligibleCPUUsage: 1280})
	if decision != "MAINTAIN_CURRENT_STATE" || !strings.Contains(reason, "(CPU 80.0% of host >= 75%)") {
		t.Fatalf("makeDecision() = %s (%s), want the duration guard on 80%% of the host", decision, reason)
	}
}

// TestCollectedCPUSampleIsComparedAsHostPercent crosses the boundary the unit
// conversion sits on: per-user samples from the collector are summed by
// collectSystemMetricsForPurpose and judged by makeDecision against the host
// threshold on the collector's core count (4 in the mock).
func TestCollectedCPUSampleIsComparedAsHostPercent(t *testing.T) {
	tests := []struct {
		name         string
		aliceCPU     float64
		bobCPU       float64
		wantDecision string
	}{
		{name: "one and a half CPUs is 37.5% of the host", aliceCPU: 100, bobCPU: 50, wantDecision: "MAINTAIN_CURRENT_STATE"},
		{name: "three CPUs is 75% of the host", aliceCPU: 200, bobCPU: 100, wantDecision: "ACTIVATE_LIMITS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.UserIncludeList = []string{"^alice$", "^bob$"}
			cfg.CPUThreshold = 75
			cfg.CPUReleaseThreshold = 40
			cfg.CPUThresholdDuration = 0
			cfg.IgnoreSystemLoad = true
			collector := &mockMetricsCollector{allUserMetrics: map[int]*metrics.UserMetrics{
				1000: {UID: 1000, Username: "alice", CPUUsage: tt.aliceCPU, ProcessCount: 1},
				1001: {UID: 1001, Username: "bob", CPUUsage: tt.bobCPU, ProcessCount: 1},
			}}
			manager, err := NewManager(cfg, collector, &mockCgroupManager{}, &mockPrometheusExporter{})
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}

			sample, err := manager.collectSystemMetricsForPurpose(true)
			if err != nil {
				t.Fatalf("collectSystemMetricsForPurpose() error: %v", err)
			}
			if want := tt.aliceCPU + tt.bobCPU; sample.CPUEligibleCPUUsage != want {
				t.Fatalf("eligible per-core sum = %.1f, want %.1f", sample.CPUEligibleCPUUsage, want)
			}
			if decision, reason := manager.makeDecision(sample); decision != tt.wantDecision {
				t.Fatalf("makeDecision() = %s (%s), want %s", decision, reason, tt.wantDecision)
			}
		})
	}
}

func TestEligibleHostCPUPercentLogValue(t *testing.T) {
	tests := []struct {
		name    string
		metrics *SystemMetrics
		want    interface{}
	}{
		{name: "nil sample", metrics: nil, want: "unavailable"},
		{name: "unknown core count", metrics: &SystemMetrics{CPUEligibleCPUUsage: 390.1}, want: "unavailable"},
		{name: "zero usage is measured", metrics: &SystemMetrics{TotalCores: 16}, want: 0.0},
		{name: "per-core sum normalized", metrics: &SystemMetrics{TotalCores: 16, CPUEligibleCPUUsage: 800}, want: 50.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eligibleHostCPUPercentLogValue(tt.metrics); got != tt.want {
				t.Fatalf("eligibleHostCPUPercentLogValue() = %v, want %v", got, tt.want)
			}
		})
	}
}
