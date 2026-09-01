package state

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/cpupoints"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type persistenceMetricsCollector struct {
	mockMetricsCollector
	writer *resmanmetrics.DBWriter
}

func (c *persistenceMetricsCollector) GetDBWriter() *resmanmetrics.DBWriter { return c.writer }

type persistenceCgroupManager struct {
	mockCgroupManager
	cpu    map[string]cgroup.CPUPointsNodeSnapshot
	memory map[int]cgroup.MemoryAccountingSnapshot
}

func (m *persistenceCgroupManager) GetCPUPointsNodeSnapshot(path string) (cgroup.CPUPointsNodeSnapshot, error) {
	snapshot, ok := m.cpu[path]
	if !ok {
		return cgroup.CPUPointsNodeSnapshot{}, fmt.Errorf("missing CPU snapshot %s", path)
	}
	return snapshot, nil
}

func (m *persistenceCgroupManager) GetMemoryAccountingSnapshot(uid int) (cgroup.MemoryAccountingSnapshot, error) {
	snapshot, ok := m.memory[uid]
	if !ok {
		return cgroup.MemoryAccountingSnapshot{}, fmt.Errorf("missing RAM snapshot for UID %d", uid)
	}
	return snapshot, nil
}

func TestPersistenceIntervalKeepsPolicyAppliedStateCoverageAndKernelDeltasDistinct(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	cgroups := &persistenceCgroupManager{
		cpu:    make(map[string]cgroup.CPUPointsNodeSnapshot),
		memory: make(map[int]cgroup.MemoryAccountingSnapshot),
	}
	manager := testCPUPointsManagerForReload(t, policy, cgroups)
	dbManager, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbManager.Close() }()
	manager.metricsCollector = &persistenceMetricsCollector{writer: resmanmetrics.NewDBWriter(dbManager, 0)}

	hierarchy := manager.cpuPointsHierarchy
	leaf := hierarchy.Guaranteed + "/user_1000"
	identity := cgroup.CgroupIdentity{Device: 1, Inode: 10}
	cgroups.cpu[hierarchy.Parent] = cpuPersistenceSnapshot(identity, 1000, 10, 4, 20, "360000 100000", 900)
	cgroups.cpu[hierarchy.Guaranteed] = cpuPersistenceSnapshot(identity, 700, 0, 0, 0, "max 100000", 300)
	cgroups.cpu[hierarchy.BestEffort] = cpuPersistenceSnapshot(identity, 300, 0, 0, 0, "max 100000", 100)
	cgroups.cpu[leaf] = cpuPersistenceSnapshot(identity, 650, 0, 0, 0, "max 100000", 300)
	cgroups.memory[1000] = memoryPersistenceSnapshot(identity, 8<<20, 7, 0, 0, 0)

	weight, _ := cpupoints.NewKernelCPUWeight(300)
	manager.cpuAllocations[1000] = cpuPointsAllocation{
		class: cpupoints.AllocationClassGuaranteed, weight: weight, domainPath: hierarchy.Guaranteed, leafPath: leaf,
		pidNamespaceMismatches: 1,
	}
	manager.appliedGuaranteePoints, _ = cpupoints.NewAppliedGuaranteePoints(300)
	manager.programmedGuaranteePoints = 300
	manager.cpuPointsDegraded = true
	manager.resourceLimits[1000] = userResourceLimitState{ram: true, ramApplied: true, swap: true}
	manager.ramCoverage[1000] = ramCoverageState{coverage: RAMCoveragePartial, partial: map[int]uint64{42: 100}}
	manager.cpuPointsLifecycleEvents[1000] = cpuPointsLifecycleEvent{
		state: resmanmetrics.CPUPointsLifecycleApplied, pidNamespaceMismatches: 1,
	}

	t1 := time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)
	sample1 := persistenceSample(t1)
	manager.collectPersistenceInterval(sample1)
	user1 := sample1.PersistenceUsers[1000]
	if user1.ConfiguredGuaranteePoints == nil || *user1.ConfiguredGuaranteePoints != 300 || user1.ConfiguredClass != "guaranteed" {
		t.Fatalf("configured allocation = %+v", user1)
	}
	if !sample1.PersistenceSystem.CPUPointsDegraded {
		t.Fatal("degraded reconciliation state was not persisted")
	}
	if user1.AppliedClass == nil || *user1.AppliedClass != "guaranteed" || user1.AppliedWeight == nil || *user1.AppliedWeight != 300 {
		t.Fatalf("applied allocation = %+v", user1)
	}
	if user1.LeafCPUUsageUsecDelta != nil || sample1.PersistenceSystem.ParentCPUUsageUsecDelta != nil || user1.MemoryHighEventsDelta != nil {
		t.Fatalf("first baselines must be unavailable: user=%+v system=%+v", user1, sample1.PersistenceSystem)
	}
	if user1.RAMCoverage == nil || *user1.RAMCoverage != "partial" || user1.RAMCoverageIncompleteProcessCount != 1 || user1.RAMCgroupUsageBytes == nil || *user1.RAMCgroupUsageBytes != 8<<20 {
		t.Fatalf("RAM coverage = %+v", user1)
	}
	if user1.PIDNamespaceMismatchCount != 1 || user1.Metrics.ProcessCount != 3 || user1.Metrics.EnforceableUsage.ProcessCount != 2 {
		t.Fatalf("process coverage = %+v", user1)
	}
	manager.clearPersistedCPUPointsLifecycleEvents()

	cgroups.cpu[hierarchy.Parent] = cpuPersistenceSnapshot(identity, 1900, 30, 14, 90, "360000 100000", 900)
	cgroups.cpu[hierarchy.Guaranteed] = cpuPersistenceSnapshot(identity, 1400, 0, 0, 0, "max 100000", 300)
	cgroups.cpu[hierarchy.BestEffort] = cpuPersistenceSnapshot(identity, 500, 0, 0, 0, "max 100000", 100)
	cgroups.cpu[leaf] = cpuPersistenceSnapshot(identity, 1240, 0, 0, 0, "max 100000", 300)
	cgroups.memory[1000] = memoryPersistenceSnapshot(identity, 9<<20, 160, 0, 0, 0)
	t2 := t1.Add(30 * time.Second)
	sample2 := persistenceSample(t2)
	manager.collectPersistenceInterval(sample2)
	user2 := sample2.PersistenceUsers[1000]
	if user2.PIDNamespaceMismatchCount != 1 || sample2.CPUPointsUsers[1000].ProcessCoverage != resmanmetrics.CPUPointsCoveragePartial {
		t.Fatalf("persistent allocation coverage was lost after lifecycle acknowledgement: %+v", sample2.CPUPointsUsers[1000])
	}
	if sample2.PersistenceSystem.IntervalStart == nil || !sample2.PersistenceSystem.IntervalStart.Equal(t1) {
		t.Fatalf("interval start = %v, want %s", sample2.PersistenceSystem.IntervalStart, t1)
	}
	assertUint64Pointer(t, "parent usage", sample2.PersistenceSystem.ParentCPUUsageUsecDelta, 900)
	assertUint64Pointer(t, "guaranteed usage", sample2.PersistenceSystem.GuaranteedDomainCPUUsageUsecDelta, 700)
	assertUint64Pointer(t, "best-effort usage", sample2.PersistenceSystem.BestEffortDomainCPUUsageUsecDelta, 200)
	assertUint64Pointer(t, "parent periods", sample2.PersistenceSystem.ParentCPUPeriodsDelta, 20)
	assertUint64Pointer(t, "parent throttled periods", sample2.PersistenceSystem.ParentCPUThrottledPeriodsDelta, 10)
	assertUint64Pointer(t, "parent throttled usec", sample2.PersistenceSystem.ParentCPUThrottledUsecDelta, 70)
	assertUint64Pointer(t, "leaf usage", user2.LeafCPUUsageUsecDelta, 590)
	assertUint64Pointer(t, "memory high", user2.MemoryHighEventsDelta, 153)
	assertUint64Pointer(t, "memory max", user2.MemoryMaxEventsDelta, 0)
	assertUint64Pointer(t, "memory OOM", user2.MemoryOOMEventsDelta, 0)
	assertUint64Pointer(t, "memory OOM kill", user2.MemoryOOMKillEventsDelta, 0)

	recreated := cgroup.CgroupIdentity{Device: 1, Inode: 99}
	cgroups.cpu[hierarchy.Parent] = cpuPersistenceSnapshot(recreated, 1, 1, 0, 0, "360000 100000", 900)
	cgroups.cpu[leaf] = cpuPersistenceSnapshot(recreated, 1, 0, 0, 0, "max 100000", 300)
	cgroups.memory[1000] = memoryPersistenceSnapshot(recreated, 1, 1, 0, 0, 0)
	sample3 := persistenceSample(t2.Add(30 * time.Second))
	manager.collectPersistenceInterval(sample3)
	if sample3.PersistenceSystem.ParentCPUUsageUsecDelta != nil || sample3.PersistenceUsers[1000].LeafCPUUsageUsecDelta != nil || sample3.PersistenceUsers[1000].MemoryHighEventsDelta != nil {
		t.Fatalf("recreated cgroup produced a synthetic delta: user=%+v system=%+v", sample3.PersistenceUsers[1000], sample3.PersistenceSystem)
	}
}

