package state

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/systemdunit"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type systemdCPUApplyCall struct {
	unit        string
	assignments map[systemdunit.PropertyName]uint64
	devices     map[systemdunit.PropertyName][]systemdunit.DeviceLimit
}

type systemdResourceCheckCall struct {
	uid      uint32
	resource systemdunit.ResourceKind
}

type systemdPropertyRestoreCall struct {
	unit       string
	properties []systemdunit.PropertyName
}

type fakeSystemdCPUUnitAdapter struct {
	mu                 sync.Mutex
	topology           systemdunit.TopologySnapshot
	applies            []systemdCPUApplyCall
	restores           []string
	propertyRestores   []systemdPropertyRestoreCall
	inactiveClean      []string
	owned              map[string]systemdunit.UnitIdentity
	activeProperties   map[string]map[systemdunit.PropertyName]bool
	failApplyUnit      string
	failRestoreUnit    string
	propertyConflicts  map[string][]systemdunit.PropertyName
	applyHook          func(string)
	reconcileError     error
	discoverError      error
	authority          map[systemdunit.ResourceKind]systemdunit.ResourceAuthority
	authorityError     map[systemdunit.ResourceKind]error
	resourceChecks     []systemdResourceCheckCall
	authorityChecks    int
	authorityHook      func(int)
	authorityCounts    map[int]int
	confirmCalls       int
	confirmHook        func(int)
	confirmError       error
	confirmedApplied   []string
	confirmApplyErr    map[string]error
	confirmStarted     chan struct{}
	confirmProceed     chan struct{}
	closed             bool
	inventoryCaptures  int
	inventoryDetails   []bool
	ioWeightProbeCalls int
	ioWeightProbeError error
}

func (a *fakeSystemdCPUUnitAdapter) ProbeIODeviceWeights(_ context.Context, _ []systemdunit.IODeviceWeightProbeTarget) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ioWeightProbeCalls++
	return a.ioWeightProbeError
}

type systemdPlanCaptureLogger struct {
	infos []string
}

func (*systemdPlanCaptureLogger) Debug(string, ...interface{}) {}
func (l *systemdPlanCaptureLogger) Info(message string, _ ...interface{}) {
	l.infos = append(l.infos, message)
}
func (*systemdPlanCaptureLogger) Warn(string, ...interface{})               {}
func (*systemdPlanCaptureLogger) Error(string, ...interface{})              {}
func (*systemdPlanCaptureLogger) DebugChecked(string, ...interface{}) error { return nil }
func (*systemdPlanCaptureLogger) InfoChecked(string, ...interface{}) error  { return nil }

func (a *fakeSystemdCPUUnitAdapter) Discover(context.Context) (systemdunit.TopologySnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.discoverError != nil {
		return systemdunit.TopologySnapshot{}, a.discoverError
	}
	return a.topology, nil
}

func (a *fakeSystemdCPUUnitAdapter) ConfirmTopology(_ context.Context, expected systemdunit.TopologySnapshot) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.confirmCalls++
	if a.confirmHook != nil {
		a.confirmHook(a.confirmCalls)
	}
	if a.confirmError != nil {
		return a.confirmError
	}
	if !reflect.DeepEqual(topologyIdentities(expected), topologyIdentities(a.topology)) {
		return &systemdunit.AdapterError{Reason: systemdunit.ReasonTopologyChanged, Operation: "confirm_topology", Err: errors.New("injected topology turnover")}
	}
	return nil
}

func topologyIdentities(topology systemdunit.TopologySnapshot) []systemdunit.UnitIdentity {
	result := []systemdunit.UnitIdentity{topology.Parent.Identity}
	for _, user := range topology.Users {
		result = append(result, user.Unit.Identity)
	}
	return result
}

func (a *fakeSystemdCPUUnitAdapter) ConfirmApplied(_ context.Context, identity systemdunit.UnitIdentity, _ []systemdunit.PropertyAssignment) (systemdunit.UnitSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.confirmStarted != nil {
		close(a.confirmStarted)
		a.confirmStarted = nil
		<-a.confirmProceed
	}
	a.confirmedApplied = append(a.confirmedApplied, identity.Name)
	if err := a.confirmApplyErr[identity.Name]; err != nil {
		return systemdunit.UnitSnapshot{}, err
	}
	return systemdunit.UnitSnapshot{Identity: identity}, nil
}

func (a *fakeSystemdCPUUnitAdapter) Apply(_ context.Context, identity systemdunit.UnitIdentity, assignments []systemdunit.PropertyAssignment) (systemdunit.UnitSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	values := make(map[systemdunit.PropertyName]uint64, len(assignments))
	devices := make(map[systemdunit.PropertyName][]systemdunit.DeviceLimit)
	for _, assignment := range assignments {
		if limits := assignment.DeviceLimits(); limits != nil {
			devices[assignment.Name()] = limits
		} else {
			values[assignment.Name()] = assignment.Value()
		}
	}
	a.applies = append(a.applies, systemdCPUApplyCall{unit: identity.Name, assignments: values, devices: devices})
	if a.applyHook != nil {
		a.applyHook(identity.Name)
	}
	if a.owned == nil {
		a.owned = make(map[string]systemdunit.UnitIdentity)
	}
	a.owned[identity.Name] = identity
	if a.activeProperties == nil {
		a.activeProperties = make(map[string]map[systemdunit.PropertyName]bool)
	}
	if a.activeProperties[identity.Name] == nil {
		a.activeProperties[identity.Name] = make(map[systemdunit.PropertyName]bool)
	}
	for _, assignment := range assignments {
		a.activeProperties[identity.Name][assignment.Name()] = true
	}
	if identity.Name == a.failApplyUnit {
		return systemdunit.UnitSnapshot{}, errors.New("injected systemd apply failure")
	}
	return systemdunit.UnitSnapshot{Identity: identity}, nil
}

