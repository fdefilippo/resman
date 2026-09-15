package state

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
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

type blockingIODeviceWeightClassifier struct {
	entered  chan struct{}
	canceled chan struct{}
}

func (f *blockingIODeviceWeightClassifier) Classify(ctx context.Context, _ string) (systemdunit.IODeviceWeightCapabilitySnapshot, error) {
	close(f.entered)
	<-ctx.Done()
	close(f.canceled)
	return systemdunit.IODeviceWeightCapabilitySnapshot{}, ctx.Err()
}

func (*blockingIODeviceWeightClassifier) Confirm(context.Context, systemdunit.IODeviceWeightCapabilitySnapshot) (systemdunit.IODeviceWeightCapabilitySnapshot, error) {
	return systemdunit.IODeviceWeightCapabilitySnapshot{}, errors.New("unexpected confirmation")
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

func testIODeviceWeightObservation(t *testing.T, outcome systemdunit.IODeviceWeightCapabilityOutcome, reason systemdunit.IODeviceWeightCapabilityReason) systemdunit.IODeviceWeightCapabilitySnapshot {
	t.Helper()
	number := systemdunit.IODeviceWeightDeviceNumber{Major: 8}
	snapshot, err := systemdunit.NewIODeviceWeightCapabilitySnapshot("8:0", systemdunit.IODeviceWeightPlatformIdentity{}, []systemdunit.IODeviceWeightDeviceCapability{{
		Identity: systemdunit.IODeviceWeightDeviceIdentity{Number: number, DeviceNode: "/dev/vda", SysfsPath: "/sys/devices/vda", SysfsInode: 10},
		Outcome:  outcome, Reason: reason,
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

func TestIODeviceWeightCapabilityReevaluatesBootObservationsUntilAccepted(t *testing.T) {
	tests := []struct {
		name          string
		initial       systemdunit.IODeviceWeightCapabilitySnapshot
		initialState  IODeviceWeightActivationState
		initialReason string
	}{
		{
			name:          "BFQ becomes active after startup",
			initial:       testIODeviceWeightObservation(t, systemdunit.IODeviceWeightMechanismInactive, systemdunit.IODeviceWeightReasonNoActiveMechanism),
			initialState:  IODeviceWeightRequestedPending,
			initialReason: string(systemdunit.IODeviceWeightReasonNoActiveMechanism),
		},
		{
			name:          "incomplete LVM topology stabilizes",
			initial:       testIODeviceWeightObservation(t, systemdunit.IODeviceWeightAmbiguousTopology, systemdunit.IODeviceWeightReasonAmbiguousTopology),
			initialState:  IODeviceWeightRefusedObservation,
			initialReason: string(systemdunit.IODeviceWeightReasonAmbiguousTopology),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}
			manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
			manager.cfg.IOWeightDevices = "8:0"
			classifier := &fakeIODeviceWeightClassifier{snapshot: test.initial}
			if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
				t.Fatal(err)
			}

			first := manager.AttemptIODeviceWeightCapability(context.Background())
			if first.Status.State != test.initialState || first.Status.Reason != test.initialReason || !first.Retry || adapter.ioWeightProbeCalls != 0 {
				t.Fatalf("initial observation result=%+v probe_calls=%d", first, adapter.ioWeightProbeCalls)
			}

			classifier.snapshot = testIODeviceWeightCandidate(t)
			second := manager.AttemptIODeviceWeightCapability(context.Background())
			if second.Status.State != IODeviceWeightFunctionallyAccepted || !second.Retry || !second.ActivateCycle || adapter.ioWeightProbeCalls != 1 {
				t.Fatalf("stabilized observation result=%+v probe_calls=%d", second, adapter.ioWeightProbeCalls)
			}
		})
	}
}

func TestIODeviceWeightConfigGenerationCancelsInFlightClassification(t *testing.T) {
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &blockingIODeviceWeightClassifier{entered: make(chan struct{}), canceled: make(chan struct{})}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	done := make(chan IODeviceWeightAttemptResult, 1)
	go func() { done <- manager.AttemptIODeviceWeightCapability(context.Background()) }()
	<-classifier.entered
	finishUpdate := manager.BeginConfigUpdate()
	finishUpdate()
	<-classifier.canceled
	result := <-done
	if result.Status.State != IODeviceWeightRequestedPending || result.Status.Reason != "cancelled_generation" || !result.Retry {
		t.Fatalf("canceled generation result = %+v", result)
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
	if !status.Programmed || !status.ReadBack || status.PartialUsers != 1 || status.CompleteUsers != 2 || status.UnavailableUsers != 0 || status.AuthorityCoverage != "partial" {
		t.Fatalf("weighted-I/O status = %+v", status)
	}
	if len(status.Values) != 3 || status.Values[1].RequestedValue != 700 || status.Values[1].SystemdValue != 6700 || status.Values[1].KernelValue != 700 || status.Values[1].NominalShare <= 0 {
		t.Fatalf("weighted-I/O value path = %+v", status.Values)
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

func TestIODeviceWeightInvalidPlanReplacesPreviouslyAcceptedStatus(t *testing.T) {
	topology := testSystemdTopology(1000, 1000)
	identity := topology.Users[0].Unit.Identity
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: topology,
		owned:    map[string]systemdunit.UnitIdentity{identity.Name: identity},
		activeProperties: map[string]map[systemdunit.PropertyName]bool{
			identity.Name: {systemdunit.PropertyIODeviceWeight: true},
		},
	}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.ioWeightUnits[1000] = identity
	manager.ioWeightStatus.State = IODeviceWeightFunctionallyAccepted
	manager.ioWeightStatus.Programmed = true
	manager.ioWeightStatus.ReadBack = true
	manager.ioWeightCapability = testIODeviceWeightCandidate(t)
	manager.mu.Unlock()

	err := manager.reconcileSystemdIODeviceWeights(context.Background(), completeIODeviceWeightSample(topology), manager.cfg)
	if err == nil {
		t.Fatal("reconcileSystemdIODeviceWeights() accepted duplicate plan participants")
	}
	status := manager.GetIODeviceWeightStatus()
	if status.State != IODeviceWeightRefusedIntervention || status.Reason != "invalid_policy_plan" || status.Programmed || status.ReadBack {
		t.Fatalf("invalid plan left stale accepted status: %+v", status)
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
	status := manager.GetIODeviceWeightStatus()
	if status.SiblingSlices != 2 || status.UnavailableUsers != 1 || status.AuthorityCoverage != "unavailable" {
		t.Fatalf("unobserved sibling was omitted from public denominator: %+v", status)
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

func TestIODeviceWeightEvidenceUnavailableUsesOneControlCadenceGrace(t *testing.T) {
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
		Reason: systemdunit.IODeviceWeightReasonEvidenceUnavailable,
		Device: "8:0",
		Err:    errors.New("injected device loss"),
	}

	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), sample, manager.cfg); err == nil {
		t.Fatal("first evidence-unavailable reconciliation unexpectedly succeeded")
	}
	first := manager.GetIODeviceWeightStatus()
	if first.State != IODeviceWeightRequestedPending || !first.Programmed || first.ReadBack {
		t.Fatalf("first evidence-unavailable cadence = %+v", first)
	}
	if !adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatal("first evidence-unavailable cadence released the weight before the grace elapsed")
	}
	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), sample, manager.cfg); err == nil {
		t.Fatal("second evidence-unavailable reconciliation unexpectedly succeeded")
	}
	second := manager.GetIODeviceWeightStatus()
	if second.Programmed || second.ReadBack || adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatalf("second evidence-unavailable cadence did not release the weight: status=%+v active=%+v", second, adapter.activeProperties)
	}
}