func TestOperationalCPUPointsSnapshotExistsWithoutDatabaseWriterAndUsesTheDecisionInterval(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{})
	cgroups := &persistenceCgroupManager{cpu: make(map[string]cgroup.CPUPointsNodeSnapshot), memory: make(map[int]cgroup.MemoryAccountingSnapshot)}
	manager := testCPUPointsManagerForReload(t, policy, cgroups)
	manager.metricsCollector = &persistenceMetricsCollector{}
	hierarchy := manager.cpuPointsHierarchy
	identity := cgroup.CgroupIdentity{Device: 1, Inode: 10}
	cgroups.cpu[hierarchy.Parent] = cpuPersistenceSnapshot(identity, 1000, 10, 4, 20, "360000 100000", 900)
	cgroups.cpu[hierarchy.Guaranteed] = cpuPersistenceSnapshot(identity, 700, 0, 0, 0, "max 100000", 1)
	cgroups.cpu[hierarchy.BestEffort] = cpuPersistenceSnapshot(identity, 300, 0, 0, 0, "max 100000", 100)

	t1 := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	manager.collectPersistenceInterval(persistenceSample(t1))
	cgroups.cpu[hierarchy.Parent] = cpuPersistenceSnapshot(identity, 1900, 30, 14, 90, "360000 100000", 900)
	cgroups.cpu[hierarchy.Guaranteed] = cpuPersistenceSnapshot(identity, 1400, 0, 0, 0, "max 100000", 1)
	cgroups.cpu[hierarchy.BestEffort] = cpuPersistenceSnapshot(identity, 500, 0, 0, 0, "max 100000", 100)
	sample := persistenceSample(t1.Add(30 * time.Second))
	manager.collectPersistenceInterval(sample)

	if sample.CPUPointsSystem.IntervalStart == nil || !sample.CPUPointsSystem.IntervalStart.Equal(t1) {
		t.Fatalf("operational interval start = %v, want %s", sample.CPUPointsSystem.IntervalStart, t1)
	}
	assertUint64Pointer(t, "operational parent usage", sample.CPUPointsSystem.ParentCPUUsageUsecDelta, 900)
	if sample.CPUPointsSystem.DeliveryState != resmanmetrics.CPUPointsDeliveryThrottledParent {
		t.Fatalf("delivery state = %q, want throttled_parent", sample.CPUPointsSystem.DeliveryState)
	}
	if sample.CPUPointsSystem.LendingState != resmanmetrics.CPUPointsLendingGuaranteedPriority {
		t.Fatalf("lending state = %q, want guaranteed_priority", sample.CPUPointsSystem.LendingState)
	}
	status := manager.GetStatus()
	if status.CPUPoints.SampleEpochID != sample.Timestamp.UnixNano() || status.CPUPoints.ParentCPUUsageUsecDelta == nil {
		t.Fatalf("runtime status did not publish decision snapshot: %+v", status.CPUPoints)
	}
}

