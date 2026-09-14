package state

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/internal/systemdunit"
)

var expectedMemorySystemdProperties = []systemdunit.PropertyName{
	systemdunit.PropertyMemoryHigh,
	systemdunit.PropertyMemoryMax,
	systemdunit.PropertyMemorySwapMax,
}

func TestSystemdNativeMemoryAndIOPlansAreIndependentFromCPUEligibility(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000, 1001)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true
	manager.cfg.DisableSwap = true
	manager.cfg.IOEnabled = true
	manager.cfg.UserIncludeList = []string{"nobody"}
	manager.cfg.RAMUserIncludeList = []string{"alice"}
	manager.cfg.IOUserIncludeList = []string{"bob"}
	manager.resolveSystemdIODevices = func(string) ([]string, error) { return []string{"/dev/vda"}, nil }

	metrics := sampleWithProcessAuthority(adapter.topology, &SystemMetrics{RAMEligibleUsers: []int{1000}, IOEligibleUsers: []int{1001}})
	if err := manager.activateLimits(metrics); err != nil {
		t.Fatalf("activateLimits() error: %v", err)
	}
	if manager.limitsActive || !manager.resourceLimitsActive {
		t.Fatalf("CPU active=%t resources active=%t, want resource-only enforcement", manager.limitsActive, manager.resourceLimitsActive)
	}
	if state := manager.resourceLimits[1000]; !state.ramApplied || state.ioApplied || state.ramAuthority.State != systemdunit.ResourceCoverageComplete {
		t.Fatalf("UID 1000 state = %+v", state)
	}
	if state := manager.resourceLimits[1001]; state.ramApplied || !state.ioApplied || state.ioAuthority.State != systemdunit.ResourceCoverageComplete {
		t.Fatalf("UID 1001 state = %+v", state)
	}
	if hasSystemdProperty(adapter.applies, systemdunit.PropertyCPUWeight) || hasSystemdProperty(adapter.applies, systemdunit.PropertyCPUQuotaPerSecUSec) {
		t.Fatalf("resource-only plan mutated CPU properties: %+v", adapter.applies)
	}
	for _, property := range []systemdunit.PropertyName{
		systemdunit.PropertyMemoryHigh, systemdunit.PropertyMemoryMax, systemdunit.PropertyMemorySwapMax,
		systemdunit.PropertyIOReadBandwidthMax, systemdunit.PropertyIOWriteBandwidthMax,
		systemdunit.PropertyIOReadIOPSMax, systemdunit.PropertyIOWriteIOPSMax,
	} {
		if !hasSystemdProperty(adapter.applies, property) {
			t.Fatalf("resource plan omitted %s: %+v", property, adapter.applies)
		}
	}
}

