package systemdunit

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	godbus "github.com/godbus/dbus/v5"
)

type fakeUnitState struct {
	listed listedUnit
	unit   map[string]any
	slice  map[string]any
}

type fakeSetCall struct {
	unit        string
	runtime     bool
	assignments []PropertyAssignment
}

type fakeUnitTransport struct {
	units            map[string]*fakeUnitState
	setCalls         []fakeSetCall
	listErr          error
	unitErr          error
	sliceErr         error
	setErr           error
	revertErr        error
	reloadErr        error
	ignoreWrites     bool
	blockList        bool
	unitReads        int
	onUnitRead       func(*fakeUnitTransport, string, int)
	onSet            func(*fakeUnitTransport, string, []PropertyAssignment)
	onRevert         func(*fakeUnitTransport, string)
	onReload         func(*fakeUnitTransport)
	skipRevertEffect bool
	revertCalls      []string
	reloadCalls      int
	diskPaths        map[string][]string
	fingerprintSalt  map[string]string
	closed           bool
}

func (f *fakeUnitTransport) listUserSlices(ctx context.Context) ([]listedUnit, error) {
	if f.blockList {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	result := make([]listedUnit, 0, len(f.units))
	for _, unit := range f.units {
		result = append(result, unit.listed)
	}
	return result, nil
}

func (f *fakeUnitTransport) unitProperties(_ context.Context, unit string) (map[string]any, error) {
	if f.unitErr != nil {
		return nil, f.unitErr
	}
	state, ok := f.units[unit]
	if !ok {
		return nil, godbus.NewError("org.freedesktop.systemd1.NoSuchUnit", []any{unit})
	}
	f.unitReads++
	if f.onUnitRead != nil {
		f.onUnitRead(f, unit, f.unitReads)
	}
	return cloneAnyMap(state.unit), nil
}

func (f *fakeUnitTransport) sliceProperties(_ context.Context, unit string) (map[string]any, error) {
	if f.sliceErr != nil {
		return nil, f.sliceErr
	}
	state, ok := f.units[unit]
	if !ok {
		return nil, godbus.NewError("org.freedesktop.systemd1.NoSuchUnit", []any{unit})
	}
	return cloneAnyMap(state.slice), nil
}

func (f *fakeUnitTransport) setUnitProperties(_ context.Context, unit string, runtime bool, assignments []PropertyAssignment) error {
	f.setCalls = append(f.setCalls, fakeSetCall{unit: unit, runtime: runtime, assignments: append([]PropertyAssignment(nil), assignments...)})
	if f.onSet != nil {
		f.onSet(f, unit, assignments)
	}
	if f.setErr != nil {
		return f.setErr
	}
	if !f.ignoreWrites {
		f.applyAssignments(unit, assignments)
	}
	return nil
}

func (f *fakeUnitTransport) revertUnitFiles(_ context.Context, unit string) error {
	f.revertCalls = append(f.revertCalls, unit)
	if f.onRevert != nil {
		f.onRevert(f, unit)
	}
	if f.revertErr != nil {
		return f.revertErr
	}
	if !f.skipRevertEffect {
		f.applyRevert(unit)
	}
	return nil
}

func (f *fakeUnitTransport) reload(context.Context) error {
	f.reloadCalls++
	if f.reloadErr != nil {
		return f.reloadErr
	}
	if f.onReload != nil {
		f.onReload(f)
	}
	return nil
}

func (f *fakeUnitTransport) applyAssignments(unit string, assignments []PropertyAssignment) {
	state := f.units[unit]
	paths, _ := state.unit["DropInPaths"].([]string)
	seen := make(map[string]bool, len(paths)+len(assignments))
	for _, path := range paths {
		seen[path] = true
	}
	for _, assignment := range assignments {
		if _, deviceProperty := approvedDeviceProperties[assignment.name]; deviceProperty {
			values := make([]dbusDeviceLimit, len(assignment.value.devices))
			for index, value := range assignment.value.devices {
				values[index] = dbusDeviceLimit(value)
			}
			state.slice[string(assignment.name)] = values
		} else {
			state.slice[string(assignment.name)] = assignment.value.scalar
		}
		seen[managedRuntimeDropInPath(unit, assignment.name)] = true
	}
	paths = paths[:0]
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	state.unit["DropInPaths"] = paths
}

func (f *fakeUnitTransport) applyRevert(unit string) {
	state, ok := f.units[unit]
	if !ok {
		if f.diskPaths != nil {
			f.diskPaths[unit] = nil
		}
		return
	}
	for property := range approvedScalarProperties {
		state.slice[string(property)] = uint64(SystemdUnset)
	}
	for property := range approvedDeviceProperties {
		state.slice[string(property)] = []dbusDeviceLimit{}
	}
	state.unit["DropInPaths"] = []string{}
}

func (f *fakeUnitTransport) close() { f.closed = true }

func (f *fakeUnitTransport) mutablePaths(unit string) ([]string, error) {
	if paths, ok := f.diskPaths[unit]; ok {
		return append([]string(nil), paths...), nil
	}
	state, ok := f.units[unit]
	if !ok {
		return nil, os.ErrNotExist
	}
	fragmentPath, _ := state.unit["FragmentPath"].(string)
	dropInPaths, _ := state.unit["DropInPaths"].([]string)
	return mutableUnitFilePaths(unitFileSnapshot{fragmentPath: fragmentPath, dropInPaths: dropInPaths}), nil
}

func (f *fakeUnitTransport) fingerprints(paths []string) ([]unitFileFingerprint, error) {
	result := make([]unitFileFingerprint, 0, len(paths))
	for _, path := range paths {
		digest := sha256.Sum256([]byte(path + f.fingerprintSalt[path]))
		result = append(result, unitFileFingerprint{path: path, digest: digest})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].path < result[right].path })
	return result, nil
}

type memoryLeaseJournalStore struct {
	journal   durableLeaseJournal
	loadErr   error
	saveErr   error
	saveCalls int
	failSave  int
}

func newMemoryLeaseJournalStore() *memoryLeaseJournalStore {
	return &memoryLeaseJournalStore{journal: durableLeaseJournal{Version: leaseJournalVersion}}
}