func TestOperationalCPUPointsUserSnapshotNeverCallsPartialOrZeroIngressACompleteGuarantee(t *testing.T) {
	class := "guaranteed"
	weight := uint64(300)
	users := map[int]resmanmetrics.UserPersistenceMetrics{
		1000: {
			Metrics:         &resmanmetrics.UserMetrics{UID: 1000, Username: "alice", ProcessCount: 3, CPULimitRequested: true, EnforceableUsage: resmanmetrics.ProcessSetMetrics{ProcessCount: 2}},
			ConfiguredClass: "guaranteed", LifecycleState: resmanmetrics.CPUPointsLifecycleApplied,
			AppliedClass: &class, AppliedWeight: &weight, PIDNamespaceMismatchCount: 1,
		},
		1001: {
			Metrics:         &resmanmetrics.UserMetrics{UID: 1001, Username: "bob", ProcessCount: 1, CPULimitRequested: true},
			ConfiguredClass: "guaranteed", LifecycleState: resmanmetrics.CPUPointsLifecycleApplied,
			AppliedClass: &class, AppliedWeight: &weight,
		},
		1002: {
			Metrics:         &resmanmetrics.UserMetrics{UID: 1002, Username: "carol", ProcessCount: 1, CPULimitRequested: true, EnforceableUsage: resmanmetrics.ProcessSetMetrics{ProcessCount: 1}},
			ConfiguredClass: "guaranteed", LifecycleState: resmanmetrics.CPUPointsLifecycleFailed,
		},
		1003: {
			Metrics:         &resmanmetrics.UserMetrics{UID: 1003, Username: "dave", ProcessCount: 1, CPULimitRequested: true, EnforceableUsage: resmanmetrics.ProcessSetMetrics{ProcessCount: 1}},
			ConfiguredClass: "guaranteed", LifecycleState: resmanmetrics.CPUPointsLifecycleFailed,
			AppliedClass: &class, AppliedWeight: &weight,
		},
	}
	snapshots := operationalCPUPointsUserSnapshots(users, true)
	partial := snapshots[1000]
	if !partial.AppliedToProcesses || partial.CompleteUIDWorkloadGuaranteed || partial.ProcessCoverage != resmanmetrics.CPUPointsCoveragePartial {
		t.Fatalf("partial snapshot = %+v", partial)
	}
	zero := snapshots[1001]
	if zero.AppliedToProcesses || zero.CompleteUIDWorkloadGuaranteed || zero.ProcessCoverage != resmanmetrics.CPUPointsCoverageNone {
		t.Fatalf("zero-ingress snapshot = %+v", zero)
	}
	refused := snapshots[1002]
	if refused.AppliedToProcesses || refused.CompleteUIDWorkloadGuaranteed || refused.ProcessCoverage != resmanmetrics.CPUPointsCoverageNone {
		t.Fatalf("RAM-active refused activation was reported applied: %+v", refused)
	}
	deferred := snapshots[1003]
	if !deferred.AppliedToProcesses || !deferred.CompleteUIDWorkloadGuaranteed || deferred.ProcessCoverage != resmanmetrics.CPUPointsCoverageComplete {
		t.Fatalf("deferred release lost the still-applied CPU allocation: %+v", deferred)
	}
	if !partial.ReconciliationDegraded || !zero.ReconciliationDegraded {
		t.Fatal("degraded reconciliation state was not propagated to users")
	}
}

