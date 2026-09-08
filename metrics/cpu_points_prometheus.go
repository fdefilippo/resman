package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type cpuPointsPrometheusMetrics struct {
	reservePoints              *prometheus.GaugeVec
	nominalPoolPoints          *prometheus.GaugeVec
	bestEffortPoints           *prometheus.GaugeVec
	capacityAvailable          *prometheus.GaugeVec
	onlineCPUs                 *prometheus.GaugeVec
	parentQuotaUsec            *prometheus.GaugeVec
	parentPeriodUsec           *prometheus.GaugeVec
	reconciliationDegraded     *prometheus.GaugeVec
	appliedGuaranteePoints     *prometheus.GaugeVec
	programmedGuaranteeWeight  *prometheus.GaugeVec
	programmedSiblingWeightSum *prometheus.GaugeVec
	programmedBestEffortWeight *prometheus.GaugeVec
	intervalSeconds            *prometheus.GaugeVec
	parentUsageUsecDelta       *prometheus.GaugeVec
	observedSiblingWeightSum   *prometheus.GaugeVec
	rootPoints                 *prometheus.GaugeVec
	parentPeriodsDelta         *prometheus.GaugeVec
	parentThrottledDelta       *prometheus.GaugeVec
	parentThrottledUsecDelta   *prometheus.GaugeVec
	deliveryState              *prometheus.GaugeVec
	denominatorState           *prometheus.GaugeVec
	userConfiguredClass        *prometheus.GaugeVec
	userConfiguredGuarantee    *prometheus.GaugeVec
	userRequested              *prometheus.GaugeVec
	userLifecycle              *prometheus.GaugeVec
	userAppliedClass           *prometheus.GaugeVec
	userAppliedWeight          *prometheus.GaugeVec
	userProgrammedWeight       *prometheus.GaugeVec
	userObservedWeight         *prometheus.GaugeVec
	userIOCoverage             *prometheus.GaugeVec
	userAppliedToProcesses     *prometheus.GaugeVec
	userCompleteUIDGuaranteed  *prometheus.GaugeVec
	userReconciliationDegraded *prometheus.GaugeVec
	userProcessCoverage        *prometheus.GaugeVec
	userObservedProcesses      *prometheus.GaugeVec
	userEnforceableProcesses   *prometheus.GaugeVec
	userLeafUsageUsecDelta     *prometheus.GaugeVec
	userRAMCgroupUsage         *prometheus.GaugeVec
	userRAMCoverage            *prometheus.GaugeVec
	userRAMIncompleteProcesses *prometheus.GaugeVec
	userMemoryHighEventsDelta  *prometheus.GaugeVec
	userMemoryMaxEventsDelta   *prometheus.GaugeVec
	userMemoryOOMEventsDelta   *prometheus.GaugeVec
	userMemoryOOMKillsDelta    *prometheus.GaugeVec
}