func TestSystemdNativeResourceRefusalDoesNotDisableCPUPlan(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	authority := systemdunit.ResourceAuthority{
		Resource: systemdunit.ResourceMemory,
		State:    systemdunit.ResourceCoverageRefused,
		Reason:   systemdunit.ResourceCoverageRuntimeDescendant,
	}
	adapter := &fakeSystemdCPUUnitAdapter{
		topology:  testSystemdTopology(0, 1000),
		authority: map[systemdunit.ResourceKind]systemdunit.ResourceAuthority{systemdunit.ResourceMemory: authority},
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{"alice"}
	manager.cfg.RAMEnabled = true
	manager.cfg.RAMUserIncludeList = []string{"alice"}

	err := manager.activateLimits(sampleWithProcessAuthority(adapter.topology, &SystemMetrics{CPUEligibleUsers: []int{1000}, RAMEligibleUsers: []int{1000}}))
	var reconciliationErr *SystemdResourceReconciliationError
	if !errors.As(err, &reconciliationErr) || reconciliationErr.Resource != systemdunit.ResourceMemory {
		t.Fatalf("activateLimits() error = %v, want typed memory refusal", err)
	}
	if !manager.limitsActive || manager.resourceLimits[1000].ramApplied {
		t.Fatalf("CPU active=%t resource=%+v, want CPU-only enforcement", manager.limitsActive, manager.resourceLimits[1000])
	}
	if hasSystemdProperty(adapter.applies, systemdunit.PropertyMemoryHigh) || hasSystemdProperty(adapter.applies, systemdunit.PropertyMemoryMax) {
		t.Fatalf("refused memory plan performed a mutation: %+v", adapter.applies)
	}
	if manager.resourceLimits[1000].ramAuthority.Reason != systemdunit.ResourceCoverageRuntimeDescendant {
		t.Fatalf("refusal reason was not retained: %+v", manager.resourceLimits[1000])
	}
}

func TestSystemdNativeAuthorityLossReleasesOnlyTheRefusedResource(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{"alice"}
	manager.cfg.RAMEnabled = true
	manager.cfg.RAMUserIncludeList = []string{"alice"}
	metrics := sampleWithProcessAuthority(adapter.topology, &SystemMetrics{CPUEligibleUsers: []int{1000}, RAMEligibleUsers: []int{1000}})
	if err := manager.activateLimits(metrics); err != nil {
		t.Fatal(err)
	}
	memoryAppliesBefore := countSystemdPropertyApplications(adapter.applies, systemdunit.PropertyMemoryHigh)
	authority := systemdunit.ResourceAuthority{Resource: systemdunit.ResourceMemory, State: systemdunit.ResourceCoveragePartial, Reason: systemdunit.ResourceCoverageAuthoritySplit}
	adapter.authority = map[systemdunit.ResourceKind]systemdunit.ResourceAuthority{systemdunit.ResourceMemory: authority}
	adapter.authorityError = map[systemdunit.ResourceKind]error{systemdunit.ResourceMemory: &systemdunit.ResourceAuthorityError{UID: 1000, Authority: authority}}

	err := manager.activateLimits(metrics)
	var reconciliationErr *SystemdResourceReconciliationError
	if !errors.As(err, &reconciliationErr) || reconciliationErr.Step != "authority" {
		t.Fatalf("activateLimits() error = %v, want typed authority refusal", err)
	}
	if got := countSystemdPropertyApplications(adapter.applies, systemdunit.PropertyMemoryHigh); got != memoryAppliesBefore {
		t.Fatalf("authority refusal performed a new memory apply: before=%d after=%d", memoryAppliesBefore, got)
	}
	want := append([]systemdunit.PropertyName(nil), expectedMemorySystemdProperties...)
	memoryRestore, ok := findPropertyRestore(adapter.propertyRestores, systemdunit.PropertyMemoryHigh)
	if !ok || !reflect.DeepEqual(memoryRestore.properties, want) {
		t.Fatalf("memory restore = %+v, want only %v (all calls: %+v)", memoryRestore, want, adapter.propertyRestores)
	}
	for _, property := range memoryRestore.properties {
		if property.IsCPU() {
			t.Fatalf("resource refusal restored CPU property %s", property)
		}
	}
	state := manager.resourceLimits[1000]
	if state.ramApplied || state.ramAuthority.Reason != systemdunit.ResourceCoverageAuthoritySplit || !manager.limitsActive {
		t.Fatalf("post-refusal state = %+v CPU active=%t", state, manager.limitsActive)
	}
}

func TestSystemdNativeMissingUserSliceReportsAuthoritySplit(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology()}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true
	err := manager.activateLimits(sampleWithProcessAuthority(adapter.topology, &SystemMetrics{RAMEligibleUsers: []int{1000}}))
	var authorityErr *systemdunit.ResourceAuthorityError
	if !errors.As(err, &authorityErr) || authorityErr.Authority.State != systemdunit.ResourceCoveragePartial || authorityErr.Authority.Reason != systemdunit.ResourceCoverageAuthoritySplit {
		t.Fatalf("activateLimits() error = %v, want typed authority_split partial coverage", err)
	}
	if len(adapter.applies) != 0 {
		t.Fatalf("authority split performed mutations: %+v", adapter.applies)
	}
}

