package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

var memorySystemdProperties = []systemdunit.PropertyName{
	systemdunit.PropertyMemoryHigh,
	systemdunit.PropertyMemoryMax,
	systemdunit.PropertyMemorySwapMax,
}

// ioSystemdProperties is a restoration superset, not a policy assignment list.
// Keep IOWeight so an unexpected recovered lease can be released completely;
// weighted I/O is an adapter capability and no native policy produces it.
var ioSystemdProperties = []systemdunit.PropertyName{
	systemdunit.PropertyIOWeight,
	systemdunit.PropertyIOReadBandwidthMax,
	systemdunit.PropertyIOWriteBandwidthMax,
	systemdunit.PropertyIOReadIOPSMax,
	systemdunit.PropertyIOWriteIOPSMax,
}

// SystemdResourceReconciliationError identifies one independently failed
// memory or I/O plan without implying that CPU enforcement failed.
type SystemdResourceReconciliationError struct {
	UID      int
	Resource systemdunit.ResourceKind
	Step     string
	Err      error
}

func (e *SystemdResourceReconciliationError) Error() string {
	return fmt.Sprintf("reconcile systemd-native %s for UID %d at %s: %v", e.Resource, e.UID, e.Step, e.Err)
}

// Unwrap exposes the typed adapter or authority error.
func (e *SystemdResourceReconciliationError) Unwrap() error { return e.Err }

func (m *Manager) activateSystemdEnforcement(metrics *SystemMetrics) error {
	cfg := m.GetConfig()
	m.mu.Lock()
	m.systemdCPURequested = len(metrics.CPUEligibleUsers) > 0
	m.systemdResourcesRequested = (cfg.RAMEnabled && len(metrics.RAMEligibleUsers) > 0) || (cfg.IOEnabled && len(metrics.IOEligibleUsers) > 0)
	cpuRequested := m.systemdCPURequested
	resourceRequested := m.systemdResourcesRequested
	m.mu.Unlock()

	var reconcileErrors []error
	if cpuRequested {
		if err := m.reconcileSystemdCPUPoints(context.Background(), m.CurrentCPUPointsPolicy()); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	} else if err := m.restoreSystemdCPUProperties(context.Background()); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}
	if resourceRequested {
		if err := m.reconcileSystemdResources(context.Background(), metrics, cfg); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	} else if err := m.restoreAllSystemdResourceProperties(context.Background()); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}
	return errors.Join(reconcileErrors...)
}

func (m *Manager) reconcileSystemdResources(ctx context.Context, metrics *SystemMetrics, cfg *config.Config) error {
	initializeCycleResourceAuthorities(metrics)
	const attempts = 2
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		resetCycleResourceAuthorities(metrics, cfg)
		err = m.reconcileSystemdResourcesAttempt(ctx, metrics, cfg)
		if err == nil || ctx.Err() != nil || !systemdunit.IsRetryableReconciliation(err) {
			return err
		}
	}
	return err
}

