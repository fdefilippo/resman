package state

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

func TestOperationalCPUPointsUserSnapshotNeverCallsUnavailableObservationApplied(t *testing.T) {
	unavailable := "unavailable"
	weight := uint64(300)
	users := map[int]resmanmetrics.UserPersistenceMetrics{
		1000: {
			Metrics: &resmanmetrics.UserMetrics{
				UID: 1000, ProcessCount: 2,
				EnforceableUsage: resmanmetrics.ProcessSetMetrics{ProcessCount: 2},
			},
			ConfiguredClass:      "guaranteed",
			AppliedClass:         stringPointer("guaranteed"),
			AppliedWeight:        &weight,
			CPUWeight:            &weight,
			CPUAuthorityCoverage: &unavailable,
		},
	}

	snapshot := operationalCPUPointsUserSnapshots(users, false)[1000]
	if snapshot.AppliedToProcesses || snapshot.CompleteUIDWorkloadGuaranteed {
		t.Fatalf("unavailable observation published applied state: %+v", snapshot)
	}
	if snapshot.ProcessCoverage != resmanmetrics.CPUPointsCoverageUnavailable {
		t.Fatalf("process coverage = %q, want unavailable", snapshot.ProcessCoverage)
	}
}

func TestCgroupCounterDeltaRejectsIdentityChangeAndCounterResetIndependently(t *testing.T) {
	first := cgroup.CgroupIdentity{Device: 1, Inode: 10}
	second := cgroup.CgroupIdentity{Device: 1, Inode: 11}
	if got := cgroupCounterDelta(second, 200, first, 100, true); got != nil {
		t.Fatalf("identity change delta = %v, want nil", got)
	}
	if got := cgroupCounterDelta(first, 99, first, 100, true); got != nil {
		t.Fatalf("counter reset delta = %v, want nil", got)
	}
	if got := cgroupCounterDelta(first, 125, first, 100, true); got == nil || *got != 25 {
		t.Fatalf("comparable delta = %v, want 25", got)
	}
}

func TestOperationalCPUPointsDeliveryRequiresCompleteComparableDeltas(t *testing.T) {
	zero, parent, periods := uint64(0), uint64(1000), uint64(10)
	for _, missing := range []string{"usage", "periods", "throttled_periods", "throttled_usec", "none"} {
		t.Run(missing, func(t *testing.T) {
			sample := resmanmetrics.SystemPersistenceMetrics{
				CPUCapacityAvailable:           true,
				ParentCPUUsageUsecDelta:        &parent,
				ParentCPUPeriodsDelta:          &periods,
				ParentCPUThrottledPeriodsDelta: &zero,
				ParentCPUThrottledUsecDelta:    &zero,
			}
			switch missing {
			case "usage":
				sample.ParentCPUUsageUsecDelta = nil
			case "periods":
				sample.ParentCPUPeriodsDelta = nil
			case "throttled_periods":
				sample.ParentCPUThrottledPeriodsDelta = nil
			case "throttled_usec":
				sample.ParentCPUThrottledUsecDelta = nil
			}
			got := operationalCPUPointsSystemSnapshot(100, "", sample).DeliveryState
			want := resmanmetrics.CPUPointsDeliveryUnavailable
			if missing == "none" {
				want = resmanmetrics.CPUPointsDeliveryAvailable
			}
			if got != want {
				t.Fatalf("delivery = %s, want %s", got, want)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

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
		Path: "/user.slice/user-1000.slice", Identity: identity, CurrentBytes: current,
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
