package cpupoints

import (
	"fmt"
	"sort"
)

// FlatPlanReason is a bounded classification for rejected flat-topology plans.
type FlatPlanReason string

const (
	FlatPlanInvalidParticipant      FlatPlanReason = "invalid_participant"
	FlatPlanDuplicateUID            FlatPlanReason = "duplicate_uid"
	FlatPlanBestEffortCardinality   FlatPlanReason = "best_effort_cardinality"
	FlatPlanCapacityUnrepresentable FlatPlanReason = "capacity_unrepresentable"
)

// FlatPlanError reports why no immutable flat-topology plan was produced.
type FlatPlanError struct {
	Reason                 FlatPlanReason
	UID                    int
	ActiveBestEffortSlices uint64
	AggregateWeight        uint64
	MaximumScale           uint64
	Cause                  error
}

func (e *FlatPlanError) Error() string {
	switch e.Reason {
	case FlatPlanInvalidParticipant, FlatPlanDuplicateUID:
		return fmt.Sprintf("flat CPU Points plan rejected (%s) for UID %d", e.Reason, e.UID)
	case FlatPlanBestEffortCardinality:
		return fmt.Sprintf("flat CPU Points plan rejected (%s): %d active best-effort slices exceed aggregate kernel weight %d at maximum exact scale %d", e.Reason, e.ActiveBestEffortSlices, e.AggregateWeight, e.MaximumScale)
	default:
		if e.Cause != nil {
			return fmt.Sprintf("flat CPU Points plan rejected (%s): %v", e.Reason, e.Cause)
		}
		return fmt.Sprintf("flat CPU Points plan rejected (%s)", e.Reason)
	}
}

// Unwrap exposes an underlying capacity conversion failure.
func (e *FlatPlanError) Unwrap() error { return e.Cause }

// ActiveUserSlice is one authoritative active user slice. Eligible controls
// individual CPU policy; every value still participates in scheduling.
type ActiveUserSlice struct {
	UID      int
	Eligible bool
}

// FlatAllocationClass identifies a scheduling role in the flat user.slice hierarchy.
type FlatAllocationClass string

const (
	FlatAllocationRoot       FlatAllocationClass = "root"
	FlatAllocationGuaranteed FlatAllocationClass = "guaranteed"
	FlatAllocationBestEffort FlatAllocationClass = "best_effort"
)

// FlatSlicePlan is one immutable user-slice weight decision.
type FlatSlicePlan struct {
	uid                 int
	eligible            bool
	mapped              bool
	class               FlatAllocationClass
	configuredGuarantee uint64
	weight              KernelCPUWeight
}

// UID returns the numeric user identity represented by this slice.
func (p FlatSlicePlan) UID() int { return p.uid }

// Eligible reports whether the user participates in individual CPU policy decisions.
func (p FlatSlicePlan) Eligible() bool { return p.eligible }

// Mapped reports whether the policy map contains this UID, independently of eligibility.
func (p FlatSlicePlan) Mapped() bool { return p.mapped }

// Class returns the scheduling role selected for this active slice.
func (p FlatSlicePlan) Class() FlatAllocationClass { return p.class }

// ConfiguredGuaranteePoints returns the mapped guarantee, or zero when none applies.
func (p FlatSlicePlan) ConfiguredGuaranteePoints() uint64 { return p.configuredGuarantee }

// Weight returns the exact kernel weight selected for this slice.
func (p FlatSlicePlan) Weight() KernelCPUWeight { return p.weight }

// FlatPlan is one immutable parent quota and complete active-slice denominator.
type FlatPlan struct {
	parentQuota               ParentQuota
	scale                     uint64
	bestEffortAggregateWeight KernelCPUWeight
	slices                    []FlatSlicePlan
}

// ParentQuota returns the quota derived from the same live online-CPU denominator.
func (p FlatPlan) ParentQuota() ParentQuota { return p.parentQuota }

// Scale returns the common integer multiplier used for every exact entitlement.
func (p FlatPlan) Scale() uint64 { return p.scale }

// BestEffortAggregateWeight returns the exact aggregate weight partitioned among best-effort leaves.
func (p FlatPlan) BestEffortAggregateWeight() KernelCPUWeight { return p.bestEffortAggregateWeight }

// Slices returns a defensive UID-sorted copy of the complete active denominator.
func (p FlatPlan) Slices() []FlatSlicePlan { return append([]FlatSlicePlan(nil), p.slices...) }