func (s *memoryLeaseJournalStore) Load() (durableLeaseJournal, error) {
	return s.journal, s.loadErr
}

func (s *memoryLeaseJournalStore) Save(journal durableLeaseJournal) error {
	s.saveCalls++
	if s.saveErr != nil && (s.failSave == 0 || s.saveCalls == s.failSave) {
		return s.saveErr
	}
	s.journal = journal
	return nil
}

type fakeKernelVerifier struct {
	calls int
	err   error
}

func (v *fakeKernelVerifier) verify(UnitSnapshot, []PropertyAssignment) error {
	v.calls++
	return v.err
}

func TestDiscoverReturnsAuthoritativeSortedUserSlices(t *testing.T) {
	transport := newFakeUnitTransport(1002, 0, 1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})

	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if topology.Parent.Identity.Name != parentUserSlice || topology.Parent.ControlGroup != "/user.slice" {
		t.Fatalf("parent = %+v", topology.Parent)
	}
	wantUIDs := []uint32{0, 1001, 1002}
	gotUIDs := make([]uint32, len(topology.Users))
	for index, user := range topology.Users {
		gotUIDs[index] = user.UID
		if user.Unit.Identity.ControlGroupID == 0 || user.Unit.Identity.InvocationIDString() == "00000000000000000000000000000000" {
			t.Fatalf("user %d has unstable identity %+v", user.UID, user.Unit.Identity)
		}
	}
	if !reflect.DeepEqual(gotUIDs, wantUIDs) {
		t.Fatalf("UIDs = %v, want %v", gotUIDs, wantUIDs)
	}
}

func TestDiscoverTreatsAUserSliceThatDepartsDuringReadAsAbsent(t *testing.T) {
	transport := newFakeUnitTransport(1001, 1002)
	transport.onUnitRead = func(f *fakeUnitTransport, unit string, _ int) {
		if unit == "user-1001.slice" {
			delete(f.units, unit)
		}
	}
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})

	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(topology.Users) != 1 || topology.Users[0].UID != 1002 {
		t.Fatalf("discovered users = %+v, want only UID 1002", topology.Users)
	}
}

func TestReconcileOwnedCleansAUnitThatDepartedAfterApplication(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity := topology.Users[0].Unit.Identity
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	path := managedRuntimeDropInPath(identity.Name, PropertyCPUWeight)
	transport.diskPaths = map[string][]string{identity.Name: {path}}
	delete(transport.units, identity.Name)

	if err := adapter.ReconcileOwned(context.Background()); err != nil {
		t.Fatalf("ReconcileOwned() error: %v", err)
	}
	if got := adapter.OwnedUnits(); len(got) != 0 {
		t.Fatalf("OwnedUnits() = %+v, want empty after inactive cleanup", got)
	}
	if !reflect.DeepEqual(transport.revertCalls, []string{identity.Name}) || transport.reloadCalls != 1 {
		t.Fatalf("inactive cleanup calls = revert %v reload %d", transport.revertCalls, transport.reloadCalls)
	}
}

func TestReconcileOwnedCleansAUnitThatDepartsBetweenListAndRead(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity := topology.Users[0].Unit.Identity
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	path := managedRuntimeDropInPath(identity.Name, PropertyCPUWeight)
	transport.diskPaths = map[string][]string{identity.Name: {path}}
	transport.onUnitRead = func(f *fakeUnitTransport, unit string, _ int) {
		if unit == identity.Name {
			delete(f.units, unit)
		}
	}

	if err := adapter.ReconcileOwned(context.Background()); err != nil {
		t.Fatalf("ReconcileOwned() error: %v", err)
	}
	if got := adapter.OwnedUnits(); len(got) != 0 {
		t.Fatalf("OwnedUnits() = %+v, want empty after list/read departure", got)
	}
	if !reflect.DeepEqual(transport.revertCalls, []string{identity.Name}) || transport.reloadCalls != 1 {
		t.Fatalf("inactive cleanup calls = revert %v reload %d", transport.revertCalls, transport.reloadCalls)
	}
}

func TestReconcileOwnedReevaluatesAPriorConflict(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity := topology.Users[0].Unit.Identity
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatal(err)
	}
	transport.units[identity.Name].slice[string(PropertyCPUWeight)] = uint64(123)
	if err := adapter.ReconcileOwned(context.Background()); err == nil {
		t.Fatal("ReconcileOwned() accepted an external property conflict")
	}
	transport.units[identity.Name].slice[string(PropertyCPUWeight)] = uint64(321)
	if err := adapter.ReconcileOwned(context.Background()); err != nil {
		t.Fatalf("ReconcileOwned() retained a stale conflict: %v", err)
	}
}

func TestStartupCleansAUnitThatDepartsBetweenListAndRead(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatal(err)
	}
	path := managedRuntimeDropInPath(identity.Name, PropertyCPUWeight)
	transport.diskPaths = map[string][]string{identity.Name: {path}}
	transport.onUnitRead = func(f *fakeUnitTransport, unit string, _ int) {
		if unit == identity.Name {
			delete(f.units, unit)
		}
	}

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if got := restarted.OwnedUnits(); len(got) != 0 {
		t.Fatalf("OwnedUnits() = %+v, want empty after startup list/read departure", got)
	}
	if len(store.journal.Units) != 0 {
		t.Fatalf("durable lease remained after inactive startup cleanup: %+v", store.journal)
	}
}

