package state

import (
	"fmt"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/internal/cpupoints"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

const (
	metricsDatabaseCPUPointsReadFailure = "cpu_points_observation_failure"
	metricsDatabaseRAMReadFailure       = "ram_observation_failure"
)

// collectPersistenceInterval captures kernel counters at the same decision
// sample boundary as the process-derived user metrics.
func (m *Manager) collectPersistenceInterval(sample *SystemMetrics) {
	if m.metricsCollector == nil || m.metricsCollector.GetDBWriter() == nil {
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
	m.mu.Unlock()
	sample.PersistenceSystem = system
	sample.PersistenceUsers = users
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
		m.logger.Warn("Failed to collect persisted cgroup accounting", "uid", uid, "error_type", errorType, "error", err)
	} else {
		m.logger.Warn("Failed to collect persisted cgroup accounting", "error_type", errorType, "error", err)
	}
	if m.prometheusExporter != nil {
		m.prometheusExporter.RecordError(metricsDatabaseErrorComponent, errorType)
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
