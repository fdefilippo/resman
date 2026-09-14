package state

import (
	"context"
	"fmt"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/systemdunit"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type systemdAccountingReader interface {
	ObserveAccounting(context.Context, systemdunit.UnitIdentity) (systemdunit.UnitAccounting, error)
}

// collectSystemdPersistenceInterval owns one observation baseline per unit and
// kernel directory lifetime. It never reads the migration manager's UID paths.
func (m *Manager) collectSystemdPersistenceInterval(sample *SystemMetrics) {
	m.mu.RLock()
	policy, plan := m.cpuPointsPolicy, m.systemdCPUPlan
	complete, requested := m.systemdCPUComplete, m.systemdCPURequested
	previousCPU, previousRAM, previousTime := m.persistencePreviousCPU, m.persistencePreviousRAM, m.persistencePreviousTime
	previousUnits := m.persistencePreviousSystemd
	parentIdentity := m.systemdCPUParent
	units := make(map[int]systemdunit.UnitIdentity, len(m.systemdCPUSlices))
	for uid, identity := range m.systemdCPUSlices {
		units[uid] = identity
	}
	resources := make(map[int]userResourceLimitState, len(m.resourceLimits))
	for uid, resource := range m.resourceLimits {
		resources[uid] = resource
	}
	resourceUnits := make(map[int]systemdunit.UnitIdentity, len(m.systemdResourceUnits))
	for uid, identity := range m.systemdResourceUnits {
		resourceUnits[uid] = identity
	}
	degraded := m.cpuPointsDegraded
	m.mu.RUnlock()
	capacity := cpupoints.CapacityState{}
	if m.cpuCapacity != nil {
		capacity = m.cpuCapacity.State()
	}
	root := policy.Root().Value()
	system := resmanmetrics.SystemPersistenceMetrics{
		SampleEpochID: sample.Timestamp.UnixNano(), IntervalEnd: sample.Timestamp,
		TotalCPUUsagePercent: sample.TotalCPUUsage, TotalCores: sample.TotalCores, SystemLoad: sample.SystemLoad,
		NominalParentPoolPoints: policy.Pool().Value(), ConfiguredBestEffortPoints: policy.BestEffort().Value(),
		ConfiguredRootPoints: &root, EnforcementMode: string(cgroup.EnforcementModeSystemdNative),
		CPUCapacityAvailable: capacity.Available, DenominatorState: resmanmetrics.CPUPointsDenominatorUnavailable,
		CPUPointsDegraded: degraded,
	}
	if !previousTime.IsZero() {
		system.IntervalStart = &previousTime
	}
	if capacity.Available {
		online := capacity.LastVerified.OnlineCPUs().Value()
		system.OnlineCPUs = &online
	}
	if requested && plan.ParentQuota().PeriodMicroseconds() != 0 {
		quota, period := plan.ParentQuota().QuotaMicroseconds(), plan.ParentQuota().PeriodMicroseconds()
		system.ProgrammedParentQuotaUsec, system.ProgrammedParentPeriodUsec = &quota, &period
	}
	currentCPU := make(map[string]cgroup.CPUPointsNodeSnapshot)
	currentRAM := make(map[int]cgroup.MemoryAccountingSnapshot)
	currentUnits := make(map[int]systemdunit.UnitIdentity)
	users := make(map[int]resmanmetrics.UserPersistenceMetrics)
	reader, readable := m.systemdUnits.(systemdAccountingReader)
	sample.systemdAuthorityInventory = nil
	sample.systemdAuthorityInventoryErr = nil
	topology, topologyErr := m.systemdUnits.Discover(context.Background())
	if topologyErr != nil {
		sample.systemdAuthorityInventoryErr = &systemdAuthorityInventoryError{Failure: systemdAuthorityInventoryDiscoveryFailed, Err: topologyErr}
		m.recordPersistenceObservationError(sample, metricsDatabaseCPUPointsReadFailure, 0, topologyErr)
	} else {
		sample.systemdObservationContext = persistenceTopologyFingerprint(topology)
	}
	var coverage map[uint32]bool
	if topologyErr == nil {
		cfg := m.GetConfig()
		includeResourceDetail := (cfg.RAMEnabled && len(sample.RAMEligibleUsers) > 0) || (cfg.IOEnabled && len(sample.IOEligibleUsers) > 0)
		inventory, err := m.systemdUnits.CaptureProcessAuthorityInventory(context.Background(), topology, system.SampleEpochID, includeResourceDetail)
		if err != nil {
			sample.systemdAuthorityInventoryErr = &systemdAuthorityInventoryError{Failure: systemdAuthorityInventoryInspectionFailed, Err: err}
			m.recordPersistenceObservationError(sample, metricsDatabaseCPUPointsReadFailure, 0, err)
		} else {
			sample.systemdAuthorityInventory = &inventory
			coverage = inventory.CPUCoverage()
		}
	}
	allocations := make(map[int]cpupoints.FlatSlicePlan)
	if requested {
		for _, allocation := range plan.Slices() {
			allocations[allocation.UID()] = allocation
		}
	}
	observedWeight, programmedWeight, bestEffortWeight := uint64(0), uint64(0), uint64(0)
	for _, allocation := range allocations {
		weight := uint64(allocation.Weight().Value())
		programmedWeight += weight
		if allocation.Class() == cpupoints.FlatAllocationBestEffort {
			bestEffortWeight += weight
		}
		if allocation.Class() == cpupoints.FlatAllocationGuaranteed {
			system.AppliedGuaranteePoints += allocation.ConfiguredGuaranteePoints()
			system.ProgrammedGuaranteeWeight += weight
		}
	}
	allWeights := readable && topologyErr == nil && complete && capacity.Available && requested && len(topology.Users) == len(units)
	if allWeights && capacity.LastVerified != plan.ParentQuota() {
		allWeights = false
	}
	observe := func(uid int, identity systemdunit.UnitIdentity) systemdunit.UnitAccounting {
		if !readable {
			return systemdunit.UnitAccounting{}
		}
		observed, err := reader.ObserveAccounting(context.Background(), identity)
		if err != nil {
			m.recordPersistenceObservationError(sample, metricsDatabaseCPUPointsReadFailure, uid, err)
			return systemdunit.UnitAccounting{}
		}
		return observed
	}
	if topologyErr == nil && requested {
		if topology.Parent.Identity != parentIdentity {
			allWeights = false
		}
		parent := observe(0, topology.Parent.Identity)
		if parent.CPU != nil {
			key := accountingIdentityKey(topology.Parent.Identity)
			previous, exists := previousCPU[key]
			cpu := *parent.CPU
			currentCPU[key] = cpu
			system.ParentCPUQuota = &cpu.CPUQuota
			system.ParentCPUUsageUsecDelta = cgroupCounterDelta(cpu.Identity, cpu.CPUStat.UsageUsec, previous.Identity, previous.CPUStat.UsageUsec, exists)
			system.ParentCPUPeriodsDelta = cgroupCounterDelta(cpu.Identity, cpu.CPUStat.NrPeriods, previous.Identity, previous.CPUStat.NrPeriods, exists)
			system.ParentCPUThrottledPeriodsDelta = cgroupCounterDelta(cpu.Identity, cpu.CPUStat.NrThrottled, previous.Identity, previous.CPUStat.NrThrottled, exists)
			system.ParentCPUThrottledUsecDelta = cgroupCounterDelta(cpu.Identity, cpu.CPUStat.ThrottledUsec, previous.Identity, previous.CPUStat.ThrottledUsec, exists)
			if cpu.CPUQuota != fmt.Sprintf("%d %d", plan.ParentQuota().QuotaMicroseconds(), plan.ParentQuota().PeriodMicroseconds()) {
				allWeights = false
			}
		} else {
			allWeights = false
			if parent.CPUError != nil {
				m.recordPersistenceObservationError(sample, metricsDatabaseCPUPointsReadFailure, 0, parent.CPUError)
			}
		}
	}
	for _, unit := range topology.Users {
		uid := int(unit.UID)
		metrics := sample.UserMetrics[uid]
		processUnavailable := metrics == nil
		if metrics == nil {
			metrics = &resmanmetrics.UserMetrics{UID: uid, Username: m.getUsername(uid)}
		}
		user := resmanmetrics.UserPersistenceMetrics{Metrics: metrics, ConfiguredClass: string(policy.ClassForUID(uid)), LifecycleState: resmanmetrics.CPUPointsLifecycleEligibleInactive}
		user.ProcessObservationUnavailable = processUnavailable
		if uid == 0 {
			user.ConfiguredClass = string(cpupoints.FlatAllocationRoot)
		}
		if !metrics.EligibleForCPU {
			user.LifecycleState = resmanmetrics.CPUPointsLifecycleIneligible
		}
		if guarantee, exists := policy.GuaranteeForUID(uid); exists {
			points := guarantee.Points().Value()
			user.ConfiguredGuaranteePoints = &points
		}
		cpuCoverage := resmanmetrics.CPUPointsCoverageUnavailable
		if covered, known := coverage[unit.UID]; known {
			cpuCoverage = resmanmetrics.CPUPointsCoveragePartial
			if covered {
				cpuCoverage = resmanmetrics.CPUPointsCoverageComplete
			}
		}
		coverageText := string(cpuCoverage)
		user.CPUAuthorityCoverage = &coverageText
		observed := observe(uid, unit.Unit.Identity)
		if observed.CPU != nil {
			key := accountingIdentityKey(unit.Unit.Identity)
			previous, exists := previousCPU[key]
			cpu := *observed.CPU
			currentCPU[key] = cpu
			user.CPUWeight, user.CPUQuota = &cpu.CPUWeight, cpu.CPUQuota
			user.LeafCPUUsageUsecDelta = cgroupCounterDelta(cpu.Identity, cpu.CPUStat.UsageUsec, previous.Identity, previous.CPUStat.UsageUsec, exists)
			observedWeight += cpu.CPUWeight
		} else {
			allWeights = false
			if observed.CPUError != nil {
				if _, planned := allocations[uid]; planned {
					m.recordPersistenceObservationError(sample, metricsDatabaseCPUPointsReadFailure, uid, observed.CPUError)
				} else {
					m.recordOptionalPersistenceObservationGap(metricsDatabaseCPUPointsReadFailure, uid, observed.CPUError)
				}
			}
		}
		if allocation, exists := allocations[uid]; exists && units[uid] == unit.Unit.Identity {
			weight, class := uint64(allocation.Weight().Value()), string(allocation.Class())
			user.AppliedWeight, user.AppliedClass = &weight, &class
			user.LifecycleState = resmanmetrics.CPUPointsLifecycleApplied
			if user.CPUWeight == nil || *user.CPUWeight != weight {
				allWeights = false
				user.LifecycleState = resmanmetrics.CPUPointsLifecycleFailed
			}
		} else if requested {
			allWeights = false
		}
		resource := resources[uid]
		if resource.ramAuthority.State != "" {
			text := string(resource.ramAuthority.State)
			user.RAMCoverage = &text
		}
		if resource.ioAuthority.State != "" {
			text := string(resource.ioAuthority.State)
			user.IOCoverage = &text
		}
		if resourceUnits[uid] != unit.Unit.Identity {
			text := "unavailable"
			if resource.ramApplied {
				user.RAMCoverage = &text
			}
			if resource.ioApplied {
				user.IOCoverage = &text
			}
		} else if resource.ramApplied {
			swapDisabled := resource.swap
			user.RAMSwapDisabled = &swapDisabled
		}
		if observed.Memory != nil {
			memory := *observed.Memory
			// The CPU identity key includes InvocationID. Memory baselines also
			// require the same unit lifetime, even if a kernel inode is reused.
			sameUnit := previousUnits[uid] == unit.Unit.Identity
			previous, exists := previousRAM[uid]
			currentRAM[uid] = memory
			currentUnits[uid] = unit.Unit.Identity
			user.RAMCgroupUsageBytes = &memory.CurrentBytes
			user.MemoryHighLimit, user.MemoryMaxLimit, user.MemorySwapMax = &memory.HighLimit, &memory.MaxLimit, &memory.SwapMax
			user.MemoryHighEventsDelta = cgroupCounterDelta(memory.Identity, memory.Events.High, previous.Identity, previous.Events.High, exists && sameUnit)
			user.MemoryMaxEventsDelta = cgroupCounterDelta(memory.Identity, memory.Events.Max, previous.Identity, previous.Events.Max, exists && sameUnit)
			user.MemoryOOMEventsDelta = cgroupCounterDelta(memory.Identity, memory.Events.OOM, previous.Identity, previous.Events.OOM, exists && sameUnit)
			user.MemoryOOMKillEventsDelta = cgroupCounterDelta(memory.Identity, memory.Events.OOMKill, previous.Identity, previous.Events.OOMKill, exists && sameUnit)
		} else if resource.ramApplied {
			if observed.MemoryError != nil {
				m.recordPersistenceObservationError(sample, metricsDatabaseRAMReadFailure, uid, observed.MemoryError)
			}
			text := "unavailable"
			user.RAMCoverage = &text
		} else if observed.MemoryError != nil {
			m.recordOptionalPersistenceObservationGap(metricsDatabaseRAMReadFailure, uid, observed.MemoryError)
		}
		users[uid] = user
	}
	// Process observation is independent of slice membership. Keep workloads
	// outside user.slice in history without inventing an applied allocation or
	// adding them to the flat scheduling denominator.
	for uid, observed := range sample.UserMetrics {
		if _, exists := users[uid]; exists || observed == nil {
			continue
		}
		unavailable := string(resmanmetrics.CPUPointsCoverageUnavailable)
		user := resmanmetrics.UserPersistenceMetrics{
			Metrics: observed, ConfiguredClass: string(policy.ClassForUID(uid)),
			LifecycleState:       resmanmetrics.CPUPointsLifecycleEligibleInactive,
			CPUAuthorityCoverage: &unavailable, RAMCoverage: &unavailable, IOCoverage: &unavailable,
		}
		if !observed.EligibleForCPU {
			user.LifecycleState = resmanmetrics.CPUPointsLifecycleIneligible
		}
		if topologyErr != nil {
			// Failed discovery does not prove that an old allocation is absent.
			user.LifecycleState = resmanmetrics.CPUPointsLifecycleFailed
		}
		if uid == 0 {
			user.ConfiguredClass = string(cpupoints.FlatAllocationRoot)
		}
		if guarantee, exists := policy.GuaranteeForUID(uid); exists {
			points := guarantee.Points().Value()
			user.ConfiguredGuaranteePoints = &points
		}
		users[uid] = user
	}
	if topologyErr == nil {
		if err := m.systemdUnits.ConfirmTopology(context.Background(), topology); err != nil {
			allWeights = false
			m.recordPersistenceObservationError(sample, metricsDatabaseCPUPointsReadFailure, 0, err)
			clear(currentCPU)
			clear(currentRAM)
			clear(currentUnits)
		}
	}
	if m.cpuCapacity != nil {
		final := m.cpuCapacity.State()
		if !final.Available || final.LastVerified != capacity.LastVerified {
			allWeights = false
			system.CPUCapacityAvailable = false
			system.OnlineCPUs = nil
		}
	}
	if requested {
		system.ProgrammedSiblingWeightSum, system.ProgrammedBestEffortWeight = &programmedWeight, &bestEffortWeight
		system.DenominatorState = resmanmetrics.CPUPointsDenominatorIncomplete
		if allWeights {
			system.DenominatorState = resmanmetrics.CPUPointsDenominatorComplete
			system.ObservedSiblingWeightSum = &observedWeight
		}
		system.CPUPointsDegraded = degraded || !allWeights || len(sample.systemdObservationFailures) > 0
	} else if topologyErr == nil {
		system.DenominatorState = resmanmetrics.CPUPointsDenominatorInactive
	}
	system.CPUPointsDegraded = system.CPUPointsDegraded || degraded || len(sample.systemdObservationFailures) > 0
	m.mu.Lock()
	m.persistencePreviousCPU, m.persistencePreviousRAM, m.persistencePreviousTime = currentCPU, currentRAM, sample.Timestamp
	m.persistencePreviousSystemd = currentUnits
	sample.CPUPointsSystem = operationalCPUPointsSystemSnapshot(policy.Reserve().Value(), string(capacity.UnavailableReason), system)
	sample.CPUPointsUsers = operationalCPUPointsUserSnapshots(users, system.CPUPointsDegraded)
	m.cpuPointsSystemSnapshot, m.cpuPointsUserSnapshots = sample.CPUPointsSystem, cloneCPUPointsUserSnapshots(sample.CPUPointsUsers)
	m.mu.Unlock()
	sample.PersistenceSystem, sample.PersistenceUsers = system, users
}

