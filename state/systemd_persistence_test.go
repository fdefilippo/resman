package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/systemdunit"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type accountingSystemdAdapter struct {
	*fakeSystemdCPUUnitAdapter
	values   map[string]systemdunit.UnitAccounting
	coverage map[uint32]bool
	readHook func(systemdunit.UnitIdentity)
}

func (a *accountingSystemdAdapter) ObserveAccounting(_ context.Context, identity systemdunit.UnitIdentity) (systemdunit.UnitAccounting, error) {
	if a.readHook != nil {
		a.readHook(identity)
	}
	return a.values[identity.Name], nil
}
func (a *accountingSystemdAdapter) ObserveCPUCoverage(context.Context, systemdunit.TopologySnapshot) (map[uint32]bool, error) {
	return a.coverage, nil
}

type forbiddenAccountingCgroupManager struct{ mockCgroupManager }

func (*forbiddenAccountingCgroupManager) GetCPUPointsNodeSnapshot(string) (cgroup.CPUPointsNodeSnapshot, error) {
	panic("systemd accounting entered migration CPU reader")
}
func (*forbiddenAccountingCgroupManager) GetMemoryAccountingSnapshot(int) (cgroup.MemoryAccountingSnapshot, error) {
	panic("systemd accounting entered migration RAM reader")
}

func accountingManager(t *testing.T) (*Manager, *accountingSystemdAdapter) {
	t.Helper()
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {1000, 300}})
	base := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000, 1001)}
	m := testSystemdCPUPointsManager(t, policy, base, &forbiddenAccountingCgroupManager{}, 4)
	m.GetConfig().UserIncludeList = []string{"alice"}
	if err := m.activateSystemdCPUPoints(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatal(err)
	}
	a := &accountingSystemdAdapter{fakeSystemdCPUUnitAdapter: base, values: make(map[string]systemdunit.UnitAccounting), coverage: map[uint32]bool{0: true, 1000: true, 1001: true}}
	for _, identity := range topologyIdentities(base.topology) {
		weight := uint64(3300)
		quota := "max 100000"
		if identity.Name == "user-1000.slice" {
			weight = 9900
		}
		if identity.IsParentUserSlice() {
			quota = "360000 100000"
		}
		cpu := cpuPersistenceSnapshot(cgroup.CgroupIdentity{Device: 1, Inode: identity.ControlGroupID}, 100, 10, 2, 3, quota, weight)
		memory := memoryPersistenceSnapshot(cpu.Identity, 96<<20, 10, 2, 1, 1)
		a.values[identity.Name] = systemdunit.UnitAccounting{Identity: identity, CPU: &cpu, Memory: &memory}
	}
	m.systemdUnits = a
	m.resourceLimits[1000] = userResourceLimitState{ramApplied: true, ioApplied: true, ramAuthority: systemdunit.ResourceAuthority{State: systemdunit.ResourceCoverageComplete}, ioAuthority: systemdunit.ResourceAuthority{State: systemdunit.ResourceCoverageComplete}}
	m.systemdResourceUnits[1000] = base.topology.Users[1].Unit.Identity
	return m, a
}

func TestSystemdAccountingRoundTripUsesSlicesAndFlatDenominator(t *testing.T) {
	m, a := accountingManager(t)
	start := time.Now().UTC()
	m.collectPersistenceInterval(persistenceSample(start))
	for name, value := range a.values {
		cpu := *value.CPU
		cpu.CPUStat.UsageUsec += 100
		cpu.CPUStat.NrPeriods += 10
		value.CPU = &cpu
		memory := *value.Memory
		memory.Events.High += 7
		value.Memory = &memory
		a.values[name] = value
	}
	sample := persistenceSample(start.Add(30 * time.Second))
	m.collectPersistenceInterval(sample)
	if sample.CPUPointsSystem.DenominatorState != resmanmetrics.CPUPointsDenominatorComplete {
		t.Fatalf("denominator: %+v", sample.CPUPointsSystem)
	}
	assertUint64Pointer(t, "root points", sample.CPUPointsSystem.ConfiguredRootPoints, 100)
	assertUint64Pointer(t, "programmed siblings", sample.CPUPointsSystem.ProgrammedSiblingWeightSum, 16500)
	assertUint64Pointer(t, "observed siblings", sample.CPUPointsSystem.ObservedSiblingWeightSum, 16500)
	assertUint64Pointer(t, "best effort", sample.CPUPointsSystem.ProgrammedBestEffortWeight, 3300)
	user := sample.CPUPointsUsers[1000]
	assertUint64Pointer(t, "RAM", user.RAMCgroupUsageBytes, 96<<20)
	assertUint64Pointer(t, "high events", user.MemoryHighEventsDelta, 7)
	if !user.CompleteUIDWorkloadGuaranteed || user.RAMCoverage == nil || *user.RAMCoverage != "complete" || user.IOCoverage == nil || *user.IOCoverage != "complete" {
		t.Fatalf("coverage: %+v", user)
	}
	if sample.CPUPointsUsers[0].ConfiguredClass != "root" || len(sample.PersistenceUsers) != 3 {
		t.Fatalf("root or excluded sibling absent: %+v", sample.PersistenceUsers)
	}
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	writer := resmanmetrics.NewDBWriter(db, 1)
	if err := writer.WriteMetricsBatch(resmanmetrics.PersistenceBatch{System: sample.PersistenceSystem, Users: sample.PersistenceUsers}); err != nil {
		t.Fatal(err)
	}
	systems, err := db.GetSystemHistory(start.Add(-time.Second), start.Add(time.Minute), 10)
	if err != nil || len(systems) != 1 {
		t.Fatalf("history: %v %v", systems, err)
	}
	if systems[0].DenominatorState != "complete" || systems[0].EnforcementMode != "systemd_native" {
		t.Fatalf("stored state: %+v", systems[0])
	}
	assertUint64Pointer(t, "stored root", systems[0].ConfiguredRootPoints, 100)
	users, err := db.GetUserHistory(1000, start.Add(-time.Second), start.Add(time.Minute), 10)
	if err != nil || len(users) != 1 {
		t.Fatalf("user history: %v %v", users, err)
	}
	assertUint64Pointer(t, "stored RAM", users[0].RAMCgroupUsageBytes, 96<<20)
	rootRows, err := db.GetUserHistory(0, start.Add(-time.Second), start.Add(time.Minute), 10)
	if err != nil || len(rootRows) != 1 || !rootRows[0].ProcessObservationUnavailable {
		t.Fatalf("unobserved root process sample: %+v %v", rootRows, err)
	}
	rootSummary, err := db.GetUserSummary(0, start.Add(-time.Second), start.Add(time.Minute))
	if err != nil || rootSummary != nil {
		t.Fatalf("unobserved root manufactured a process summary: %+v %v", rootSummary, err)
	}
	if users[0].CPUAuthorityCoverage == nil || *users[0].CPUAuthorityCoverage != "complete" || users[0].IOCoverage == nil || *users[0].IOCoverage != "complete" {
		t.Fatalf("stored coverage: %+v", users[0])
	}
}

