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
	"errors"
	"fmt"
	"sort"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/internal/cpupoints"
)

// CPUPointsClassChangeError reports active UIDs whose allocation class would
// change. Reload cannot move those leaves without violating the RAM-charge
// and process-origin contracts.
type CPUPointsClassChangeError struct {
	UIDs []int
}

func (e *CPUPointsClassChangeError) Error() string {
	return fmt.Sprintf("CPU Points policy changes the active allocation class for UIDs %v; release those users before retrying reload", e.UIDs)
}

// CPUPointsPreflight marks a rejection that performed no kernel mutation.
func (e *CPUPointsClassChangeError) CPUPointsPreflight() {}

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

// ValidateCPUPointsPolicyTransition runs the active-allocation preflight
// without mutating cgroups or publishing the candidate.
func (m *Manager) ValidateCPUPointsPolicyTransition(candidate cpupoints.PolicySnapshot) error {
	leaveOperation := m.opGate.Enter()
	defer leaveOperation()
	return m.preflightCPUPointsPolicyLocked(candidate)
}

// ReconcileCPUPointsPolicy verifies and safely programs a detached candidate.
// The caller publishes it separately only after both source files are confirmed.
func (m *Manager) ReconcileCPUPointsPolicy(candidate, fallback cpupoints.PolicySnapshot) error {
	leaveOperation := m.opGate.Enter()
	defer leaveOperation()
	return m.reconcileCPUPointsPolicyLocked(candidate, fallback, true)
}

// PublishCPUPointsPolicy makes a successfully reconciled candidate authoritative.
func (m *Manager) PublishCPUPointsPolicy(candidate cpupoints.PolicySnapshot) {
	m.mu.Lock()
	m.cpuPointsPolicy = candidate
	m.pendingCPUPointsPolicy = nil
	m.cpuPointsDegraded = false
	m.mu.Unlock()
}

func (m *Manager) preflightCPUPointsPolicyLocked(candidate cpupoints.PolicySnapshot) error {
	m.mu.RLock()
	affected := make([]int, 0)
	for uid, allocation := range m.cpuAllocations {
		if candidate.ClassForUID(uid) != allocation.class {
			affected = append(affected, uid)
		}
	}
	m.mu.RUnlock()
	if len(affected) == 0 {
		return nil
	}
	sort.Ints(affected)
	return &CPUPointsClassChangeError{UIDs: affected}
}

