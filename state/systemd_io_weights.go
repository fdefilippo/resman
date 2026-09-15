package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

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
type IODeviceWeightActivationState = ioweights.ActivationState

const (
	IODeviceWeightDisabled             = ioweights.ActivationDisabled
	IODeviceWeightRequestedPending     = ioweights.ActivationRequestedPending
	IODeviceWeightRefusedObservation   = ioweights.ActivationRefusedObservation
	IODeviceWeightRefusedIntervention  = ioweights.ActivationRefusedIntervention
	IODeviceWeightProbeCandidateState  = ioweights.ActivationProbeCandidate
	IODeviceWeightFunctionallyAccepted = ioweights.ActivationFunctionallyAccepted
	IODeviceWeightReleasePending       = ioweights.ActivationReleasePending
)

// IODeviceWeightVerificationState distinguishes absence of an attempt from a
// failed, confirmed, or explicitly released mutation.
type IODeviceWeightVerificationState = ioweights.VerificationState

const (
	IODeviceWeightNotAttempted = ioweights.VerificationNotAttempted
	IODeviceWeightConfirmed    = ioweights.VerificationConfirmed
	IODeviceWeightFailed       = ioweights.VerificationFailed
	IODeviceWeightReleased     = ioweights.VerificationReleased
)

// IODeviceWeightValueStatus records the public and systemd values plus the
// expected kernel-domain value whose exact presence was confirmed for one
// sibling slice and selected device. KernelValue is not a raw file capture.
type IODeviceWeightValueStatus struct {
	UID            int                         `json:"uid"`
	Class          string                      `json:"class"`
	Device         string                      `json:"device"`
	Mechanism      string                      `json:"mechanism"`
	Coverage       ioweights.AuthorityCoverage `json:"coverage"`
	RequestedValue uint64                      `json:"requested_value"`
	SystemdValue   uint64                      `json:"systemd_value,omitempty"`
	KernelValue    uint64                      `json:"kernel_value,omitempty"`
	NominalShare   float64                     `json:"nominal_share"`
	Programmed     bool                        `json:"programmed"`
	ReadBack       bool                        `json:"read_back"`
}

func ioDeviceWeightValuesJSON(values []IODeviceWeightValueStatus) string {
	if len(values) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(fmt.Sprintf("encode typed weighted-I/O values: %v", err))
	}
	return string(encoded)
}

func optionalIODeviceWeightTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	result := value
	return &result
}

// IODeviceWeightStatus is one lock-safe capability and policy snapshot.
type IODeviceWeightStatus struct {
	State                  IODeviceWeightActivationState
	Reason                 string
	Selector               string
	Mechanism              ioweights.MechanismState
	ClassificationAttempts uint64
	ProbeAttempts          uint64
	Programmed             bool
	ProgrammedState        IODeviceWeightVerificationState
	ReadBack               bool
	ReadBackState          IODeviceWeightVerificationState
	EffectQualified        bool
	AuthorityCoverage      ioweights.AuthorityCoverage
	CompleteUsers          int
	PartialUsers           int
	UnavailableUsers       int
	SiblingSlices          int
	TotalPoints            uint64
	RequestedAt            time.Time
	NextRetryAt            time.Time
	Values                 []IODeviceWeightValueStatus
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
		m.ioWeightStatus = newDisabledIODeviceWeightStatus()
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
					m.ioWeightStatus.ProgrammedState = IODeviceWeightConfirmed
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
	equal := m.ioWeightPolicy.Equal(policy)
	enabled := m.cfg != nil && m.cfg.GetIOWeightDevices() != ""
	if equal && m.ioWeightStatus.State != IODeviceWeightRefusedIntervention {
		m.mu.Unlock()
		return
	}
	m.ioWeightPolicy = policy
	if !equal && enabled {
		m.ioWeightPolicyReconcilePending = true
		m.ioWeightStatus.RequestedAt = time.Now()
		if m.ioWeightStatus.State != IODeviceWeightFunctionallyAccepted {
			m.ioWeightStatus.State = IODeviceWeightRequestedPending
			m.ioWeightStatus.Reason = "configuration_changed"
		}
	}
	if m.ioWeightStatus.State == IODeviceWeightRefusedIntervention {
		m.ioWeightCapability = systemdunit.IODeviceWeightCapabilitySnapshot{}
		m.ioWeightStatus.State = IODeviceWeightReleasePending
		if enabled {
			m.ioWeightStatus.State = IODeviceWeightRequestedPending
		}
		m.ioWeightStatus.Reason = "configuration_changed"
		m.ioWeightStatus.ReadBack = false
		m.ioWeightCapabilityUnavailableAttempts = 0
		m.ioWeightControlUnavailableCycles = 0
	}
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
	status := m.ioWeightStatus
	status.Values = append([]IODeviceWeightValueStatus(nil), status.Values...)
	return status
}

