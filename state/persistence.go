package state

import (
	"fmt"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/internal/cpupoints"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

const (
	typedEnforcementObservationComponent = "enforcement_observation"
	metricsDatabaseCPUPointsReadFailure  = "cpu_points_observation_failure"
	metricsDatabaseRAMReadFailure        = "ram_observation_failure"
)

// collectPersistenceInterval captures kernel counters at the same decision
// sample boundary as the process-derived user metrics.
func (m *Manager) collectPersistenceInterval(sample *SystemMetrics) {
	if m.cgroupManager == nil {
		return
	}

	m.mu.RLock()
	policy := m.cpuPointsPolicy
	hierarchy := m.cpuPointsHierarchy
	allocations := make(map[int]cpuPointsAllocation, len(m.cpuAllocations))
	for uid, allocation := range m.cpuAllocations {
		allocations[uid] = allocation
	}
	resources := make(map[int]userResourceLimitState, len(m.resourceLimits))
	for uid, state := range m.resourceLimits {
		resources[uid] = state
	}
	coverage := make(map[int]ramCoverageState, len(m.ramCoverage))
	for uid, state := range m.ramCoverage {
		copyState := state
		copyState.partial = make(map[int]uint64, len(state.partial))
		for pid, startTime := range state.partial {
			copyState.partial[pid] = startTime
		}
		coverage[uid] = copyState
	}
	events := make(map[int]cpuPointsLifecycleEvent, len(m.cpuPointsLifecycleEvents))
	for uid, event := range m.cpuPointsLifecycleEvents {
		events[uid] = event
	}
	previousCPU := m.persistencePreviousCPU
	previousRAM := m.persistencePreviousRAM
	previousTime := m.persistencePreviousTime
	degraded := m.cpuPointsDegraded
	appliedGuarantees := m.appliedGuaranteePoints.Value()
	programmedGuarantees := m.programmedGuaranteePoints
	m.mu.RUnlock()

	capacity := cpupoints.CapacityState{}
	if m.cpuCapacity != nil {
		capacity = m.cpuCapacity.State()
	}
	system := resmanmetrics.SystemPersistenceMetrics{
		SampleEpochID:              sample.Timestamp.UnixNano(),
		IntervalEnd:                sample.Timestamp,
		TotalCPUUsagePercent:       sample.TotalCPUUsage,
		TotalCores:                 sample.TotalCores,
		SystemLoad:                 sample.SystemLoad,
		NominalParentPoolPoints:    policy.Pool().Value(),
		CPUCapacityAvailable:       capacity.Available,
		CPUPointsDegraded:          degraded,
		AppliedGuaranteePoints:     appliedGuarantees,
		ProgrammedGuaranteeWeight:  programmedGuarantees,
		ConfiguredBestEffortWeight: policy.BestEffort().Value(),
	}
	capacityReason := string(capacity.UnavailableReason)
	if !previousTime.IsZero() {
		start := previousTime
		system.IntervalStart = &start
	}
	if capacity.LastVerified.PeriodMicroseconds() != 0 {
		quota := capacity.LastVerified.QuotaMicroseconds()
		period := capacity.LastVerified.PeriodMicroseconds()
		system.ProgrammedParentQuotaUsec = &quota
		system.ProgrammedParentPeriodUsec = &period
		if capacity.Available {
			online := capacity.LastVerified.OnlineCPUs().Value()
			system.OnlineCPUs = &online
		}
	}

	currentCPU := make(map[string]cgroup.CPUPointsNodeSnapshot)
	nodes := []struct {
		name string
		path string
	}{
		{name: "parent", path: hierarchy.Parent},
		{name: "guaranteed", path: hierarchy.Guaranteed},
		{name: "best_effort", path: hierarchy.BestEffort},
	}
	for _, node := range nodes {
		if node.path == "" {
			continue
		}
		observed, err := m.cgroupManager.GetCPUPointsNodeSnapshot(node.path)
		if err != nil {
			m.recordPersistenceObservationError(metricsDatabaseCPUPointsReadFailure, 0, fmt.Errorf("read %s CPU Points node: %w", node.name, err))
			continue
		}
		currentCPU[node.path] = observed
		previous, hasPrevious := previousCPU[node.path]
		switch node.name {
		case "parent":
			quota := observed.CPUQuota
			system.ParentCPUQuota = &quota
			system.ParentCPUUsageUsecDelta = cgroupCounterDelta(observed.Identity, observed.CPUStat.UsageUsec, previous.Identity, previous.CPUStat.UsageUsec, hasPrevious)
			system.ParentCPUPeriodsDelta = cgroupCounterDelta(observed.Identity, observed.CPUStat.NrPeriods, previous.Identity, previous.CPUStat.NrPeriods, hasPrevious)
			system.ParentCPUThrottledPeriodsDelta = cgroupCounterDelta(observed.Identity, observed.CPUStat.NrThrottled, previous.Identity, previous.CPUStat.NrThrottled, hasPrevious)
			system.ParentCPUThrottledUsecDelta = cgroupCounterDelta(observed.Identity, observed.CPUStat.ThrottledUsec, previous.Identity, previous.CPUStat.ThrottledUsec, hasPrevious)
		case "guaranteed":
			weight := observed.CPUWeight
			system.GuaranteedDomainCPUWeight = &weight
			system.GuaranteedDomainCPUUsageUsecDelta = cgroupCounterDelta(observed.Identity, observed.CPUStat.UsageUsec, previous.Identity, previous.CPUStat.UsageUsec, hasPrevious)
		case "best_effort":
			weight := observed.CPUWeight
			system.BestEffortDomainCPUWeight = &weight
			system.BestEffortDomainCPUUsageUsecDelta = cgroupCounterDelta(observed.Identity, observed.CPUStat.UsageUsec, previous.Identity, previous.CPUStat.UsageUsec, hasPrevious)
		}
	}

	users := make(map[int]resmanmetrics.UserPersistenceMetrics, len(sample.UserMetrics))
	currentRAM := make(map[int]cgroup.MemoryAccountingSnapshot)
	for uid, observed := range sample.UserMetrics {
		user := resmanmetrics.UserPersistenceMetrics{
			Metrics:         observed,
			ConfiguredClass: string(policy.ClassForUID(uid)),
			LifecycleState:  resmanmetrics.CPUPointsLifecycleEligibleInactive,
		}
		if guarantee, ok := policy.GuaranteeForUID(uid); ok {
			points := guarantee.Points().Value()
			user.ConfiguredGuaranteePoints = &points
		}
		if !observed.EligibleForCPU {
			user.LifecycleState = resmanmetrics.CPUPointsLifecycleIneligible
		}
		event, hasEvent := events[uid]
		if hasEvent {
			user.LifecycleState = event.state
			user.PIDNamespaceMismatchCount = event.pidNamespaceMismatches
			user.PIDNamespaceUnavailableCount = event.pidNamespaceUnavailable
		}
		if allocation, ok := allocations[uid]; ok {
			class := string(allocation.class)
			weight := uint64(allocation.weight.Value())
			user.AppliedClass = &class
			user.AppliedWeight = &weight
			user.PIDNamespaceMismatchCount = allocation.pidNamespaceMismatches
			user.PIDNamespaceUnavailableCount = allocation.pidNamespaceUnavailable
			user.CgroupPath = allocation.leafPath
			if !hasEvent || event.state == resmanmetrics.CPUPointsLifecycleApplied {
				user.LifecycleState = resmanmetrics.CPUPointsLifecycleApplied
			}
			if leaf, ok := currentCPU[allocation.leafPath]; ok {
				user.CPUQuota = leaf.CPUQuota
				leafWeight := leaf.CPUWeight
				user.CPUWeight = &leafWeight
				previous, hasPrevious := previousCPU[allocation.leafPath]
				user.LeafCPUUsageUsecDelta = cgroupCounterDelta(leaf.Identity, leaf.CPUStat.UsageUsec, previous.Identity, previous.CPUStat.UsageUsec, hasPrevious)
			} else {
				leaf, err := m.cgroupManager.GetCPUPointsNodeSnapshot(allocation.leafPath)
				if err != nil {
					m.recordPersistenceObservationError(metricsDatabaseCPUPointsReadFailure, uid, fmt.Errorf("read CPU Points leaf: %w", err))
				} else {
					currentCPU[allocation.leafPath] = leaf
					user.CPUQuota = leaf.CPUQuota
					leafWeight := leaf.CPUWeight
					user.CPUWeight = &leafWeight
					previous, hasPrevious := previousCPU[allocation.leafPath]
					user.LeafCPUUsageUsecDelta = cgroupCounterDelta(leaf.Identity, leaf.CPUStat.UsageUsec, previous.Identity, previous.CPUStat.UsageUsec, hasPrevious)
				}
			}
		}

		resource := resources[uid]
		if _, cpuApplied := allocations[uid]; cpuApplied && !resource.ramApplied {
			memory, err := m.cgroupManager.GetMemoryAccountingSnapshot(uid)
			if err != nil {
				m.recordOptionalPersistenceObservationGap(metricsDatabaseRAMReadFailure, uid, err)
			} else {
				user.CgroupPath = memory.Path
				currentBytes := memory.CurrentBytes
				user.RAMCgroupUsageBytes = &currentBytes
			}
		}
		if resource.ramApplied {
			ramCoverage := RAMCoverageComplete
			incomplete := 0
			if state, ok := coverage[uid]; ok && state.coverage == RAMCoveragePartial {
				ramCoverage = RAMCoveragePartial
				incomplete = len(state.partial)
			}
			coverageText := string(ramCoverage)
			user.RAMCoverage = &coverageText
			user.RAMCoverageIncompleteProcessCount = incomplete
			swapDisabled := resource.swap
			user.RAMSwapDisabled = &swapDisabled
			memory, err := m.cgroupManager.GetMemoryAccountingSnapshot(uid)
			if err != nil {
				m.recordPersistenceObservationError(metricsDatabaseRAMReadFailure, uid, err)
			} else {
				currentRAM[uid] = memory
				user.CgroupPath = memory.Path
				currentBytes := memory.CurrentBytes
				user.RAMCgroupUsageBytes = &currentBytes
				high, max, swapMax := memory.HighLimit, memory.MaxLimit, memory.SwapMax
				user.MemoryHighLimit, user.MemoryMaxLimit, user.MemorySwapMax = &high, &max, &swapMax
				previous, hasPrevious := previousRAM[uid]
				user.MemoryHighEventsDelta = cgroupCounterDelta(memory.Identity, memory.Events.High, previous.Identity, previous.Events.High, hasPrevious)
				user.MemoryMaxEventsDelta = cgroupCounterDelta(memory.Identity, memory.Events.Max, previous.Identity, previous.Events.Max, hasPrevious)
				user.MemoryOOMEventsDelta = cgroupCounterDelta(memory.Identity, memory.Events.OOM, previous.Identity, previous.Events.OOM, hasPrevious)
				user.MemoryOOMKillEventsDelta = cgroupCounterDelta(memory.Identity, memory.Events.OOMKill, previous.Identity, previous.Events.OOMKill, hasPrevious)
			}
		}
		users[uid] = user
	}

	m.mu.Lock()
	m.persistencePreviousCPU = currentCPU
	m.persistencePreviousRAM = currentRAM
	m.persistencePreviousTime = sample.Timestamp
	sample.CPUPointsSystem = operationalCPUPointsSystemSnapshot(policy.Reserve().Value(), capacityReason, system)
	sample.CPUPointsUsers = operationalCPUPointsUserSnapshots(users, degraded)
	m.cpuPointsSystemSnapshot = sample.CPUPointsSystem
	m.cpuPointsUserSnapshots = cloneCPUPointsUserSnapshots(sample.CPUPointsUsers)
	m.mu.Unlock()
	sample.PersistenceSystem = system
	sample.PersistenceUsers = users
}

func operationalCPUPointsSystemSnapshot(reserve uint64, capacityReason string, persisted resmanmetrics.SystemPersistenceMetrics) resmanmetrics.CPUPointsSystemSnapshot {
	delivery := resmanmetrics.CPUPointsDeliveryUnavailable
	if persisted.CPUCapacityAvailable && completeParentCPUPointsInterval(persisted) {
		delivery = resmanmetrics.CPUPointsDeliveryAvailable
		if *persisted.ParentCPUThrottledPeriodsDelta > 0 {
			delivery = resmanmetrics.CPUPointsDeliveryThrottledParent
		}
	}
	lending := resmanmetrics.CPUPointsLendingUnavailable
	if persisted.CPUCapacityAvailable && completeCPUPointsLendingInterval(persisted) {
		lending = resmanmetrics.CPUPointsLendingInactive
		if *persisted.GuaranteedDomainCPUUsageUsecDelta > 0 {
			lending = resmanmetrics.CPUPointsLendingGuaranteedPriority
		} else if *persisted.BestEffortDomainCPUUsageUsecDelta > 0 {
			lending = resmanmetrics.CPUPointsLendingBestEffortEntitled
			if observedBestEffortBorrowing(persisted) {
				lending = resmanmetrics.CPUPointsLendingBestEffortBorrowed
			}
		}
	}
	return resmanmetrics.CPUPointsSystemSnapshot{
		SampleEpochID:                     persisted.SampleEpochID,
		IntervalStart:                     persisted.IntervalStart,
		IntervalEnd:                       persisted.IntervalEnd,
		ReservePoints:                     reserve,
		NominalParentPoolPoints:           persisted.NominalParentPoolPoints,
		ConfiguredBestEffortPoints:        persisted.ConfiguredBestEffortWeight,
		CapacityAvailable:                 persisted.CPUCapacityAvailable,
		CapacityUnavailableReason:         capacityReason,
		OnlineCPUs:                        persisted.OnlineCPUs,
		ProgrammedParentQuotaUsec:         persisted.ProgrammedParentQuotaUsec,
		ProgrammedParentPeriodUsec:        persisted.ProgrammedParentPeriodUsec,
		ReconciliationDegraded:            persisted.CPUPointsDegraded,
		AppliedGuaranteePoints:            persisted.AppliedGuaranteePoints,
		ProgrammedGuaranteeWeight:         persisted.ProgrammedGuaranteeWeight,
		GuaranteedDomainWeight:            persisted.GuaranteedDomainCPUWeight,
		BestEffortDomainWeight:            persisted.BestEffortDomainCPUWeight,
		ParentCPUUsageUsecDelta:           persisted.ParentCPUUsageUsecDelta,
		GuaranteedDomainCPUUsageUsecDelta: persisted.GuaranteedDomainCPUUsageUsecDelta,
		BestEffortDomainCPUUsageUsecDelta: persisted.BestEffortDomainCPUUsageUsecDelta,
		ParentCPUPeriodsDelta:             persisted.ParentCPUPeriodsDelta,
		ParentCPUThrottledPeriodsDelta:    persisted.ParentCPUThrottledPeriodsDelta,
		ParentCPUThrottledUsecDelta:       persisted.ParentCPUThrottledUsecDelta,
		DeliveryState:                     delivery,
		LendingState:                      lending,
	}
}

func completeParentCPUPointsInterval(snapshot resmanmetrics.SystemPersistenceMetrics) bool {
	return snapshot.ParentCPUUsageUsecDelta != nil &&
		snapshot.ParentCPUPeriodsDelta != nil &&
		snapshot.ParentCPUThrottledPeriodsDelta != nil &&
		snapshot.ParentCPUThrottledUsecDelta != nil
}

func completeCPUPointsLendingInterval(snapshot resmanmetrics.SystemPersistenceMetrics) bool {
	return snapshot.ParentCPUUsageUsecDelta != nil &&
		snapshot.GuaranteedDomainCPUUsageUsecDelta != nil &&
		snapshot.BestEffortDomainCPUUsageUsecDelta != nil
}

func observedBestEffortBorrowing(snapshot resmanmetrics.SystemPersistenceMetrics) bool {
	if snapshot.AppliedGuaranteePoints == 0 || snapshot.ParentCPUUsageUsecDelta == nil || snapshot.BestEffortDomainCPUUsageUsecDelta == nil || *snapshot.ParentCPUUsageUsecDelta == 0 {
		return false
	}
	totalWeight := snapshot.ProgrammedGuaranteeWeight + snapshot.ConfiguredBestEffortWeight
	if totalWeight == 0 {
		return false
	}
	observedShare := float64(*snapshot.BestEffortDomainCPUUsageUsecDelta) / float64(*snapshot.ParentCPUUsageUsecDelta)
	entitledShare := float64(snapshot.ConfiguredBestEffortWeight) / float64(totalWeight)
	return observedShare > entitledShare
}

func operationalCPUPointsUserSnapshots(persisted map[int]resmanmetrics.UserPersistenceMetrics, degraded bool) map[int]resmanmetrics.CPUPointsUserSnapshot {
	result := make(map[int]resmanmetrics.CPUPointsUserSnapshot, len(persisted))
	for uid, user := range persisted {
		observedCount, enforceableCount := 0, 0
		requested := false
		username := ""
		if user.Metrics != nil {
			username = user.Metrics.Username
			observedCount = user.Metrics.ProcessCount
			enforceableCount = user.Metrics.EnforceableUsage.ProcessCount
			requested = user.Metrics.CPULimitRequested
		}
		acquiredCount := enforceableCount - user.PIDNamespaceMismatchCount - user.PIDNamespaceUnavailableCount
		if acquiredCount < 0 {
			acquiredCount = 0
		}
		applied := user.AppliedClass != nil && user.AppliedWeight != nil && acquiredCount > 0
		coverage := resmanmetrics.CPUPointsCoverageNone
		if user.LifecycleState == resmanmetrics.CPUPointsLifecycleFailed && user.Metrics == nil {
			coverage = resmanmetrics.CPUPointsCoverageUnavailable
		} else if applied {
			coverage = resmanmetrics.CPUPointsCoverageComplete
			if acquiredCount != observedCount || observedCount != enforceableCount || user.PIDNamespaceMismatchCount > 0 || user.PIDNamespaceUnavailableCount > 0 {
				coverage = resmanmetrics.CPUPointsCoveragePartial
			}
		}
		result[uid] = resmanmetrics.CPUPointsUserSnapshot{
			UID: uid, Username: username,
			ConfiguredClass: user.ConfiguredClass, ConfiguredGuaranteePoints: user.ConfiguredGuaranteePoints,
			CPUEnforcementRequested: requested, LifecycleState: user.LifecycleState,
			AppliedClass: user.AppliedClass, AppliedWeight: user.AppliedWeight,
			AppliedToProcesses: applied, CompleteUIDWorkloadGuaranteed: applied && coverage == resmanmetrics.CPUPointsCoverageComplete,
			ReconciliationDegraded: degraded, ProcessCoverage: coverage,
			ObservedProcessCount: observedCount, EnforceableProcessCount: enforceableCount,
			PIDNamespaceMismatchCount: user.PIDNamespaceMismatchCount, PIDNamespaceUnavailableCount: user.PIDNamespaceUnavailableCount,
			CgroupPath: user.CgroupPath, LeafCPUUsageUsecDelta: user.LeafCPUUsageUsecDelta,
			RAMCgroupUsageBytes: user.RAMCgroupUsageBytes, RAMCoverage: user.RAMCoverage,
			RAMCoverageIncompleteProcessCount: user.RAMCoverageIncompleteProcessCount, RAMSwapDisabled: user.RAMSwapDisabled,
			MemoryHighLimit: user.MemoryHighLimit, MemoryMaxLimit: user.MemoryMaxLimit, MemorySwapMax: user.MemorySwapMax,
			MemoryHighEventsDelta: user.MemoryHighEventsDelta, MemoryMaxEventsDelta: user.MemoryMaxEventsDelta,
			MemoryOOMEventsDelta: user.MemoryOOMEventsDelta, MemoryOOMKillEventsDelta: user.MemoryOOMKillEventsDelta,
		}
	}
	return result
}

func cloneCPUPointsUserSnapshots(source map[int]resmanmetrics.CPUPointsUserSnapshot) map[int]resmanmetrics.CPUPointsUserSnapshot {
	cloned := make(map[int]resmanmetrics.CPUPointsUserSnapshot, len(source))
	for uid, snapshot := range source {
		cloned[uid] = snapshot
	}
	return cloned
}

func cgroupCounterDelta(currentIdentity cgroup.CgroupIdentity, current uint64, previousIdentity cgroup.CgroupIdentity, previous uint64, available bool) *uint64 {
	if !available || currentIdentity != previousIdentity || current < previous {
		return nil
	}
	delta := current - previous
	return &delta
}

func (m *Manager) recordPersistenceObservationError(errorType string, uid int, err error) {
	if uid > 0 {
		m.logger.Warn("Failed to collect typed enforcement accounting", "uid", uid, "error_type", errorType, "error", err)
	} else {
		m.logger.Warn("Failed to collect typed enforcement accounting", "error_type", errorType, "error", err)
	}
	if m.prometheusExporter != nil {
		m.prometheusExporter.RecordError(typedEnforcementObservationComponent, errorType)
	}
}

func (m *Manager) recordOptionalPersistenceObservationGap(errorType string, uid int, err error) {
	m.logger.Debug("Optional typed enforcement accounting unavailable", "uid", uid, "error_type", errorType, "error", err)
	if m.prometheusExporter != nil {
		m.prometheusExporter.RecordError(typedEnforcementObservationComponent, errorType)
	}
}

func (m *Manager) recordCPUPointsAdmissionOutcome(uid int, result cgroup.ProcessMoveResult, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil && result.Applied() {
		if result.NamespaceSkipped() == 0 {
			delete(m.cpuPointsLifecycleEvents, uid)
			return
		}
		m.cpuPointsLifecycleEvents[uid] = cpuPointsLifecycleEvent{
			state: resmanmetrics.CPUPointsLifecycleApplied, pidNamespaceMismatches: result.PIDNamespaceMismatches, pidNamespaceUnavailable: result.PIDNamespaceUnavailable,
		}
		return
	}
	state := resmanmetrics.CPUPointsLifecycleFailed
	if !result.Applied() && result.NamespaceSkipped() > 0 {
		state = resmanmetrics.CPUPointsLifecycleNamespaceRejected
	}
	m.cpuPointsLifecycleEvents[uid] = cpuPointsLifecycleEvent{
		state: state, pidNamespaceMismatches: result.PIDNamespaceMismatches, pidNamespaceUnavailable: result.PIDNamespaceUnavailable,
	}
}

func (m *Manager) recordCPUPointsReleaseOutcome(uid int, released bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.cpuPointsLifecycleEvents[uid] = cpuPointsLifecycleEvent{state: resmanmetrics.CPUPointsLifecycleFailed}
		return
	}
	if released {
		m.cpuPointsLifecycleEvents[uid] = cpuPointsLifecycleEvent{state: resmanmetrics.CPUPointsLifecycleReleased}
	}
}

func (m *Manager) clearPersistedCPUPointsLifecycleEvents() {
	m.mu.Lock()
	clear(m.cpuPointsLifecycleEvents)
	m.mu.Unlock()
}