func (a *fakeSystemdCPUUnitAdapter) CaptureProcessAuthorityInventory(_ context.Context, topology systemdunit.TopologySnapshot, sampleEpochID int64, includeResourceDetail bool) (systemdunit.ProcessAuthorityInventory, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inventoryCaptures++
	a.inventoryDetails = append(a.inventoryDetails, includeResourceDetail)
	observations := make([]systemdunit.ProcessAuthorityObservation, 0, len(topology.Users))
	for _, user := range topology.Users {
		observations = append(observations, systemdunit.ProcessAuthorityObservation{
			UID:               user.UID,
			Identity:          user.Unit.Identity,
			CPUCoverage:       true,
			ResourceAuthority: systemdunit.ResourceAuthority{State: systemdunit.ResourceCoverageComplete, Reason: systemdunit.ResourceCoverageVerified},
		})
	}
	return systemdunit.NewProcessAuthorityInventory(sampleEpochID, systemdunit.TopologyFingerprint(topology), includeResourceDetail, observations), nil
}

func (a *fakeSystemdCPUUnitAdapter) CheckCapturedResourceAuthorities(_ context.Context, inventory systemdunit.ProcessAuthorityInventory, requests []systemdunit.ResourceAuthorityRequest) ([]systemdunit.ResourceAuthorityResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.authorityChecks++
	if a.authorityHook != nil {
		a.authorityHook(a.authorityChecks)
	}
	results := make([]systemdunit.ResourceAuthorityResult, len(requests))
	for index, request := range requests {
		a.resourceChecks = append(a.resourceChecks, systemdResourceCheckCall{uid: request.UID, resource: request.Resource})
		if _, found := inventory.Observation(request.UID, request.Identity); !found {
			authority := systemdunit.ResourceAuthority{Resource: request.Resource, State: systemdunit.ResourceCoverageRefused, Reason: systemdunit.ResourceCoverageTopologyChanged}
			results[index] = systemdunit.ResourceAuthorityResult{Authority: authority, Err: &systemdunit.ResourceAuthorityError{UID: request.UID, Authority: authority, Err: errors.New("unit is absent from the sample authority inventory")}}
			continue
		}
		authority, ok := a.authority[request.Resource]
		if !ok {
			authority = systemdunit.ResourceAuthority{Resource: request.Resource, State: systemdunit.ResourceCoverageComplete, Reason: systemdunit.ResourceCoverageVerified}
		}
		results[index] = systemdunit.ResourceAuthorityResult{Authority: authority, Err: a.authorityError[request.Resource]}
	}
	if count, ok := a.authorityCounts[a.authorityChecks]; ok {
		return results[:count], nil
	}
	return results, nil
}

func (a *fakeSystemdCPUUnitAdapter) Restore(_ context.Context, identity systemdunit.UnitIdentity) (systemdunit.RestoreResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.restores = append(a.restores, identity.Name)
	if identity.Name == a.failRestoreUnit {
		return systemdunit.RestoreResult{}, errors.New("injected systemd restore failure")
	}
	delete(a.owned, identity.Name)
	delete(a.activeProperties, identity.Name)
	return systemdunit.RestoreResult{}, nil
}

func (a *fakeSystemdCPUUnitAdapter) RestoreProperties(_ context.Context, identity systemdunit.UnitIdentity, properties []systemdunit.PropertyName) (systemdunit.RestoreResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.restores = append(a.restores, identity.Name)
	a.propertyRestores = append(a.propertyRestores, systemdPropertyRestoreCall{unit: identity.Name, properties: append([]systemdunit.PropertyName(nil), properties...)})
	conflictProperties := append([]systemdunit.PropertyName(nil), a.propertyConflicts[identity.Name]...)
	conflictSet := make(map[systemdunit.PropertyName]struct{}, len(conflictProperties))
	conflicts := make([]systemdunit.PropertyConflict, 0, len(conflictProperties))
	for _, property := range conflictProperties {
		conflictSet[property] = struct{}{}
		conflicts = append(conflicts, systemdunit.PropertyConflict{Property: property})
	}
	restored := make([]systemdunit.PropertyName, 0, len(properties))
	for _, property := range properties {
		if _, conflict := conflictSet[property]; conflict {
			continue
		}
		delete(a.activeProperties[identity.Name], property)
		restored = append(restored, property)
	}
	result := systemdunit.RestoreResult{Restored: restored, Conflicts: conflicts}
	if len(conflicts) != 0 {
		return result, &systemdunit.RestoreConflictError{Unit: identity.Name, Conflicts: conflicts}
	}
	return result, nil
}

func (a *fakeSystemdCPUUnitAdapter) ReconcileOwned(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reconcileError != nil {
		return a.reconcileError
	}
	active := map[string]systemdunit.UnitIdentity{a.topology.Parent.Identity.Name: a.topology.Parent.Identity}
	for _, user := range a.topology.Users {
		active[user.Unit.Identity.Name] = user.Unit.Identity
	}
	for name, identity := range a.owned {
		current, ok := active[name]
		if !ok {
			a.inactiveClean = append(a.inactiveClean, name)
			delete(a.owned, name)
			continue
		}
		if current != identity {
			a.owned[name] = current
		}
	}
	return nil
}

func (a *fakeSystemdCPUUnitAdapter) OwnedUnits() []systemdunit.UnitIdentity {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]systemdunit.UnitIdentity, 0, len(a.owned))
	for _, identity := range a.owned {
		result = append(result, identity)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}

