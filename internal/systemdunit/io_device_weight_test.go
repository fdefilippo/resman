package systemdunit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNewIODeviceWeightAssignmentCanonicalizesAndMapsMechanisms(t *testing.T) {
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{
		{Path: "/dev/vdb", Weight: 333, Mechanism: IODeviceWeightMechanismIOCost},
		{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantLimits := []DeviceLimit{{Path: "/dev/vda", Value: 331}, {Path: "/dev/vdb", Value: 333}}
	if got := assignment.DeviceLimits(); !reflect.DeepEqual(got, wantLimits) {
		t.Fatalf("DeviceLimits() = %+v, want %+v", got, wantLimits)
	}
	wantRequests := []IODeviceWeightRequest{
		{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ},
		{Path: "/dev/vdb", Weight: 333, Mechanism: IODeviceWeightMechanismIOCost},
	}
	if got := assignment.IODeviceWeightRequests(); !reflect.DeepEqual(got, wantRequests) {
		t.Fatalf("IODeviceWeightRequests() = %+v, want %+v", got, wantRequests)
	}
	limits := assignment.DeviceLimits()
	limits[0].Value = 999
	requests := assignment.IODeviceWeightRequests()
	requests[0].Weight = 999
	if got := assignment.DeviceLimits()[0].Value; got != 331 {
		t.Fatalf("assignment changed through defensive copy: %d", got)
	}
}

func TestBFQIODeviceWeightDomainIsInjective(t *testing.T) {
	seen := make(map[uint64]bool, maximumIODeviceWeight)
	for weight := minimumIODeviceWeight; weight <= maximumIODeviceWeight; weight++ {
		systemdValue := systemdIODeviceWeight(weight, IODeviceWeightMechanismBFQ)
		if systemdValue > 10_000 || bfqWeight(systemdValue) != weight || seen[systemdValue] {
			t.Fatalf("BFQ mapping weight=%d systemd=%d kernel=%d duplicate=%t", weight, systemdValue, bfqWeight(systemdValue), seen[systemdValue])
		}
		seen[systemdValue] = true
	}
}

func TestNewIODeviceWeightAssignmentRejectsUntypedInvalidOrDuplicateRequests(t *testing.T) {
	tests := []struct {
		name     string
		requests []IODeviceWeightRequest
	}{
		{name: "empty"},
		{name: "below range", requests: []IODeviceWeightRequest{{Path: "/dev/vda", Mechanism: IODeviceWeightMechanismBFQ}}},
		{name: "above range", requests: []IODeviceWeightRequest{{Path: "/dev/vda", Weight: 1_001, Mechanism: IODeviceWeightMechanismBFQ}}},
		{name: "unknown mechanism", requests: []IODeviceWeightRequest{{Path: "/dev/vda", Weight: 100, Mechanism: "unknown"}}},
		{name: "relative path", requests: []IODeviceWeightRequest{{Path: "vda", Weight: 100, Mechanism: IODeviceWeightMechanismBFQ}}},
		{name: "duplicate", requests: []IODeviceWeightRequest{
			{Path: "/dev/vda", Weight: 100, Mechanism: IODeviceWeightMechanismBFQ},
			{Path: "/dev/vda", Weight: 200, Mechanism: IODeviceWeightMechanismIOCost},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewIODeviceWeightAssignment(test.requests); err == nil {
				t.Fatal("NewIODeviceWeightAssignment() error = nil")
			}
		})
	}
	if _, err := NewDevicePropertyAssignment(PropertyIODeviceWeight, []DeviceLimit{{Path: "/dev/vda", Value: 100}}); err == nil {
		t.Fatal("generic per-device constructor accepted IODeviceWeight without mechanism context")
	}
}

func TestKernelVerifierChecksExactIODeviceWeightOverrideByMechanism(t *testing.T) {
	tests := []struct {
		name       string
		mechanism  IODeviceWeightMechanism
		weight     uint64
		filename   string
		kernelData string
	}{
		{name: "bfq", mechanism: IODeviceWeightMechanismBFQ, weight: 121, filename: "io.bfq.weight", kernelData: "default 100\n8:0 121\n"},
		{name: "io cost", mechanism: IODeviceWeightMechanismIOCost, weight: 333, filename: "io.weight", kernelData: "default 100\n8:0 333\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "user.slice", "user-1000.slice")
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, test.filename), []byte(test.kernelData), 0600); err != nil {
				t.Fatal(err)
			}
			assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: test.weight, Mechanism: test.mechanism}})
			if err != nil {
				t.Fatal(err)
			}
			verifier := newCgroupVerifier(root)
			verifier.stat = func(string) (os.FileInfo, error) { return fakeBlockDeviceInfo{}, nil }
			snapshot := UnitSnapshot{Identity: UnitIdentity{Name: "user-1000.slice"}, ControlGroup: "/user.slice/user-1000.slice",
				Properties: newPropertySet(map[PropertyName]propertyValue{PropertyIODeviceWeight: assignment.value})}
			if err := verifier.verify(snapshot, []PropertyAssignment{assignment}); err != nil {
				t.Fatalf("verify() error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(path, test.filename), []byte("default 100\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifier.verify(snapshot, []PropertyAssignment{assignment}); err == nil {
				t.Fatal("verify() accepted a default-only readback")
			}
		})
	}
}