func newDisabledIODeviceWeightStatus() IODeviceWeightStatus {
	return IODeviceWeightStatus{
		State: IODeviceWeightDisabled, ProgrammedState: IODeviceWeightNotAttempted,
		ReadBackState: IODeviceWeightNotAttempted, AuthorityCoverage: ioweights.AuthorityUnavailable,
		Mechanism: ioweights.MechanismNone, ObservedDelivery: ioweights.DeliveryNotMeasured,
	}
}

// SetIODeviceWeightNextRetry publishes scheduler-owned retry timing without
// exposing the timer implementation to the state manager.
func (m *Manager) SetIODeviceWeightNextRetry(delay time.Duration) {
	m.mu.Lock()
	if delay <= 0 {
		m.ioWeightStatus.NextRetryAt = time.Time{}
	} else {
		m.ioWeightStatus.NextRetryAt = time.Now().Add(delay)
	}
	m.mu.Unlock()
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
	m.mu.Lock()
	if selector != "" && m.ioWeightStatus.RequestedAt.IsZero() {
		m.ioWeightStatus.RequestedAt = time.Now()
	}
	m.mu.Unlock()
	m.mu.RLock()
	interventionStopped := m.ioWeightStatus.State == IODeviceWeightRefusedIntervention && m.ioWeightStatus.Selector == selector
	m.mu.RUnlock()
	if interventionStopped {
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus()}
	}
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
		return m.publishIODeviceWeightCapabilityFailure(ctx, IODeviceWeightRequestedPending, "adapter_unavailable", selector, systemdunit.IODeviceWeightCapabilitySnapshot{}, true)
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
			m.mu.Lock()
			activateCycle := m.ioWeightPolicyReconcilePending
			m.ioWeightPolicyReconcilePending = false
			m.mu.Unlock()
			m.publishIODeviceWeightCapability(IODeviceWeightFunctionallyAccepted, "", selector, confirmed, true, true)
			return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true, ActivateCycle: activateCycle}
		}
		if probeCtx.Err() != nil {
			return m.publishIODeviceWeightCapabilityFailure(ctx, IODeviceWeightRequestedPending, "cancelled_generation", selector, systemdunit.IODeviceWeightCapabilitySnapshot{}, true)
		}
	}

	m.incrementIODeviceWeightClassificationAttempts()
	snapshot, err := m.ioWeightClassifier.Classify(probeCtx, selector)
	if err != nil {
		if probeCtx.Err() != nil {
			return m.publishIODeviceWeightCapabilityFailure(ctx, IODeviceWeightRequestedPending, "cancelled_generation", selector, systemdunit.IODeviceWeightCapabilitySnapshot{}, true)
		}
		_ = m.releaseFailedIODeviceWeightPlan(ctx, IODeviceWeightRefusedIntervention, "invalid_classifier_input", selector, systemdunit.IODeviceWeightCapabilitySnapshot{}, err)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus()}
	}
	switch snapshot.Outcome() {
	case systemdunit.IODeviceWeightProbeCandidate:
		m.publishIODeviceWeightCapability(IODeviceWeightProbeCandidateState, string(snapshot.Reason()), selector, snapshot, true, false)
	case systemdunit.IODeviceWeightMechanismInactive, systemdunit.IODeviceWeightEvidenceUnavailable:
		return m.publishIODeviceWeightCapabilityFailure(ctx, IODeviceWeightRequestedPending, string(snapshot.Reason()), selector, snapshot, true)
	default:
		return m.publishIODeviceWeightCapabilityFailure(ctx, IODeviceWeightRefusedObservation, string(snapshot.Reason()), selector, snapshot, true)
	}

	m.incrementIODeviceWeightProbeAttempts()
	if err := m.systemdIOWeights.ProbeIODeviceWeights(probeCtx, snapshot.ProbeTargets()); err != nil {
		if probeCtx.Err() != nil || ioWeightProbeRetryable(err) {
			return m.publishIODeviceWeightCapabilityFailure(ctx, IODeviceWeightRequestedPending, string(systemdunit.IODeviceWeightReasonEvidenceUnavailable), selector, snapshot, true)
		}
		_ = m.releaseFailedIODeviceWeightPlan(ctx, IODeviceWeightRefusedIntervention, ioWeightProbeReason(err), selector, snapshot, err)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus()}
	}
	confirmed, err := m.ioWeightClassifier.Confirm(probeCtx, snapshot)
	if err != nil {
		state, reason := ioWeightCapabilityLoss(err)
		return m.publishIODeviceWeightCapabilityFailure(ctx, state, reason, selector, confirmed, true)
	}
	m.publishIODeviceWeightCapability(IODeviceWeightFunctionallyAccepted, "", selector, confirmed, true, false)
	m.mu.Lock()
	m.ioWeightPolicyReconcilePending = false
	m.mu.Unlock()
	return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: true, ActivateCycle: true}
}