func (a *fakeSystemdCPUUnitAdapter) Leases(identity systemdunit.UnitIdentity) []systemdunit.PropertyLease {
	a.mu.Lock()
	defer a.mu.Unlock()
	ownedIdentity, ok := a.owned[identity.Name]
	if !ok || ownedIdentity != identity {
		return nil
	}
	properties := a.activeProperties[identity.Name]
	if len(properties) == 0 {
		property := systemdunit.PropertyCPUWeight
		if identity.Name == "user.slice" {
			property = systemdunit.PropertyCPUQuotaPerSecUSec
		}
		return []systemdunit.PropertyLease{{Property: property, Baseline: systemdunit.SystemdUnset, LastApplied: 1}}
	}
	result := make([]systemdunit.PropertyLease, 0, len(properties))
	for property := range properties {
		lease := systemdunit.PropertyLease{Property: property, Baseline: systemdunit.SystemdUnset, LastApplied: 1}
		switch property {
		case systemdunit.PropertyIODeviceWeight, systemdunit.PropertyIOReadBandwidthMax, systemdunit.PropertyIOWriteBandwidthMax, systemdunit.PropertyIOReadIOPSMax, systemdunit.PropertyIOWriteIOPSMax:
			lease.LastAppliedDeviceLimits = []systemdunit.DeviceLimit{{Path: "/dev/vda", Value: 1}}
		}
		result = append(result, lease)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Property < result[right].Property })
	return result
}

func (a *fakeSystemdCPUUnitAdapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
}

func (a *fakeSystemdCPUUnitAdapter) replaceTopology(topology systemdunit.TopologySnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.topology = topology
}

type forbiddenSystemdNativeCgroupManager struct {
	mockCgroupManager
	mutations []string
}

func TestSystemdNativeCycleReportsOnlyAnAcknowledgedAppliedAction(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{"^alice$"}
	run := &controlCycleContext{
		ctx:      context.Background(),
		cycleID:  1,
		decision: decisionActivate,
		metrics:  &SystemMetrics{CPUEligibleUsers: []int{1000}},
	}

	if err := manager.stageExecuteDecision(run); err != nil {
		t.Fatalf("stageExecuteDecision() error: %v", err)
	}
	want := cgroup.EnforcementCycleState{
		Mode:            cgroup.EnforcementModeSystemdNative,
		RequestedIntent: cgroup.EnforcementPolicyIntentActivate,
		AppliedAction:   cgroup.AppliedEnforcementActionActivate,
		BlockReason:     cgroup.EnforcementBlockReasonNone,
	}
	if run.enforcementState != want {
		t.Fatalf("cycle enforcement state = %+v, want %+v", run.enforcementState, want)
	}
	status := manager.GetStatus()
	if status.RequestedPolicyIntent != want.RequestedIntent ||
		status.AppliedEnforcementAction != want.AppliedAction ||
		status.EnforcementBlockReason != want.BlockReason {
		t.Fatalf("runtime enforcement projection = %+v, want %+v", status, want)
	}
}

func TestSystemdNativeCycleFailuresCannotPublishSuccessfulNonDegradedOutcomes(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		decision string
		prepare  func(*testing.T, *Manager, *fakeSystemdCPUUnitAdapter)
	}{
		{
			name:     "identity",
			decision: decisionActivate,
			prepare: func(_ *testing.T, _ *Manager, adapter *fakeSystemdCPUUnitAdapter) {
				adapter.confirmError = &systemdunit.AdapterError{Reason: systemdunit.ReasonTopologyChanged, Operation: "confirm_topology", Err: errors.New("injected identity change")}
			},
		},
		{
			name:     "application",
			decision: decisionActivate,
			prepare: func(_ *testing.T, _ *Manager, adapter *fakeSystemdCPUUnitAdapter) {
				adapter.failApplyUnit = "user-1000.slice"
			},
		},
		{
			name:     "readback",
			decision: decisionActivate,
			prepare: func(_ *testing.T, _ *Manager, adapter *fakeSystemdCPUUnitAdapter) {
				adapter.confirmApplyErr = map[string]error{"user-1000.slice": errors.New("injected readback failure")}
			},
		},
		{
			name:     "restore",
			decision: decisionDeactivate,
			prepare: func(t *testing.T, manager *Manager, adapter *fakeSystemdCPUUnitAdapter) {
				if err := manager.activateSystemdCPUPoints(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
					t.Fatalf("prepare active plan: %v", err)
				}
				adapter.failRestoreUnit = "user-1000.slice"
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			policy := testCPUPointsPolicy(t, map[string]struct {
				uid    int
				points int
			}{"alice": {uid: 1000, points: 300}})
			adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
			manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
			manager.cfg.UserIncludeList = []string{"^alice$"}
			logger := &failingCompletionLogger{}
			manager.logger = logger
			scenario.prepare(t, manager, adapter)

			sample := &SystemMetrics{
				CPUEligibleUsers: []int{1000},
				CPUPointsUsers:   make(map[int]resmanmetrics.CPUPointsUserSnapshot),
				PersistenceUsers: make(map[int]resmanmetrics.UserPersistenceMetrics),
			}
			run := &controlCycleContext{
				ctx:       context.Background(),
				cfg:       manager.GetConfig(),
				cycleID:   1,
				trigger:   ControlCycleTriggerManual,
				startTime: time.Now(),
				decision:  scenario.decision,
				reason:    "test decision",
				metrics:   sample,
			}
			err := runControlCyclePipeline(manager, run, []controlCycleStage{
				{name: "execute_decision", run: (*Manager).stageExecuteDecision, continueAfterError: true},
				{name: "finalize_enforcement_observation", run: (*Manager).stageFinalizeEnforcementObservation},
				{name: "update_prometheus", run: (*Manager).stageUpdatePrometheus},
				{name: "log_completion", run: (*Manager).stageLogCompletion},
			})
			if err == nil {
				t.Fatal("failed systemd-native cycle returned nil")
			}
			fields := logFieldsByKey(t, logger.fields)
			if fields["outcome"] != "degraded" || fields["deferred_error_count"] == 0 {
				t.Fatalf("completion outcome=%#v deferred_error_count=%#v, want degraded and non-zero", fields["outcome"], fields["deferred_error_count"])
			}
			if !sample.PersistenceSystem.CPUPointsDegraded || !sample.CPUPointsSystem.ReconciliationDegraded {
				t.Fatalf("failed cycle snapshots remained non-degraded: persistence=%t runtime=%t", sample.PersistenceSystem.CPUPointsDegraded, sample.CPUPointsSystem.ReconciliationDegraded)
			}
			status := manager.GetStatus()
			if status.EnforcementMode != cgroup.EnforcementModeSystemdNative || !status.CPUPoints.ReconciliationDegraded {
				t.Fatalf("runtime status = mode=%s degraded=%t", status.EnforcementMode, status.CPUPoints.ReconciliationDegraded)
			}
			exported := manager.prometheusExporter.(*mockPrometheusExporter).snapshot().lastSystemSnapshot
			if exported.EnforcementMode != cgroup.EnforcementModeSystemdNative || exported.CPUPoints == nil || !exported.CPUPoints.ReconciliationDegraded {
				t.Fatalf("Prometheus snapshot = mode=%s cpu_points=%+v", exported.EnforcementMode, exported.CPUPoints)
			}
		})
	}
}

