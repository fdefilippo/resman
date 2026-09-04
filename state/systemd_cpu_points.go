package state

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

// SystemdCPUUnitAdapter is the ownership-preserving systemd boundary used by
// the state manager. It intentionally exposes no process-management method.
type SystemdCPUUnitAdapter interface {
	Discover(context.Context) (systemdunit.TopologySnapshot, error)
	Apply(context.Context, systemdunit.UnitIdentity, []systemdunit.PropertyAssignment) (systemdunit.UnitSnapshot, error)
	Restore(context.Context, systemdunit.UnitIdentity) (systemdunit.RestoreResult, error)
	ReconcileOwned(context.Context) error
	OwnedUnits() []systemdunit.UnitIdentity
	Close()
}

// WithSystemdCPUEnforcement installs the authoritative systemd adapter and
// selects in-place CPU enforcement without weakening cgroup migration guards.
func WithSystemdCPUEnforcement(adapter SystemdCPUUnitAdapter) ManagerOption {
	return func(m *Manager) error {
		if adapter == nil {
			return fmt.Errorf("systemd CPU unit adapter is required")
		}
		m.systemdUnits = adapter
		m.enforcementStatus = cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeSystemdNative,
			Reason: cgroup.EnforcementReasonSystemdNativeAdapter,
		}
		// A recovered durable lease means CPU properties are still effective.
		// Reconcile them in the first cycle instead of publishing an inactive
		// manager beside an enforced systemd topology.
		m.systemdCPURequested = len(adapter.OwnedUnits()) > 0
		return nil
	}
}

func (m *Manager) activateSystemdCPUPoints(metrics *SystemMetrics) error {
	m.mu.Lock()
	wasRequested := m.systemdCPURequested
	m.systemdCPURequested = len(metrics.CPUEligibleUsers) > 0
	m.mu.Unlock()
	if len(metrics.CPUEligibleUsers) == 0 {
		if wasRequested || len(m.systemdUnits.OwnedUnits()) > 0 {
			return m.restoreSystemdCPUPoints(context.Background())
		}
		return nil
	}
	return m.reconcileSystemdCPUPoints(context.Background(), m.CurrentCPUPointsPolicy())
}

func (m *Manager) reconcileSystemdCPUPoints(ctx context.Context, policy cpupoints.PolicySnapshot) error {
	if m.systemdUnits == nil {
		return m.deferSystemdCPUPointsError("adapter", fmt.Errorf("systemd CPU adapter is unavailable"))
	}
	if err := m.systemdUnits.ReconcileOwned(ctx); err != nil {
		return m.deferSystemdCPUPointsError("owned_leases", err)
	}
	topology, err := m.systemdUnits.Discover(ctx)
	if err != nil {
		return m.deferSystemdCPUPointsError("discover", err)
	}
	capacity, err := m.cpuCapacity.Refresh(policy.Pool())
	if err != nil {
		return m.deferSystemdCPUPointsError("capacity", err)
	}

	participants := make([]cpupoints.ActiveUserSlice, 0, len(topology.Users))
	units := make(map[int]systemdunit.UnitIdentity, len(topology.Users))
	for _, user := range topology.Users {
		uid := int(user.UID)
		participants = append(participants, cpupoints.ActiveUserSlice{
			UID:      uid,
			Eligible: m.GetConfig().EvaluateUserEligibility(m.getUsername(uid)).EligibleForCPU,
		})
		units[uid] = user.Unit.Identity
	}
	plan, err := cpupoints.PlanFlatTopology(policy, capacity.LastVerified.OnlineCPUs(), participants)
	if err != nil {
		return m.deferSystemdCPUPointsError("plan", err)
	}

	parentAssignments, err := systemdunit.NewCPUQuotaAssignmentsFromCgroupMax(
		plan.ParentQuota().QuotaMicroseconds(),
		plan.ParentQuota().PeriodMicroseconds(),
	)
	if err != nil {
		return m.deferSystemdCPUPointsError("parent_quota", err)
	}
	if _, err := m.systemdUnits.Apply(ctx, topology.Parent.Identity, parentAssignments); err != nil {
		return m.deferSystemdCPUPointsError("parent_quota", err)
	}

	for _, slice := range plan.Slices() {
		assignment, err := systemdunit.NewPropertyAssignment(systemdunit.PropertyCPUWeight, uint64(slice.Weight().Value()))
		if err != nil {
			return m.deferSystemdCPUPointsError("leaf_weight", err)
		}
		if _, err := m.systemdUnits.Apply(ctx, units[slice.UID()], []systemdunit.PropertyAssignment{assignment}); err != nil {
			return m.deferSystemdCPUPointsError("leaf_weight", fmt.Errorf("UID %d: %w", slice.UID(), err))
		}
	}

	m.publishSystemdCPUPointsPlan(topology.Parent.Identity, units, plan)
	return nil
}

