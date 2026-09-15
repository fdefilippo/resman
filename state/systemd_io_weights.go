package state

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/ioweights"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

// IODeviceWeightClassifier is the read-only capability boundary.
type IODeviceWeightClassifier interface {
	Classify(context.Context, string) (systemdunit.IODeviceWeightCapabilitySnapshot, error)
	Confirm(context.Context, systemdunit.IODeviceWeightCapabilitySnapshot) (systemdunit.IODeviceWeightCapabilitySnapshot, error)
}

// SystemdIODeviceWeightAdapter is the owned systemd mutation boundary.
type SystemdIODeviceWeightAdapter interface {
	SystemdCPUUnitAdapter
	ProbeIODeviceWeights(context.Context, []systemdunit.IODeviceWeightProbeTarget) error
}

// IODeviceWeightActivationState is the bounded public lifecycle vocabulary.
type IODeviceWeightActivationState string

const (
	IODeviceWeightDisabled             IODeviceWeightActivationState = "disabled"
	IODeviceWeightRequestedPending     IODeviceWeightActivationState = "requested_pending"
	IODeviceWeightRefusedObservation   IODeviceWeightActivationState = "refused_observation"
	IODeviceWeightRefusedIntervention  IODeviceWeightActivationState = "refused_intervention"
	IODeviceWeightProbeCandidateState  IODeviceWeightActivationState = "probe_candidate"
	IODeviceWeightFunctionallyAccepted IODeviceWeightActivationState = "functionally_accepted"
)

// IODeviceWeightStatus is one lock-safe capability and policy snapshot.
type IODeviceWeightStatus struct {
	State                  IODeviceWeightActivationState
	Reason                 string
	Selector               string
	ClassificationAttempts uint64
	ProbeAttempts          uint64
	Programmed             bool
	ReadBack               bool
	EffectQualified        bool
	PartialUsers           int
	ObservedDelivery       string
}

// IODeviceWeightAttemptResult tells the post-READY scheduler whether another
// bounded capability pass is required and whether a control cycle should run.
type IODeviceWeightAttemptResult struct {
	Status        IODeviceWeightStatus
	Retry         bool
	ActivateCycle bool
}

// WithSystemdIODeviceWeights installs the immutable policy, classifier and
// owned adapter without turning the feature into a synchronous startup gate.
func WithSystemdIODeviceWeights(adapter SystemdIODeviceWeightAdapter, classifier IODeviceWeightClassifier, policy ioweights.PolicySnapshot) ManagerOption {
	return func(m *Manager) error {
		if adapter == nil || classifier == nil {
			return fmt.Errorf("weighted I/O requires an owned adapter and read-only classifier")
		}
		m.systemdIOWeights = adapter
		m.ioWeightClassifier = classifier
		m.ioWeightPolicy = policy
		m.ioWeightStatus = IODeviceWeightStatus{State: IODeviceWeightDisabled, ObservedDelivery: "not_measured"}
		for _, identity := range adapter.OwnedUnits() {
			uid, userSlice := systemdUserSliceUID(identity.Name)
			if !userSlice {
				continue
			}
			for _, lease := range adapter.Leases(identity) {
				resource, resourceProperty := lease.Property.Resource()
				if lease.Active() && resourceProperty && resource == systemdunit.ResourceIOWeight {
					m.ioWeightUnits[uid] = identity
					m.ioWeightStatus.Programmed = true
					break
				}
			}
		}
		return nil
	}
}

// PublishIODeviceWeightPolicy replaces the immutable policy for the next
// configuration generation. Source loading and NSS I/O happen before this call.
func (m *Manager) PublishIODeviceWeightPolicy(policy ioweights.PolicySnapshot) {
	m.mu.Lock()
	m.ioWeightPolicy = policy
	m.ioWeightCapability = systemdunit.IODeviceWeightCapabilitySnapshot{}
	m.ioWeightStatus.Programmed = false
	m.ioWeightStatus.ReadBack = false
	m.ioWeightUnavailableCycles = 0
	m.mu.Unlock()
}

// CurrentIODeviceWeightPolicy returns the immutable current policy snapshot.
func (m *Manager) CurrentIODeviceWeightPolicy() ioweights.PolicySnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ioWeightPolicy
}

// GetIODeviceWeightStatus returns a detached lifecycle snapshot.
func (m *Manager) GetIODeviceWeightStatus() IODeviceWeightStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ioWeightStatus
}