func TestSystemdNativeReconciliationAppliesOneCompleteFlatPlanWithoutPIDMigration(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000, 1001, 1002)}
	cgroups := &forbiddenSystemdNativeCgroupManager{}
	manager := testSystemdCPUPointsManager(t, policy, adapter, cgroups, 4)
	manager.cfg.UserIncludeList = []string{"^alice$", "^bob$", "^carol$"}
	manager.cfg.UserExcludeList = []string{"^carol$"}

	err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000, 1001}})
	if err != nil {
		t.Fatalf("activateLimits() error: %v", err)
	}
	if len(cgroups.mutations) != 0 {
		t.Fatalf("systemd-native reconciliation called cgroup mutation methods: %v", cgroups.mutations)
	}
	if len(adapter.applies) != 5 {
		t.Fatalf("systemd apply calls = %d, want parent plus four active slices", len(adapter.applies))
	}
	assertSystemdCPUAssignments(t, adapter.applies, map[string]map[systemdunit.PropertyName]uint64{
		"user.slice":      {systemdunit.PropertyCPUQuotaPerSecUSec: 3_600_000, systemdunit.PropertyCPUQuotaPeriodUSec: 100_000},
		"user-0.slice":    {systemdunit.PropertyCPUWeight: 3_300},
		"user-1000.slice": {systemdunit.PropertyCPUWeight: 9_900},
		"user-1001.slice": {systemdunit.PropertyCPUWeight: 1_650},
		"user-1002.slice": {systemdunit.PropertyCPUWeight: 1_650},
	})
	for _, call := range adapter.applies {
		if call.unit != "user.slice" {
			if _, finiteQuota := call.assignments[systemdunit.PropertyCPUQuotaPerSecUSec]; finiteQuota {
				t.Fatalf("leaf %s received a finite CPU quota: %v", call.unit, call.assignments)
			}
		}
	}
	if !manager.systemdCPUComplete || !manager.limitsActive || len(manager.systemdCPUSlices) != 4 {
		t.Fatalf("published state complete=%t active=%t slices=%d", manager.systemdCPUComplete, manager.limitsActive, len(manager.systemdCPUSlices))
	}
	if !manager.activeUsers[1000] || !manager.activeUsers[1001] || manager.activeUsers[1002] || manager.activeUsers[0] {
		t.Fatalf("individual policy state = %v, want only eligible requested users", manager.activeUsers)
	}
	status := manager.GetStatus()
	if status.EnforcementMode != cgroup.EnforcementModeSystemdNative || status.EnforcementReason != cgroup.EnforcementReasonSystemdNativeAdapter {
		t.Fatalf("runtime enforcement status = %+v, want systemd-native adapter", status)
	}
}

func TestSystemdNativeReconciliationDoesNotPublishAPartialPlanAndRetries(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000, 1001), failApplyUnit: "user-1001.slice"}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}

	err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000, 1001}})
	if err == nil {
		t.Fatal("activateLimits() error = nil, want failed leaf application")
	}
	var reconciliationErr *CPUPointsReconciliationError
	if !errors.As(err, &reconciliationErr) || reconciliationErr.Step != "systemd_leaf_weight" {
		t.Fatalf("activateLimits() error = %v, want typed systemd_leaf_weight reconciliation failure", err)
	}
	if manager.systemdCPUComplete || manager.limitsActive || len(manager.activeUsers) != 0 || !manager.cpuPointsDegraded {
		t.Fatalf("partial publication complete=%t active=%t users=%v degraded=%t", manager.systemdCPUComplete, manager.limitsActive, manager.activeUsers, manager.cpuPointsDegraded)
	}
	if len(adapter.OwnedUnits()) == 0 {
		t.Fatal("partial application lost durable owned units needed for retry and restoration")
	}

	adapter.failApplyUnit = ""
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatalf("stageReconcileCPUPoints() retry error: %v", err)
	}
	if !manager.systemdCPUComplete || !manager.limitsActive || manager.cpuPointsDegraded {
		t.Fatalf("retry publication complete=%t active=%t degraded=%t", manager.systemdCPUComplete, manager.limitsActive, manager.cpuPointsDegraded)
	}
}