func TestCgroupCounterDeltaRejectsIdentityChangeAndCounterResetIndependently(t *testing.T) {
	identity := cgroup.CgroupIdentity{Device: 1, Inode: 10}
	otherIdentity := cgroup.CgroupIdentity{Device: 1, Inode: 11}
	for _, tt := range []struct {
		name             string
		currentIdentity  cgroup.CgroupIdentity
		current          uint64
		previousIdentity cgroup.CgroupIdentity
		previous         uint64
	}{
		{
			name:            "recreated identity with a higher counter",
			currentIdentity: otherIdentity, current: 200,
			previousIdentity: identity, previous: 100,
		},
		{
			name:            "stable identity with a decreasing counter",
			currentIdentity: identity, current: 50,
			previousIdentity: identity, previous: 100,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if delta := cgroupCounterDelta(tt.currentIdentity, tt.current, tt.previousIdentity, tt.previous, true); delta != nil {
				t.Fatalf("cgroupCounterDelta() = %d, want unavailable", *delta)
			}
		})
	}
}

func TestPersistenceTopologyDistinguishesUnavailableCapacityFromVerifiedHistory(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{})
	cpus, err := cpupoints.NewOnlineCPUCount(4)
	if err != nil {
		t.Fatal(err)
	}
	lastVerified, err := cpupoints.PlanParentQuota(cpus, policy.Pool())
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name           string
		state          cpupoints.CapacityState
		wantOnline     bool
		wantProgrammed bool
	}{
		{
			name:           "unavailable capacity retains only the last verified plan",
			state:          cpupoints.CapacityState{Available: false, LastVerified: lastVerified},
			wantProgrammed: true,
		},
		{
			name:  "unavailable capacity without a verified plan remains nullable",
			state: cpupoints.CapacityState{Available: false},
		},
		{
			name:       "available capacity publishes denominator and plan",
			state:      cpupoints.CapacityState{Available: true, LastVerified: lastVerified},
			wantOnline: true, wantProgrammed: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cgroups := &persistenceCgroupManager{cpu: make(map[string]cgroup.CPUPointsNodeSnapshot), memory: make(map[int]cgroup.MemoryAccountingSnapshot)}
			manager := testCPUPointsManagerForReload(t, policy, cgroups)
			manager.metricsCollector = &persistenceMetricsCollector{writer: resmanmetrics.NewDBWriter(nil, 0)}
			manager.cpuCapacity = &mutableCPUCapacityProvider{state: tt.state}
			identity := cgroup.CgroupIdentity{Device: 7, Inode: 70}
			for _, path := range []string{manager.cpuPointsHierarchy.Parent, manager.cpuPointsHierarchy.Guaranteed, manager.cpuPointsHierarchy.BestEffort} {
				cgroups.cpu[path] = cpuPersistenceSnapshot(identity, 1, 1, 0, 0, "max 100000", 100)
			}

			sample := persistenceSample(time.Now().UTC())
			manager.collectPersistenceInterval(sample)
			got := sample.PersistenceSystem
			if got.CPUCapacityAvailable != tt.state.Available {
				t.Fatalf("CPUCapacityAvailable = %t, want %t", got.CPUCapacityAvailable, tt.state.Available)
			}
			if (got.OnlineCPUs != nil) != tt.wantOnline {
				t.Fatalf("OnlineCPUs = %v, want present=%t", got.OnlineCPUs, tt.wantOnline)
			}
			if (got.ProgrammedParentQuotaUsec != nil) != tt.wantProgrammed || (got.ProgrammedParentPeriodUsec != nil) != tt.wantProgrammed {
				t.Fatalf("programmed plan = quota %v period %v, want present=%t", got.ProgrammedParentQuotaUsec, got.ProgrammedParentPeriodUsec, tt.wantProgrammed)
			}
		})
	}
}

