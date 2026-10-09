package systemdunit

import (
	"context"
	"errors"
	"os"
	"testing"
)

func cpuRefusalVerifier() *fakeKernelVerifier {
	return &fakeKernelVerifier{preflightByName: map[PropertyName]error{
		PropertyCPUQuotaPerSecUSec: os.ErrNotExist,
	}}
}

func TestStructuralCapabilityRefusalIsTypedAndNamesTheMissingInterface(t *testing.T) {
	adapter := mustTestAdapter(t, newFakeUnitTransport(), cpuRefusalVerifier())

	refusal, err := adapter.resolveStartupCapabilities(context.Background(), StartupRequirements{})
	if err != nil {
		t.Fatalf("resolveStartupCapabilities() error = %v, want a typed refusal", err)
	}
	if refusal == nil {
		t.Fatal("resolveStartupCapabilities() accepted a host without the cpu interface")
	}
	want := MissingCapability{
		Feature: "CPU limiting", Controller: "cpu", InterfaceName: "cpu.max",
		Property: PropertyCPUQuotaPerSecUSec,
	}
	if refusal.Capability != want {
		t.Fatalf("refused capability = %+v, want %+v", refusal.Capability, want)
	}
	if !IsRequiredCapabilityError(refusal.Err) {
		t.Fatalf("refusal error = %v, want a required-capability error", refusal.Err)
	}
}

func TestTransientCapabilityFailureNeverBecomesAnObservationRefusal(t *testing.T) {
	// A probe that cannot run is not evidence that the host lacks the
	// interface, so enforcement must never be given up for it.
	transport := newFakeUnitTransport()
	transport.onProbeStart = func(f *fakeUnitTransport, unit string) {
		delete(f.units[unit].slice, string(PropertyCPUQuotaPeriodUSec))
	}
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})

	refusal, err := adapter.resolveStartupCapabilities(context.Background(), StartupRequirements{})
	if refusal != nil {
		t.Fatalf("transient failure produced an observation refusal: %+v", refusal)
	}
	assertAdapterReason(t, err, ReasonMalformedReply)
}

func TestAvailableCapabilitiesProduceNeitherRefusalNorError(t *testing.T) {
	adapter := mustTestAdapter(t, newFakeUnitTransport(), &fakeKernelVerifier{})

	refusal, err := adapter.resolveStartupCapabilities(context.Background(), StartupRequirements{})
	if refusal != nil || err != nil {
		t.Fatalf("resolveStartupCapabilities() = %+v, %v, want enforcement selected", refusal, err)
	}
}

func TestCapabilityRefusalReleasesPropertiesOwnedByAnEarlierRun(t *testing.T) {
	transport := newFakeUnitTransport(1000)
	verifier := cpuRefusalVerifier()
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, verifier, store)
	identity := identityFor(t, adapter, 1000)
	assignment, err := NewPropertyAssignment(PropertyMemoryHigh, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{assignment}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	refusal, err := adapter.resolveStartupCapabilities(context.Background(), StartupRequirements{})
	if err != nil || refusal == nil {
		t.Fatalf("resolveStartupCapabilities() = %+v, %v, want a refusal", refusal, err)
	}
	if refusal.Released.Units != 1 || refusal.Released.Properties != 1 {
		t.Fatalf("released = %+v, want one property of one unit", refusal.Released)
	}
	for _, lease := range adapter.Leases(identity) {
		if lease.Active() {
			t.Fatalf("property %s remained owned after entering observation", lease.Property)
		}
	}
}

func TestUnsafeReleaseBeforeObservationStaysAnError(t *testing.T) {
	// An externally changed property is preserved, never overwritten, so the
	// daemon must refuse to start rather than observe beside applied limits
	// it can no longer account for.
	transport := newFakeUnitTransport(1000)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, cpuRefusalVerifier(), store)
	identity := identityFor(t, adapter, 1000)
	assignment, err := NewPropertyAssignment(PropertyMemoryHigh, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{assignment}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	transport.units[identity.Name].slice[string(PropertyMemoryHigh)] = uint64(1 << 29)

	refusal, err := adapter.resolveStartupCapabilities(context.Background(), StartupRequirements{})
	if refusal != nil {
		t.Fatalf("unsafe release produced an observation refusal: %+v", refusal)
	}
	if err == nil {
		t.Fatal("resolveStartupCapabilities() accepted an unreleasable lease")
	}
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("error = %v, want the refusal retained for startup classification", err)
	}
}

func TestReleaseOwnedLeasesSkipsUnitsWithoutActiveOwnership(t *testing.T) {
	adapter := mustTestAdapter(t, newFakeUnitTransport(1000), &fakeKernelVerifier{})

	report, err := adapter.ReleaseOwnedLeases(context.Background())
	if err != nil {
		t.Fatalf("ReleaseOwnedLeases() error = %v", err)
	}
	if report.Units != 0 || report.Properties != 0 {
		t.Fatalf("report = %+v, want nothing released", report)
	}
}

func TestMissingCapabilityIsExtractedOnlyFromAStructuralRefusal(t *testing.T) {
	refusal := requiredCapabilityError("CPU limiting", "cpu", "cpu.max", PropertyCPUQuotaPerSecUSec, os.ErrNotExist)
	capability, ok := MissingCapabilityFromError(refusal)
	if !ok || capability.InterfaceName != "cpu.max" || capability.Feature != "CPU limiting" {
		t.Fatalf("MissingCapabilityFromError(%v) = %+v, %v", refusal, capability, ok)
	}
	if !errors.Is(refusal, os.ErrNotExist) {
		t.Fatal("the typed refusal lost the kernel error behind it")
	}
	for _, other := range []error{
		capabilityProbePreparationError("CPU limiting", os.ErrPermission),
		capabilityProbeStartError("CPU limiting", os.ErrPermission),
		os.ErrNotExist,
		nil,
	} {
		if _, ok := MissingCapabilityFromError(other); ok {
			t.Fatalf("MissingCapabilityFromError(%v) reported a structural absence", other)
		}
	}
}

func TestIncompleteCapabilityIsNeverPublishedAsADiagnosis(t *testing.T) {
	partial := &RequiredCapabilityError{
		Capability: MissingCapability{Feature: "CPU limiting", Controller: "cpu"},
		Err:        os.ErrNotExist,
	}
	if _, ok := MissingCapabilityFromError(partial); ok {
		t.Fatal("an incomplete capability was published as a diagnosis")
	}
}

func TestDurableLeasePresenceIsAnsweredFromTheJournalAlone(t *testing.T) {
	store := newMemoryLeaseJournalStore()
	present, err := durableLeasesPresent(store)
	if err != nil || present {
		t.Fatalf("durableLeasesPresent(empty) = %v, %v", present, err)
	}
	store.journal.Units = []durableUnitLease{{
		Unit:       "user-1000.slice",
		Properties: []durablePropertyLease{{Property: PropertyMemoryHigh}},
	}}
	present, err = durableLeasesPresent(store)
	if err != nil || !present {
		t.Fatalf("durableLeasesPresent(owned) = %v, %v", present, err)
	}
	store.journal.Units = []durableUnitLease{{Unit: "user-1000.slice"}}
	present, err = durableLeasesPresent(store)
	if err != nil || present {
		t.Fatalf("durableLeasesPresent(no property) = %v, %v", present, err)
	}
	store.loadErr = os.ErrPermission
	if _, err := durableLeasesPresent(store); err == nil {
		t.Fatal("an unreadable journal was reported as no ownership")
	}
}
