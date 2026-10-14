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
		"user-resmancapprobe0123456789abcd.slice",
		"user-resmancapprobe0123456789abcde.slice",
		"user-resmancapprobe0123456789abcdef0.slice",
		"user-resmancapprobe0123456789abcdef01.slice",
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

func TestStartupCapabilitiesProbeUserSliceWithoutTransientUnits(t *testing.T) {
	// resman-318.2: a transient probe slice below user.slice inherits
	// After=systemd-user-sessions.service and deadlocks the early start. The
	// startup probe therefore writes user.slice itself and starts no unit.
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{}
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, verifier, store)

	if err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{Memory: true, IO: true}); err != nil {
		t.Fatalf("requireStartupCapabilities() error = %v", err)
	}
	const capabilityCount = 4
	if len(transport.probeStarts) != 0 || len(transport.probeStops) != 0 {
		t.Fatalf("startup started transient units: starts=%v stops=%v", transport.probeStarts, transport.probeStops)
	}
	for _, call := range transport.setCalls {
		if call.unit != parentUserSlice {
			t.Fatalf("startup probe wrote %s, want only %s", call.unit, parentUserSlice)
		}
	}
	if len(verifier.preflightCalls) != capabilityCount {
		t.Fatalf("preflight calls = %d, want one interface check per capability", len(verifier.preflightCalls))
	}
	if !setCallsContainScalarValues(transport.setCalls, map[PropertyName]uint64{
		PropertyCPUQuotaPerSecUSec: 500_000,
		PropertyCPUQuotaPeriodUSec: 100_000,
	}) {
		t.Fatal("the exact bounded CPU quota and explicit period never reached SetUnitProperties")
	}
	if setCallsContainScalarValues(transport.setCalls, map[PropertyName]uint64{PropertyCPUWeight: 100}) {
		t.Fatal("the probe wrote a CPU weight, whose restoration fails where cpu is not delegated")
	}
	if !setCallsContainScalarValues(transport.setCalls, map[PropertyName]uint64{PropertyIOWeight: 1_000}) {
		t.Fatal("the non-vacuous I/O weight never reached SetUnitProperties")
	}
	if len(transport.revertCalls) != 1 || transport.revertCalls[0] != parentUserSlice || transport.reloadCalls != 1 {
		t.Fatalf("restore calls: revert=%v reload=%d, want one complete user.slice restore", transport.revertCalls, transport.reloadCalls)
	}
	if len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("capability probe left ownership: memory=%v journal=%+v", adapter.OwnedUnits(), store.journal)
	}
	for _, property := range []PropertyName{PropertyCPUWeight, PropertyCPUQuotaPerSecUSec, PropertyIOWeight, PropertyMemoryMax} {
		value, ok := transport.units[parentUserSlice].slice[string(property)]
		if !ok || value != uint64(SystemdUnset) {
			t.Fatalf("user.slice %s = %v after the probe, want its baseline", property, value)
		}
	}
}

func TestStartupCapabilityMissingOnUserSliceIsADefinitiveRefusal(t *testing.T) {
	// On a CONFIG_RT_GROUP_SCHED kernel with a realtime task outside the root,
	// systemd's +cpu write fails and user.slice never exposes cpu.max: the
	// activation readback fails, and that must read as a missing capability.
	transport := newFakeUnitTransport()
	verifier := &absentCPUInterfaceVerifier{fakeKernelVerifier: fakeKernelVerifier{
		preflightByName: map[PropertyName]error{PropertyCPUQuotaPerSecUSec: errors.New("cpu.max is absent")},
	}}
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, verifier, store)

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{})
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("requireStartupCapabilities() error = %v, want required capability", err)
	}
	for _, part := range []string{"CPU limiting", `controller "cpu"`, `interface "cpu.max"`} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("requireStartupCapabilities() error = %v, want %q", err, part)
		}
	}
	if len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("refused probe left ownership (error %v): memory=%v journal=%+v", err, adapter.OwnedUnits(), store.journal)
	}
}

// absentCPUInterfaceVerifier models a user.slice without cpu.max: like the
// production verifier, a set CPU quota cannot be read back, while the unset
// baseline of an absent interface verifies.
type absentCPUInterfaceVerifier struct {
	fakeKernelVerifier
}

func (v *absentCPUInterfaceVerifier) verify(snapshot UnitSnapshot, assignments []PropertyAssignment) error {
	v.calls++
	for _, assignment := range assignments {
		if assignment.name != PropertyCPUQuotaPerSecUSec && assignment.name != PropertyCPUQuotaPeriodUSec {
			continue
		}
		perSecond, _ := snapshot.Properties.Value(PropertyCPUQuotaPerSecUSec)
		period, _ := snapshot.Properties.Value(PropertyCPUQuotaPeriodUSec)
		if perSecond != SystemdUnset || period != SystemdUnset {
			return errors.New("read effective cpu.max: no such file or directory")
		}
	}
	return nil
}

