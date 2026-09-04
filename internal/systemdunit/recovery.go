package systemdunit

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

type adapterLeaseSnapshot struct {
	leases     map[propertyLeaseKey]propertyLeaseState
	overrides  map[UnitIdentity]unitOverrideLease
	phases     map[string]leasePhase
	generation uint64
}

func (a *Adapter) loadAndReconcile(ctx context.Context) error {
	journal, err := a.store.Load()
	if err != nil {
		return leaseStoreError("load", err)
	}
	a.generation = journal.Generation
	if err := a.importJournal(journal); err != nil {
		return leaseStoreError("load", err)
	}
	if len(journal.Units) == 0 {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, a.timeout)
	listed, err := a.transport.listUserSlices(listCtx)
	cancel()
	if err != nil {
		return classifyTransportError("recover_discover", "", err)
	}
	active := make(map[string]listedUnit, len(listed))
	for _, unit := range listed {
		active[unit.name] = unit
	}
	for _, record := range journal.Units {
		callCtx, cancel := context.WithTimeout(ctx, a.timeout)
		listedUnit, ok := active[record.Unit]
		if !ok {
			if err := a.recoverInactive(callCtx, record.Unit); err != nil {
				a.recordInactiveRecoveryFailure(record.Unit, err)
			}
			cancel()
			continue
		}
		current, err := a.readUnit(callCtx, record.Unit, listedUnit.objectPath)
		if err != nil {
			a.recordRecoveryConflict(record.Unit, err)
			cancel()
			continue
		}
		if err := a.recoverActive(callCtx, record.Unit, current); err != nil {
			a.recordRecoveryConflict(record.Unit, err)
		}
		cancel()
	}
	return nil
}

func (a *Adapter) recoverActive(ctx context.Context, unit string, current UnitSnapshot) error {
	identity, override, phase, ok := a.unitLeaseState(unit)
	if !ok {
		return fmt.Errorf("durable unit %s has no in-memory lease state", unit)
	}
	actual, err := a.captureFootprint(current)
	if err != nil {
		return err
	}
	sameIdentity := identity == current.Identity
	switch phase {
	case leasePhaseApplied:
		if !a.propertiesMatch(unit, func(state propertyLeaseState) uint64 { return state.lease.LastApplied }, current) || !equalFingerprints(actual, override.fingerprints) {
			return externalRecoveryConflict(unit, "recorded applied values or footprint differ from current state")
		}
		a.rebindUnit(identity, current.Identity)
		if !sameIdentity {
			if err := a.persistLeaseState(); err != nil {
				a.rebindUnit(current.Identity, identity)
				return err
			}
		}
		state := LeaseRecoveryReclaimed
		if !sameIdentity {
			state = LeaseRecoveryOrphaned
		}
		a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: state})
		return nil
	case leasePhaseApplying:
		if a.propertiesMatch(unit, func(state propertyLeaseState) uint64 { return state.lease.LastApplied }, current) && pendingFootprintMatches(actual, override) {
			before := a.snapshotLeaseState()
			a.rebindUnit(identity, current.Identity)
			a.confirmAppliedUnit(unit, actual)
			if err := a.persistLeaseState(); err != nil {
				a.restoreLeaseState(before)
				return err
			}
			state := LeaseRecoveryReclaimed
			if !sameIdentity {
				state = LeaseRecoveryOrphaned
			}
			a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: state})
			return nil
		}
		if sameIdentity && a.propertiesMatch(unit, func(state propertyLeaseState) uint64 { return state.previousApplied }, current) && equalFingerprints(actual, override.previousFingerprint) {
			return a.rollbackUndispatchedApply(unit)
		}
		return externalRecoveryConflict(unit, "uncertain apply cannot be resolved from current values and footprint")
	case leasePhaseRestoring, leasePhaseReloading:
		return a.resumeRestore(ctx, unit, current, actual)
	default:
		return fmt.Errorf("unit %s has unsupported recovery phase %q", unit, phase)
	}
}

