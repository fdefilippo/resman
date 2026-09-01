package state

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
)

const normalCPUQuota = "max 100000"

type idleReleaseResult struct {
	uid      int
	released bool
	err      error
}

func (m *Manager) ensureCPUPointsHierarchy() (cgroup.CPUPointsHierarchy, error) {
	capacity, err := m.cpuCapacity.Refresh(m.cpuPointsPolicy.Pool())
	if err != nil {
		return cgroup.CPUPointsHierarchy{}, fmt.Errorf("refresh authoritative CPU Points capacity: %w", err)
	}
	bestEffortWeight, err := m.cpuPointsPolicy.BestEffort().KernelWeight()
	if err != nil {
		return cgroup.CPUPointsHierarchy{}, fmt.Errorf("convert best-effort points to kernel weight: %w", err)
	}

	m.mu.RLock()
	hierarchy := m.cpuPointsHierarchy
	m.mu.RUnlock()
	if hierarchy.Parent == "" {
		hierarchy, err = m.cgroupManager.EnsureCPUPointsHierarchy(capacity.LastVerified, bestEffortWeight)
		if err != nil {
			return cgroup.CPUPointsHierarchy{}, err
		}
		m.mu.Lock()
		m.cpuPointsHierarchy = hierarchy
		m.sharedCgroupPath = hierarchy.Parent
		m.programmedGuaranteePoints = 1
		m.mu.Unlock()
		return hierarchy, nil
	}
	if err := m.cgroupManager.ApplyCPUPointsParentQuota(hierarchy, capacity.LastVerified); err != nil {
		return cgroup.CPUPointsHierarchy{}, err
	}
	return hierarchy, nil
}

func (m *Manager) cpuPointsAllocationFor(uid int) (cpupoints.AllocationClass, cpupoints.KernelCPUWeight, uint64, error) {
	if guarantee, ok := m.cpuPointsPolicy.GuaranteeForUID(uid); ok {
		weight, err := guarantee.Points().KernelWeight()
		return cpupoints.AllocationClassGuaranteed, weight, guarantee.Points().Value(), err
	}
	weight, err := cpupoints.NewKernelCPUWeight(100)
	return cpupoints.AllocationClassBestEffort, weight, 0, err
}

func (m *Manager) admitCPUPointsUser(uid int, hierarchy cgroup.CPUPointsHierarchy) (path string, result cgroup.ProcessMoveResult, err error) {
	defer func() {
		m.recordCPUPointsAdmissionOutcome(uid, result, err)
	}()
	class, leafWeight, guarantee, err := m.cpuPointsAllocationFor(uid)
	if err != nil {
		return "", cgroup.ProcessMoveResult{}, err
	}
	domainPath, err := hierarchy.DomainPath(class)
	if err != nil {
		return "", cgroup.ProcessMoveResult{}, err
	}
	m.mu.RLock()
	resources := m.resourceLimits[uid]
	_, alreadyAllocated := m.cpuAllocations[uid]
	m.mu.RUnlock()
	if !alreadyAllocated && resources.ramApplied && resources.standalone {
		return "", cgroup.ProcessMoveResult{}, &RAMActiveCPUTransitionError{
			UID:  uid,
			From: "standalone",
			To:   string(class),
		}
	}

	if class == cpupoints.AllocationClassGuaranteed {
		m.mu.RLock()
		current := m.appliedGuaranteePoints.Value()
		programmed := m.programmedGuaranteePoints
		m.mu.RUnlock()
		target := current + guarantee
		if programmed > target {
			target = programmed
		}
		next, err := cpupoints.NewAppliedGuaranteePoints(target)
		if err != nil {
			return "", cgroup.ProcessMoveResult{}, fmt.Errorf("compute acquired guarantee for UID %d: %w", uid, err)
		}
		domainWeight, err := next.KernelWeight()
		if err != nil {
			return "", cgroup.ProcessMoveResult{}, err
		}
		// Raising the aggregate first is mandatory: a participating leaf must
		// never observe an underweight guaranteed domain.
		m.mu.Lock()
		if target > m.programmedGuaranteePoints {
			// Record the conservative requested high-water mark before the
			// write. If write verification fails after the kernel accepted the
			// value, a later admission must not lower it accidentally.
			m.programmedGuaranteePoints = target
		}
		m.mu.Unlock()
		if err := m.cgroupManager.ApplyCPUPointsGuaranteedWeight(hierarchy, domainWeight); err != nil {
			return "", cgroup.ProcessMoveResult{}, err
		}
	}

	path, result, err = m.cgroupManager.EnsureCPUPointsUserPlacement(uid, domainPath, leafWeight)
	m.recordCgroupIngressSkips(result)
	if err == nil && !result.Applied() {
		err = cgroupIngressNoopError(uid, result)
	}
	if err != nil {
		// A prior domain-weight increase is intentionally retained: overweighting
		// guaranteed users is conservative, while rolling it back could race a
		// partially completed ingress and violate a guarantee.
		return "", result, err
	}
	m.setStandaloneResourceCgroup(uid, false)

	m.mu.Lock()
	m.cpuAllocations[uid] = cpuPointsAllocation{
		class: class, weight: leafWeight, domainPath: domainPath, leafPath: path,
		pidNamespaceMismatches: result.PIDNamespaceMismatches, pidNamespaceUnavailable: result.PIDNamespaceUnavailable,
	}
	m.recordPartialRAMCoverageLocked(uid, result.MovedProcesses)
	if class == cpupoints.AllocationClassGuaranteed {
		next, _ := cpupoints.NewAppliedGuaranteePoints(m.appliedGuaranteePoints.Value() + guarantee)
		m.appliedGuaranteePoints = next
	}
	m.mu.Unlock()
	return path, result, nil
}