func (m *Manager) publishSystemdCPUPointsPlan(parent systemdunit.UnitIdentity, units map[int]systemdunit.UnitIdentity, plan cpupoints.FlatPlan) {
	now := time.Now()
	m.mu.Lock()
	wasActive := m.limitsActive
	previouslyActive := m.activeUsers
	m.activeUsers = make(map[int]bool)
	m.requestedCPUUsers = make(map[int]bool)
	for _, slice := range plan.Slices() {
		if !slice.Eligible() {
			continue
		}
		m.requestedCPUUsers[slice.UID()] = true
		m.activeUsers[slice.UID()] = true
		if !previouslyActive[slice.UID()] {
			m.userLimitedAt[slice.UID()] = now
		}
	}
	for uid := range m.userLimitedAt {
		if !m.activeUsers[uid] {
			delete(m.userLimitedAt, uid)
		}
	}
	m.systemdCPUParent = parent
	m.systemdCPUSlices = make(map[int]systemdunit.UnitIdentity, len(units))
	for uid, identity := range units {
		m.systemdCPUSlices[uid] = identity
	}
	m.systemdCPUComplete = true
	m.cpuPointsDegraded = false
	m.pendingCPUPointsPolicy = nil
	m.limitsActive = len(m.activeUsers) > 0
	if m.limitsActive && !wasActive {
		m.limitsAppliedTime = now
	}
	if !m.limitsActive {
		m.limitsAppliedTime = time.Time{}
	}
	activated := m.limitsActive && !wasActive
	deactivated := !m.limitsActive && wasActive
	m.mu.Unlock()
	m.recordCPUTransitions(activated, deactivated)
}

func (m *Manager) deferSystemdCPUPointsError(step string, err error) error {
	m.mu.Lock()
	m.systemdCPUComplete = false
	m.cpuPointsDegraded = true
	m.mu.Unlock()
	wrapped := &CPUPointsReconciliationError{Step: "systemd_" + step, Err: err}
	if m.prometheusExporter != nil {
		m.prometheusExporter.RecordError("cpu_points_reconciliation", cpuPointsReconciliationErrorType(wrapped))
	}
	return wrapped
}

func (m *Manager) restoreSystemdCPUPoints(ctx context.Context) error {
	if m.systemdUnits == nil {
		return fmt.Errorf("restore systemd CPU Points: adapter is unavailable")
	}
	var restoreErrors []error
	if err := m.systemdUnits.ReconcileOwned(ctx); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("reconcile owned systemd properties before restore: %w", err))
	}
	owned := m.systemdUnits.OwnedUnits()
	// Restore every leaf before the finite parent so an incomplete shutdown
	// retains the conservative host reserve until all individual weights are safe.
	sort.SliceStable(owned, func(left, right int) bool {
		if owned[left].IsParentUserSlice() {
			return false
		}
		if owned[right].IsParentUserSlice() {
			return true
		}
		return owned[left].Name < owned[right].Name
	})
	for _, identity := range owned {
		result, err := m.systemdUnits.Restore(ctx, identity)
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore %s: %w", identity.Name, err))
			continue
		}
		if len(result.Conflicts) > 0 {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore %s preserved %d external property conflicts", identity.Name, len(result.Conflicts)))
		}
	}
	if err := errors.Join(restoreErrors...); err != nil {
		m.mu.Lock()
		m.systemdCPUComplete = false
		m.cpuPointsDegraded = true
		m.mu.Unlock()
		return err
	}

	m.mu.Lock()
	wasActive := m.limitsActive
	m.systemdCPURequested = false
	m.systemdCPUComplete = false
	m.systemdCPUParent = systemdunit.UnitIdentity{}
	m.systemdCPUSlices = make(map[int]systemdunit.UnitIdentity)
	m.requestedCPUUsers = make(map[int]bool)
	m.activeUsers = make(map[int]bool)
	m.userLimitedAt = make(map[int]time.Time)
	m.limitsActive = false
	m.limitsAppliedTime = time.Time{}
	m.cpuPointsDegraded = false
	m.mu.Unlock()
	m.recordCPUTransitions(false, wasActive)
	return nil
}
