package state

import (
	"context"
	"errors"
	"fmt"
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
	for _, uid := range desiredUIDs {
		identity, present := units[uid]
		if !present {
			if ramDesired[uid] {
				reconcileErrors = append(reconcileErrors, m.recordMissingSystemdResourceAuthority(uid, systemdunit.ResourceMemory))
			}
			if ioDesired[uid] {
				reconcileErrors = append(reconcileErrors, m.recordMissingSystemdResourceAuthority(uid, systemdunit.ResourceIO))
			}
			continue
		}
		if ramDesired[uid] {
			assignments, err := m.systemdMemoryAssignments(uid, cfg)
			if err != nil {
				reconcileErrors = append(reconcileErrors, &SystemdResourceReconciliationError{UID: uid, Resource: systemdunit.ResourceMemory, Step: "plan", Err: err})
			} else if err := m.applySystemdResource(ctx, uid, identity, systemdunit.ResourceMemory, assignments, cfg.DisableSwap); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
		}
		if ioDesired[uid] {
			if err := m.applySystemdResource(ctx, uid, identity, systemdunit.ResourceIO, ioAssignments, false); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
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
	values := []struct {
		name  systemdunit.PropertyName
		value uint64
	}{
		{name: systemdunit.PropertyMemoryHigh, value: highBytes},
		{name: systemdunit.PropertyMemoryMax, value: maxBytes},
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

func (m *Manager) applySystemdResource(ctx context.Context, uid int, identity systemdunit.UnitIdentity, resource systemdunit.ResourceKind, assignments []systemdunit.PropertyAssignment, swap bool) error {
	authority, err := m.systemdUnits.CheckResourceAuthority(ctx, identity, uint32(uid), resource, assignments)
	m.recordSystemdResourceAuthority(uid, resource, authority)
	if err != nil {
		return &SystemdResourceReconciliationError{UID: uid, Resource: resource, Step: "authority", Err: err}
	}
	if _, err := m.systemdUnits.Apply(ctx, identity, assignments); err != nil {
		return &SystemdResourceReconciliationError{UID: uid, Resource: resource, Step: "apply", Err: err}
	}
	m.mu.Lock()
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
	m.mu.Unlock()
	return nil
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