func (a *Adapter) recoverInactive(ctx context.Context, unit string) error {
	_, override, phase, ok := a.unitLeaseState(unit)
	if !ok {
		return fmt.Errorf("durable inactive unit %s has no in-memory lease state", unit)
	}
	paths, err := a.unitFiles.mutablePaths(unit)
	if err != nil {
		return &AdapterError{Reason: ReasonUnitFileVerification, Operation: "recover_inactive", Unit: unit, Err: err}
	}
	actual, err := a.unitFiles.fingerprints(paths)
	if err != nil {
		return &AdapterError{Reason: ReasonUnitFileVerification, Operation: "recover_inactive", Unit: unit, Err: err}
	}
	if len(actual) == 0 && phase == leasePhaseApplying && len(override.previousFingerprint) == 0 {
		if err := a.rollbackUndispatchedApply(unit); err != nil {
			return err
		}
		a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: LeaseRecoveryInactive})
		return nil
	}
	if len(actual) == 0 && (phase == leasePhaseRestoring || phase == leasePhaseReloading) {
		if err := a.transport.reload(ctx); err != nil {
			return classifyTransportError("recover_inactive_reload", unit, err)
		}
		if err := a.removeUnitLeaseDurably(unit); err != nil {
			return err
		}
		a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: LeaseRecoveryInactive})
		return nil
	}
	exact := equalFingerprints(actual, override.fingerprints)
	if phase == leasePhaseApplying && pendingFootprintMatches(actual, override) {
		exact = true
	}
	if !exact {
		return externalRecoveryConflict(unit, "inactive unit footprint differs from the durable ownership record")
	}
	if phase == leasePhaseReloading {
		return externalRecoveryConflict(unit, "inactive unit footprint reappeared after RevertUnitFiles")
	}
	if phase == leasePhaseApplied || phase == leasePhaseApplying {
		before := a.snapshotLeaseState()
		a.stageUnitRestore(unit)
		if err := a.persistLeaseState(); err != nil {
			a.restoreLeaseState(before)
			return err
		}
	}
	if err := a.transport.revertUnitFiles(ctx, unit); err != nil {
		return classifyTransportError("recover_inactive_revert", unit, err)
	}
	before := a.snapshotLeaseState()
	a.phases[unit] = leasePhaseReloading
	if err := a.persistLeaseState(); err != nil {
		a.restoreLeaseState(before)
		return err
	}
	if err := a.transport.reload(ctx); err != nil {
		return classifyTransportError("recover_inactive_reload", unit, err)
	}
	paths, err = a.unitFiles.mutablePaths(unit)
	if err != nil {
		return &AdapterError{Reason: ReasonUnitFileVerification, Operation: "recover_inactive_readback", Unit: unit, Err: err}
	}
	if len(paths) != 0 {
		return externalRecoveryConflict(unit, "guarded inactive-unit cleanup left a mutable footprint")
	}
	if err := a.removeUnitLeaseDurably(unit); err != nil {
		return err
	}
	a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: LeaseRecoveryInactive})
	return nil
}

func (a *Adapter) resumeRestore(ctx context.Context, unit string, current UnitSnapshot, actual []unitFileFingerprint) error {
	_, override, phase, _ := a.unitLeaseState(unit)
	if len(actual) == 0 {
		if err := a.transport.reload(ctx); err != nil {
			return classifyTransportError("recover_restore_reload", unit, err)
		}
		refreshed, err := a.readUnit(ctx, unit, current.Identity.ObjectPath)
		if err != nil {
			return err
		}
		if !a.propertiesMatch(unit, func(state propertyLeaseState) uint64 { return state.lease.Baseline }, refreshed) {
			return externalRecoveryConflict(unit, "post-revert properties do not match the recorded baselines")
		}
		if err := a.removeUnitLeaseDurably(unit); err != nil {
			return err
		}
		a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: LeaseRecoveryReclaimed})
		return nil
	}
	if phase != leasePhaseRestoring || !equalFingerprints(actual, override.fingerprints) || !a.propertiesMatch(unit, func(state propertyLeaseState) uint64 { return state.previousApplied }, current) {
		return externalRecoveryConflict(unit, "uncertain restore cannot be resolved from current values and footprint")
	}
	if err := a.transport.revertUnitFiles(ctx, unit); err != nil {
		return classifyTransportError("recover_restore_revert", unit, err)
	}
	before := a.snapshotLeaseState()
	a.phases[unit] = leasePhaseReloading
	if err := a.persistLeaseState(); err != nil {
		a.restoreLeaseState(before)
		return err
	}
	if err := a.transport.reload(ctx); err != nil {
		return classifyTransportError("recover_restore_reload", unit, err)
	}
	refreshed, err := a.readUnit(ctx, unit, current.Identity.ObjectPath)
	if err != nil {
		return err
	}
	if !a.propertiesMatch(unit, func(state propertyLeaseState) uint64 { return state.lease.Baseline }, refreshed) {
		return externalRecoveryConflict(unit, "restored properties do not match the recorded baselines")
	}
	if err := a.removeUnitLeaseDurably(unit); err != nil {
		return err
	}
	a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: LeaseRecoveryReclaimed})
	return nil
}