func (m *Manager) releaseCPUPointsUser(uid int, preserveObservation bool) (released bool, err error) {
	m.mu.RLock()
	allocation, exists := m.cpuAllocations[uid]
	hierarchy := m.cpuPointsHierarchy
	resources := m.resourceLimits[uid]
	m.mu.RUnlock()
	if exists {
		defer func() {
			m.recordCPUPointsReleaseOutcome(uid, released, err)
		}()
	}
	if !exists {
		m.mu.RLock()
		sharedPath := m.sharedCgroupPath
		m.mu.RUnlock()
		if sharedPath == "" {
			return true, nil
		}
		if preserveObservation {
			_, result, err := m.cgroupManager.EnsureUserCgroupPlacement(uid, "", normalCPUQuota)
			m.recordCgroupIngressSkips(result)
			return err == nil, err
		}
		err := m.cgroupManager.ReleaseUserFromSharedCgroup(uid, sharedPath, normalCPUQuota)
		return err == nil, err
	}
	if resources.ramApplied {
		destination := "origin"
		if preserveObservation {
			destination = "standalone"
		}
		return false, &RAMActiveCPUTransitionError{
			UID:  uid,
			From: string(allocation.class),
			To:   destination,
		}
	}

	var releaseErr error
	if preserveObservation {
		_, result, err := m.cgroupManager.EnsureUserCgroupPlacement(uid, "", normalCPUQuota)
		m.recordCgroupIngressSkips(result)
		releaseErr = err
		if releaseErr == nil && result.NamespaceSkipped() > 0 && !result.Applied() {
			releaseErr = cgroupIngressNoopError(uid, result)
		}
	} else {
		releaseErr = m.cgroupManager.ReleaseCPUPointsUser(uid, allocation.domainPath)
	}
	if releaseErr != nil {
		return false, releaseErr
	}

	m.mu.Lock()
	delete(m.cpuAllocations, uid)
	delete(m.ramCoverage, uid)
	current := m.appliedGuaranteePoints.Value()
	if allocation.class == cpupoints.AllocationClassGuaranteed {
		current -= uint64(allocation.weight.Value())
		next, _ := cpupoints.NewAppliedGuaranteePoints(current)
		m.appliedGuaranteePoints = next
	}
	m.mu.Unlock()

	if allocation.class != cpupoints.AllocationClassGuaranteed {
		return true, nil
	}
	weightValue := int(current)
	if weightValue == 0 {
		weightValue = 1
	}
	weight, err := cpupoints.NewKernelCPUWeight(weightValue)
	if err != nil {
		return true, err
	}
	// The leaf is gone before the aggregate is lowered. A failed lowering leaves
	// a conservative overweight domain and is still reported to the caller.
	if err := m.cgroupManager.ApplyCPUPointsGuaranteedWeight(hierarchy, weight); err != nil {
		return true, err
	}
	m.mu.Lock()
	m.programmedGuaranteePoints = current
	if current == 0 {
		m.programmedGuaranteePoints = 1
	}
	m.mu.Unlock()
	return true, nil
}

func (m *Manager) recordPartialRAMCoverageLocked(uid int, processes []cgroup.ProcessReference) {
	if len(processes) == 0 {
		return
	}
	state := m.ramCoverage[uid]
	if state.partial == nil {
		state.partial = make(map[int]uint64)
	}
	for _, process := range processes {
		state.partial[process.PID] = process.StartTime
	}
	state.coverage = RAMCoveragePartial
	m.ramCoverage[uid] = state
}

func (m *Manager) recordCgroupIngressSkips(result cgroup.ProcessMoveResult) {
	if m.prometheusExporter != nil && result.NamespaceSkipped() > 0 {
		m.prometheusExporter.RecordCgroupIngressSkips(result)
	}
}

func cgroupIngressNoopError(uid int, result cgroup.ProcessMoveResult) error {
	return fmt.Errorf(
		"no process entered the ResMan cgroup for UID %d (candidates=%d, pid_namespace_mismatch=%d, pid_namespace_unavailable=%d)",
		uid,
		result.Candidates,
		result.PIDNamespaceMismatches,
		result.PIDNamespaceUnavailable,
	)
}

func (m *Manager) setStandaloneResourceCgroup(uid int, active bool) {
	m.mu.Lock()
	state := m.resourceLimits[uid]
	state.standalone = active
	if !active && !state.ram && !state.io && !state.ramApplied && !state.ioApplied && !state.swap {
		delete(m.resourceLimits, uid)
	} else {
		m.resourceLimits[uid] = state
	}
	m.mu.Unlock()
}

func (m *Manager) provisionStandaloneResourceUser(uid int, cfg *config.Config, eligibility config.UserEligibility) (bool, error) {
	wantRAM := cfg.RAMEnabled && eligibility.EligibleForRAM
	wantIO := cfg.IOEnabled && eligibility.EligibleForIO
	m.setResourceLimitIntent(uid, wantRAM, wantIO)
	if !wantRAM && !wantIO {
		return false, nil
	}

	if err := m.cgroupManager.CreateUserCgroup(uid); err != nil {
		return false, fmt.Errorf("create standalone resource cgroup for UID %d: %w", uid, err)
	}
	if err := m.cgroupManager.EnsureUnlimitedCPUQuota(uid); err != nil {
		cleanupErr := m.cgroupManager.CleanupUserCgroup(uid)
		return false, errors.Join(
			fmt.Errorf("ensure unlimited CPU quota for standalone resource cgroup UID %d: %w", uid, err),
			cleanupErr,
		)
	}
	moveResult, err := m.cgroupManager.MoveAllUserProcesses(uid)
	m.recordCgroupIngressSkips(moveResult)
	if err != nil {
		cleanupErr := m.cgroupManager.CleanupUserCgroup(uid)
		return false, errors.Join(
			fmt.Errorf("move processes to standalone resource cgroup for UID %d: %w", uid, err),
			cleanupErr,
		)
	}
	if !moveResult.Applied() {
		cleanupErr := m.cgroupManager.CleanupUserCgroup(uid)
		return false, errors.Join(cgroupIngressNoopError(uid, moveResult), cleanupErr)
	}
	m.setStandaloneResourceCgroup(uid, true)

	ramErr := m.applyRAMResourceLimit(uid, cfg, cfg.RAMQuotaPerUser, eligibility.EligibleForRAM)
	ioErr := m.applyIOResourceLimit(uid, cfg, eligibility.EligibleForIO)
	m.mu.RLock()
	state := m.resourceLimits[uid]
	m.mu.RUnlock()
	if !state.ramApplied && !state.ioApplied {
		cleanupErr := m.cgroupManager.CleanupUserCgroup(uid)
		m.mu.Lock()
		delete(m.resourceLimits, uid)
		m.mu.Unlock()
		m.setResourceLimitIntent(uid, wantRAM, wantIO)
		return false, errors.Join(ramErr, ioErr, cleanupErr, fmt.Errorf("standalone resource cgroup for UID %d has no applied RAM or IO limit", uid))
	}

	return true, errors.Join(ramErr, ioErr)
}

func (m *Manager) hasObservedEnforcementLocked() bool {
	return len(m.activeUsers) > 0 || m.hasObservedResourceEnforcementLocked()
}

func (m *Manager) hasObservedResourceEnforcementLocked() bool {
	for _, state := range m.resourceLimits {
		if state.ramApplied || state.ioApplied {
			return true
		}
	}
	return false
}

