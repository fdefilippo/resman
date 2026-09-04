package systemdunit

import (
	"context"
	"errors"
	"fmt"
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
	units        map[string]*fakeUnitState
	setCalls     []fakeSetCall
	listErr      error
	unitErr      error
	sliceErr     error
	setErr       error
	ignoreWrites bool
	blockList    bool
	unitReads    int
	onUnitRead   func(*fakeUnitTransport, string, int)
	onSet        func(*fakeUnitTransport, string, []PropertyAssignment)
	closed       bool
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
		for _, assignment := range assignments {
			f.units[unit].slice[string(assignment.name)] = assignment.value
		}
	}
	return nil
}

func (f *fakeUnitTransport) close() { f.closed = true }

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

func TestRestoreRestoresOnlyStillOwnedProperties(t *testing.T) {
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
	if len(result.Restored) != 1 || result.Restored[0] != PropertyMemoryHigh {
		t.Fatalf("restored = %v", result.Restored)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0].Property != PropertyCPUWeight || result.Conflicts[0].Current != 777 {
		t.Fatalf("conflicts = %+v", result.Conflicts)
	}
	if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(777) {
		t.Fatalf("external CPUWeight was overwritten: %v", got)
	}
	if got := transport.units[identity.Name].slice[string(PropertyMemoryHigh)]; got != uint64(SystemdUnset) {
		t.Fatalf("MemoryHigh = %v, want restored unset sentinel", got)
	}
	if leases := adapter.Leases(identity); len(leases) != 0 {
		t.Fatalf("leases after restore = %+v", leases)
	}
}

func TestFailedMutationRetainsEnoughStateForExactRestoration(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	transport.setErr = errors.New("connection lost after dispatch")
	transport.onSet = func(f *fakeUnitTransport, unit string, assignments []PropertyAssignment) {
		for _, assignment := range assignments {
			f.units[unit].slice[string(assignment.name)] = assignment.value
		}
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
	adapter, err := newAdapter(transport, &fakeKernelVerifier{}, 10*time.Millisecond)
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

func TestAdapterPublicMethodsExposeNoGeneralUnitManagementCapability(t *testing.T) {
	typeOfAdapter := reflect.TypeOf((*Adapter)(nil))
	var methods []string
	for index := 0; index < typeOfAdapter.NumMethod(); index++ {
		methods = append(methods, typeOfAdapter.Method(index).Name)
	}
	sort.Strings(methods)
	want := []string{"Apply", "Close", "Discover", "Leases", "Restore"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("public Adapter methods = %v, want %v", methods, want)
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
		"ControlGroup":                     controlGroup,
		"ControlGroupId":                   uint64(seed + 100),
		string(PropertyCPUWeight):          uint64(SystemdUnset),
		string(PropertyCPUQuotaPerSecUSec): uint64(SystemdUnset),
		string(PropertyCPUQuotaPeriodUSec): uint64(SystemdUnset),
		string(PropertyMemoryHigh):         uint64(SystemdUnset),
		string(PropertyMemoryMax):          uint64(SystemdUnset),
		string(PropertyMemorySwapMax):      uint64(SystemdUnset),
		string(PropertyIOWeight):           uint64(SystemdUnset),
	}
	return &fakeUnitState{
		listed: listedUnit{name: name, objectPath: "/org/freedesktop/systemd1/unit/" + name, loadState: "loaded", activeState: "active"},
		unit: map[string]any{
			"Id":           name,
			"ActiveState":  "active",
			"InvocationID": invocationBytes(seed),
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
		result[key] = value
	}
	return result
}

func mustTestAdapter(t *testing.T, transport unitTransport, verifier kernelVerifier) *Adapter {
	t.Helper()
	adapter, err := newAdapter(transport, verifier, time.Second)
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