// AttemptIODeviceWeightCapability performs one serialized post-READY
// classification or owned probe for the current configuration generation.
func (m *Manager) AttemptIODeviceWeightCapability(ctx context.Context) IODeviceWeightAttemptResult {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()
	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

	cfg := m.GetConfig()
	selector := cfg.GetIOWeightDevices()
	if selector == "" {
		err := m.restoreAllSystemdIODeviceWeights(ctx)
		state := IODeviceWeightDisabled
		reason := ""
		if err != nil {
			state, reason = IODeviceWeightRefusedIntervention, "unsafe_restore"
		}
		m.publishIODeviceWeightCapability(state, reason, selector, systemdunit.IODeviceWeightCapabilitySnapshot{}, err != nil, err != nil)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus()}
	}
	if m.systemdIOWeights == nil || m.ioWeightClassifier == nil || m.enforcementStatus.Mode != cgroup.EnforcementModeSystemdNative {
		m.publishIODeviceWeightCapability(IODeviceWeightRequestedPending, "adapter_unavailable", selector, systemdunit.IODeviceWeightCapabilitySnapshot{}, true, false)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true}
	}

	probeCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.ioWeightProbeToken++
	token := m.ioWeightProbeToken
	m.ioWeightProbeCancel = cancel
	previous := m.ioWeightCapability
	previousAccepted := m.ioWeightStatus.State == IODeviceWeightFunctionallyAccepted && m.ioWeightStatus.Selector == selector
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		if m.ioWeightProbeToken == token {
			m.ioWeightProbeCancel = nil
		}
		m.mu.Unlock()
	}()

	if previousAccepted {
		m.incrementIODeviceWeightClassificationAttempts()
		confirmed, err := m.ioWeightClassifier.Confirm(probeCtx, previous)
		if err == nil {
			m.publishIODeviceWeightCapability(IODeviceWeightFunctionallyAccepted, "", selector, confirmed, true, true)
			return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true}
		}
		if probeCtx.Err() != nil {
			m.publishIODeviceWeightCapability(IODeviceWeightRequestedPending, "cancelled_generation", selector, systemdunit.IODeviceWeightCapabilitySnapshot{}, false, false)
			return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true}
		}
	}

	m.incrementIODeviceWeightClassificationAttempts()
	snapshot, err := m.ioWeightClassifier.Classify(probeCtx, selector)
	if err != nil {
		m.publishIODeviceWeightCapability(IODeviceWeightRefusedIntervention, "invalid_classifier_input", selector, systemdunit.IODeviceWeightCapabilitySnapshot{}, false, false)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus()}
	}
	switch snapshot.Outcome() {
	case systemdunit.IODeviceWeightProbeCandidate:
		m.publishIODeviceWeightCapability(IODeviceWeightProbeCandidateState, string(snapshot.Reason()), selector, snapshot, false, false)
	case systemdunit.IODeviceWeightMechanismInactive, systemdunit.IODeviceWeightEvidenceUnavailable:
		m.publishIODeviceWeightCapability(IODeviceWeightRequestedPending, string(snapshot.Reason()), selector, snapshot, false, false)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true}
	default:
		m.publishIODeviceWeightCapability(IODeviceWeightRefusedObservation, string(snapshot.Reason()), selector, snapshot, false, false)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true}
	}

	m.incrementIODeviceWeightProbeAttempts()
	if err := m.systemdIOWeights.ProbeIODeviceWeights(probeCtx, snapshot.ProbeTargets()); err != nil {
		if probeCtx.Err() != nil || ioWeightProbeRetryable(err) {
			m.publishIODeviceWeightCapability(IODeviceWeightRequestedPending, "probe_unavailable", selector, snapshot, false, false)
			return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true}
		}
		m.publishIODeviceWeightCapability(IODeviceWeightRefusedIntervention, ioWeightProbeReason(err), selector, snapshot, false, false)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus()}
	}
	confirmed, err := m.ioWeightClassifier.Confirm(probeCtx, snapshot)
	if err != nil {
		m.publishIODeviceWeightCapability(IODeviceWeightRefusedObservation, "capability_changed", selector, confirmed, false, false)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true}
	}
	m.publishIODeviceWeightCapability(IODeviceWeightFunctionallyAccepted, "", selector, confirmed, false, false)
	return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true, ActivateCycle: true}
}

func ioWeightProbeRetryable(err error) bool {
	var adapterErr *systemdunit.AdapterError
	if !errors.As(err, &adapterErr) {
		return true
	}
	switch adapterErr.Reason {
	case systemdunit.ReasonExternalConflict, systemdunit.ReasonLeaseStore,
		systemdunit.ReasonReadbackMismatch, systemdunit.ReasonKernelVerification,
		systemdunit.ReasonUnitFileVerification, systemdunit.ReasonCapabilityProbe:
		return false
	default:
		return true
	}
}

func ioWeightProbeReason(err error) string {
	var adapterErr *systemdunit.AdapterError
	if errors.As(err, &adapterErr) {
		return string(adapterErr.Reason)
	}
	return "probe_failed"
}