func (m *cpuPointsPrometheusMetrics) register(registry prometheus.Registerer, namespace string, labels prometheus.Labels) {
	system := func(name, help string) *prometheus.GaugeVec {
		return promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help, ConstLabels: labels}, nil)
	}
	user := func(name, help string, extra ...string) *prometheus.GaugeVec {
		variable := append([]string{"uid", "username"}, extra...)
		return promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help, ConstLabels: labels}, variable)
	}
	m.reservePoints = system("cpu_points_reserve", "Configured nominal CPU Points kept outside the finite ResMan parent")
	m.nominalPoolPoints = system("cpu_points_nominal_parent_pool", "Configured nominal parent pool in CPU Points; this is not delivered CPU bandwidth")
	m.bestEffortPoints = system("cpu_points_best_effort_entitlement", "Configured aggregate best-effort entitlement in CPU Points")
	m.capacityAvailable = system("cpu_points_capacity_available", "Whether the live online-CPU denominator and programmed parent quota are currently verified")
	m.onlineCPUs = system("cpu_points_online_cpus", "Verified live online logical CPU denominator used to program the CPU Points parent")
	m.parentQuotaUsec = system("cpu_points_parent_quota_microseconds", "Programmed finite parent quota in microseconds; nominal capacity, not a delivered guarantee")
	m.parentPeriodUsec = system("cpu_points_parent_period_microseconds", "Programmed CPU Points parent period in microseconds")
	m.reconciliationDegraded = system("cpu_points_reconciliation_degraded", "Whether CPU Points reconciliation retained a conservative prior state after an error")
	m.appliedGuaranteePoints = system("cpu_points_applied_guarantee_total", "Sum of mapped guarantees in the last published plan; consult reconciliation and denominator state for current validity")
	m.programmedGuaranteeWeight = system("cpu_points_programmed_guaranteed_weight_sum", "Sum of programmed weights of guaranteed user slices")
	m.programmedSiblingWeightSum = system("cpu_points_programmed_sibling_weight_sum", "Sum of programmed weights for every active user slice")
	m.programmedBestEffortWeight = system("cpu_points_programmed_best_effort_weight", "Programmed aggregate best-effort weight across active user slices")
	m.intervalSeconds = system("cpu_points_observation_interval_seconds", "Common authoritative control-cycle interval for CPU Points usage and throttling deltas")
	m.parentUsageUsecDelta = system("cpu_points_parent_usage_microseconds_delta", "Effective CPU time delivered by the parent during the common control-cycle interval; use as the allocation denominator")
	m.observedSiblingWeightSum = system("cpu_points_observed_sibling_weight_sum", "Sum of observed weights after complete sibling confirmation")
	m.rootPoints = system("cpu_points_root_entitlement", "Configured root CPU Points entitlement inside the parent pool")
	m.parentPeriodsDelta = system("cpu_points_parent_periods_delta", "Parent CFS periods observed during the common control-cycle interval")
	m.parentThrottledDelta = system("cpu_points_parent_throttled_periods_delta", "Parent CFS throttled periods during the common interval; positive values can be expected under a saturated finite pool")
	m.parentThrottledUsecDelta = system("cpu_points_parent_throttled_microseconds_delta", "Parent CFS throttled time during the common interval; CFS may under-deliver nominal quota")
	m.deliveryState = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "cpu_points_delivery_state", Help: "Bounded observed parent state: available, throttled_parent, or unavailable", ConstLabels: labels}, []string{"state"})
	m.denominatorState = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "cpu_points_denominator_state", Help: "Confirmed flat scheduling denominator: unavailable, inactive, incomplete or complete", ConstLabels: labels}, []string{"state"})
	m.userConfiguredClass = user("user_cpu_points_configured_class", "Configured bounded CPU Points class", "class")
	m.userConfiguredGuarantee = user("user_cpu_points_configured_guarantee", "Optional mapped-user guarantee in CPU Points; absent for best-effort users")
	m.userRequested = user("user_cpu_points_enforcement_requested", "Whether CPU enforcement is currently requested for the user")
	m.userLifecycle = user("user_cpu_points_lifecycle_state", "Bounded CPU Points lifecycle state", "state")
	m.userAppliedClass = user("user_cpu_points_applied_class", "Class of a currently verified authoritative user slice", "class")
	m.userAppliedWeight = user("user_cpu_points_applied_weight", "Raw verified leaf cpu.weight; weight is not observed CPU Points delivery")
	m.userProgrammedWeight = user("user_cpu_points_programmed_weight", "Weight in the last published plan for this unit lifetime; not proof of current kernel agreement")
	m.userObservedWeight = user("user_cpu_points_observed_weight", "Raw cpu.weight read from the authoritative user slice during this sample")
	m.userIOCoverage = user("user_io_coverage", "Resource-specific I/O authority coverage", "coverage")
	m.userAppliedToProcesses = user("user_cpu_points_applied_to_processes", "Whether the authoritative slice weight agrees with the plan; coverage is reported separately")
	m.userCompleteUIDGuaranteed = user("user_cpu_points_complete_uid_workload_guaranteed", "Whether the applied leaf covers the complete observed UID workload; partial host subsets are zero")
	m.userReconciliationDegraded = user("user_cpu_points_reconciliation_degraded", "Whether the current CPU Points epoch is conservatively degraded")
	m.userProcessCoverage = user("user_cpu_points_process_coverage", "Bounded applied process coverage: none, complete, partial, or unavailable", "coverage")
	m.userObservedProcesses = user("user_cpu_points_observed_processes", "Complete observed process count for the UID")
	m.userEnforceableProcesses = user("user_cpu_points_enforceable_processes", "Host-enforceable process count within the authoritative systemd boundary")
	m.userLeafUsageUsecDelta = user("user_cpu_points_leaf_usage_microseconds_delta", "Effective leaf CPU time during the same authoritative interval as the parent denominator")
	m.userRAMCgroupUsage = user("user_ram_cgroup_memory_current_bytes", "memory.current charged to the authoritative user slice; not process-derived UID memory")
	m.userRAMCoverage = user("user_ram_cgroup_coverage", "Resource authority coverage on the authoritative user slice: complete, partial, refused or unavailable", "coverage")
	m.userRAMIncompleteProcesses = user("user_ram_cgroup_coverage_incomplete_processes", "Processes whose pre-ingress page charges make RAM cgroup accounting partial")
	m.userMemoryHighEventsDelta = user("user_memory_high_events_delta", "memory.high events during the common interval; growth can mean throttling or stall and does not promise a kill")
	m.userMemoryMaxEventsDelta = user("user_memory_max_events_delta", "memory.max events during the common interval, distinct from memory.high throttling")
	m.userMemoryOOMEventsDelta = user("user_memory_oom_events_delta", "Memory OOM events during the common interval, distinct from memory.high throttling")
	m.userMemoryOOMKillsDelta = user("user_memory_oom_kill_events_delta", "Memory OOM kill events during the common interval; zero does not mean memory.high enforcement is absent")
}

