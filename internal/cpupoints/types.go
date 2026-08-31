// Package cpupoints defines the units and arithmetic used by CPU Points policy.
// Pool points use the current online-CPU count as a moving denominator.
// Guarantee weights divide the effective bandwidth delivered to that parent;
// they are proportional entitlements, not fixed CPU time or physical isolation.
package cpupoints

import (
	"fmt"
	"math"
	"strconv"
)

const (
	// TotalPoints is the normalized capacity of the online host CPUs.
	TotalPoints uint64 = 1000
	// ParentPeriodMicroseconds is the fixed cpu.max period used by CPU Points.
	ParentPeriodMicroseconds uint64 = 100000
	// MinimumParentQuotaMicroseconds is the measured minimum accepted by cgroup v2.
	MinimumParentQuotaMicroseconds uint64 = 1000
	// MinimumKernelCPUWeight is the smallest legal cgroup v2 cpu.weight value.
	MinimumKernelCPUWeight uint64 = 1
	// MaximumKernelCPUWeight is the largest legal cgroup v2 cpu.weight value.
	MaximumKernelCPUWeight uint64 = 10000
)

// ReservePoints is the part of normalized host capacity reserved outside ResMan.
type ReservePoints struct{ value uint16 }

// ParentPoolPoints is the normalized host capacity available to the ResMan CPU pool.
type ParentPoolPoints struct{ value uint16 }

// ConfiguredGuaranteePoints is a configured per-user minimum proportional entitlement.
type ConfiguredGuaranteePoints struct{ value uint16 }

// BestEffortPoints is the aggregate proportional entitlement of the best-effort domain.
type BestEffortPoints struct{ value uint16 }

// AcquiredGuaranteePoints is the sum of guarantees whose user leaves ResMan acquired.
type AcquiredGuaranteePoints struct{ value uint16 }

// AppliedGuaranteePoints is the sum of guarantees confirmed on acquired user leaves.
type AppliedGuaranteePoints struct{ value uint16 }

// OnlineCPUCount is a trustworthy live count of online logical host CPUs.
type OnlineCPUCount struct{ value uint64 }

// KernelCPUWeight is an exact value accepted by the cgroup v2 cpu.weight interface.
type KernelCPUWeight struct{ value uint16 }

// ParentQuota is one atomic conversion from a live CPU denominator and pool points.
// It is a programming plan for cpu.max, not proof that the kernel applied or
// delivered the nominal bandwidth.
type ParentQuota struct {
	poolPoints ParentPoolPoints
	onlineCPUs OnlineCPUCount
	quota      uint64
	period     uint64
}

// NewReservePoints validates a reserve in the range 0 through 990.
func NewReservePoints(value uint64) (ReservePoints, error) {
	if err := validateRange("reserve points", value, 0, 990); err != nil {
		return ReservePoints{}, err
	}
	return ReservePoints{value: uint16(value)}, nil
}

// NewParentPoolPoints validates a parent pool in the range 10 through 1000.
func NewParentPoolPoints(value uint64) (ParentPoolPoints, error) {
	if err := validateRange("parent pool points", value, 10, TotalPoints); err != nil {
		return ParentPoolPoints{}, err
	}
	return ParentPoolPoints{value: uint16(value)}, nil
}

// NewConfiguredGuaranteePoints validates a configured guarantee in the range 1 through 1000.
func NewConfiguredGuaranteePoints(value uint64) (ConfiguredGuaranteePoints, error) {
	if err := validateRange("guaranteed points", value, 1, TotalPoints); err != nil {
		return ConfiguredGuaranteePoints{}, err
	}
	return ConfiguredGuaranteePoints{value: uint16(value)}, nil
}

// NewBestEffortPoints validates an aggregate best-effort entitlement in the range 1 through 1000.
func NewBestEffortPoints(value uint64) (BestEffortPoints, error) {
	if err := validateRange("best-effort points", value, 1, TotalPoints); err != nil {
		return BestEffortPoints{}, err
	}
	return BestEffortPoints{value: uint16(value)}, nil
}

// NewAcquiredGuaranteePoints validates an acquired guarantee sum in the range 0 through 1000.
func NewAcquiredGuaranteePoints(value uint64) (AcquiredGuaranteePoints, error) {
	if err := validateRange("acquired guarantee points", value, 0, TotalPoints); err != nil {
		return AcquiredGuaranteePoints{}, err
	}
	return AcquiredGuaranteePoints{value: uint16(value)}, nil
}

// NewAppliedGuaranteePoints validates an applied guarantee sum in the range 0 through 1000.
func NewAppliedGuaranteePoints(value uint64) (AppliedGuaranteePoints, error) {
	if err := validateRange("applied guarantee points", value, 0, TotalPoints); err != nil {
		return AppliedGuaranteePoints{}, err
	}
	return AppliedGuaranteePoints{value: uint16(value)}, nil
}

// NewOnlineCPUCount rejects an unavailable zero denominator.
func NewOnlineCPUCount(value uint64) (OnlineCPUCount, error) {
	if value == 0 {
		return OnlineCPUCount{}, fmt.Errorf("online CPU count must be positive")
	}
	return OnlineCPUCount{value: value}, nil
}

