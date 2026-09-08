package state

import (
	"github.com/fdefilippo/resman/cgroup"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

const (
	typedEnforcementObservationComponent = "enforcement_observation"
	metricsDatabaseCPUPointsReadFailure  = "cpu_points_observation_failure"
	metricsDatabaseRAMReadFailure        = "ram_observation_failure"
)

// collectPersistenceInterval captures one coherent decision interval. Only the
// systemd adapter may publish applied resource state; observation-only mode
// publishes intent without consulting or creating managed cgroups.
func (m *Manager) collectPersistenceInterval(sample *SystemMetrics) {
	if m.systemdUnits != nil {
		m.collectSystemdPersistenceInterval(sample)
		return
	}

	m.mu.RLock()
	policy := m.cpuPointsPolicy
	events := make(map[int]cpuPointsLifecycleEvent, len(m.cpuPointsLifecycleEvents))
	for uid, event := range m.cpuPointsLifecycleEvents {
		events[uid] = event
	}
	previousTime := m.persistencePreviousTime
	degraded := m.cpuPointsDegraded
	m.mu.RUnlock()

	rootPoints := policy.Root().Value()
	system := resmanmetrics.SystemPersistenceMetrics{
		EnforcementMode:            string(m.enforcementStatus.Mode),
		DenominatorState:           resmanmetrics.CPUPointsDenominatorUnavailable,
		SampleEpochID:              sample.Timestamp.UnixNano(),
		IntervalEnd:                sample.Timestamp,
		TotalCPUUsagePercent:       sample.TotalCPUUsage,
		TotalCores:                 sample.TotalCores,
		SystemLoad:                 sample.SystemLoad,
		NominalParentPoolPoints:    policy.Pool().Value(),
		ConfiguredRootPoints:       &rootPoints,
		ConfiguredBestEffortPoints: policy.BestEffort().Value(),
		CPUCapacityAvailable:       false,
		CPUPointsDegraded:          degraded,
	}
	if !previousTime.IsZero() {
		start := previousTime
		system.IntervalStart = &start
	}

	users := make(map[int]resmanmetrics.UserPersistenceMetrics, len(sample.UserMetrics))
	for uid, observed := range sample.UserMetrics {
		unavailable := string(resmanmetrics.CPUPointsCoverageUnavailable)
		user := resmanmetrics.UserPersistenceMetrics{
			Metrics:              observed,
			ConfiguredClass:      string(policy.ClassForUID(uid)),
			LifecycleState:       resmanmetrics.CPUPointsLifecycleEligibleInactive,
			CPUAuthorityCoverage: &unavailable,
			RAMCoverage:          &unavailable,
			IOCoverage:           &unavailable,
		}
		if guarantee, ok := policy.GuaranteeForUID(uid); ok {
			points := guarantee.Points().Value()
			user.ConfiguredGuaranteePoints = &points
		}
		if observed == nil || !observed.EligibleForCPU {
			user.LifecycleState = resmanmetrics.CPUPointsLifecycleIneligible
		}
		if event, ok := events[uid]; ok {
			user.LifecycleState = event.state
		}
		users[uid] = user
	}

	capacityReason := m.enforcementStatus.Reason
	m.mu.Lock()
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
	return resmanmetrics.CPUPointsSystemSnapshot{
		SampleEpochID:                  persisted.SampleEpochID,
		IntervalStart:                  persisted.IntervalStart,
		IntervalEnd:                    persisted.IntervalEnd,
		ReservePoints:                  reserve,
		NominalParentPoolPoints:        persisted.NominalParentPoolPoints,
		ConfiguredBestEffortPoints:     persisted.ConfiguredBestEffortPoints,
		CapacityAvailable:              persisted.CPUCapacityAvailable,
		CapacityUnavailableReason:      capacityReason,
		OnlineCPUs:                     persisted.OnlineCPUs,
		ProgrammedParentQuotaUsec:      persisted.ProgrammedParentQuotaUsec,
		ProgrammedParentPeriodUsec:     persisted.ProgrammedParentPeriodUsec,
		ReconciliationDegraded:         persisted.CPUPointsDegraded,
		AppliedGuaranteePoints:         persisted.AppliedGuaranteePoints,
		ProgrammedGuaranteeWeight:      persisted.ProgrammedGuaranteeWeight,
		ProgrammedSiblingWeightSum:     persisted.ProgrammedSiblingWeightSum,
		ProgrammedBestEffortWeight:     persisted.ProgrammedBestEffortWeight,
		ParentCPUUsageUsecDelta:        persisted.ParentCPUUsageUsecDelta,
		ObservedSiblingWeightSum:       persisted.ObservedSiblingWeightSum,
		ConfiguredRootPoints:           persisted.ConfiguredRootPoints,
		ParentCPUPeriodsDelta:          persisted.ParentCPUPeriodsDelta,
		ParentCPUThrottledPeriodsDelta: persisted.ParentCPUThrottledPeriodsDelta,
		ParentCPUThrottledUsecDelta:    persisted.ParentCPUThrottledUsecDelta,
		DeliveryState:                  delivery,
		DenominatorState:               persisted.DenominatorState,
		EnforcementMode:                persisted.EnforcementMode,
	}
}

func completeParentCPUPointsInterval(snapshot resmanmetrics.SystemPersistenceMetrics) bool {
	return snapshot.ParentCPUUsageUsecDelta != nil &&
		snapshot.ParentCPUPeriodsDelta != nil &&
		snapshot.ParentCPUThrottledPeriodsDelta != nil &&
		snapshot.ParentCPUThrottledUsecDelta != nil
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
		applied := user.AppliedClass != nil && user.AppliedWeight != nil && enforceableCount > 0
		coverage := resmanmetrics.CPUPointsCoverageNone
		if user.LifecycleState == resmanmetrics.CPUPointsLifecycleFailed && user.Metrics == nil {
			coverage = resmanmetrics.CPUPointsCoverageUnavailable
		} else if applied {
			coverage = resmanmetrics.CPUPointsCoverageComplete
			if observedCount != enforceableCount {
				coverage = resmanmetrics.CPUPointsCoveragePartial
			}
		}
		if user.CPUAuthorityCoverage != nil {
			coverage = resmanmetrics.CPUPointsProcessCoverage(*user.CPUAuthorityCoverage)
			applied = (coverage == resmanmetrics.CPUPointsCoverageComplete || coverage == resmanmetrics.CPUPointsCoveragePartial) &&
				user.AppliedWeight != nil && user.CPUWeight != nil && *user.AppliedWeight == *user.CPUWeight
		}
		result[uid] = resmanmetrics.CPUPointsUserSnapshot{
			ProcessObservationUnavailable: user.ProcessObservationUnavailable,
			ObservedWeight:                user.CPUWeight, IOCoverage: user.IOCoverage, CPUAuthorityCoverage: user.CPUAuthorityCoverage,
			UID: uid, Username: username,
			ConfiguredClass: user.ConfiguredClass, ConfiguredGuaranteePoints: user.ConfiguredGuaranteePoints,
			CPUEnforcementRequested: requested, LifecycleState: user.LifecycleState,
			AppliedClass: user.AppliedClass, AppliedWeight: user.AppliedWeight,
			AppliedToProcesses: applied, CompleteUIDWorkloadGuaranteed: applied && (!degraded || user.CPUAuthorityCoverage == nil) && coverage == resmanmetrics.CPUPointsCoverageComplete,
			ReconciliationDegraded: degraded, ProcessCoverage: coverage,
			ObservedProcessCount: observedCount, EnforceableProcessCount: enforceableCount,
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

func (m *Manager) clearPersistedCPUPointsLifecycleEvents() {
	m.mu.Lock()
	clear(m.cpuPointsLifecycleEvents)
	m.mu.Unlock()
}