func ioWeightProbeRetryable(err error) bool {
	if ioWeightProbeHasRetryableTransportCause(err) {
		return true
	}
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

func ioWeightProbeHasRetryableTransportCause(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if adapterErr, ok := err.(*systemdunit.AdapterError); ok {
		if adapterErr.Reason == systemdunit.ReasonBusUnavailable || adapterErr.Reason == systemdunit.ReasonTimeout {
			return true
		}
		return ioWeightProbeHasRetryableTransportCause(adapterErr.Err)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range joined.Unwrap() {
			if ioWeightProbeHasRetryableTransportCause(nested) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return ioWeightProbeHasRetryableTransportCause(wrapped.Unwrap())
	}
	return false
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
	if m.prometheusExporter != nil {
		m.prometheusExporter.IncrementIODeviceWeightClassification()
	}
}

func (m *Manager) incrementIODeviceWeightProbeAttempts() {
	m.mu.Lock()
	m.ioWeightStatus.ProbeAttempts++
	m.mu.Unlock()
	if m.prometheusExporter != nil {
		m.prometheusExporter.IncrementIODeviceWeightProbe()
	}
}

func (m *Manager) publishIODeviceWeightCapability(state IODeviceWeightActivationState, reason, selector string, snapshot systemdunit.IODeviceWeightCapabilitySnapshot, preserveProgrammed, preserveReadback bool) {
	m.mu.Lock()
	status := m.ioWeightStatus
	changed := status.State != state || status.Reason != reason
	status.State, status.Reason, status.Selector = state, reason, selector
	mechanism := ioDeviceWeightMechanismSummary(snapshot)
	if mechanism != "none" || !preserveProgrammed {
		status.Mechanism = mechanism
	}
	if !preserveProgrammed {
		status.Programmed = false
		status.ProgrammedState = IODeviceWeightNotAttempted
		status.Values = nil
	}
	if !preserveReadback {
		status.ReadBack = false
		if status.Programmed {
			status.ReadBackState = IODeviceWeightFailed
		} else {
			status.ReadBackState = IODeviceWeightNotAttempted
		}
	}
	status.ObservedDelivery = ioweights.DeliveryNotMeasured
	m.ioWeightCapability = snapshot
	if state == IODeviceWeightFunctionallyAccepted {
		m.ioWeightCapabilityUnavailableAttempts = 0
		m.ioWeightControlUnavailableCycles = 0
	}
	if state == IODeviceWeightDisabled {
		status.RequestedAt = time.Time{}
		status.NextRetryAt = time.Time{}
	}
	m.ioWeightStatus = status
	m.mu.Unlock()
	if changed && m.logger != nil {
		m.logger.Info("Weighted I/O lifecycle changed", "state", state, "reason", reason, "programmed", status.Programmed, "read_back", status.ReadBack)
	}
}

func ioDeviceWeightMechanismSummary(snapshot systemdunit.IODeviceWeightCapabilitySnapshot) ioweights.MechanismState {
	mechanisms := make(map[string]struct{})
	for _, device := range snapshot.Devices() {
		if device.Mechanism != "" {
			mechanisms[string(device.Mechanism)] = struct{}{}
		}
	}
	if len(mechanisms) == 0 {
		return ioweights.MechanismNone
	}
	if len(mechanisms) > 1 {
		return ioweights.MechanismMixed
	}
	for mechanism := range mechanisms {
		return ioweights.MechanismState(mechanism)
	}
	return ioweights.MechanismNone
}

func (m *Manager) publishIODeviceWeightCapabilityFailure(ctx context.Context, state IODeviceWeightActivationState, reason, selector string, snapshot systemdunit.IODeviceWeightCapabilitySnapshot, retry bool) IODeviceWeightAttemptResult {
	m.mu.Lock()
	programmed := m.ioWeightStatus.Programmed
	graceEligible := reason == string(systemdunit.IODeviceWeightReasonEvidenceUnavailable)
	if programmed && graceEligible {
		m.ioWeightCapabilityUnavailableAttempts++
	} else if !graceEligible {
		m.ioWeightCapabilityUnavailableAttempts = 0
	}
	cycles := m.ioWeightCapabilityUnavailableAttempts
	m.mu.Unlock()
	if programmed && (!graceEligible || cycles > 1) {
		if err := m.restoreAllSystemdIODeviceWeights(ctx); err != nil {
			m.publishIODeviceWeightCapability(IODeviceWeightRefusedIntervention, "unsafe_restore", selector, snapshot, true, false)
			return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus()}
		}
		m.publishIODeviceWeightCapability(state, reason, selector, snapshot, false, false)
		return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: retry}
	}
	m.publishIODeviceWeightCapability(state, reason, selector, snapshot, true, false)
	return IODeviceWeightAttemptResult{Status: m.GetIODeviceWeightStatus(), Retry: retry}
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
		state, reason := ioWeightCapabilityLoss(err)
		return m.deferOrReleaseIODeviceWeights(ctx, state, reason, err)
	}
	topology, err := m.systemdIOWeights.Discover(ctx)
	if err != nil {
		return m.deferOrReleaseIODeviceWeights(ctx, IODeviceWeightRequestedPending, "topology_unavailable", err)
	}
	if sample == nil || sample.systemdAuthorityInventory == nil || sample.systemdAuthorityInventory.SampleEpochID() != sample.Timestamp.UnixNano() {
		return m.deferOrReleaseIODeviceWeights(ctx, IODeviceWeightRequestedPending, "authority_unavailable", fmt.Errorf("current sample has no exact authority inventory"))
	}
	participants := make([]ioweights.ActiveUserSlice, 0, len(topology.Users))
	identities := make(map[int]systemdunit.UnitIdentity, len(topology.Users))
	coverage := make(map[int]ioweights.AuthorityCoverage, len(topology.Users))
	for _, user := range topology.Users {
		uid := int(user.UID)
		participants = append(participants, ioweights.ActiveUserSlice{UID: uid, Eligible: cfg.EvaluateUserEligibility(m.getUsername(uid)).EligibleForIO})
		identities[uid] = user.Unit.Identity
		observation, ok := sample.systemdAuthorityInventory.Observation(user.UID, user.Unit.Identity)
		if !ok {
			coverage[uid] = ioweights.AuthorityUnavailable
			continue
		}
		coverage[uid] = ioweights.AuthorityComplete
		if !observation.CPUCoverage {
			coverage[uid] = ioweights.AuthorityPartial
		}
	}
	plan, err := ioweights.Plan(policy, participants)
	if err != nil {
		return m.releaseFailedIODeviceWeightPlan(ctx, IODeviceWeightRefusedIntervention, "invalid_policy_plan", cfg.GetIOWeightDevices(), confirmed, err)
	}
	if err := m.systemdIOWeights.ConfirmTopology(ctx, topology); err != nil {
		return m.deferOrReleaseIODeviceWeights(ctx, IODeviceWeightRefusedObservation, "topology_changed", err)
	}
	targets := confirmed.ProbeTargets()
	values := make([]IODeviceWeightValueStatus, 0, len(plan)*len(targets))
	var totalPoints uint64
	completeCount, partialCount, unavailableCount := 0, 0, 0
	for _, slice := range plan {
		totalPoints += slice.Weight().Value()
		switch coverage[slice.UID()] {
		case ioweights.AuthorityComplete:
			completeCount++
		case ioweights.AuthorityPartial:
			partialCount++
		default:
			unavailableCount++
		}
	}
	applied := make([]struct {
		uid         int
		identity    systemdunit.UnitIdentity
		assignments []systemdunit.PropertyAssignment
	}, 0, len(plan))
	for _, slice := range plan {
		if coverage[slice.UID()] == ioweights.AuthorityUnavailable {
			for _, target := range targets {
				values = append(values, IODeviceWeightValueStatus{UID: slice.UID(), Class: string(slice.Class()), Device: target.Identity.Number.String(), Mechanism: string(target.Mechanism), Coverage: ioweights.AuthorityUnavailable, RequestedValue: slice.Weight().Value(), NominalShare: float64(slice.Weight().Value()) / float64(totalPoints)})
			}
			continue
		}
		requests := make([]systemdunit.IODeviceWeightRequest, 0, len(targets))
		for _, target := range targets {
			requests = append(requests, systemdunit.IODeviceWeightRequest{Path: target.Identity.DeviceNode, Weight: slice.Weight().Value(), Mechanism: target.Mechanism})
		}
		assignment, assignmentErr := systemdunit.NewIODeviceWeightAssignment(requests)
		if assignmentErr != nil {
			return m.releaseFailedIODeviceWeightPlan(ctx, IODeviceWeightRefusedIntervention, "invalid_assignment", cfg.GetIOWeightDevices(), confirmed, assignmentErr)
		}
		limitsByPath := make(map[string]uint64, len(targets))
		for _, limit := range assignment.DeviceLimits() {
			limitsByPath[limit.Path] = limit.Value
		}
		for _, target := range targets {
			values = append(values, IODeviceWeightValueStatus{
				UID: slice.UID(), Class: string(slice.Class()), Device: target.Identity.Number.String(), Mechanism: string(target.Mechanism), Coverage: coverage[slice.UID()],
				RequestedValue: slice.Weight().Value(), SystemdValue: limitsByPath[target.Identity.DeviceNode], KernelValue: slice.Weight().Value(), NominalShare: float64(slice.Weight().Value()) / float64(totalPoints), Programmed: true,
			})
		}
		identity := identities[slice.UID()]
		if _, applyErr := m.systemdIOWeights.Apply(ctx, identity, []systemdunit.PropertyAssignment{assignment}); applyErr != nil {
			state := IODeviceWeightRequestedPending
			reason := "apply_unavailable"
			if !ioWeightProbeRetryable(applyErr) {
				state, reason = IODeviceWeightRefusedIntervention, ioWeightProbeReason(applyErr)
			}
			return m.releaseFailedIODeviceWeightPlan(ctx, state, reason, cfg.GetIOWeightDevices(), confirmed, applyErr)
		}
		applied = append(applied, struct {
			uid         int
			identity    systemdunit.UnitIdentity
			assignments []systemdunit.PropertyAssignment
		}{slice.UID(), identity, []systemdunit.PropertyAssignment{assignment}})
	}
	for _, item := range applied {
		if _, err := m.systemdIOWeights.ConfirmApplied(ctx, item.identity, item.assignments); err != nil {
			state := IODeviceWeightRequestedPending
			reason := "readback_unavailable"
			if !ioWeightProbeRetryable(err) {
				state, reason = IODeviceWeightRefusedIntervention, ioWeightProbeReason(err)
			}
			return m.releaseFailedIODeviceWeightPlan(ctx, state, reason, cfg.GetIOWeightDevices(), confirmed, err)
		}
	}
	for index := range values {
		if values[index].Programmed {
			values[index].ReadBack = true
		}
	}
	if err := m.systemdIOWeights.ConfirmTopology(ctx, topology); err != nil {
		return m.releaseFailedIODeviceWeightPlan(ctx, IODeviceWeightRefusedObservation, "topology_changed", cfg.GetIOWeightDevices(), confirmed, err)
	}
	current := make(map[int]systemdunit.UnitIdentity, len(applied))
	for _, item := range applied {
		current[item.uid] = item.identity
	}
	if err := m.restoreStaleSystemdIODeviceWeights(ctx, current); err != nil {
		return m.releaseFailedIODeviceWeightPlan(ctx, IODeviceWeightRefusedIntervention, "external_property_conflict", cfg.GetIOWeightDevices(), confirmed, err)
	}
	m.mu.Lock()
	m.ioWeightCapability = confirmed
	m.ioWeightUnits = current
	m.ioWeightCapabilityUnavailableAttempts = 0
	m.ioWeightControlUnavailableCycles = 0
	m.ioWeightStatus.State = IODeviceWeightFunctionallyAccepted
	m.ioWeightStatus.Programmed = len(applied) > 0
	m.ioWeightStatus.ProgrammedState = IODeviceWeightNotAttempted
	m.ioWeightStatus.ReadBack = len(applied) > 0
	m.ioWeightStatus.ReadBackState = IODeviceWeightNotAttempted
	if len(applied) > 0 {
		m.ioWeightStatus.ProgrammedState = IODeviceWeightConfirmed
		m.ioWeightStatus.ReadBackState = IODeviceWeightConfirmed
	}
	m.ioWeightStatus.Mechanism = ioDeviceWeightMechanismSummary(confirmed)
	m.ioWeightStatus.CompleteUsers = completeCount
	m.ioWeightStatus.PartialUsers = partialCount
	m.ioWeightStatus.UnavailableUsers = unavailableCount
	m.ioWeightStatus.AuthorityCoverage = ioweights.AuthorityComplete
	if partialCount > 0 {
		m.ioWeightStatus.AuthorityCoverage = ioweights.AuthorityPartial
	}
	if unavailableCount > 0 {
		m.ioWeightStatus.AuthorityCoverage = ioweights.AuthorityUnavailable
	}
	m.ioWeightStatus.SiblingSlices = len(plan)
	m.ioWeightStatus.TotalPoints = totalPoints
	m.ioWeightStatus.Values = values
	m.mu.Unlock()
	return nil
}