func (a *Adapter) importJournal(journal durableLeaseJournal) error {
	for _, unit := range journal.Units {
		identity, err := identityFromDurable(unit.Unit, unit.Identity)
		if err != nil {
			return err
		}
		override := unitOverrideLease{
			managedPaths:        durableFootprintPaths(unit.Footprint),
			fingerprints:        fingerprintsFromDurable(unit.Footprint),
			previousFingerprint: fingerprintsFromDurable(unit.PreviousFootprint),
		}
		a.overrides[identity] = override
		a.phases[unit.Unit] = unit.Phase
		for _, property := range unit.Properties {
			a.leases[propertyLeaseKey{identity: identity, property: property.Property}] = propertyLeaseState{
				lease:           PropertyLease{Property: property.Property, Baseline: property.Baseline, LastApplied: property.LastApplied},
				previousApplied: property.PreviousApplied,
				uncertain:       property.Uncertain,
				newLease:        property.NewLease,
			}
		}
	}
	return nil
}

func (a *Adapter) persistLeaseState() error {
	nextGeneration := a.generation + 1
	journal, err := a.exportJournal(nextGeneration)
	if err != nil {
		return leaseStoreError("encode", err)
	}
	if err := a.store.Save(journal); err != nil {
		return leaseStoreError("persist", err)
	}
	a.generation = nextGeneration
	return nil
}

func (a *Adapter) exportJournal(generation uint64) (durableLeaseJournal, error) {
	journal := durableLeaseJournal{Version: leaseJournalVersion, Generation: generation}
	identities := make([]UnitIdentity, 0, len(a.overrides))
	for identity := range a.overrides {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(left, right int) bool { return identities[left].Name < identities[right].Name })
	for _, identity := range identities {
		override := a.overrides[identity]
		unit := durableUnitLease{
			Unit:              identity.Name,
			Identity:          identityToDurable(identity),
			Phase:             a.phases[identity.Name],
			Footprint:         durableFootprint(override.managedPaths, override.fingerprints),
			PreviousFootprint: durableFingerprints(override.previousFingerprint),
		}
		for key, state := range a.leases {
			if key.identity != identity {
				continue
			}
			unit.Properties = append(unit.Properties, durablePropertyLease{
				Property: key.property, Baseline: state.lease.Baseline, PreviousApplied: state.previousApplied,
				LastApplied: state.lease.LastApplied, Uncertain: state.uncertain, NewLease: state.newLease,
			})
		}
		journal.Units = append(journal.Units, unit)
	}
	sortDurableJournal(&journal)
	if err := validateDurableLeaseJournal(journal); err != nil {
		return durableLeaseJournal{}, err
	}
	return journal, nil
}

func (a *Adapter) snapshotLeaseState() adapterLeaseSnapshot {
	result := adapterLeaseSnapshot{
		leases: make(map[propertyLeaseKey]propertyLeaseState, len(a.leases)), overrides: make(map[UnitIdentity]unitOverrideLease, len(a.overrides)),
		phases: make(map[string]leasePhase, len(a.phases)), generation: a.generation,
	}
	for key, value := range a.leases {
		result.leases[key] = value
	}
	for key, value := range a.overrides {
		value.managedPaths = append([]string(nil), value.managedPaths...)
		value.fingerprints = append([]unitFileFingerprint(nil), value.fingerprints...)
		value.previousFingerprint = append([]unitFileFingerprint(nil), value.previousFingerprint...)
		result.overrides[key] = value
	}
	for key, value := range a.phases {
		result.phases[key] = value
	}
	return result
}

func (a *Adapter) restoreLeaseState(snapshot adapterLeaseSnapshot) {
	a.leases = snapshot.leases
	a.overrides = snapshot.overrides
	a.phases = snapshot.phases
	a.generation = snapshot.generation
}

