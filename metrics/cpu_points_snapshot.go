package metrics

import "time"

// CPUPointsDeliveryState describes the observed parent-delivery condition.
type CPUPointsDeliveryState string

const (
	CPUPointsDeliveryUnavailable     CPUPointsDeliveryState = "unavailable"
	CPUPointsDeliveryAvailable       CPUPointsDeliveryState = "available"
	CPUPointsDeliveryThrottledParent CPUPointsDeliveryState = "throttled_parent"
)

// CPUPointsDenominatorState describes whether the complete active sibling set
// and its programmed weights have been reconfirmed for this observation.
type CPUPointsDenominatorState string

const (
	CPUPointsDenominatorUnavailable CPUPointsDenominatorState = "unavailable"
	CPUPointsDenominatorInactive    CPUPointsDenominatorState = "inactive"
	CPUPointsDenominatorComplete    CPUPointsDenominatorState = "complete"
	CPUPointsDenominatorIncomplete  CPUPointsDenominatorState = "incomplete"
)

// CPUPointsProcessCoverage describes whether an applied leaf covers the whole
// observed UID workload or only the host-enforceable subset.
type CPUPointsProcessCoverage string

const (
	CPUPointsCoverageUnavailable CPUPointsProcessCoverage = "unavailable"
	CPUPointsCoverageNone        CPUPointsProcessCoverage = "none"
	CPUPointsCoverageComplete    CPUPointsProcessCoverage = "complete"
	CPUPointsCoveragePartial     CPUPointsProcessCoverage = "partial"
)

// CPUPointsSystemSnapshot is one typed operational view from the authoritative
// control-cycle interval. Points, counters and the online denominator are
// values rather than labels.
type CPUPointsSystemSnapshot struct {
	SampleEpochID                  int64
	IntervalStart                  *time.Time
	IntervalEnd                    time.Time
	ReservePoints                  uint64
	NominalParentPoolPoints        uint64
	ConfiguredBestEffortPoints     uint64
	CapacityAvailable              bool
	CapacityUnavailableReason      string
	OnlineCPUs                     *uint64
	ProgrammedParentQuotaUsec      *uint64
	ProgrammedParentPeriodUsec     *uint64
	ReconciliationDegraded         bool
	AppliedGuaranteePoints         uint64
	ProgrammedGuaranteeWeight      uint64
	ProgrammedSiblingWeightSum     *uint64
	ProgrammedBestEffortWeight     *uint64
	ParentCPUUsageUsecDelta        *uint64
	ObservedSiblingWeightSum       *uint64
	ConfiguredRootPoints           *uint64
	ParentCPUPeriodsDelta          *uint64
	ParentCPUThrottledPeriodsDelta *uint64
	ParentCPUThrottledUsecDelta    *uint64
	DeliveryState                  CPUPointsDeliveryState
	DenominatorState               CPUPointsDenominatorState
	EnforcementMode                string
}

// CPUPointsUserSnapshot separates configured policy, requested enforcement,
// observed leaf state, process coverage and post-ingress RAM accounting.
type CPUPointsUserSnapshot struct {
	ProcessObservationUnavailable     bool
	UID                               int
	Username                          string
	ConfiguredClass                   string
	ConfiguredGuaranteePoints         *uint64
	CPUEnforcementRequested           bool
	LifecycleState                    CPUPointsLifecycleState
	AppliedClass                      *string
	AppliedWeight                     *uint64
	ObservedWeight                    *uint64
	IOCoverage                        *string
	CPUAuthorityCoverage              *string
	AppliedToProcesses                bool
	CompleteUIDWorkloadGuaranteed     bool
	ReconciliationDegraded            bool
	ProcessCoverage                   CPUPointsProcessCoverage
	ObservedProcessCount              int
	EnforceableProcessCount           int
	CgroupPath                        string
	LeafCPUUsageUsecDelta             *uint64
	RAMCgroupUsageBytes               *uint64
	RAMCoverage                       *string
	RAMCoverageIncompleteProcessCount int
	RAMSwapDisabled                   *bool
	MemoryHighLimit                   *string
	MemoryMaxLimit                    *string
	MemorySwapMax                     *string
	MemoryHighEventsDelta             *uint64
	MemoryMaxEventsDelta              *uint64
	MemoryOOMEventsDelta              *uint64
	MemoryOOMKillEventsDelta          *uint64
}