func ioWeightCapabilityLoss(err error) (IODeviceWeightActivationState, string) {
	var capabilityErr *systemdunit.IODeviceWeightCapabilityError
	if !errors.As(err, &capabilityErr) {
		return IODeviceWeightRequestedPending, "capability_changed"
	}
	switch capabilityErr.Reason {
	case systemdunit.IODeviceWeightReasonEvidenceUnavailable, systemdunit.IODeviceWeightReasonDeviceMissing:
		return IODeviceWeightRequestedPending, string(capabilityErr.Reason)
	default:
		return IODeviceWeightRefusedObservation, string(capabilityErr.Reason)
	}
}

func (m *Manager) deferOrReleaseIODeviceWeights(ctx context.Context, state IODeviceWeightActivationState, reason string, cause error) error {
	m.mu.Lock()
	graceEligible := reason == string(systemdunit.IODeviceWeightReasonEvidenceUnavailable)
	if graceEligible {
		m.ioWeightControlUnavailableCycles++
	} else {
		m.ioWeightControlUnavailableCycles = 0
	}
	cycles := m.ioWeightControlUnavailableCycles
	m.ioWeightStatus.State = state
	m.ioWeightStatus.Reason = reason
	m.ioWeightStatus.ReadBack = false
	m.ioWeightStatus.ReadBackState = IODeviceWeightFailed
	m.mu.Unlock()
	if graceEligible && cycles <= 1 {
		return cause
	}
	if restoreErr := m.restoreAllSystemdIODeviceWeights(ctx); restoreErr != nil {
		m.publishIODeviceWeightCapability(IODeviceWeightRefusedIntervention, "unsafe_restore", m.GetConfig().GetIOWeightDevices(), systemdunit.IODeviceWeightCapabilitySnapshot{}, true, false)
		return errors.Join(cause, restoreErr)
	}
	return cause
}