func TestReconcileOwnedRebindsAnExactlyRecreatedActiveUnit(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldIdentity := topology.Users[0].Unit.Identity
	if _, err := adapter.Apply(context.Background(), oldIdentity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	recreated := fakeUnit(oldIdentity.Name, "/user.slice/"+oldIdentity.Name, 9001)
	recreated.slice[string(PropertyCPUWeight)] = uint64(321)
	recreated.unit["DropInPaths"] = []string{managedRuntimeDropInPath(oldIdentity.Name, PropertyCPUWeight)}
	transport.units[oldIdentity.Name] = recreated
	if err := adapter.ReconcileOwned(context.Background()); err != nil {
		t.Fatalf("ReconcileOwned() error: %v", err)
	}
	owned := adapter.OwnedUnits()
	if len(owned) != 1 || owned[0] == oldIdentity || owned[0].Name != oldIdentity.Name {
		t.Fatalf("OwnedUnits() = %+v, want recreated identity for %s", owned, oldIdentity.Name)
	}
}

func TestCPUQuotaAssignmentsPreserveExactCgroupMaxMeaning(t *testing.T) {
	assignments, err := NewCPUQuotaAssignmentsFromCgroupMax(360000, 100000)
	if err != nil {
		t.Fatalf("NewCPUQuotaAssignmentsFromCgroupMax() error: %v", err)
	}
	want := map[PropertyName]uint64{
		PropertyCPUQuotaPerSecUSec: 3_600_000,
		PropertyCPUQuotaPeriodUSec: 100_000,
	}
	for _, assignment := range assignments {
		if want[assignment.Name()] != assignment.Value() {
			t.Fatalf("assignment %s = %d, want %d", assignment.Name(), assignment.Value(), want[assignment.Name()])
		}
		delete(want, assignment.Name())
	}
	if len(want) != 0 {
		t.Fatalf("missing quota assignments: %v", want)
	}
	if _, err := NewCPUQuotaAssignmentsFromCgroupMax(1000, 300000); err == nil {
		t.Fatal("inexact quota period was accepted")
	}
}

func TestApplyRevalidatesAnUnchangedLeaseWithoutDBusOrJournalWrites(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	verifier := &fakeKernelVerifier{}
	store := newMemoryLeaseJournalStore()
	adapter, err := newAdapter(context.Background(), transport, verifier, transport, store, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity := topology.Users[0].Unit.Identity
	assignments := []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}
	if _, err := adapter.Apply(context.Background(), identity, assignments); err != nil {
		t.Fatalf("initial Apply() error: %v", err)
	}
	setCalls := len(transport.setCalls)
	saveCalls := store.saveCalls
	verifyCalls := verifier.calls
	unitReads := transport.unitReads

	if _, err := adapter.Apply(context.Background(), identity, assignments); err != nil {
		t.Fatalf("unchanged Apply() error: %v", err)
	}
	if len(transport.setCalls) != setCalls {
		t.Fatalf("unchanged Apply() D-Bus writes = %d, want %d", len(transport.setCalls), setCalls)
	}
	if store.saveCalls != saveCalls {
		t.Fatalf("unchanged Apply() journal writes = %d, want %d", store.saveCalls, saveCalls)
	}
	if verifier.calls != verifyCalls+1 {
		t.Fatalf("unchanged Apply() kernel verifications = %d, want %d", verifier.calls, verifyCalls+1)
	}
	if transport.unitReads <= unitReads {
		t.Fatalf("unchanged Apply() unit reads = %d, want more than %d", transport.unitReads, unitReads)
	}
}

func TestApplyUsesRuntimeOnlyReadbackAndKernelVerification(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	verifier := &fakeKernelVerifier{}
	adapter := mustTestAdapter(t, transport, verifier)
	identity := identityFor(t, adapter, 1001)
	weight := mustAssignment(t, PropertyCPUWeight, 321)
	memory := mustAssignment(t, PropertyMemoryMax, 256<<20)

	snapshot, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{memory, weight})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(transport.setCalls) != 1 || !transport.setCalls[0].runtime {
		t.Fatalf("set calls = %+v, want one runtime-only mutation", transport.setCalls)
	}
	if got, _ := snapshot.Properties.Value(PropertyCPUWeight); got != 321 {
		t.Fatalf("CPUWeight readback = %d, want 321", got)
	}
	if verifier.calls != 1 {
		t.Fatalf("kernel verifier calls = %d, want 1", verifier.calls)
	}
	leases := adapter.Leases(identity)
	if len(leases) != 2 || leases[0].Baseline != SystemdUnset || leases[0].LastApplied != 321 {
		t.Fatalf("leases = %+v", leases)
	}
}

func TestApplyRejectsSystemdReadbackMismatch(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	transport.ignoreWrites = true
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)

	_, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)})
	assertAdapterReason(t, err, ReasonReadbackMismatch)
}

func TestApplyRejectsUnitRecreatedAfterMutationBeforeReadback(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	transport.onSet = func(f *fakeUnitTransport, unit string, _ []PropertyAssignment) {
		f.units[unit].unit["InvocationID"] = invocationBytes(77)
	}

	_, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)})
	assertAdapterReason(t, err, ReasonUnitRecreated)
}

func TestReadConfirmsOneUnitLifetimeAroundPropertySnapshot(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	parentReads := 0
	transport.onUnitRead = func(f *fakeUnitTransport, unit string, _ int) {
		if unit == parentUserSlice {
			parentReads++
		}
		if unit == parentUserSlice && parentReads == 2 {
			f.units[unit].unit["InvocationID"] = invocationBytes(88)
		}
	}
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})

	_, err := adapter.Discover(context.Background())
	assertAdapterReason(t, err, ReasonUnitRecreated)
}

func TestRestorePreservesExternalConflictAndRestoresOtherProperties(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	weight := mustAssignment(t, PropertyCPUWeight, 400)
	memory := mustAssignment(t, PropertyMemoryHigh, 64<<20)
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{weight, memory}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	transport.units[identity.Name].slice[string(PropertyCPUWeight)] = uint64(777)

	result, err := adapter.Restore(context.Background(), identity)
	var conflict *RestoreConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Restore() error = %v, want RestoreConflictError", err)
	}
	if !reflect.DeepEqual(result.Restored, []PropertyName{PropertyMemoryHigh}) {
		t.Fatalf("restored = %v, want MemoryHigh", result.Restored)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0].Property != PropertyCPUWeight || result.Conflicts[0].Current != 777 {
		t.Fatalf("conflicts = %+v", result.Conflicts)
	}
	if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(777) {
		t.Fatalf("external CPUWeight was overwritten: %v", got)
	}
	if got := transport.units[identity.Name].slice[string(PropertyMemoryHigh)]; got != uint64(SystemdUnset) {
		t.Fatalf("MemoryHigh = %v, want independently restored baseline", got)
	}
	if len(transport.revertCalls) != 0 {
		t.Fatalf("revert calls = %v, want none", transport.revertCalls)
	}
	if leases := adapter.Leases(identity); len(leases) != 2 {
		t.Fatalf("leases after conflict = %+v, want both retained", leases)
	}
	beforeCalls := len(transport.setCalls)
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{weight}); err == nil {
		t.Fatal("Apply() after external conflict error = nil")
	} else {
		assertAdapterReason(t, err, ReasonExternalConflict)
	}
	if len(transport.setCalls) != beforeCalls {
		t.Fatalf("set calls after repeated conflict = %d, want %d", len(transport.setCalls), beforeCalls)
	}
}

