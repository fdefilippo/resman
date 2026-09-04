package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fdefilippo/resman/internal/operationgate"
)

const parentUserSlice = "user.slice"

type propertyLeaseState struct {
	lease           PropertyLease
	previousApplied uint64
	uncertain       bool
}

type propertyLeaseKey struct {
	identity UnitIdentity
	property PropertyName
}

type kernelVerifier interface {
	verify(UnitSnapshot, []PropertyAssignment) error
}

// Adapter is the narrow, runtime-only systemd resource-control boundary.
// The operation gate may span D-Bus and read-only cgroup verification I/O.
type Adapter struct {
	transport unitTransport
	verifier  kernelVerifier
	timeout   time.Duration
	opGate    operationgate.Gate
	leases    map[propertyLeaseKey]propertyLeaseState
	closed    bool
}

// New opens the authoritative system bus and a read-only cgroup verifier. The
// supplied context owns the connection lifetime and must remain live until Close.
func New(ctx context.Context, cgroupRoot string, timeout time.Duration) (*Adapter, error) {
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	transport, err := openDBusTransport(ctx)
	if err != nil {
		return nil, classifyTransportError("connect", "", err)
	}
	adapter, err := newAdapter(transport, newCgroupVerifier(cgroupRoot), timeout)
	if err != nil {
		transport.close()
		return nil, err
	}
	return adapter, nil
}

func newAdapter(transport unitTransport, verifier kernelVerifier, timeout time.Duration) (*Adapter, error) {
	if transport == nil {
		return nil, &AdapterError{Reason: ReasonBusUnavailable, Operation: "construct", Err: fmt.Errorf("systemd transport is required")}
	}
	if verifier == nil {
		return nil, &AdapterError{Reason: ReasonKernelVerification, Operation: "construct", Err: fmt.Errorf("read-only cgroup verifier is required")}
	}
	if timeout <= 0 {
		return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "construct", Err: fmt.Errorf("call timeout must be positive")}
	}
	return &Adapter{
		transport: transport,
		verifier:  verifier,
		timeout:   timeout,
		leases:    make(map[propertyLeaseKey]propertyLeaseState),
	}, nil
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

	listed, err := a.transport.listUserSlices(callCtx)
	if err != nil {
		return TopologySnapshot{}, classifyTransportError("discover", "", err)
	}
	return a.snapshotTopology(callCtx, listed)
}

