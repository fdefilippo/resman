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
	"context"
	"fmt"
	"time"

	"github.com/fdefilippo/resman/cgroup"
)

func (m *Manager) hasObservedResourceEnforcementLocked() bool {
	for _, state := range m.resourceLimits {
		if state.ramApplied || state.ioApplied {
			return true
		}
	}
	return false
}

func (m *Manager) refreshResourceLimitsActiveLocked(now time.Time) {
	active := m.hasObservedResourceEnforcementLocked()
	if active && !m.resourceLimitsActive {
		m.resourceLimitsAppliedTime = now
	}
	if !active {
		m.resourceLimitsAppliedTime = time.Time{}
	}
	m.resourceLimitsActive = active
}

func (m *Manager) recordCPUTransitions(activated, deactivated bool) {
	if m.prometheusExporter == nil {
		return
	}
	if activated {
		m.prometheusExporter.IncrementCPULimitsActivated()
	}
	if deactivated {
		m.prometheusExporter.IncrementCPULimitsDeactivated()
	}
}

func (m *Manager) activateLimits(metrics *SystemMetrics) error {
	switch m.enforcementStatus.Mode {
	case cgroup.EnforcementModeSystemdNative:
		return m.activateSystemdEnforcement(metrics)
	case cgroup.EnforcementModeObservationOnly:
		m.recordObservationOnlyIntent("ACTIVATE_LIMITS", metrics)
		return nil
	default:
		return fmt.Errorf("refusing enforcement with unsupported mode %q", m.enforcementStatus.Mode)
	}
}

func (m *Manager) deactivateLimits() error {
	switch m.enforcementStatus.Mode {
	case cgroup.EnforcementModeSystemdNative:
		return m.restoreSystemdCPUPoints(context.Background())
	case cgroup.EnforcementModeObservationOnly:
		m.mu.Lock()
		m.requestedCPUUsers = make(map[int]bool)
		m.activeUsers = make(map[int]bool)
		m.userLimitedAt = make(map[int]time.Time)
		m.resourceLimits = make(map[int]userResourceLimitState)
		m.limitsActive = false
		m.resourceLimitsActive = false
		m.limitsAppliedTime = time.Time{}
		m.resourceLimitsAppliedTime = time.Time{}
		m.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("refusing deactivation with unsupported mode %q", m.enforcementStatus.Mode)
	}
}

// ForceActivateLimits executes the supported enforcement boundary immediately.
func (m *Manager) ForceActivateLimits() error {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()
	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

	metrics, err := m.collectSystemMetrics()
	if err != nil {
		return err
	}
	return m.activateLimits(metrics)
}

// ForceDeactivateLimits restores authoritative systemd properties or clears
// observation-only intent without entering a retired managed cgroup.
func (m *Manager) ForceDeactivateLimits() error {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()
	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

	err := m.deactivateLimits()
	if m.stabilityTracker == nil {
		m.stabilityTracker = newUserStabilityTracker()
	}
	m.stabilityTracker.Reset()
	return err
}