func (m *Manager) incrementIODeviceWeightClassificationAttempts() {
	m.mu.Lock()
	m.ioWeightStatus.ClassificationAttempts++
	m.mu.Unlock()
}

func (m *Manager) incrementIODeviceWeightProbeAttempts() {
	m.mu.Lock()
	m.ioWeightStatus.ProbeAttempts++
	m.mu.Unlock()
}

func (m *Manager) publishIODeviceWeightCapability(state IODeviceWeightActivationState, reason, selector string, snapshot systemdunit.IODeviceWeightCapabilitySnapshot, preserveProgrammed, preserveReadback bool) {
	m.mu.Lock()
	status := m.ioWeightStatus
	status.State, status.Reason, status.Selector = state, reason, selector
	if !preserveProgrammed {
		status.Programmed = false
	}
	if !preserveReadback {
		status.ReadBack = false
	}
	status.ObservedDelivery = "not_measured"
	m.ioWeightStatus = status
	m.ioWeightCapability = snapshot
	m.mu.Unlock()
}

func (m *Manager) stageReconcileIODeviceWeights(run *controlCycleContext) error {
	if m.enforcementStatus.Mode != cgroup.EnforcementModeSystemdNative || run == nil || run.cfg == nil {
		return nil
	}
	if run.cfg.GetIOWeightDevices() == "" {
		return m.restoreAllSystemdIODeviceWeights(run.ctx)
	}
	m.mu.RLock()
	accepted := m.ioWeightStatus.State == IODeviceWeightFunctionallyAccepted && m.ioWeightStatus.Selector == run.cfg.GetIOWeightDevices()
	m.mu.RUnlock()
	if !accepted {
		return nil
	}
	if err := m.reconcileSystemdIODeviceWeights(run.ctx, run.metrics, run.cfg); err != nil {
		return fmt.Errorf("reconcile weighted I/O: %w", err)
	}
	return nil
}

func (m *Manager) reconcileSystemdIODeviceWeights(ctx context.Context, sample *SystemMetrics, cfg *config.Config) error {
	m.mu.RLock()
	snapshot, policy := m.ioWeightCapability, m.ioWeightPolicy
	m.mu.RUnlock()
	confirmed, err := m.ioWeightClassifier.Confirm(ctx, snapshot)
	if err != nil {
		return m.deferOrReleaseIODeviceWeights(ctx, "capability_changed", err)
	}
	topology, err := m.systemdIOWeights.Discover(ctx)
	if err != nil {
		return m.deferOrReleaseIODeviceWeights(ctx, "topology_unavailable", err)
	}
	if sample == nil || sample.systemdAuthorityInventory == nil || sample.systemdAuthorityInventory.SampleEpochID() != sample.Timestamp.UnixNano() {
		return m.deferOrReleaseIODeviceWeights(ctx, "authority_unavailable", fmt.Errorf("current sample has no exact authority inventory"))
	}
	participants := make([]ioweights.ActiveUserSlice, 0, len(topology.Users))
	identities := make(map[int]systemdunit.UnitIdentity, len(topology.Users))
	partial := make(map[int]bool)
	for _, user := range topology.Users {
		observation, ok := sample.systemdAuthorityInventory.Observation(user.UID, user.Unit.Identity)
		if !ok {
			continue
		}
		uid := int(user.UID)
		participants = append(participants, ioweights.ActiveUserSlice{UID: uid, Eligible: cfg.EvaluateUserEligibility(m.getUsername(uid)).EligibleForIO})
		identities[uid] = user.Unit.Identity
		partial[uid] = !observation.CPUCoverage
	}
	plan, err := ioweights.Plan(policy, participants)
	if err != nil {
		return err
	}
	if err := m.systemdIOWeights.ConfirmTopology(ctx, topology); err != nil {
		return m.deferOrReleaseIODeviceWeights(ctx, "topology_changed", err)
	}
	targets := confirmed.ProbeTargets()
	applied := make([]struct {
		uid         int
		identity    systemdunit.UnitIdentity
		assignments []systemdunit.PropertyAssignment
	}, 0, len(plan))
	for _, slice := range plan {
		requests := make([]systemdunit.IODeviceWeightRequest, 0, len(targets))
		for _, target := range targets {
			requests = append(requests, systemdunit.IODeviceWeightRequest{Path: target.Identity.DeviceNode, Weight: slice.Weight().Value(), Mechanism: target.Mechanism})
		}
		assignment, assignmentErr := systemdunit.NewIODeviceWeightAssignment(requests)
		if assignmentErr != nil {
			return assignmentErr
		}
		identity := identities[slice.UID()]
		if _, applyErr := m.systemdIOWeights.Apply(ctx, identity, []systemdunit.PropertyAssignment{assignment}); applyErr != nil {
			if !ioWeightProbeRetryable(applyErr) {
				m.publishIODeviceWeightCapability(IODeviceWeightRefusedIntervention, ioWeightProbeReason(applyErr), cfg.GetIOWeightDevices(), confirmed, true, true)
			}
			return applyErr
		}
		applied = append(applied, struct {
			uid         int
			identity    systemdunit.UnitIdentity
			assignments []systemdunit.PropertyAssignment
		}{slice.UID(), identity, []systemdunit.PropertyAssignment{assignment}})
	}
	for _, item := range applied {
		if _, err := m.systemdIOWeights.ConfirmApplied(ctx, item.identity, item.assignments); err != nil {
			return err
		}
	}
	if err := m.systemdIOWeights.ConfirmTopology(ctx, topology); err != nil {
		return err
	}
	current := make(map[int]systemdunit.UnitIdentity, len(applied))
	partialCount := 0
	for _, item := range applied {
		current[item.uid] = item.identity
		if partial[item.uid] {
			partialCount++
		}
	}
	if err := m.restoreStaleSystemdIODeviceWeights(ctx, current); err != nil {
		return err
	}
	m.mu.Lock()
	m.ioWeightCapability = confirmed
	m.ioWeightUnits = current
	m.ioWeightUnavailableCycles = 0
	m.ioWeightStatus.State = IODeviceWeightFunctionallyAccepted
	m.ioWeightStatus.Programmed = len(applied) > 0
	m.ioWeightStatus.ReadBack = len(applied) > 0
	m.ioWeightStatus.PartialUsers = partialCount
	m.mu.Unlock()
	return nil
}

