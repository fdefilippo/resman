package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fdefilippo/resman/config"
)

type idleReleaseResult struct {
	uid int
	err error
}

func (m *Manager) releaseIdleUsers(metrics *SystemMetrics) error {
	cfg := m.GetConfig()
	m.mu.RLock()
	limitsActive := m.limitsActive
	sharedPath := m.sharedCgroupPath
	m.mu.RUnlock()
	if !limitsActive {
		return nil // Limiti non attivi, nessun rilascio necessario
	}
	if sharedPath != "" {
		if _, err := m.applySharedCPUQuota(sharedPath, metrics.TotalCores, cfg); err != nil {
			return fmt.Errorf("failed to reconcile shared CPU quota: %w", err)
		}
	}

	const idleThreshold = 0.1
	normalQuota := cfg.CPUQuotaNormal
	now := time.Now()
	minActiveTime := time.Duration(cfg.GetMinActiveTime()) * time.Second
	eligible := make(map[int]bool, len(metrics.EligibleUsers))
	for _, uid := range metrics.EligibleUsers {
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

	for _, uid := range metrics.EligibleUsers {
		if _, active := m.activeUsers[uid]; active {
			continue
		}
		if cpuEMA, ok := userCPUEMA(metrics, uid); ok && cpuEMA >= idleThreshold {
			usersToAdd = append(usersToAdd, uid)
		}
	}
	m.mu.Unlock()

	for _, uid := range usersToRelease {
		m.reconcileUserResourceLimits(uid, cfg, eligible[uid])
	}

	results := m.releaseTrackedUsers(usersToRelease, sharedPath, normalQuota)
	releasedUsers := make([]int, 0, len(usersToRelease))
	for _, result := range results {
		if result.err != nil {
			m.logger.Warn("Failed to release idle user from shared cgroup",
				"uid", result.uid, "shared_path", sharedPath, "error", result.err)
			continue
		}
		releasedUsers = append(releasedUsers, result.uid)
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
				"cpu", metrics.UserCPUUsage[uid],
			)

			userCgroupPath, err := m.cgroupManager.CreateUserSubCgroup(uid, sharedPath)
			if err != nil {
				m.logger.Warn("Failed to re-create user sub-cgroup",
					"uid", uid, "error", err)
				continue
			}

			if err := m.cgroupManager.MoveAllUserProcessesToSharedCgroup(uid, sharedPath); err != nil {
				m.logger.Warn("Failed to move processes for re-added user; user will not be marked limited",
					"uid", uid, "error", err)
				continue
			}
			m.applyUserResourceLimits(uid, cfg)

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
		m.reconcileUserResourceLimits(uid, cfg, true)
	}

	return nil
}

func userCPUEMA(metrics *SystemMetrics, uid int) (float64, bool) {
	if userMetrics, ok := metrics.UserMetrics[uid]; ok && userMetrics != nil {
		return userMetrics.CPUUsageEMA, true
	}
	cpuUsage, ok := metrics.UserCPUUsage[uid]
	return cpuUsage, ok
}

func (m *Manager) releaseTrackedUsers(users []int, sharedPath, normalQuota string) []idleReleaseResult {
	results := make(chan idleReleaseResult, len(users))
	for _, uid := range users {
		go func(uid int) {
			if sharedPath == "" {
				results <- idleReleaseResult{uid: uid}
				return
			}
			err := m.cgroupManager.ReleaseUserFromSharedCgroup(uid, sharedPath, normalQuota)
			results <- idleReleaseResult{uid: uid, err: err}
		}(uid)
	}

	releases := make([]idleReleaseResult, 0, len(users))
	for range users {
		releases = append(releases, <-results)
	}
	return releases
}