func TestPersistenceLifecycleDerivesCPUIneligibleFromTheDecisionSample(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{})
	cgroups := &persistenceCgroupManager{cpu: make(map[string]cgroup.CPUPointsNodeSnapshot), memory: make(map[int]cgroup.MemoryAccountingSnapshot)}
	manager := testCPUPointsManagerForReload(t, policy, cgroups)
	manager.metricsCollector = &persistenceMetricsCollector{writer: resmanmetrics.NewDBWriter(nil, 0)}
	identity := cgroup.CgroupIdentity{Device: 8, Inode: 80}
	for _, path := range []string{manager.cpuPointsHierarchy.Parent, manager.cpuPointsHierarchy.Guaranteed, manager.cpuPointsHierarchy.BestEffort} {
		cgroups.cpu[path] = cpuPersistenceSnapshot(identity, 1, 1, 0, 0, "max 100000", 100)
	}

	sample := persistenceSample(time.Now().UTC())
	sample.UserMetrics[1000].EligibleForCPU = false
	manager.collectPersistenceInterval(sample)
	if got := sample.PersistenceUsers[1000].LifecycleState; got != resmanmetrics.CPUPointsLifecycleIneligible {
		t.Fatalf("LifecycleState = %q, want %q", got, resmanmetrics.CPUPointsLifecycleIneligible)
	}
}

func TestRejectedActiveClassReloadKeepsCandidateOutOfPersistedState(t *testing.T) {
	oldPolicy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	candidate := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{})
	cgroups := &persistenceCgroupManager{cpu: make(map[string]cgroup.CPUPointsNodeSnapshot), memory: make(map[int]cgroup.MemoryAccountingSnapshot)}
	manager := testCPUPointsManagerForReload(t, oldPolicy, cgroups)
	dbManager, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbManager.Close() }()
	manager.metricsCollector = &persistenceMetricsCollector{writer: resmanmetrics.NewDBWriter(dbManager, 0)}

	hierarchy := manager.cpuPointsHierarchy
	leaf := hierarchy.Guaranteed + "/user_1000"
	identity := cgroup.CgroupIdentity{Device: 3, Inode: 30}
	for _, path := range []string{hierarchy.Parent, hierarchy.Guaranteed, hierarchy.BestEffort, leaf} {
		cgroups.cpu[path] = cpuPersistenceSnapshot(identity, 1, 1, 0, 0, "max 100000", 300)
	}
	weight, _ := cpupoints.NewKernelCPUWeight(300)
	manager.cpuAllocations[1000] = cpuPointsAllocation{class: cpupoints.AllocationClassGuaranteed, weight: weight, domainPath: hierarchy.Guaranteed, leafPath: leaf}
	manager.appliedGuaranteePoints, _ = cpupoints.NewAppliedGuaranteePoints(300)
	manager.programmedGuaranteePoints = 300
	if err := manager.ReconcileCPUPointsPolicy(candidate, oldPolicy); err == nil {
		t.Fatal("ReconcileCPUPointsPolicy() accepted an active class change")
	}

	sample := persistenceSample(time.Now().UTC())
	manager.collectPersistenceInterval(sample)
	user := sample.PersistenceUsers[1000]
	if user.ConfiguredClass != "guaranteed" || user.ConfiguredGuaranteePoints == nil || *user.ConfiguredGuaranteePoints != 300 ||
		user.AppliedClass == nil || *user.AppliedClass != "guaranteed" {
		t.Fatalf("rejected candidate leaked into persisted state: %+v", user)
	}
}