func TestStartupCapabilityInterfaceMissingAfterActivationIsADefinitiveRefusal(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{preflightByName: map[PropertyName]error{
		PropertyIOReadBandwidthMax: errors.New("io.max is absent"),
	}}
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, verifier, store)

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{IO: true})
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("requireStartupCapabilities() error = %v, want required capability", err)
	}
	for _, part := range []string{"I/O limiting", `controller "io"`, `interface "io.max"`} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("requireStartupCapabilities() error = %v, want %q", err, part)
		}
	}
	if len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("refused probe left ownership: memory=%v journal=%+v", adapter.OwnedUnits(), store.journal)
	}
}

func TestStartupCapabilityApplyFailureIsNotAMissingCapability(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.setErr = errors.New("connection reset by peer")
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{})
	if err == nil || IsRequiredCapabilityError(err) || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("requireStartupCapabilities() error = %v, want the transport failure", err)
	}
}

func TestStartupCapabilityRestoreFailureReportsTheSurvivingLease(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.revertErr = errors.New("injected restore failure")
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{})
	if err == nil || !strings.Contains(err.Error(), "injected restore failure") ||
		!strings.Contains(err.Error(), "left a durable property lease on user.slice") {
		t.Fatalf("requireStartupCapabilities() error = %v, want the restore failure and the surviving lease", err)
	}
	if len(store.journal.Units) != 1 {
		t.Fatalf("durable lease after a failed restore = %+v, want it kept for recovery", store.journal)
	}
}

func TestStartupCapabilityAcceptsStoppedProbeWithVanishedRuntimeFootprint(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.onProbeStop = func(f *fakeUnitTransport, unit string, _ *fakeUnitState) {
		// Real systemd can remove the transient slice and its complete runtime
		// property footprint as one stop operation.
		f.diskPaths[unit] = nil
	}
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	if err := adapter.probeTransientCapability(context.Background(), transport, capability); err != nil {
		t.Fatalf("probeTransientCapability() error = %v", err)
	}
	if len(transport.revertCalls) != 0 || transport.reloadCalls != 0 {
		t.Fatalf("vanished probe footprint triggered revert=%v reload=%d", transport.revertCalls, transport.reloadCalls)
	}
	if len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("vanished probe footprint left ownership: memory=%v journal=%+v", adapter.OwnedUnits(), store.journal)
	}
}

func TestStartupValidatesParentThroughProductionPropertyAndIdentityPathBeforeProbing(t *testing.T) {
	transport := newFakeUnitTransport()
	delete(transport.units[parentUserSlice].slice, string(PropertyCPUQuotaPeriodUSec))
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{})
	assertAdapterReason(t, err, ReasonMalformedReply)
	if len(transport.setCalls) != 0 || len(transport.probeStarts) != 0 {
		t.Fatalf("the probe wrote before parent validation: set=%+v starts=%+v", transport.setCalls, transport.probeStarts)
	}
}

func TestStartupCapabilityMissingBackportedCPUPeriodFailsClosedAndCleansProbe(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.onProbeStart = func(f *fakeUnitTransport, unit string) {
		delete(f.units[unit].slice, string(PropertyCPUQuotaPeriodUSec))
	}
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
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

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("probeTransientCapability() error = %v, want required capability", err)
	}
	for _, part := range []string{"I/O limiting", `controller "io"`, `interface "io.max"`} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("probeTransientCapability() error = %v, want %q", err, part)
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

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	if err == nil || !strings.Contains(err.Error(), "injected apply failure") {
		t.Fatalf("probeTransientCapability() error = %v, want apply failure", err)
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

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	if err == nil || !strings.Contains(err.Error(), "injected cleanup failure") {
		t.Fatalf("probeTransientCapability() error = %v, want cleanup failure", err)
	}
}

func TestStartupCapabilityReportsMutableUnitFilesLeftAfterStop(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.onProbeStop = func(f *fakeUnitTransport, unit string, _ *fakeUnitState) {
		if f.diskPaths == nil {
			f.diskPaths = make(map[string][]string)
		}
		f.diskPaths[unit] = []string{"/run/systemd/system.control/" + unit + ".d/50-CPUWeight.conf"}
	}
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	if err == nil || !strings.Contains(err.Error(), "left mutable unit files") {
		t.Fatalf("probeTransientCapability() error = %v, want mutable unit-file residue", err)
	}
}

func TestStartupCapabilityReportsDurableLeaseLeftAfterCleanup(t *testing.T) {
	transport := newFakeUnitTransport()
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]
	transport.onProbeStop = func(_ *fakeUnitTransport, _ string, _ *fakeUnitState) {
		store.saveErr = errors.New("injected journal cleanup failure")
	}

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	if err == nil || !strings.Contains(err.Error(), "injected journal cleanup failure") ||
		!strings.Contains(err.Error(), "left a durable property lease") {
		t.Fatalf("probeTransientCapability() error = %v, want durable lease residue", err)
	}
	if len(store.journal.Units) != 1 {
		t.Fatalf("durable probe residue = %+v, want one unit", store.journal)
	}
}