func (m *cpuPointsPrometheusMetrics) updateSystem(snapshot CPUPointsSystemSnapshot) {
	m.reservePoints.WithLabelValues().Set(float64(snapshot.ReservePoints))
	m.nominalPoolPoints.WithLabelValues().Set(float64(snapshot.NominalParentPoolPoints))
	m.bestEffortPoints.WithLabelValues().Set(float64(snapshot.ConfiguredBestEffortPoints))
	m.capacityAvailable.WithLabelValues().Set(boolMetricValue(snapshot.CapacityAvailable))
	m.reconciliationDegraded.WithLabelValues().Set(boolMetricValue(snapshot.ReconciliationDegraded))
	m.appliedGuaranteePoints.WithLabelValues().Set(float64(snapshot.AppliedGuaranteePoints))
	m.programmedGuaranteeWeight.WithLabelValues().Set(float64(snapshot.ProgrammedGuaranteeWeight))
	setOptionalGauge(m.onlineCPUs, snapshot.OnlineCPUs)
	setOptionalGauge(m.parentQuotaUsec, snapshot.ProgrammedParentQuotaUsec)
	setOptionalGauge(m.parentPeriodUsec, snapshot.ProgrammedParentPeriodUsec)
	setOptionalGauge(m.programmedSiblingWeightSum, snapshot.ProgrammedSiblingWeightSum)
	setOptionalGauge(m.programmedBestEffortWeight, snapshot.ProgrammedBestEffortWeight)
	setOptionalGauge(m.parentUsageUsecDelta, snapshot.ParentCPUUsageUsecDelta)
	setOptionalGauge(m.observedSiblingWeightSum, snapshot.ObservedSiblingWeightSum)
	setOptionalGauge(m.rootPoints, snapshot.ConfiguredRootPoints)
	setOptionalGauge(m.parentPeriodsDelta, snapshot.ParentCPUPeriodsDelta)
	setOptionalGauge(m.parentThrottledDelta, snapshot.ParentCPUThrottledPeriodsDelta)
	setOptionalGauge(m.parentThrottledUsecDelta, snapshot.ParentCPUThrottledUsecDelta)
	if snapshot.IntervalStart == nil || snapshot.IntervalEnd.IsZero() || !snapshot.IntervalEnd.After(*snapshot.IntervalStart) {
		m.intervalSeconds.DeleteLabelValues()
	} else {
		m.intervalSeconds.WithLabelValues().Set(snapshot.IntervalEnd.Sub(*snapshot.IntervalStart).Seconds())
	}
	for _, state := range []CPUPointsDeliveryState{CPUPointsDeliveryUnavailable, CPUPointsDeliveryAvailable, CPUPointsDeliveryThrottledParent} {
		m.deliveryState.DeleteLabelValues(string(state))
	}
	if validCPUPointsDeliveryState(snapshot.DeliveryState) {
		m.deliveryState.WithLabelValues(string(snapshot.DeliveryState)).Set(1)
	}
	for _, state := range []CPUPointsDenominatorState{CPUPointsDenominatorUnavailable, CPUPointsDenominatorInactive, CPUPointsDenominatorComplete, CPUPointsDenominatorIncomplete} {
		m.denominatorState.DeleteLabelValues(string(state))
	}
	if validCPUPointsDenominatorState(snapshot.DenominatorState) {
		m.denominatorState.WithLabelValues(string(snapshot.DenominatorState)).Set(1)
	}
}

