package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fdefilippo/resman/config"
)
func (m *Manager) releaseIdleUsers(metrics *SystemMetrics) error {
	if !m.limitsActive {
		return nil // Limiti non attivi, nessun rilascio necessario
	}

	// Soglia per considerare un utente "inattivo" (0.1% CPU)
	const idleThreshold = 0.1

	m.mu.Lock()
	sharedPath := m.sharedCgroupPath
	usersToRelease := make([]int, 0)
	usersToAdd := make([]int, 0) // utenti da riaggiungere (erano stati rilasciati ma sono tornati attivi)

	for uid := range m.activeUsers {
		// Controlla se l'utente è ancora attivo (ha processi in esecuzione)
		// O(1) lookup instead of O(N*M) linear search
		if _, userStillActive := metrics.UserCPUUsage[uid]; !userStillActive {
			usersToRelease = append(usersToRelease, uid)
			continue
		}

		// Controlla uso CPU dell'utente
		if cpuUsage, ok := metrics.UserCPUUsage[uid]; ok {
			if cpuUsage < idleThreshold {
				// Utente inattivo (CPU < 0.1%)
				usersToRelease = append(usersToRelease, uid)
			}
		}
	}

	// Controlla se ci sono utenti eligible (passano i filtri config) che sono
	// sopra la soglia di idle ma non sono in activeUsers (erano stati rilasciati
	// in precedenza da releaseIdleUsers). Devono essere riaggiunti.
	for _, uid := range metrics.EligibleUsers {
		if _, active := m.activeUsers[uid]; active {
			continue
		}
		if cpuUsage, ok := metrics.UserCPUUsage[uid]; ok && cpuUsage >= idleThreshold {
			usersToAdd = append(usersToAdd, uid)
		}
	}

	// Rimuovi utenti dalla mappa e pulisci PSI/boost
	for _, uid := range usersToRelease {
		delete(m.activeUsers, uid)
		delete(m.psiBoostedAt, uid)
		if m.psiWatcher != nil {
			m.psiWatcher.RemoveMonitor(uid, "cpu")
			m.psiWatcher.RemoveMonitor(uid, "io")
		}
		// Sposta l'utente fuori dal cgroup condiviso e rimuovi il sottocgroup.
		m.wg.Add(1)
		go func(uid int, sharedPath string) {
			defer m.wg.Done()
			if sharedPath == "" {
				return
			}
			if err := m.cgroupManager.ReleaseUserFromSharedCgroup(uid, sharedPath); err != nil {
				m.logger.Warn("Failed to release idle user from shared cgroup",
					"uid", uid, "shared_path", sharedPath, "error", err)
			}
		}(uid, sharedPath)
	}

	remainingLimited := len(m.activeUsers)
	m.mu.Unlock()

	// Log rilascio
	if len(usersToRelease) > 0 {
		m.logger.Info("Releasing idle users from CPU limits",
			"users_released", len(usersToRelease),
			"users_still_limited", remainingLimited,
			"idle_threshold", idleThreshold,
		)
	}

	// Applica i limiti per gli utenti riaggiunti (dopo aver rilasciato il lock)
	// activeUsers[uid] viene marcato solo dopo che CreateUserSubCgroup ha successo
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

			m.wg.Add(1)
			go func(uid int, sharedPath string) {
				defer m.wg.Done()
				time.Sleep(300 * time.Millisecond)
				if err := m.cgroupManager.MoveAllUserProcessesToSharedCgroup(uid, sharedPath); err != nil {
					m.logger.Warn("Failed to move processes for re-added user",
						"uid", uid, "error", err)
				}
				if err := m.cgroupManager.ApplyCPUWeight(uid, 100); err != nil {
					m.logger.Warn("Failed to set CPU weight for re-added user",
						"uid", uid, "weight", 100, "error", err)
				}
			}(uid, sharedPath)

			if m.psiWatcher != nil {
				cpuPressurePath := filepath.Join(userCgroupPath, "cpu.pressure")
				ioPressurePath := filepath.Join(userCgroupPath, "io.pressure")
				m.psiWatcher.AddMonitor(uid, "cpu", cpuPressurePath)
				m.psiWatcher.AddMonitor(uid, "io", ioPressurePath)
			}

			added = append(added, uid)
		}

		// Only mark users as active after successful cgroup creation
		if len(added) > 0 {
			m.mu.Lock()
			for _, uid := range added {
				m.activeUsers[uid] = true
			}
			m.limitsAppliedTime = time.Now()
			remainingLimited = len(m.activeUsers)
			m.mu.Unlock()
		}
	}

	if len(usersToRelease) == 0 && len(usersToAdd) == 0 {
		return nil
	}

	return nil
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
	m.mu.RUnlock()

	// Crea un set per gli utenti attuali per un controllo efficiente
	currentActiveSet := make(map[int]bool)
	for uid := range metrics.UserCPUUsage {
		currentActiveSet[uid] = true
	}

	var firstError error
	limitedCount := 0

	// Fase 1: Pulisci gli utenti che non sono più attivi
	var removedCount int
	for _, uid := range previouslyLimited {
		if !currentActiveSet[uid] {
			// Questo utente era limitato ma ora non è più attivo
			m.mu.Lock()
			delete(m.activeUsers, uid)
			m.mu.Unlock()

			removedCount++
			m.logger.Debug("User removed from active tracking", "uid", uid)
		}
	}

	// Fase 2: Crea/Configura il cgroup condiviso
	m.mu.RLock()
	sharedPath := m.sharedCgroupPath
	m.mu.RUnlock()
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

		// Calcola la quota TOTALE per tutti gli utenti
		availableCores := metrics.TotalCores - cfg.GetMinSystemCores()
		if availableCores < 1 {
			availableCores = 1
		}

		// Converti in quota cgroup
		totalQuota := availableCores * 100000
		sharedQuota := fmt.Sprintf("%d 100000", totalQuota)

		// Applica la quota al cgroup condiviso
		if err := m.cgroupManager.ApplySharedCPULimit(sharedPath, sharedQuota); err != nil {
			return fmt.Errorf("failed to apply shared CPU limit %s to %s: %w", sharedQuota, sharedPath, err)
		}

		m.logger.Info("Shared cgroup configured",
			"path", sharedPath,
			"total_quota", sharedQuota,
			"available_cores", availableCores,
			"min_system_cores", cfg.GetMinSystemCores(),
			"total_cores", metrics.TotalCores,
		)
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

			// Sposta i processi dell'utente nel cgroup condiviso
			m.wg.Add(1)
			go func(uid int, weight int, username string, sharedPath string) {
				defer m.wg.Done()
				time.Sleep(300 * time.Millisecond)
				if err := m.cgroupManager.MoveAllUserProcessesToSharedCgroup(uid, sharedPath); err != nil {
					m.logger.Warn("Failed to move some processes to shared cgroup",
						"uid", uid,
						"username", username,
						"shared_cgroup", sharedPath,
						"error", err,
					)
				}

				// Imposta il peso dopo aver spostato i processi
				if err := m.cgroupManager.ApplyCPUWeight(uid, weight); err != nil {
					m.logger.Warn("Failed to set CPU weight for user, using default",
						"uid", uid,
						"username", username,
						"weight", weight,
						"error", err,
					)
				}

				// Applica limite RAM se abilitato e l'utente è soggetto a RAM limits
				if m.shouldApplyRAMLimits(uid) {
					quotaBytes, err := config.ParseRAMQuota(cfg.RAMQuotaPerUser)
					if err != nil || quotaBytes == 0 {
						m.logger.Debug("RAM quota per user is 0 or invalid, skipping",
							"uid", uid,
							"quota", cfg.RAMQuotaPerUser,
						)
					} else {
						// Calcola memory.high come percentuale di memory.max
						highBytes := uint64(float64(quotaBytes) * cfg.GetRAMHighRatio())
						highStr := strconv.FormatUint(highBytes, 10)
						maxStr := cfg.RAMQuotaPerUser

						if cfg.DisableSwap {
							if err := m.cgroupManager.ApplyRAMLimitWithHighAndSwapDisabled(uid, maxStr, highStr); err != nil {
								m.logger.Warn("Failed to apply RAM high+max limits with swap disabled for user",
									"uid", uid,
									"high", highStr,
									"max", maxStr,
									"error", err,
								)
							}
						} else {
							if err := m.cgroupManager.ApplyRAMLimitWithHigh(uid, maxStr, highStr); err != nil {
								m.logger.Warn("Failed to apply RAM high+max limits for user",
									"uid", uid,
									"high", highStr,
									"max", maxStr,
									"error", err,
								)
							}
						}
					}
				}
				// Applica limiti IO se abilitati
				if m.shouldApplyIOLimits(uid) {
					readBPS := cfg.GetIOReadBPS()
					writeBPS := cfg.GetIOWriteBPS()
					readIOPS := cfg.GetIOReadIOPS()
					writeIOPS := cfg.GetIOWriteIOPS()
					deviceFilter := cfg.GetIODeviceFilter()

					if err := m.cgroupManager.ApplyIOLimit(uid, readBPS, writeBPS, readIOPS, writeIOPS, deviceFilter); err != nil {
						m.logger.Warn("Failed to apply IO limit for user",
							"uid", uid,
							"readBPS", readBPS,
							"writeBPS", writeBPS,
							"error", err,
						)
					} else {
						m.logger.Debug("IO limit applied for user",
							"uid", uid,
							"readBPS", readBPS,
							"writeBPS", writeBPS,
						)
					}
				}
			}(uid, weight, username, sharedPath)

			// Segna l'utente come limitato
			m.mu.Lock()
			m.activeUsers[uid] = true
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
		m.mu.Lock()
		m.limitsActive = true
		m.limitsAppliedTime = time.Now()
		m.mu.Unlock()

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


