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
)

func TestSystemLoadGuardUsesMeasuredCPUAttribution(t *testing.T) {
	tests := []struct {
		name                string
		totalCPUUsage       float64
		eligibleCPUUsage    float64
		wantDecision        string
		wantReasonSubstring string
		wantShare           float64
		wantMeasurable      bool
	}{
		{
			name:             "eligible users own the measured load",
			totalCPUUsage:    100,
			eligibleCPUUsage: 370,
			wantDecision:     "ACTIVATE_LIMITS",
			wantShare:        0.925,
			wantMeasurable:   true,
		},
		{
			name:                "external users own the measured load",
			totalCPUUsage:       100,
			eligibleCPUUsage:    80,
			wantDecision:        "MAINTAIN_CURRENT_STATE",
			wantReasonSubstring: "primarily external: CPU-eligible users account for 20.0%",
			wantShare:           0.2,
			wantMeasurable:      true,
		},
		{
			name:             "evenly mixed load remains actionable",
			totalCPUUsage:    100,
			eligibleCPUUsage: 200,
			wantDecision:     "ACTIVATE_LIMITS",
			wantShare:        0.5,
			wantMeasurable:   true,
		},
		{
			name:                "mixed load with external majority is suppressed",
			totalCPUUsage:       100,
			eligibleCPUUsage:    180,
			wantDecision:        "MAINTAIN_CURRENT_STATE",
			wantReasonSubstring: "primarily external: CPU-eligible users account for 45.0%",
			wantShare:           0.45,
			wantMeasurable:      true,
		},
		{
			name:                "empty host sample cannot establish attribution",
			eligibleCPUUsage:    80,
			wantDecision:        "MAINTAIN_CURRENT_STATE",
			wantReasonSubstring: "system load attribution is unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.CPUThreshold = 75
			cfg.CPUThresholdDuration = 0
			cfg.IgnoreSystemLoad = false
			cfg.MinSystemCores = 1
			manager := &Manager{
				cfg:                cfg,
				thresholdTracker:   &ThresholdTracker{},
				ioThresholdTracker: &ThresholdTracker{},
			}
			metrics := &SystemMetrics{
				TotalCores:          4,
				TotalCPUUsage:       tt.totalCPUUsage,
				CPUEligibleCPUUsage: tt.eligibleCPUUsage,
				SystemUnderLoad:     true,
			}

			decision, reason := manager.makeDecision(metrics)
			if decision != tt.wantDecision {
				t.Fatalf("decision = %s, want %s (reason: %s)", decision, tt.wantDecision, reason)
			}
			if tt.wantReasonSubstring != "" && !strings.Contains(reason, tt.wantReasonSubstring) {
				t.Fatalf("reason = %q, want substring %q", reason, tt.wantReasonSubstring)
			}

			attribution := measureCPULoadAttribution(metrics)
			if attribution.measurable != tt.wantMeasurable {
				t.Fatalf("measurable = %t, want %t", attribution.measurable, tt.wantMeasurable)
			}
			if attribution.eligibleShare != tt.wantShare {
				t.Fatalf("eligible share = %f, want %f", attribution.eligibleShare, tt.wantShare)
			}
		})
	}
}

func TestTemporaryExternalLoadSuppressionPreservesCPUThresholdProgress(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CPUThreshold = 75
	cfg.CPUThresholdDuration = 90
	cfg.IgnoreSystemLoad = false
	cfg.MinSystemCores = 1

	firstCrossing := time.Now().Add(-45 * time.Second)
	tracker := &ThresholdTracker{
		firstOverThresholdTime: firstCrossing,
		overThresholdCycles:    3,
		totalCycles:            3,
	}
	manager := &Manager{
		cfg:                cfg,
		thresholdTracker:   tracker,
		ioThresholdTracker: &ThresholdTracker{},
	}

	decision, reason := manager.makeDecision(&SystemMetrics{
		TotalCores:          4,
		TotalCPUUsage:       100,
		CPUEligibleCPUUsage: 80,
		SystemUnderLoad:     true,
	})
	if decision != "MAINTAIN_CURRENT_STATE" || !strings.Contains(reason, "primarily external") {
		t.Fatalf("decision = %s, reason = %q, want external-load suppression", decision, reason)
	}

	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	if !tracker.firstOverThresholdTime.Equal(firstCrossing) || tracker.overThresholdCycles != 3 || tracker.totalCycles != 3 {
		t.Fatalf(
			"tracker changed during temporary suppression: first=%s cycles=%d total=%d",
			tracker.firstOverThresholdTime,
			tracker.overThresholdCycles,
			tracker.totalCycles,
		)
	}
}
