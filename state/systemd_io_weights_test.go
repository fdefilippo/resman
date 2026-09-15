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

func TestIODeviceWeightReconciliationReleasesWholePlanAfterPartialReadbackFailure(t *testing.T) {
	topology := testSystemdTopology(0, 1000, 1001)
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: topology,
		confirmApplyErr: map[string]error{
			"user-1000.slice": &systemdunit.AdapterError{Reason: systemdunit.ReasonReadbackMismatch, Operation: "confirm_applied", Err: errors.New("injected mismatch")},
		},
	}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	policy := testIODeviceWeightPolicy(t)
	if err := WithSystemdIODeviceWeights(adapter, classifier, policy)(manager); err != nil {
		t.Fatal(err)
	}
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("AttemptIODeviceWeightCapability() = %+v", result)
	}

	sample := completeIODeviceWeightSample(topology)
	err := manager.reconcileSystemdIODeviceWeights(context.Background(), sample, manager.cfg)
	if err == nil {
		t.Fatal("reconcileSystemdIODeviceWeights() unexpectedly succeeded")
	}
	if len(adapter.applies) != len(topology.Users) {
		t.Fatalf("Apply calls = %d, want %d before readback", len(adapter.applies), len(topology.Users))
	}
	if len(adapter.propertyRestores) != len(topology.Users) {
		t.Fatalf("atomic rollback restores = %+v, want one for every applied slice", adapter.propertyRestores)
	}
	for _, properties := range adapter.activeProperties {
		if properties[systemdunit.PropertyIODeviceWeight] {
			t.Fatalf("partial plan remained active after rollback: %+v", adapter.activeProperties)
		}
	}
	status := manager.GetIODeviceWeightStatus()
	if status.State != IODeviceWeightRefusedIntervention || status.Programmed || status.ReadBack {
		t.Fatalf("weighted-I/O status after deterministic mismatch = %+v", status)
	}

	classifications := classifier.classifyCall
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Retry || result.ActivateCycle {
		t.Fatalf("intervention refusal retried in the same generation: %+v", result)
	}
	if classifier.classifyCall != classifications {
		t.Fatalf("intervention refusal called classifier again: %d -> %d", classifications, classifier.classifyCall)
	}
	manager.PublishIODeviceWeightPolicy(policy)
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("new policy generation did not permit a fresh attempt: %+v", result)
	}
}

func completeIODeviceWeightSample(topology systemdunit.TopologySnapshot) *SystemMetrics {
	now := time.Now()
	observations := make([]systemdunit.ProcessAuthorityObservation, 0, len(topology.Users))
	for _, user := range topology.Users {
		observations = append(observations, systemdunit.ProcessAuthorityObservation{UID: user.UID, Identity: user.Unit.Identity, CPUCoverage: true})
	}
	return &SystemMetrics{
		Timestamp: now,
		systemdAuthorityInventory: pointerInventory(systemdunit.NewProcessAuthorityInventory(
			now.UnixNano(), systemdunit.TopologyFingerprint(topology), false, observations,
		)),
	}
}

func TestIODeviceWeightReconciliationIgnoresUnobservedUnrelatedSession(t *testing.T) {
	captured := testSystemdTopology(1000)
	current := testSystemdTopology(1000, 1001)
	adapter := &fakeSystemdCPUUnitAdapter{topology: current}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("AttemptIODeviceWeightCapability() = %+v", result)
	}

	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), completeIODeviceWeightSample(captured), manager.cfg); err != nil {
		t.Fatalf("reconcileSystemdIODeviceWeights() error = %v", err)
	}
	if len(adapter.applies) != 1 || adapter.applies[0].unit != "user-1000.slice" {
		t.Fatalf("unrelated unobserved session changed the sampled plan: %+v", adapter.applies)
	}
}

func TestIODeviceWeightReconciliationDoesNotApplyToRecreatedUnit(t *testing.T) {
	captured := testSystemdTopology(1000)
	current := testSystemdTopology(1000)
	current.Users[0].Unit = testSystemdUnit("user-1000.slice", 99)
	adapter := &fakeSystemdCPUUnitAdapter{topology: current}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("AttemptIODeviceWeightCapability() = %+v", result)
	}

	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), completeIODeviceWeightSample(captured), manager.cfg); err != nil {
		t.Fatalf("reconcileSystemdIODeviceWeights() error = %v", err)
	}
	if len(adapter.applies) != 0 {
		t.Fatalf("recreated unit received a weight from an old sample: %+v", adapter.applies)
	}
}

