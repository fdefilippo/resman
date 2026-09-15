package metrics

import "time"

// CPUPointsLifecycleState identifies the persisted CPU allocation outcome.
type CPUPointsLifecycleState string

const (
	CPUPointsLifecycleIneligible       CPUPointsLifecycleState = "ineligible"
	CPUPointsLifecycleEligibleInactive CPUPointsLifecycleState = "eligible_inactive"
	CPUPointsLifecycleApplied          CPUPointsLifecycleState = "applied"
	CPUPointsLifecycleFailed           CPUPointsLifecycleState = "failed"
	CPUPointsLifecycleReleased         CPUPointsLifecycleState = "released"
)

// UserPersistenceMetrics combines one user observation with typed policy,
// applied allocation, process coverage and RAM accounting state.
type UserPersistenceMetrics struct {
	ProcessObservationUnavailable     bool
	Metrics                           *UserMetrics
	ConfiguredGuaranteePoints         *uint64
	ConfiguredClass                   string
	LifecycleState                    CPUPointsLifecycleState
	AppliedClass                      *string
	AppliedWeight                     *uint64
	CgroupPath                        string
	CPUQuota                          string
	CPUWeight                         *uint64
	IOCoverage                        *string
	CPUAuthorityCoverage              *string
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

// SystemPersistenceMetrics contains one synchronized control-cycle interval.
type SystemPersistenceMetrics struct {
	IODeviceWeightState                         string
	IODeviceWeightReason                        string
	IODeviceWeightSelector                      string
	IODeviceWeightMechanism                     string
	IODeviceWeightClassificationAttempts        uint64
	IODeviceWeightProbeAttempts                 uint64
	IODeviceWeightProgrammed                    bool
	IODeviceWeightProgrammedState               string
	IODeviceWeightReadBack                      bool
	IODeviceWeightReadBackState                 string
	IODeviceWeightFunctionallyAccepted          bool
	IODeviceWeightEffectQualified               bool
	IODeviceWeightEffectQualificationProvenance string
	IODeviceWeightAuthorityCoverage             string
	IODeviceWeightCompleteUsers                 int
	IODeviceWeightPartialUsers                  int
	IODeviceWeightUnavailableUsers              int
	IODeviceWeightSiblingSlices                 int
	IODeviceWeightTotalPoints                   uint64
	IODeviceWeightRequestedAt                   *time.Time
	IODeviceWeightNextRetryAt                   *time.Time
	IODeviceWeightValuesJSON                    string
	IODeviceWeightObservedDelivery              string
	DenominatorState                            CPUPointsDenominatorState
	EnforcementMode                             string
	SampleEpochID                               int64
	IntervalStart                               *time.Time
	IntervalEnd                                 time.Time
	TotalCPUUsagePercent                        float64
	TotalCores                                  int
	SystemLoad                                  float64
	CPULimitsActive                             bool
	ResourceLimitsActive                        bool
	AnyLimitsActive                             bool
	CPUActivelyLimitedUsersCount                int
	ActivelyLimitedUsersCount                   int
	NominalParentPoolPoints                     uint64
	CPUCapacityAvailable                        bool
	OnlineCPUs                                  *uint64
	ProgrammedParentQuotaUsec                   *uint64
	ProgrammedParentPeriodUsec                  *uint64
	CPUPointsDegraded                           bool
	AppliedGuaranteePoints                      uint64
	ProgrammedGuaranteeWeight                   uint64
	ConfiguredBestEffortPoints                  uint64
	ParentCPUQuota                              *string
	ProgrammedSiblingWeightSum                  *uint64
	ProgrammedBestEffortWeight                  *uint64
	ParentCPUUsageUsecDelta                     *uint64
	ObservedSiblingWeightSum                    *uint64
	ConfiguredRootPoints                        *uint64
	ParentCPUPeriodsDelta                       *uint64
	ParentCPUThrottledPeriodsDelta              *uint64
	ParentCPUThrottledUsecDelta                 *uint64
}

// PersistenceBatch is one atomic user/system history transaction.
type PersistenceBatch struct {
	System SystemPersistenceMetrics
	Users  map[int]UserPersistenceMetrics
}