func (m *Manager) refreshResourceLimitsActiveLocked(now time.Time) {
	active := m.hasObservedResourceEnforcementLocked()
	if active && !m.resourceLimitsActive {
		m.resourceLimitsAppliedTime = now
	}
	if !active {
		m.resourceLimitsAppliedTime = time.Time{}
	}
	m.resourceLimitsActive = active
}

func (m *Manager) refreshCPULimitsActiveLocked(now time.Time) (activated, deactivated bool) {
	active := len(m.activeUsers) > 0
	activated = active && !m.limitsActive
	deactivated = !active && m.limitsActive
	if activated {
		m.limitsAppliedTime = now
	}
	if !active {
		m.limitsAppliedTime = time.Time{}
	}
	m.limitsActive = active
	return activated, deactivated
}

func (m *Manager) recordCPUTransitions(activated, deactivated bool) {
	if m.prometheusExporter == nil {
		return
	}
	if activated {
		m.prometheusExporter.IncrementCPULimitsActivated()
	}
	if deactivated {
		m.prometheusExporter.IncrementCPULimitsDeactivated()
	}
}

func (m *Manager) reconcileStandaloneResourceUsers(metrics *SystemMetrics, cfg *config.Config) error {
	desired := make(map[int]config.UserEligibility)
	ramEnabled := cfg.RAMEnabled
	ioEnabled := cfg.IOEnabled
	m.mu.RLock()
	for uid, userMetrics := range metrics.UserMetrics {
		if userMetrics == nil || m.activeUsers[uid] {
			continue
		}
		eligibility := config.UserEligibility{
			EligibleForCPU: userMetrics.EligibleForCPU,
			EligibleForRAM: userMetrics.EligibleForRAM,
			EligibleForIO:  userMetrics.EligibleForIO,
		}
		if (ramEnabled && eligibility.EligibleForRAM) || (ioEnabled && eligibility.EligibleForIO) {
			desired[uid] = eligibility
		}
	}
	standalone := make(map[int]userResourceLimitState)
	for uid, state := range m.resourceLimits {
		if state.standalone {
			standalone[uid] = state
		}
	}
	m.mu.RUnlock()

	var reconcileErrors []error
	for uid := range standalone {
		eligibility, keep := desired[uid]
		if keep {
			if err := m.reconcileUserResourceLimits(uid, cfg, eligibility); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
			delete(desired, uid)
			continue
		}

		m.setResourceLimitIntent(uid, false, false)
		if err := m.cgroupManager.CleanupUserCgroup(uid); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("cleanup standalone resource cgroup for UID %d: %w", uid, err))
			continue
		}
		m.mu.Lock()
		delete(m.resourceLimits, uid)
		m.mu.Unlock()
	}

	for uid, eligibility := range desired {
		if _, err := m.provisionStandaloneResourceUser(uid, cfg, eligibility); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}
	m.mu.Lock()
	m.refreshResourceLimitsActiveLocked(time.Now())
	m.mu.Unlock()
	return errors.Join(reconcileErrors...)
}

func (m *Manager) releaseIdleUsers(metrics *SystemMetrics) error {
	cfg := m.GetConfig()
	var reconcileErrors []error
	m.mu.RLock()
	limitsActive := m.limitsActive || m.resourceLimitsActive
	sharedPath := m.sharedCgroupPath
	m.mu.RUnlock()
	if !limitsActive {
		return nil // No active enforcement needs reconciliation.
	}
	if sharedPath != "" {
		if _, err := m.ensureCPUPointsHierarchy(); err != nil {
			return fmt.Errorf("failed to reconcile CPU Points hierarchy: %w", err)
		}
	}

	const idleThreshold = 0.1
	now := time.Now()
	minActiveTime := time.Duration(cfg.GetMinActiveTime()) * time.Second
	eligible := make(map[int]bool, len(metrics.CPUEligibleUsers))
	for _, uid := range metrics.CPUEligibleUsers {
		eligible[uid] = true
	}

	m.mu.Lock()
	usersToRelease := make([]int, 0)
	usersToAdd := make([]int, 0)

	for uid := range m.activeUsers {
		if _, userStillActive := metrics.UserCPUUsage[uid]; !userStillActive {
			usersToRelease = append(usersToRelease, uid)
			continue
		}
		if !eligible[uid] {
			usersToRelease = append(usersToRelease, uid)
			continue
		}
		cpuEMA, ok := userCPUEMA(metrics, uid)
		if !ok || cpuEMA >= idleThreshold {
			continue
		}
		limitedAt := m.userLimitedAt[uid]
		if !limitedAt.IsZero() && now.Sub(limitedAt) < minActiveTime {
			continue
		}
		usersToRelease = append(usersToRelease, uid)
	}

	for uid := range eligible {
		if _, active := m.activeUsers[uid]; active {
			continue
		}
		if cpuEMA, ok := userCPUEMA(metrics, uid); ok && cpuEMA >= idleThreshold {
			usersToAdd = append(usersToAdd, uid)
		}
	}
	if m.requestedCPUUsers == nil {
		m.requestedCPUUsers = make(map[int]bool)
	}
	for _, uid := range usersToRelease {
		delete(m.requestedCPUUsers, uid)
	}
	for _, uid := range usersToAdd {
		m.requestedCPUUsers[uid] = true
	}
	m.mu.Unlock()
	if len(usersToAdd) > 0 && sharedPath == "" {
		hierarchy, err := m.ensureCPUPointsHierarchy()
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("create CPU Points hierarchy during maintenance: %w", err))
		} else {
			sharedPath = hierarchy.Parent
		}
	}

	for _, uid := range usersToRelease {
		if err := m.reconcileUserResourceLimits(uid, cfg, userEligibilityFromMetrics(metrics, uid)); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}

	results := m.releaseTrackedUsers(usersToRelease)
	releasedUsers := make([]int, 0, len(usersToRelease))
	for _, result := range results {
		if result.released {
			releasedUsers = append(releasedUsers, result.uid)
		}
		if result.err != nil {
			m.logger.Warn("Failed to release idle user from shared cgroup",
				"uid", result.uid, "shared_path", sharedPath, "error", result.err)
			reconcileErrors = append(reconcileErrors, result.err)
			continue
		}
	}

	m.commitReleasedUsers(releasedUsers)
	m.mu.RLock()
	remainingLimited := len(m.activeUsers)
	m.mu.RUnlock()

	if len(usersToRelease) > 0 {
		m.logger.Info("Idle user release completed",
			"users_released", len(releasedUsers),
			"release_failures", len(usersToRelease)-len(releasedUsers),
			"users_still_limited", remainingLimited,
			"idle_threshold", idleThreshold,
			"idle_hold_seconds", cfg.GetMinActiveTime(),
		)
	}

	if len(usersToAdd) > 0 && sharedPath != "" {
		var added []int
		for _, uid := range usersToAdd {
			username := m.metricsCollector.GetUsernameFromUID(uid)
			m.logger.Info("Re-adding user to shared cgroup (CPU usage recovered)",
				"uid", uid, "username", username,
				"cpu", userEnforceableCPUUsage(metrics, uid),
			)

			m.mu.RLock()
			hierarchy := m.cpuPointsHierarchy
			m.mu.RUnlock()
			userCgroupPath, _, err := m.admitCPUPointsUser(uid, hierarchy)
			if err != nil {
				m.logger.Warn("Failed to move processes for re-added user; user will not be marked limited",
					"uid", uid, "error", err)
				reconcileErrors = append(reconcileErrors, err)
				continue
			}
			if err := m.applyUserResourceLimits(uid, cfg, userEligibilityFromMetrics(metrics, uid)); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}

			if m.psiWatcher != nil {
				cpuPressurePath := filepath.Join(userCgroupPath, "cpu.pressure")
				ioPressurePath := filepath.Join(userCgroupPath, "io.pressure")
				if err := m.psiWatcher.AddMonitor(uid, "cpu", cpuPressurePath); err != nil {
					m.logger.Warn("Failed to add CPU pressure monitor for re-added user",
						"uid", uid, "path", cpuPressurePath, "error", err)
				}
				if err := m.psiWatcher.AddMonitor(uid, "io", ioPressurePath); err != nil {
					m.logger.Warn("Failed to add IO pressure monitor for re-added user",
						"uid", uid, "path", ioPressurePath, "error", err)
				}
			}

			added = append(added, uid)
		}

		if len(added) > 0 {
			readdedAt := time.Now()
			m.mu.Lock()
			for _, uid := range added {
				m.activeUsers[uid] = true
				m.userLimitedAt[uid] = readdedAt
			}
			m.mu.Unlock()
		}
	}

	m.mu.RLock()
	activeUsers := make([]int, 0, len(m.activeUsers))
	for uid := range m.activeUsers {
		if eligible[uid] {
			activeUsers = append(activeUsers, uid)
		}
	}
	m.mu.RUnlock()
	for _, uid := range activeUsers {
		if err := m.reconcileUserResourceLimits(uid, cfg, userEligibilityFromMetrics(metrics, uid)); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}
	if err := m.reconcileStandaloneResourceUsers(metrics, cfg); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}
	if err := m.reconcileActiveProcessMembership(cfg); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}

	now = time.Now()
	m.mu.Lock()
	cpuActivated, cpuDeactivated := m.refreshCPULimitsActiveLocked(now)
	m.mu.Unlock()
	m.recordCPUTransitions(cpuActivated, cpuDeactivated)
	if cpuActivated && m.stabilityTracker != nil {
		m.stabilityTracker.Reset()
	}
	_ = cpuDeactivated

	return errors.Join(reconcileErrors...)
}