func TestPersistenceLifecycleKeepsDeferredReleaseSeparateFromAppliedStateAndNeverFabricatesDisappearedRows(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	cgroups := &persistenceCgroupManager{cpu: make(map[string]cgroup.CPUPointsNodeSnapshot), memory: make(map[int]cgroup.MemoryAccountingSnapshot)}
	manager := testCPUPointsManagerForReload(t, policy, cgroups)
	dbManager, err := database.NewDatabaseManager(privateMetricsDatabasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbManager.Close() }()
	manager.metricsCollector = &persistenceMetricsCollector{writer: resmanmetrics.NewDBWriter(dbManager, 0)}

	hierarchy := manager.cpuPointsHierarchy
	leaf := hierarchy.Guaranteed + "/user_1000"
	identity := cgroup.CgroupIdentity{Device: 2, Inode: 20}
	for _, path := range []string{hierarchy.Parent, hierarchy.Guaranteed, hierarchy.BestEffort, leaf} {
		cgroups.cpu[path] = cpuPersistenceSnapshot(identity, 1, 1, 0, 0, "max 100000", 100)
	}
	weight, _ := cpupoints.NewKernelCPUWeight(300)
	manager.cpuAllocations[1000] = cpuPointsAllocation{class: cpupoints.AllocationClassGuaranteed, weight: weight, domainPath: hierarchy.Guaranteed, leafPath: leaf}
	manager.appliedGuaranteePoints, _ = cpupoints.NewAppliedGuaranteePoints(300)
	manager.programmedGuaranteePoints = 300
	manager.resourceLimits[1000] = userResourceLimitState{ramApplied: true}
	cgroups.memory[1000] = memoryPersistenceSnapshot(identity, 8<<20, 0, 0, 0, 0)
	if released, err := manager.releaseCPUPointsUser(1000, false); released || err == nil {
		t.Fatalf("RAM-active release = %t, %v; want deferred failure", released, err)
	}
	sample := persistenceSample(time.Now().UTC())
	manager.collectPersistenceInterval(sample)
	user := sample.PersistenceUsers[1000]
	if user.LifecycleState != resmanmetrics.CPUPointsLifecycleFailed || user.AppliedClass == nil {
		t.Fatalf("deferred release lost applied allocation: %+v", user)
	}
	operational := sample.CPUPointsUsers[1000]
	if !operational.AppliedToProcesses || operational.ProcessCoverage == resmanmetrics.CPUPointsCoverageNone {
		t.Fatalf("deferred release was not exposed as still CPU-applied: %+v", operational)
	}

	manager.resourceLimits[1000] = userResourceLimitState{}
	if released, err := manager.releaseCPUPointsUser(1000, false); !released || err != nil {
		t.Fatalf("release retry = %t, %v; want success", released, err)
	}
	retried := persistenceSample(sample.Timestamp.Add(time.Second))
	manager.collectPersistenceInterval(retried)
	releasedUser := retried.PersistenceUsers[1000]
	if releasedUser.LifecycleState != resmanmetrics.CPUPointsLifecycleReleased || releasedUser.AppliedClass != nil {
		t.Fatalf("successful retry did not round-trip as released: %+v", releasedUser)
	}

	manager.recordCPUPointsReleaseOutcome(2000, true, nil)
	disappeared := &SystemMetrics{Timestamp: sample.Timestamp.Add(2 * time.Second), UserMetrics: map[int]*resmanmetrics.UserMetrics{}}
	manager.collectPersistenceInterval(disappeared)
	if _, exists := disappeared.PersistenceUsers[2000]; exists {
		t.Fatal("release for an unobserved UID fabricated a history row")
	}
}