func TestSystemdNativeReconciliationConfirmsTopologyBeforeAnyMutation(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	adapter.confirmHook = func(call int) {
		if call == 1 {
			adapter.topology = testSystemdTopology(0, 1001)
		}
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}

	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000, 1001}}); err != nil {
		t.Fatalf("activateLimits() error: %v", err)
	}
	for _, call := range adapter.applies {
		if call.unit == "user-1000.slice" {
			t.Fatalf("stale slice was mutated before pre-mutation confirmation: %+v", adapter.applies)
		}
	}
	if !manager.activeUsers[1001] || manager.activeUsers[1000] {
		t.Fatalf("published users = %v, want only the confirmed replacement", manager.activeUsers)
	}
}

func TestSystemdNativeReconciliationConfirmsTopologyBeforeAcknowledgement(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	adapter.confirmHook = func(call int) {
		if call == 2 {
			adapter.topology = testSystemdTopology(0, 1000, 1001)
		}
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}

	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000, 1001}}); err != nil {
		t.Fatalf("activateLimits() error: %v", err)
	}
	if adapter.confirmCalls != 4 {
		t.Fatalf("topology confirmations = %d, want pre/post confirmation on both bounded attempts", adapter.confirmCalls)
	}
	if !manager.activeUsers[1000] || !manager.activeUsers[1001] || len(manager.systemdCPUSlices) != 3 {
		t.Fatalf("published stale topology users=%v slices=%v", manager.activeUsers, manager.systemdCPUSlices)
	}
}

func TestSystemdNativeReconciliationRetriesAnOnlineCPUChangeBeforeAcknowledgement(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	capacity := manager.cpuCapacity.(*mutableCPUCapacityProvider)
	capacity.onRefresh = func(call int, provider *mutableCPUCapacityProvider) {
		if call == 2 {
			provider.cpus = 2
		}
	}

	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatalf("activateLimits() error: %v", err)
	}
	if capacity.refreshCalls != 4 {
		t.Fatalf("capacity refreshes = %d, want initial/final checks on both bounded attempts", capacity.refreshCalls)
	}
	lastParent := uint64(0)
	for _, call := range adapter.applies {
		if call.unit == "user.slice" {
			lastParent = call.assignments[systemdunit.PropertyCPUQuotaPerSecUSec]
		}
	}
	if lastParent != 1_800_000 {
		t.Fatalf("published parent quota = %d, want quota recomputed for two online CPUs", lastParent)
	}
}

func TestSystemdNativeReconciliationBoundsTransientTopologyRetries(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{
		topology:     testSystemdTopology(0, 1000),
		confirmError: &systemdunit.AdapterError{Reason: systemdunit.ReasonTopologyChanged, Operation: "confirm_topology", Err: errors.New("continuous turnover")},
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}

	err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}})
	if err == nil || !systemdunit.IsRetryableReconciliation(err) {
		t.Fatalf("activateLimits() error = %v, want typed retryable topology outcome", err)
	}
	if adapter.confirmCalls != 2 || len(adapter.applies) != 0 {
		t.Fatalf("bounded retry confirmations=%d mutations=%v, want two attempts and no mutation", adapter.confirmCalls, adapter.applies)
	}
	if manager.systemdCPUComplete || !manager.cpuPointsDegraded {
		t.Fatalf("optimistic acknowledgement complete=%t degraded=%t", manager.systemdCPUComplete, manager.cpuPointsDegraded)
	}
}

func TestSystemdNativeCancelledReconciliationDoesNotRetryOrAcknowledge(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{
		topology:     testSystemdTopology(0, 1000),
		confirmError: context.Canceled,
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := manager.reconcileSystemdCPUPoints(ctx, policy)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reconcileSystemdCPUPoints() error = %v, want context cancellation", err)
	}
	if adapter.confirmCalls != 1 || len(adapter.applies) != 0 {
		t.Fatalf("cancelled reconciliation confirmations=%d mutations=%v", adapter.confirmCalls, adapter.applies)
	}
	if manager.systemdCPUComplete || !manager.cpuPointsDegraded {
		t.Fatalf("cancelled reconciliation complete=%t degraded=%t", manager.systemdCPUComplete, manager.cpuPointsDegraded)
	}
}

func TestSystemdNativeReconciliationDoesNotAcknowledgeAnExternalPropertyChange(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: testSystemdTopology(0, 1000),
		confirmApplyErr: map[string]error{
			"user-1000.slice": &systemdunit.AdapterError{Reason: systemdunit.ReasonExternalConflict, Operation: "confirm_applied", Unit: "user-1000.slice", Err: errors.New("operator changed CPUWeight")},
		},
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}

	err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}})
	var adapterErr *systemdunit.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Reason != systemdunit.ReasonExternalConflict {
		t.Fatalf("activateLimits() error = %v, want preserved external-property conflict", err)
	}
	if manager.systemdCPUComplete || !manager.cpuPointsDegraded || manager.limitsActive {
		t.Fatalf("external change was acknowledged complete=%t degraded=%t active=%t", manager.systemdCPUComplete, manager.cpuPointsDegraded, manager.limitsActive)
	}
}

func TestSystemdNativePolicyReconciliationsAreSerializedByTheManagerOperationGate(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	manager.systemdCPURequested = true

	leave := manager.opGate.Enter()
	done := make(chan error, 1)
	go func() {
		done <- manager.ReconcileCPUPointsPolicy(policy, policy)
	}()
	select {
	case err := <-done:
		t.Fatalf("policy reconciliation bypassed manager operation gate: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	leave()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReconcileCPUPointsPolicy() error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serialized policy reconciliation did not complete")
	}
}