func (m *Manager) deferOrReleaseIODeviceWeights(ctx context.Context, reason string, cause error) error {
	m.mu.Lock()
	m.ioWeightUnavailableCycles++
	cycles := m.ioWeightUnavailableCycles
	m.ioWeightStatus.State = IODeviceWeightRequestedPending
	m.ioWeightStatus.Reason = reason
	m.ioWeightStatus.ReadBack = false
	m.mu.Unlock()
	if cycles <= 1 {
		return cause
	}
	return errors.Join(cause, m.restoreAllSystemdIODeviceWeights(ctx))
}

func (m *Manager) restoreStaleSystemdIODeviceWeights(ctx context.Context, current map[int]systemdunit.UnitIdentity) error {
	m.mu.RLock()
	tracked := make(map[int]systemdunit.UnitIdentity, len(m.ioWeightUnits))
	for uid, identity := range m.ioWeightUnits {
		tracked[uid] = identity
	}
	m.mu.RUnlock()
	var restoreErrors []error
	for uid, identity := range tracked {
		if current[uid] == identity {
			continue
		}
		if _, err := m.systemdIOWeights.RestoreProperties(ctx, identity, []systemdunit.PropertyName{systemdunit.PropertyIODeviceWeight}); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore stale weighted I/O for %s: %w", identity.Name, err))
		}
	}
	return errors.Join(restoreErrors...)
}

func (m *Manager) restoreAllSystemdIODeviceWeights(ctx context.Context) error {
	if m.systemdIOWeights == nil {
		return nil
	}
	if err := m.systemdIOWeights.ReconcileOwned(ctx); err != nil {
		return fmt.Errorf("reconcile weighted-I/O leases before restore: %w", err)
	}
	owned := m.systemdIOWeights.OwnedUnits()
	sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
	var restoreErrors []error
	for _, identity := range owned {
		hasWeight := false
		for _, lease := range m.systemdIOWeights.Leases(identity) {
			resource, ok := lease.Property.Resource()
			if lease.Active() && ok && resource == systemdunit.ResourceIOWeight {
				hasWeight = true
				break
			}
		}
		if !hasWeight {
			continue
		}
		result, err := m.systemdIOWeights.RestoreProperties(ctx, identity, []systemdunit.PropertyName{systemdunit.PropertyIODeviceWeight})
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore weighted I/O for %s: %w", identity.Name, err))
			continue
		}
		if len(result.Conflicts) != 0 {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore weighted I/O for %s preserved %d external conflicts", identity.Name, len(result.Conflicts)))
		}
	}
	if err := errors.Join(restoreErrors...); err != nil {
		return err
	}
	m.mu.Lock()
	m.ioWeightUnits = make(map[int]systemdunit.UnitIdentity)
	m.ioWeightStatus.Programmed = false
	m.ioWeightStatus.ReadBack = false
	m.ioWeightStatus.PartialUsers = 0
	m.ioWeightUnavailableCycles = 0
	m.mu.Unlock()
	return nil
}