func (m *Manager) reconcileSystemdResourcesAttempt(ctx context.Context, metrics *SystemMetrics, cfg *config.Config) error {
	if m.systemdUnits == nil {
		return fmt.Errorf("reconcile systemd-native resources: adapter is unavailable")
	}
	if err := m.systemdUnits.ReconcileOwned(ctx); err != nil {
		return fmt.Errorf("reconcile owned systemd properties before resource plan: %w", err)
	}
	topology, err := m.systemdUnits.Discover(ctx)
	if err != nil {
		return fmt.Errorf("discover systemd resource topology: %w", err)
	}
	units := make(map[int]systemdunit.UnitIdentity, len(topology.Users))
	for _, user := range topology.Users {
		units[int(user.UID)] = user.Unit.Identity
	}
	m.pruneReleasedSystemdResourceState(units)
	ramDesired := desiredResourceUsers(cfg.RAMEnabled, metrics.RAMEligibleUsers)
	ioDesired := desiredResourceUsers(cfg.IOEnabled, metrics.IOEligibleUsers)
	desiredUIDs := unionResourceUIDs(ramDesired, ioDesired)

	var ioAssignments []systemdunit.PropertyAssignment
	if len(ioDesired) > 0 {
		ioAssignments, err = m.systemdIOAssignments(cfg, 1)
		if err != nil {
			return &SystemdResourceReconciliationError{Resource: systemdunit.ResourceIO, Step: "plan", Err: err}
		}
	}
	var reconcileErrors []error
	type plannedResource struct {
		uid         int
		identity    systemdunit.UnitIdentity
		resource    systemdunit.ResourceKind
		assignments []systemdunit.PropertyAssignment
		swap        bool
	}
	planned := make([]plannedResource, 0, len(desiredUIDs)*2)
	for _, uid := range desiredUIDs {
		identity, present := units[uid]
		if !present {
			if ramDesired[uid] {
				authority := systemdunit.ResourceAuthority{Resource: systemdunit.ResourceMemory, State: systemdunit.ResourceCoveragePartial, Reason: systemdunit.ResourceCoverageAuthoritySplit}
				recordCycleResourceAuthority(metrics, uid, systemdunit.ResourceMemory, authority)
				reconcileErrors = append(reconcileErrors, m.recordMissingSystemdResourceAuthority(uid, systemdunit.ResourceMemory))
			}
			if ioDesired[uid] {
				authority := systemdunit.ResourceAuthority{Resource: systemdunit.ResourceIO, State: systemdunit.ResourceCoveragePartial, Reason: systemdunit.ResourceCoverageAuthoritySplit}
				recordCycleResourceAuthority(metrics, uid, systemdunit.ResourceIO, authority)
				reconcileErrors = append(reconcileErrors, m.recordMissingSystemdResourceAuthority(uid, systemdunit.ResourceIO))
			}
			continue
		}
		if ramDesired[uid] {
			assignments, err := m.systemdMemoryAssignments(uid, cfg)
			if err != nil {
				reconcileErrors = append(reconcileErrors, &SystemdResourceReconciliationError{UID: uid, Resource: systemdunit.ResourceMemory, Step: "plan", Err: err})
			} else {
				planned = append(planned, plannedResource{uid: uid, identity: identity, resource: systemdunit.ResourceMemory, assignments: assignments, swap: cfg.DisableSwap})
			}
		}
		if ioDesired[uid] {
			planned = append(planned, plannedResource{uid: uid, identity: identity, resource: systemdunit.ResourceIO, assignments: ioAssignments})
		}
	}
	requests := make([]systemdunit.ResourceAuthorityRequest, len(planned))
	for index, plan := range planned {
		requests[index] = systemdunit.ResourceAuthorityRequest{Identity: plan.identity, UID: uint32(plan.uid), Resource: plan.resource, Assignments: plan.assignments}
	}
	authorities, err := m.systemdUnits.CheckResourceAuthorities(ctx, requests)
	if err != nil {
		invalidateCycleResourceAuthorities(metrics)
		return fmt.Errorf("inspect systemd resource authority: %w", err)
	}
	if len(authorities) != len(planned) {
		invalidateCycleResourceAuthorities(metrics)
		return fmt.Errorf("inspect systemd resource authority: adapter returned %d results for %d requests", len(authorities), len(planned))
	}
	// The first authority pass builds a complete process snapshot. Reconfirm the
	// unit identity set immediately before any apply or restore so no mutation is
	// based on a topology that already changed while the plan was assembled.
	if err := m.systemdUnits.ConfirmTopology(ctx, topology); err != nil {
		invalidateCycleResourceAuthorities(metrics)
		return &SystemdResourceReconciliationError{Step: "pre_mutation_identity", Err: err}
	}
	applied := make([]plannedResource, 0, len(planned))
	for index, plan := range planned {
		result := authorities[index]
		if result.Err != nil || result.Authority.State != systemdunit.ResourceCoverageComplete || result.Authority.Reason != systemdunit.ResourceCoverageVerified {
			recordCycleResourceAuthority(metrics, plan.uid, plan.resource, result.Authority)
			if result.Err == nil {
				result.Err = &systemdunit.ResourceAuthorityError{UID: uint32(plan.uid), Authority: result.Authority, Err: fmt.Errorf("adapter did not confirm complete resource authority")}
			}
			reconcileErrors = append(reconcileErrors, m.refuseSystemdResource(ctx, plan.uid, plan.identity, plan.resource, result.Authority, result.Err))
			continue
		}
		if err := m.mutateSystemdResource(ctx, plan.uid, plan.identity, plan.resource, plan.assignments); err != nil {
			refused := systemdunit.ResourceAuthority{Resource: plan.resource, State: systemdunit.ResourceCoverageRefused, Reason: systemdunit.ResourceCoverageApplyFailed}
			recordCycleResourceAuthority(metrics, plan.uid, plan.resource, refused)
			reconcileErrors = append(reconcileErrors, err)
			continue
		}
		applied = append(applied, plan)
	}

	if len(applied) > 0 {
		readbackConfirmed := make([]plannedResource, 0, len(applied))
		for _, plan := range applied {
			if _, err := m.systemdUnits.ConfirmApplied(ctx, plan.identity, plan.assignments); err != nil {
				if systemdunit.IsRetryableReconciliation(err) {
					invalidateCycleResourceAuthorities(metrics)
					return errors.Join(errors.Join(reconcileErrors...), &SystemdResourceReconciliationError{UID: plan.uid, Resource: plan.resource, Step: "pre_acknowledgement_readback", Err: err})
				}
				unavailable := systemdunit.ResourceAuthority{Resource: plan.resource, State: systemdunit.ResourceCoverageRefused, Reason: systemdunit.ResourceCoverageInspectionFailed}
				recordCycleResourceAuthority(metrics, plan.uid, plan.resource, unavailable)
				m.recordSystemdResourceUnapplied(plan.uid, plan.resource, unavailable)
				reconcileErrors = append(reconcileErrors, &SystemdResourceReconciliationError{UID: plan.uid, Resource: plan.resource, Step: "pre_acknowledgement_readback", Err: err})
				continue
			}
			readbackConfirmed = append(readbackConfirmed, plan)
		}
		confirmedRequests := make([]systemdunit.ResourceAuthorityRequest, len(readbackConfirmed))
		for index, plan := range readbackConfirmed {
			confirmedRequests[index] = systemdunit.ResourceAuthorityRequest{Identity: plan.identity, UID: uint32(plan.uid), Resource: plan.resource, Assignments: plan.assignments}
		}
		confirmed, err := m.systemdUnits.CheckResourceAuthorities(ctx, confirmedRequests)
		if err != nil {
			invalidateCycleResourceAuthorities(metrics)
			return errors.Join(errors.Join(reconcileErrors...), fmt.Errorf("confirm systemd resource authority before acknowledgement: %w", err))
		}
		if len(confirmed) != len(readbackConfirmed) {
			invalidateCycleResourceAuthorities(metrics)
			return errors.Join(errors.Join(reconcileErrors...), fmt.Errorf("confirm systemd resource authority before acknowledgement: adapter returned %d results for %d requests", len(confirmed), len(readbackConfirmed)))
		}
		if err := m.systemdUnits.ConfirmTopology(ctx, topology); err != nil {
			invalidateCycleResourceAuthorities(metrics)
			return errors.Join(errors.Join(reconcileErrors...), &SystemdResourceReconciliationError{Step: "pre_acknowledgement_identity", Err: err})
		}
		for index, plan := range readbackConfirmed {
			result := confirmed[index]
			recordCycleResourceAuthority(metrics, plan.uid, plan.resource, result.Authority)
			if result.Err != nil || result.Authority.State != systemdunit.ResourceCoverageComplete || result.Authority.Reason != systemdunit.ResourceCoverageVerified {
				if result.Err == nil {
					result.Err = &systemdunit.ResourceAuthorityError{UID: uint32(plan.uid), Authority: result.Authority, Err: fmt.Errorf("adapter did not reconfirm complete resource authority")}
				}
				restoreErr := m.restoreSystemdResource(ctx, plan.uid, plan.identity, plan.resource)
				m.recordSystemdResourceAuthority(plan.uid, plan.resource, result.Authority)
				reconcileErrors = append(reconcileErrors,
					&SystemdResourceReconciliationError{UID: plan.uid, Resource: plan.resource, Step: "pre_acknowledgement_authority", Err: result.Err},
					restoreErr,
				)
				continue
			}
			m.publishSystemdResource(plan.uid, plan.identity, plan.resource, plan.swap, result.Authority)
		}
	}

	m.mu.RLock()
	tracked := make(map[int]systemdunit.UnitIdentity, len(m.systemdResourceUnits))
	for uid, identity := range m.systemdResourceUnits {
		tracked[uid] = identity
	}
	m.mu.RUnlock()
	for uid, identity := range tracked {
		if !ramDesired[uid] {
			if err := m.restoreSystemdResource(ctx, uid, identity, systemdunit.ResourceMemory); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
		}
		if !ioDesired[uid] {
			if err := m.restoreSystemdResource(ctx, uid, identity, systemdunit.ResourceIO); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
		}
	}
	m.mu.Lock()
	m.systemdResourcesRequested = len(ramDesired) > 0 || len(ioDesired) > 0
	m.refreshResourceLimitsActiveLocked(time.Now())
	m.mu.Unlock()
	return errors.Join(reconcileErrors...)
}