func TestOperationalCPUPointsLendingStateDistinguishesEntitlementFromBorrowing(t *testing.T) {
	parent, guaranteed, bestEffort := uint64(1000), uint64(900), uint64(100)
	base := resmanmetrics.SystemPersistenceMetrics{
		CPUCapacityAvailable: true, AppliedGuaranteePoints: 300,
		ProgrammedGuaranteeWeight: 300, ConfiguredBestEffortWeight: 100,
		ParentCPUUsageUsecDelta: &parent,
	}

	withGuaranteedActivity := base
	withGuaranteedActivity.GuaranteedDomainCPUUsageUsecDelta = &guaranteed
	withGuaranteedActivity.BestEffortDomainCPUUsageUsecDelta = &bestEffort
	if got := operationalCPUPointsSystemSnapshot(100, "", withGuaranteedActivity).LendingState; got != resmanmetrics.CPUPointsLendingGuaranteedPriority {
		t.Fatalf("active guaranteed domain lending state = %q", got)
	}

	zero := uint64(0)
	withBorrowing := base
	withBorrowing.GuaranteedDomainCPUUsageUsecDelta = &zero
	withBorrowing.BestEffortDomainCPUUsageUsecDelta = &parent
	if got := operationalCPUPointsSystemSnapshot(100, "", withBorrowing).LendingState; got != resmanmetrics.CPUPointsLendingBestEffortBorrowed {
		t.Fatalf("inactive guaranteed domain lending state = %q", got)
	}

	withoutMappedGuarantees := base
	withoutMappedGuarantees.AppliedGuaranteePoints = 0
	withoutMappedGuarantees.ProgrammedGuaranteeWeight = 0
	withoutMappedGuarantees.GuaranteedDomainCPUUsageUsecDelta = &zero
	withoutMappedGuarantees.BestEffortDomainCPUUsageUsecDelta = &parent
	if got := operationalCPUPointsSystemSnapshot(100, "", withoutMappedGuarantees).LendingState; got != resmanmetrics.CPUPointsLendingBestEffortEntitled {
		t.Fatalf("best-effort-only lending state = %q, want entitlement without borrowing", got)
	}

	atEntitlementBoundary := base
	atEntitlementBoundary.GuaranteedDomainCPUUsageUsecDelta = &zero
	entitledBestEffort := uint64(250)
	atEntitlementBoundary.BestEffortDomainCPUUsageUsecDelta = &entitledBestEffort
	if got := operationalCPUPointsSystemSnapshot(100, "", atEntitlementBoundary).LendingState; got != resmanmetrics.CPUPointsLendingBestEffortEntitled {
		t.Fatalf("best-effort lending state at exact entitlement = %q, want entitlement", got)
	}

	aboveEntitlementBoundary := atEntitlementBoundary
	borrowedBestEffort := uint64(251)
	aboveEntitlementBoundary.BestEffortDomainCPUUsageUsecDelta = &borrowedBestEffort
	if got := operationalCPUPointsSystemSnapshot(100, "", aboveEntitlementBoundary).LendingState; got != resmanmetrics.CPUPointsLendingBestEffortBorrowed {
		t.Fatalf("best-effort lending state above entitlement = %q, want borrowed", got)
	}
}

func TestOperationalCPUPointsStatesRequireCompleteComparableDeltas(t *testing.T) {
	zero, parent, bestEffort := uint64(0), uint64(1000), uint64(900)
	periods := uint64(10)
	base := resmanmetrics.SystemPersistenceMetrics{
		CPUCapacityAvailable: true, AppliedGuaranteePoints: 300,
		ProgrammedGuaranteeWeight: 300, ConfiguredBestEffortWeight: 100,
	}

	if got := operationalCPUPointsSystemSnapshot(100, "", base); got.DeliveryState != resmanmetrics.CPUPointsDeliveryUnavailable || got.LendingState != resmanmetrics.CPUPointsLendingUnavailable {
		t.Fatalf("states without a comparable interval = delivery %q lending %q, want unavailable/unavailable", got.DeliveryState, got.LendingState)
	}

	missingGuaranteed := base
	missingGuaranteed.ParentCPUUsageUsecDelta = &parent
	missingGuaranteed.BestEffortDomainCPUUsageUsecDelta = &bestEffort
	if got := operationalCPUPointsSystemSnapshot(100, "", missingGuaranteed).LendingState; got != resmanmetrics.CPUPointsLendingUnavailable {
		t.Fatalf("lending state without guaranteed-domain delta = %q, want unavailable", got)
	}

	incompleteDelivery := base
	incompleteDelivery.ParentCPUUsageUsecDelta = &parent
	incompleteDelivery.ParentCPUPeriodsDelta = &periods
	incompleteDelivery.ParentCPUThrottledPeriodsDelta = &zero
	if got := operationalCPUPointsSystemSnapshot(100, "", incompleteDelivery).DeliveryState; got != resmanmetrics.CPUPointsDeliveryUnavailable {
		t.Fatalf("delivery state without throttled-usec delta = %q, want unavailable", got)
	}
}