type activeProcessMembership struct {
	uid        int
	sharedPath string
}

func (m *Manager) reconcileActiveProcessMembership(cfg *config.Config) error {
	m.mu.RLock()
	targets := make([]activeProcessMembership, 0, len(m.activeUsers)+len(m.resourceLimits))
	tracked := make(map[int]bool, len(m.activeUsers))
	for uid := range m.activeUsers {
		allocation := m.cpuAllocations[uid]
		targetPath := allocation.domainPath
		if targetPath == "" {
			targetPath = m.sharedCgroupPath
		}
		targets = append(targets, activeProcessMembership{uid: uid, sharedPath: targetPath})
		tracked[uid] = true
	}
	for uid, state := range m.resourceLimits {
		if state.standalone && (state.ramApplied || state.ioApplied) && !tracked[uid] {
			targets = append(targets, activeProcessMembership{uid: uid})
		}
	}
	m.mu.RUnlock()

	sort.Slice(targets, func(i, j int) bool { return targets[i].uid < targets[j].uid })
	var reconcileErrors []error
	for _, target := range targets {
		result, err := m.cgroupManager.ReconcileUserProcessMembership(
			target.uid,
			target.sharedPath,
			normalCPUQuota,
		)
		m.recordCgroupIngressSkips(result.Ingress)
		m.mu.Lock()
		m.recordPartialRAMCoverageLocked(target.uid, result.Ingress.MovedProcesses)
		if allocation, ok := m.cpuAllocations[target.uid]; ok {
			allocation.pidNamespaceMismatches = result.Ingress.PIDNamespaceMismatches
			allocation.pidNamespaceUnavailable = result.Ingress.PIDNamespaceUnavailable
			m.cpuAllocations[target.uid] = allocation
		}
		m.mu.Unlock()
		if err == nil && result.Ingress.NamespaceSkipped() > 0 && !result.Ingress.Applied() {
			err = cgroupIngressNoopError(target.uid, result.Ingress)
		}
		if err != nil {
			if m.prometheusExporter != nil {
				errorType := processMembershipReconcileFailure
				var originUnavailable *cgroup.ProcessOriginUnavailableError
				if errors.As(err, &originUnavailable) {
					errorType = processMembershipOriginUnavailable
				}
				m.prometheusExporter.RecordError(processMembershipErrorComponent, errorType)
			}
			reconcileErrors = append(reconcileErrors, fmt.Errorf(
				"reconcile active process membership for UID %d: %w",
				target.uid,
				err,
			))
			continue
		}
		if result.MovedIn > 0 || result.RestoredExcluded > 0 || result.SkippedReused > 0 {
			m.logger.Info("Active process membership reconciled",
				"uid", target.uid,
				"shared_path", target.sharedPath,
				"processes_scanned", result.Scanned,
				"processes_moved_in", result.MovedIn,
				"excluded_processes_restored", result.RestoredExcluded,
				"reused_pids_skipped", result.SkippedReused,
			)
		}
	}
	return errors.Join(reconcileErrors...)
}

func userCPUEMA(metrics *SystemMetrics, uid int) (float64, bool) {
	if userMetrics, ok := metrics.UserMetrics[uid]; ok && userMetrics != nil {
		return userMetrics.EnforceableUsage.CPUUsageEMA, true
	}
	cpuUsage, ok := metrics.UserCPUUsage[uid]
	return cpuUsage, ok
}