func (a *Adapter) unitLeaseState(unit string) (UnitIdentity, unitOverrideLease, leasePhase, bool) {
	for identity, override := range a.overrides {
		if identity.Name == unit {
			return identity, override, a.phases[unit], true
		}
	}
	return UnitIdentity{}, unitOverrideLease{}, "", false
}

func (a *Adapter) propertiesMatch(unit string, want func(propertyLeaseState) uint64, snapshot UnitSnapshot) bool {
	identity, _, _, ok := a.unitLeaseState(unit)
	if !ok {
		return false
	}
	matched := 0
	for key, state := range a.leases {
		if key.identity != identity {
			continue
		}
		current, exists := snapshot.Properties.Value(key.property)
		if !exists || current != want(state) {
			return false
		}
		matched++
	}
	return matched > 0
}

func (a *Adapter) rebindUnit(oldIdentity, newIdentity UnitIdentity) {
	if oldIdentity == newIdentity {
		return
	}
	override := a.overrides[oldIdentity]
	delete(a.overrides, oldIdentity)
	a.overrides[newIdentity] = override
	for key, state := range a.leases {
		if key.identity != oldIdentity {
			continue
		}
		delete(a.leases, key)
		key.identity = newIdentity
		a.leases[key] = state
	}
}

func (a *Adapter) confirmAppliedUnit(unit string, actual []unitFileFingerprint) {
	identity, override, _, _ := a.unitLeaseState(unit)
	override.fingerprints = append([]unitFileFingerprint(nil), actual...)
	override.previousFingerprint = nil
	a.overrides[identity] = override
	a.phases[unit] = leasePhaseApplied
	for key, state := range a.leases {
		if key.identity != identity {
			continue
		}
		state.uncertain = false
		state.newLease = false
		a.leases[key] = state
	}
}

func (a *Adapter) stageUnitRestore(unit string) {
	identity, _, _, ok := a.unitLeaseState(unit)
	if !ok {
		return
	}
	for key, state := range a.leases {
		if key.identity != identity {
			continue
		}
		state.previousApplied = state.lease.LastApplied
		state.lease.LastApplied = state.lease.Baseline
		state.uncertain = true
		state.newLease = false
		a.leases[key] = state
	}
	a.phases[unit] = leasePhaseRestoring
}

func (a *Adapter) rollbackUndispatchedApply(unit string) error {
	before := a.snapshotLeaseState()
	identity, override, _, _ := a.unitLeaseState(unit)
	for key, state := range a.leases {
		if key.identity != identity || !state.uncertain {
			continue
		}
		if state.newLease {
			delete(a.leases, key)
			continue
		}
		state.lease.LastApplied = state.previousApplied
		state.uncertain = false
		state.newLease = false
		a.leases[key] = state
	}
	override.managedPaths = fingerprintPaths(override.previousFingerprint)
	override.fingerprints = append([]unitFileFingerprint(nil), override.previousFingerprint...)
	override.previousFingerprint = nil
	if !a.hasUnitProperties(identity) {
		delete(a.overrides, identity)
		delete(a.phases, unit)
	} else {
		a.overrides[identity] = override
		a.phases[unit] = leasePhaseApplied
	}
	if err := a.persistLeaseState(); err != nil {
		a.restoreLeaseState(before)
		return err
	}
	return nil
}

func (a *Adapter) removeUnitLeaseDurably(unit string) error {
	before := a.snapshotLeaseState()
	identity, _, _, ok := a.unitLeaseState(unit)
	if ok {
		for key := range a.leases {
			if key.identity == identity {
				delete(a.leases, key)
			}
		}
		delete(a.overrides, identity)
	}
	delete(a.phases, unit)
	if err := a.persistLeaseState(); err != nil {
		a.restoreLeaseState(before)
		return err
	}
	return nil
}

func (a *Adapter) hasUnitProperties(identity UnitIdentity) bool {
	for key := range a.leases {
		if key.identity == identity {
			return true
		}
	}
	return false
}

