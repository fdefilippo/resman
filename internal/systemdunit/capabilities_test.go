package systemdunit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCapabilityProbeUnitIsOneReservedTopLevelSlice(t *testing.T) {
	if capabilityProbeExecutable != "/usr/bin/sleep" {
		t.Fatalf("capabilityProbeExecutable = %q, want the packaged coreutils payload", capabilityProbeExecutable)
	}
	unit, err := newCapabilityProbeUnit()
	if err != nil {
		t.Fatal(err)
	}
	leaf := strings.TrimSuffix(unit, ".slice")
	if !isCapabilityProbeUnit(unit) || !strings.HasPrefix(unit, capabilityProbeUnitPrefix) || strings.Count(leaf, "-") != 1 {
		t.Fatalf("newCapabilityProbeUnit() = %q, want one reserved direct user.slice child", unit)
	}
	for _, invalid := range []string{
		"user-resmancapprobe.slice",
		"user-resmancapprobe0123456789abcdeg.slice",
		"user-resmancapprobe0123456789abcdef.service",
		"user-1000.slice",
	} {
		if isCapabilityProbeUnit(invalid) {
			t.Fatalf("isCapabilityProbeUnit(%q) = true", invalid)
		}
	}
	service := capabilityProbeServiceUnit(unit)
	if !strings.HasPrefix(service, "resmancapprobe") || !strings.HasSuffix(service, ".service") || strings.Contains(strings.TrimSuffix(service, ".service"), "-") {
		t.Fatalf("capabilityProbeServiceUnit(%q) = %q, want a flat dedicated service", unit, service)
	}
}

func TestStartupCapabilitiesUseProductionReadApplyConfirmAndRestore(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{}
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, verifier, store)

	if err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{Memory: true, IO: true}); err != nil {
		t.Fatalf("requireStartupCapabilities() error = %v", err)
	}
	const capabilityCount = 4
	if len(transport.probeStarts) != capabilityCount || len(transport.probeStops) != capabilityCount {
		t.Fatalf("probe calls: starts=%d stops=%d, want %d each", len(transport.probeStarts), len(transport.probeStops), capabilityCount)
	}
	if len(verifier.preflightCalls) != capabilityCount {
		t.Fatalf("preflight calls = %d, want %d", len(verifier.preflightCalls), capabilityCount)
	}
	if len(transport.setCalls) != capabilityCount*2 {
		t.Fatalf("SetUnitProperties calls = %d, want one apply and one baseline restore for each probe", len(transport.setCalls))
	}
	if len(transport.revertCalls) != capabilityCount || transport.reloadCalls != capabilityCount {
		t.Fatalf("restore calls: revert=%d reload=%d, want %d each", len(transport.revertCalls), transport.reloadCalls, capabilityCount)
	}
	if verifier.identityCalls["/user.slice"] < 2 {
		t.Fatalf("parent production identity reads = %d, want at least two", verifier.identityCalls["/user.slice"])
	}
	if !setCallsContainScalarValues(transport.setCalls, map[PropertyName]uint64{
		PropertyCPUQuotaPerSecUSec: 500_000,
		PropertyCPUQuotaPeriodUSec: 100_000,
	}) {
		t.Fatal("the exact bounded CPU quota and explicit period never reached SetUnitProperties")
	}
	if verifier.calls != capabilityCount*4 {
		t.Fatalf("kernel verification calls = %d, want apply, confirmation, baseline reset and final restoration for every probe", verifier.calls)
	}
	if len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("capability probe left ownership: memory=%v journal=%+v", adapter.OwnedUnits(), store.journal)
	}
	for _, start := range transport.probeStarts {
		if !isCapabilityProbeUnit(start.unit) {
			t.Fatalf("unexpected probe unit %q", start.unit)
		}
		if _, exists := transport.units[start.unit]; exists {
			t.Fatalf("transient probe %s remained loaded in the fake transport", start.unit)
		}
	}
}

func TestStartupValidatesParentThroughProductionPropertyAndIdentityPathBeforeProbing(t *testing.T) {
	transport := newFakeUnitTransport()
	delete(transport.units[parentUserSlice].slice, string(PropertyCPUQuotaPeriodUSec))
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{})
	assertAdapterReason(t, err, ReasonMalformedReply)
	if len(transport.probeStarts) != 0 {
		t.Fatalf("capability probes started before parent validation: %+v", transport.probeStarts)
	}
}

func TestStartupCapabilityMissingBackportedCPUPeriodFailsClosedAndCleansProbe(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.onProbeStart = func(f *fakeUnitTransport, unit string) {
		delete(f.units[unit].slice, string(PropertyCPUQuotaPeriodUSec))
	}
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{})
	assertAdapterReason(t, err, ReasonMalformedReply)
	if len(transport.probeStarts) != 1 || len(transport.probeStops) != 1 || len(adapter.OwnedUnits()) != 0 {
		t.Fatalf("failed probe cleanup: starts=%v stops=%v owned=%v", transport.probeStarts, transport.probeStops, adapter.OwnedUnits())
	}
}

func TestStartupCapabilitiesClassifyEnabledIOMissingInterfaceAndCleanProbe(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{preflightByName: map[PropertyName]error{
		PropertyIOReadBandwidthMax: errors.New("io.max is absent"),
	}}
	adapter := mustTestAdapter(t, transport, verifier)
	capabilities := mustStartupCapabilities(t, StartupRequirements{IO: true})
	capability := capabilities[len(capabilities)-1]

	err := adapter.probeStartupCapability(context.Background(), transport, capability)
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("probeStartupCapability() error = %v, want required capability", err)
	}
	for _, part := range []string{"I/O limiting", `controller "io"`, `interface "io.max"`} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("probeStartupCapability() error = %v, want %q", err, part)
		}
	}
	if len(transport.probeStarts) != 1 || len(transport.probeStops) != 1 || len(adapter.OwnedUnits()) != 0 {
		t.Fatalf("probe cleanup calls start=%v stop=%v owned=%v", transport.probeStarts, transport.probeStops, adapter.OwnedUnits())
	}
}