func userEnforceableCPUUsage(metrics *SystemMetrics, uid int) float64 {
	if userMetrics, ok := metrics.UserMetrics[uid]; ok && userMetrics != nil {
		return userMetrics.EnforceableUsage.CPUUsage
	}
	return 0
}

func userEligibilityFromMetrics(metrics *SystemMetrics, uid int) config.UserEligibility {
	userMetrics := metrics.UserMetrics[uid]
	if userMetrics == nil {
		return config.UserEligibility{}
	}
	return config.UserEligibility{
		EligibleForCPU: userMetrics.EligibleForCPU,
		EligibleForRAM: userMetrics.EligibleForRAM,
		EligibleForIO:  userMetrics.EligibleForIO,
	}
}

func (m *Manager) releaseTrackedUsers(users []int) []idleReleaseResult {
	m.mu.RLock()
	observed := make(map[int]bool, len(users))
	for _, uid := range users {
		observed[uid] = m.blockIOObservedUsers[uid]
	}
	m.mu.RUnlock()
	releases := make([]idleReleaseResult, 0, len(users))
	sort.Ints(users)
	for _, uid := range users {
		released, err := m.releaseCPUPointsUser(uid, observed[uid])
		releases = append(releases, idleReleaseResult{uid: uid, released: released, err: err})
	}
	return releases
}

func (m *Manager) commitReleasedUsers(users []int) {
	m.mu.Lock()
	for _, uid := range users {
		delete(m.activeUsers, uid)
		delete(m.userLimitedAt, uid)
		delete(m.resourceLimits, uid)
		delete(m.cpuAllocations, uid)
	}
	m.mu.Unlock()

	for _, uid := range users {
		if m.psiWatcher != nil {
			m.psiWatcher.RemoveMonitor(uid, "cpu")
			m.psiWatcher.RemoveMonitor(uid, "io")
		}
	}
	if m.ioRemediation != nil {
		m.ioRemediation.ForgetUsers(users)
	}
	if m.stabilityTracker != nil {
		m.stabilityTracker.ForgetUsers(users)
	}
}

func (m *Manager) activateLimits(metrics *SystemMetrics) (resultErr error) {
	defer func() {
		if resultErr != nil && m.prometheusExporter != nil {
			m.prometheusExporter.RecordError(limitTransitionErrorComponent, limitTransitionActivationFailure)
		}
	}()

	cfg := m.GetConfig()

	// Snapshot the users that are currently limited.
	m.mu.RLock()
	previouslyLimited := make([]int, 0, len(m.activeUsers))
	for uid := range m.activeUsers {
		previouslyLimited = append(previouslyLimited, uid)
	}
	sharedPath := m.sharedCgroupPath
	m.mu.RUnlock()

	currentActiveSet := make(map[int]bool)
	for uid := range metrics.UserCPUUsage {
		currentActiveSet[uid] = true
	}
	eligibleSet := make(map[int]bool, len(metrics.CPUEligibleUsers))
	for _, uid := range metrics.CPUEligibleUsers {
		eligibleSet[uid] = true
	}
	m.mu.Lock()
	m.requestedCPUUsers = eligibleSet
	m.mu.Unlock()

	var firstError error
	limitedCount := 0

	usersToRelease := make([]int, 0)
	for _, uid := range previouslyLimited {
		if !currentActiveSet[uid] || !eligibleSet[uid] {
			usersToRelease = append(usersToRelease, uid)
			if err := m.reconcileUserResourceLimits(uid, cfg, userEligibilityFromMetrics(metrics, uid)); err != nil && firstError == nil {
				firstError = err
			}
		}
	}
	releaseResults := m.releaseTrackedUsers(usersToRelease)
	releasedUsers := make([]int, 0, len(usersToRelease))
	for _, result := range releaseResults {
		if result.released {
			releasedUsers = append(releasedUsers, result.uid)
		}
		if result.err != nil {
			m.logger.Warn("Failed to release ineligible user from shared cgroup",
				"uid", result.uid,
				"shared_path", sharedPath,
				"error", result.err,
			)
			if firstError == nil {
				firstError = result.err
			}
			continue
		}
	}
	m.commitReleasedUsers(releasedUsers)
	removedCount := len(releasedUsers)
	releasedSet := make(map[int]bool, len(releasedUsers))
	for _, uid := range releasedUsers {
		releasedSet[uid] = true
		eligibility := userEligibilityFromMetrics(metrics, uid)
		if _, err := m.provisionStandaloneResourceUser(uid, cfg, eligibility); err != nil {
			m.logger.Warn("Failed to preserve RAM or IO enforcement after CPU release", "uid", uid, "error", err)
			if firstError == nil {
				firstError = err
			}
		}
	}

	// Phase 2: create or configure the shared cgroup.
	var hierarchy cgroup.CPUPointsHierarchy
	if len(eligibleSet) > 0 {
		var err error
		hierarchy, err = m.ensureCPUPointsHierarchy()
		if err != nil {
			return fmt.Errorf("failed to establish CPU Points hierarchy: %w", err)
		}
		sharedPath = hierarchy.Parent
	}

	// Phase 3: configure user sub-cgroups for the current users.
	// CPUEligibleUsers was already filtered by CPU policy during collection.
	for uid := range eligibleSet {
		eligibility := userEligibilityFromMetrics(metrics, uid)
		m.setResourceLimitIntent(
			uid,
			cfg.RAMEnabled && eligibility.EligibleForRAM,
			cfg.IOEnabled && eligibility.EligibleForIO,
		)
		username := m.metricsCollector.GetUsernameFromUID(uid)
		userStr := fmt.Sprintf("%s(%d)", username, uid)
		// Check whether the user is already limited.
		m.mu.RLock()
		alreadyLimited := m.activeUsers[uid]
		m.mu.RUnlock()

		if !alreadyLimited {
			// Move directly from an observation cgroup when one exists. The cgroup
			// manager carries the logical block-I/O counter across the placement.
			userCgroupPath, _, err := m.admitCPUPointsUser(uid, hierarchy)
			if err != nil {
				m.logger.Error("Failed to place user in shared cgroup",
					"user", userStr,
					"shared_cgroup", sharedPath,
					"error", err,
				)
				if firstError == nil {
					firstError = err
				}
				continue
			}

			// Start PSI monitoring for adaptive boosting.
			if m.psiWatcher != nil {
				cpuPressurePath := filepath.Join(userCgroupPath, "cpu.pressure")
				ioPressurePath := filepath.Join(userCgroupPath, "io.pressure")
				if err := m.psiWatcher.AddMonitor(uid, "cpu", cpuPressurePath); err != nil {
					m.logger.Warn("Failed to monitor user cpu.pressure",
						"uid", uid, "path", cpuPressurePath, "error", err)
				}
				if err := m.psiWatcher.AddMonitor(uid, "io", ioPressurePath); err != nil {
					m.logger.Warn("Failed to monitor user io.pressure",
						"uid", uid, "path", ioPressurePath, "error", err)
				}
			}

			if err := m.applyUserResourceLimits(uid, cfg, eligibility); err != nil {
				m.logger.Warn("CPU enforcement applied with partial RAM or IO failure", "uid", uid, "error", err)
				if firstError == nil {
					firstError = err
				}
			}

			// Mark the user limited only after its processes were moved successfully.
			m.mu.Lock()
			m.activeUsers[uid] = true
			m.userLimitedAt[uid] = time.Now()
			m.mu.Unlock()

			limitedCount++
			m.notifyUserLimited(cfg, uid, username, metrics)

			m.logger.Debug("User configured in shared cgroup",
				"uid", uid,
				"allocation_class", m.cpuPointsPolicy.ClassForUID(uid),
				"shared_path", m.sharedCgroupPath,
			)
		}
	}

	standaloneCount := 0
	for uid := range metrics.UserCPUUsage {
		if eligibleSet[uid] || releasedSet[uid] {
			continue
		}
		eligibility := userEligibilityFromMetrics(metrics, uid)
		provisioned, err := m.provisionStandaloneResourceUser(uid, cfg, eligibility)
		if err != nil {
			m.logger.Warn("Failed to provision standalone RAM or IO enforcement", "uid", uid, "error", err)
			if firstError == nil {
				firstError = err
			}
		}
		if provisioned {
			standaloneCount++
		}
	}

	if limitedCount > 0 || standaloneCount > 0 || removedCount > 0 {
		now := time.Now()
		m.mu.Lock()
		cpuActivated, cpuDeactivated := m.refreshCPULimitsActiveLocked(now)
		m.refreshResourceLimitsActiveLocked(now)
		m.mu.Unlock()
		m.recordCPUTransitions(cpuActivated, cpuDeactivated)
		if cpuActivated && m.stabilityTracker != nil {
			m.stabilityTracker.Reset()
		}

		m.logger.Info("Resource limits reconciled",
			"cpu_limited_users", limitedCount,
			"standalone_resource_users", standaloneCount,
			"users_freed", removedCount,
			"total_active_users", len(metrics.UserCPUUsage),
			"shared_cgroup", m.sharedCgroupPath,
		)
	}
	if limitedCount == 0 && standaloneCount == 0 && removedCount == 0 && firstError == nil {
		firstError = fmt.Errorf("activation requested but no CPU, RAM, or IO enforcement was applied")
		m.logger.Warn("Activation requested but no resource enforcement was applied")
	}

	return firstError
}