func (m *Manager) releaseFailedIODeviceWeightPlan(ctx context.Context, state IODeviceWeightActivationState, reason, selector string, snapshot systemdunit.IODeviceWeightCapabilitySnapshot, cause error) error {
	restoreErr := m.restoreAllSystemdIODeviceWeights(ctx)
	if restoreErr != nil {
		m.publishIODeviceWeightCapability(IODeviceWeightRefusedIntervention, "unsafe_restore", selector, snapshot, true, false)
		return errors.Join(cause, restoreErr)
	}
	m.publishIODeviceWeightCapability(state, reason, selector, snapshot, false, false)
	m.mu.Lock()
	m.ioWeightStatus.ProgrammedState = IODeviceWeightFailed
	m.ioWeightStatus.ReadBackState = IODeviceWeightFailed
	m.mu.Unlock()
	return cause
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
		result, err := m.systemdIOWeights.RestoreProperties(ctx, identity, []systemdunit.PropertyName{systemdunit.PropertyIODeviceWeight})
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore stale weighted I/O for %s: %w", identity.Name, err))
		} else if len(result.Conflicts) != 0 {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore stale weighted I/O for %s preserved %d external conflicts", identity.Name, len(result.Conflicts)))
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
	hadProgrammed := m.ioWeightStatus.Programmed
	m.ioWeightStatus.Programmed = false
	if hadProgrammed {
		m.ioWeightStatus.ProgrammedState = IODeviceWeightReleased
	} else {
		m.ioWeightStatus.ProgrammedState = IODeviceWeightNotAttempted
	}
	m.ioWeightStatus.ReadBack = false
	if hadProgrammed {
		m.ioWeightStatus.ReadBackState = IODeviceWeightReleased
	} else {
		m.ioWeightStatus.ReadBackState = IODeviceWeightNotAttempted
	}
	m.ioWeightStatus.PartialUsers = 0
	m.ioWeightStatus.CompleteUsers = 0
	m.ioWeightStatus.UnavailableUsers = 0
	m.ioWeightStatus.AuthorityCoverage = ioweights.AuthorityUnavailable
	m.ioWeightStatus.SiblingSlices = 0
	m.ioWeightStatus.TotalPoints = 0
	m.ioWeightStatus.Values = nil
	m.ioWeightCapabilityUnavailableAttempts = 0
	m.ioWeightControlUnavailableCycles = 0
	m.mu.Unlock()
	return nil
}