func TestFailedMutationRetainsEnoughStateForExactRestoration(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	transport.setErr = errors.New("connection lost after dispatch")
	transport.onSet = func(f *fakeUnitTransport, unit string, assignments []PropertyAssignment) {
		f.applyAssignments(unit, assignments)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 500)}); err == nil {
		t.Fatal("Apply() error = nil, want transport failure")
	}
	transport.setErr = nil
	transport.onSet = nil

	result, err := adapter.Restore(context.Background(), identity)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if len(result.Restored) != 1 || result.Restored[0] != PropertyCPUWeight {
		t.Fatalf("restored = %v", result.Restored)
	}
	if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(SystemdUnset) {
		t.Fatalf("CPUWeight = %v, want restored unset sentinel", got)
	}
}

func TestRestoreLostReplyIsResolvedWithoutAFalseExternalConflict(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 500)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	transport.revertErr = errors.New("connection lost after revert dispatch")
	transport.onRevert = func(f *fakeUnitTransport, unit string) { f.applyRevert(unit) }
	if _, err := adapter.Restore(context.Background(), identity); err == nil {
		t.Fatal("first Restore() error = nil, want lost-reply error")
	}
	transport.revertErr = nil
	transport.onRevert = nil

	result, err := adapter.Restore(context.Background(), identity)
	if err != nil {
		t.Fatalf("second Restore() error = %v", err)
	}
	if !reflect.DeepEqual(result.Restored, []PropertyName{PropertyCPUWeight}) || len(result.Conflicts) != 0 {
		t.Fatalf("second Restore() result = %+v", result)
	}
	if len(transport.revertCalls) != 1 {
		t.Fatalf("revert calls = %v, want no duplicate after the lost reply", transport.revertCalls)
	}
	if leases := adapter.Leases(identity); len(leases) != 0 {
		t.Fatalf("leases after resolved restoration = %+v", leases)
	}
}

func TestApplyAndRestoreGuardTheCompleteMutableUnitFileFootprint(t *testing.T) {
	t.Run("preexisting operator drop-in prevents the first mutation", func(t *testing.T) {
		transport := newFakeUnitTransport(1001)
		transport.units["user-1001.slice"].unit["DropInPaths"] = []string{"/etc/systemd/system/user-1001.slice.d/10-operator.conf"}
		adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
		identity := identityFor(t, adapter, 1001)

		_, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 500)})
		assertAdapterReason(t, err, ReasonExternalConflict)
		if len(transport.setCalls) != 0 {
			t.Fatalf("set calls = %d, want zero", len(transport.setCalls))
		}
	})

	t.Run("operator drop-in not yet loaded by systemd prevents mutation", func(t *testing.T) {
		transport := newFakeUnitTransport(1001)
		transport.diskPaths = map[string][]string{
			"user-1001.slice": {"/etc/systemd/system/user-1001.slice.d/10-unloaded.conf"},
		}
		adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
		identity := identityFor(t, adapter, 1001)

		_, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 500)})
		assertAdapterReason(t, err, ReasonExternalConflict)
		if len(transport.setCalls) != 0 {
			t.Fatalf("set calls = %d, want zero", len(transport.setCalls))
		}
	})

	t.Run("operator drop-in added after apply prevents cleanup", func(t *testing.T) {
		transport := newFakeUnitTransport(1001)
		adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
		identity := identityFor(t, adapter, 1001)
		if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 500)}); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
		paths := transport.units[identity.Name].unit["DropInPaths"].([]string)
		operatorPath := "/etc/systemd/system/user-1001.slice.d/90-operator.conf"
		transport.units[identity.Name].unit["DropInPaths"] = append(paths, operatorPath)

		_, err := adapter.Restore(context.Background(), identity)
		assertAdapterReason(t, err, ReasonExternalConflict)
		if len(transport.revertCalls) != 0 {
			t.Fatalf("revert calls = %v, want none", transport.revertCalls)
		}
		if got := transport.units[identity.Name].unit["DropInPaths"].([]string); !slicesContain(got, operatorPath) {
			t.Fatalf("operator drop-in was not preserved: %v", got)
		}
	})
}

func TestRepeatedApplyPreservesTheFirstBaseline(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	for _, weight := range []uint64{200, 700} {
		if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, weight)}); err != nil {
			t.Fatalf("Apply(%d) error = %v", weight, err)
		}
	}
	leases := adapter.Leases(identity)
	if len(leases) != 1 || leases[0].Baseline != SystemdUnset || leases[0].LastApplied != 700 {
		t.Fatalf("leases = %+v", leases)
	}
	if _, err := adapter.Restore(context.Background(), identity); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(SystemdUnset) {
		t.Fatalf("CPUWeight = %v, want original unset baseline", got)
	}
}

func TestApplyPersistsWriteAheadOwnershipBeforeTheFirstMutation(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	store.saveErr = errors.New("storage unavailable")
	store.failSave = 1
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, adapter, 1001)

	_, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)})
	assertAdapterReason(t, err, ReasonLeaseStore)
	if len(transport.setCalls) != 0 {
		t.Fatalf("SetUnitProperties calls = %d, want zero before durable ownership", len(transport.setCalls))
	}
	if len(adapter.Leases(identity)) != 0 {
		t.Fatal("failed durable staging became visible in memory")
	}
}