func (m *Manager) deactivateLimits() error {
	cfg := m.GetConfig()
	m.logger.Info("Deactivating CPU limits")

	m.wg.Wait()

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

	// Pulisci la mappa
	for uid := range m.activeUsers {
		delete(m.activeUsers, uid)
	}
	m.limitsActive = false
	m.limitsAppliedTime = time.Time{}

	// Pulisci il percorso del cgroup condiviso
	sharedPath := m.sharedCgroupPath
	m.sharedCgroupPath = ""
	m.mu.Unlock()

	// Rimuovi monitoraggi PSI per questi utenti
	if m.psiWatcher != nil {
		for _, uid := range usersToCleanup {
			m.psiWatcher.RemoveMonitor(uid, "cpu")
			m.psiWatcher.RemoveMonitor(uid, "io")
		}
	}

	// FIX A1: Cleanup stability tracker to prevent memory leak
	m.stabilityTracker.mu.Lock()
	for _, uid := range usersToCleanup {
		delete(m.stabilityTracker.underThreshold, uid)
	}
	m.stabilityTracker.mu.Unlock()

	// Pulisci PSI boost tracker
	m.mu.Lock()
	for _, uid := range usersToCleanup {
		delete(m.psiBoostedAt, uid)
	}
	m.mu.Unlock()

	var firstError error
	deactivatedCount := 0

	// Per ogni utente, rimuovi i limiti
	for _, uid := range usersToCleanup {
		username := m.metricsCollector.GetUsernameFromUID(uid)
		userStr := fmt.Sprintf("%s(%d)", username, uid)
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

		// Rimuovi limite RAM se abilitato e l'utente era soggetto a RAM limits
		if m.shouldApplyRAMLimits(uid) {
			// Rimuovi prima memory.high
			if err := m.cgroupManager.RemoveRAMHigh(uid); err != nil {
				m.logger.Warn("Failed to remove RAM high limit for user",
					"user", userStr,
					"error", err,
				)
			}
			// Poi rimuovi memory.max
			if err := m.cgroupManager.RemoveRAMLimit(uid); err != nil {
				m.logger.Warn("Failed to remove RAM limit for user",
					"user", userStr,
					"error", err,
				)
			} else {
				m.logger.Debug("RAM limits removed for user", "uid", uid)
			}
		}

		// Rimuovi limiti IO se abilitati e l'utente era soggetto a IO limits
		if m.shouldApplyIOLimits(uid) {
			if err := m.cgroupManager.RemoveIOLimit(uid); err != nil {
				m.logger.Warn("Failed to remove IO limit for user",
					"user", userStr,
					"error", err,
				)
			} else {
				m.logger.Debug("IO limit removed for user", "uid", uid)
			}
		}
	}

	// Rimuovi il cgroup condiviso se esiste
	if sharedPath != "" {
		if err := os.RemoveAll(sharedPath); err != nil {
			m.logger.Warn("Failed to remove shared cgroup",
				"path", sharedPath,
				"error", err,
			)
		} else {
			m.logger.Debug("Shared cgroup removed", "path", sharedPath)
		}
	}

	m.logger.Info("CPU limits deactivated",
		"users_freed", deactivatedCount,
		"attempted", userCount,
		"shared_cgroup_removed", sharedPath != "",
	)

	return firstError
}


func (m *Manager) shouldApplyRAMLimits(uid int) bool {
	cfg := m.GetConfig()
	if !cfg.RAMEnabled {
		return false
	}
	username := m.getUsername(uid)
	return cfg.IsUserWhitelistedForRAM(username)
}


func (m *Manager) shouldApplyIOLimits(uid int) bool {
	cfg := m.GetConfig()
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
	// FIX A4: Reset stability tracker on forced deactivation to avoid stale state
	if m.stabilityTracker == nil {
		m.stabilityTracker = &UserStabilityTracker{underThreshold: make(map[int]int)}
	}
	m.stabilityTracker.mu.Lock()
	m.stabilityTracker.underThreshold = make(map[int]int)
	m.stabilityTracker.mu.Unlock()
	return err
}