func TestSystemdNativeStateRemainsReadableDuringFinalBusAndKernelConfirmation(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	confirmStarted := make(chan struct{})
	confirmProceed := make(chan struct{})
	adapter := &fakeSystemdCPUUnitAdapter{
		topology:       testSystemdTopology(0, 1000),
		confirmStarted: confirmStarted,
		confirmProceed: confirmProceed,
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}})
	}()
	<-confirmStarted

	statusDone := make(chan RuntimeStatus, 1)
	go func() { statusDone <- manager.GetStatus() }()
	select {
	case <-statusDone:
	case <-time.After(time.Second):
		close(confirmProceed)
		<-reconcileDone
		t.Fatal("state mutex was held during final systemd confirmation")
	}
	close(confirmProceed)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("activateLimits() error: %v", err)
	}
}

func TestSystemdNativeRepeatedArrivalDepartureReconciliationHasNoOrphanedWork(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	manager.systemdCPURequested = true

	const workers = 16
	done := make(chan error, workers)
	turnoverDone := make(chan struct{})
	go func() {
		defer close(turnoverDone)
		for index := 0; index < workers*4; index++ {
			if index%2 == 0 {
				adapter.replaceTopology(testSystemdTopology(0, 1000, 1001))
			} else {
				adapter.replaceTopology(testSystemdTopology(0, 1000))
			}
			runtime.Gosched()
		}
	}()
	for index := 0; index < workers; index++ {
		go func() {
			done <- manager.ReconcileCPUPointsPolicy(policy, policy)
		}()
	}
	for index := 0; index < workers; index++ {
		select {
		case err := <-done:
			if err != nil && !systemdunit.IsRetryableReconciliation(err) {
				t.Fatalf("concurrent reconciliation error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent reconciliation deadlocked")
		}
	}
	<-turnoverDone
	adapter.replaceTopology(testSystemdTopology(0, 1000))
	if err := manager.ReconcileCPUPointsPolicy(policy, policy); err != nil {
		t.Fatalf("stable reconciliation after turnover error: %v", err)
	}
	if !manager.systemdCPUComplete || !manager.activeUsers[1000] {
		t.Fatalf("final acknowledgement complete=%t users=%v", manager.systemdCPUComplete, manager.activeUsers)
	}
}

func TestSystemdNativeMaintainDoesNotRepeatTheStageReconciliation(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}

	adapter.applies = nil
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatalf("stageReconcileCPUPoints() error: %v", err)
	}
	stageCalls := len(adapter.applies)
	restoresBefore := len(adapter.restores)
	metrics := &SystemMetrics{CPUEligibleUsers: []int{1000}}
	if err := manager.executeDecision("MAINTAIN_CURRENT_STATE", metrics); err != nil {
		t.Fatalf("executeDecision() error: %v", err)
	}
	if len(adapter.applies) != stageCalls {
		t.Fatalf("maintain repeated systemd writes: before=%d after=%d", stageCalls, len(adapter.applies))
	}
	if len(adapter.restores) != restoresBefore {
		t.Fatalf("maintain unexpectedly restored systemd properties: before=%d after=%d", restoresBefore, len(adapter.restores))
	}
}

func TestSystemdNativeReconciliationReleasesThePlanWhenOnlyRootRemains(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}

	adapter.applies = nil
	adapter.restores = nil
	adapter.topology = testSystemdTopology(0)
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatalf("stageReconcileCPUPoints() error: %v", err)
	}
	if len(adapter.applies) != 0 {
		t.Fatalf("root-only topology was reapplied: %+v", adapter.applies)
	}
	if !reflect.DeepEqual(adapter.restores, []string{"user-0.slice", "user.slice"}) {
		t.Fatalf("root-only restore order = %v, want root then parent", adapter.restores)
	}
	if manager.systemdCPURequested || manager.limitsActive || manager.systemdCPUComplete || len(adapter.OwnedUnits()) != 0 {
		t.Fatalf("root-only release requested=%t active=%t complete=%t owned=%v", manager.systemdCPURequested, manager.limitsActive, manager.systemdCPUComplete, adapter.OwnedUnits())
	}
}

func TestSystemdNativeReconciliationTracksArrivalDepartureAndLiveCapacity(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}

	adapter.applies = nil
	adapter.topology = testSystemdTopology(0, 1000, 1001)
	manager.cpuCapacity.(*mutableCPUCapacityProvider).cpus = 2
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatalf("arrival/hotplug reconciliation error: %v", err)
	}
	if !manager.activeUsers[1001] || !manager.requestedCPUUsers[1001] {
		t.Fatalf("new eligible slice was not published as requested and active: requested=%v active=%v", manager.requestedCPUUsers, manager.activeUsers)
	}
	assertSystemdCPUAssignments(t, adapter.applies, map[string]map[systemdunit.PropertyName]uint64{
		"user.slice":      {systemdunit.PropertyCPUQuotaPerSecUSec: 1_800_000, systemdunit.PropertyCPUQuotaPeriodUSec: 100_000},
		"user-0.slice":    {systemdunit.PropertyCPUWeight: 3_300},
		"user-1000.slice": {systemdunit.PropertyCPUWeight: 9_900},
		"user-1001.slice": {systemdunit.PropertyCPUWeight: 3_300},
	})

	adapter.applies = nil
	adapter.topology = testSystemdTopology(0, 1001)
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatalf("departure reconciliation error: %v", err)
	}
	if !reflect.DeepEqual(adapter.inactiveClean, []string{"user-1000.slice"}) {
		t.Fatalf("inactive cleanups = %v, want departed user-1000.slice", adapter.inactiveClean)
	}
	if manager.activeUsers[1000] || !manager.activeUsers[1001] || len(manager.systemdCPUSlices) != 2 {
		t.Fatalf("published users=%v slices=%v after departure", manager.activeUsers, manager.systemdCPUSlices)
	}
}