func TestSystemdNativeResourceBusLossDoesNotPublishAppliedState(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000), failApplyUnit: "user-1000.slice"}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true
	err := manager.activateLimits(sampleWithProcessAuthority(adapter.topology, &SystemMetrics{RAMEligibleUsers: []int{1000}}))
	var reconciliationErr *SystemdResourceReconciliationError
	if !errors.As(err, &reconciliationErr) || reconciliationErr.Resource != systemdunit.ResourceMemory || reconciliationErr.Step != "apply" {
		t.Fatalf("activateLimits() error = %v, want typed memory apply failure", err)
	}
	if manager.resourceLimits[1000].ramApplied || manager.resourceLimitsActive {
		t.Fatalf("failed apply published active resource state: %+v", manager.resourceLimits[1000])
	}
}

func TestSystemdNativeResourceAuthorityResultMismatchInvalidatesTheCycle(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	ioAuthority := systemdunit.ResourceAuthority{
		Resource: systemdunit.ResourceIO,
		State:    systemdunit.ResourceCoveragePartial,
		Reason:   systemdunit.ResourceCoverageAuthoritySplit,
	}
	adapter := &fakeSystemdCPUUnitAdapter{
		topology:        testSystemdTopology(1000),
		authority:       map[systemdunit.ResourceKind]systemdunit.ResourceAuthority{systemdunit.ResourceIO: ioAuthority},
		authorityCounts: map[int]int{1: 0},
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true
	manager.cfg.IOEnabled = true
	manager.resolveSystemdIODevices = func(string) ([]string, error) { return []string{"/dev/vda"}, nil }
	metrics := sampleWithProcessAuthority(adapter.topology, &SystemMetrics{RAMEligibleUsers: []int{1000}, IOEligibleUsers: []int{1000}})
	initializeCycleResourceAuthorities(metrics)
	resetCycleResourceAuthorities(metrics, manager.cfg)

	err := manager.reconcileSystemdResourcesAttempt(context.Background(), metrics, manager.cfg)
	if err == nil || !strings.Contains(err.Error(), "adapter returned 0 results for 2 requests") {
		t.Fatalf("resource reconciliation error = %v, want result-count mismatch", err)
	}
	if authority := metrics.systemdRAMAuthority[1000]; authority != nil {
		t.Fatalf("count mismatch retained RAM authority: %+v", authority)
	}
	if authority := metrics.systemdIOAuthority[1000]; authority != nil {
		t.Fatalf("count mismatch retained I/O authority: %+v", authority)
	}
}

func TestSystemdNativeResourceAuthorityIsNotRescannedAfterApply(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}
	adapter.authorityHook = func(call int) {
		if call != 2 {
			return
		}
		authority := systemdunit.ResourceAuthority{
			Resource: systemdunit.ResourceMemory,
			State:    systemdunit.ResourceCoveragePartial,
			Reason:   systemdunit.ResourceCoverageAuthoritySplit,
		}
		adapter.authority = map[systemdunit.ResourceKind]systemdunit.ResourceAuthority{systemdunit.ResourceMemory: authority}
		adapter.authorityError = map[systemdunit.ResourceKind]error{
			systemdunit.ResourceMemory: &systemdunit.ResourceAuthorityError{UID: 1000, Authority: authority, Err: errors.New("workload escaped during reconciliation")},
		}
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true

	err := manager.activateLimits(sampleWithProcessAuthority(adapter.topology, &SystemMetrics{RAMEligibleUsers: []int{1000}}))
	if err != nil {
		t.Fatalf("activateLimits() error = %v", err)
	}
	state := manager.resourceLimits[1000]
	if !state.ramApplied || state.ramAuthority.Reason != systemdunit.ResourceCoverageVerified {
		t.Fatalf("captured authority was not acknowledged: %+v", state)
	}
	if adapter.authorityChecks != 1 {
		t.Fatalf("resource authority checks = %d, want one sample-scoped classification", adapter.authorityChecks)
	}
	if adapter.confirmCalls != 2 || len(adapter.confirmedApplied) != 1 || adapter.confirmedApplied[0] != "user-1000.slice" {
		t.Fatalf("live confirmations = topology:%d applied:%v, want two topology checks and one property/kernel readback", adapter.confirmCalls, adapter.confirmedApplied)
	}
	if _, ok := findPropertyRestore(adapter.propertyRestores, systemdunit.PropertyMemoryHigh); ok {
		t.Fatalf("sample-scoped authority caused an unexpected restore: %+v", adapter.propertyRestores)
	}
}

func TestSystemdNativeResourceTopologyIsReconfirmedBeforeAcknowledgement(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}
	adapter.confirmHook = func(call int) {
		if call == 2 {
			adapter.topology = testSystemdTopology(1000)
			adapter.topology.Users[0].Unit.Identity.InvocationID[0]++
		}
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true
	metrics := sampleWithProcessAuthority(adapter.topology, &SystemMetrics{RAMEligibleUsers: []int{1000}})
	initializeCycleResourceAuthorities(metrics)
	resetCycleResourceAuthorities(metrics, manager.cfg)

	err := manager.reconcileSystemdResourcesAttempt(context.Background(), metrics, manager.cfg)
	var reconciliationErr *SystemdResourceReconciliationError
	if !errors.As(err, &reconciliationErr) || reconciliationErr.Step != "pre_acknowledgement_identity" {
		t.Fatalf("resource reconciliation error = %v, want pre_acknowledgement_identity", err)
	}
	if state := manager.resourceLimits[1000]; state.ramApplied || state.ramAuthority.State != "" {
		t.Fatalf("stale topology published RAM state: %+v", state)
	}
	if authority, requested := metrics.systemdRAMAuthority[1000]; !requested || authority != nil {
		t.Fatalf("stale topology published cycle authority: requested=%t authority=%+v", requested, authority)
	}
}

func TestSystemdNativeResourceAuthorityRejectsInventoryIdentityMismatch(t *testing.T) {
	captured := testSystemdTopology(1000)
	tests := []struct {
		name   string
		mutate func(*fakeSystemdCPUUnitAdapter, *SystemMetrics)
	}{
		{name: "new unit", mutate: func(adapter *fakeSystemdCPUUnitAdapter, _ *SystemMetrics) {
			adapter.topology = testSystemdTopology(1000, 1001)
		}},
		{name: "missing unit", mutate: func(adapter *fakeSystemdCPUUnitAdapter, _ *SystemMetrics) { adapter.topology = testSystemdTopology() }},
		{name: "recreated unit", mutate: func(adapter *fakeSystemdCPUUnitAdapter, _ *SystemMetrics) {
			adapter.topology.Users[0].Unit.Identity.InvocationID[0]++
		}},
		{name: "different sample epoch", mutate: func(_ *fakeSystemdCPUUnitAdapter, sample *SystemMetrics) { sample.PersistenceSystem.SampleEpochID++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := &fakeSystemdCPUUnitAdapter{topology: captured}
			manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
			manager.cfg.RAMEnabled = true
			sample := sampleWithProcessAuthority(captured, &SystemMetrics{RAMEligibleUsers: []int{1000}})
			test.mutate(adapter, sample)

			err := manager.activateLimits(sample)
			var authorityErr *systemdunit.ResourceAuthorityError
			if !errors.As(err, &authorityErr) || authorityErr.Authority.Reason != systemdunit.ResourceCoverageTopologyChanged {
				t.Fatalf("activateLimits() error = %v, want typed sample-topology refusal", err)
			}
			if adapter.authorityChecks != 0 || len(adapter.applies) != 0 {
				t.Fatalf("mismatched inventory performed checks=%d applies=%v", adapter.authorityChecks, adapter.applies)
			}
		})
	}
}

func TestSystemdNativeResourceRetryReusesOnlyTheCapturedSample(t *testing.T) {
	captured := testSystemdTopology(1000)
	adapter := &fakeSystemdCPUUnitAdapter{topology: captured}
	adapter.confirmHook = func(call int) {
		if call == 1 {
			adapter.topology = testSystemdTopology(1000, 1001)
		}
	}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true
	sample := sampleWithProcessAuthority(captured, &SystemMetrics{RAMEligibleUsers: []int{1000}})

	err := manager.reconcileSystemdResources(context.Background(), sample, manager.cfg)
	var authorityErr *systemdunit.ResourceAuthorityError
	if !errors.As(err, &authorityErr) || authorityErr.Authority.Reason != systemdunit.ResourceCoverageTopologyChanged {
		t.Fatalf("reconcileSystemdResources() error = %v, want sample-topology refusal after retry", err)
	}
	if adapter.authorityChecks != 1 || adapter.inventoryCaptures != 0 {
		t.Fatalf("retry checks=%d captures=%d, want one captured-authority check and no new inventory", adapter.authorityChecks, adapter.inventoryCaptures)
	}
}

func TestSystemdNativeResourceFinalReadbackPreservesAnExternalPropertyChange(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: testSystemdTopology(1000),
		confirmApplyErr: map[string]error{
			"user-1000.slice": &systemdunit.AdapterError{Reason: systemdunit.ReasonExternalConflict, Operation: "confirm_applied", Unit: "user-1000.slice", Err: errors.New("operator changed MemoryMax")},
		},
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true

	err := manager.activateLimits(sampleWithProcessAuthority(adapter.topology, &SystemMetrics{RAMEligibleUsers: []int{1000}}))
	var reconciliationErr *SystemdResourceReconciliationError
	if !errors.As(err, &reconciliationErr) || reconciliationErr.Step != "pre_acknowledgement_readback" {
		t.Fatalf("activateLimits() error = %v, want final readback conflict", err)
	}
	state := manager.resourceLimits[1000]
	if state.ramApplied || state.ramAuthority.Reason != systemdunit.ResourceCoverageInspectionFailed {
		t.Fatalf("external property change was acknowledged as applied: %+v", state)
	}
	if len(adapter.propertyRestores) != 0 {
		t.Fatalf("external property change was overwritten by compensation: %+v", adapter.propertyRestores)
	}
}

func TestSystemdNativeResourceReleaseDoesNotRemoveActiveCPUWeight(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{"alice"}
	manager.cfg.RAMEnabled = true
	manager.cfg.RAMUserIncludeList = []string{"alice"}
	if err := manager.activateLimits(sampleWithProcessAuthority(adapter.topology, &SystemMetrics{CPUEligibleUsers: []int{1000}, RAMEligibleUsers: []int{1000}})); err != nil {
		t.Fatal(err)
	}
	manager.cfg.RAMEnabled = false
	if err := manager.reconcileSystemdResources(context.Background(), &SystemMetrics{}, manager.cfg); err != nil {
		t.Fatal(err)
	}
	if !manager.limitsActive || manager.resourceLimitsActive {
		t.Fatalf("CPU active=%t resources active=%t after RAM release", manager.limitsActive, manager.resourceLimitsActive)
	}
	if !containsString(adapter.restores, "user-1000.slice") {
		t.Fatalf("RAM release did not use selected property restoration: %v", adapter.restores)
	}
	memoryRestore, ok := findPropertyRestore(adapter.propertyRestores, systemdunit.PropertyMemoryHigh)
	if !ok || !reflect.DeepEqual(memoryRestore.properties, expectedMemorySystemdProperties) {
		t.Fatalf("RAM release restored %v, want a memory-only call with %v", adapter.propertyRestores, expectedMemorySystemdProperties)
	}
}

func TestSystemdMemoryAssignmentsUseConfiguredRatioAndHostPageGranularity(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	manager := testSystemdCPUPointsManager(t, policy, &fakeSystemdCPUUnitAdapter{}, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMQuotaPerUser = "512M"
	manager.cfg.RAMHighRatio = 0.8
	assignments, err := manager.systemdMemoryAssignments(1000, manager.cfg)
	if err != nil {
		t.Fatal(err)
	}
	values := map[systemdunit.PropertyName]uint64{}
	for _, assignment := range assignments {
		values[assignment.Name()] = assignment.Value()
	}
	pageSize := uint64(os.Getpagesize())
	const maxBytes = uint64(512 << 20)
	wantHigh := (maxBytes * 8 / 10) / pageSize * pageSize
	if values[systemdunit.PropertyMemoryHigh] != wantHigh || values[systemdunit.PropertyMemoryMax] != maxBytes {
		t.Fatalf("memory assignments = %v, want high=%d max=%d", values, wantHigh, maxBytes)
	}
	if values[systemdunit.PropertyMemoryHigh] == values[systemdunit.PropertyMemoryMax] {
		t.Fatal("RAM_HIGH_RATIO was ignored")
	}

	manager.cfg.RAMHighRatio = 0
	assignments, err = manager.systemdMemoryAssignments(1000, manager.cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range assignments {
		if assignment.Name() == systemdunit.PropertyMemoryHigh {
			t.Fatal("RAM_HIGH_RATIO=0 did not disable MemoryHigh")
		}
	}
}

func TestDesiredResourceUsersRequiresTheResourceToBeEnabled(t *testing.T) {
	if got := desiredResourceUsers(false, []int{1000, 1001}); len(got) != 0 {
		t.Fatalf("disabled resource users = %v, want none", got)
	}
	if got := desiredResourceUsers(true, []int{1000, 1001}); !got[1000] || !got[1001] || len(got) != 2 {
		t.Fatalf("enabled resource users = %v", got)
	}
}

func sampleWithProcessAuthority(topology systemdunit.TopologySnapshot, sample *SystemMetrics) *SystemMetrics {
	const sampleEpochID = int64(1)
	sample.PersistenceSystem.SampleEpochID = sampleEpochID
	observations := make([]systemdunit.ProcessAuthorityObservation, 0, len(topology.Users))
	for _, user := range topology.Users {
		observations = append(observations, systemdunit.ProcessAuthorityObservation{
			UID:               user.UID,
			Identity:          user.Unit.Identity,
			CPUCoverage:       true,
			ResourceAuthority: systemdunit.ResourceAuthority{State: systemdunit.ResourceCoverageComplete, Reason: systemdunit.ResourceCoverageVerified},
		})
	}
	inventory := systemdunit.NewProcessAuthorityInventory(sampleEpochID, systemdunit.TopologyFingerprint(topology), true, observations)
	sample.systemdAuthorityInventory = &inventory
	return sample
}

func hasSystemdProperty(calls []systemdCPUApplyCall, property systemdunit.PropertyName) bool {
	for _, call := range calls {
		if _, ok := call.assignments[property]; ok {
			return true
		}
		if _, ok := call.devices[property]; ok {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countSystemdPropertyApplications(calls []systemdCPUApplyCall, property systemdunit.PropertyName) int {
	count := 0
	for _, call := range calls {
		if _, ok := call.assignments[property]; ok {
			count++
		}
		if _, ok := call.devices[property]; ok {
			count++
		}
	}
	return count
}

func findPropertyRestore(calls []systemdPropertyRestoreCall, property systemdunit.PropertyName) (systemdPropertyRestoreCall, bool) {
	for _, call := range calls {
		for _, candidate := range call.properties {
			if candidate == property {
				return call, true
			}
		}
	}
	return systemdPropertyRestoreCall{}, false
}