func (m *Manager) applyUserResourceLimits(uid int, cfg *config.Config, eligibility config.UserEligibility) error {
	m.setResourceLimitIntent(
		uid,
		cfg.RAMEnabled && eligibility.EligibleForRAM,
		cfg.IOEnabled && eligibility.EligibleForIO,
	)

	ramQuota := cfg.RAMQuotaPerUser
	if cfg.GetAutodetectPatterns() && m.policyEngine != nil {
		if policy, exists := m.policyEngine.GetPolicy(uid); exists {
			if policy.RAMQuota != "" {
				ramQuota = policy.RAMQuota
			}
		}
	}
	ramErr := m.applyRAMResourceLimit(uid, cfg, ramQuota, eligibility.EligibleForRAM)
	ioErr := m.applyIOResourceLimit(uid, cfg, eligibility.EligibleForIO)
	return errors.Join(ramErr, ioErr)
}

func (m *Manager) applyRAMResourceLimit(uid int, cfg *config.Config, ramQuota string, eligible bool) error {
	if !cfg.RAMEnabled || !eligible {
		return nil
	}
	quotaBytes, err := config.ParseByteQuota(ramQuota)
	if err != nil || quotaBytes == 0 {
		return fmt.Errorf("invalid RAM quota %q for UID %d", ramQuota, uid)
	}

	m.setResourceLimitState(uid, true, false, cfg.DisableSwap, false)
	highBytes := uint64(float64(quotaBytes) * cfg.GetRAMHighRatio())
	highStr := strconv.FormatUint(highBytes, 10)
	if cfg.DisableSwap {
		if err := m.cgroupManager.ApplyRAMLimitWithHighAndSwapDisabled(uid, ramQuota, highStr); err != nil {
			return fmt.Errorf("apply RAM high and max limits with swap disabled for UID %d: %w", uid, err)
		}
		m.setResourceLimitState(uid, true, false, true, true)
		return nil
	}
	if err := m.cgroupManager.ApplyRAMLimitWithHigh(uid, ramQuota, highStr); err != nil {
		return fmt.Errorf("apply RAM high and max limits for UID %d: %w", uid, err)
	}
	m.setResourceLimitState(uid, true, false, false, true)
	return nil
}

func (m *Manager) applyIOResourceLimit(uid int, cfg *config.Config, eligible bool) error {
	if !cfg.IOEnabled || !eligible {
		return nil
	}
	readBPS := cfg.GetIOReadBPS()
	writeBPS := cfg.GetIOWriteBPS()
	readIOPS := cfg.GetIOReadIOPS()
	writeIOPS := cfg.GetIOWriteIOPS()
	deviceFilter := cfg.GetIODeviceFilter()

	m.setResourceLimitState(uid, false, true, false, false)
	if err := m.cgroupManager.ApplyIOLimit(uid, readBPS, writeBPS, readIOPS, writeIOPS, deviceFilter); err != nil {
		return fmt.Errorf("apply IO limit for UID %d: %w", uid, err)
	}
	m.setResourceLimitState(uid, false, true, false, true)
	m.logger.Debug("IO limit applied for user",
		"uid", uid,
		"readBPS", readBPS,
		"writeBPS", writeBPS,
	)
	return nil
}