func TestStartupResolvesAnApplyThatSucceededBeforeConfirmationPersistence(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	store.saveErr = errors.New("confirmation persistence unavailable")
	store.failSave = 2
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)

	_, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)})
	assertAdapterReason(t, err, ReasonLeaseStore)
	if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(321) {
		t.Fatalf("CPUWeight = %v, want dispatched value", got)
	}
	if store.journal.Units[0].Phase != leasePhaseApplying {
		t.Fatalf("durable phase = %q, want uncertain apply", store.journal.Units[0].Phase)
	}
	setCalls := len(transport.setCalls)

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if got := restarted.Leases(identity); len(got) != 1 || got[0].LastApplied != 321 {
		t.Fatalf("recovered leases = %+v", got)
	}
	if len(transport.setCalls) != setCalls {
		t.Fatal("startup repeated an already dispatched Apply")
	}
	if store.journal.Units[0].Phase != leasePhaseApplied {
		t.Fatalf("recovered durable phase = %q, want applied", store.journal.Units[0].Phase)
	}
}

func TestStartupDiscardsAnApplyWhoseMutationWasNeverDispatched(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	transport.setErr = errors.New("connection failed before dispatch")
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err == nil {
		t.Fatal("Apply() error = nil")
	}
	transport.setErr = nil

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if got := restarted.Leases(identity); len(got) != 0 {
		t.Fatalf("undispatched leases = %+v, want none", got)
	}
	if len(store.journal.Units) != 0 {
		t.Fatalf("undispatched durable record remained: %+v", store.journal)
	}
}

func TestRestorePersistsIntentBeforeRevertUnitFiles(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, adapter, 1001)
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	store.saveErr = errors.New("restore staging unavailable")
	store.failSave = 3
	_, err := adapter.Restore(context.Background(), identity)
	assertAdapterReason(t, err, ReasonLeaseStore)
	if len(transport.revertCalls) != 0 {
		t.Fatalf("RevertUnitFiles calls = %v, want none before durable restore intent", transport.revertCalls)
	}
	if store.journal.Units[0].Phase != leasePhaseApplied {
		t.Fatalf("durable phase = %q, want applied", store.journal.Units[0].Phase)
	}
}

func TestStartupReclaimsConfirmedLeaseWithoutRewritingTheUnit(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	setCalls := len(transport.setCalls)

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if got := restarted.Leases(identity); len(got) != 1 || got[0].LastApplied != 321 {
		t.Fatalf("reclaimed leases = %+v", got)
	}
	if len(transport.setCalls) != setCalls {
		t.Fatalf("startup rewrote the unit: calls = %d, want %d", len(transport.setCalls), setCalls)
	}
	want := []LeaseRecoveryOutcome{{Unit: identity.Name, State: LeaseRecoveryReclaimed}}
	if got := restarted.RecoveryReport(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecoveryReport() = %+v, want %+v", got, want)
	}
}

func TestStartupRebindsAnExactOrphanedFootprintToARecreatedUnit(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	oldIdentity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), oldIdentity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	transport.units[oldIdentity.Name].unit["InvocationID"] = invocationBytes(9001)
	transport.units[oldIdentity.Name].slice["ControlGroupId"] = uint64(9002)

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	newIdentity := identityFor(t, restarted, 1001)
	if newIdentity == oldIdentity {
		t.Fatal("test did not recreate the unit identity")
	}
	if got := restarted.Leases(newIdentity); len(got) != 1 || got[0].LastApplied != 321 {
		t.Fatalf("rebound leases = %+v", got)
	}
	want := []LeaseRecoveryOutcome{{Unit: oldIdentity.Name, State: LeaseRecoveryOrphaned}}
	if got := restarted.RecoveryReport(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecoveryReport() = %+v, want %+v", got, want)
	}
	if _, err := restarted.Restore(context.Background(), newIdentity); err != nil {
		t.Fatalf("Restore() after identity rebound error = %v", err)
	}
	if len(store.journal.Units) != 0 {
		t.Fatalf("durable lease remained after restoration: %+v", store.journal)
	}
}

func TestStartupCompletesCrashBetweenRevertUnitFilesAndReload(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	transport.skipRevertEffect = true
	transport.diskPaths = map[string][]string{identity.Name: nil}
	transport.onRevert = func(f *fakeUnitTransport, unit string) {
		// Real systemd removes the files but keeps DropInPaths and normalized
		// property values stale until Reload completes.
		f.diskPaths[unit] = nil
	}
	transport.onReload = func(f *fakeUnitTransport) {
		f.units[identity.Name].unit["DropInPaths"] = []string{}
		f.units[identity.Name].slice[string(PropertyCPUWeight)] = uint64(SystemdUnset)
	}
	transport.reloadErr = errors.New("simulated crash before Reload completes")
	if _, err := first.Restore(context.Background(), identity); err == nil {
		t.Fatal("Restore() error = nil, want interrupted reload")
	}
	if store.journal.Units[0].Phase != leasePhaseReloading {
		t.Fatalf("durable phase = %q, want %q", store.journal.Units[0].Phase, leasePhaseReloading)
	}
	if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(321) {
		t.Fatalf("pre-reload CPUWeight = %v, want last applied value", got)
	}
	if got := transport.units[identity.Name].unit["DropInPaths"].([]string); len(got) != 1 {
		t.Fatalf("pre-reload D-Bus DropInPaths = %v, want stale managed path", got)
	}

	transport.reloadErr = nil
	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if len(store.journal.Units) != 0 {
		t.Fatalf("midpoint lease remained after startup recovery: %+v", store.journal)
	}
	if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(SystemdUnset) {
		t.Fatalf("recovered CPUWeight = %v, want baseline", got)
	}
	if got := restarted.RecoveryReport(); len(got) != 1 || got[0].State != LeaseRecoveryReclaimed {
		t.Fatalf("RecoveryReport() = %+v", got)
	}
}

func TestStartupDoesNotRevertADivergentInactiveFootprint(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	paths := append([]string(nil), transport.units[identity.Name].unit["DropInPaths"].([]string)...)
	transport.diskPaths = map[string][]string{identity.Name: paths}
	transport.fingerprintSalt = map[string]string{paths[0]: "operator-content"}
	delete(transport.units, identity.Name)

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	want := []LeaseRecoveryOutcome{{Unit: identity.Name, State: LeaseRecoveryConflict}}
	if got := restarted.RecoveryReport(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecoveryReport() = %+v, want %+v", got, want)
	}
	if len(transport.revertCalls) != 0 {
		t.Fatalf("divergent inactive footprint was reverted: %v", transport.revertCalls)
	}
	if len(store.journal.Units) != 1 {
		t.Fatal("divergent inactive lease was deleted")
	}
}