func (m *Manager) reconcileCPUPointsPolicyLocked(candidate, fallback cpupoints.PolicySnapshot, force bool) error {
	if err := m.preflightCPUPointsPolicyLocked(candidate); err != nil {
		return err
	}

	previousCapacity := m.cpuCapacity.State()
	capacity, err := m.cpuCapacity.Refresh(candidate.Pool())
	if err != nil {
		return m.deferCPUPointsPolicyLocked(fallback, &CPUPointsReconciliationError{Step: "capacity", Err: err})
	}

	m.mu.RLock()
	hierarchy := m.cpuPointsHierarchy
	allocations := make(map[int]cpuPointsAllocation, len(m.cpuAllocations))
	for uid, allocation := range m.cpuAllocations {
		allocations[uid] = allocation
	}
	currentPolicy := m.cpuPointsPolicy
	pending := m.pendingCPUPointsPolicy != nil
	m.mu.RUnlock()

	if hierarchy.Parent == "" {
		m.clearCPUPointsRetryLocked()
		return nil
	}
	if !force && !pending {
		if previousCapacity.Available && previousCapacity.LastVerified == capacity.LastVerified {
			return nil
		}
		if err := m.cgroupManager.ApplyCPUPointsParentQuota(hierarchy, capacity.LastVerified); err != nil {
			return m.deferCPUPointsPolicyLocked(fallback, &CPUPointsReconciliationError{Step: "parent_quota", Err: err})
		}
		return nil
	}

	if err := m.cgroupManager.ApplyCPUPointsParentQuota(hierarchy, capacity.LastVerified); err != nil {
		return m.deferCPUPointsPolicyLocked(fallback, &CPUPointsReconciliationError{Step: "parent_quota", Err: err})
	}

	targetBestEffort, err := candidate.BestEffort().KernelWeight()
	if err != nil {
		return m.deferCPUPointsPolicyLocked(fallback, &CPUPointsReconciliationError{Step: "best_effort_weight", Err: err})
	}
	currentBestEffort := currentPolicy.BestEffort().Value()
	if targetBestEffort.Value() < int(currentBestEffort) {
		if err := m.cgroupManager.ApplyCPUPointsBestEffortWeight(hierarchy, targetBestEffort); err != nil {
			return m.deferCPUPointsPolicyLocked(fallback, &CPUPointsReconciliationError{Step: "best_effort_weight", Err: err})
		}
	}

	type leafUpdate struct {
		uid  int
		from cpupoints.KernelCPUWeight
		to   cpupoints.KernelCPUWeight
		path string
	}
	updates := make([]leafUpdate, 0)
	var targetSum uint64
	for uid, allocation := range allocations {
		if allocation.class != cpupoints.AllocationClassGuaranteed {
			continue
		}
		guarantee, ok := candidate.GuaranteeForUID(uid)
		if !ok {
			return &CPUPointsClassChangeError{UIDs: []int{uid}}
		}
		target, conversionErr := guarantee.Points().KernelWeight()
		if conversionErr != nil {
			return m.deferCPUPointsPolicyLocked(fallback, &CPUPointsReconciliationError{Step: "leaf_weight", UID: uid, Err: conversionErr})
		}
		targetSum += guarantee.Points().Value()
		if target.Value() != allocation.weight.Value() {
			updates = append(updates, leafUpdate{uid: uid, from: allocation.weight, to: target, path: allocation.leafPath})
		}
	}
	sort.Slice(updates, func(i, j int) bool {
		leftDecrease := updates[i].to.Value() < updates[i].from.Value()
		rightDecrease := updates[j].to.Value() < updates[j].from.Value()
		if leftDecrease != rightDecrease {
			return leftDecrease
		}
		return updates[i].uid < updates[j].uid
	})

	m.mu.RLock()
	programmed := m.programmedGuaranteePoints
	m.mu.RUnlock()
	if targetSum > programmed {
		if err := m.applyCPUPointsAggregateWeightLocked(hierarchy, targetSum); err != nil {
			return m.deferCPUPointsPolicyLocked(fallback, err)
		}
	}

	for _, update := range updates {
		if err := m.cgroupManager.ApplyCPUPointsUserWeight(update.path, update.to); err != nil {
			return m.deferCPUPointsPolicyLocked(fallback, &CPUPointsReconciliationError{Step: "leaf_weight", UID: update.uid, Err: err})
		}
		m.mu.Lock()
		allocation := m.cpuAllocations[update.uid]
		allocation.weight = update.to
		m.cpuAllocations[update.uid] = allocation
		m.recomputeAppliedGuaranteeLocked()
		m.mu.Unlock()
	}

	if err := m.applyCPUPointsAggregateWeightLocked(hierarchy, targetSum); err != nil {
		return m.deferCPUPointsPolicyLocked(fallback, err)
	}
	if targetBestEffort.Value() >= int(currentBestEffort) {
		if err := m.cgroupManager.ApplyCPUPointsBestEffortWeight(hierarchy, targetBestEffort); err != nil {
			return m.deferCPUPointsPolicyLocked(fallback, &CPUPointsReconciliationError{Step: "best_effort_weight", Err: err})
		}
	}

	m.clearCPUPointsRetryLocked()
	return nil
}

func (m *Manager) applyCPUPointsAggregateWeightLocked(hierarchy cgroup.CPUPointsHierarchy, points uint64) error {
	value := int(points)
	if value == 0 {
		value = 1
	}
	weight, err := cpupoints.NewKernelCPUWeight(value)
	if err != nil {
		return &CPUPointsReconciliationError{Step: "guaranteed_domain_weight", Err: err}
	}
	m.mu.Lock()
	if points > m.programmedGuaranteePoints {
		m.programmedGuaranteePoints = points
	}
	m.mu.Unlock()
	if err := m.cgroupManager.ApplyCPUPointsGuaranteedWeight(hierarchy, weight); err != nil {
		return &CPUPointsReconciliationError{Step: "guaranteed_domain_weight", Err: err}
	}
	m.mu.Lock()
	m.programmedGuaranteePoints = points
	if points == 0 {
		m.programmedGuaranteePoints = 1
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) recomputeAppliedGuaranteeLocked() {
	var total uint64
	for _, allocation := range m.cpuAllocations {
		if allocation.class == cpupoints.AllocationClassGuaranteed {
			total += uint64(allocation.weight.Value())
		}
	}
	applied, err := cpupoints.NewAppliedGuaranteePoints(total)
	if err == nil {
		m.appliedGuaranteePoints = applied
	}
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
	case "capacity", "parent_quota", "guaranteed_domain_weight", "best_effort_weight", "leaf_weight":
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

func (m *Manager) retryCPUPointsPolicyLocked() error {
	m.mu.RLock()
	target := m.cpuPointsPolicy
	if m.pendingCPUPointsPolicy != nil {
		target = *m.pendingCPUPointsPolicy
	}
	m.mu.RUnlock()
	err := m.reconcileCPUPointsPolicyLocked(target, target, false)
	if err == nil {
		return nil
	}
	return err
}