func (m *Manager) reconcileUserResourceLimits(uid int, cfg *config.Config, eligibility config.UserEligibility) error {
	wantRAM := cfg.RAMEnabled && eligibility.EligibleForRAM
	wantIO := cfg.IOEnabled && eligibility.EligibleForIO
	previous := m.setResourceLimitIntent(uid, wantRAM, wantIO)
	var reconcileErrors []error
	if previous.ramApplied && !wantRAM {
		if err := m.removeTrackedResourceLimits(uid, true, false); err != nil {
			m.logger.Warn("Failed to reconcile RAM limits after configuration change",
				"uid", uid,
				"error", err,
			)
			reconcileErrors = append(reconcileErrors, err)
		}
	} else if previous.ramApplied && wantRAM && previous.swap && !cfg.DisableSwap {
		if err := m.removeTrackedRAMSwapLimit(uid); err != nil {
			m.logger.Warn("Failed to reconcile RAM swap limit after configuration change",
				"uid", uid,
				"error", err,
			)
			reconcileErrors = append(reconcileErrors, err)
		}
	} else if wantRAM && (!previous.ramApplied || (!previous.swap && cfg.DisableSwap)) {
		ramQuota := cfg.RAMQuotaPerUser
		if cfg.GetAutodetectPatterns() && m.policyEngine != nil {
			if policy, exists := m.policyEngine.GetPolicy(uid); exists && policy.RAMQuota != "" {
				ramQuota = policy.RAMQuota
			}
		}
		if err := m.applyRAMResourceLimit(uid, cfg, ramQuota, eligibility.EligibleForRAM); err != nil {
			m.logger.Warn("Failed to reconcile RAM limit after configuration change", "uid", uid, "error", err)
			reconcileErrors = append(reconcileErrors, err)
		}
	}

	if previous.ioApplied && !wantIO {
		if err := m.removeTrackedResourceLimits(uid, false, true); err != nil {
			m.logger.Warn("Failed to reconcile IO limits after configuration change",
				"uid", uid,
				"error", err,
			)
			reconcileErrors = append(reconcileErrors, err)
		}
	} else if wantIO && !previous.ioApplied {
		if err := m.applyIOResourceLimit(uid, cfg, eligibility.EligibleForIO); err != nil {
			m.logger.Warn("Failed to reconcile IO limit after configuration change", "uid", uid, "error", err)
			reconcileErrors = append(reconcileErrors, err)
		}
	}
	return errors.Join(reconcileErrors...)
}

func (m *Manager) removeTrackedResourceLimits(uid int, removeRAM, removeIO bool) error {
	var firstError error
	if removeRAM {
		m.mu.RLock()
		swapApplied := m.resourceLimits[uid].swap
		m.mu.RUnlock()
		highErr := m.cgroupManager.RemoveRAMHigh(uid)
		maxErr := m.cgroupManager.RemoveRAMLimit(uid)
		var swapErr error
		if swapApplied {
			swapErr = m.cgroupManager.RemoveRAMSwapLimit(uid)
		}
		if highErr != nil {
			firstError = fmt.Errorf("remove memory.high for UID %d: %w", uid, highErr)
		}
		if maxErr != nil && firstError == nil {
			firstError = fmt.Errorf("remove memory.max for UID %d: %w", uid, maxErr)
		}
		if swapErr != nil && firstError == nil {
			firstError = fmt.Errorf("remove memory.swap.max for UID %d: %w", uid, swapErr)
		}
		if highErr == nil && maxErr == nil && swapErr == nil {
			m.clearResourceLimitState(uid, true, false)
			m.logger.Debug("RAM limits removed for user", "uid", uid)
		}
	}
	if removeIO {
		if err := m.cgroupManager.RemoveIOLimit(uid); err != nil {
			if firstError == nil {
				firstError = fmt.Errorf("remove IO limit for UID %d: %w", uid, err)
			}
		} else {
			m.clearResourceLimitState(uid, false, true)
			if m.ioRemediation != nil {
				m.ioRemediation.ForgetUsers([]int{uid})
			}
			m.logger.Debug("IO limit removed for user", "uid", uid)
		}
	}
	return firstError
}

func (m *Manager) removeTrackedRAMSwapLimit(uid int) error {
	if err := m.cgroupManager.RemoveRAMSwapLimit(uid); err != nil {
		return fmt.Errorf("remove memory.swap.max for UID %d: %w", uid, err)
	}
	m.mu.Lock()
	state := m.resourceLimits[uid]
	state.swap = false
	m.resourceLimits[uid] = state
	m.mu.Unlock()
	return nil
}

func (m *Manager) setResourceLimitState(uid int, ram, io, swap, applied bool) {
	m.mu.Lock()
	if m.resourceLimits == nil {
		m.resourceLimits = make(map[int]userResourceLimitState)
	}
	state := m.resourceLimits[uid]
	if ram {
		state.ram = true
		state.ramApplied = applied
		state.swap = state.swap || swap
	}
	if io {
		state.io = true
		state.ioApplied = applied
	}
	m.resourceLimits[uid] = state
	m.mu.Unlock()
}

func (m *Manager) setResourceLimitIntent(uid int, ramRequested, ioRequested bool) userResourceLimitState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resourceLimits == nil {
		m.resourceLimits = make(map[int]userResourceLimitState)
	}
	previous := m.resourceLimits[uid]
	current := previous
	current.ram = ramRequested
	current.io = ioRequested
	if !current.ram && !current.io && !current.ramApplied && !current.ioApplied && !current.swap && !current.standalone {
		delete(m.resourceLimits, uid)
	} else {
		m.resourceLimits[uid] = current
	}
	return previous
}

func (m *Manager) clearResourceLimitState(uid int, ram, io bool) {
	m.mu.Lock()
	state := m.resourceLimits[uid]
	if ram {
		state.ram = false
		state.ramApplied = false
		state.swap = false
	}
	if io {
		state.io = false
		state.ioApplied = false
	}
	if !state.ram && !state.io && !state.standalone {
		delete(m.resourceLimits, uid)
	} else {
		m.resourceLimits[uid] = state
	}
	m.mu.Unlock()
}

