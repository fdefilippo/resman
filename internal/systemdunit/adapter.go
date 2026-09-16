package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fdefilippo/resman/internal/operationgate"
)

const parentUserSlice = "user.slice"

type propertyLeaseState struct {
	lease                   PropertyLease
	baseline                propertyValue
	lastApplied             propertyValue
	previousApplied         propertyValue
	ioWeightTargets         []ioDeviceWeightTarget
	previousIOWeightTargets []ioDeviceWeightTarget
	pendingIOWeightResets   []ioDeviceWeightReset
	uncertain               bool
	newLease                bool
}

type propertyLeaseKey struct {
	identity UnitIdentity
	property PropertyName
}

type unitOverrideLease struct {
	managedPaths        []string
	fingerprints        []unitFileFingerprint
	previousFingerprint []unitFileFingerprint
}

type kernelVerifier interface {
	identity(string) (uint64, error)
	verify(UnitSnapshot, []PropertyAssignment) error
	preflight(UnitSnapshot, []PropertyAssignment) error
	preflightApply(UnitSnapshot, []PropertyAssignment) error
	prepareIODeviceWeightResets(UnitSnapshot, PropertyAssignment, PropertyAssignment) ([]ioDeviceWeightReset, error)
	resetIODeviceWeightOverrides(UnitSnapshot, []ioDeviceWeightReset) error
}

// Adapter is the narrow, runtime-only systemd resource-control boundary.
// The operation gate may span D-Bus, cgroup verification I/O and the guarded
// IODeviceWeight keyed-reset primitive used only after a durable D-Bus reset.
type Adapter struct {
	transport  unitTransport
	verifier   kernelVerifier
	coverage   resourceCoverageInspector
	unitFiles  unitFileInspector
	timeout    time.Duration
	opGate     operationgate.Gate
	leases     map[propertyLeaseKey]propertyLeaseState
	overrides  map[UnitIdentity]unitOverrideLease
	phases     map[string]leasePhase
	store      leaseJournalStore
	generation uint64
	recovery   []LeaseRecoveryOutcome
	blocked    map[string]error
	closed     bool
}

// ManagerVersion returns the running systemd Manager version as diagnostic
// provenance. Weighted-I/O authorization never depends on this value.
func (a *Adapter) ManagerVersion(ctx context.Context) (string, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("manager_version"); err != nil {
		return "", err
	}
	transport, ok := a.transport.(managerDiagnosticTransport)
	if !ok {
		return "", &AdapterError{Reason: ReasonMalformedReply, Operation: "manager_version",
			Err: fmt.Errorf("systemd transport has no Manager diagnostic property reader")}
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	value, err := transport.managerProperty(callCtx, "Version")
	if err != nil {
		return "", classifyTransportError("manager_version", "", err)
	}
	if strings.TrimSpace(value) == "" {
		return "", &AdapterError{Reason: ReasonMalformedReply, Operation: "manager_version",
			Err: fmt.Errorf("systemd Manager.Version is empty")}
	}
	return strings.TrimSpace(value), nil
}

// New opens the authoritative system bus and the guarded cgroup boundary. The
// supplied context bounds startup recovery only; Close owns the connection lifetime.
func New(ctx context.Context, cgroupRoot string, timeout time.Duration, requirements StartupRequirements) (*Adapter, error) {
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	transport, err := openDBusTransport(ctx)
	if err != nil {
		return nil, classifyTransportError("connect", "", err)
	}
	adapter, err := newAdapter(ctx, transport, newCgroupVerifier(cgroupRoot), localUnitFileInspector{}, newFileLeaseJournalStoreForOwner(DefaultLeaseJournalPath, 0), timeout)
	if err != nil {
		transport.close()
		return nil, err
	}
	if err := adapter.requireStartupCapabilities(ctx, requirements); err != nil {
		adapter.Close()
		return nil, err
	}
	return adapter, nil
}

func newAdapter(ctx context.Context, transport unitTransport, verifier kernelVerifier, unitFiles unitFileInspector, store leaseJournalStore, timeout time.Duration) (*Adapter, error) {
	if transport == nil {
		return nil, &AdapterError{Reason: ReasonBusUnavailable, Operation: "construct", Err: fmt.Errorf("systemd transport is required")}
	}
	if verifier == nil {
		return nil, &AdapterError{Reason: ReasonKernelVerification, Operation: "construct", Err: fmt.Errorf("guarded cgroup verifier is required")}
	}
	if unitFiles == nil {
		return nil, &AdapterError{Reason: ReasonUnitFileVerification, Operation: "construct", Err: fmt.Errorf("unit-file inspector is required")}
	}
	if store == nil {
		return nil, &AdapterError{Reason: ReasonLeaseStore, Operation: "construct", Err: fmt.Errorf("lease journal store is required")}
	}
	if timeout <= 0 {
		return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "construct", Err: fmt.Errorf("call timeout must be positive")}
	}
	adapter := &Adapter{
		transport: transport,
		verifier:  verifier,
		coverage:  newProcCoverageInspector(""),
		unitFiles: unitFiles,
		timeout:   timeout,
		leases:    make(map[propertyLeaseKey]propertyLeaseState),
		overrides: make(map[UnitIdentity]unitOverrideLease),
		phases:    make(map[string]leasePhase),
		store:     store,
		blocked:   make(map[string]error),
	}
	if err := adapter.loadAndReconcile(ctx); err != nil {
		return nil, err
	}
	return adapter, nil
}

// CheckResourceAuthority confirms that one complete user workload can be
// governed for a specific resource before the first property mutation.
func (a *Adapter) CheckResourceAuthority(ctx context.Context, identity UnitIdentity, uid uint32, resource ResourceKind, assignments []PropertyAssignment) (ResourceAuthority, error) {
	results, err := a.CheckResourceAuthorities(ctx, []ResourceAuthorityRequest{{Identity: identity, UID: uid, Resource: resource, Assignments: assignments}})
	if err != nil {
		return ResourceAuthority{}, err
	}
	return results[0].Authority, results[0].Err
}