func initializeCycleResourceAuthorities(metrics *SystemMetrics) {
	if metrics == nil {
		return
	}
	metrics.systemdRAMAuthority = make(map[int]*systemdunit.ResourceAuthority)
	metrics.systemdIOAuthority = make(map[int]*systemdunit.ResourceAuthority)
}

func resetCycleResourceAuthorities(metrics *SystemMetrics, cfg *config.Config) {
	if metrics == nil {
		return
	}
	clear(metrics.systemdRAMAuthority)
	clear(metrics.systemdIOAuthority)
	for uid := range desiredResourceUsers(cfg.RAMEnabled, metrics.RAMEligibleUsers) {
		metrics.systemdRAMAuthority[uid] = nil
	}
	for uid := range desiredResourceUsers(cfg.IOEnabled, metrics.IOEligibleUsers) {
		metrics.systemdIOAuthority[uid] = nil
	}
}

func recordCycleResourceAuthority(metrics *SystemMetrics, uid int, resource systemdunit.ResourceKind, authority systemdunit.ResourceAuthority) {
	if metrics == nil {
		return
	}
	observed := authority
	if resource == systemdunit.ResourceMemory {
		metrics.systemdRAMAuthority[uid] = &observed
		return
	}
	metrics.systemdIOAuthority[uid] = &observed
}

