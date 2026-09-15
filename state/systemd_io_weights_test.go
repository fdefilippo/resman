package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/ioweights"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

type fakeIODeviceWeightClassifier struct {
	snapshot     systemdunit.IODeviceWeightCapabilitySnapshot
	classifyErr  error
	confirmErr   error
	classifyCall int
	confirmCall  int
}

func (f *fakeIODeviceWeightClassifier) Classify(context.Context, string) (systemdunit.IODeviceWeightCapabilitySnapshot, error) {
	f.classifyCall++
	return f.snapshot, f.classifyErr
}

func (f *fakeIODeviceWeightClassifier) Confirm(context.Context, systemdunit.IODeviceWeightCapabilitySnapshot) (systemdunit.IODeviceWeightCapabilitySnapshot, error) {
	f.confirmCall++
	return f.snapshot, f.confirmErr
}

type ioWeightExactResolver map[string][]cpupoints.ResolvedUserIdentity

func (r ioWeightExactResolver) ResolveExactUsername(username string) ([]cpupoints.ResolvedUserIdentity, error) {
	identities, ok := r[username]
	if !ok {
		return nil, errors.New("not found")
	}
	return identities, nil
}

func testIODeviceWeightPolicy(t *testing.T) ioweights.PolicySnapshot {
	t.Helper()
	root, _ := ioweights.NewWeight(200)
	defaultIO, _ := ioweights.NewWeight(100)
	path, _ := ioweights.NewPolicyMapPath("/etc/resman/io-weights.map")
	policy, err := ioweights.NewPolicyLoader().LoadContent(ioweights.PolicyInputs{Root: root, Default: defaultIO, MapPath: path}, []byte(ioweights.PolicyMapMarker+"\nalice=700\n"), ioWeightExactResolver{
		"alice": {{Username: "alice", UID: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func testIODeviceWeightCandidate(t *testing.T) systemdunit.IODeviceWeightCapabilitySnapshot {
	t.Helper()
	number := systemdunit.IODeviceWeightDeviceNumber{Major: 8}
	snapshot, err := systemdunit.NewIODeviceWeightProbeCandidateSnapshot("8:0", systemdunit.IODeviceWeightPlatformIdentity{}, []systemdunit.IODeviceWeightDeviceCapability{{
		Identity: systemdunit.IODeviceWeightDeviceIdentity{Number: number, DeviceNode: "/dev/vda", SysfsPath: "/sys/devices/vda", SysfsInode: 10},
		Outcome:  systemdunit.IODeviceWeightProbeCandidate, Mechanism: systemdunit.IODeviceWeightMechanismBFQ,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestIODeviceWeightCapabilityProbePrecedesProductionMutation(t *testing.T) {
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	result := manager.AttemptIODeviceWeightCapability(context.Background())
	if result.Status.State != IODeviceWeightFunctionallyAccepted || !result.ActivateCycle || adapter.ioWeightProbeCalls != 1 {
		t.Fatalf("capability result=%+v probe_calls=%d", result, adapter.ioWeightProbeCalls)
	}
	if len(adapter.applies) != 0 {
		t.Fatalf("capability probe used production slice Apply: %+v", adapter.applies)
	}
}

func TestIODeviceWeightReconciliationAppliesRootMappedAndDefaultWithPartialAuthority(t *testing.T) {
	topology := testSystemdTopology(0, 1000, 1001)
	adapter := &fakeSystemdCPUUnitAdapter{topology: topology}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	manager.cfg.IOUserIncludeList = []string{"^alice$"}
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("AttemptIODeviceWeightCapability() = %+v", result)
	}
	now := time.Now()
	observations := make([]systemdunit.ProcessAuthorityObservation, 0, len(topology.Users))
	for _, user := range topology.Users {
		observations = append(observations, systemdunit.ProcessAuthorityObservation{UID: user.UID, Identity: user.Unit.Identity, CPUCoverage: user.UID != 1000})
	}
	sample := &SystemMetrics{Timestamp: now, systemdAuthorityInventory: pointerInventory(systemdunit.NewProcessAuthorityInventory(now.UnixNano(), systemdunit.TopologyFingerprint(topology), false, observations))}
	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), sample, manager.cfg); err != nil {
		t.Fatalf("reconcileSystemdIODeviceWeights() error = %v", err)
	}
	want := map[string]uint64{"user-0.slice": 1200, "user-1000.slice": 6700, "user-1001.slice": 100}
	if len(adapter.applies) != len(want) {
		t.Fatalf("Apply calls = %+v, want %d", adapter.applies, len(want))
	}
	for _, call := range adapter.applies {
		limits := call.devices[systemdunit.PropertyIODeviceWeight]
		if len(limits) != 1 || limits[0].Value != want[call.unit] {
			t.Fatalf("%s IODeviceWeight = %+v, want %d", call.unit, limits, want[call.unit])
		}
	}
	status := manager.GetIODeviceWeightStatus()
	if !status.Programmed || !status.ReadBack || status.PartialUsers != 1 {
		t.Fatalf("weighted-I/O status = %+v", status)
	}
}

func pointerInventory(value systemdunit.ProcessAuthorityInventory) *systemdunit.ProcessAuthorityInventory {
	return &value
}

func TestDisabledIODeviceWeightRestoresOnlyRecoveredWeightLease(t *testing.T) {
	identity := testSystemdUnit("user-1000.slice", 1000).Identity
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: testSystemdTopology(1000),
		owned:    map[string]systemdunit.UnitIdentity{identity.Name: identity},
		activeProperties: map[string]map[systemdunit.PropertyName]bool{identity.Name: {
			systemdunit.PropertyIODeviceWeight:     true,
			systemdunit.PropertyIOReadBandwidthMax: true,
		}},
	}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	result := manager.AttemptIODeviceWeightCapability(context.Background())
	if result.Status.State != IODeviceWeightDisabled || adapter.ioWeightProbeCalls != 0 {
		t.Fatalf("disabled result=%+v probes=%d", result, adapter.ioWeightProbeCalls)
	}
	if len(adapter.propertyRestores) != 1 || len(adapter.propertyRestores[0].properties) != 1 || adapter.propertyRestores[0].properties[0] != systemdunit.PropertyIODeviceWeight {
		t.Fatalf("selective recovery restore = %+v", adapter.propertyRestores)
	}
	if !adapter.activeProperties[identity.Name][systemdunit.PropertyIOReadBandwidthMax] {
		t.Fatal("weighted-I/O recovery cleanup released an independent hard cap")
	}
}