func (m *cpuPointsPrometheusMetrics) updateUser(uid, username string, snapshot CPUPointsUserSnapshot) {
	if snapshot.LifecycleState == "" {
		m.deleteUser(uid, username)
		return
	}
	m.deleteUserStateLabels(uid, username)
	setOptionalUserGauge(m.userProgrammedWeight, uid, username, snapshot.AppliedWeight)
	setOptionalUserGauge(m.userObservedWeight, uid, username, snapshot.ObservedWeight)
	for _, coverage := range []string{"complete", "partial", "refused", "unavailable"} {
		m.userIOCoverage.DeleteLabelValues(uid, username, coverage)
	}
	if snapshot.IOCoverage != nil && validRAMCgroupCoverage(*snapshot.IOCoverage) {
		m.userIOCoverage.WithLabelValues(uid, username, *snapshot.IOCoverage).Set(1)
	}
	if validCPUPointsClass(snapshot.ConfiguredClass) {
		m.userConfiguredClass.WithLabelValues(uid, username, snapshot.ConfiguredClass).Set(1)
	}
	setOptionalUserGauge(m.userConfiguredGuarantee, uid, username, snapshot.ConfiguredGuaranteePoints)
	m.userRequested.WithLabelValues(uid, username).Set(boolMetricValue(snapshot.CPUEnforcementRequested))
	if validCPUPointsLifecycleState(snapshot.LifecycleState) {
		m.userLifecycle.WithLabelValues(uid, username, string(snapshot.LifecycleState)).Set(1)
	}
	if snapshot.AppliedClass != nil && snapshot.AppliedToProcesses && validCPUPointsClass(*snapshot.AppliedClass) {
		m.userAppliedClass.WithLabelValues(uid, username, *snapshot.AppliedClass).Set(1)
	}
	if snapshot.AppliedToProcesses {
		setOptionalUserGauge(m.userAppliedWeight, uid, username, snapshot.AppliedWeight)
	} else {
		m.userAppliedWeight.DeleteLabelValues(uid, username)
	}
	m.userAppliedToProcesses.WithLabelValues(uid, username).Set(boolMetricValue(snapshot.AppliedToProcesses))
	m.userCompleteUIDGuaranteed.WithLabelValues(uid, username).Set(boolMetricValue(snapshot.CompleteUIDWorkloadGuaranteed))
	m.userReconciliationDegraded.WithLabelValues(uid, username).Set(boolMetricValue(snapshot.ReconciliationDegraded))
	if validCPUPointsProcessCoverage(snapshot.ProcessCoverage) {
		m.userProcessCoverage.WithLabelValues(uid, username, string(snapshot.ProcessCoverage)).Set(1)
	}
	m.userObservedProcesses.WithLabelValues(uid, username).Set(float64(snapshot.ObservedProcessCount))
	m.userEnforceableProcesses.WithLabelValues(uid, username).Set(float64(snapshot.EnforceableProcessCount))
	if snapshot.ProcessObservationUnavailable {
		m.userObservedProcesses.DeleteLabelValues(uid, username)
		m.userEnforceableProcesses.DeleteLabelValues(uid, username)
	}
	setOptionalUserGauge(m.userLeafUsageUsecDelta, uid, username, snapshot.LeafCPUUsageUsecDelta)
	setOptionalUserGauge(m.userRAMCgroupUsage, uid, username, snapshot.RAMCgroupUsageBytes)
	for _, coverage := range []string{"complete", "partial", "refused", "unavailable"} {
		m.userRAMCoverage.DeleteLabelValues(uid, username, coverage)
	}
	if snapshot.RAMCoverage != nil && validRAMCgroupCoverage(*snapshot.RAMCoverage) {
		m.userRAMCoverage.WithLabelValues(uid, username, *snapshot.RAMCoverage).Set(1)
	}
	m.userRAMIncompleteProcesses.WithLabelValues(uid, username).Set(float64(snapshot.RAMCoverageIncompleteProcessCount))
	setOptionalUserGauge(m.userMemoryHighEventsDelta, uid, username, snapshot.MemoryHighEventsDelta)
	setOptionalUserGauge(m.userMemoryMaxEventsDelta, uid, username, snapshot.MemoryMaxEventsDelta)
	setOptionalUserGauge(m.userMemoryOOMEventsDelta, uid, username, snapshot.MemoryOOMEventsDelta)
	setOptionalUserGauge(m.userMemoryOOMKillsDelta, uid, username, snapshot.MemoryOOMKillEventsDelta)
}