// PlanFlatTopology constructs a complete mutation-free plan from one policy,
// one live online-CPU denominator and one authoritative active-slice set.
func PlanFlatTopology(policy PolicySnapshot, onlineCPUs OnlineCPUCount, active []ActiveUserSlice) (FlatPlan, error) {
	quota, err := PlanParentQuota(onlineCPUs, policy.Pool())
	if err != nil {
		return FlatPlan{}, &FlatPlanError{Reason: FlatPlanCapacityUnrepresentable, Cause: err}
	}

	participants := append([]ActiveUserSlice(nil), active...)
	sort.Slice(participants, func(left, right int) bool { return participants[left].UID < participants[right].UID })
	seen := make(map[int]bool, len(participants))
	for _, participant := range participants {
		if participant.UID < 0 {
			return FlatPlan{}, &FlatPlanError{Reason: FlatPlanInvalidParticipant, UID: participant.UID}
		}
		if seen[participant.UID] {
			return FlatPlan{}, &FlatPlanError{Reason: FlatPlanDuplicateUID, UID: participant.UID}
		}
		seen[participant.UID] = true
	}

	maximumPoints := maxExactEntitlement(policy)
	// This is the largest common integer scale that keeps every exact public
	// entitlement inside cpu.weight. If its aggregate best-effort weight cannot
	// give every best-effort leaf weight 1, no smaller exact scale can do so.
	maximumScale := MaximumKernelCPUWeight / maximumPoints
	aggregateBestEffortWeight := policy.BestEffort().Value() * maximumScale
	bestEffortCount := uint64(0)
	for _, participant := range participants {
		if participant.UID == 0 {
			continue
		}
		_, mapped := policy.GuaranteeForUID(participant.UID)
		if !mapped || !participant.Eligible {
			bestEffortCount++
		}
	}
	if bestEffortCount > aggregateBestEffortWeight {
		return FlatPlan{}, &FlatPlanError{
			Reason: FlatPlanBestEffortCardinality, ActiveBestEffortSlices: bestEffortCount,
			AggregateWeight: aggregateBestEffortWeight, MaximumScale: maximumScale,
		}
	}

	aggregateWeight, err := NewKernelCPUWeight(int(aggregateBestEffortWeight))
	if err != nil {
		return FlatPlan{}, &FlatPlanError{Reason: FlatPlanCapacityUnrepresentable, Cause: err}
	}
	plan := FlatPlan{parentQuota: quota, scale: maximumScale, bestEffortAggregateWeight: aggregateWeight}
	baseBestEffortWeight, remainder := uint64(0), uint64(0)
	if bestEffortCount > 0 {
		baseBestEffortWeight = aggregateBestEffortWeight / bestEffortCount
		remainder = aggregateBestEffortWeight % bestEffortCount
	}
	bestEffortIndex := uint64(0)
	for _, participant := range participants {
		entry := FlatSlicePlan{uid: participant.UID, eligible: participant.Eligible}
		guarantee, mapped := policy.GuaranteeForUID(participant.UID)
		entry.mapped = mapped
		if mapped {
			entry.configuredGuarantee = guarantee.Points().Value()
		}
		var weight uint64
		switch {
		case participant.UID == 0:
			entry.class = FlatAllocationRoot
			weight = policy.Root().Value() * maximumScale
		case mapped && participant.Eligible:
			entry.class = FlatAllocationGuaranteed
			weight = entry.configuredGuarantee * maximumScale
		default:
			entry.class = FlatAllocationBestEffort
			weight = baseBestEffortWeight
			// Assign the indivisible remainder in stable UID order so the same
			// topology always produces the same complete plan.
			if bestEffortIndex < remainder {
				weight++
			}
			bestEffortIndex++
		}
		entry.weight, err = NewKernelCPUWeight(int(weight))
		if err != nil {
			return FlatPlan{}, &FlatPlanError{Reason: FlatPlanCapacityUnrepresentable, UID: participant.UID, Cause: err}
		}
		plan.slices = append(plan.slices, entry)
	}
	return plan, nil
}

func maxExactEntitlement(policy PolicySnapshot) uint64 {
	maximum := policy.Root().Value()
	if policy.BestEffort().Value() > maximum {
		maximum = policy.BestEffort().Value()
	}
	for _, guarantee := range policy.Entries() {
		if guarantee.Points().Value() > maximum {
			maximum = guarantee.Points().Value()
		}
	}
	return maximum
}