func TestApplyRefusesAUnitBlockedDuringStartupRecovery(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	setCalls := len(transport.setCalls)
	transport.unitErr = errors.New("temporary read failure during startup recovery")
	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	transport.unitErr = nil

	if _, err := restarted.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 400)}); err == nil {
		t.Fatal("Apply() accepted a unit blocked during startup recovery")
	}
	if len(transport.setCalls) != setCalls {
		t.Fatalf("blocked Apply reached D-Bus: calls=%d, want %d", len(transport.setCalls), setCalls)
	}
}

func TestStartupCompletesRestoreWhoseFinalJournalRemovalFailed(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	store.saveErr = errors.New("final journal removal unavailable")
	store.failSave = 5
	if _, err := first.Restore(context.Background(), identity); err == nil {
		t.Fatal("Restore() error = nil, want final persistence failure")
	}
	if store.journal.Units[0].Phase != leasePhaseReloading {
		t.Fatalf("durable phase = %q, want reloading", store.journal.Units[0].Phase)
	}
	if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(SystemdUnset) {
		t.Fatalf("restored CPUWeight = %v, want baseline", got)
	}

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if len(store.journal.Units) != 0 {
		t.Fatalf("completed restore remained durable: %+v", store.journal)
	}
	if got := restarted.RecoveryReport(); len(got) != 1 || got[0].State != LeaseRecoveryReclaimed {
		t.Fatalf("RecoveryReport() = %+v", got)
	}
}

func TestAdapterConstructionFailsClosedWhenTheJournalCannotBeLoaded(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	store.loadErr = errors.New("corrupt journal")
	_, err := newAdapter(context.Background(), transport, &fakeKernelVerifier{}, transport, store, time.Second)
	assertAdapterReason(t, err, ReasonLeaseStore)
	if len(transport.setCalls) != 0 || len(transport.revertCalls) != 0 {
		t.Fatalf("invalid journal caused mutation: set=%d revert=%d", len(transport.setCalls), len(transport.revertCalls))
	}
}

func TestStartupCleansAnExactRecordedFootprintForAnInactiveUnitWithoutLoadingIt(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	paths := append([]string(nil), transport.units[identity.Name].unit["DropInPaths"].([]string)...)
	transport.diskPaths = map[string][]string{identity.Name: paths}
	delete(transport.units, identity.Name)

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if len(transport.revertCalls) != 1 || transport.revertCalls[0] != identity.Name {
		t.Fatalf("inactive cleanup calls = %v", transport.revertCalls)
	}
	if len(store.journal.Units) != 0 {
		t.Fatalf("inactive lease remained after cleanup: %+v", store.journal)
	}
	want := []LeaseRecoveryOutcome{{Unit: identity.Name, State: LeaseRecoveryInactive}}
	if got := restarted.RecoveryReport(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecoveryReport() = %+v, want %+v", got, want)
	}
}

func TestStartupReportsExactInactiveLeaseAsPendingWhenCleanupCannotComplete(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	paths := append([]string(nil), transport.units[identity.Name].unit["DropInPaths"].([]string)...)
	transport.diskPaths = map[string][]string{identity.Name: paths}
	delete(transport.units, identity.Name)
	transport.revertErr = errors.New("system bus temporarily unavailable")

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	want := []LeaseRecoveryOutcome{{Unit: identity.Name, State: LeaseRecoveryPending}}
	if got := restarted.RecoveryReport(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecoveryReport() = %+v, want %+v", got, want)
	}
	if len(store.journal.Units) != 1 {
		t.Fatal("pending inactive lease was deleted")
	}
	if _, err := restarted.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 400)}); err == nil {
		t.Fatal("Apply() accepted a unit with pending inactive reconciliation")
	}
}

func TestStartupRetainsDivergentRecordedStateAsAnExternalConflict(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, first, 1001)
	if _, err := first.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	path := managedRuntimeDropInPath(identity.Name, PropertyCPUWeight)
	transport.fingerprintSalt = map[string]string{path: "externally-rewritten"}
	setCalls := len(transport.setCalls)

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	want := []LeaseRecoveryOutcome{{Unit: identity.Name, State: LeaseRecoveryConflict}}
	if got := restarted.RecoveryReport(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecoveryReport() = %+v, want %+v", got, want)
	}
	if _, err := restarted.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 400)}); err == nil {
		t.Fatal("Apply() accepted a conflicted durable footprint")
	}
	if len(transport.setCalls) != setCalls || len(transport.revertCalls) != 0 {
		t.Fatalf("conflict caused mutation: set=%d revert=%v", len(transport.setCalls), transport.revertCalls)
	}
	if len(store.journal.Units) != 1 {
		t.Fatal("conflicted durable record was deleted")
	}
}

func TestUnitRecreationFailsClosedBeforeMutation(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	transport.units[identity.Name].unit["InvocationID"] = invocationBytes(99)

	_, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 500)})
	assertAdapterReason(t, err, ReasonUnitRecreated)
	if len(transport.setCalls) != 0 {
		t.Fatalf("set calls = %d, want zero", len(transport.setCalls))
	}
}

func TestMalformedRepliesAndTransportFailuresAreTyped(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeUnitTransport)
		want   ErrorReason
	}{
		{name: "missing parent", mutate: func(f *fakeUnitTransport) { delete(f.units, parentUserSlice) }, want: ReasonUnitMissing},
		{name: "malformed property", mutate: func(f *fakeUnitTransport) { f.units[parentUserSlice].slice[string(PropertyCPUWeight)] = "100" }, want: ReasonMalformedReply},
		{name: "authorization denied", mutate: func(f *fakeUnitTransport) {
			f.listErr = godbus.NewError("org.freedesktop.DBus.Error.AccessDenied", nil)
		}, want: ReasonAuthorizationDenied},
		{name: "bus loss", mutate: func(f *fakeUnitTransport) { f.listErr = errors.New("disconnected") }, want: ReasonBusUnavailable},
		{name: "missing unit value error", mutate: func(f *fakeUnitTransport) {
			f.listErr = godbus.MakeNoObjectError(godbus.ObjectPath("/org/freedesktop/systemd1/unit/user_2d1001_2eslice"))
		}, want: ReasonUnitMissing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newFakeUnitTransport(1001)
			test.mutate(transport)
			adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
			_, err := adapter.Discover(context.Background())
			assertAdapterReason(t, err, test.want)
		})
	}
}