func TestStartupCapabilitiesCleanDurableIntentWhenApplyFails(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.setErr = errors.New("injected apply failure")
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeStartupCapability(context.Background(), transport, capability)
	if err == nil || !strings.Contains(err.Error(), "injected apply failure") {
		t.Fatalf("probeStartupCapability() error = %v, want apply failure", err)
	}
	if len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("failed apply left ownership: memory=%v journal=%+v", adapter.OwnedUnits(), store.journal)
	}
	if len(transport.probeStops) != 1 {
		t.Fatalf("failed apply did not stop its probe: %v", transport.probeStops)
	}
}

func TestStartupCapabilitiesPropagateProbeCleanupFailure(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.probeStopErr = errors.New("injected cleanup failure")
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeStartupCapability(context.Background(), transport, capability)
	if err == nil || !strings.Contains(err.Error(), "injected cleanup failure") {
		t.Fatalf("probeStartupCapability() error = %v, want cleanup failure", err)
	}
}

func TestStartupCapabilitiesCleanProbeWhenStartAcknowledgementFailsAfterCreation(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.probeStarted = true
	transport.probeStartErr = errors.New("injected post-start failure")
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeStartupCapability(context.Background(), transport, capability)
	if !IsCapabilityProbeError(err) || !strings.Contains(err.Error(), "injected post-start failure") {
		t.Fatalf("probeStartupCapability() error = %v, want typed probe-start failure", err)
	}
	if len(transport.probeStops) != 1 || transport.probeStops[0] != transport.probeStarts[0].unit {
		t.Fatalf("post-start failure cleanup start=%v stop=%v", transport.probeStarts, transport.probeStops)
	}
}

func TestStartupCapabilityProbeFailureNamesPayloadWithoutClaimingMissingInterface(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.probeStartErr = errors.New("transient service failed before execution")
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeStartupCapability(context.Background(), transport, capability)
	if !IsCapabilityProbeError(err) || IsRequiredCapabilityError(err) {
		t.Fatalf("probeStartupCapability() error = %v, want only typed probe failure", err)
	}
	for _, required := range []string{"could not start capability probe executable", capabilityProbeExecutable, "transient service failed before execution"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("probeStartupCapability() error = %v, want %q", err, required)
		}
	}
	for _, forbidden := range []string{`controller "cpu"`, `interface "cpu.max"`} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("probeStartupCapability() error = %v, must not claim missing %s", err, forbidden)
		}
	}
}

func TestStartupCleansAnInterruptedReservedProbeBeforeValidation(t *testing.T) {
	transport := newFakeUnitTransport()
	stale := capabilityProbeUnitPrefix + "0123456789abcdef.slice"
	transport.units[stale] = fakeUnit(stale, "/user.slice/"+stale, 700)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})

	if err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{}); err != nil {
		t.Fatalf("requireStartupCapabilities() error = %v", err)
	}
	if len(transport.probeStops) < 2 || transport.probeStops[0] != stale {
		t.Fatalf("interrupted probe cleanup order = %v, want %s first", transport.probeStops, stale)
	}
}

func TestStartupRecoversDurableProbeLeaseAfterDaemonInterruption(t *testing.T) {
	transport := newFakeUnitTransport()
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	unit := capabilityProbeUnitPrefix + "0123456789abcdef.slice"
	activation := mustAssignment(t, PropertyCPUWeight, 100)
	listed, started, err := transport.startCapabilityProbe(context.Background(), unit, []PropertyAssignment{activation})
	if err != nil || !started {
		t.Fatalf("startCapabilityProbe() = started %t, error %v", started, err)
	}
	snapshot, err := first.readUnit(context.Background(), listed.name, listed.objectPath)
	if err != nil {
		t.Fatal(err)
	}
	assignments := mustStartupCapabilities(t, StartupRequirements{})[0].probeAssignments
	if _, err := first.Apply(context.Background(), snapshot.Identity, assignments); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(store.journal.Units) != 1 {
		t.Fatalf("interrupted probe journal = %+v, want one durable unit", store.journal)
	}

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if err := restarted.requireStartupCapabilities(context.Background(), StartupRequirements{}); err != nil {
		t.Fatalf("requireStartupCapabilities() after interruption error = %v", err)
	}
	if restarted.ownsUnitName(unit) || len(store.journal.Units) != 0 || len(transport.diskPaths[unit]) != 0 {
		t.Fatalf("interrupted probe survived recovery: owned=%t journal=%+v files=%v", restarted.ownsUnitName(unit), store.journal, transport.diskPaths[unit])
	}
}

func mustStartupCapabilities(t *testing.T, requirements StartupRequirements) []startupCapability {
	t.Helper()
	capabilities, err := startupCapabilities(requirements)
	if err != nil {
		t.Fatal(err)
	}
	return capabilities
}

func setCallsContainScalarValues(calls []fakeSetCall, wanted map[PropertyName]uint64) bool {
	for _, call := range calls {
		seen := make(map[PropertyName]uint64, len(call.assignments))
		for _, assignment := range call.assignments {
			seen[assignment.Name()] = assignment.Value()
		}
		matched := true
		for property, value := range wanted {
			matched = matched && seen[property] == value
		}
		if matched {
			return true
		}
	}
	return false
}