func TestStartupCapabilityRequiresInactiveReconciliationToReleaseEveryLease(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.revertErr = errors.New("injected inactive cleanup failure")
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	if err == nil || !strings.Contains(err.Error(), "injected inactive cleanup failure") ||
		!strings.Contains(err.Error(), "left a durable property lease") {
		t.Fatalf("probeTransientCapability() error = %v, want incomplete inactive reconciliation", err)
	}
	if len(adapter.OwnedUnits()) != 1 || len(store.journal.Units) != 1 {
		t.Fatalf("failed inactive reconciliation lost ownership: memory=%v journal=%+v", adapter.OwnedUnits(), store.journal)
	}
	transport.revertErr = nil
	if err := adapter.ReconcileOwned(context.Background()); err != nil {
		t.Fatalf("ReconcileOwned() retry error = %v", err)
	}
	if len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("inactive reconciliation retry left ownership: memory=%v journal=%+v", adapter.OwnedUnits(), store.journal)
	}
}

func TestStartupCapabilitiesCleanProbeWhenStartAcknowledgementFailsAfterCreation(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.probeStarted = true
	transport.probeStartErr = errors.New("injected post-start failure")
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	if !IsCapabilityProbeError(err) || !strings.Contains(err.Error(), "injected post-start failure") {
		t.Fatalf("probeTransientCapability() error = %v, want typed probe-start failure", err)
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

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	if !IsCapabilityProbeError(err) || IsRequiredCapabilityError(err) {
		t.Fatalf("probeTransientCapability() error = %v, want only typed probe failure", err)
	}
	for _, required := range []string{"could not start capability probe executable", capabilityProbeExecutable, "transient service failed before execution"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("probeTransientCapability() error = %v, want %q", err, required)
		}
	}
	for _, forbidden := range []string{`controller "cpu"`, `interface "cpu.max"`} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("probeTransientCapability() error = %v, must not claim missing %s", err, forbidden)
		}
	}
}

func TestStartupCapabilityProbeTimeoutPreservesRetryableReason(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.probeStartErr = context.DeadlineExceeded
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	capability := mustStartupCapabilities(t, StartupRequirements{})[0]

	err := adapter.probeTransientCapability(context.Background(), transport, capability)
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Reason != ReasonTimeout {
		t.Fatalf("probeTransientCapability() error = %v, want timeout reason", err)
	}
	if IsCapabilityProbeError(err) {
		t.Fatalf("timeout was flattened into a permanent capability-probe error: %v", err)
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
	if len(transport.probeStops) != 1 || transport.probeStops[0] != stale {
		t.Fatalf("interrupted probe cleanup = %v, want only %s", transport.probeStops, stale)
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

func TestAutoObservesWithoutResidueWhereTheKernelRefusesCPU(t *testing.T) {
	// The resman-rho remedy for vmgclalpr2095/2098: under auto, a kernel that
	// refuses +cpu must yield a typed refusal, and the user.slice probe must
	// leave no property lease that a later release could fail to restore.
	transport := newFakeUnitTransport()
	verifier := &absentCPUInterfaceVerifier{fakeKernelVerifier: fakeKernelVerifier{
		preflightByName: map[PropertyName]error{PropertyCPUQuotaPerSecUSec: errors.New("cpu.max is absent")},
	}}
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, verifier, store)

	refusal, err := adapter.resolveStartupCapabilities(context.Background(), StartupRequirements{})
	if err != nil || refusal == nil {
		t.Fatalf("resolveStartupCapabilities() = (%+v, %v), want a typed refusal", refusal, err)
	}
	if refusal.Capability.InterfaceName != "cpu.max" {
		t.Fatalf("refusal names %+v, want cpu.max", refusal.Capability)
	}
	if len(adapter.OwnedUnits()) != 0 || len(store.journal.Units) != 0 {
		t.Fatalf("refusal left ownership: memory=%v journal=%+v", adapter.OwnedUnits(), store.journal)
	}
}

func TestStartupIOProbeRefusesBeforeWritingWithoutARootIOController(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{preflightByName: map[PropertyName]error{
		PropertyIOReadBandwidthMax: errors.New("required I/O controller is unavailable"),
	}}
	adapter := mustTestAdapter(t, transport, verifier)

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{IO: true})
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("requireStartupCapabilities() error = %v, want required capability", err)
	}
	if setCallsContainScalarValues(transport.setCalls, map[PropertyName]uint64{PropertyIOWeight: 1_000}) {
		t.Fatal("the I/O weight was written although the root offers no I/O controller")
	}
}