func TestOptionalCPUPointsRAMObservationFailureIsCountedWithoutFabricatingUsage(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{})
	cgroups := &persistenceCgroupManager{cpu: make(map[string]cgroup.CPUPointsNodeSnapshot), memory: make(map[int]cgroup.MemoryAccountingSnapshot)}
	manager := testCPUPointsManagerForReload(t, policy, cgroups)
	hierarchy := manager.cpuPointsHierarchy
	leaf := hierarchy.BestEffort + "/user_1000"
	identity := cgroup.CgroupIdentity{Device: 9, Inode: 90}
	for _, path := range []string{hierarchy.Parent, hierarchy.Guaranteed, hierarchy.BestEffort, leaf} {
		cgroups.cpu[path] = cpuPersistenceSnapshot(identity, 1, 1, 0, 0, "max 100000", 100)
	}
	weight, _ := cpupoints.NewKernelCPUWeight(100)
	manager.cpuAllocations[1000] = cpuPointsAllocation{class: cpupoints.AllocationClassBestEffort, weight: weight, domainPath: hierarchy.BestEffort, leafPath: leaf}

	sample := persistenceSample(time.Now().UTC())
	manager.collectPersistenceInterval(sample)
	if sample.PersistenceUsers[1000].RAMCgroupUsageBytes != nil {
		t.Fatalf("optional failed RAM observation fabricated usage: %v", sample.PersistenceUsers[1000].RAMCgroupUsageBytes)
	}
	exporter := manager.prometheusExporter.(*mockPrometheusExporter)
	if got := exporter.recordedErrors(); !reflect.DeepEqual(got, []prometheusErrorRecord{{
		component: typedEnforcementObservationComponent,
		errorType: metricsDatabaseRAMReadFailure,
	}}) {
		t.Fatalf("optional RAM observation errors = %+v", got)
	}
}

func TestCPUPointsAdmissionLifecyclePreservesBoundedNamespaceCoverage(t *testing.T) {
	manager := &Manager{cpuPointsLifecycleEvents: make(map[int]cpuPointsLifecycleEvent)}
	manager.recordCPUPointsAdmissionOutcome(1000, cgroup.ProcessMoveResult{
		Candidates: 3, Moved: 2, PIDNamespaceMismatches: 1,
	}, nil)
	event, ok := manager.cpuPointsLifecycleEvents[1000]
	if !ok || event.state != resmanmetrics.CPUPointsLifecycleApplied || event.pidNamespaceMismatches != 1 {
		t.Fatalf("partial applied event = %+v, exists=%t", event, ok)
	}

	manager.recordCPUPointsAdmissionOutcome(1001, cgroup.ProcessMoveResult{
		Candidates: 1, PIDNamespaceUnavailable: 1,
	}, fmt.Errorf("no process entered"))
	event = manager.cpuPointsLifecycleEvents[1001]
	if event.state != resmanmetrics.CPUPointsLifecycleNamespaceRejected || event.pidNamespaceUnavailable != 1 {
		t.Fatalf("namespace-rejected event = %+v", event)
	}

	manager.recordCPUPointsAdmissionOutcome(1000, cgroup.ProcessMoveResult{AlreadyPresent: 2}, nil)
	if _, exists := manager.cpuPointsLifecycleEvents[1000]; exists {
		t.Fatal("complete successful admission retained a stale coverage event")
	}
}

func persistenceSample(timestamp time.Time) *SystemMetrics {
	return &SystemMetrics{
		Timestamp: timestamp, TotalCPUUsage: 75, TotalCores: 4, SystemLoad: 2,
		UserMetrics: map[int]*resmanmetrics.UserMetrics{
			1000: {
				UID: 1000, Username: "alice", CPUUsage: 50, MemoryUsage: 96 << 20, ProcessCount: 3,
				EligibleForCPU: true, EligibleForRAM: true, CPULimitRequested: true, CPULimitActive: true,
				RAMLimitRequested: true, RAMLimitActive: true,
				EnforceableUsage: resmanmetrics.ProcessSetMetrics{ProcessCount: 2, MemoryUsage: 64 << 20},
			},
		},
	}
}

func cpuPersistenceSnapshot(identity cgroup.CgroupIdentity, usage, periods, throttled, throttledUsec uint64, quota string, weight uint64) cgroup.CPUPointsNodeSnapshot {
	return cgroup.CPUPointsNodeSnapshot{
		Identity: identity, CPUQuota: quota, CPUWeight: weight,
		CPUStat: cgroup.CPUStatCounters{UsageUsec: usage, NrPeriods: periods, NrThrottled: throttled, ThrottledUsec: throttledUsec},
	}
}

func memoryPersistenceSnapshot(identity cgroup.CgroupIdentity, current, high, max, oom, oomKill uint64) cgroup.MemoryAccountingSnapshot {
	return cgroup.MemoryAccountingSnapshot{
		Path: "/limited/guaranteed/user_1000", Identity: identity, CurrentBytes: current,
		HighLimit: "16M", MaxLimit: "48M", SwapMax: "0",
		Events: cgroup.MemoryEventCounters{High: high, Max: max, OOM: oom, OOMKill: oomKill},
	}
}

func assertUint64Pointer(t *testing.T, name string, got *uint64, want uint64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", name, got, want)
	}
}