// CheckResourceAuthorities confirms multiple resource plans against one
// immutable /proc observation so cost does not multiply by user or resource.
func (a *Adapter) CheckResourceAuthorities(ctx context.Context, requests []ResourceAuthorityRequest) ([]ResourceAuthorityResult, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("check_resource_authorities"); err != nil {
		return nil, err
	}
	results := make([]ResourceAuthorityResult, len(requests))
	if len(requests) == 0 {
		return results, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	type preparedRequest struct {
		index     int
		request   ResourceAuthorityRequest
		validated []PropertyAssignment
		snapshot  UnitSnapshot
	}
	prepared := make([]preparedRequest, 0, len(requests))
	targets := make([]resourceCoverageTarget, 0, len(requests))
	for index, request := range requests {
		validated, err := validateResourceAssignments(request.Resource, request.Assignments)
		if err != nil {
			results[index].Err = err
			continue
		}
		snapshot, err := a.readUnit(callCtx, request.Identity.Name, request.Identity.ObjectPath)
		if err == nil {
			err = requireSameIdentity("check_resource_authorities", request.Identity, snapshot.Identity)
		}
		if err != nil {
			results[index].Err = err
			continue
		}
		prepared = append(prepared, preparedRequest{index: index, request: request, validated: validated, snapshot: snapshot})
		targets = append(targets, coverageTargetFor(request.UID, snapshot))
	}
	inspections := a.coverage.capture(callCtx, targets, true).resources
	for preparedIndex, item := range prepared {
		authority := inspections[preparedIndex].authority
		authority.Resource = item.request.Resource
		results[item.index].Authority = authority
		if inspections[preparedIndex].err != nil || authority.State != ResourceCoverageComplete {
			results[item.index].Err = &ResourceAuthorityError{UID: item.request.UID, Authority: authority, Err: inspections[preparedIndex].err}
			continue
		}
		if err := a.verifier.preflightApply(item.snapshot, item.validated); err != nil {
			authority = ResourceAuthority{Resource: item.request.Resource, State: ResourceCoverageRefused, Reason: ResourceCoverageControllerMissing}
			results[item.index] = ResourceAuthorityResult{Authority: authority, Err: &ResourceAuthorityError{UID: item.request.UID, Authority: authority, Err: err}}
		}
	}
	return results, nil
}

// CheckCapturedResourceAuthorities confirms resource plans against one frozen
// process-authority inventory while retaining live unit and kernel preflight checks.
func (a *Adapter) CheckCapturedResourceAuthorities(ctx context.Context, inventory ProcessAuthorityInventory, requests []ResourceAuthorityRequest) ([]ResourceAuthorityResult, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("check_captured_resource_authorities"); err != nil {
		return nil, err
	}
	results := make([]ResourceAuthorityResult, len(requests))
	if len(requests) == 0 {
		return results, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	for index, request := range requests {
		validated, err := validateResourceAssignments(request.Resource, request.Assignments)
		if err != nil {
			results[index].Err = err
			continue
		}
		snapshot, err := a.readUnit(callCtx, request.Identity.Name, request.Identity.ObjectPath)
		if err == nil {
			err = requireSameIdentity("check_captured_resource_authorities", request.Identity, snapshot.Identity)
		}
		if err != nil {
			results[index].Err = err
			continue
		}
		if !inventory.HasResourceDetail() {
			authority := ResourceAuthority{Resource: request.Resource, State: ResourceCoverageRefused, Reason: ResourceCoverageInspectionFailed}
			results[index] = ResourceAuthorityResult{Authority: authority, Err: &ResourceAuthorityError{UID: request.UID, Authority: authority, Err: fmt.Errorf("sample inventory has no RAM/I/O authority detail")}}
			continue
		}
		observation, found := inventory.Observation(request.UID, snapshot.Identity)
		if !found {
			authority := ResourceAuthority{Resource: request.Resource, State: ResourceCoverageRefused, Reason: ResourceCoverageTopologyChanged}
			results[index] = ResourceAuthorityResult{Authority: authority, Err: &ResourceAuthorityError{UID: request.UID, Authority: authority, Err: fmt.Errorf("unit is absent from the sample authority inventory")}}
			continue
		}
		authority := observation.ResourceAuthority
		authority.Resource = request.Resource
		results[index].Authority = authority
		if observation.ResourceError != nil || authority.State != ResourceCoverageComplete {
			results[index].Err = &ResourceAuthorityError{UID: request.UID, Authority: authority, Err: observation.ResourceError}
			continue
		}
		if err := a.verifier.preflightApply(snapshot, validated); err != nil {
			authority = ResourceAuthority{Resource: request.Resource, State: ResourceCoverageRefused, Reason: ResourceCoverageControllerMissing}
			results[index] = ResourceAuthorityResult{Authority: authority, Err: &ResourceAuthorityError{UID: request.UID, Authority: authority, Err: err}}
		}
	}
	return results, nil
}

// Close closes the D-Bus connection. Callers must restore owned properties first.
func (a *Adapter) Close() {
	leave := a.opGate.Enter()
	defer leave()
	if a.closed {
		return
	}
	a.closed = true
	a.transport.close()
}

// Discover returns user.slice and every active canonical user-UID.slice from D-Bus.
func (a *Adapter) Discover(ctx context.Context) (TopologySnapshot, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("discover"); err != nil {
		return TopologySnapshot{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	return a.discover(callCtx, "discover")
}

func (a *Adapter) discover(ctx context.Context, operation string) (TopologySnapshot, error) {
	listed, err := a.transport.listUserSlices(ctx)
	if err != nil {
		return TopologySnapshot{}, classifyTransportError(operation, "", err)
	}
	return a.snapshotTopology(ctx, listed)
}

// ConfirmTopology verifies that the complete active user-slice identity set
// still matches a prior discovery. Callers use it immediately before mutation
// and acknowledgement so unit turnover cannot publish a stale plan as complete.
func (a *Adapter) ConfirmTopology(ctx context.Context, expected TopologySnapshot) error {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("confirm_topology"); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	current, err := a.discover(callCtx, "confirm_topology")
	if err != nil {
		return err
	}
	if sameTopologyIdentity(expected, current) {
		return nil
	}
	return &AdapterError{
		Reason:    ReasonTopologyChanged,
		Operation: "confirm_topology",
		Unit:      parentUserSlice,
		Err:       fmt.Errorf("active user-slice identity set changed since discovery"),
	}
}

func sameTopologyIdentity(expected, current TopologySnapshot) bool {
	if expected.Parent.Identity != current.Parent.Identity || len(expected.Users) != len(current.Users) {
		return false
	}
	for index := range expected.Users {
		if expected.Users[index].UID != current.Users[index].UID || expected.Users[index].Unit.Identity != current.Users[index].Unit.Identity {
			return false
		}
	}
	return true
}

func (a *Adapter) snapshotTopology(ctx context.Context, listed []listedUnit) (TopologySnapshot, error) {
	var result TopologySnapshot
	seen := make(map[string]bool, len(listed))
	parentFound := false
	for _, candidate := range listed {
		if isCapabilityProbeUnit(candidate.name) {
			continue
		}
		if seen[candidate.name] {
			return TopologySnapshot{}, malformedReply("discover", candidate.name, "duplicate unit in ListUnitsByPatterns reply")
		}
		seen[candidate.name] = true
		if candidate.loadState != "loaded" || candidate.activeState != "active" {
			return TopologySnapshot{}, malformedReply("discover", candidate.name, fmt.Sprintf("unexpected state load=%q active=%q", candidate.loadState, candidate.activeState))
		}
		if !strings.HasPrefix(candidate.objectPath, "/org/freedesktop/systemd1/unit/") {
			return TopologySnapshot{}, malformedReply("discover", candidate.name, "invalid systemd object path")
		}

		snapshot, err := a.readUnit(ctx, candidate.name, candidate.objectPath)
		if err != nil {
			var adapterErr *AdapterError
			if candidate.name != parentUserSlice && errors.As(err, &adapterErr) && adapterErr.Reason == ReasonUnitMissing {
				// User slices may disappear between ListUnitsByPatterns and the
				// authoritative property read. Treat that turnover as absence from
				// this complete snapshot; the next discovery observes a later set.
				continue
			}
			return TopologySnapshot{}, err
		}
		if candidate.name == parentUserSlice {
			result.Parent = snapshot
			parentFound = true
			continue
		}
		uid, ok := parseUserSliceName(candidate.name)
		if !ok {
			return TopologySnapshot{}, malformedReply("discover", candidate.name, "non-canonical user slice name")
		}
		result.Users = append(result.Users, UserSliceSnapshot{UID: uid, Unit: snapshot})
	}
	if !parentFound {
		return TopologySnapshot{}, &AdapterError{Reason: ReasonUnitMissing, Operation: "discover", Unit: parentUserSlice, Err: fmt.Errorf("active parent slice was not returned by systemd")}
	}
	sort.Slice(result.Users, func(i, j int) bool { return result.Users[i].UID < result.Users[j].UID })
	return result, nil
}

// Apply sets approved properties with runtime=true, reads them back and verifies
// their effective cgroup values before returning the authoritative snapshot.
func (a *Adapter) Apply(ctx context.Context, identity UnitIdentity, assignments []PropertyAssignment) (UnitSnapshot, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("apply"); err != nil {
		return UnitSnapshot{}, err
	}
	if err := a.blocked[identity.Name]; err != nil {
		return UnitSnapshot{}, err
	}
	validated, err := validateAssignments(assignments)
	if err != nil {
		return UnitSnapshot{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	before, err := a.readUnit(callCtx, identity.Name, identity.ObjectPath)
	if err != nil {
		return UnitSnapshot{}, err
	}
	if err := requireSameIdentity("apply", identity, before.Identity); err != nil {
		return UnitSnapshot{}, err
	}
	currentOverride, trackedOverride := a.overrides[identity]
	if err := a.requireManagedUnitFileFootprint("apply", before, currentOverride, trackedOverride); err != nil {
		return UnitSnapshot{}, err
	}
	for _, assignment := range validated {
		if assignment.name != PropertyIODeviceWeight {
			continue
		}
		if err := a.verifier.preflightApply(before, []PropertyAssignment{assignment}); err != nil {
			return UnitSnapshot{}, &AdapterError{Reason: ReasonKernelVerification, Operation: "apply_preflight", Unit: identity.Name, Property: assignment.name, Err: err}
		}
	}
	if trackedOverride && a.phases[identity.Name] == leasePhaseApplied {
		fingerprints, err := a.captureFootprint(before)
		if err != nil {
			return UnitSnapshot{}, err
		}
		if !equalFingerprints(fingerprints, currentOverride.fingerprints) {
			return UnitSnapshot{}, externalRecoveryConflict(identity.Name, "managed unit-file content changed after application")
		}
		if a.propertiesMatch(identity.Name, func(state propertyLeaseState) propertyValue {
			return state.lastApplied
		}, before) && a.appliedAssignmentsMatch(identity, before, validated) {
			// Reconciliation is deliberately read-only when the durable lease,
			// systemd properties, runtime drop-ins, and effective kernel values
			// still agree. This keeps verification periodic without rewriting the
			// unit or fsyncing an identical journal every control cycle.
			if err := a.verifier.verify(before, validated); err != nil {
				return UnitSnapshot{}, &AdapterError{Reason: ReasonKernelVerification, Operation: "apply_readback", Unit: identity.Name, Err: err}
			}
			return before, nil
		}
	}

	staged := make(map[propertyLeaseKey]propertyLeaseState, len(validated))
	for _, assignment := range validated {
		current, ok := before.Properties.propertyValue(assignment.name)
		if !ok {
			return UnitSnapshot{}, malformedReply("apply", identity.Name, fmt.Sprintf("property %s is absent", assignment.name))
		}
		key := propertyLeaseKey{identity: identity, property: assignment.name}
		state, tracked := a.leases[key]
		if tracked {
			if assignment.name == PropertyIODeviceWeight && !ioDeviceWeightTargetMechanismsCompatible(state.ioWeightTargets, assignment.ioDeviceWeightTargets) {
				return UnitSnapshot{}, &AdapterError{Reason: ReasonExternalConflict, Operation: "apply", Unit: identity.Name, Property: assignment.name,
					Err: fmt.Errorf("qualified mechanism changed for an existing device while the property lease is active")}
			}
			owned, resolved := resolveUncertainOwnership(current, state)
			if !owned {
				return UnitSnapshot{}, externalConflict(identity.Name, assignment.name, state.lastApplied, current)
			}
			state = resolved
		} else {
			if assignment.name == PropertyIODeviceWeight && len(current.devices) != 0 {
				return UnitSnapshot{}, &AdapterError{Reason: ReasonExternalConflict, Operation: "apply", Unit: identity.Name, Property: assignment.name,
					Err: fmt.Errorf("cannot acquire IODeviceWeight while an unowned device tuple is present")}
			}
			state = newPropertyLeaseState(assignment.name, current)
			state.newLease = true
		}
		state.previousApplied = state.lastApplied
		state.previousIOWeightTargets = cloneIODeviceWeightTargets(state.ioWeightTargets)
		state.pendingIOWeightResets = nil
		if tracked && assignment.name == PropertyIODeviceWeight {
			previous := PropertyAssignment{name: PropertyIODeviceWeight, value: clonePropertyValue(PropertyIODeviceWeight, state.lastApplied), ioDeviceWeightTargets: cloneIODeviceWeightTargets(state.ioWeightTargets)}
			resets, err := a.verifier.prepareIODeviceWeightResets(before, previous, assignment)
			if err != nil {
				return UnitSnapshot{}, &AdapterError{Reason: ReasonKernelVerification, Operation: "apply_reset_preflight", Unit: identity.Name, Property: PropertyIODeviceWeight, Err: err}
			}
			state.pendingIOWeightResets = resets
		}
		if assignment.name == PropertyIODeviceWeight {
			state.ioWeightTargets = cloneIODeviceWeightTargets(assignment.ioDeviceWeightTargets)
		}
		state.lastApplied = clonePropertyValue(assignment.name, assignment.value)
		state.lease = publicPropertyLease(assignment.name, state.baseline, state.lastApplied)
		state.uncertain = true
		staged[key] = state
	}
	beforeStage := a.snapshotLeaseState()
	for key, state := range staged {
		a.leases[key] = state
	}
	stagedOverride := stageManagedUnitFileMutation(identity.Name, currentOverride, validated)
	a.overrides[identity] = stagedOverride
	a.phases[identity.Name] = leasePhaseApplying
	if err := a.persistLeaseState(); err != nil {
		a.restoreLeaseState(beforeStage)
		return UnitSnapshot{}, err
	}

	// systemd treats non-empty per-device arrays as upserts. Prepend an empty
	// IODeviceWeight value when the owned target set shrinks so the reset and
	// replacement are processed in order by one SetUnitProperties method call.
	mutationAssignments := systemdMutationAssignments(before, validated)
	// runtime=true is deliberately fixed here. The public adapter cannot persist unit changes.
	if err := a.transport.setUnitProperties(callCtx, identity.Name, true, mutationAssignments); err != nil {
		return UnitSnapshot{}, classifyTransportError("apply", identity.Name, err)
	}
	after, err := a.readUnit(callCtx, identity.Name, identity.ObjectPath)
	if err != nil {
		return UnitSnapshot{}, err
	}
	if err := requireSameIdentity("apply_readback", identity, after.Identity); err != nil {
		return UnitSnapshot{}, err
	}
	if err := verifyReadback("apply_readback", after, validated); err != nil {
		return UnitSnapshot{}, err
	}
	if err := a.requireManagedUnitFileFootprint("apply_readback", after, stagedOverride, true); err != nil {
		return UnitSnapshot{}, err
	}
	resetCompleted, err := a.completePendingIODeviceWeightResets(after, "apply_readback")
	if err != nil {
		return UnitSnapshot{}, err
	}
	if resetCompleted {
		after, err = a.readUnit(callCtx, identity.Name, identity.ObjectPath)
		if err != nil {
			return UnitSnapshot{}, err
		}
		if err := requireSameIdentity("apply_reset_confirmation", identity, after.Identity); err != nil {
			return UnitSnapshot{}, err
		}
		if err := verifyReadback("apply_reset_confirmation", after, validated); err != nil {
			return UnitSnapshot{}, err
		}
		if err := a.requireManagedUnitFileFootprint("apply_reset_confirmation", after, stagedOverride, true); err != nil {
			return UnitSnapshot{}, err
		}
	}
	if err := a.verifier.verify(after, validated); err != nil {
		return UnitSnapshot{}, &AdapterError{Reason: ReasonKernelVerification, Operation: "apply_readback", Unit: identity.Name, Err: err}
	}
	fingerprints, err := a.captureFootprint(after)
	if err != nil {
		return UnitSnapshot{}, err
	}
	beforeConfirmation := a.snapshotLeaseState()
	a.confirmAppliedUnit(identity.Name, fingerprints)
	if err := a.persistLeaseState(); err != nil {
		a.restoreLeaseState(beforeConfirmation)
		return UnitSnapshot{}, err
	}
	return after, nil
}

func systemdMutationAssignments(before UnitSnapshot, assignments []PropertyAssignment) []PropertyAssignment {
	result := make([]PropertyAssignment, 0, len(assignments)+1)
	for _, assignment := range assignments {
		if assignment.name == PropertyIODeviceWeight && ioDeviceWeightTargetRemoved(before, assignment) {
			result = append(result, PropertyAssignment{name: PropertyIODeviceWeight, value: devicePropertyValue(nil)})
		}
		result = append(result, assignment)
	}
	return result
}

func ioDeviceWeightTargetRemoved(before UnitSnapshot, desired PropertyAssignment) bool {
	current, ok := before.Properties.propertyValue(PropertyIODeviceWeight)
	if !ok || len(current.devices) == 0 {
		return false
	}
	desiredPaths := make(map[string]bool, len(desired.value.devices))
	for _, device := range desired.value.devices {
		desiredPaths[device.Path] = true
	}
	for _, device := range current.devices {
		if !desiredPaths[device.Path] {
			return true
		}
	}
	return false
}

// ConfirmApplied performs a read-only final confirmation of one exact unit
// lifetime, the durable ResMan footprint, D-Bus properties and effective kernel
// values. It never repairs drift: an external value wins and is reported.
func (a *Adapter) ConfirmApplied(ctx context.Context, identity UnitIdentity, assignments []PropertyAssignment) (UnitSnapshot, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("confirm_applied"); err != nil {
		return UnitSnapshot{}, err
	}
	validated, err := validateAssignments(assignments)
	if err != nil {
		return UnitSnapshot{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	snapshot, err := a.readUnit(callCtx, identity.Name, identity.ObjectPath)
	if err != nil {
		return UnitSnapshot{}, err
	}
	if err := requireSameIdentity("confirm_applied", identity, snapshot.Identity); err != nil {
		return UnitSnapshot{}, err
	}
	override, tracked := a.overrides[identity]
	if !tracked || a.phases[identity.Name] != leasePhaseApplied {
		return UnitSnapshot{}, &AdapterError{Reason: ReasonReadbackMismatch, Operation: "confirm_applied", Unit: identity.Name, Err: fmt.Errorf("no confirmed durable lease exists for the requested unit lifetime")}
	}
	if err := a.requireManagedUnitFileFootprint("confirm_applied", snapshot, override, true); err != nil {
		return UnitSnapshot{}, err
	}
	fingerprints, err := a.captureFootprint(snapshot)
	if err != nil {
		return UnitSnapshot{}, err
	}
	if !equalFingerprints(fingerprints, override.fingerprints) {
		return UnitSnapshot{}, externalRecoveryConflict(identity.Name, "managed unit-file content changed after application")
	}
	for _, assignment := range validated {
		key := propertyLeaseKey{identity: identity, property: assignment.name}
		state, exists := a.leases[key]
		if !exists || !propertyValuesEqual(assignment.name, state.lastApplied, assignment.value) ||
			!ioDeviceWeightTargetsEqual(state.ioWeightTargets, assignment.ioDeviceWeightTargets) {
			return UnitSnapshot{}, &AdapterError{Reason: ReasonReadbackMismatch, Operation: "confirm_applied", Unit: identity.Name, Property: assignment.name, Err: fmt.Errorf("requested value does not match the durable lease")}
		}
		current, exists := snapshot.Properties.propertyValue(assignment.name)
		if !exists {
			return UnitSnapshot{}, malformedReply("confirm_applied", identity.Name, fmt.Sprintf("property %s is absent", assignment.name))
		}
		if !propertyValuesEqual(assignment.name, current, state.lastApplied) {
			return UnitSnapshot{}, externalConflictFor("confirm_applied", identity.Name, assignment.name, state.lastApplied, current)
		}
	}
	if err := a.verifier.verify(snapshot, validated); err != nil {
		return UnitSnapshot{}, &AdapterError{Reason: ReasonKernelVerification, Operation: "confirm_applied", Unit: identity.Name, Err: err}
	}
	return snapshot, nil
}

func (a *Adapter) appliedAssignmentsMatch(identity UnitIdentity, snapshot UnitSnapshot, assignments []PropertyAssignment) bool {
	for _, assignment := range assignments {
		state, ok := a.leases[propertyLeaseKey{identity: identity, property: assignment.name}]
		if !ok || state.uncertain || !propertyValuesEqual(assignment.name, state.lastApplied, assignment.value) ||
			!ioDeviceWeightTargetsEqual(state.ioWeightTargets, assignment.ioDeviceWeightTargets) {
			return false
		}
		current, ok := snapshot.Properties.propertyValue(assignment.name)
		if !ok || !propertyValuesEqual(assignment.name, current, assignment.value) {
			return false
		}
	}
	return len(assignments) > 0
}

// RestoreProperties restores selected properties without reverting the unit's
// other ResMan-owned runtime overrides. Baseline reset drop-ins remain tracked
// until a later complete unit restore can remove the exact footprint safely.
func (a *Adapter) RestoreProperties(ctx context.Context, identity UnitIdentity, properties []PropertyName) (RestoreResult, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("restore_properties"); err != nil {
		return RestoreResult{}, err
	}
	if err := a.blocked[identity.Name]; err != nil {
		return RestoreResult{}, err
	}
	selected, err := validateRestoreProperties(properties)
	if err != nil {
		return RestoreResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	current, err := a.readUnit(callCtx, identity.Name, identity.ObjectPath)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := requireSameIdentity("restore_properties", identity, current.Identity); err != nil {
		return RestoreResult{}, err
	}
	override, tracked := a.overrides[identity]
	if err := a.requireManagedUnitFileFootprint("restore_properties", current, override, tracked); err != nil {
		return RestoreResult{}, err
	}
	if tracked {
		fingerprints, err := a.captureFootprint(current)
		if err != nil {
			return RestoreResult{}, err
		}
		if !equalFingerprints(fingerprints, override.fingerprints) {
			return RestoreResult{}, externalRecoveryConflict(identity.Name, "managed unit-file content changed before property restoration")
		}
	}

	var result RestoreResult
	var assignments []PropertyAssignment
	var keys []propertyLeaseKey
	for _, property := range selected {
		key := propertyLeaseKey{identity: identity, property: property}
		state, exists := a.leases[key]
		if !exists {
			continue
		}
		value, exists := current.Properties.propertyValue(property)
		if !exists {
			return result, malformedReply("restore_properties", identity.Name, fmt.Sprintf("property %s is absent", property))
		}
		owned, resolved := resolveUncertainOwnership(value, state)
		if !owned {
			result.Conflicts = append(result.Conflicts, publicPropertyConflict(property, state.lastApplied, value))
			continue
		}
		a.leases[key] = resolved
		if propertyValuesEqual(property, value, resolved.baseline) {
			result.Restored = append(result.Restored, property)
			continue
		}
		assignments = append(assignments, PropertyAssignment{name: property, value: clonePropertyValue(property, resolved.baseline), ioDeviceWeightTargets: cloneIODeviceWeightTargets(resolved.ioWeightTargets)})
		keys = append(keys, key)
	}
	if len(assignments) != 0 {
		before := a.snapshotLeaseState()
		for index, key := range keys {
			state := a.leases[key]
			state.previousApplied = state.lastApplied
			state.previousIOWeightTargets = cloneIODeviceWeightTargets(state.ioWeightTargets)
			state.pendingIOWeightResets = nil
			if key.property == PropertyIODeviceWeight {
				previous := PropertyAssignment{name: key.property, value: clonePropertyValue(key.property, state.lastApplied), ioDeviceWeightTargets: cloneIODeviceWeightTargets(state.ioWeightTargets)}
				assignment := assignments[index]
				resets, err := a.verifier.prepareIODeviceWeightResets(current, previous, assignment)
				if err != nil {
					a.restoreLeaseState(before)
					return result, &AdapterError{Reason: ReasonKernelVerification, Operation: "restore_properties_reset_preflight", Unit: identity.Name, Property: key.property, Err: err}
				}
				state.pendingIOWeightResets = resets
			}
			state.lastApplied = clonePropertyValue(key.property, state.baseline)
			state.lease = publicPropertyLease(key.property, state.baseline, state.lastApplied)
			state.uncertain = true
			a.leases[key] = state
		}
		a.overrides[identity] = stageManagedUnitFileMutation(identity.Name, override, assignments)
		a.phases[identity.Name] = leasePhaseApplying
		if err := a.persistLeaseState(); err != nil {
			a.restoreLeaseState(before)
			return result, err
		}
		if err := a.transport.setUnitProperties(callCtx, identity.Name, true, assignments); err != nil {
			return result, classifyTransportError("restore_properties", identity.Name, err)
		}
		after, err := a.readUnit(callCtx, identity.Name, identity.ObjectPath)
		if err != nil {
			return result, err
		}
		if err := requireSameIdentity("restore_properties_readback", identity, after.Identity); err != nil {
			return result, err
		}
		if err := verifyReadback("restore_properties_readback", after, assignments); err != nil {
			return result, err
		}
		resetCompleted, err := a.completePendingIODeviceWeightResets(after, "restore_properties_readback")
		if err != nil {
			return result, err
		}
		if resetCompleted {
			after, err = a.readUnit(callCtx, identity.Name, identity.ObjectPath)
			if err != nil {
				return result, err
			}
			if err := requireSameIdentity("restore_properties_reset_confirmation", identity, after.Identity); err != nil {
				return result, err
			}
			if err := verifyReadback("restore_properties_reset_confirmation", after, assignments); err != nil {
				return result, err
			}
			if err := a.requireManagedUnitFileFootprint("restore_properties_reset_confirmation", after, a.overrides[identity], true); err != nil {
				return result, err
			}
		}
		if err := a.verifier.verify(after, assignments); err != nil {
			return result, &AdapterError{Reason: ReasonKernelVerification, Operation: "restore_properties_readback", Unit: identity.Name, Err: err}
		}
		fingerprints, err := a.captureFootprint(after)
		if err != nil {
			return result, err
		}
		beforeConfirmation := a.snapshotLeaseState()
		a.confirmAppliedUnit(identity.Name, fingerprints)
		if err := a.persistLeaseState(); err != nil {
			a.restoreLeaseState(beforeConfirmation)
			return result, err
		}
		for _, key := range keys {
			result.Restored = append(result.Restored, key.property)
		}
	}
	sortPropertyNames(result.Restored)
	sort.Slice(result.Conflicts, func(i, j int) bool { return result.Conflicts[i].Property < result.Conflicts[j].Property })
	return result, conflictError(identity.Name, result.Conflicts)
}

// Restore restores every still-owned property for one unit. Externally changed
// properties are preserved and returned as typed conflicts.
func (a *Adapter) Restore(ctx context.Context, identity UnitIdentity) (RestoreResult, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("restore"); err != nil {
		return RestoreResult{}, err
	}
	if err := a.blocked[identity.Name]; err != nil {
		return RestoreResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	current, err := a.readUnit(callCtx, identity.Name, identity.ObjectPath)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := requireSameIdentity("restore", identity, current.Identity); err != nil {
		return RestoreResult{}, err
	}
	override, trackedOverride := a.overrides[identity]
	currentMutablePaths, err := a.combinedMutableUnitFilePaths(current)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := requireRestorableUnitFileFootprint(identity.Name, currentMutablePaths, override, trackedOverride); err != nil {
		return RestoreResult{}, err
	}
	if trackedOverride && len(currentMutablePaths) > 0 {
		fingerprints, err := a.unitFiles.fingerprints(currentMutablePaths)
		if err != nil {
			return RestoreResult{}, &AdapterError{Reason: ReasonUnitFileVerification, Operation: "restore", Unit: identity.Name, Err: err}
		}
		if a.phases[identity.Name] == leasePhaseApplying && pathsMatchFootprint(fingerprints, override.managedPaths) {
			override.fingerprints = fingerprints
			a.overrides[identity] = override
		} else if !equalFingerprints(fingerprints, override.fingerprints) {
			return RestoreResult{}, externalRecoveryConflict(identity.Name, "managed unit-file content changed before restoration")
		}
	}

	var result RestoreResult
	var restore []PropertyAssignment
	var restoreNeeded []PropertyAssignment
	var ownedKeys []propertyLeaseKey
	var restoreKeys []propertyLeaseKey
	for key, state := range a.leases {
		if key.identity != identity {
			continue
		}
		value, ok := current.Properties.propertyValue(key.property)
		if !ok {
			return result, malformedReply("restore", identity.Name, fmt.Sprintf("property %s is absent", key.property))
		}
		owned, resolved := resolveUncertainOwnership(value, state)
		if !owned {
			result.Conflicts = append(result.Conflicts, publicPropertyConflict(key.property, state.lastApplied, value))
			continue
		}
		a.leases[key] = resolved
		assignment := PropertyAssignment{name: key.property, value: clonePropertyValue(key.property, resolved.baseline), ioDeviceWeightTargets: cloneIODeviceWeightTargets(resolved.ioWeightTargets)}
		restore = append(restore, assignment)
		ownedKeys = append(ownedKeys, key)
		if propertyValuesEqual(key.property, value, resolved.baseline) {
			result.Restored = append(result.Restored, key.property)
			continue
		}
		restoreNeeded = append(restoreNeeded, assignment)
		restoreKeys = append(restoreKeys, key)
	}
	sortPropertyNames(result.Restored)
	sort.Slice(result.Conflicts, func(i, j int) bool { return result.Conflicts[i].Property < result.Conflicts[j].Property })
	sort.Slice(restore, func(i, j int) bool { return restore[i].name < restore[j].name })
	sort.Slice(restoreNeeded, func(i, j int) bool { return restoreNeeded[i].name < restoreNeeded[j].name })
	sort.Slice(ownedKeys, func(i, j int) bool { return ownedKeys[i].property < ownedKeys[j].property })
	sort.Slice(restoreKeys, func(i, j int) bool { return restoreKeys[i].property < restoreKeys[j].property })

	if len(result.Conflicts) > 0 {
		if len(restoreNeeded) != 0 {
			if err := a.restoreOwnedPropertiesWithoutRevert(callCtx, identity, restoreNeeded, restoreKeys, &result); err != nil {
				return result, errors.Join(err, conflictError(identity.Name, result.Conflicts))
			}
		}
		sortPropertyNames(result.Restored)
		return result, conflictError(identity.Name, result.Conflicts)
	}
	if len(ownedKeys) > 0 && len(restoreKeys) == 0 && len(currentMutablePaths) == 0 {
		if err := a.verifyRestored(callCtx, identity, restore); err != nil {
			return result, err
		}
		if err := a.removeUnitLeaseDurably(identity.Name); err != nil {
			return result, err
		}
		sortPropertyNames(result.Restored)
		return result, nil
	}
	if len(ownedKeys) > 0 {
		if len(override.managedPaths) == 0 {
			return result, &AdapterError{Reason: ReasonUnitFileVerification, Operation: "restore", Unit: identity.Name, Err: fmt.Errorf("property leases exist without a recorded runtime drop-in footprint")}
		}
		// Revert/Reload removes ResMan's runtime drop-ins without necessarily
		// applying their scalar or per-device baselines to an active cgroup.
		// Restore every changed property while the lease is still authoritative,
		// using the existing write-ahead/readback ownership transaction, and only
		// then remove the runtime files.
		if len(restoreNeeded) > 0 {
			programmedRestore := effectiveBaselineAssignments(restoreNeeded)
			if err := a.restoreOwnedPropertiesWithoutRevert(callCtx, identity, programmedRestore, restoreKeys, &result); err != nil {
				return result, err
			}
			// Recheck the complete footprint after the reset transaction, before
			// permitting a destructive revert. A new operator file wins.
			refreshed, err := a.readUnit(callCtx, identity.Name, identity.ObjectPath)
			if err != nil {
				return result, err
			}
			if err := requireSameIdentity("restore_before_revert", identity, refreshed.Identity); err != nil {
				return result, err
			}
			if !a.propertiesMatch(identity.Name, func(state propertyLeaseState) propertyValue { return state.lastApplied }, refreshed) {
				return result, externalRecoveryConflict(identity.Name, "property changed after baseline restore")
			}
			if err := a.requireManagedUnitFileFootprint("restore_before_revert", refreshed, a.overrides[identity], true); err != nil {
				return result, err
			}
			actual, err := a.captureFootprint(refreshed)
			if err != nil {
				return result, err
			}
			if !equalFingerprints(actual, a.overrides[identity].fingerprints) {
				return result, externalRecoveryConflict(identity.Name, "unit file changed after baseline restore")
			}
		}
		beforeRestore := a.snapshotLeaseState()
		for _, key := range ownedKeys {
			state := a.leases[key]
			state.previousApplied = state.lastApplied
			state.lastApplied = clonePropertyValue(key.property, state.baseline)
			state.lease = publicPropertyLease(key.property, state.baseline, state.lastApplied)
			state.uncertain = true
			a.leases[key] = state
		}
		a.phases[identity.Name] = leasePhaseRestoring
		if err := a.persistLeaseState(); err != nil {
			a.restoreLeaseState(beforeRestore)
			return result, err
		}
		if err := a.transport.revertUnitFiles(callCtx, identity.Name); err != nil {
			return result, errors.Join(classifyTransportError("restore", identity.Name, err), conflictError(identity.Name, result.Conflicts))
		}
		beforeReload := a.snapshotLeaseState()
		a.phases[identity.Name] = leasePhaseReloading
		if err := a.persistLeaseState(); err != nil {
			a.restoreLeaseState(beforeReload)
			return result, errors.Join(err, conflictError(identity.Name, result.Conflicts))
		}
		if err := a.transport.reload(callCtx); err != nil {
			return result, errors.Join(classifyTransportError("restore_reload", identity.Name, err), conflictError(identity.Name, result.Conflicts))
		}
		if err := a.verifyRestored(callCtx, identity, restore); err != nil {
			return result, errors.Join(err, conflictError(identity.Name, result.Conflicts))
		}
		for _, key := range ownedKeys {
			if !containsPropertyName(result.Restored, key.property) {
				result.Restored = append(result.Restored, key.property)
			}
		}
		if err := a.removeUnitLeaseDurably(identity.Name); err != nil {
			return result, errors.Join(err, conflictError(identity.Name, result.Conflicts))
		}
		sortPropertyNames(result.Restored)
	}
	return result, conflictError(identity.Name, result.Conflicts)
}

func (a *Adapter) restoreOwnedPropertiesWithoutRevert(ctx context.Context, identity UnitIdentity, assignments []PropertyAssignment, keys []propertyLeaseKey, result *RestoreResult) error {
	if len(assignments) != len(keys) {
		return fmt.Errorf("restore property assignment count %d does not match lease count %d", len(assignments), len(keys))
	}
	before := a.snapshotLeaseState()
	for index, key := range keys {
		if assignments[index].name != key.property {
			return fmt.Errorf("restore property assignment %s does not match lease %s", assignments[index].name, key.property)
		}
		state := a.leases[key]
		state.previousApplied = state.lastApplied
		state.previousIOWeightTargets = cloneIODeviceWeightTargets(state.ioWeightTargets)
		state.pendingIOWeightResets = nil
		if key.property == PropertyIODeviceWeight {
			previous := PropertyAssignment{name: key.property, value: clonePropertyValue(key.property, state.lastApplied), ioDeviceWeightTargets: cloneIODeviceWeightTargets(state.ioWeightTargets)}
			resets, err := a.verifier.prepareIODeviceWeightResets(UnitSnapshot{Identity: identity, ControlGroup: ""}, previous, assignments[index])
			if err != nil {
				a.restoreLeaseState(before)
				return &AdapterError{Reason: ReasonKernelVerification, Operation: "restore_properties_reset_preflight", Unit: identity.Name, Property: key.property, Err: err}
			}
			state.pendingIOWeightResets = resets
		}
		state.lastApplied = clonePropertyValue(key.property, assignments[index].value)
		state.lease = publicPropertyLease(key.property, state.baseline, state.lastApplied)
		state.uncertain = true
		a.leases[key] = state
	}
	a.phases[identity.Name] = leasePhaseApplying
	if err := a.persistLeaseState(); err != nil {
		a.restoreLeaseState(before)
		return err
	}
	if err := a.transport.setUnitProperties(ctx, identity.Name, true, assignments); err != nil {
		return classifyTransportError("restore_properties", identity.Name, err)
	}
	after, err := a.readUnit(ctx, identity.Name, identity.ObjectPath)
	if err != nil {
		return err
	}
	if err := requireSameIdentity("restore_properties_readback", identity, after.Identity); err != nil {
		return err
	}
	if err := verifyReadback("restore_properties_readback", after, assignments); err != nil {
		return err
	}
	resetCompleted, err := a.completePendingIODeviceWeightResets(after, "restore_properties_readback")
	if err != nil {
		return err
	}
	if resetCompleted {
		after, err = a.readUnit(ctx, identity.Name, identity.ObjectPath)
		if err != nil {
			return err
		}
		if err := requireSameIdentity("restore_properties_reset_confirmation", identity, after.Identity); err != nil {
			return err
		}
		if err := verifyReadback("restore_properties_reset_confirmation", after, assignments); err != nil {
			return err
		}
		if err := a.requireManagedUnitFileFootprint("restore_properties_reset_confirmation", after, a.overrides[identity], true); err != nil {
			return err
		}
	}
	if err := a.verifier.verify(after, assignments); err != nil {
		return &AdapterError{Reason: ReasonKernelVerification, Operation: "restore_properties_readback", Unit: identity.Name, Err: err}
	}
	fingerprints, err := a.captureFootprint(after)
	if err != nil {
		return err
	}
	beforeConfirmation := a.snapshotLeaseState()
	a.confirmAppliedUnit(identity.Name, fingerprints)
	if err := a.persistLeaseState(); err != nil {
		a.restoreLeaseState(beforeConfirmation)
		return err
	}
	for _, key := range keys {
		result.Restored = append(result.Restored, key.property)
	}
	return nil
}

func (a *Adapter) completePendingIODeviceWeightResets(snapshot UnitSnapshot, operation string) (bool, error) {
	var resets []ioDeviceWeightReset
	for key, state := range a.leases {
		if key.identity == snapshot.Identity {
			resets = append(resets, state.pendingIOWeightResets...)
		}
	}
	if len(resets) == 0 {
		return false, nil
	}
	if err := a.verifier.resetIODeviceWeightOverrides(snapshot, resets); err != nil {
		reason := ReasonKernelVerification
		var conflict *ioDeviceWeightResetConflict
		if errors.As(err, &conflict) {
			reason = ReasonExternalConflict
		}
		return false, &AdapterError{Reason: reason, Operation: operation + "_keyed_reset", Unit: snapshot.Identity.Name, Property: PropertyIODeviceWeight, Err: err}
	}
	return true, nil
}

func (a *Adapter) hasPendingIODeviceWeightResets(unit string) bool {
	for key, state := range a.leases {
		if key.identity.Name == unit && len(state.pendingIOWeightResets) != 0 {
			return true
		}
	}
	return false
}

func effectiveBaselineAssignments(baselines []PropertyAssignment) []PropertyAssignment {
	result := make([]PropertyAssignment, len(baselines))
	for index, baseline := range baselines {
		result[index] = clonePropertyAssignment(baseline)
		if (baseline.name == PropertyCPUWeight || baseline.name == PropertyIOWeight) && baseline.value.scalar == SystemdUnset {
			// An empty weight assignment changes systemd's normalized property but
			// does not reliably reset the live cgroup. Program the kernel default
			// explicitly; the guarded RevertUnitFiles removes this temporary value.
			result[index].value = scalarPropertyValue(100)
		}
	}
	return result
}

func containsPropertyName(properties []PropertyName, wanted PropertyName) bool {
	for _, property := range properties {
		if property == wanted {
			return true
		}
	}
	return false
}

// RecoveryReport returns a sorted defensive copy of startup lease-reconciliation outcomes.
func (a *Adapter) RecoveryReport() []LeaseRecoveryOutcome {
	leave := a.opGate.Enter()
	defer leave()
	result := append([]LeaseRecoveryOutcome(nil), a.recovery...)
	sort.Slice(result, func(left, right int) bool {
		if result[left].Unit == result[right].Unit {
			return result[left].State < result[right].State
		}
		return result[left].Unit < result[right].Unit
	})
	return result
}

// OwnedUnits returns the current durable property-lease identities. Callers
// use this inventory for exact release; a unit name or cgroup path alone never
// establishes ResMan ownership.
func (a *Adapter) OwnedUnits() []UnitIdentity {
	leave := a.opGate.Enter()
	defer leave()
	result := make([]UnitIdentity, 0, len(a.overrides))
	for identity := range a.overrides {
		result = append(result, identity)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}

// Leases returns a sorted defensive copy of the properties currently owned for a unit lifetime.
func (a *Adapter) Leases(identity UnitIdentity) []PropertyLease {
	leave := a.opGate.Enter()
	defer leave()
	var result []PropertyLease
	for key, state := range a.leases {
		if key.identity == identity {
			result = append(result, state.lease)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Property < result[j].Property })
	return result
}

func (a *Adapter) readUnit(ctx context.Context, unit, objectPath string) (UnitSnapshot, error) {
	unitBefore, err := a.transport.unitProperties(ctx, unit)
	if err != nil {
		return UnitSnapshot{}, classifyTransportError("read_unit", unit, err)
	}
	sliceBefore, err := a.transport.sliceProperties(ctx, unit)
	if err != nil {
		return UnitSnapshot{}, classifyTransportError("read_slice", unit, err)
	}
	controlGroupBefore, err := parseControlGroup(unit, sliceBefore)
	if err != nil {
		return UnitSnapshot{}, err
	}
	kernelIDBefore, err := a.kernelControlGroupID("read_identity", unit, controlGroupBefore)
	if err != nil {
		return UnitSnapshot{}, err
	}
	unitAfter, err := a.transport.unitProperties(ctx, unit)
	if err != nil {
		return UnitSnapshot{}, classifyTransportError("confirm_unit", unit, err)
	}
	sliceAfter, err := a.transport.sliceProperties(ctx, unit)
	if err != nil {
		return UnitSnapshot{}, classifyTransportError("confirm_slice", unit, err)
	}
	controlGroupAfter, err := parseControlGroup(unit, sliceAfter)
	if err != nil {
		return UnitSnapshot{}, err
	}
	kernelIDAfter, err := a.kernelControlGroupID("confirm_identity", unit, controlGroupAfter)
	if err != nil {
		return UnitSnapshot{}, err
	}
	beforeIdentity, err := parseUnitIdentity(unit, objectPath, unitBefore, sliceBefore, kernelIDBefore)
	if err != nil {
		return UnitSnapshot{}, err
	}
	afterIdentity, err := parseUnitIdentity(unit, objectPath, unitAfter, sliceAfter, kernelIDAfter)
	if err != nil {
		return UnitSnapshot{}, err
	}
	if beforeIdentity != afterIdentity || controlGroupBefore != controlGroupAfter {
		return UnitSnapshot{}, &AdapterError{Reason: ReasonUnitRecreated, Operation: "confirm_unit", Unit: unit, Err: fmt.Errorf("unit identity changed during read")}
	}
	properties := make(map[PropertyName]propertyValue, len(approvedScalarProperties)+len(approvedDeviceProperties))
	for property := range approvedScalarProperties {
		value, ok := sliceAfter[string(property)].(uint64)
		if !ok {
			return UnitSnapshot{}, malformedReply("read_slice", unit, fmt.Sprintf("property %s is absent or is not uint64", property))
		}
		if err := validatePropertyValue(property, value); err != nil {
			return UnitSnapshot{}, malformedReply("read_slice", unit, fmt.Sprintf("property %s has invalid value: %v", property, err))
		}
		properties[property] = scalarPropertyValue(value)
	}
	for property := range approvedDeviceProperties {
		value, err := parseDevicePropertyValue(sliceAfter[string(property)])
		if err != nil {
			return UnitSnapshot{}, malformedReply("read_slice", unit, fmt.Sprintf("property %s is absent or malformed: %v", property, err))
		}
		if property == PropertyIODeviceWeight {
			if err := validateIODeviceWeightValues(value); err != nil {
				return UnitSnapshot{}, malformedReply("read_slice", unit, fmt.Sprintf("property %s has invalid value: %v", property, err))
			}
		}
		properties[property] = devicePropertyValue(value)
	}
	unitFiles, err := parseUnitFileSnapshot(unit, unitAfter)
	if err != nil {
		return UnitSnapshot{}, err
	}
	if isCapabilityProbeTransientFragment(unit, unitFiles.fragmentPath) {
		// The transient slice definition belongs to the bounded probe lifecycle,
		// not to an operator or to ResMan's property-lease footprint. The probe
		// service owns its removal; system.control drop-ins remain fully guarded.
		unitFiles.fragmentPath = ""
	}
	return UnitSnapshot{Identity: beforeIdentity, ControlGroup: controlGroupAfter, Properties: newPropertySet(properties), unitFiles: unitFiles}, nil
}

func (a *Adapter) kernelControlGroupID(operation, unit, controlGroup string) (uint64, error) {
	identity, err := a.verifier.identity(controlGroup)
	if err != nil {
		return 0, &AdapterError{Reason: ReasonKernelVerification, Operation: operation, Unit: unit, Err: err}
	}
	if identity == 0 {
		return 0, &AdapterError{Reason: ReasonKernelVerification, Operation: operation, Unit: unit, Err: fmt.Errorf("kernel returned a zero cgroup identity")}
	}
	return identity, nil
}

func parseControlGroup(unit string, sliceProperties map[string]any) (string, error) {
	controlGroup, ok := sliceProperties["ControlGroup"].(string)
	if !ok || !validControlGroup(controlGroup) {
		return "", malformedReply("read_slice", unit, "ControlGroup is absent or invalid")
	}
	return controlGroup, nil
}

func parseUnitFileSnapshot(unit string, properties map[string]any) (unitFileSnapshot, error) {
	fragmentPath, ok := properties["FragmentPath"].(string)
	if !ok || (fragmentPath != "" && !validAbsolutePath(fragmentPath)) {
		return unitFileSnapshot{}, malformedReply("read_unit", unit, "FragmentPath is absent or invalid")
	}
	dropInPaths, ok := properties["DropInPaths"].([]string)
	if !ok {
		return unitFileSnapshot{}, malformedReply("read_unit", unit, "DropInPaths is absent or malformed")
	}
	result := unitFileSnapshot{fragmentPath: fragmentPath, dropInPaths: append([]string(nil), dropInPaths...)}
	for _, path := range result.dropInPaths {
		if !validAbsolutePath(path) {
			return unitFileSnapshot{}, malformedReply("read_unit", unit, fmt.Sprintf("DropInPaths contains invalid path %q", path))
		}
	}
	sort.Strings(result.dropInPaths)
	return result, nil
}

func validAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.Contains(path, "//")
}

func extendManagedUnitFileFootprint(unit string, current unitOverrideLease, assignments []PropertyAssignment) unitOverrideLease {
	paths := make(map[string]bool, len(current.managedPaths)+len(assignments))
	for _, path := range current.managedPaths {
		paths[path] = true
	}
	for _, assignment := range assignments {
		paths[managedRuntimeDropInPath(unit, assignment.name)] = true
	}
	result := unitOverrideLease{managedPaths: make([]string, 0, len(paths))}
	for path := range paths {
		result.managedPaths = append(result.managedPaths, path)
	}
	sort.Strings(result.managedPaths)
	return result
}

func stageManagedUnitFileMutation(unit string, current unitOverrideLease, assignments []PropertyAssignment) unitOverrideLease {
	result := extendManagedUnitFileFootprint(unit, current, assignments)
	// Preserve the complete pre-write footprint for rollback, but require its
	// exact digest during forward recovery only for files outside this D-Bus
	// mutation. The assigned properties are authenticated by typed readback;
	// their drop-ins are expected to be rewritten by systemd.
	result.previousFingerprint = append([]unitFileFingerprint(nil), current.fingerprints...)
	mutable := make(map[string]bool, len(assignments))
	for _, assignment := range assignments {
		mutable[managedRuntimeDropInPath(unit, assignment.name)] = true
	}
	for _, fingerprint := range current.fingerprints {
		if !mutable[fingerprint.path] {
			result.fingerprints = append(result.fingerprints, fingerprint)
		}
	}
	return result
}

func managedRuntimeDropInPath(unit string, property PropertyName) string {
	filename := map[PropertyName]string{
		PropertyCPUWeight:           "50-CPUWeight.conf",
		PropertyCPUQuotaPerSecUSec:  "50-CPUQuota.conf",
		PropertyCPUQuotaPeriodUSec:  "50-CPUQuotaPeriodSec.conf",
		PropertyMemoryHigh:          "50-MemoryHigh.conf",
		PropertyMemoryMax:           "50-MemoryMax.conf",
		PropertyMemorySwapMax:       "50-MemorySwapMax.conf",
		PropertyIOWeight:            "50-IOWeight.conf",
		PropertyIODeviceWeight:      "50-IODeviceWeight.conf",
		PropertyIOReadBandwidthMax:  "50-IOReadBandwidthMax.conf",
		PropertyIOWriteBandwidthMax: "50-IOWriteBandwidthMax.conf",
		PropertyIOReadIOPSMax:       "50-IOReadIOPSMax.conf",
		PropertyIOWriteIOPSMax:      "50-IOWriteIOPSMax.conf",
	}[property]
	root := "/run/systemd/system.control"
	if isCapabilityProbeUnit(unit) {
		// systemd stores SetUnitProperties(runtime=true) overrides for a
		// transient unit beside its transient definition rather than in
		// system.control. This remains an exact, guarded probe-only footprint.
		root = "/run/systemd/transient"
	}
	return filepath.Join(root, unit+".d", filename)
}

func (a *Adapter) requireManagedUnitFileFootprint(operation string, snapshot UnitSnapshot, expected unitOverrideLease, tracked bool) error {
	actual, err := a.combinedMutableUnitFilePaths(snapshot)
	if err != nil {
		return err
	}
	want := expected.managedPaths
	if !tracked {
		want = nil
	}
	if equalStrings(actual, want) {
		return nil
	}
	return &AdapterError{
		Reason:    ReasonExternalConflict,
		Operation: operation,
		Unit:      snapshot.Identity.Name,
		Err:       fmt.Errorf("mutable unit-file footprint is %v, expected %v", actual, want),
	}
}

func requireRestorableUnitFileFootprint(unit string, actual []string, expected unitOverrideLease, tracked bool) error {
	if tracked && (equalStrings(actual, expected.managedPaths) || len(actual) == 0) {
		return nil
	}
	if !tracked && len(actual) == 0 {
		return nil
	}
	return &AdapterError{
		Reason:    ReasonExternalConflict,
		Operation: "restore",
		Unit:      unit,
		Err:       fmt.Errorf("mutable unit-file footprint is %v, expected the recorded ResMan footprint %v or an empty post-revert footprint", actual, expected.managedPaths),
	}
}

func (a *Adapter) combinedMutableUnitFilePaths(snapshot UnitSnapshot) ([]string, error) {
	dbusPaths := mutableUnitFilePaths(snapshot.unitFiles)
	diskPaths, err := a.unitFiles.mutablePaths(snapshot.Identity.Name)
	if err != nil {
		return nil, &AdapterError{Reason: ReasonUnitFileVerification, Operation: "inspect_unit_files", Unit: snapshot.Identity.Name, Err: err}
	}
	paths := make(map[string]bool, len(dbusPaths)+len(diskPaths))
	for _, path := range dbusPaths {
		paths[path] = true
	}
	for _, path := range diskPaths {
		paths[path] = true
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func mutableUnitFilePaths(snapshot unitFileSnapshot) []string {
	var result []string
	if pathIsRevertableLocalConfiguration(snapshot.fragmentPath) {
		result = append(result, snapshot.fragmentPath)
	}
	for _, path := range snapshot.dropInPaths {
		if pathIsRevertableLocalConfiguration(path) {
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func pathIsRevertableLocalConfiguration(path string) bool {
	if path == "" {
		return false
	}
	for _, root := range []string{
		"/etc/systemd/system",
		"/run/systemd/system",
		"/etc/systemd/system.control",
		"/run/systemd/system.control",
		"/run/systemd/transient",
	} {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func parseUnitIdentity(unit, objectPath string, unitProperties, sliceProperties map[string]any, kernelControlGroupID uint64) (UnitIdentity, error) {
	id, ok := unitProperties["Id"].(string)
	if !ok || id != unit {
		return UnitIdentity{}, malformedReply("read_unit", unit, "Id is absent or does not match the requested unit")
	}
	active, ok := unitProperties["ActiveState"].(string)
	if !ok || active != "active" {
		return UnitIdentity{}, &AdapterError{Reason: ReasonUnitMissing, Operation: "read_unit", Unit: unit, Err: fmt.Errorf("unit is not active")}
	}
	invocationBytes, ok := unitProperties["InvocationID"].([]byte)
	if !ok || len(invocationBytes) != 16 {
		return UnitIdentity{}, malformedReply("read_unit", unit, "InvocationID is absent or malformed")
	}
	var invocationID [16]byte
	copy(invocationID[:], invocationBytes)
	if invocationID == [16]byte{} {
		return UnitIdentity{}, malformedReply("read_unit", unit, "InvocationID is zero")
	}
	if controlGroupID, present := sliceProperties["ControlGroupId"]; present {
		typedControlGroupID, ok := controlGroupID.(uint64)
		if !ok || typedControlGroupID == 0 {
			return UnitIdentity{}, malformedReply("read_slice", unit, "ControlGroupId is present but malformed or zero")
		}
		if typedControlGroupID != kernelControlGroupID {
			return UnitIdentity{}, &AdapterError{Reason: ReasonUnitRecreated, Operation: "confirm_identity", Unit: unit, Err: fmt.Errorf("systemd cgroup ID %d does not match kernel cgroup ID %d", typedControlGroupID, kernelControlGroupID)}
		}
	}
	return UnitIdentity{Name: unit, ObjectPath: objectPath, InvocationID: invocationID, ControlGroupID: kernelControlGroupID}, nil
}

func validateAssignments(assignments []PropertyAssignment) ([]PropertyAssignment, error) {
	if len(assignments) == 0 {
		return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Err: fmt.Errorf("at least one property assignment is required")}
	}
	if len(assignments) > len(approvedScalarProperties)+len(approvedDeviceProperties) {
		return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Err: fmt.Errorf("too many property assignments")}
	}
	result := make([]PropertyAssignment, len(assignments))
	for index, assignment := range assignments {
		result[index] = clonePropertyAssignment(assignment)
	}
	seen := make(map[PropertyName]bool, len(result))
	for _, assignment := range result {
		if seen[assignment.name] {
			return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Property: assignment.name, Err: fmt.Errorf("duplicate property assignment")}
		}
		seen[assignment.name] = true
		if !isApprovedProperty(assignment.name) {
			return nil, &AdapterError{Reason: ReasonPropertyNotAllowed, Operation: "apply", Property: assignment.name, Err: fmt.Errorf("property is not approved")}
		}
		if _, deviceProperty := approvedDeviceProperties[assignment.name]; deviceProperty {
			if err := validateDeviceLimits(assignment.value.devices); err != nil {
				return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Property: assignment.name, Err: err}
			}
			if err := validateIODeviceWeightAssignment(assignment); err != nil {
				return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Property: assignment.name, Err: err}
			}
		} else if err := validatePropertyValue(assignment.name, assignment.value.scalar); err != nil {
			return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Property: assignment.name, Err: err}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result, nil
}

func validateResourceAssignments(resource ResourceKind, assignments []PropertyAssignment) ([]PropertyAssignment, error) {
	validated, err := validateAssignments(assignments)
	if err != nil {
		return nil, err
	}
	allowed := map[ResourceKind]map[PropertyName]bool{
		ResourceMemory: {
			PropertyMemoryHigh: true, PropertyMemoryMax: true, PropertyMemorySwapMax: true,
		},
		ResourceIO: {
			PropertyIOWeight: true, PropertyIOReadBandwidthMax: true, PropertyIOWriteBandwidthMax: true,
			PropertyIOReadIOPSMax: true, PropertyIOWriteIOPSMax: true,
		},
	}
	resourceProperties, ok := allowed[resource]
	if !ok {
		return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "check_resource_authority", Err: fmt.Errorf("unsupported resource %q", resource)}
	}
	for _, assignment := range validated {
		if !resourceProperties[assignment.name] {
			return nil, &AdapterError{Reason: ReasonPropertyNotAllowed, Operation: "check_resource_authority", Property: assignment.name, Err: fmt.Errorf("property does not belong to %s", resource)}
		}
	}
	return validated, nil
}

func validateRestoreProperties(properties []PropertyName) ([]PropertyName, error) {
	if len(properties) == 0 {
		return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "restore_properties", Err: fmt.Errorf("at least one property is required")}
	}
	result := append([]PropertyName(nil), properties...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	for index, property := range result {
		if !isApprovedProperty(property) {
			return nil, &AdapterError{Reason: ReasonPropertyNotAllowed, Operation: "restore_properties", Property: property, Err: fmt.Errorf("property is not approved")}
		}
		if index > 0 && result[index-1] == property {
			return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "restore_properties", Property: property, Err: fmt.Errorf("duplicate property")}
		}
	}
	return result, nil
}

func verifyReadback(operation string, snapshot UnitSnapshot, assignments []PropertyAssignment) error {
	for _, assignment := range assignments {
		current, ok := snapshot.Properties.propertyValue(assignment.name)
		if !ok || !propertyValuesEqual(assignment.name, current, assignment.value) {
			return &AdapterError{
				Reason:    ReasonReadbackMismatch,
				Operation: operation,
				Unit:      snapshot.Identity.Name,
				Property:  assignment.name,
				Err:       fmt.Errorf("wrote %s and read back %s", formatPropertyValue(assignment.name, assignment.value), formatPropertyValue(assignment.name, current)),
			}
		}
	}
	return nil
}

func requireSameIdentity(operation string, expected, current UnitIdentity) error {
	if expected == current {
		return nil
	}
	return &AdapterError{Reason: ReasonUnitRecreated, Operation: operation, Unit: expected.Name, Err: fmt.Errorf("expected invocation %s and cgroup ID %d, got invocation %s and cgroup ID %d", expected.InvocationIDString(), expected.ControlGroupID, current.InvocationIDString(), current.ControlGroupID)}
}

func resolveUncertainOwnership(current propertyValue, state propertyLeaseState) (bool, propertyLeaseState) {
	if !state.uncertain {
		return propertyValuesEqual(state.lease.Property, current, state.lastApplied), state
	}
	if propertyValuesEqual(state.lease.Property, current, state.lastApplied) {
		if len(state.pendingIOWeightResets) != 0 {
			return true, state
		}
		state.uncertain = false
		state.newLease = false
		state.previousIOWeightTargets = nil
		return true, state
	}
	if propertyValuesEqual(state.lease.Property, current, state.previousApplied) {
		state.lastApplied = clonePropertyValue(state.lease.Property, state.previousApplied)
		state.ioWeightTargets = cloneIODeviceWeightTargets(state.previousIOWeightTargets)
		state.previousIOWeightTargets = nil
		state.pendingIOWeightResets = nil
		state.lease = publicPropertyLease(state.lease.Property, state.baseline, state.lastApplied)
		state.uncertain = false
		return true, state
	}
	return false, state
}

func parseUserSliceName(name string) (uint32, bool) {
	if !strings.HasPrefix(name, "user-") || !strings.HasSuffix(name, ".slice") {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, "user-"), ".slice")
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || strconv.FormatUint(parsed, 10) != value {
		return 0, false
	}
	return uint32(parsed), true
}

func validControlGroup(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.Contains(value, "//")
}

func malformedReply(operation, unit, message string) error {
	return &AdapterError{Reason: ReasonMalformedReply, Operation: operation, Unit: unit, Err: fmt.Errorf("%s", message)}
}

func externalConflict(unit string, property PropertyName, lastApplied, current propertyValue) error {
	return externalConflictFor("apply", unit, property, lastApplied, current)
}

func externalConflictFor(operation, unit string, property PropertyName, lastApplied, current propertyValue) error {
	return &AdapterError{Reason: ReasonExternalConflict, Operation: operation, Unit: unit, Property: property, Err: fmt.Errorf("last applied %s differs from current %s", formatPropertyValue(property, lastApplied), formatPropertyValue(property, current))}
}

func conflictError(unit string, conflicts []PropertyConflict) error {
	if len(conflicts) == 0 {
		return nil
	}
	copyConflicts := append([]PropertyConflict(nil), conflicts...)
	return &RestoreConflictError{Unit: unit, Conflicts: copyConflicts}
}

func sortPropertyNames(names []PropertyName) {
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
}

func (a *Adapter) requireOpen(operation string) error {
	if !a.closed {
		return nil
	}
	return &AdapterError{Reason: ReasonClosed, Operation: operation, Err: fmt.Errorf("adapter is closed")}
}