func TestSystemdNativeReconciliationPublishesSiblingDenominatorsOnlyAfterKernelOrdering(t *testing.T) {
	policy := testCPUPointsPolicy(t, nil)
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}

	adapter.topology = testSystemdTopology(0, 1000, 1001)
	arrivalObserved := false
	adapter.applyHook = func(unit string) {
		if unit != "user-1001.slice" {
			return
		}
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		arrivalObserved = true
		if manager.systemdCPUComplete || manager.systemdCPUSlices[1001].Name != "" {
			t.Fatalf("new sibling was published before its kernel weight: complete=%t slices=%v", manager.systemdCPUComplete, manager.systemdCPUSlices)
		}
	}
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatal(err)
	}
	if !arrivalObserved || !manager.systemdCPUComplete || manager.systemdCPUSlices[1001].Name == "" {
		t.Fatalf("arrival acknowledgement observed=%t complete=%t slices=%v", arrivalObserved, manager.systemdCPUComplete, manager.systemdCPUSlices)
	}

	adapter.applyHook = func(unit string) {
		if unit != "user.slice" {
			return
		}
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		if manager.systemdCPUComplete || manager.systemdCPUSlices[1001].Name == "" {
			t.Fatalf("departing sibling left the published denominator before kernel reconciliation: complete=%t slices=%v", manager.systemdCPUComplete, manager.systemdCPUSlices)
		}
	}
	adapter.topology = testSystemdTopology(0, 1000)
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatal(err)
	}
	if !manager.systemdCPUComplete || manager.systemdCPUSlices[1001].Name != "" {
		t.Fatalf("departure was not published after reconciliation: complete=%t slices=%v", manager.systemdCPUComplete, manager.systemdCPUSlices)
	}
}

func TestSystemdNativePlanLoggingOccursOnlyOnPublishChangeAndRelease(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	logger := &systemdPlanCaptureLogger{}
	manager.logger = logger

	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(logger.infos, []string{"Systemd-native CPU Points plan published"}) {
		t.Fatalf("initial plan log = %v", logger.infos)
	}
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatal(err)
	}
	if len(logger.infos) != 1 {
		t.Fatalf("unchanged plan emitted another log: %v", logger.infos)
	}

	adapter.topology = testSystemdTopology(0, 1000, 1001)
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatal(err)
	}
	if len(logger.infos) != 2 || logger.infos[1] != "Systemd-native CPU Points plan published" {
		t.Fatalf("changed plan log = %v", logger.infos)
	}
	if err := manager.ForceDeactivateLimits(); err != nil {
		t.Fatal(err)
	}
	if len(logger.infos) != 3 || logger.infos[2] != "Systemd-native CPU Points plan released" {
		t.Fatalf("released plan log = %v", logger.infos)
	}
}

func TestSystemdNativePolicyReloadReplansWithoutPublishingTheCandidate(t *testing.T) {
	initial := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	candidate := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 400}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000, 1001)}
	manager := testSystemdCPUPointsManager(t, initial, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000, 1001}}); err != nil {
		t.Fatal(err)
	}

	adapter.applies = nil
	if err := manager.ReconcileCPUPointsPolicy(candidate, initial); err != nil {
		t.Fatalf("ReconcileCPUPointsPolicy() error: %v", err)
	}
	assertSystemdCPUAssignments(t, adapter.applies, map[string]map[systemdunit.PropertyName]uint64{
		"user.slice":      {systemdunit.PropertyCPUQuotaPerSecUSec: 3_600_000, systemdunit.PropertyCPUQuotaPeriodUSec: 100_000},
		"user-0.slice":    {systemdunit.PropertyCPUWeight: 2_500},
		"user-1000.slice": {systemdunit.PropertyCPUWeight: 10_000},
		"user-1001.slice": {systemdunit.PropertyCPUWeight: 2_500},
	})
	currentGuarantee, ok := manager.CurrentCPUPointsPolicy().GuaranteeForUID(1000)
	if !ok || currentGuarantee.Points().Value() != 300 {
		t.Fatal("reconciliation published the detached policy candidate")
	}
}

func TestSystemdNativeReleaseRestoresOnlyOwnedUnitsWithParentLast(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}

	if err := manager.ForceDeactivateLimits(); err != nil {
		t.Fatalf("ForceDeactivateLimits() error: %v", err)
	}
	if !reflect.DeepEqual(adapter.restores, []string{"user-0.slice", "user-1000.slice", "user.slice"}) {
		t.Fatalf("restore order = %v, want leaves then parent", adapter.restores)
	}
	if len(adapter.OwnedUnits()) != 0 || manager.limitsActive || manager.systemdCPUComplete {
		t.Fatalf("release left owned=%v active=%t complete=%t", adapter.OwnedUnits(), manager.limitsActive, manager.systemdCPUComplete)
	}
}

func TestSystemdNativeCleanupRestoresPartialPlanBeforeClosingAdapter(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000), failApplyUnit: "user-1000.slice"}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}

	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err == nil {
		t.Fatal("activateLimits() error = nil, want partial application")
	}
	if manager.limitsActive {
		t.Fatal("partial plan was published as active")
	}
	if len(adapter.OwnedUnits()) == 0 {
		t.Fatal("partial plan did not retain owned properties")
	}

	adapter.failApplyUnit = ""
	if err := manager.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}
	if !adapter.closed {
		t.Fatal("Cleanup() did not close the systemd adapter")
	}
	if len(adapter.OwnedUnits()) != 0 {
		t.Fatalf("Cleanup() left owned units: %v", adapter.OwnedUnits())
	}
}