func invalidateCycleResourceAuthorities(metrics *SystemMetrics) {
	if metrics == nil {
		return
	}
	for uid := range metrics.systemdRAMAuthority {
		metrics.systemdRAMAuthority[uid] = nil
	}
	for uid := range metrics.systemdIOAuthority {
		metrics.systemdIOAuthority[uid] = nil
	}
}

func (m *Manager) pruneReleasedSystemdResourceState(current map[int]systemdunit.UnitIdentity) {
	m.mu.RLock()
	tracked := make(map[int]systemdunit.UnitIdentity, len(m.systemdResourceUnits))
	for uid, identity := range m.systemdResourceUnits {
		tracked[uid] = identity
	}
	m.mu.RUnlock()
	type resourceActivity struct {
		uid          int
		identity     systemdunit.UnitIdentity
		activeMemory bool
		activeIO     bool
	}
	activity := make([]resourceActivity, 0, len(tracked))
	for uid, identity := range tracked {
		activeMemory := false
		activeIO := false
		for _, lease := range m.systemdUnits.Leases(identity) {
			resource, ok := lease.Property.Resource()
			if !ok || !lease.Active() {
				continue
			}
			switch resource {
			case systemdunit.ResourceMemory:
				activeMemory = true
			case systemdunit.ResourceIO:
				activeIO = true
			}
		}
		activity = append(activity, resourceActivity{uid: uid, identity: identity, activeMemory: activeMemory, activeIO: activeIO})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, observed := range activity {
		if m.systemdResourceUnits[observed.uid] != observed.identity {
			continue
		}
		state := m.resourceLimits[observed.uid]
		if !observed.activeMemory {
			state.ram = false
			state.ramApplied = false
			state.swap = false
		}
		if !observed.activeIO {
			state.io = false
			state.ioApplied = false
		}
		currentIdentity, present := current[observed.uid]
		if !observed.activeMemory && !observed.activeIO && (!present || currentIdentity != observed.identity) {
			delete(m.resourceLimits, observed.uid)
			delete(m.systemdResourceUnits, observed.uid)
			continue
		}
		m.resourceLimits[observed.uid] = state
	}
}

func desiredResourceUsers(enabled bool, users []int) map[int]bool {
	result := make(map[int]bool, len(users))
	if !enabled {
		return result
	}
	for _, uid := range users {
		if uid > 0 {
			result[uid] = true
		}
	}
	return result
}

func unionResourceUIDs(left, right map[int]bool) []int {
	seen := make(map[int]bool, len(left)+len(right))
	for uid := range left {
		seen[uid] = true
	}
	for uid := range right {
		seen[uid] = true
	}
	result := make([]int, 0, len(seen))
	for uid := range seen {
		result = append(result, uid)
	}
	sort.Ints(result)
	return result
}

func (m *Manager) systemdMemoryAssignments(uid int, cfg *config.Config) ([]systemdunit.PropertyAssignment, error) {
	quota := cfg.RAMQuotaPerUser
	if cfg.GetAutodetectPatterns() && m.policyEngine != nil {
		if policy, exists := m.policyEngine.GetPolicy(uid); exists && policy.RAMQuota != "" {
			quota = policy.RAMQuota
		}
	}
	maxBytes, err := config.ParseByteQuota(quota)
	if err != nil || maxBytes == 0 {
		return nil, fmt.Errorf("invalid RAM quota %q", quota)
	}
	highBytes := uint64(float64(maxBytes) * cfg.GetRAMHighRatio())
	pageSize := uint64(os.Getpagesize())
	maxBytes -= maxBytes % pageSize
	highBytes -= highBytes % pageSize
	if maxBytes == 0 {
		return nil, fmt.Errorf("RAM quota %q is smaller than the host page size %d", quota, pageSize)
	}
	values := []struct {
		name  systemdunit.PropertyName
		value uint64
	}{
		{name: systemdunit.PropertyMemoryMax, value: maxBytes},
	}
	if cfg.GetRAMHighRatio() > 0 {
		values = append([]struct {
			name  systemdunit.PropertyName
			value uint64
		}{{name: systemdunit.PropertyMemoryHigh, value: highBytes}}, values...)
	}
	if cfg.DisableSwap {
		values = append(values, struct {
			name  systemdunit.PropertyName
			value uint64
		}{name: systemdunit.PropertyMemorySwapMax, value: 0})
	}
	result := make([]systemdunit.PropertyAssignment, 0, len(values))
	for _, value := range values {
		assignment, err := systemdunit.NewPropertyAssignment(value.name, value.value)
		if err != nil {
			return nil, err
		}
		result = append(result, assignment)
	}
	return result, nil
}

func (m *Manager) systemdIOAssignments(cfg *config.Config, multiplier float64) ([]systemdunit.PropertyAssignment, error) {
	devices, err := m.resolveSystemdIODevices(cfg.GetIODeviceFilter())
	if err != nil {
		return nil, err
	}
	type limit struct {
		name  systemdunit.PropertyName
		value uint64
	}
	var limits []limit
	for _, candidate := range []struct {
		name  systemdunit.PropertyName
		value string
	}{
		{name: systemdunit.PropertyIOReadBandwidthMax, value: cfg.GetIOReadBPS()},
		{name: systemdunit.PropertyIOWriteBandwidthMax, value: cfg.GetIOWriteBPS()},
	} {
		value := strings.TrimSpace(candidate.value)
		if value == "" || value == "0" || value == "max" {
			continue
		}
		parsed, err := config.ParseByteQuota(value)
		if err != nil || parsed == 0 {
			return nil, fmt.Errorf("invalid %s value %q", candidate.name, value)
		}
		limits = append(limits, limit{name: candidate.name, value: uint64(float64(parsed) * multiplier)})
	}
	for _, candidate := range []struct {
		name  systemdunit.PropertyName
		value int
	}{
		{name: systemdunit.PropertyIOReadIOPSMax, value: cfg.GetIOReadIOPS()},
		{name: systemdunit.PropertyIOWriteIOPSMax, value: cfg.GetIOWriteIOPS()},
	} {
		if candidate.value > 0 {
			limits = append(limits, limit{name: candidate.name, value: uint64(float64(candidate.value) * multiplier)})
		}
	}
	if len(limits) == 0 {
		return nil, fmt.Errorf("I/O limiting is enabled without a finite bandwidth or IOPS limit")
	}
	result := make([]systemdunit.PropertyAssignment, 0, len(limits))
	for _, limit := range limits {
		values := make([]systemdunit.DeviceLimit, len(devices))
		for index, device := range devices {
			values[index] = systemdunit.DeviceLimit{Path: device, Value: limit.value}
		}
		assignment, err := systemdunit.NewDevicePropertyAssignment(limit.name, values)
		if err != nil {
			return nil, err
		}
		result = append(result, assignment)
	}
	return result, nil
}

func (m *Manager) mutateSystemdResource(ctx context.Context, uid int, identity systemdunit.UnitIdentity, resource systemdunit.ResourceKind, assignments []systemdunit.PropertyAssignment) error {
	if _, err := m.systemdUnits.Apply(ctx, identity, assignments); err != nil {
		return &SystemdResourceReconciliationError{UID: uid, Resource: resource, Step: "apply", Err: err}
	}
	return nil
}

func (m *Manager) publishSystemdResource(uid int, identity systemdunit.UnitIdentity, resource systemdunit.ResourceKind, swap bool, authority systemdunit.ResourceAuthority) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.resourceLimits[uid]
	if resource == systemdunit.ResourceMemory {
		state.ram = true
		state.ramApplied = true
		state.swap = swap
		state.ramAuthority = authority
	} else {
		state.io = true
		state.ioApplied = true
		state.ioAuthority = authority
	}
	m.resourceLimits[uid] = state
	m.systemdResourceUnits[uid] = identity
}