func (m *Manager) deactivateLimits() (resultErr error) {
	defer func() {
		if resultErr != nil && m.prometheusExporter != nil {
			m.prometheusExporter.RecordError(limitTransitionErrorComponent, limitTransitionDeactivationFailure)
		}
	}()

	m.logger.Info("Deactivating resource limits")

	m.mu.Lock()
	m.requestedCPUUsers = make(map[int]bool)
	for uid, resources := range m.resourceLimits {
		resources.ram = false
		resources.io = false
		if !resources.ramApplied && !resources.ioApplied && !resources.swap && !resources.standalone {
			delete(m.resourceLimits, uid)
			continue
		}
		m.resourceLimits[uid] = resources
	}
	usersToCleanup := make([]int, 0, len(m.activeUsers))
	for uid := range m.activeUsers {
		usersToCleanup = append(usersToCleanup, uid)
	}
	standaloneToCleanup := make([]int, 0)
	for uid, resources := range m.resourceLimits {
		if resources.standalone {
			standaloneToCleanup = append(standaloneToCleanup, uid)
		}
	}

	// Preserve the attempted user count for the completion log.
	userCount := len(usersToCleanup) + len(standaloneToCleanup)

	sharedPath := m.sharedCgroupPath
	m.mu.Unlock()

	var firstError error
	deactivatedCount := 0
	deactivatedUsers := make(map[int]bool, len(usersToCleanup))

	// Remove limits for each tracked user.
	for _, uid := range usersToCleanup {
		username := m.metricsCollector.GetUsernameFromUID(uid)
		userStr := fmt.Sprintf("%s(%d)", username, uid)
		m.mu.RLock()
		appliedResources := m.resourceLimits[uid]
		observedForBlockIO := m.blockIOObservedUsers[uid]
		m.mu.RUnlock()

		if sharedPath != "" {
			if err := m.removeTrackedResourceLimits(uid, appliedResources.ramApplied, appliedResources.ioApplied); err != nil {
				m.logger.Warn("Failed to remove tracked resource limits before shared cgroup release",
					"user", userStr,
					"error", err,
				)
			}
			released, releaseErr := m.releaseCPUPointsUser(uid, observedForBlockIO)
			if releaseErr != nil {
				m.logger.Error("Failed to release user from shared cgroup",
					"user", userStr,
					"shared_cgroup", sharedPath,
					"error", releaseErr,
				)
				if firstError == nil {
					firstError = releaseErr
				}
				if !released {
					continue
				}
			}

			deactivatedUsers[uid] = true
			deactivatedCount++
			m.logger.Debug("User released from shared CPU limits",
				"uid", uid,
				"shared_cgroup", sharedPath,
			)
			continue
		}

		if firstError == nil {
			firstError = fmt.Errorf("active CPU Points user %d has no hierarchy", uid)
		}
	}

	standaloneDeactivated := make(map[int]bool, len(standaloneToCleanup))
	for _, uid := range standaloneToCleanup {
		m.mu.RLock()
		state := m.resourceLimits[uid]
		observedForBlockIO := m.blockIOObservedUsers[uid]
		m.mu.RUnlock()
		if observedForBlockIO {
			if err := m.removeTrackedResourceLimits(uid, state.ramApplied, state.ioApplied); err != nil {
				m.logger.Error("Failed to remove standalone resource limits while preserving block I/O observation", "uid", uid, "error", err)
				if firstError == nil {
					firstError = err
				}
				continue
			}
			standaloneDeactivated[uid] = true
			deactivatedCount++
			continue
		}
		if err := m.cgroupManager.CleanupUserCgroup(uid); err != nil {
			m.logger.Error("Failed to release standalone RAM or IO cgroup", "uid", uid, "error", err)
			if firstError == nil {
				firstError = err
			}
			continue
		}
		standaloneDeactivated[uid] = true
		deactivatedCount++
	}

	// Remove the shared cgroup when it exists.
	sharedRemoved := sharedPath == ""
	if sharedPath != "" && len(deactivatedUsers) == len(usersToCleanup) {
		m.mu.RLock()
		hierarchy := m.cpuPointsHierarchy
		m.mu.RUnlock()
		if err := m.cgroupManager.RemoveCPUPointsHierarchy(hierarchy); err != nil {
			m.logger.Warn("Failed to remove shared cgroup",
				"path", sharedPath,
				"error", err,
			)
			if firstError == nil {
				firstError = err
			}
		} else {
			sharedRemoved = true
			m.logger.Debug("Shared cgroup removed", "path", sharedPath)
		}
	}

	if m.psiWatcher != nil {
		for uid := range deactivatedUsers {
			m.psiWatcher.RemoveMonitor(uid, "cpu")
			m.psiWatcher.RemoveMonitor(uid, "io")
		}
	}

	fullyDeactivated := false
	confirmedDeactivation := false
	m.mu.Lock()
	wasActive := m.limitsActive
	for uid := range deactivatedUsers {
		delete(m.activeUsers, uid)
		delete(m.userLimitedAt, uid)
		delete(m.resourceLimits, uid)
	}
	for uid := range standaloneDeactivated {
		delete(m.resourceLimits, uid)
	}
	if len(m.activeUsers) == 0 {
		m.limitsActive = false
		m.limitsAppliedTime = time.Time{}
		confirmedDeactivation = wasActive
		if sharedRemoved {
			m.sharedCgroupPath = ""
			m.cpuPointsHierarchy = cgroup.CPUPointsHierarchy{}
		}
	}
	m.refreshResourceLimitsActiveLocked(time.Now())
	fullyDeactivated = !m.hasObservedEnforcementLocked()
	m.mu.Unlock()
	if confirmedDeactivation && m.prometheusExporter != nil {
		m.prometheusExporter.IncrementCPULimitsDeactivated()
	}
	if m.stabilityTracker != nil {
		if fullyDeactivated {
			m.stabilityTracker.Reset()
		} else {
			released := make([]int, 0, len(deactivatedUsers))
			for uid := range deactivatedUsers {
				released = append(released, uid)
			}
			m.stabilityTracker.ForgetUsers(released)
		}
	}
	if m.ioRemediation != nil {
		released := make([]int, 0, len(deactivatedUsers))
		for uid := range deactivatedUsers {
			released = append(released, uid)
		}
		m.ioRemediation.ForgetUsers(released)
	}

	if firstError != nil {
		m.logger.Warn("Resource limit deactivation incomplete",
			"users_freed", deactivatedCount,
			"attempted", userCount,
			"shared_cgroup_removed", sharedRemoved,
			"error", firstError,
		)
		return firstError
	}

	m.logger.Info("Resource limits deactivated",
		"users_freed", deactivatedCount,
		"attempted", userCount,
		"shared_cgroup_removed", sharedRemoved,
	)
	return nil
}

func (m *Manager) ForceActivateLimits() error {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()

	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

	metrics, err := m.collectSystemMetrics()
	if err != nil {
		return err
	}
	return m.activateLimits(metrics)
}

func (m *Manager) ForceDeactivateLimits() error {
	leaveEpoch := m.epoch.Enter()
	defer leaveEpoch()

	leaveOperation := m.opGate.Enter()
	defer leaveOperation()

	err := m.deactivateLimits()
	if m.stabilityTracker == nil {
		m.stabilityTracker = newUserStabilityTracker()
	}
	m.stabilityTracker.Reset()
	return err
}