func TestKernelVerifierConfirmsIODeviceWeightResetRemovesOwnedEntry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "io.bfq.weight"), []byte("default 100\n8:1 200\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ}})
	if err != nil {
		t.Fatal(err)
	}
	assignment.value = devicePropertyValue(nil)
	verifier := newCgroupVerifier(root)
	verifier.stat = func(string) (os.FileInfo, error) { return fakeBlockDeviceInfo{}, nil }
	snapshot := UnitSnapshot{Identity: UnitIdentity{Name: "user-1000.slice"}, ControlGroup: "/user.slice/user-1000.slice",
		Properties: newPropertySet(map[PropertyName]propertyValue{PropertyIODeviceWeight: devicePropertyValue(nil)})}
	if err := verifier.verify(snapshot, []PropertyAssignment{assignment}); err != nil {
		t.Fatalf("verify(reset) error = %v", err)
	}
	if err := os.Remove(filepath.Join(path, "io.bfq.weight")); err != nil {
		t.Fatal(err)
	}
	if err := verifier.verify(snapshot, []PropertyAssignment{assignment}); err != nil {
		t.Fatalf("verify(reset without materialized interface) error = %v", err)
	}
}

func TestIODeviceWeightPreflightRequiresSelectedMechanismInterface(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.slice", "cgroup.controllers"), []byte("io\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ}})
	if err != nil {
		t.Fatal(err)
	}
	verifier := newCgroupVerifier(root)
	verifier.stat = func(string) (os.FileInfo, error) { return fakeBlockDeviceInfo{}, nil }
	if err := verifier.preflight(UnitSnapshot{Identity: UnitIdentity{Name: "user-1000.slice"}, ControlGroup: "/user.slice/user-1000.slice"}, []PropertyAssignment{assignment}); err == nil {
		t.Fatal("strict preflight accepted an absent io.bfq.weight")
	}
	if err := os.WriteFile(filepath.Join(path, "io.bfq.weight"), []byte("default 100\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifier.preflight(UnitSnapshot{Identity: UnitIdentity{Name: "user-1000.slice"}, ControlGroup: "/user.slice/user-1000.slice"}, []PropertyAssignment{assignment}); err != nil {
		t.Fatalf("strict preflight rejected qualified interface: %v", err)
	}
}

func TestIODeviceWeightLeaseSurvivesRestartAndRestoresExactBaseline(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, adapter, 1001)
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{assignment}); err != nil {
		t.Fatal(err)
	}
	if len(store.journal.Units) != 1 || len(store.journal.Units[0].Properties) != 1 {
		t.Fatalf("durable lease = %+v", store.journal)
	}
	property := store.journal.Units[0].Properties[0]
	if property.Property != PropertyIODeviceWeight || len(property.IODeviceWeightTargets) != 1 ||
		property.IODeviceWeightTargets[0].Mechanism != IODeviceWeightMechanismBFQ {
		t.Fatalf("durable typed context = %+v", property)
	}
	invalid := store.journal
	invalid.Units = append([]durableUnitLease(nil), invalid.Units...)
	invalid.Units[0].Properties = append([]durablePropertyLease(nil), invalid.Units[0].Properties...)
	invalid.Units[0].Properties[0].IODeviceWeightTargets = nil
	if err := validateDurableLeaseJournal(invalid); err == nil {
		t.Fatal("durable journal accepted IODeviceWeight without mechanism context")
	}
	transport.units[identity.Name].unit["InvocationID"] = invocationBytes(9001)
	adapter = mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	recreatedIdentity := identityFor(t, adapter, 1001)
	if recreatedIdentity == identity {
		t.Fatal("test did not recreate the unit identity")
	}
	if _, err := adapter.ConfirmApplied(context.Background(), recreatedIdentity, []PropertyAssignment{assignment}); err != nil {
		t.Fatalf("ConfirmApplied() after restart error = %v", err)
	}
	result, err := adapter.Restore(context.Background(), recreatedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Restored, []PropertyName{PropertyIODeviceWeight}) || len(store.journal.Units) != 0 {
		t.Fatalf("restore result=%+v journal=%+v", result, store.journal)
	}
	got := transport.units[recreatedIdentity.Name].slice[string(PropertyIODeviceWeight)].([]dbusDeviceLimit)
	if len(got) != 0 {
		t.Fatalf("restored IODeviceWeight = %+v, want empty baseline", got)
	}
}