func (m *cpuPointsPrometheusMetrics) deleteUserStateLabels(uid, username string) {
	for _, class := range []string{"guaranteed", "best_effort", "root"} {
		m.userConfiguredClass.DeleteLabelValues(uid, username, class)
		m.userAppliedClass.DeleteLabelValues(uid, username, class)
	}
	for _, state := range []CPUPointsLifecycleState{CPUPointsLifecycleIneligible, CPUPointsLifecycleEligibleInactive, CPUPointsLifecycleApplied, CPUPointsLifecycleFailed, CPUPointsLifecycleReleased} {
		m.userLifecycle.DeleteLabelValues(uid, username, string(state))
	}
	for _, coverage := range []CPUPointsProcessCoverage{CPUPointsCoverageUnavailable, CPUPointsCoverageNone, CPUPointsCoverageComplete, CPUPointsCoveragePartial} {
		m.userProcessCoverage.DeleteLabelValues(uid, username, string(coverage))
	}
}

func (m *cpuPointsPrometheusMetrics) deleteUser(uid, username string) {
	m.deleteUserStateLabels(uid, username)
	for _, metric := range []*prometheus.GaugeVec{
		m.userConfiguredGuarantee, m.userRequested, m.userAppliedWeight,
		m.userObservedWeight, m.userProgrammedWeight,
		m.userAppliedToProcesses, m.userCompleteUIDGuaranteed, m.userReconciliationDegraded,
		m.userObservedProcesses, m.userEnforceableProcesses,
		m.userLeafUsageUsecDelta, m.userRAMCgroupUsage,
		m.userRAMIncompleteProcesses, m.userMemoryHighEventsDelta, m.userMemoryMaxEventsDelta,
		m.userMemoryOOMEventsDelta, m.userMemoryOOMKillsDelta,
	} {
		metric.DeleteLabelValues(uid, username)
	}
	for _, coverage := range []string{"complete", "partial", "refused", "unavailable"} {
		m.userRAMCoverage.DeleteLabelValues(uid, username, coverage)
		m.userIOCoverage.DeleteLabelValues(uid, username, coverage)
	}
}

func setOptionalGauge(gauge *prometheus.GaugeVec, value *uint64) {
	if value == nil {
		gauge.DeleteLabelValues()
		return
	}
	gauge.WithLabelValues().Set(float64(*value))
}

func setOptionalUserGauge(gauge *prometheus.GaugeVec, uid, username string, value *uint64) {
	if value == nil {
		gauge.DeleteLabelValues(uid, username)
		return
	}
	gauge.WithLabelValues(uid, username).Set(float64(*value))
}

func validCPUPointsClass(class string) bool {
	return class == "guaranteed" || class == "best_effort" || class == "root"
}

func validCPUPointsDeliveryState(state CPUPointsDeliveryState) bool {
	return state == CPUPointsDeliveryUnavailable || state == CPUPointsDeliveryAvailable || state == CPUPointsDeliveryThrottledParent
}

func validCPUPointsDenominatorState(state CPUPointsDenominatorState) bool {
	return state == CPUPointsDenominatorUnavailable || state == CPUPointsDenominatorInactive || state == CPUPointsDenominatorComplete || state == CPUPointsDenominatorIncomplete
}

func validCPUPointsLifecycleState(state CPUPointsLifecycleState) bool {
	switch state {
	case CPUPointsLifecycleIneligible, CPUPointsLifecycleEligibleInactive, CPUPointsLifecycleApplied,
		CPUPointsLifecycleFailed, CPUPointsLifecycleReleased:
		return true
	default:
		return false
	}
}

func validCPUPointsProcessCoverage(coverage CPUPointsProcessCoverage) bool {
	return coverage == CPUPointsCoverageUnavailable || coverage == CPUPointsCoverageNone || coverage == CPUPointsCoverageComplete || coverage == CPUPointsCoveragePartial
}

func validRAMCgroupCoverage(coverage string) bool {
	return coverage == "complete" || coverage == "partial" || coverage == "refused" || coverage == "unavailable"
}