func (a *Adapter) captureFootprint(snapshot UnitSnapshot) ([]unitFileFingerprint, error) {
	paths, err := a.combinedMutableUnitFilePaths(snapshot)
	if err != nil {
		return nil, err
	}
	result, err := a.unitFiles.fingerprints(paths)
	if err != nil {
		return nil, &AdapterError{Reason: ReasonUnitFileVerification, Operation: "fingerprint_unit_files", Unit: snapshot.Identity.Name, Err: err}
	}
	return result, nil
}

func (a *Adapter) recordRecoveryConflict(unit string, err error) {
	if err == nil {
		return
	}
	a.blocked[unit] = err
	a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: LeaseRecoveryConflict})
}

func (a *Adapter) recordInactiveRecoveryFailure(unit string, err error) {
	if err == nil {
		return
	}
	state := LeaseRecoveryPending
	var adapterErr *AdapterError
	if errors.As(err, &adapterErr) && adapterErr.Reason == ReasonExternalConflict {
		state = LeaseRecoveryConflict
	}
	a.blocked[unit] = err
	a.recovery = append(a.recovery, LeaseRecoveryOutcome{Unit: unit, State: state})
}

func identityToDurable(identity UnitIdentity) durableUnitIdentity {
	return durableUnitIdentity{ObjectPath: identity.ObjectPath, InvocationID: identity.InvocationIDString(), ControlGroupID: identity.ControlGroupID}
}

func identityFromDurable(unit string, value durableUnitIdentity) (UnitIdentity, error) {
	decoded, err := hex.DecodeString(value.InvocationID)
	if err != nil || len(decoded) != 16 {
		return UnitIdentity{}, fmt.Errorf("unit %s has invalid durable invocation ID", unit)
	}
	var invocationID [16]byte
	copy(invocationID[:], decoded)
	return UnitIdentity{Name: unit, ObjectPath: value.ObjectPath, InvocationID: invocationID, ControlGroupID: value.ControlGroupID}, nil
}

func durableFootprint(paths []string, fingerprints []unitFileFingerprint) []durableFileFingerprint {
	digests := make(map[string]string, len(fingerprints))
	for _, fingerprint := range fingerprints {
		digests[fingerprint.path] = hex.EncodeToString(fingerprint.digest[:])
	}
	result := make([]durableFileFingerprint, 0, len(paths))
	for _, path := range paths {
		result = append(result, durableFileFingerprint{Path: path, SHA256: digests[path]})
	}
	return result
}

func durableFingerprints(fingerprints []unitFileFingerprint) []durableFileFingerprint {
	return durableFootprint(fingerprintPaths(fingerprints), fingerprints)
}

func durableFootprintPaths(fingerprints []durableFileFingerprint) []string {
	result := make([]string, 0, len(fingerprints))
	for _, fingerprint := range fingerprints {
		result = append(result, fingerprint.Path)
	}
	return result
}

func fingerprintsFromDurable(values []durableFileFingerprint) []unitFileFingerprint {
	result := make([]unitFileFingerprint, 0, len(values))
	for _, value := range values {
		if value.SHA256 == "" {
			continue
		}
		decoded, _ := hex.DecodeString(value.SHA256)
		var digest [32]byte
		copy(digest[:], decoded)
		result = append(result, unitFileFingerprint{path: value.Path, digest: digest})
	}
	return result
}

func fingerprintPaths(values []unitFileFingerprint) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.path)
	}
	return result
}

func equalFingerprints(left, right []unitFileFingerprint) bool {
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

func pathsMatchFootprint(actual []unitFileFingerprint, expected []string) bool {
	return equalStrings(fingerprintPaths(actual), expected)
}

func pendingFootprintMatches(actual []unitFileFingerprint, expected unitOverrideLease) bool {
	if !pathsMatchFootprint(actual, expected.managedPaths) {
		return false
	}
	actualByPath := make(map[string][32]byte, len(actual))
	for _, fingerprint := range actual {
		actualByPath[fingerprint.path] = fingerprint.digest
	}
	for _, fingerprint := range expected.fingerprints {
		if actualByPath[fingerprint.path] != fingerprint.digest {
			return false
		}
	}
	return true
}

func externalRecoveryConflict(unit, message string) error {
	return &AdapterError{Reason: ReasonExternalConflict, Operation: "recover", Unit: unit, Err: errors.New(message)}
}

func leaseStoreError(operation string, err error) error {
	return &AdapterError{Reason: ReasonLeaseStore, Operation: operation, Err: err}
}