func (m *Manager) refuseSystemdResource(ctx context.Context, uid int, identity systemdunit.UnitIdentity, resource systemdunit.ResourceKind, authority systemdunit.ResourceAuthority, authorityErr error) error {
	m.mu.RLock()
	state := m.resourceLimits[uid]
	trackedIdentity, tracked := m.systemdResourceUnits[uid]
	applied := state.ramApplied
	if resource == systemdunit.ResourceIO {
		applied = state.ioApplied
	}
	m.mu.RUnlock()
	var restoreErr error
	if applied && tracked && trackedIdentity == identity {
		restoreErr = m.restoreSystemdResource(ctx, uid, identity, resource)
	}
	// Retain the refusal after a successful release so every observation
	// surface explains why the resource is not currently applied.
	m.recordSystemdResourceAuthority(uid, resource, authority)
	return errors.Join(
		&SystemdResourceReconciliationError{UID: uid, Resource: resource, Step: "authority", Err: authorityErr},
		restoreErr,
	)
}

func (m *Manager) recordMissingSystemdResourceAuthority(uid int, resource systemdunit.ResourceKind) error {
	authority := systemdunit.ResourceAuthority{Resource: resource, State: systemdunit.ResourceCoveragePartial, Reason: systemdunit.ResourceCoverageAuthoritySplit}
	m.recordSystemdResourceAuthority(uid, resource, authority)
	return &SystemdResourceReconciliationError{
		UID: uid, Resource: resource, Step: "authority",
		Err: &systemdunit.ResourceAuthorityError{UID: uint32(uid), Authority: authority, Err: fmt.Errorf("no authoritative user-%d.slice contains the observed UID workload", uid)},
	}
}