func TestIODeviceWeightDeviceLossUsesOneControlCadenceGrace(t *testing.T) {
	topology := testSystemdTopology(1000)
	adapter := &fakeSystemdCPUUnitAdapter{topology: topology}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("AttemptIODeviceWeightCapability() = %+v", result)
	}
	sample := completeIODeviceWeightSample(topology)
	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), sample, manager.cfg); err != nil {
		t.Fatalf("initial reconciliation error = %v", err)
	}
	identity := topology.Users[0].Unit.Identity
	classifier.confirmErr = &systemdunit.IODeviceWeightCapabilityError{
		Reason: systemdunit.IODeviceWeightReasonDeviceMissing,
		Device: "8:0",
		Err:    errors.New("injected device loss"),
	}

	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), sample, manager.cfg); err == nil {
		t.Fatal("first device-loss reconciliation unexpectedly succeeded")
	}
	first := manager.GetIODeviceWeightStatus()
	if first.State != IODeviceWeightRequestedPending || !first.Programmed || first.ReadBack {
		t.Fatalf("first device-loss cadence = %+v", first)
	}
	if !adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatal("first device-loss cadence released the weight before the grace elapsed")
	}
	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), sample, manager.cfg); err == nil {
		t.Fatal("second device-loss reconciliation unexpectedly succeeded")
	}
	second := manager.GetIODeviceWeightStatus()
	if second.Programmed || second.ReadBack || adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatalf("second device-loss cadence did not release the weight: status=%+v active=%+v", second, adapter.activeProperties)
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

func TestDisabledIODeviceWeightPreservesExternalConflictAndRequiresIntervention(t *testing.T) {
	identity := testSystemdUnit("user-1000.slice", 1000).Identity
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: testSystemdTopology(1000),
		owned:    map[string]systemdunit.UnitIdentity{identity.Name: identity},
		activeProperties: map[string]map[systemdunit.PropertyName]bool{
			identity.Name: {systemdunit.PropertyIODeviceWeight: true},
		},
		propertyConflicts: map[string][]systemdunit.PropertyName{
			identity.Name: {systemdunit.PropertyIODeviceWeight},
		},
	}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}

	result := manager.AttemptIODeviceWeightCapability(context.Background())
	if result.Status.State != IODeviceWeightRefusedIntervention || result.Status.Reason != "unsafe_restore" {
		t.Fatalf("conflicting disabled recovery = %+v", result)
	}
	if !result.Status.Programmed || !adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatalf("external weight was not preserved: status=%+v active=%+v", result.Status, adapter.activeProperties)
	}
	if retry := manager.AttemptIODeviceWeightCapability(context.Background()); retry.Retry {
		t.Fatalf("unsafe recovery retried without a new generation: %+v", retry)
	}
}

func TestRecoveredIODeviceWeightUsesOneRetryGraceBeforeSafeRelease(t *testing.T) {
	identity := testSystemdUnit("user-1000.slice", 1000).Identity
	adapter := &fakeSystemdCPUUnitAdapter{
		topology:           testSystemdTopology(1000),
		owned:              map[string]systemdunit.UnitIdentity{identity.Name: identity},
		activeProperties:   map[string]map[systemdunit.PropertyName]bool{identity.Name: {systemdunit.PropertyIODeviceWeight: true}},
		ioWeightProbeError: errors.New("injected transient probe failure"),
	}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}

	first := manager.AttemptIODeviceWeightCapability(context.Background())
	if first.Status.State != IODeviceWeightRequestedPending || !first.Status.Programmed || !adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatalf("first unavailable cadence did not preserve recovered weight: result=%+v active=%+v", first, adapter.activeProperties)
	}
	second := manager.AttemptIODeviceWeightCapability(context.Background())
	if second.Status.State != IODeviceWeightRequestedPending || second.Status.Programmed || adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatalf("second unavailable cadence did not release recovered weight: result=%+v active=%+v", second, adapter.activeProperties)
	}
	if len(adapter.propertyRestores) != 1 || adapter.propertyRestores[0].unit != identity.Name {
		t.Fatalf("recovered weight release calls = %+v", adapter.propertyRestores)
	}
}