func TestIODeviceWeightRestorePreservesExternalChange(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{assignment}); err != nil {
		t.Fatal(err)
	}
	transport.units[identity.Name].slice[string(PropertyIODeviceWeight)] = []dbusDeviceLimit{{Path: "/dev/vda", Value: 777}}
	result, err := adapter.Restore(context.Background(), identity)
	var conflict *RestoreConflictError
	if !errors.As(err, &conflict) || len(result.Conflicts) != 1 || len(transport.revertCalls) != 0 {
		t.Fatalf("Restore() result=%+v error=%v revert=%v", result, err, transport.revertCalls)
	}
	got := transport.units[identity.Name].slice[string(PropertyIODeviceWeight)].([]dbusDeviceLimit)
	if len(got) != 1 || got[0].Value != 777 {
		t.Fatalf("external IODeviceWeight was overwritten: %+v", got)
	}
}

func TestProbeIODeviceWeightUsesOwnedTransientUnitAndCleansSynchronously(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{}
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, verifier, store)
	targets := []IODeviceWeightProbeTarget{
		{Identity: IODeviceWeightDeviceIdentity{Number: IODeviceWeightDeviceNumber{Major: 8}, DeviceNode: "/dev/vda"}, Mechanism: IODeviceWeightMechanismBFQ},
		{Identity: IODeviceWeightDeviceIdentity{Number: IODeviceWeightDeviceNumber{Major: 8, Minor: 16}, DeviceNode: "/dev/vdb"}, Mechanism: IODeviceWeightMechanismIOCost},
	}
	wantProbe := []IODeviceWeightRequest{
		{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ},
		{Path: "/dev/vdb", Weight: 333, Mechanism: IODeviceWeightMechanismIOCost},
	}
	if err := adapter.ProbeIODeviceWeights(context.Background(), targets); err != nil {
		t.Fatalf("ProbeIODeviceWeights() error = %v", err)
	}
	if len(transport.probeStarts) != 2 || len(transport.probeStops) != 2 || len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("probe residue starts=%v stops=%v owned=%v journal=%+v", transport.probeStarts, transport.probeStops, adapter.OwnedUnits(), store.journal)
	}
	if len(verifier.preflightCalls) != 2 || verifier.preflightCalls[1][0].name != PropertyIODeviceWeight {
		t.Fatalf("strict typed preflight calls = %+v", verifier.preflightCalls)
	}
	if len(verifier.preflightApplyCalls) != 1 || verifier.preflightApplyCalls[0][0].name != PropertyIODeviceWeight {
		t.Fatalf("pre-mutation typed preflight calls = %+v", verifier.preflightApplyCalls)
	}
	found := false
	for _, call := range transport.setCalls {
		for _, applied := range call.assignments {
			if applied.name == PropertyIODeviceWeight && reflect.DeepEqual(applied.IODeviceWeightRequests(), wantProbe) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("typed IODeviceWeight never reached SetUnitProperties")
	}
}

func TestProbeIODeviceWeightsRejectsIncompleteClassifierHandoffWithoutMutation(t *testing.T) {
	transport := newFakeUnitTransport()
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	tests := [][]IODeviceWeightProbeTarget{
		nil,
		{{Identity: IODeviceWeightDeviceIdentity{Number: IODeviceWeightDeviceNumber{Major: 8}}, Mechanism: IODeviceWeightMechanismBFQ}},
		{{Identity: IODeviceWeightDeviceIdentity{Number: IODeviceWeightDeviceNumber{Major: 8}, DeviceNode: "/dev/vda"}}},
	}
	for _, targets := range tests {
		if err := adapter.ProbeIODeviceWeights(context.Background(), targets); err == nil {
			t.Fatalf("ProbeIODeviceWeights(%+v) unexpectedly succeeded", targets)
		}
	}
	if len(transport.probeStarts) != 0 || len(transport.setCalls) != 0 {
		t.Fatalf("invalid classifier handoff mutated systemd: starts=%v sets=%v", transport.probeStarts, transport.setCalls)
	}
}

func TestProbeIODeviceWeightCleansTransientUnitWhenStrictPreflightFails(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{preflightByName: map[PropertyName]error{PropertyIODeviceWeight: errors.New("qualified interface disappeared")}}
	adapter := mustTestAdapter(t, transport, verifier)
	capabilities := mustStartupCapabilities(t, StartupRequirements{IODeviceWeights: []IODeviceWeightRequest{{Path: "/dev/vda", Weight: 333, Mechanism: IODeviceWeightMechanismIOCost}}})
	err := adapter.probeStartupCapability(context.Background(), transport, capabilities[len(capabilities)-1])
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("probeStartupCapability() error = %v, want required capability", err)
	}
	if len(transport.probeStarts) != 1 || len(transport.probeStops) != 1 || len(adapter.OwnedUnits()) != 0 {
		t.Fatalf("failed probe residue starts=%v stops=%v owned=%v", transport.probeStarts, transport.probeStops, adapter.OwnedUnits())
	}
}