func TestSystemdNativeManagerRequiresAnAuthoritativeAdapter(t *testing.T) {
	_, err := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, &mockCgroupManager{}, &mockPrometheusExporter{},
		WithEnforcementStatus(cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeSystemdNative,
			Reason: cgroup.EnforcementReasonSystemdNativeAdapter,
		}),
	)
	if err == nil {
		t.Fatal("NewManager() error = nil, want missing systemd adapter rejection")
	}
}

func TestSystemdAdapterCannotBeInstalledUnderANonNativeMode(t *testing.T) {
	adapter := &fakeSystemdCPUUnitAdapter{}
	_, err := NewManager(config.DefaultConfig(), &mockMetricsCollector{}, &mockCgroupManager{}, &mockPrometheusExporter{},
		WithSystemdCPUEnforcement(adapter),
		WithEnforcementStatus(cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeObservationOnly,
			Reason: cgroup.EnforcementReasonSystemdOwnsHostWorkloads,
		}),
	)
	if err == nil {
		t.Fatal("NewManager() error = nil, want adapter and enforcement-mode mismatch rejection")
	}
}

func TestSystemdNativeManagerReconcilesRecoveredDurableOwnership(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	parent := testSystemdUnit("user.slice", 1).Identity
	user := testSystemdUnit("user-1000.slice", 2).Identity
	adapter := &fakeSystemdCPUUnitAdapter{
		topology: testSystemdTopology(0, 1000),
		owned: map[string]systemdunit.UnitIdentity{
			parent.Name: parent,
			user.Name:   user,
		},
	}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	if !manager.systemdCPURequested {
		t.Fatal("recovered durable leases did not schedule first-cycle reconciliation")
	}
	if err := manager.stageReconcileCPUPoints(nil); err != nil {
		t.Fatalf("stageReconcileCPUPoints() error: %v", err)
	}
	if !manager.systemdCPUComplete || !manager.limitsActive {
		t.Fatalf("recovered state complete=%t active=%t, want reconciled publication", manager.systemdCPUComplete, manager.limitsActive)
	}
}

func TestSystemdNativeCPUCannotFallThroughLegacyRAMIOOrRemediationPaths(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	cgroups := &forbiddenSystemdNativeCgroupManager{}
	manager := testSystemdCPUPointsManager(t, policy, adapter, cgroups, 4)
	manager.cfg.UserIncludeList = []string{".*"}
	manager.cfg.RAMEnabled = true
	manager.cfg.IOEnabled = true
	manager.cfg.IOReadIOPS = 1
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}

	observed := &SystemMetrics{IOEligibleUsers: []int{1000}}
	manager.collectEligibleBlockIOPS(observed, manager.cfg.GetIODecisionPolicy())
	if observed.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatalf("systemd-native block-I/O unavailable users = %d, want 1 until the resource adapter exists", observed.IOBlockIOPSUnavailableUsers)
	}
	if len(cgroups.mutations) != 0 {
		t.Fatalf("systemd-native CPU enforcement fell through legacy cgroup paths: %v", cgroups.mutations)
	}
}

func testSystemdCPUPointsManager(t *testing.T, policy cpupoints.PolicySnapshot, adapter *fakeSystemdCPUUnitAdapter, cgroups CgroupManager, cpus uint64) *Manager {
	t.Helper()
	count, _ := cpupoints.NewOnlineCPUCount(cpus)
	quota, err := cpupoints.PlanParentQuota(count, policy.Pool())
	if err != nil {
		t.Fatal(err)
	}
	capacity := &mutableCPUCapacityProvider{state: cpupoints.CapacityState{Available: true, LastVerified: quota}, cpus: cpus}
	collector := &mockMetricsCollector{usernames: map[int]string{0: "root", 1000: "alice", 1001: "bob", 1002: "carol"}}
	manager, err := NewManager(config.DefaultConfig(), collector, cgroups, &mockPrometheusExporter{},
		WithCPUPointsRuntime(policy, capacity),
		WithEnforcementStatus(cgroup.EnforcementStatus{Mode: cgroup.EnforcementModeObservationOnly, Reason: cgroup.EnforcementReasonSystemdOwnsHostWorkloads}),
		WithSystemdCPUEnforcement(adapter),
	)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func testSystemdTopology(uids ...uint32) systemdunit.TopologySnapshot {
	parent := testSystemdUnit("user.slice", 1)
	topology := systemdunit.TopologySnapshot{Parent: parent}
	for index, uid := range uids {
		topology.Users = append(topology.Users, systemdunit.UserSliceSnapshot{
			UID:  uid,
			Unit: testSystemdUnit(fmt.Sprintf("user-%d.slice", uid), uint64(index+2)),
		})
	}
	return topology
}

func testSystemdUnit(name string, seed uint64) systemdunit.UnitSnapshot {
	var invocation [16]byte
	invocation[0] = byte(seed)
	return systemdunit.UnitSnapshot{Identity: systemdunit.UnitIdentity{
		Name: name, ObjectPath: "/org/freedesktop/systemd1/unit/" + name,
		InvocationID: invocation, ControlGroupID: seed,
	}}
}

func assertSystemdCPUAssignments(t *testing.T, calls []systemdCPUApplyCall, want map[string]map[systemdunit.PropertyName]uint64) {
	t.Helper()
	got := make(map[string]map[systemdunit.PropertyName]uint64, len(calls))
	for _, call := range calls {
		got[call.unit] = call.assignments
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("systemd assignments = %v, want %v", got, want)
	}
}
