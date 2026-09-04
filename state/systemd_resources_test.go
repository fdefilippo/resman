package state

import (
	"context"
	"errors"
	"testing"

	"github.com/fdefilippo/resman/internal/systemdunit"
)

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

	metrics := &SystemMetrics{RAMEligibleUsers: []int{1000}, IOEligibleUsers: []int{1001}}
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
	authorityErr := &systemdunit.ResourceAuthorityError{UID: 1000, Authority: authority}
	adapter := &fakeSystemdCPUUnitAdapter{
		topology:       testSystemdTopology(0, 1000),
		authority:      map[systemdunit.ResourceKind]systemdunit.ResourceAuthority{systemdunit.ResourceMemory: authority},
		authorityError: map[systemdunit.ResourceKind]error{systemdunit.ResourceMemory: authorityErr},
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{"alice"}
	manager.cfg.RAMEnabled = true
	manager.cfg.RAMUserIncludeList = []string{"alice"}

	err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}, RAMEligibleUsers: []int{1000}})
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

func TestSystemdNativeMissingUserSliceReportsAuthoritySplit(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology()}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.RAMEnabled = true
	err := manager.activateLimits(&SystemMetrics{RAMEligibleUsers: []int{1000}})
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
	err := manager.activateLimits(&SystemMetrics{RAMEligibleUsers: []int{1000}})
	var reconciliationErr *SystemdResourceReconciliationError
	if !errors.As(err, &reconciliationErr) || reconciliationErr.Resource != systemdunit.ResourceMemory || reconciliationErr.Step != "apply" {
		t.Fatalf("activateLimits() error = %v, want typed memory apply failure", err)
	}
	if manager.resourceLimits[1000].ramApplied || manager.resourceLimitsActive {
		t.Fatalf("failed apply published active resource state: %+v", manager.resourceLimits[1000])
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
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}, RAMEligibleUsers: []int{1000}}); err != nil {
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