func TestIODeviceWeightUsesNativeDBusDeviceTuple(t *testing.T) {
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ}})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := dbusPropertyValue(assignment).([]dbusDeviceLimit)
	if !ok || !reflect.DeepEqual(got, []dbusDeviceLimit{{Path: "/dev/vda", Value: 331}}) {
		t.Fatalf("dbusPropertyValue() = %#v, want native a(st) tuple", got)
	}
}

func TestIODeviceWeightIsIndependentFromHardIOAuthority(t *testing.T) {
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ}})
	if err != nil {
		t.Fatal(err)
	}
	if resource, ok := PropertyIODeviceWeight.Resource(); !ok || resource != ResourceIOWeight {
		t.Fatalf("IODeviceWeight resource = %q, present=%t, want %q", resource, ok, ResourceIOWeight)
	}
	if _, err := validateResourceAssignments(ResourceIO, []PropertyAssignment{assignment}); err == nil {
		t.Fatal("hard-I/O authority accepted an IODeviceWeight assignment")
	}
}

func TestApplyRejectsChangedIODeviceWeightMechanismOnActiveLease(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	bfq, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 100, Mechanism: IODeviceWeightMechanismBFQ}})
	if err != nil {
		t.Fatal(err)
	}
	ioCost, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 100, Mechanism: IODeviceWeightMechanismIOCost}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{bfq}); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Apply(context.Background(), identity, []PropertyAssignment{ioCost})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Reason != ReasonExternalConflict {
		t.Fatalf("Apply(changed mechanism) error = %v, want external conflict", err)
	}
}

func TestConfirmAppliedRejectsChangedIODeviceWeightMechanism(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	identity := identityFor(t, adapter, 1001)
	bfq, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 100, Mechanism: IODeviceWeightMechanismBFQ}})
	if err != nil {
		t.Fatal(err)
	}
	ioCost, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{{Path: "/dev/vda", Weight: 100, Mechanism: IODeviceWeightMechanismIOCost}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{bfq}); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.ConfirmApplied(context.Background(), identity, []PropertyAssignment{ioCost})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Reason != ReasonReadbackMismatch {
		t.Fatalf("ConfirmApplied(changed mechanism) error = %v, want readback mismatch", err)
	}
}

func TestApplyRejectsIODeviceWeightAliasesBeforeMutation(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{"user.slice", "user.slice/user-1001.slice"} {
		if err := os.MkdirAll(filepath.Join(root, relative), 0700); err != nil {
			t.Fatal(err)
		}
	}
	transport := newFakeUnitTransport(1001)
	delete(transport.units[parentUserSlice].slice, "ControlGroupId")
	delete(transport.units["user-1001.slice"].slice, "ControlGroupId")
	verifier := newCgroupVerifier(root)
	verifier.stat = func(string) (os.FileInfo, error) { return fakeBlockDeviceInfo{}, nil }
	store := newMemoryLeaseJournalStore()
	adapter, err := newAdapter(context.Background(), transport, verifier, transport, store, DefaultCallTimeout)
	if err != nil {
		t.Fatal(err)
	}
	identity := identityFor(t, adapter, 1001)
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{
		{Path: "/dev/disk/by-path/alias", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ},
		{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{assignment}); err == nil {
		t.Fatal("Apply() accepted two paths for the same block device")
	}
	if len(transport.setCalls) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("alias rejection occurred after mutation: calls=%+v journal=%+v", transport.setCalls, store.journal)
	}
}

func TestKernelVerifierPreflightRejectsIODeviceWeightAliases(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1001.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.slice", "cgroup.controllers"), []byte("io\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "io.bfq.weight"), []byte("default 100\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assignment, err := NewIODeviceWeightAssignment([]IODeviceWeightRequest{
		{Path: "/dev/disk/by-path/alias", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ},
		{Path: "/dev/vda", Weight: 121, Mechanism: IODeviceWeightMechanismBFQ},
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier := newCgroupVerifier(root)
	verifier.stat = func(string) (os.FileInfo, error) { return fakeBlockDeviceInfo{}, nil }
	snapshot := UnitSnapshot{
		Identity:     UnitIdentity{Name: "user-1001.slice"},
		ControlGroup: "/user.slice/user-1001.slice",
	}
	if err := verifier.preflight(snapshot, []PropertyAssignment{assignment}); err == nil || !strings.Contains(err.Error(), "same device 8:0") {
		t.Fatalf("preflight() error = %v, want duplicate device 8:0", err)
	}
}