func TestSystemdAccountingDoesNotInventCompleteObservations(t *testing.T) {
	for _, fault := range []string{"missing_cpu", "missing_memory", "external_weight", "recreated_unit", "late_topology", "incomplete_plan", "authority_split", "missing_coverage", "capacity_unavailable"} {
		t.Run(fault, func(t *testing.T) {
			m, a := accountingManager(t)
			start := time.Now()
			m.collectPersistenceInterval(persistenceSample(start))
			value := a.values["user-1000.slice"]
			switch fault {
			case "missing_cpu":
				value.CPU = nil
			case "missing_memory":
				value.Memory = nil
				value.MemoryError = errors.New("unreadable memory")
			case "external_weight":
				cpu := *value.CPU
				cpu.CPUWeight = 1
				value.CPU = &cpu
			case "recreated_unit":
				a.topology.Users[1].Unit.Identity.InvocationID[0]++
			case "late_topology":
				a.confirmError = errors.New("late topology change")
			case "incomplete_plan":
				m.systemdCPUComplete = false
			case "authority_split":
				a.coverage[1000] = false
			case "missing_coverage":
				a.coverage = nil
			case "capacity_unavailable":
				m.cpuCapacity.(*mutableCPUCapacityProvider).state.Available = false
			}
			a.values["user-1000.slice"] = value
			sample := persistenceSample(start.Add(30 * time.Second))
			m.collectPersistenceInterval(sample)
			user := sample.CPUPointsUsers[1000]
			if fault == "missing_memory" {
				if user.RAMCgroupUsageBytes != nil || user.MemoryHighEventsDelta != nil || user.RAMCoverage == nil || *user.RAMCoverage != "unavailable" {
					t.Fatalf("invented RAM: %+v", user)
				}
				return
			}
			if user.CompleteUIDWorkloadGuaranteed {
				t.Fatalf("invented guarantee: %+v", user)
			}
			if fault == "recreated_unit" && (user.RAMCoverage == nil || *user.RAMCoverage != "unavailable" || user.IOCoverage == nil || *user.IOCoverage != "unavailable") {
				t.Fatalf("resource authority inherited from an earlier unit lifetime: %+v", user)
			}
			if fault != "authority_split" && fault != "missing_coverage" && sample.CPUPointsSystem.DenominatorState == resmanmetrics.CPUPointsDenominatorComplete {
				t.Fatal("invented complete denominator")
			}
		})
	}
}

func TestSystemdAccountingResetsOnlyUnavailableOrRecreatedBaselines(t *testing.T) {
	for _, fault := range []string{"missing", "unit_recreated", "cgroup_recreated", "counter_decreased"} {
		t.Run(fault, func(t *testing.T) {
			m, a := accountingManager(t)
			start := time.Now()
			m.collectPersistenceInterval(persistenceSample(start))
			value := a.values["user-1000.slice"]
			cpu := *value.CPU
			cpu.CPUStat.UsageUsec = 300
			value.CPU = &cpu
			switch fault {
			case "missing":
				a.values["user-1000.slice"] = systemdunit.UnitAccounting{}
				m.collectPersistenceInterval(persistenceSample(start.Add(10 * time.Second)))
			case "unit_recreated":
				a.topology.Users[1].Unit.Identity.InvocationID[0]++
			case "cgroup_recreated":
				cpu.Identity.Inode++
			case "counter_decreased":
				cpu.CPUStat.UsageUsec = 1
			}
			a.values["user-1000.slice"] = value
			sample := persistenceSample(start.Add(30 * time.Second))
			m.collectPersistenceInterval(sample)
			if sample.CPUPointsUsers[1000].LeafCPUUsageUsecDelta != nil {
				t.Fatalf("invalid baseline: %+v", sample.CPUPointsUsers[1000])
			}
		})
	}
}