func TestCallsAreBoundedByAdapterTimeout(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.blockList = true
	adapter, err := newAdapter(context.Background(), transport, &fakeKernelVerifier{}, transport, newMemoryLeaseJournalStore(), 10*time.Millisecond)
	if err != nil {
		t.Fatalf("newAdapter() error = %v", err)
	}
	started := time.Now()
	_, err = adapter.Discover(context.Background())
	assertAdapterReason(t, err, ReasonTimeout)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded call took %s", elapsed)
	}
}

func TestPositivePropertyAllowlistRejectsUnknownAndInvalidValues(t *testing.T) {
	if _, err := NewPropertyAssignment("ExecStart", 1); err == nil {
		t.Fatal("ExecStart was accepted")
	} else {
		assertAdapterReason(t, err, ReasonPropertyNotAllowed)
	}
	if _, err := NewPropertyAssignment(PropertyCPUWeight, 0); err == nil {
		t.Fatal("CPUWeight=0 was accepted")
	} else {
		assertAdapterReason(t, err, ReasonInvalidValue)
	}
	if _, err := NewPropertyAssignment(PropertyCPUQuotaPeriodUSec, 999); err == nil {
		t.Fatal("CPUQuotaPeriodUSec=999 was accepted")
	} else {
		assertAdapterReason(t, err, ReasonInvalidValue)
	}
}

func TestDevicePropertyParserAcceptsDynamicDBusTuples(t *testing.T) {
	got, err := parseDevicePropertyValue([][]interface{}{
		{"/dev/sdb", uint64(2 << 20)},
		{"/dev/sda", uint64(1 << 20)},
	})
	if err != nil {
		t.Fatalf("parseDevicePropertyValue() error = %v", err)
	}
	want := []DeviceLimit{{Path: "/dev/sda", Value: 1 << 20}, {Path: "/dev/sdb", Value: 2 << 20}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDevicePropertyValue() = %v, want %v", got, want)
	}
}

func TestAdapterPublicMethodsExposeNoGeneralUnitManagementCapability(t *testing.T) {
	typeOfAdapter := reflect.TypeOf((*Adapter)(nil))
	var methods []string
	for index := 0; index < typeOfAdapter.NumMethod(); index++ {
		methods = append(methods, typeOfAdapter.Method(index).Name)
	}
	sort.Strings(methods)
	want := []string{"Apply", "CheckResourceAuthorities", "CheckResourceAuthority", "Close", "Discover", "Leases", "OwnedUnits", "ReconcileOwned", "RecoveryReport", "Restore", "RestoreProperties"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("public Adapter methods = %v, want %v", methods, want)
	}
}

func TestAdapterPersistsAndRestoresStructuredIODeviceLimits(t *testing.T) {
	transport := newFakeUnitTransport(1000)
	store := newMemoryLeaseJournalStore()
	adapter, err := newAdapter(context.Background(), transport, &fakeKernelVerifier{}, transport, store, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity := topology.Users[0].Unit.Identity
	memory := mustAssignment(t, PropertyMemoryHigh, 64<<20)
	read, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{memory, read}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	leases := adapter.Leases(identity)
	deviceLeases := 0
	for _, lease := range leases {
		if len(lease.LastAppliedDeviceLimits) == 1 {
			deviceLeases++
		}
	}
	if len(leases) != 2 || deviceLeases != 1 {
		t.Fatalf("leases = %+v, want scalar and structured ownership", leases)
	}

	restarted, err := newAdapter(context.Background(), transport, &fakeKernelVerifier{}, transport, store, time.Second)
	if err != nil {
		t.Fatalf("restart adapter: %v", err)
	}
	if _, err := restarted.RestoreProperties(context.Background(), identity, []PropertyName{PropertyMemoryHigh}); err != nil {
		t.Fatalf("RestoreProperties() error: %v", err)
	}
	if got := transport.units[identity.Name].slice[string(PropertyMemoryHigh)]; got != uint64(SystemdUnset) {
		t.Fatalf("MemoryHigh after selected restore = %v", got)
	}
	if got := transport.units[identity.Name].slice[string(PropertyIOReadBandwidthMax)].([]dbusDeviceLimit); len(got) != 1 || got[0].Value != 1<<20 {
		t.Fatalf("I/O limit changed during memory-only restore: %+v", got)
	}
	if len(transport.revertCalls) != 0 {
		t.Fatal("selected property restoration reverted unrelated unit overrides")
	}
	if _, err := restarted.Restore(context.Background(), identity); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
	if got := transport.units[identity.Name].slice[string(PropertyIOReadBandwidthMax)].([]dbusDeviceLimit); len(got) != 0 {
		t.Fatalf("I/O limit after complete restore = %+v", got)
	}
	if len(store.journal.Units) != 0 {
		t.Fatalf("durable journal retained restored unit: %+v", store.journal.Units)
	}
}

func TestRestorePropertiesRestoresOwnedValuesAndPreservesExternalConflict(t *testing.T) {
	transport := newFakeUnitTransport(1000)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	topology, _ := adapter.Discover(context.Background())
	identity := topology.Users[0].Unit.Identity
	memory := mustAssignment(t, PropertyMemoryMax, 128<<20)
	read, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{memory, read}); err != nil {
		t.Fatal(err)
	}
	transport.units[identity.Name].slice[string(PropertyIOReadBandwidthMax)] = []dbusDeviceLimit{{Path: "/dev/vda", Value: 2 << 20}}
	result, err := adapter.RestoreProperties(context.Background(), identity, []PropertyName{PropertyMemoryMax, PropertyIOReadBandwidthMax})
	var conflict *RestoreConflictError
	if !errors.As(err, &conflict) || len(result.Conflicts) != 1 || result.Conflicts[0].Property != PropertyIOReadBandwidthMax {
		t.Fatalf("result=%+v error=%v, want one I/O conflict", result, err)
	}
	if got := transport.units[identity.Name].slice[string(PropertyMemoryMax)]; got != uint64(SystemdUnset) {
		t.Fatalf("non-conflicting MemoryMax was not restored: %v", got)
	}
	got := transport.units[identity.Name].slice[string(PropertyIOReadBandwidthMax)].([]dbusDeviceLimit)
	if len(got) != 1 || got[0].Value != 2<<20 {
		t.Fatalf("external I/O limit was overwritten: %+v", got)
	}
}

