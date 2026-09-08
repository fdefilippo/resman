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
	"errors"
	"fmt"

	"github.com/fdefilippo/resman/internal/cpupoints"
)

// CPUPointsReconciliationError identifies a bounded failed topology step.
type CPUPointsReconciliationError struct {
	Step string
	UID  int
	Err  error
}

func (e *CPUPointsReconciliationError) Error() string {
	if e.UID > 0 {
		return fmt.Sprintf("reconcile CPU Points %s for UID %d: %v", e.Step, e.UID, e.Err)
	}
	return fmt.Sprintf("reconcile CPU Points %s: %v", e.Step, e.Err)
}

func (e *CPUPointsReconciliationError) Unwrap() error { return e.Err }

// CurrentCPUPointsPolicy returns the immutable authoritative requested epoch.
func (m *Manager) CurrentCPUPointsPolicy() cpupoints.PolicySnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cpuPointsPolicy
}

// ReconcileCPUPointsPolicy verifies and safely programs a detached candidate.
// Observation-only mode has no kernel state to mutate.
func (m *Manager) ReconcileCPUPointsPolicy(candidate, fallback cpupoints.PolicySnapshot) error {
	leaveOperation := m.opGate.Enter()
	defer leaveOperation()
	return m.reconcileCPUPointsPolicyLocked(candidate, fallback)
}

// PublishCPUPointsPolicy makes a successfully reconciled candidate authoritative.
func (m *Manager) PublishCPUPointsPolicy(candidate cpupoints.PolicySnapshot) {
	m.mu.Lock()
	m.cpuPointsPolicy = candidate
	m.pendingCPUPointsPolicy = nil
	m.cpuPointsDegraded = false
	m.mu.Unlock()
}

func (m *Manager) reconcileCPUPointsPolicyLocked(candidate, fallback cpupoints.PolicySnapshot) error {
	m.mu.RLock()
	requested := m.systemdCPURequested
	m.mu.RUnlock()
	if !requested {
		m.clearCPUPointsRetryLocked()
		return nil
	}
	if err := m.reconcileSystemdCPUPoints(context.Background(), candidate); err != nil {
		return m.deferCPUPointsPolicyLocked(fallback, err)
	}
	m.clearCPUPointsRetryLocked()
	return nil
}

func (m *Manager) deferCPUPointsPolicyLocked(fallback cpupoints.PolicySnapshot, err error) error {
	m.mu.Lock()
	copy := fallback
	m.pendingCPUPointsPolicy = &copy
	m.cpuPointsDegraded = true
	m.mu.Unlock()
	if m.prometheusExporter != nil {
		m.prometheusExporter.RecordError("cpu_points_reconciliation", cpuPointsReconciliationErrorType(err))
	}
	return err
}

func cpuPointsReconciliationErrorType(err error) string {
	var reconciliationErr *CPUPointsReconciliationError
	if !errors.As(err, &reconciliationErr) {
		return "internal"
	}
	switch reconciliationErr.Step {
	case "systemd_adapter", "systemd_owned_leases", "systemd_discover", "systemd_capacity",
		"systemd_plan", "systemd_parent_quota", "systemd_leaf_weight":
		return reconciliationErr.Step
	default:
		return "internal"
	}
}

func (m *Manager) clearCPUPointsRetryLocked() {
	m.mu.Lock()
	m.pendingCPUPointsPolicy = nil
	m.cpuPointsDegraded = false
	m.mu.Unlock()
}