func (a *Adapter) snapshotTopology(ctx context.Context, listed []listedUnit) (TopologySnapshot, error) {
	var result TopologySnapshot
	seen := make(map[string]bool, len(listed))
	parentFound := false
	for _, candidate := range listed {
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

	staged := make(map[propertyLeaseKey]propertyLeaseState, len(validated))
	for _, assignment := range validated {
		current, ok := before.Properties.Value(assignment.name)
		if !ok {
			return UnitSnapshot{}, malformedReply("apply", identity.Name, fmt.Sprintf("property %s is absent", assignment.name))
		}
		key := propertyLeaseKey{identity: identity, property: assignment.name}
		state, tracked := a.leases[key]
		if tracked {
			owned, resolved := resolveUncertainOwnership(current, state)
			if !owned {
				delete(a.leases, key)
				return UnitSnapshot{}, externalConflict(identity.Name, assignment.name, state.lease.LastApplied, current)
			}
			state = resolved
		} else {
			state.lease = PropertyLease{Property: assignment.name, Baseline: current, LastApplied: current}
		}
		state.previousApplied = state.lease.LastApplied
		state.lease.LastApplied = assignment.value
		state.uncertain = true
		staged[key] = state
	}
	for key, state := range staged {
		a.leases[key] = state
	}

	// runtime=true is deliberately fixed here. The public adapter cannot persist unit changes.
	if err := a.transport.setUnitProperties(callCtx, identity.Name, true, validated); err != nil {
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
	if err := a.verifier.verify(after, validated); err != nil {
		return UnitSnapshot{}, &AdapterError{Reason: ReasonKernelVerification, Operation: "apply_readback", Unit: identity.Name, Err: err}
	}
	for key, state := range staged {
		state.uncertain = false
		a.leases[key] = state
	}
	return after, nil
}

// Restore restores every still-owned property for one unit. Externally changed
// properties are preserved and returned as typed conflicts.
func (a *Adapter) Restore(ctx context.Context, identity UnitIdentity) (RestoreResult, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("restore"); err != nil {
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

	var result RestoreResult
	var restore []PropertyAssignment
	var restoreKeys []propertyLeaseKey
	for key, state := range a.leases {
		if key.identity != identity {
			continue
		}
		value, ok := current.Properties.Value(key.property)
		if !ok {
			return result, malformedReply("restore", identity.Name, fmt.Sprintf("property %s is absent", key.property))
		}
		owned, resolved := resolveUncertainOwnership(value, state)
		if !owned {
			result.Conflicts = append(result.Conflicts, PropertyConflict{Property: key.property, LastApplied: state.lease.LastApplied, Current: value})
			delete(a.leases, key)
			continue
		}
		a.leases[key] = resolved
		if value == resolved.lease.Baseline {
			result.Restored = append(result.Restored, key.property)
			delete(a.leases, key)
			continue
		}
		restore = append(restore, PropertyAssignment{name: key.property, value: resolved.lease.Baseline})
		restoreKeys = append(restoreKeys, key)
	}
	sortPropertyNames(result.Restored)
	sort.Slice(result.Conflicts, func(i, j int) bool { return result.Conflicts[i].Property < result.Conflicts[j].Property })
	sort.Slice(restore, func(i, j int) bool { return restore[i].name < restore[j].name })
	sort.Slice(restoreKeys, func(i, j int) bool { return restoreKeys[i].property < restoreKeys[j].property })

	if len(restore) > 0 {
		if err := a.transport.setUnitProperties(callCtx, identity.Name, true, restore); err != nil {
			return result, errors.Join(classifyTransportError("restore", identity.Name, err), conflictError(identity.Name, result.Conflicts))
		}
		after, err := a.readUnit(callCtx, identity.Name, identity.ObjectPath)
		if err != nil {
			return result, errors.Join(err, conflictError(identity.Name, result.Conflicts))
		}
		if err := requireSameIdentity("restore_readback", identity, after.Identity); err != nil {
			return result, errors.Join(err, conflictError(identity.Name, result.Conflicts))
		}
		if err := verifyReadback("restore_readback", after, restore); err != nil {
			return result, errors.Join(err, conflictError(identity.Name, result.Conflicts))
		}
		if err := a.verifier.verify(after, restore); err != nil {
			verificationErr := &AdapterError{Reason: ReasonKernelVerification, Operation: "restore_readback", Unit: identity.Name, Err: err}
			return result, errors.Join(verificationErr, conflictError(identity.Name, result.Conflicts))
		}
		for _, key := range restoreKeys {
			result.Restored = append(result.Restored, key.property)
			delete(a.leases, key)
		}
		sortPropertyNames(result.Restored)
	}
	return result, conflictError(identity.Name, result.Conflicts)
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
	sliceProperties, err := a.transport.sliceProperties(ctx, unit)
	if err != nil {
		return UnitSnapshot{}, classifyTransportError("read_slice", unit, err)
	}
	unitAfter, err := a.transport.unitProperties(ctx, unit)
	if err != nil {
		return UnitSnapshot{}, classifyTransportError("confirm_unit", unit, err)
	}
	beforeIdentity, err := parseUnitIdentity(unit, objectPath, unitBefore, sliceProperties)
	if err != nil {
		return UnitSnapshot{}, err
	}
	afterIdentity, err := parseUnitIdentity(unit, objectPath, unitAfter, sliceProperties)
	if err != nil {
		return UnitSnapshot{}, err
	}
	if beforeIdentity != afterIdentity {
		return UnitSnapshot{}, &AdapterError{Reason: ReasonUnitRecreated, Operation: "confirm_unit", Unit: unit, Err: fmt.Errorf("unit identity changed during read")}
	}
	controlGroup, ok := sliceProperties["ControlGroup"].(string)
	if !ok || !validControlGroup(controlGroup) {
		return UnitSnapshot{}, malformedReply("read_slice", unit, "ControlGroup is absent or invalid")
	}
	properties := make(map[PropertyName]uint64, len(approvedScalarProperties))
	for property := range approvedScalarProperties {
		value, ok := sliceProperties[string(property)].(uint64)
		if !ok {
			return UnitSnapshot{}, malformedReply("read_slice", unit, fmt.Sprintf("property %s is absent or is not uint64", property))
		}
		if err := validatePropertyValue(property, value); err != nil {
			return UnitSnapshot{}, malformedReply("read_slice", unit, fmt.Sprintf("property %s has invalid value: %v", property, err))
		}
		properties[property] = value
	}
	return UnitSnapshot{Identity: beforeIdentity, ControlGroup: controlGroup, Properties: newPropertySet(properties)}, nil
}

func parseUnitIdentity(unit, objectPath string, unitProperties, sliceProperties map[string]any) (UnitIdentity, error) {
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
	controlGroupID, ok := sliceProperties["ControlGroupId"].(uint64)
	if !ok || controlGroupID == 0 {
		return UnitIdentity{}, malformedReply("read_slice", unit, "ControlGroupId is absent or zero")
	}
	return UnitIdentity{Name: unit, ObjectPath: objectPath, InvocationID: invocationID, ControlGroupID: controlGroupID}, nil
}

func validateAssignments(assignments []PropertyAssignment) ([]PropertyAssignment, error) {
	if len(assignments) == 0 {
		return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Err: fmt.Errorf("at least one property assignment is required")}
	}
	if len(assignments) > len(approvedScalarProperties) {
		return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Err: fmt.Errorf("too many property assignments")}
	}
	result := append([]PropertyAssignment(nil), assignments...)
	seen := make(map[PropertyName]bool, len(result))
	for _, assignment := range result {
		if seen[assignment.name] {
			return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Property: assignment.name, Err: fmt.Errorf("duplicate property assignment")}
		}
		seen[assignment.name] = true
		if _, ok := approvedScalarProperties[assignment.name]; !ok {
			return nil, &AdapterError{Reason: ReasonPropertyNotAllowed, Operation: "apply", Property: assignment.name, Err: fmt.Errorf("property is not approved")}
		}
		if err := validatePropertyValue(assignment.name, assignment.value); err != nil {
			return nil, &AdapterError{Reason: ReasonInvalidValue, Operation: "apply", Property: assignment.name, Err: err}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result, nil
}

func verifyReadback(operation string, snapshot UnitSnapshot, assignments []PropertyAssignment) error {
	for _, assignment := range assignments {
		current, ok := snapshot.Properties.Value(assignment.name)
		if !ok || current != assignment.value {
			return &AdapterError{
				Reason:    ReasonReadbackMismatch,
				Operation: operation,
				Unit:      snapshot.Identity.Name,
				Property:  assignment.name,
				Err:       fmt.Errorf("wrote %d and read back %d", assignment.value, current),
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

func resolveUncertainOwnership(current uint64, state propertyLeaseState) (bool, propertyLeaseState) {
	if !state.uncertain {
		return current == state.lease.LastApplied, state
	}
	if current == state.lease.LastApplied {
		state.uncertain = false
		return true, state
	}
	if current == state.previousApplied {
		state.lease.LastApplied = state.previousApplied
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
	return strings.HasPrefix(value, "/") && !strings.Contains(value, "//") && !strings.Contains(value, "/../") && !strings.HasSuffix(value, "/..")
}

func malformedReply(operation, unit, message string) error {
	return &AdapterError{Reason: ReasonMalformedReply, Operation: operation, Unit: unit, Err: fmt.Errorf("%s", message)}
}

func externalConflict(unit string, property PropertyName, lastApplied, current uint64) error {
	return &AdapterError{Reason: ReasonExternalConflict, Operation: "apply", Unit: unit, Property: property, Err: fmt.Errorf("last applied %d differs from current %d", lastApplied, current)}
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
