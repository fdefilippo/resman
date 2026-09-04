package state

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

// SystemdCPUUnitAdapter is the ownership-preserving systemd boundary used by
// the state manager. It intentionally exposes no process-management method.
type SystemdCPUUnitAdapter interface {
	Discover(context.Context) (systemdunit.TopologySnapshot, error)
	CheckResourceAuthority(context.Context, systemdunit.UnitIdentity, uint32, systemdunit.ResourceKind, []systemdunit.PropertyAssignment) (systemdunit.ResourceAuthority, error)
	Apply(context.Context, systemdunit.UnitIdentity, []systemdunit.PropertyAssignment) (systemdunit.UnitSnapshot, error)
	Restore(context.Context, systemdunit.UnitIdentity) (systemdunit.RestoreResult, error)
	RestoreProperties(context.Context, systemdunit.UnitIdentity, []systemdunit.PropertyName) (systemdunit.RestoreResult, error)
	ReconcileOwned(context.Context) error
	OwnedUnits() []systemdunit.UnitIdentity
	Leases(systemdunit.UnitIdentity) []systemdunit.PropertyLease
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
		for _, identity := range adapter.OwnedUnits() {
			for _, lease := range adapter.Leases(identity) {
				if !lease.Active() {
					continue
				}
				if lease.Property.IsCPU() {
					m.systemdCPURequested = true
					if identity.IsParentUserSlice() {
						m.systemdCPUParent = identity
					} else if uid, ok := systemdUserSliceUID(identity.Name); ok {
						m.systemdCPUSlices[uid] = identity
					}
				}
				if _, resourceProperty := lease.Property.Resource(); resourceProperty {
					m.systemdResourcesRequested = true
					if uid, ok := systemdUserSliceUID(identity.Name); ok {
						m.systemdResourceUnits[uid] = identity
					}
				}
			}
		}
		return nil
	}
}

func systemdUserSliceUID(name string) (int, bool) {
	if !strings.HasPrefix(name, "user-") || !strings.HasSuffix(name, ".slice") {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, "user-"), ".slice")
	uid, err := strconv.ParseUint(value, 10, 31)
	if err != nil || strconv.FormatUint(uid, 10) != value {
		return 0, false
	}
	return int(uid), true
}