func TestIODeviceWeightCapabilityRetryDoesNotConsumeControlCadenceGrace(t *testing.T) {
	topology := testSystemdTopology(1000)
	identity := topology.Users[0].Unit.Identity
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: topology,
		owned:    map[string]systemdunit.UnitIdentity{identity.Name: identity},
		activeProperties: map[string]map[systemdunit.PropertyName]bool{
			identity.Name: {systemdunit.PropertyIODeviceWeight: true},
		},
	}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.ioWeightUnits[1000] = identity
	manager.ioWeightStatus.State = IODeviceWeightFunctionallyAccepted
	manager.ioWeightStatus.Programmed = true
	manager.mu.Unlock()

	manager.publishIODeviceWeightCapabilityFailure(context.Background(), IODeviceWeightRequestedPending,
		string(systemdunit.IODeviceWeightReasonEvidenceUnavailable), "8:0", systemdunit.IODeviceWeightCapabilitySnapshot{}, true)
	if err := manager.deferOrReleaseIODeviceWeights(context.Background(), IODeviceWeightRequestedPending,
		string(systemdunit.IODeviceWeightReasonEvidenceUnavailable), errors.New("control evidence unavailable")); err == nil {
		t.Fatal("control-cycle evidence failure unexpectedly succeeded")
	}
	if !manager.GetIODeviceWeightStatus().Programmed || !adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatal("asynchronous retry consumed the independent control-cadence grace")
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