func TestCompleteRestoreStillRestoresOwnedPropertiesWhenAnotherPropertyConflicts(t *testing.T) {
	transport := newFakeUnitTransport(1000)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	topology, _ := adapter.Discover(context.Background())
	identity := topology.Users[0].Unit.Identity
	memory := mustAssignment(t, PropertyMemoryMax, 128<<20)
	read, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{memory, read}); err != nil {
		t.Fatal(err)
	}
	transport.units[identity.Name].slice[string(PropertyIOReadBandwidthMax)] = []dbusDeviceLimit{{Path: "/dev/vda", Value: 2 << 20}}
	result, err := adapter.Restore(context.Background(), identity)
	var conflict *RestoreConflictError
	if !errors.As(err, &conflict) || len(result.Conflicts) != 1 || result.Conflicts[0].Property != PropertyIOReadBandwidthMax {
		t.Fatalf("result=%+v error=%v, want one I/O conflict", result, err)
	}
	if got := transport.units[identity.Name].slice[string(PropertyMemoryMax)]; got != uint64(SystemdUnset) {
		t.Fatalf("non-conflicting MemoryMax was not restored: %v", got)
	}
	got := transport.units[identity.Name].slice[string(PropertyIOReadBandwidthMax)].([]dbusDeviceLimit)
	if len(got) != 1 || got[0].Value != 2<<20 {
		t.Fatalf("external I/O limit was overwritten: %+v", got)
	}
	if len(transport.revertCalls) != 0 {
		t.Fatalf("unit-wide revert ran despite an external conflict: %v", transport.revertCalls)
	}
}

func newFakeUnitTransport(uids ...uint32) *fakeUnitTransport {
	transport := &fakeUnitTransport{units: make(map[string]*fakeUnitState)}
	transport.units[parentUserSlice] = fakeUnit(parentUserSlice, "/user.slice", 1)
	for _, uid := range uids {
		name := fmt.Sprintf("user-%d.slice", uid)
		transport.units[name] = fakeUnit(name, fmt.Sprintf("/user.slice/%s", name), uid+2)
	}
	return transport
}

func fakeUnit(name, controlGroup string, seed uint32) *fakeUnitState {
	properties := map[string]any{
		"ControlGroup":                      controlGroup,
		"ControlGroupId":                    uint64(seed + 100),
		string(PropertyCPUWeight):           uint64(SystemdUnset),
		string(PropertyCPUQuotaPerSecUSec):  uint64(SystemdUnset),
		string(PropertyCPUQuotaPeriodUSec):  uint64(SystemdUnset),
		string(PropertyMemoryHigh):          uint64(SystemdUnset),
		string(PropertyMemoryMax):           uint64(SystemdUnset),
		string(PropertyMemorySwapMax):       uint64(SystemdUnset),
		string(PropertyIOWeight):            uint64(SystemdUnset),
		string(PropertyIOReadBandwidthMax):  []dbusDeviceLimit{},
		string(PropertyIOWriteBandwidthMax): []dbusDeviceLimit{},
		string(PropertyIOReadIOPSMax):       []dbusDeviceLimit{},
		string(PropertyIOWriteIOPSMax):      []dbusDeviceLimit{},
	}
	return &fakeUnitState{
		listed: listedUnit{name: name, objectPath: "/org/freedesktop/systemd1/unit/" + name, loadState: "loaded", activeState: "active"},
		unit: map[string]any{
			"Id":           name,
			"ActiveState":  "active",
			"InvocationID": invocationBytes(seed),
			"FragmentPath": "",
			"DropInPaths":  []string{},
		},
		slice: properties,
	}
}

func invocationBytes(seed uint32) []byte {
	result := make([]byte, 16)
	for index := range result {
		result[index] = byte(seed + uint32(index) + 1)
	}
	return result
}

func cloneAnyMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		if bytes, ok := value.([]byte); ok {
			result[key] = append([]byte(nil), bytes...)
			continue
		}
		if strings, ok := value.([]string); ok {
			result[key] = append([]string(nil), strings...)
			continue
		}
		result[key] = value
	}
	return result
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mustTestAdapter(t *testing.T, transport unitTransport, verifier kernelVerifier) *Adapter {
	t.Helper()
	return mustTestAdapterWithStore(t, transport, verifier, newMemoryLeaseJournalStore())
}

func mustTestAdapterWithStore(t *testing.T, transport unitTransport, verifier kernelVerifier, store leaseJournalStore) *Adapter {
	t.Helper()
	inspector, ok := transport.(unitFileInspector)
	if !ok {
		t.Fatal("test transport does not implement unitFileInspector")
	}
	adapter, err := newAdapter(context.Background(), transport, verifier, inspector, store, time.Second)
	if err != nil {
		t.Fatalf("newAdapter() error = %v", err)
	}
	return adapter
}

func identityFor(t *testing.T, adapter *Adapter, uid uint32) UnitIdentity {
	t.Helper()
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	for _, user := range topology.Users {
		if user.UID == uid {
			return user.Unit.Identity
		}
	}
	t.Fatalf("UID %d was not discovered", uid)
	return UnitIdentity{}
}

func mustAssignment(t *testing.T, name PropertyName, value uint64) PropertyAssignment {
	t.Helper()
	assignment, err := NewPropertyAssignment(name, value)
	if err != nil {
		t.Fatalf("NewPropertyAssignment(%s, %d) error = %v", name, value, err)
	}
	return assignment
}

func assertAdapterReason(t *testing.T, err error, want ErrorReason) {
	t.Helper()
	var typed *AdapterError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want AdapterError", err)
	}
	if typed.Reason != want {
		t.Fatalf("reason = %q, want %q (error: %v)", typed.Reason, want, err)
	}
}