// finalizeSystemdResourceCoverage projects the authority outcome produced by
// this control cycle onto every observation surface before it is published.
func (m *Manager) finalizeSystemdResourceCoverage(sample *SystemMetrics) {
	if m.systemdUnits == nil || sample == nil {
		return
	}
	for uid, user := range sample.PersistenceUsers {
		user.RAMCoverage = finalizedResourceCoverage(user.CPUAuthorityCoverage, user.RAMCoverage, sample.systemdRAMAuthority, uid)
		user.IOCoverage = finalizedResourceCoverage(user.CPUAuthorityCoverage, user.IOCoverage, sample.systemdIOAuthority, uid)
		sample.PersistenceUsers[uid] = user

		snapshot := sample.CPUPointsUsers[uid]
		snapshot.RAMCoverage = user.RAMCoverage
		snapshot.IOCoverage = user.IOCoverage
		sample.CPUPointsUsers[uid] = snapshot
	}
	m.mu.Lock()
	m.cpuPointsUserSnapshots = cloneCPUPointsUserSnapshots(sample.CPUPointsUsers)
	m.mu.Unlock()
}

func finalizedResourceCoverage(cpuCoverage, collected *string, authorities map[int]*systemdunit.ResourceAuthority, uid int) *string {
	if cpuCoverage == nil || *cpuCoverage == string(resmanmetrics.CPUPointsCoverageUnavailable) {
		unavailable := string(resmanmetrics.CPUPointsCoverageUnavailable)
		return &unavailable
	}
	authority, requested := authorities[uid]
	if !requested {
		return nil
	}
	if authority == nil || authority.State == "" {
		unavailable := string(resmanmetrics.CPUPointsCoverageUnavailable)
		return &unavailable
	}
	if authority.State == systemdunit.ResourceCoverageComplete && collected != nil && *collected == string(resmanmetrics.CPUPointsCoverageUnavailable) {
		unavailable := string(resmanmetrics.CPUPointsCoverageUnavailable)
		return &unavailable
	}
	coverage := string(authority.State)
	return &coverage
}

func accountingIdentityKey(identity systemdunit.UnitIdentity) string {
	return fmt.Sprintf("%s:%s:%d", identity.Name, identity.InvocationIDString(), identity.ControlGroupID)
}

func persistenceTopologyFingerprint(topology systemdunit.TopologySnapshot) string {
	return systemdunit.TopologyFingerprint(topology)
}