// NewKernelCPUWeight validates an exact cgroup v2 cpu.weight value without clamping.
func NewKernelCPUWeight(value int) (KernelCPUWeight, error) {
	if value < int(MinimumKernelCPUWeight) || value > int(MaximumKernelCPUWeight) {
		return KernelCPUWeight{}, fmt.Errorf("kernel CPU weight %d is outside the supported range %d..%d", value, MinimumKernelCPUWeight, MaximumKernelCPUWeight)
	}
	return KernelCPUWeight{value: uint16(value)}, nil
}

// ParentPool derives the capacity left after the configured reserve.
func (p ReservePoints) ParentPool() ParentPoolPoints {
	return ParentPoolPoints{value: uint16(TotalPoints) - p.value}
}

// Value returns the normalized reserve value.
func (p ReservePoints) Value() uint64 { return uint64(p.value) }

// Value returns the normalized parent pool value.
func (p ParentPoolPoints) Value() uint64 { return uint64(p.value) }

// Value returns the configured guarantee value.
func (p ConfiguredGuaranteePoints) Value() uint64 { return uint64(p.value) }

// Value returns the aggregate best-effort entitlement.
func (p BestEffortPoints) Value() uint64 { return uint64(p.value) }

// Value returns the acquired guarantee sum.
func (p AcquiredGuaranteePoints) Value() uint64 { return uint64(p.value) }

// Value returns the applied guarantee sum.
func (p AppliedGuaranteePoints) Value() uint64 { return uint64(p.value) }

// Value returns the number of online logical CPUs.
func (c OnlineCPUCount) Value() uint64 { return c.value }

// Value returns the exact kernel cpu.weight value.
func (w KernelCPUWeight) Value() int { return int(w.value) }

// KernelWeight converts configured guarantee points into the same exact numeric
// weight. The identity conversion preserves all ratios and never creates a
// second public unit or a capacity quota.
func (p ConfiguredGuaranteePoints) KernelWeight() (KernelCPUWeight, error) {
	return NewKernelCPUWeight(int(p.value))
}

// KernelWeight converts the aggregate best-effort entitlement into an exact proportional weight.
func (p BestEffortPoints) KernelWeight() (KernelCPUWeight, error) {
	return NewKernelCPUWeight(int(p.value))
}

// KernelWeight converts a non-zero acquired guarantee sum into an exact proportional weight.
func (p AcquiredGuaranteePoints) KernelWeight() (KernelCPUWeight, error) {
	if p.value == 0 {
		return KernelCPUWeight{}, fmt.Errorf("zero acquired guarantee points do not define a participating domain weight")
	}
	return NewKernelCPUWeight(int(p.value))
}

// KernelWeight converts a non-zero applied guarantee sum into an exact proportional weight.
func (p AppliedGuaranteePoints) KernelWeight() (KernelCPUWeight, error) {
	if p.value == 0 {
		return KernelCPUWeight{}, fmt.Errorf("zero applied guarantee points do not define a participating domain weight")
	}
	return NewKernelCPUWeight(int(p.value))
}

// PlanParentQuota converts live capacity and pool points to the exact parent cpu.max value.
func PlanParentQuota(onlineCPUs OnlineCPUCount, pool ParentPoolPoints) (ParentQuota, error) {
	if onlineCPUs.value > math.MaxUint64/ParentPeriodMicroseconds {
		return ParentQuota{}, fmt.Errorf("parent CPU quota overflows for %d online CPUs and period %d", onlineCPUs.value, ParentPeriodMicroseconds)
	}

	capacity := onlineCPUs.value * ParentPeriodMicroseconds
	quota := floorScaledQuota(capacity, uint64(pool.value))
	if quota < MinimumParentQuotaMicroseconds {
		return ParentQuota{}, fmt.Errorf("parent CPU quota %d is below the kernel minimum %d", quota, MinimumParentQuotaMicroseconds)
	}

	return ParentQuota{
		poolPoints: pool,
		onlineCPUs: onlineCPUs,
		quota:      quota,
		period:     ParentPeriodMicroseconds,
	}, nil
}

func floorScaledQuota(capacity, points uint64) uint64 {
	// Dividing before the final multiplication keeps every accepted point value
	// overflow-safe while preserving floor(capacity*points/1000) exactly.
	return (capacity/TotalPoints)*points + ((capacity%TotalPoints)*points)/TotalPoints
}

// PoolPoints returns the pool value used by this exact conversion.
func (q ParentQuota) PoolPoints() ParentPoolPoints { return q.poolPoints }

// OnlineCPUs returns the denominator used by this exact conversion.
func (q ParentQuota) OnlineCPUs() OnlineCPUCount { return q.onlineCPUs }

// QuotaMicroseconds returns the programmed quota field for cpu.max.
func (q ParentQuota) QuotaMicroseconds() uint64 { return q.quota }

// PeriodMicroseconds returns the programmed period field for cpu.max.
func (q ParentQuota) PeriodMicroseconds() uint64 { return q.period }

// CPUmax returns the exact cgroup v2 cpu.max representation.
func (q ParentQuota) CPUmax() string {
	return strconv.FormatUint(q.quota, 10) + " " + strconv.FormatUint(q.period, 10)
}

func validateRange(name string, value, minimum, maximum uint64) error {
	if value < minimum || value > maximum {
		return fmt.Errorf("%s %d is outside the supported range %d..%d", name, value, minimum, maximum)
	}
	return nil
}