func TestIODeviceWeightObservationRefusalReleasesWithoutGrace(t *testing.T) {
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
	classifier.confirmErr = &systemdunit.IODeviceWeightCapabilityError{
		Reason: systemdunit.IODeviceWeightReasonMechanismAmbiguous,
		Device: "8:0",
		Err:    errors.New("injected mechanism ambiguity"),
	}

	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), sample, manager.cfg); err == nil {
		t.Fatal("ambiguous mechanism reconciliation unexpectedly succeeded")
	}
	status := manager.GetIODeviceWeightStatus()
	identity := topology.Users[0].Unit.Identity
	if status.State != IODeviceWeightRefusedObservation || status.Programmed || status.ReadBack || adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatalf("observation refusal retained a weight through the evidence-only grace: status=%+v active=%+v", status, adapter.activeProperties)
	}
}

func TestIODeviceWeightAsyncObservationRefusalReleasesWithoutGrace(t *testing.T) {
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
	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), completeIODeviceWeightSample(topology), manager.cfg); err != nil {
		t.Fatalf("initial reconciliation error = %v", err)
	}
	identity := topology.Users[0].Unit.Identity
	classifier.snapshot = testIODeviceWeightObservation(t, systemdunit.IODeviceWeightMechanismAmbiguous, systemdunit.IODeviceWeightReasonMechanismAmbiguous)
	classifier.confirmErr = &systemdunit.IODeviceWeightCapabilityError{
		Reason: systemdunit.IODeviceWeightReasonMechanismAmbiguous,
		Device: "8:0",
		Err:    errors.New("injected mechanism ambiguity"),
	}
	probes := adapter.ioWeightProbeCalls

	result := manager.AttemptIODeviceWeightCapability(context.Background())
	if result.Status.State != IODeviceWeightRefusedObservation || result.Status.Reason != string(systemdunit.IODeviceWeightReasonMechanismAmbiguous) || !result.Retry {
		t.Fatalf("asynchronous observation refusal = %+v", result)
	}
	if result.Status.Programmed || result.Status.ReadBack || adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatalf("asynchronous observation refusal retained a weight through evidence-only grace: status=%+v active=%+v", result.Status, adapter.activeProperties)
	}
	if adapter.ioWeightProbeCalls != probes {
		t.Fatalf("observation refusal repeated the mutating probe: %d -> %d", probes, adapter.ioWeightProbeCalls)
	}
}

func TestIODeviceWeightRepeatedTransientProbeErrorsRemainRetryable(t *testing.T) {
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: testSystemdTopology(1000),
		ioWeightProbeError: &systemdunit.AdapterError{
			Reason: systemdunit.ReasonTimeout, Operation: "startup_capabilities", Err: context.DeadlineExceeded,
		},
	}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}

	const attempts = 8
	for attempt := 1; attempt <= attempts; attempt++ {
		result := manager.AttemptIODeviceWeightCapability(context.Background())
		if result.Status.State != IODeviceWeightRequestedPending || result.Status.Reason != string(systemdunit.IODeviceWeightReasonEvidenceUnavailable) || !result.Retry {
			t.Fatalf("transient probe attempt %d result = %+v", attempt, result)
		}
		if result.Status.ClassificationAttempts != uint64(attempt) || result.Status.ProbeAttempts != uint64(attempt) {
			t.Fatalf("transient probe attempt %d counters = classify %d probe %d", attempt, result.Status.ClassificationAttempts, result.Status.ProbeAttempts)
		}
	}
	if adapter.ioWeightProbeCalls != attempts {
		t.Fatalf("mutating probe calls = %d, want %d", adapter.ioWeightProbeCalls, attempts)
	}
}

