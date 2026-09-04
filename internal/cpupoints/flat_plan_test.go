package cpupoints

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"testing"
)

func TestPlanFlatTopologyBuildsExactCompleteImmutablePlan(t *testing.T) {
	policy := flatTestPolicy(t, 100, 100, 100, map[int]uint64{
		1001: 300,
		1002: 200,
	})
	active := []ActiveUserSlice{
		{UID: 1004, Eligible: false},
		{UID: 1002, Eligible: false},
		{UID: 0, Eligible: false},
		{UID: 1003, Eligible: true},
		{UID: 1001, Eligible: true},
	}
	capacity, err := NewOnlineCPUCount(4)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := PlanFlatTopology(policy, capacity, active)
	if err != nil {
		t.Fatalf("PlanFlatTopology(): %v", err)
	}
	if got := plan.ParentQuota().CPUmax(); got != "360000 100000" {
		t.Fatalf("parent cpu.max = %q, want 360000 100000", got)
	}
	if got := plan.Scale(); got != 33 {
		t.Fatalf("scale = %d, want 33", got)
	}
	if got := plan.BestEffortAggregateWeight().Value(); got != 3300 {
		t.Fatalf("aggregate best-effort weight = %d, want 3300", got)
	}

	want := []struct {
		uid       int
		eligible  bool
		mapped    bool
		class     FlatAllocationClass
		guarantee uint64
		weight    int
	}{
		{uid: 0, class: FlatAllocationRoot, weight: 3300},
		{uid: 1001, eligible: true, mapped: true, class: FlatAllocationGuaranteed, guarantee: 300, weight: 9900},
		{uid: 1002, mapped: true, class: FlatAllocationBestEffort, guarantee: 200, weight: 1100},
		{uid: 1003, eligible: true, class: FlatAllocationBestEffort, weight: 1100},
		{uid: 1004, class: FlatAllocationBestEffort, weight: 1100},
	}
	got := plan.Slices()
	if len(got) != len(want) {
		t.Fatalf("slices = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index].UID() != want[index].uid || got[index].Eligible() != want[index].eligible ||
			got[index].Mapped() != want[index].mapped || got[index].Class() != want[index].class ||
			got[index].ConfiguredGuaranteePoints() != want[index].guarantee || got[index].Weight().Value() != want[index].weight {
			t.Errorf("slice %d = uid=%d eligible=%t mapped=%t class=%s guarantee=%d weight=%d, want %+v",
				index, got[index].UID(), got[index].Eligible(), got[index].Mapped(), got[index].Class(),
				got[index].ConfiguredGuaranteePoints(), got[index].Weight().Value(), want[index])
		}
	}

	got[0] = FlatSlicePlan{}
	active[0] = ActiveUserSlice{UID: 9999, Eligible: true}
	preserved := plan.Slices()
	if preserved[0].UID() != 0 || preserved[len(preserved)-1].UID() != 1004 {
		t.Fatalf("caller mutation changed immutable plan: %+v", preserved)
	}
}

func TestPlanFlatTopologyPartitionsBestEffortExactlyAndEvenly(t *testing.T) {
	policy := flatTestPolicy(t, 100, 100, 101, map[int]uint64{1001: 300})
	capacity, _ := NewOnlineCPUCount(2)
	plan, err := PlanFlatTopology(policy, capacity, []ActiveUserSlice{
		{UID: 1005}, {UID: 1002}, {UID: 1004}, {UID: 1003},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Scale() != 33 || plan.BestEffortAggregateWeight().Value() != 3333 {
		t.Fatalf("scale/aggregate = %d/%d, want 33/3333", plan.Scale(), plan.BestEffortAggregateWeight().Value())
	}
	wantWeights := []int{834, 833, 833, 833}
	var total int
	for index, slice := range plan.Slices() {
		if slice.UID() != 1002+index || slice.Weight().Value() != wantWeights[index] {
			t.Errorf("slice %d = uid %d weight %d, want uid %d weight %d", index, slice.UID(), slice.Weight().Value(), 1002+index, wantWeights[index])
		}
		total += slice.Weight().Value()
	}
	if total != plan.BestEffortAggregateWeight().Value() {
		t.Fatalf("partition total = %d, want aggregate %d", total, plan.BestEffortAggregateWeight().Value())
	}
}

func TestPlanFlatTopologyRejectsOnlyUnrepresentableBestEffortCardinality(t *testing.T) {
	policy := flatTestPolicy(t, 0, 1, 1, map[int]uint64{1001: 998})
	capacity, _ := NewOnlineCPUCount(1)
	active := make([]ActiveUserSlice, 11)
	for index := range active {
		active[index] = ActiveUserSlice{UID: 2000 + index, Eligible: index%2 == 0}
	}

	if plan, err := PlanFlatTopology(policy, capacity, active[:10]); err != nil {
		t.Fatalf("ten best-effort slices should fit aggregate weight ten: %v", err)
	} else {
		for _, slice := range plan.Slices() {
			if slice.Weight().Value() != 1 {
				t.Fatalf("minimum representable best-effort weight = %d, want 1", slice.Weight().Value())
			}
		}
	}

	_, err := PlanFlatTopology(policy, capacity, active)
	var planErr *FlatPlanError
	if !errors.As(err, &planErr) || planErr.Reason != FlatPlanBestEffortCardinality ||
		planErr.ActiveBestEffortSlices != 11 || planErr.AggregateWeight != 10 || planErr.MaximumScale != 10 {
		t.Fatalf("error = %T %+v, want bounded 11-over-10 cardinality rejection", err, planErr)
	}
}

func TestPlanFlatTopologyRejectsInvalidAndDuplicateParticipants(t *testing.T) {
	policy := flatTestPolicy(t, 100, 100, 100, nil)
	capacity, _ := NewOnlineCPUCount(1)
	tests := []struct {
		name   string
		active []ActiveUserSlice
		reason FlatPlanReason
		uid    int
	}{
		{name: "negative UID", active: []ActiveUserSlice{{UID: -1}}, reason: FlatPlanInvalidParticipant, uid: -1},
		{name: "duplicate root", active: []ActiveUserSlice{{UID: 0}, {UID: 0, Eligible: true}}, reason: FlatPlanDuplicateUID, uid: 0},
		{name: "duplicate non-root", active: []ActiveUserSlice{{UID: 1001}, {UID: 1001, Eligible: true}}, reason: FlatPlanDuplicateUID, uid: 1001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PlanFlatTopology(policy, capacity, tt.active)
			var planErr *FlatPlanError
			if !errors.As(err, &planErr) || planErr.Reason != tt.reason || planErr.UID != tt.uid {
				t.Fatalf("error = %T %+v, want %s for UID %d", err, planErr, tt.reason, tt.uid)
			}
		})
	}
}

func TestPlanFlatTopologyReplansCapacityAndActivityWithoutMutatingPriorPlan(t *testing.T) {
	policy := flatTestPolicy(t, 100, 100, 100, map[int]uint64{1001: 300})
	fourCPUs, _ := NewOnlineCPUCount(4)
	twoCPUs, _ := NewOnlineCPUCount(2)
	first, err := PlanFlatTopology(policy, fourCPUs, []ActiveUserSlice{{UID: 0}, {UID: 1001, Eligible: true}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanFlatTopology(policy, twoCPUs, []ActiveUserSlice{{UID: 1002}})
	if err != nil {
		t.Fatal(err)
	}
	if first.ParentQuota().CPUmax() != "360000 100000" || second.ParentQuota().CPUmax() != "180000 100000" {
		t.Fatalf("capacity plans = %q and %q", first.ParentQuota().CPUmax(), second.ParentQuota().CPUmax())
	}
	if got := first.Slices(); len(got) != 2 || got[0].Class() != FlatAllocationRoot || got[1].Class() != FlatAllocationGuaranteed {
		t.Fatalf("first plan changed after replanning: %+v", got)
	}
	if got := second.Slices(); len(got) != 1 || got[0].UID() != 1002 || got[0].Class() != FlatAllocationBestEffort {
		t.Fatalf("second plan = %+v", got)
	}
}

func TestPlanFlatTopologyRejectsUnrepresentableLiveCapacity(t *testing.T) {
	policy := flatTestPolicy(t, 100, 100, 100, nil)
	overflowing, err := NewOnlineCPUCount(math.MaxUint64/ParentPeriodMicroseconds + 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PlanFlatTopology(policy, overflowing, nil)
	var planErr *FlatPlanError
	if !errors.As(err, &planErr) || planErr.Reason != FlatPlanCapacityUnrepresentable || planErr.Cause == nil {
		t.Fatalf("error = %T %+v, want capacity rejection", err, planErr)
	}
}

func TestPlanFlatTopologyProperties(t *testing.T) {
	tests := []struct {
		name       string
		root       uint64
		bestEffort uint64
		guarantees map[int]uint64
		active     []ActiveUserSlice
	}{
		{name: "root inactive lends all of its unused entitlement", root: 100, bestEffort: 100, guarantees: map[int]uint64{1001: 300}, active: []ActiveUserSlice{{UID: 1001, Eligible: true}}},
		{name: "excluded mapped user remains in best effort", root: 175, bestEffort: 73, guarantees: map[int]uint64{1001: 251}, active: []ActiveUserSlice{{UID: 0}, {UID: 1001}, {UID: 1002, Eligible: true}}},
		{name: "unmapped set is complete denominator", root: 1, bestEffort: 1, guarantees: map[int]uint64{1001: 698}, active: []ActiveUserSlice{{UID: 0}, {UID: 1001, Eligible: true}, {UID: 1002}, {UID: 1003}, {UID: 1004}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := flatTestPolicy(t, 100, tt.root, tt.bestEffort, tt.guarantees)
			capacity, _ := NewOnlineCPUCount(3)
			plan, err := PlanFlatTopology(policy, capacity, tt.active)
			if err != nil {
				t.Fatal(err)
			}
			assertFlatPlanProperties(t, policy, tt.active, plan)
		})
	}
}

func FuzzPlanFlatTopologyBestEffortPartition(f *testing.F) {
	for _, seed := range []struct {
		root, bestEffort, guarantee uint16
		count, flags                uint8
	}{
		{100, 100, 300, 3, 0},
		{1, 1, 1, 10, 1},
		{333, 217, 400, 31, 2},
	} {
		f.Add(seed.root, seed.bestEffort, seed.guarantee, seed.count, seed.flags)
	}
	f.Fuzz(func(t *testing.T, rootRaw, bestEffortRaw, guaranteeRaw uint16, countRaw, flags uint8) {
		root := uint64(rootRaw%499 + 1)
		bestEffort := uint64(bestEffortRaw%499 + 1)
		remaining := TotalPoints - root - bestEffort
		if remaining == 0 {
			return
		}
		guarantee := uint64(guaranteeRaw)%remaining + 1
		policy := flatTestPolicy(t, 0, root, bestEffort, map[int]uint64{1001: guarantee})
		count := int(countRaw%64) + 1
		active := make([]ActiveUserSlice, 0, count+2)
		if flags&1 != 0 {
			active = append(active, ActiveUserSlice{UID: 0})
		}
		if flags&2 != 0 {
			active = append(active, ActiveUserSlice{UID: 1001, Eligible: true})
		}
		for index := 0; index < count; index++ {
			active = append(active, ActiveUserSlice{UID: 2000 + index, Eligible: flags&4 != 0})
		}
		capacity, _ := NewOnlineCPUCount(uint64(flags%8) + 1)
		plan, err := PlanFlatTopology(policy, capacity, active)
		maximumScale := MaximumKernelCPUWeight / maxExactEntitlement(policy)
		if uint64(count) > bestEffort*maximumScale {
			var planErr *FlatPlanError
			if !errors.As(err, &planErr) || planErr.Reason != FlatPlanBestEffortCardinality {
				t.Fatalf("error = %T %v, want cardinality rejection", err, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		assertFlatPlanProperties(t, policy, active, plan)
	})
}

func flatTestPolicy(t testing.TB, reserveValue, rootValue, bestEffortValue uint64, guarantees map[int]uint64) PolicySnapshot {
	t.Helper()
	reserve, err := NewReservePoints(reserveValue)
	if err != nil {
		t.Fatal(err)
	}
	root, err := NewRootPoints(rootValue)
	if err != nil {
		t.Fatal(err)
	}
	bestEffort, err := NewBestEffortPoints(bestEffortValue)
	if err != nil {
		t.Fatal(err)
	}
	uids := make([]int, 0, len(guarantees))
	for uid := range guarantees {
		uids = append(uids, uid)
	}
	sort.Ints(uids)
	entries := make([]UserGuarantee, 0, len(uids))
	byUID := make(map[int]UserGuarantee, len(uids))
	var totalValue uint64
	for index, uid := range uids {
		points, pointsErr := NewConfiguredGuaranteePoints(guarantees[uid])
		if pointsErr != nil {
			t.Fatal(pointsErr)
		}
		entry := UserGuarantee{username: fmt.Sprintf("user%d", uid), uid: uid, points: points, line: index + 2}
		entries = append(entries, entry)
		byUID[uid] = entry
		totalValue += points.Value()
	}
	if totalValue+root.Value()+bestEffort.Value() > reserve.ParentPool().Value() {
		t.Fatalf("invalid flat-test policy: guarantees %d + root %d + best effort %d exceed pool %d", totalValue, root.Value(), bestEffort.Value(), reserve.ParentPool().Value())
	}
	total, err := NewConfiguredGuaranteeTotalPoints(totalValue)
	if err != nil {
		t.Fatal(err)
	}
	return PolicySnapshot{
		reserve: reserve, pool: reserve.ParentPool(), root: root, bestEffort: bestEffort,
		configuredTotal: total, entries: entries, guaranteesByUID: byUID,
	}
}

func assertFlatPlanProperties(t testing.TB, policy PolicySnapshot, active []ActiveUserSlice, plan FlatPlan) {
	t.Helper()
	if plan.Scale() == 0 {
		t.Fatal("plan scale is zero")
	}
	if got, want := len(plan.Slices()), len(active); got != want {
		t.Fatalf("plan slices = %d, want every active slice %d", got, want)
	}
	seen := make(map[int]bool, len(active))
	var bestEffortTotal int
	minimumBestEffort, maximumBestEffort := int(MaximumKernelCPUWeight), 0
	for _, slice := range plan.Slices() {
		if seen[slice.UID()] {
			t.Fatalf("duplicate planned UID %d", slice.UID())
		}
		seen[slice.UID()] = true
		weight := slice.Weight().Value()
		if weight < int(MinimumKernelCPUWeight) || weight > int(MaximumKernelCPUWeight) {
			t.Fatalf("UID %d weight %d is outside kernel range", slice.UID(), weight)
		}
		switch slice.Class() {
		case FlatAllocationRoot:
			if slice.UID() != 0 || weight != int(policy.Root().Value()*plan.Scale()) {
				t.Fatalf("root slice = UID %d weight %d", slice.UID(), weight)
			}
		case FlatAllocationGuaranteed:
			guarantee, ok := policy.GuaranteeForUID(slice.UID())
			if !ok || !slice.Eligible() || weight != int(guarantee.Points().Value()*plan.Scale()) {
				t.Fatalf("guaranteed slice UID %d weight %d is not exact", slice.UID(), weight)
			}
		case FlatAllocationBestEffort:
			bestEffortTotal += weight
			minimumBestEffort = min(minimumBestEffort, weight)
			maximumBestEffort = max(maximumBestEffort, weight)
		default:
			t.Fatalf("UID %d has unknown class %q", slice.UID(), slice.Class())
		}
	}
	if bestEffortTotal > 0 {
		if bestEffortTotal != plan.BestEffortAggregateWeight().Value() {
			t.Fatalf("best-effort total = %d, want %d", bestEffortTotal, plan.BestEffortAggregateWeight().Value())
		}
		if maximumBestEffort-minimumBestEffort > 1 {
			t.Fatalf("best-effort weights span %d..%d", minimumBestEffort, maximumBestEffort)
		}
	}
}