func (m *Manager) recordSystemdResourceAuthority(uid int, resource systemdunit.ResourceKind, authority systemdunit.ResourceAuthority) {
	m.mu.Lock()
	state := m.resourceLimits[uid]
	if resource == systemdunit.ResourceMemory {
		state.ram = true
		state.ramAuthority = authority
	} else {
		state.io = true
		state.ioAuthority = authority
	}
	m.resourceLimits[uid] = state
	m.mu.Unlock()
}

func (m *Manager) recordSystemdResourceUnapplied(uid int, resource systemdunit.ResourceKind, authority systemdunit.ResourceAuthority) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.resourceLimits[uid]
	if resource == systemdunit.ResourceMemory {
		state.ram = true
		state.ramApplied = false
		state.swap = false
		state.ramAuthority = authority
	} else {
		state.io = true
		state.ioApplied = false
		state.ioAuthority = authority
	}
	m.resourceLimits[uid] = state
}

func (m *Manager) restoreSystemdResource(ctx context.Context, uid int, identity systemdunit.UnitIdentity, resource systemdunit.ResourceKind) error {
	properties := memorySystemdProperties
	if resource == systemdunit.ResourceIO {
		properties = ioSystemdProperties
	}
	if _, err := m.systemdUnits.RestoreProperties(ctx, identity, properties); err != nil {
		return &SystemdResourceReconciliationError{UID: uid, Resource: resource, Step: "restore", Err: err}
	}
	m.mu.Lock()
	state := m.resourceLimits[uid]
	if resource == systemdunit.ResourceMemory {
		state.ram = false
		state.ramApplied = false
		state.swap = false
		state.ramAuthority = systemdunit.ResourceAuthority{}
	} else {
		state.io = false
		state.ioApplied = false
		state.ioAuthority = systemdunit.ResourceAuthority{}
	}
	if !state.ram && !state.io && !state.ramApplied && !state.ioApplied {
		delete(m.resourceLimits, uid)
		delete(m.systemdResourceUnits, uid)
	} else {
		m.resourceLimits[uid] = state
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) restoreAllSystemdResourceProperties(ctx context.Context) error {
	m.mu.RLock()
	tracked := make(map[int]systemdunit.UnitIdentity, len(m.systemdResourceUnits))
	for uid, identity := range m.systemdResourceUnits {
		tracked[uid] = identity
	}
	m.mu.RUnlock()
	var restoreErrors []error
	for uid, identity := range tracked {
		if err := m.restoreSystemdResource(ctx, uid, identity, systemdunit.ResourceMemory); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
		if err := m.restoreSystemdResource(ctx, uid, identity, systemdunit.ResourceIO); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	if len(restoreErrors) == 0 {
		m.mu.Lock()
		m.systemdResourcesRequested = false
		m.refreshResourceLimitsActiveLocked(time.Now())
		m.mu.Unlock()
	}
	return errors.Join(restoreErrors...)
}
