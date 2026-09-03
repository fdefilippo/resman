package metrics

import "time"

// CPUPointsLifecycleState identifies the persisted CPU allocation outcome.
type CPUPointsLifecycleState string

const (
	CPUPointsLifecycleIneligible        CPUPointsLifecycleState = "ineligible"
	CPUPointsLifecycleEligibleInactive  CPUPointsLifecycleState = "eligible_inactive"
	CPUPointsLifecycleApplied           CPUPointsLifecycleState = "applied"
	CPUPointsLifecycleNamespaceRejected CPUPointsLifecycleState = "namespace_rejected"
	CPUPointsLifecycleOwnershipRejected CPUPointsLifecycleState = "ownership_rejected"
	CPUPointsLifecycleRecovery          CPUPointsLifecycleState = "recovery"
	CPUPointsLifecycleStranded          CPUPointsLifecycleState = "stranded"
	CPUPointsLifecycleFailed            CPUPointsLifecycleState = "failed"
	CPUPointsLifecycleReleased          CPUPointsLifecycleState = "released"
)

// UserPersistenceMetrics combines one user observation with typed policy,
// applied allocation, process coverage and RAM accounting state.
type UserPersistenceMetrics struct {
	Metrics                           *UserMetrics
	ConfiguredGuaranteePoints         *uint64
	ConfiguredClass                   string
	LifecycleState                    CPUPointsLifecycleState
	AppliedClass                      *string
	AppliedWeight                     *uint64
	CgroupPath                        string
	CPUQuota                          string
	CPUWeight                         *uint64
	LeafCPUUsageUsecDelta             *uint64
	PIDNamespaceMismatchCount         int
	PIDNamespaceUnavailableCount      int
	SystemdOwnershipRefusedCount      int
	RecoveryProcessCount              int
	RestoreFailedProcessCount         int
	StrandedProcessCount              int
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

// SystemPersistenceMetrics contains one synchronized control-cycle interval.
type SystemPersistenceMetrics struct {
	SampleEpochID                     int64
	IntervalStart                     *time.Time
	IntervalEnd                       time.Time
	TotalCPUUsagePercent              float64
	TotalCores                        int
	SystemLoad                        float64
	CPULimitsActive                   bool
	ResourceLimitsActive              bool
	AnyLimitsActive                   bool
	CPUActivelyLimitedUsersCount      int
	ActivelyLimitedUsersCount         int
	NominalParentPoolPoints           uint64
	CPUCapacityAvailable              bool
	OnlineCPUs                        *uint64
	ProgrammedParentQuotaUsec         *uint64
	ProgrammedParentPeriodUsec        *uint64
	CPUPointsDegraded                 bool
	AppliedGuaranteePoints            uint64
	ProgrammedGuaranteeWeight         uint64
	ConfiguredBestEffortWeight        uint64
	ParentCPUQuota                    *string
	GuaranteedDomainCPUWeight         *uint64
	BestEffortDomainCPUWeight         *uint64
	ParentCPUUsageUsecDelta           *uint64
	GuaranteedDomainCPUUsageUsecDelta *uint64
	BestEffortDomainCPUUsageUsecDelta *uint64
	ParentCPUPeriodsDelta             *uint64
	ParentCPUThrottledPeriodsDelta    *uint64
	ParentCPUThrottledUsecDelta       *uint64
}

// PersistenceBatch is one atomic user/system history transaction.
type PersistenceBatch struct {
	System SystemPersistenceMetrics
	Users  map[int]UserPersistenceMetrics
}
