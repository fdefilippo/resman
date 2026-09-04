package state

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

type systemdCPUApplyCall struct {
	unit        string
	assignments map[systemdunit.PropertyName]uint64
}

type fakeSystemdCPUUnitAdapter struct {
	topology       systemdunit.TopologySnapshot
	applies        []systemdCPUApplyCall
	restores       []string
	inactiveClean  []string
	owned          map[string]systemdunit.UnitIdentity
	failApplyUnit  string
	reconcileError error
	discoverError  error
	closed         bool
}

func (a *fakeSystemdCPUUnitAdapter) Discover(context.Context) (systemdunit.TopologySnapshot, error) {
	if a.discoverError != nil {
		return systemdunit.TopologySnapshot{}, a.discoverError
	}
	return a.topology, nil
}

func (a *fakeSystemdCPUUnitAdapter) Apply(_ context.Context, identity systemdunit.UnitIdentity, assignments []systemdunit.PropertyAssignment) (systemdunit.UnitSnapshot, error) {
	values := make(map[systemdunit.PropertyName]uint64, len(assignments))
	for _, assignment := range assignments {
		values[assignment.Name()] = assignment.Value()
	}
	a.applies = append(a.applies, systemdCPUApplyCall{unit: identity.Name, assignments: values})
	if a.owned == nil {
		a.owned = make(map[string]systemdunit.UnitIdentity)
	}
	a.owned[identity.Name] = identity
	if identity.Name == a.failApplyUnit {
		return systemdunit.UnitSnapshot{}, errors.New("injected systemd apply failure")
	}
	return systemdunit.UnitSnapshot{Identity: identity}, nil
}

func (a *fakeSystemdCPUUnitAdapter) Restore(_ context.Context, identity systemdunit.UnitIdentity) (systemdunit.RestoreResult, error) {
	a.restores = append(a.restores, identity.Name)
	delete(a.owned, identity.Name)
	return systemdunit.RestoreResult{}, nil
}

func (a *fakeSystemdCPUUnitAdapter) ReconcileOwned(context.Context) error {
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
	result := make([]systemdunit.UnitIdentity, 0, len(a.owned))
	for _, identity := range a.owned {
		result = append(result, identity)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}

func (a *fakeSystemdCPUUnitAdapter) Close() { a.closed = true }

type forbiddenSystemdNativeCgroupManager struct {
	mockCgroupManager
	mutations []string
}

func (m *forbiddenSystemdNativeCgroupManager) EnsureCPUPointsHierarchy(cpupoints.ParentQuota, cpupoints.KernelCPUWeight) (cgroup.CPUPointsHierarchy, error) {
	m.mutations = append(m.mutations, "ensure_hierarchy")
	return cgroup.CPUPointsHierarchy{}, nil
}

func (m *forbiddenSystemdNativeCgroupManager) EnsureCPUPointsUserPlacement(int, string, cpupoints.KernelCPUWeight) (string, cgroup.ProcessMoveResult, error) {
	m.mutations = append(m.mutations, "move_processes")
	return "", cgroup.ProcessMoveResult{}, nil
}

func (m *forbiddenSystemdNativeCgroupManager) ApplyCPUPointsParentQuota(cgroup.CPUPointsHierarchy, cpupoints.ParentQuota) error {
	m.mutations = append(m.mutations, "write_parent_cgroupfs")
	return nil
}

func (m *forbiddenSystemdNativeCgroupManager) ApplyCPUPointsUserWeight(string, cpupoints.KernelCPUWeight) error {
	m.mutations = append(m.mutations, "write_leaf_cgroupfs")
	return nil
}

func (m *forbiddenSystemdNativeCgroupManager) EnsureUserCgroupPlacement(int, string, string) (string, cgroup.ProcessMoveResult, error) {
	m.mutations = append(m.mutations, "legacy_user_placement")
	return "", cgroup.ProcessMoveResult{}, nil
}

func (m *forbiddenSystemdNativeCgroupManager) ApplyRAMLimitWithHigh(int, string, string) error {
	m.mutations = append(m.mutations, "legacy_ram_limit")
	return nil
}

func (m *forbiddenSystemdNativeCgroupManager) ApplyIOLimit(int, string, string, int, int, string) error {
	m.mutations = append(m.mutations, "legacy_io_limit")
	return nil
}

func (m *forbiddenSystemdNativeCgroupManager) GetPSIStats(int) (cgroup.PSIStats, error) {
	m.mutations = append(m.mutations, "legacy_psi_read")
	return cgroup.PSIStats{}, nil
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
	if err := manager.executeDecision("MAINTAIN_CURRENT_STATE", &SystemMetrics{}); err != nil {
		t.Fatalf("executeDecision() error: %v", err)
	}
	if len(adapter.applies) != stageCalls {
		t.Fatalf("maintain repeated systemd writes: before=%d after=%d", stageCalls, len(adapter.applies))
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
			Mode:   cgroup.EnforcementModeObservationOnlySystemd,
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
	manager.cfg.IORemediationEnabled = true
	manager.cfg.IOReadIOPS = 1
	if err := manager.activateLimits(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}

	observed := &SystemMetrics{IOEligibleUsers: []int{1000}}
	manager.collectEligibleBlockIOPS(observed, time.Now(), manager.cfg.GetIODecisionPolicy(), normalCPUQuota)
	if observed.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatalf("systemd-native block-I/O unavailable users = %d, want 1 until the resource adapter exists", observed.IOBlockIOPSUnavailableUsers)
	}
	if err := manager.reconcilePatternPolicy(1000, manager.cfg); err != nil {
		t.Fatalf("reconcilePatternPolicy() error: %v", err)
	}
	if err := manager.stageIORemediation(&controlCycleContext{cfg: manager.cfg}); err != nil {
		t.Fatalf("stageIORemediation() error: %v", err)
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
		WithEnforcementStatus(cgroup.EnforcementStatus{Mode: cgroup.EnforcementModeObservationOnlySystemd, Reason: cgroup.EnforcementReasonSystemdOwnsHostWorkloads}),
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