func (m *Manager) activateSystemdCPUPoints(metrics *SystemMetrics) error {
	m.mu.Lock()
	wasRequested := m.systemdCPURequested
	m.systemdCPURequested = len(metrics.CPUEligibleUsers) > 0
	m.mu.Unlock()
	if len(metrics.CPUEligibleUsers) == 0 {
		if wasRequested {
			return m.restoreSystemdCPUProperties(context.Background())
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
	participants := make([]cpupoints.ActiveUserSlice, 0, len(topology.Users))
	units := make(map[int]systemdunit.UnitIdentity, len(topology.Users))
	eligibleUsers := 0
	for _, user := range topology.Users {
		uid := int(user.UID)
		eligible := m.GetConfig().EvaluateUserEligibility(m.getUsername(uid)).EligibleForCPU
		participants = append(participants, cpupoints.ActiveUserSlice{
			UID:      uid,
			Eligible: eligible,
		})
		if uid != 0 && eligible {
			eligibleUsers++
		}
		units[uid] = user.Unit.Identity
	}
	if eligibleUsers == 0 {
		// Root participates in the flat scheduler only while at least one
		// eligible non-root workload is governed. It cannot keep the finite
		// parent quota alive by itself.
		return m.restoreSystemdCPUProperties(ctx)
	}
	capacity, err := m.cpuCapacity.Refresh(policy.Pool())
	if err != nil {
		return m.deferSystemdCPUPointsError("capacity", err)
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
	signature, guaranteedSlices, bestEffortSlices, rootSlices := systemdCPUPointsPlanSummary(parent, units, plan)
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
	planChanged := m.systemdCPUPlanSignature != signature
	m.systemdCPUPlanSignature = signature
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
	if planChanged {
		m.logger.Info("Systemd-native CPU Points plan published",
			"parent_quota_usec", plan.ParentQuota().QuotaMicroseconds(),
			"parent_period_usec", plan.ParentQuota().PeriodMicroseconds(),
			"online_cpus", plan.ParentQuota().OnlineCPUs().Value(),
			"scale", plan.Scale(),
			"slice_count", len(plan.Slices()),
			"root_slices", rootSlices,
			"guaranteed_slices", guaranteedSlices,
			"best_effort_slices", bestEffortSlices,
		)
	}
}

func systemdCPUPointsPlanSummary(parent systemdunit.UnitIdentity, units map[int]systemdunit.UnitIdentity, plan cpupoints.FlatPlan) (string, int, int, int) {
	var signature strings.Builder
	fmt.Fprintf(&signature, "%s:%s:%d:%d:%d:%d", parent.Name, parent.InvocationIDString(), parent.ControlGroupID,
		plan.ParentQuota().QuotaMicroseconds(), plan.ParentQuota().PeriodMicroseconds(), plan.Scale())
	guaranteedSlices, bestEffortSlices, rootSlices := 0, 0, 0
	for _, slice := range plan.Slices() {
		identity := units[slice.UID()]
		fmt.Fprintf(&signature, "|%d:%s:%s:%d:%s:%d", slice.UID(), identity.Name, identity.InvocationIDString(), identity.ControlGroupID, slice.Class(), slice.Weight().Value())
		switch slice.Class() {
		case cpupoints.FlatAllocationRoot:
			rootSlices++
		case cpupoints.FlatAllocationGuaranteed:
			guaranteedSlices++
		case cpupoints.FlatAllocationBestEffort:
			bestEffortSlices++
		}
	}
	return signature.String(), guaranteedSlices, bestEffortSlices, rootSlices
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

func (m *Manager) restoreSystemdCPUProperties(ctx context.Context) error {
	if m.systemdUnits == nil {
		return fmt.Errorf("restore systemd CPU Points: adapter is unavailable")
	}
	if err := m.systemdUnits.ReconcileOwned(ctx); err != nil {
		return fmt.Errorf("reconcile owned systemd properties before CPU restore: %w", err)
	}
	owned := make(map[string]bool)
	for _, identity := range m.systemdUnits.OwnedUnits() {
		owned[identity.Name] = true
	}
	m.mu.RLock()
	parent := m.systemdCPUParent
	slices := make(map[int]systemdunit.UnitIdentity, len(m.systemdCPUSlices))
	for uid, identity := range m.systemdCPUSlices {
		slices[uid] = identity
	}
	resources := make(map[int]userResourceLimitState, len(m.resourceLimits))
	for uid, state := range m.resourceLimits {
		resources[uid] = state
	}
	m.mu.RUnlock()

	var restoreErrors []error
	orderedUIDs := make([]int, 0, len(slices))
	for uid := range slices {
		orderedUIDs = append(orderedUIDs, uid)
	}
	sort.Ints(orderedUIDs)
	for _, uid := range orderedUIDs {
		identity := slices[uid]
		if !owned[identity.Name] {
			continue
		}
		state := resources[uid]
		var err error
		if state.ramApplied || state.ioApplied {
			_, err = m.systemdUnits.RestoreProperties(ctx, identity, []systemdunit.PropertyName{systemdunit.PropertyCPUWeight})
		} else {
			_, err = m.systemdUnits.Restore(ctx, identity)
		}
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore CPU properties for %s: %w", identity.Name, err))
		}
	}
	if parent.Name != "" && owned[parent.Name] {
		if _, err := m.systemdUnits.Restore(ctx, parent); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore CPU properties for %s: %w", parent.Name, err))
		}
	}
	if err := errors.Join(restoreErrors...); err != nil {
		return err
	}
	m.mu.Lock()
	wasActive := m.limitsActive
	hadPlan := m.systemdCPUPlanSignature != ""
	m.systemdCPURequested = false
	m.systemdCPUComplete = false
	m.systemdCPUParent = systemdunit.UnitIdentity{}
	m.systemdCPUSlices = make(map[int]systemdunit.UnitIdentity)
	m.systemdCPUPlanSignature = ""
	m.requestedCPUUsers = make(map[int]bool)
	m.activeUsers = make(map[int]bool)
	m.userLimitedAt = make(map[int]time.Time)
	m.limitsActive = false
	m.limitsAppliedTime = time.Time{}
	m.cpuPointsDegraded = false
	m.mu.Unlock()
	m.recordCPUTransitions(false, wasActive)
	if hadPlan {
		m.logger.Info("Systemd-native CPU Points plan released")
	}
	return nil
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
	hadPlan := m.systemdCPUPlanSignature != ""
	m.systemdCPURequested = false
	m.systemdCPUComplete = false
	m.systemdCPUParent = systemdunit.UnitIdentity{}
	m.systemdCPUSlices = make(map[int]systemdunit.UnitIdentity)
	m.systemdCPUPlanSignature = ""
	m.systemdResourcesRequested = false
	m.systemdResourceUnits = make(map[int]systemdunit.UnitIdentity)
	m.requestedCPUUsers = make(map[int]bool)
	m.activeUsers = make(map[int]bool)
	m.userLimitedAt = make(map[int]time.Time)
	m.resourceLimits = make(map[int]userResourceLimitState)
	m.limitsActive = false
	m.resourceLimitsActive = false
	m.limitsAppliedTime = time.Time{}
	m.resourceLimitsAppliedTime = time.Time{}
	m.cpuPointsDegraded = false
	m.mu.Unlock()
	m.recordCPUTransitions(false, wasActive)
	if hadPlan {
		m.logger.Info("Systemd-native CPU Points plan released")
	}
	return nil
}