func TestSystemdAccountingPreservesProcessesWithoutUserSlices(t *testing.T) {
	for _, scenario := range []string{"process_only", "slice_departed", "discovery_unavailable", "ineligible"} {
		t.Run(scenario, func(t *testing.T) {
			m, a := accountingManager(t)
			start := time.Now().UTC()
			m.collectPersistenceInterval(persistenceSample(start))
			sample := persistenceSample(start.Add(30 * time.Second))
			uid := 1002
			if scenario == "slice_departed" {
				uid = 1000
				a.topology.Users = append(a.topology.Users[:1], a.topology.Users[2:]...)
			}
			if scenario == "discovery_unavailable" {
				a.discoverError = errors.New("discovery unavailable")
			}
			observed := &resmanmetrics.UserMetrics{UID: uid, Username: "service", CPUUsage: 40, MemoryUsage: 32 << 20, ProcessCount: 3, EligibleForCPU: scenario != "ineligible", CPULimitRequested: true}
			sample.UserMetrics[uid] = observed
			m.collectPersistenceInterval(sample)
			user, exists := sample.PersistenceUsers[uid]
			if !exists || user.Metrics != observed || user.ProcessObservationUnavailable {
				t.Fatalf("observed UID lost: %+v", sample.PersistenceUsers)
			}
			wantLifecycle := resmanmetrics.CPUPointsLifecycleEligibleInactive
			if scenario == "ineligible" {
				wantLifecycle = resmanmetrics.CPUPointsLifecycleIneligible
			}
			if scenario == "discovery_unavailable" {
				wantLifecycle = resmanmetrics.CPUPointsLifecycleFailed
			}
			if user.LifecycleState != wantLifecycle || user.AppliedClass != nil || user.AppliedWeight != nil || user.CPUWeight != nil || user.LeafCPUUsageUsecDelta != nil || user.RAMCgroupUsageBytes != nil || user.MemoryHighLimit != nil || user.MemoryHighEventsDelta != nil || user.CgroupPath != "" || user.CPUQuota != "" {
				t.Fatalf("slice observation invented or retained: %+v", user)
			}
			for _, coverage := range []*string{user.CPUAuthorityCoverage, user.RAMCoverage, user.IOCoverage} {
				if coverage == nil || *coverage != "unavailable" {
					t.Fatalf("invented resource coverage: %+v", user)
				}
			}
			if scenario == "slice_departed" && (user.ConfiguredClass != "guaranteed" || user.ConfiguredGuaranteePoints == nil || *user.ConfiguredGuaranteePoints != 300) {
				t.Fatalf("configuration lost with slice: %+v", user)
			}
			status, exists := m.GetCPUPointsUserStatus(uid)
			if !exists || status.ObservedProcessCount != 3 || !status.CPUEnforcementRequested || status.AppliedToProcesses || status.CompleteUIDWorkloadGuaranteed || status.ProcessCoverage != resmanmetrics.CPUPointsCoverageUnavailable {
				t.Fatalf("typed status disagrees with persistence: %+v", status)
			}
			if scenario == "process_only" {
				if len(sample.PersistenceUsers) != 4 || sample.CPUPointsSystem.DenominatorState != resmanmetrics.CPUPointsDenominatorComplete {
					t.Fatalf("union or denominator changed: %+v", sample.CPUPointsSystem)
				}
				assertUint64Pointer(t, "unchanged sibling weight", sample.CPUPointsSystem.ProgrammedSiblingWeightSum, 16500)
			}
			db, err := database.NewDatabaseManager(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := resmanmetrics.NewDBWriter(db, 0).WriteMetricsBatch(resmanmetrics.PersistenceBatch{System: sample.PersistenceSystem, Users: sample.PersistenceUsers}); err != nil {
				t.Fatal(err)
			}
			rows, err := db.GetUserHistory(uid, start, sample.Timestamp.Add(time.Second), 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("history missing: %+v %v", rows, err)
			}
			row := rows[0]
			if row.ProcessObservationUnavailable || row.CPUUsagePercent != 40 || row.ProcessCount != 3 || row.MemoryUsageBytes != 32<<20 || row.EligibleForCPU != observed.EligibleForCPU || row.CPUWeight != nil || row.AppliedCPUWeight != nil || row.RAMCgroupUsageBytes != nil || row.CPUPointsLifecycleState != string(wantLifecycle) {
				t.Fatalf("history disagrees with observation: %+v", row)
			}
		})
	}
}