func TestIODeviceWeightNestedProbeCleanupBusErrorRemainsRetryable(t *testing.T) {
	err := &systemdunit.AdapterError{
		Reason:    systemdunit.ReasonCapabilityProbe,
		Operation: "startup_capabilities",
		Err: fmt.Errorf("cleanup transient unit: %w", &systemdunit.AdapterError{
			Reason: systemdunit.ReasonBusUnavailable, Operation: "stop_capability_probe", Err: errors.New("connection reset"),
		}),
	}
	if !ioWeightProbeRetryable(err) {
		t.Fatalf("nested transient D-Bus cleanup error was classified as intervention: %v", err)
	}
}

func TestPublishingEquivalentIODeviceWeightPolicyPreservesAcceptedCapability(t *testing.T) {
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	policy := testIODeviceWeightPolicy(t)
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, policy)(manager); err != nil {
		t.Fatal(err)
	}
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("AttemptIODeviceWeightCapability() = %+v", result)
	}
	classifications := classifier.classifyCall

	manager.PublishIODeviceWeightPolicy(policy)
	result := manager.AttemptIODeviceWeightCapability(context.Background())
	if result.Status.State != IODeviceWeightFunctionallyAccepted || classifier.classifyCall != classifications || classifier.confirmCall == 0 {
		t.Fatalf("equivalent publication reset capability: result=%+v classify=%d confirm=%d", result, classifier.classifyCall, classifier.confirmCall)
	}
}

func TestPublishingChangedIODeviceWeightPolicyQueuesCycleWithoutRepeatingProbe(t *testing.T) {
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("AttemptIODeviceWeightCapability() = %+v", result)
	}
	root, _ := ioweights.NewWeight(250)
	defaultIO, _ := ioweights.NewWeight(100)
	changed := ioweights.NewEmptyPolicySnapshot(root, defaultIO)
	probes := adapter.ioWeightProbeCalls

	manager.PublishIODeviceWeightPolicy(changed)
	result := manager.AttemptIODeviceWeightCapability(context.Background())
	if !result.ActivateCycle || result.Status.State != IODeviceWeightFunctionallyAccepted || adapter.ioWeightProbeCalls != probes {
		t.Fatalf("changed policy result=%+v probes=%d->%d", result, probes, adapter.ioWeightProbeCalls)
	}
}

func TestPublishingDisabledIODeviceWeightPolicyRemainsDisabled(t *testing.T) {
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}, &forbiddenSystemdNativeCgroupManager{}, 4)
	root, _ := ioweights.NewWeight(250)
	defaultIO, _ := ioweights.NewWeight(100)
	manager.PublishIODeviceWeightPolicy(ioweights.NewEmptyPolicySnapshot(root, defaultIO))
	status := manager.GetIODeviceWeightStatus()
	if status.State != IODeviceWeightDisabled || !status.RequestedAt.IsZero() {
		t.Fatalf("disabled policy publication fabricated a request: %+v", status)
	}
}

func TestDisablingProgrammedIODeviceWeightPublishesReleasePending(t *testing.T) {
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.mu.Lock()
	manager.ioWeightUnits[1000] = testSystemdTopology(1000).Users[0].Unit.Identity
	manager.ioWeightStatus.Programmed = true
	manager.mu.Unlock()
	reloaded := config.DefaultConfig()
	manager.cfg.IOWeightDevices = "8:0"

	manager.UpdateConfig(reloaded)
	status := manager.GetIODeviceWeightStatus()
	if status.State != IODeviceWeightReleasePending || !status.Programmed {
		t.Fatalf("disabled programmed policy status = %+v", status)
	}
}