func (m *Manager) commitReleasedUsers(users []int) {
	m.mu.Lock()
	for _, uid := range users {
		delete(m.activeUsers, uid)
		delete(m.userLimitedAt, uid)
		delete(m.resourceLimits, uid)
		delete(m.psiBoostedAt, uid)
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

func (m *Manager) activateLimits(metrics *SystemMetrics) error {
	cfg := m.GetConfig()
	m.logger.Info("Activating CPU limits with proportional weights")

	// Incrementa il contatore di attivazioni
	if m.prometheusExporter != nil {
		m.prometheusExporter.IncrementLimitsActivated()
	}

	// Ottieni gli utenti attualmente limitati
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
	eligibleSet := make(map[int]bool, len(metrics.EligibleUsers))
	for _, uid := range metrics.EligibleUsers {
		eligibleSet[uid] = true
	}

	var firstError error
	limitedCount := 0

	usersToRelease := make([]int, 0)
	for _, uid := range previouslyLimited {
		if !currentActiveSet[uid] || !eligibleSet[uid] {
			usersToRelease = append(usersToRelease, uid)
			m.reconcileUserResourceLimits(uid, cfg, false)
		}
	}
	releaseResults := m.releaseTrackedUsers(usersToRelease, sharedPath, cfg.CPUQuotaNormal)
	releasedUsers := make([]int, 0, len(usersToRelease))
	for _, result := range releaseResults {
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
		releasedUsers = append(releasedUsers, result.uid)
	}
	m.commitReleasedUsers(releasedUsers)
	removedCount := len(releasedUsers)

	// Fase 2: Crea/Configura il cgroup condiviso
	if sharedPath == "" {
		// Crea il cgroup condiviso
		createdSharedPath, err := m.cgroupManager.CreateSharedCgroup()
		if err != nil {
			return fmt.Errorf("failed to create shared cgroup (min_system_cores=%d, total_cores=%d): %w", cfg.GetMinSystemCores(), metrics.TotalCores, err)
		}
		sharedPath = createdSharedPath
		m.mu.Lock()
		m.sharedCgroupPath = sharedPath
		m.mu.Unlock()

	}
	sharedQuota, err := m.applySharedCPUQuota(sharedPath, metrics.TotalCores, cfg)
	if err != nil {
		return fmt.Errorf("failed to apply shared CPU limit %s to %s: %w", sharedQuota, sharedPath, err)
	}

	// Fase 3: Configura i sottocgroup per gli utenti attuali
	// Usa EligibleUsers dal SystemMetrics (già filtrati da config al momento della raccolta)
	// Filter chain: EligibleUsers = USER_INCLUDE_LIST + USER_EXCLUDE_LIST (gatekeeper)
	//   → shouldApplyRAMLimits = RAM_USER_INCLUDE_LIST + RAM_USER_EXCLUDE_LIST (sub-filter)
	//   → shouldApplyIOLimits  = IO_USER_INCLUDE_LIST  + IO_USER_EXCLUDE_LIST  (sub-filter)
	for _, uid := range metrics.EligibleUsers {
		username := m.metricsCollector.GetUsernameFromUID(uid)
		userStr := fmt.Sprintf("%s(%d)", username, uid)
		// Verifica se l'utente è già limitato
		m.mu.RLock()
		alreadyLimited := m.activeUsers[uid]
		m.mu.RUnlock()

		if !alreadyLimited {
			// Crea il sottocgroup per l'utente dentro il cgroup condiviso
			m.mu.RLock()
			sharedPath := m.sharedCgroupPath
			m.mu.RUnlock()
			userCgroupPath, err := m.cgroupManager.CreateUserSubCgroup(uid, sharedPath)
			if err != nil {
				m.logger.Error("Failed to create user sub-cgroup",
					"user", userStr,
					"shared_cgroup", sharedPath,
					"error", err,
				)
				if firstError == nil {
					firstError = err
				}
				continue
			}

			// Avvia monitoraggio PSI per questo utente (adaptive boosting)
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

			// Imposta il peso per l'utente (uguale per tutti)
			// I pesi sono relativi: se tutti hanno peso 100, ottengono parti uguali
			// Se un utente non usa CPU, gli altri possono usare più della loro parte
			weight := 100 // Peso uguale per tutti

			if err := m.cgroupManager.MoveAllUserProcessesToSharedCgroup(uid, sharedPath); err != nil {
				m.logger.Warn("Failed to move processes to shared cgroup; user will not be marked limited",
					"uid", uid,
					"username", username,
					"shared_cgroup", sharedPath,
					"error", err,
				)
				if m.psiWatcher != nil {
					m.psiWatcher.RemoveMonitor(uid, "cpu")
					m.psiWatcher.RemoveMonitor(uid, "io")
				}
				if firstError == nil {
					firstError = err
				}
				continue
			}

			m.applyUserResourceLimits(uid, cfg)

			// Segna l'utente come limitato
			m.mu.Lock()
			m.activeUsers[uid] = true
			m.userLimitedAt[uid] = time.Now()
			m.mu.Unlock()

			limitedCount++
			m.notifyUserLimited(cfg, uid, username, metrics)

			m.logger.Debug("User configured in shared cgroup",
				"uid", uid,
				"weight", weight,
				"shared_path", m.sharedCgroupPath,
			)
		}
	}

	if limitedCount > 0 || removedCount > 0 {
		newActivation := false
		m.mu.Lock()
		if !m.limitsActive && len(m.activeUsers) > 0 {
			m.limitsActive = true
			m.limitsAppliedTime = time.Now()
			newActivation = true
		}
		m.mu.Unlock()
		if newActivation && m.stabilityTracker != nil {
			m.stabilityTracker.Reset()
		}

		m.logger.Info("CPU limits activated with proportional sharing",
			"users_limited", limitedCount,
			"users_freed", removedCount,
			"total_active_users", len(metrics.UserCPUUsage),
			"shared_cgroup", m.sharedCgroupPath,
			"sharing_logic", "Proportional weights (cpu.weight)",
			"description", "Users share total quota proportionally; idle users don't consume resources",
		)
	}

	return firstError
}

func (m *Manager) applyUserResourceLimits(uid int, cfg *config.Config) {
	username := m.metricsCollector.GetUsernameFromUID(uid)

	if err := m.cgroupManager.ApplyCPUWeight(uid, 100); err != nil {
		m.logger.Warn("Failed to set CPU weight for user, using default",
			"uid", uid,
			"username", username,
			"weight", 100,
			"error", err,
		)
	}

	cpuQuota := "max 100000"
	ramQuota := cfg.RAMQuotaPerUser
	if cfg.GetAutodetectPatterns() && m.policyEngine != nil {
		if policy, exists := m.policyEngine.GetPolicy(uid); exists {
			if policy.CPUQuota > 0 {
				cpuQuota = fmt.Sprintf("%d 100000", policy.CPUQuota)
			}
			if policy.RAMQuota != "" {
				ramQuota = policy.RAMQuota
			}
		}
	}
	if err := m.cgroupManager.ApplyCPUQuota(uid, cpuQuota); err != nil {
		m.logger.Warn("Failed to apply per-user CPU quota",
			"uid", uid,
			"username", username,
			"quota", cpuQuota,
			"error", err,
		)
	}

	m.applyRAMResourceLimit(uid, cfg, ramQuota)
	m.applyIOResourceLimit(uid, cfg)
}

func (m *Manager) applyRAMResourceLimit(uid int, cfg *config.Config, ramQuota string) {
	if !m.shouldApplyRAMLimitsWithConfig(uid, cfg) {
		return
	}
	quotaBytes, err := config.ParseRAMQuota(ramQuota)
	if err != nil || quotaBytes == 0 {
		m.logger.Debug("RAM quota per user is 0 or invalid, skipping",
			"uid", uid,
			"quota", ramQuota,
		)
		return
	}

	m.setResourceLimitState(uid, true, false, cfg.DisableSwap, false)
	highBytes := uint64(float64(quotaBytes) * cfg.GetRAMHighRatio())
	highStr := strconv.FormatUint(highBytes, 10)
	if cfg.DisableSwap {
		if err := m.cgroupManager.ApplyRAMLimitWithHighAndSwapDisabled(uid, ramQuota, highStr); err != nil {
			m.logger.Warn("Failed to apply RAM high+max limits with swap disabled for user",
				"uid", uid,
				"high", highStr,
				"max", ramQuota,
				"error", err,
			)
			return
		}
		m.setResourceLimitState(uid, true, false, true, true)
		return
	}
	if err := m.cgroupManager.ApplyRAMLimitWithHigh(uid, ramQuota, highStr); err != nil {
		m.logger.Warn("Failed to apply RAM high+max limits for user",
			"uid", uid,
			"high", highStr,
			"max", ramQuota,
			"error", err,
		)
		return
	}
	m.setResourceLimitState(uid, true, false, false, true)
}

func (m *Manager) applyIOResourceLimit(uid int, cfg *config.Config) {
	if !m.shouldApplyIOLimitsWithConfig(uid, cfg) {
		return
	}
	readBPS := cfg.GetIOReadBPS()
	writeBPS := cfg.GetIOWriteBPS()
	readIOPS := cfg.GetIOReadIOPS()
	writeIOPS := cfg.GetIOWriteIOPS()
	deviceFilter := cfg.GetIODeviceFilter()

	m.setResourceLimitState(uid, false, true, false, false)
	if err := m.cgroupManager.ApplyIOLimit(uid, readBPS, writeBPS, readIOPS, writeIOPS, deviceFilter); err != nil {
		m.logger.Warn("Failed to apply IO limit for user",
			"uid", uid,
			"readBPS", readBPS,
			"writeBPS", writeBPS,
			"error", err,
		)
		return
	}
	m.setResourceLimitState(uid, false, true, false, true)
	m.logger.Debug("IO limit applied for user",
		"uid", uid,
		"readBPS", readBPS,
		"writeBPS", writeBPS,
	)
}

func (m *Manager) reconcileUserResourceLimits(uid int, cfg *config.Config, cpuEligible bool) {
	m.mu.RLock()
	applied := m.resourceLimits[uid]
	m.mu.RUnlock()

	wantRAM := cpuEligible && m.shouldApplyRAMLimitsWithConfig(uid, cfg)
	wantIO := cpuEligible && m.shouldApplyIOLimitsWithConfig(uid, cfg)
	if applied.ram && !wantRAM {
		if err := m.removeTrackedResourceLimits(uid, true, false); err != nil {
			m.logger.Warn("Failed to reconcile RAM limits after configuration change",
				"uid", uid,
				"error", err,
			)
		}
	} else if applied.ram && wantRAM && applied.swap && !cfg.DisableSwap {
		if err := m.removeTrackedRAMSwapLimit(uid); err != nil {
			m.logger.Warn("Failed to reconcile RAM swap limit after configuration change",
				"uid", uid,
				"error", err,
			)
		}
	} else if wantRAM && (!applied.ram || !applied.ramApplied || (!applied.swap && cfg.DisableSwap)) {
		ramQuota := cfg.RAMQuotaPerUser
		if cfg.GetAutodetectPatterns() && m.policyEngine != nil {
			if policy, exists := m.policyEngine.GetPolicy(uid); exists && policy.RAMQuota != "" {
				ramQuota = policy.RAMQuota
			}
		}
		m.applyRAMResourceLimit(uid, cfg, ramQuota)
	}

	if applied.io && !wantIO {
		if err := m.removeTrackedResourceLimits(uid, false, true); err != nil {
			m.logger.Warn("Failed to reconcile IO limits after configuration change",
				"uid", uid,
				"error", err,
			)
		}
	} else if wantIO && (!applied.io || !applied.ioApplied) {
		m.applyIOResourceLimit(uid, cfg)
	}
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
	if !state.ram && !state.io {
		delete(m.resourceLimits, uid)
	} else {
		m.resourceLimits[uid] = state
	}
	m.mu.Unlock()
}

func (m *Manager) applySharedCPUQuota(sharedPath string, totalCores int, cfg *config.Config) (string, error) {
	availableCores := totalCores - cfg.GetMinSystemCores()
	if availableCores < 1 {
		availableCores = 1
	}
	sharedQuota := fmt.Sprintf("%d 100000", availableCores*100000)
	if err := m.cgroupManager.ApplySharedCPULimit(sharedPath, sharedQuota); err != nil {
		return sharedQuota, err
	}
	m.logger.Debug("Shared cgroup CPU quota reconciled",
		"path", sharedPath,
		"total_quota", sharedQuota,
		"available_cores", availableCores,
		"min_system_cores", cfg.GetMinSystemCores(),
		"total_cores", totalCores,
	)
	return sharedQuota, nil
}

func (m *Manager) deactivateLimits() error {
	cfg := m.GetConfig()
	m.logger.Info("Deactivating CPU limits")

	// Incrementa il contatore di disattivazioni
	if m.prometheusExporter != nil {
		m.prometheusExporter.IncrementLimitsDeactivated()
	}

	m.mu.Lock()
	usersToCleanup := make([]int, 0, len(m.activeUsers))
	for uid := range m.activeUsers {
		usersToCleanup = append(usersToCleanup, uid)
	}

	// Salva il conteggio
	userCount := len(usersToCleanup)

	sharedPath := m.sharedCgroupPath
	m.mu.Unlock()

	var firstError error
	deactivatedCount := 0
	deactivatedUsers := make(map[int]bool, len(usersToCleanup))

	if sharedPath != "" {
		if err := m.cgroupManager.ApplySharedCPULimit(sharedPath, "max 100000"); err != nil {
			m.logger.Warn("Failed to reset shared cgroup CPU quota before deactivation",
				"path", sharedPath,
				"error", err,
			)
			if firstError == nil {
				firstError = err
			}
		}
	}

	// Per ogni utente, rimuovi i limiti
	for _, uid := range usersToCleanup {
		username := m.metricsCollector.GetUsernameFromUID(uid)
		userStr := fmt.Sprintf("%s(%d)", username, uid)
		m.mu.RLock()
		appliedResources := m.resourceLimits[uid]
		m.mu.RUnlock()

		if sharedPath != "" {
			if err := m.removeTrackedResourceLimits(uid, appliedResources.ram, appliedResources.io); err != nil {
				m.logger.Warn("Failed to remove tracked resource limits before shared cgroup release",
					"user", userStr,
					"error", err,
				)
			}
			if err := m.cgroupManager.ReleaseUserFromSharedCgroup(uid, sharedPath, cfg.CPUQuotaNormal); err != nil {
				m.logger.Error("Failed to release user from shared cgroup",
					"user", userStr,
					"shared_cgroup", sharedPath,
					"error", err,
				)
				if firstError == nil {
					firstError = err
				}
				continue
			}

			deactivatedUsers[uid] = true
			deactivatedCount++
			m.logger.Debug("User released from shared CPU limits",
				"uid", uid,
				"shared_cgroup", sharedPath,
			)
			continue
		}

		// Ripristina il limite normale
		if err := m.cgroupManager.ApplyCPULimit(uid, cfg.CPUQuotaNormal); err != nil {
			m.logger.Error("Failed to restore normal CPU limit for user",
				"user", userStr,
				"error", err,
			)
			if firstError == nil {
				firstError = err
			}
			continue
		}

		deactivatedCount++
		m.logger.Debug("CPU limit removed for user", "uid", uid)

		if err := m.removeTrackedResourceLimits(uid, appliedResources.ram, appliedResources.io); err != nil {
			m.logger.Warn("Failed to remove tracked resource limits for user",
				"user", userStr,
				"error", err,
			)
			if firstError == nil {
				firstError = err
			}
		}
		deactivatedUsers[uid] = true
	}

	// Rimuovi il cgroup condiviso se esiste
	sharedRemoved := sharedPath == ""
	if sharedPath != "" {
		if err := os.Remove(sharedPath); err != nil {
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
	m.mu.Lock()
	for uid := range deactivatedUsers {
		delete(m.activeUsers, uid)
		delete(m.userLimitedAt, uid)
		delete(m.resourceLimits, uid)
		delete(m.psiBoostedAt, uid)
	}
	if len(m.activeUsers) == 0 {
		m.limitsActive = false
		m.limitsAppliedTime = time.Time{}
		fullyDeactivated = true
		if sharedRemoved {
			m.sharedCgroupPath = ""
		}
	}
	m.mu.Unlock()
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

	m.logger.Info("CPU limits deactivated",
		"users_freed", deactivatedCount,
		"attempted", userCount,
		"shared_cgroup_removed", sharedRemoved,
	)

	return firstError
}

func (m *Manager) shouldApplyRAMLimitsWithConfig(uid int, cfg *config.Config) bool {
	if !cfg.RAMEnabled {
		return false
	}
	username := m.getUsername(uid)
	return cfg.IsUserWhitelistedForRAM(username)
}

func (m *Manager) shouldApplyIOLimitsWithConfig(uid int, cfg *config.Config) bool {
	if !cfg.IOEnabled {
		return false
	}
	username := m.getUsername(uid)
	return cfg.IsUserWhitelistedForIO(username)
}

// GetUIDFromUsername risolve un username a UID scansionando /proc

func (m *Manager) ForceActivateLimits() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	metrics, err := m.collectSystemMetrics()
	if err != nil {
		return err
	}
	return m.activateLimits(metrics)
}

func (m *Manager) ForceDeactivateLimits() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	err := m.deactivateLimits()
	if m.stabilityTracker == nil {
		m.stabilityTracker = newUserStabilityTracker()
	}
	m.stabilityTracker.Reset()
	return err
}
