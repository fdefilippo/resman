/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// state/manager.go
package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/metrics"
)

// Manager coordina tutta la logica di gestione della CPU.
type Manager struct {
	cfg    *config.Config
	logger *logging.Logger
	mu     sync.RWMutex
	wg     sync.WaitGroup

	// Stato interno
	limitsActive      bool
	limitsAppliedTime time.Time
	activeUsers       map[int]bool // UID -> se limitato
	sharedCgroupPath  string       // Percorso del cgroup condiviso

	// Threshold monitoring
	thresholdTracker *ThresholdTracker

	// Dipendenze (saranno iniettate)
	metricsCollector   MetricsCollector
	cgroupManager      CgroupManager
	prometheusExporter PrometheusExporter

	// Cache per le metriche (per performance)
	metricsCache     map[string]interface{}
	metricsCacheTime map[string]time.Time
	cacheMutex       sync.RWMutex
}

// ThresholdTracker monitora il superamento della soglia CPU nel tempo
type ThresholdTracker struct {
	firstOverThresholdTime time.Time // Primo superamento soglia
	overThresholdCycles    int       // Cicli sopra soglia
	totalCycles            int       // Cicli totali
	mu                     sync.RWMutex
}

// MetricsCollector è l'interfaccia per raccogliere metriche di sistema.
type MetricsCollector interface {
	GetTotalCores() int
	GetTotalCPUUsage() float64
	GetUserCPUUsage(uid int) float64

	// ALL USERS metrics (tutti gli utenti non-system)
	GetAllUsers() []int
	GetAllUsersCPUUsage() float64
	GetAllUsersMemoryUsage() uint64

	// LIMITED USERS metrics (solo utenti che passano i filtri)
	GetLimitedUsers() []int
	GetLimitedUsersCPUUsage() float64
	GetLimitedUsersMemoryUsage() uint64

	GetMemoryUsage() float64
	IsSystemUnderLoad() bool
	GetAllUserMetrics() map[int]*metrics.UserMetrics
	GetDBWriter() *metrics.DBWriter
	WriteMetricsToDatabase(userMetrics map[int]*metrics.UserMetrics, totalCPUUsage float64, totalCores int, systemLoad float64, limitsActive bool, limitedUsersCount int)
}

// CgroupManager è l'interfaccia per gestire i cgroups.
type CgroupManager interface {
	CreateUserCgroup(uid int) error
	ApplyCPULimit(uid int, quota string) error
	ApplyCPUWeight(uid int, weight int) error
	RemoveCPULimit(uid int) error
	ApplyRAMLimit(uid int, limit string) error
	ApplyRAMLimitWithSwapDisabled(uid int, limit string) error
	RemoveRAMLimit(uid int) error
	GetCgroupRAMUsage(uid int) (uint64, error)
	CleanupUserCgroup(uid int) error
	MoveProcessToCgroup(pid int, uid int) error
	MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) error
	CreateSharedCgroup() (string, error)
	ApplySharedCPULimit(sharedPath string, quota string) error
	CreateUserSubCgroup(uid int, sharedPath string) (string, error)
	CleanupAll() error
	GetCgroupInfo(uid int) (map[string]string, error)
	GetCreatedCgroups() []int
}

// PrometheusExporter è l'interfaccia per esportare metriche Prometheus.
type PrometheusExporter interface {
	UpdateMetrics(metrics map[string]float64)
	UpdateUserMetrics(uid int, username string, cpuUsage float64, memoryUsage uint64, processCount int, isLimited bool, cgroupPath, cpuQuota string)
	UpdateSystemMetrics(totalCores int, systemLoad float64)
	Start(ctx context.Context) error
	Stop() error
	CleanupUserMetrics(activeUids map[int]bool)
	IncrementLimitsActivated()
	IncrementLimitsDeactivated()
}

// NewManager crea un nuovo Manager con le dipendenze configurate.
func NewManager(
	cfg *config.Config,
	metrics MetricsCollector,
	cgroups CgroupManager,
	prometheus PrometheusExporter,
) (*Manager, error) {

	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	logger := logging.GetLogger()

	mgr := &Manager{
		cfg:                cfg,
		logger:             logger,
		limitsActive:       false,
		limitsAppliedTime:  time.Time{},
		activeUsers:        make(map[int]bool),
		sharedCgroupPath:   "",
		thresholdTracker:   &ThresholdTracker{},
		metricsCollector:   metrics,
		cgroupManager:      cgroups,
		prometheusExporter: prometheus,
		metricsCache:       make(map[string]interface{}),
		metricsCacheTime:   make(map[string]time.Time),
	}

	logger.Info("State manager initialized",
		"polling_interval", cfg.PollingInterval,
		"cpu_threshold", cfg.CPUThreshold,
		"cpu_release_threshold", cfg.CPUReleaseThreshold,
		"cpu_threshold_duration", cfg.CPUThresholdDuration,
		"ignore_system_load", cfg.IgnoreSystemLoad,
	)
	return mgr, nil
}

// RunControlCycle esegue un singolo ciclo di controllo.
func (m *Manager) RunControlCycle(ctx context.Context) error {
	startTime := time.Now()
	cycleID := startTime.Unix()

	m.logger.Debug("Starting control cycle", "cycle_id", cycleID)

	// Controlla se siamo in un blackout timeframe
	if m.cfg.IsInBlackout() {
		nextEnd := m.cfg.GetNextBlackoutEnd()
		if nextEnd != nil {
			m.logger.Info("Skipping control cycle - blackout timeframe active",
				"cycle_id", cycleID,
				"next_check", nextEnd.Format("2006-01-02 15:04:05"),
			)
		} else {
			m.logger.Debug("Skipping control cycle - blackout timeframe active",
				"cycle_id", cycleID,
			)
		}
		return nil
	}

	// 1. Raccogli metriche del sistema
	metrics, err := m.collectSystemMetrics()
	if err != nil {
		m.logger.Error("Failed to collect system metrics", "error", err)
		return err
	}

	// 2. Aggiorna le metriche Prometheus (se abilitato)
	if m.prometheusExporter != nil {
		m.updatePrometheusMetrics(metrics)
	}

	// 3. Scrivi le metriche nel database (se abilitato)
	m.writeDatabaseMetrics(metrics)

	// 4. Prendi decisione basata sulle metriche
	decision, reason := m.makeDecision(metrics)

	// 4. Esegui l'azione corrispondente
	if err := m.executeDecision(decision, metrics); err != nil {
		m.logger.Error("Failed to execute decision",
			"decision", decision,
			"error", err,
		)
		return err
	}

	// 6. Registra lo storico del ciclo
	duration := time.Since(startTime)
	m.recordControlCycle(decision, reason, metrics, duration)

	// 5. Logga il risultato del ciclo
	m.logger.Info("Control cycle completed",
		"cycle_id", cycleID,
		"decision", decision,
		"reason", reason,
		"total_cpu_usage", metrics.TotalCPUUsage,
		"limited_users_cpu_usage", metrics.LimitedUsersCPUUsage,
		"limited_users", metrics.LimitedUsersCount,
		"system_under_load", metrics.SystemUnderLoad,
		"ignore_system_load", m.cfg.IgnoreSystemLoad,
		"duration_ms", duration.Milliseconds(),
	)

	return nil
}

// SystemMetrics contiene tutte le metriche raccolte in un ciclo.
type SystemMetrics struct {
	Timestamp         time.Time
	TotalCores        int
	TotalCPUUsage     float64 // Percentuale

	// ALL USERS metrics (tutti gli utenti non-system, UID >= SYSTEM_UID_MIN)
	AllUsersCPUUsage    float64
	AllUsersMemoryUsage uint64
	AllUsersCount       int

	// LIMITED USERS metrics (solo utenti che passano i filtri)
	LimitedUsersCPUUsage    float64
	LimitedUsersMemoryUsage uint64
	LimitedUsersCount       int

	MemoryUsage     float64 // MB
	SystemUnderLoad bool
	UserCPUUsage    map[int]float64              // UID -> percentuale
	UserMetrics     map[int]*metrics.UserMetrics // Metriche dettagliate per utente
}

// collectSystemMetrics raccoglie tutte le metriche di sistema necessarie.
func (m *Manager) collectSystemMetrics() (*SystemMetrics, error) {
	metrics := &SystemMetrics{
		Timestamp:    time.Now(),
		UserCPUUsage: make(map[int]float64),
		UserMetrics:  make(map[int]*metrics.UserMetrics),
	}

	// Raccogli metriche di base
	metrics.TotalCores = m.metricsCollector.GetTotalCores()
	metrics.TotalCPUUsage = m.metricsCollector.GetTotalCPUUsage()

	// ALL USERS metrics
	metrics.AllUsersCPUUsage = m.metricsCollector.GetAllUsersCPUUsage()
	metrics.AllUsersMemoryUsage = m.metricsCollector.GetAllUsersMemoryUsage()
	metrics.AllUsersCount = len(m.metricsCollector.GetAllUsers())

	// LIMITED USERS metrics
	metrics.LimitedUsersCPUUsage = m.metricsCollector.GetLimitedUsersCPUUsage()
	metrics.LimitedUsersMemoryUsage = m.metricsCollector.GetLimitedUsersMemoryUsage()
	metrics.LimitedUsersCount = len(m.metricsCollector.GetLimitedUsers())

	metrics.MemoryUsage = m.metricsCollector.GetMemoryUsage()
	metrics.SystemUnderLoad = m.metricsCollector.IsSystemUnderLoad()

	// Raccogli metriche dettagliate per ogni utente (CPU, memoria, processi) in una sola chiamata
	allUserMetrics := m.metricsCollector.GetAllUserMetrics()

	// Popola UserMetrics e UserCPUUsage
	for uid, userMetrics := range allUserMetrics {
		metrics.UserMetrics[uid] = userMetrics
		metrics.UserCPUUsage[uid] = userMetrics.CPUUsage
	}

	return metrics, nil
}

// makeDecision prende la decisione se attivare, mantenere o disattivare i limiti.
func (m *Manager) makeDecision(metrics *SystemMetrics) (string, string) {
	m.mu.RLock()
	limitsActive := m.limitsActive
	limitsAppliedTime := m.limitsAppliedTime
	m.mu.RUnlock()

	// Decisioni possibili
	const (
		DecisionActivate   = "ACTIVATE_LIMITS"
		DecisionMaintain   = "MAINTAIN_CURRENT_STATE"
		DecisionDeactivate = "DEACTIVATE_LIMITS"
	)

	// Se i limiti sono attivi, controlliamo se possiamo disattivarli
	if limitsActive {
		// Verifica il tempo minimo di attivazione
		if time.Since(limitsAppliedTime) < time.Duration(m.cfg.MinActiveTime)*time.Second {
			return DecisionMaintain, "Limits active, waiting for minimum activation time"
		}

		// Verifica se l'uso della CPU è sceso sotto la soglia di rilascio
		if metrics.LimitedUsersCPUUsage < float64(m.cfg.CPUReleaseThreshold) {
			// Verifica anche che il sistema non sia sotto carico
			if !metrics.SystemUnderLoad {
				// Reset tracker quando i limiti vengono rilasciati
				m.thresholdTracker.Reset()

				return DecisionDeactivate, fmt.Sprintf(
					"CPU usage below release threshold (%.1f%% < %d%%) and system not under load",
					metrics.LimitedUsersCPUUsage, m.cfg.CPUReleaseThreshold,
				)
			}
			return DecisionMaintain, "CPU usage below threshold but system still under load"
		}

		return DecisionMaintain, "Limits active, CPU usage still above release threshold"
	}

	// Se i limiti non sono attivi, controlliamo se dobbiamo attivarli
	// 1. Verifica soglia CPU
	if metrics.LimitedUsersCPUUsage >= float64(m.cfg.CPUThreshold) {
		// 2. Verifica che ci siano abbastanza core per il sistema
		if metrics.TotalCores <= m.cfg.MinSystemCores {
			m.thresholdTracker.Reset()
			return DecisionMaintain, fmt.Sprintf(
				"CPU usage high (%.1f%% >= %d%%) but insufficient cores (%d <= %d)",
				metrics.LimitedUsersCPUUsage, m.cfg.CPUThreshold,
				metrics.TotalCores, m.cfg.MinSystemCores,
			)
		}

		// 3. Verifica se dobbiamo ignorare il load average
		if !m.cfg.IgnoreSystemLoad && metrics.SystemUnderLoad {
			m.thresholdTracker.Reset()
			return DecisionMaintain, "CPU usage high but system already under load from other factors"
		}

		// 4. Verifica time window (se configurata)
		if m.cfg.CPUThresholdDuration > 0 {
			shouldActivate := m.thresholdTracker.ShouldActivateLimits(
				metrics.LimitedUsersCPUUsage,
				float64(m.cfg.CPUThreshold),
				time.Duration(m.cfg.CPUThresholdDuration)*time.Second,
			)

			if !shouldActivate {
				elapsed := m.thresholdTracker.GetElapsed()
				remaining := time.Duration(m.cfg.CPUThresholdDuration)*time.Second - elapsed
				return DecisionMaintain, fmt.Sprintf(
					"CPU threshold exceeded, waiting %s before activating limits (%.1f%% >= %d%%)",
					remaining.Round(time.Second),
					metrics.LimitedUsersCPUUsage, m.cfg.CPUThreshold,
				)
			}
		}

		return DecisionActivate, fmt.Sprintf(
			"CPU usage exceeded threshold (%.1f%% >= %d%%)",
			metrics.LimitedUsersCPUUsage, m.cfg.CPUThreshold,
		)
	}

	// CPU sotto soglia, reset tracker
	m.thresholdTracker.Reset()
	return DecisionMaintain, "CPU usage within normal range"
}

// executeDecision esegue l'azione corrispondente alla decisione presa.
func (m *Manager) executeDecision(decision string, metrics *SystemMetrics) error {
	switch decision {
	case "ACTIVATE_LIMITS":
		return m.activateLimits(metrics)
	case "DEACTIVATE_LIMITS":
		return m.deactivateLimits()
	case "MAINTAIN_CURRENT_STATE":
		// Controlla se ci sono utenti inattivi da rilasciare
		return m.releaseIdleUsers(metrics)
	default:
		return fmt.Errorf("unknown decision: %s", decision)
	}
}

// releaseIdleUsers rilascia gli utenti che non stanno usando CPU mentre i limiti sono attivi
func (m *Manager) releaseIdleUsers(metrics *SystemMetrics) error {
	if !m.limitsActive {
		return nil // Limiti non attivi, nessun rilascio necessario
	}

	// Soglia per considerare un utente "inattivo" (0.1% CPU)
	const idleThreshold = 0.1

	m.mu.Lock()
	usersToRelease := make([]int, 0)

	for uid := range m.activeUsers {
		// Controlla se l'utente è ancora attivo (ha processi in esecuzione)
		userStillActive := false
		for activeUID := range metrics.UserCPUUsage {
			if activeUID == uid {
				userStillActive = true
				break
			}
		}

		if !userStillActive {
			// Utente non più nella lista attivi
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

	// Rimuovi utenti dalla mappa
	for _, uid := range usersToRelease {
		delete(m.activeUsers, uid)
	}

	remainingLimited := len(m.activeUsers)
	m.mu.Unlock()

	if len(usersToRelease) == 0 {
		return nil // Nessun utente da rilasciare
	}

	m.logger.Info("Releasing idle users from CPU limits",
		"users_released", len(usersToRelease),
		"users_still_limited", remainingLimited,
		"idle_threshold", idleThreshold,
	)

	var firstError error
	releasedCount := 0

	// Rilascia ogni utente inattivo
	for _, uid := range usersToRelease {
		// Ripristina il limite normale
		if err := m.cgroupManager.ApplyCPULimit(uid, m.cfg.CPUQuotaNormal); err != nil {
			m.logger.Error("Failed to restore normal CPU limit for idle user",
				"uid", uid,
				"error", err,
			)
			if firstError == nil {
				firstError = err
			}
			continue
		}

		releasedCount++
		m.logger.Debug("CPU limit removed for idle user", "uid", uid)
	}

	// Logga il risultato
	m.logger.Info("Idle user release completed",
		"released", releasedCount,
		"remaining_limited", remainingLimited,
	)

	return firstError
}

// activateLimits attiva i limiti di CPU per gli utenti attivi usando pesi proporzionali.
func (m *Manager) activateLimits(metrics *SystemMetrics) error {
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
	if m.sharedCgroupPath == "" {
		// Crea il cgroup condiviso
		sharedPath, err := m.cgroupManager.CreateSharedCgroup()
		if err != nil {
			return fmt.Errorf("failed to create shared cgroup: %w", err)
		}
		m.sharedCgroupPath = sharedPath

		// Calcola la quota TOTALE per tutti gli utenti
		availableCores := metrics.TotalCores - m.cfg.MinSystemCores
		if availableCores < 1 {
			availableCores = 1
		}

		// Converti in quota cgroup
		totalQuota := availableCores * 100000
		sharedQuota := fmt.Sprintf("%d 100000", totalQuota)

		// Applica la quota al cgroup condiviso
		if err := m.cgroupManager.ApplySharedCPULimit(sharedPath, sharedQuota); err != nil {
			return fmt.Errorf("failed to apply shared CPU limit: %w", err)
		}

		m.logger.Info("Shared cgroup configured",
			"path", sharedPath,
			"total_quota", sharedQuota,
			"available_cores", availableCores,
			"min_system_cores", m.cfg.MinSystemCores,
			"total_cores", metrics.TotalCores,
		)
	}

	// Fase 3: Configura i sottocgroup per gli utenti attuali
	for uid := range metrics.UserCPUUsage {
		username := m.getUsername(uid)
		userStr := fmt.Sprintf("%s(%d)", username, uid)
		// Verifica se l'utente è già limitato
		m.mu.RLock()
		alreadyLimited := m.activeUsers[uid]
		m.mu.RUnlock()

		if !alreadyLimited {
			// Crea il sottocgroup per l'utente dentro il cgroup condiviso
			_, err := m.cgroupManager.CreateUserSubCgroup(uid, m.sharedCgroupPath)
			if err != nil {
				m.logger.Error("Failed to create user sub-cgroup",
					"user", userStr,
					"error", err,
				)
				if firstError == nil {
					firstError = err
				}
				continue
			}

			// Imposta il peso per l'utente (uguale per tutti)
			// I pesi sono relativi: se tutti hanno peso 100, ottengono parti uguali
			// Se un utente non usa CPU, gli altri possono usare più della loro parte
			weight := 100 // Peso uguale per tutti

			// Sposta i processi dell'utente nel cgroup condiviso
			m.wg.Add(1)
			go func(uid int, weight int) {
				defer m.wg.Done()
				time.Sleep(300 * time.Millisecond)
				if err := m.cgroupManager.MoveAllUserProcessesToSharedCgroup(uid, m.sharedCgroupPath); err != nil {
					m.logger.Warn("Failed to move some processes to shared cgroup",
						"uid", uid,
						"error", err,
					)
				}

				// Imposta il peso dopo aver spostato i processi
				if err := m.cgroupManager.ApplyCPUWeight(uid, weight); err != nil {
					m.logger.Warn("Failed to set CPU weight for user, using default",
						"uid", uid,
						"error", err,
					)
				}

				// Applica limite RAM se abilitato e l'utente è soggetto a RAM limits
				if m.shouldApplyRAMLimits(uid) {
					quotaBytes, err := config.ParseRAMQuota(m.cfg.RAMQuotaPerUser)
					if err != nil || quotaBytes == 0 {
						m.logger.Debug("RAM quota per user is 0 or invalid, skipping",
							"uid", uid,
							"quota", m.cfg.RAMQuotaPerUser,
						)
					} else {
						if m.cfg.DisableSwap {
							if err := m.cgroupManager.ApplyRAMLimitWithSwapDisabled(uid, m.cfg.RAMQuotaPerUser); err != nil {
								m.logger.Warn("Failed to apply RAM limit for user",
									"uid", uid,
									"limit", m.cfg.RAMQuotaPerUser,
									"error", err,
								)
							}
						} else {
							if err := m.cgroupManager.ApplyRAMLimit(uid, m.cfg.RAMQuotaPerUser); err != nil {
								m.logger.Warn("Failed to apply RAM limit for user",
									"uid", uid,
									"limit", m.cfg.RAMQuotaPerUser,
									"error", err,
								)
							}
						}
					}
				}
			}(uid, weight)

			// Segna l'utente come limitato
			m.mu.Lock()
			m.activeUsers[uid] = true
			m.mu.Unlock()

			limitedCount++

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

// deactivateLimits rimuove i limiti di CPU da tutti gli utenti.
func (m *Manager) deactivateLimits() error {
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

	var firstError error
	deactivatedCount := 0

	// Per ogni utente, rimuovi i limiti
	for _, uid := range usersToCleanup {
		username := m.getUsername(uid)
		userStr := fmt.Sprintf("%s(%d)", username, uid)
		// Ripristina il limite normale
		if err := m.cgroupManager.ApplyCPULimit(uid, m.cfg.CPUQuotaNormal); err != nil {
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
			if err := m.cgroupManager.RemoveRAMLimit(uid); err != nil {
				m.logger.Warn("Failed to remove RAM limit for user",
					"user", userStr,
					"error", err,
				)
			} else {
				m.logger.Debug("RAM limit removed for user", "uid", uid)
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

// updatePrometheusMetrics aggiorna le metriche per Prometheus.
func (m *Manager) updatePrometheusMetrics(metrics *SystemMetrics) {
	if m.prometheusExporter == nil {
		return
	}

	// Metriche base per il metodo UpdateMetrics
	promMetrics := map[string]float64{
		// System metrics
		"cpu_total_usage": metrics.TotalCPUUsage,
		"total_cores":     float64(metrics.TotalCores),

		// ALL USERS metrics
		"all_users_cpu_usage":    metrics.AllUsersCPUUsage,
		"all_users_count":        float64(metrics.AllUsersCount),
		"all_users_memory_usage": float64(metrics.AllUsersMemoryUsage),

		// LIMITED USERS metrics
		"limited_users_cpu_usage":    metrics.LimitedUsersCPUUsage,
		"limited_users_count":        float64(metrics.LimitedUsersCount),
		"limited_users_memory_usage": float64(metrics.LimitedUsersMemoryUsage),

		// Other metrics
		"memory_usage_mb": metrics.MemoryUsage,
		"limited_users":   float64(len(m.activeUsers)),
		"limits_active":   boolToFloat(m.limitsActive),
	}

	// Aggiungi load average se disponibile
	if load, err := m.getLoadAverage(); err == nil {
		promMetrics["system_load"] = load
	}

	m.prometheusExporter.UpdateMetrics(promMetrics)

	// Aggiorna metriche specifiche per utente usando UserMetrics
	for uid, userMetrics := range metrics.UserMetrics {
		username := userMetrics.Username
		if username == "" || username == strconv.Itoa(uid) {
			username = m.getUsername(uid)
		}

		isLimited := m.isUserLimited(uid)

		// Ottieni info cgroup se disponibile
		var cgroupPath, cpuQuota string
		if m.cgroupManager != nil {
			if info, err := m.cgroupManager.GetCgroupInfo(uid); err == nil {
				cgroupPath = info["path"]
				cpuQuota = info["cpu.max"]
			}
		}

		// Usa UpdateUserMetrics con tutti i parametri (CPU, memoria, processi)
		m.prometheusExporter.UpdateUserMetrics(
			uid,
			username,
			userMetrics.CPUUsage,
			userMetrics.MemoryUsage,
			userMetrics.ProcessCount,
			isLimited,
			cgroupPath,
			cpuQuota,
		)
	}

	// Pulisci metriche per utenti non più attivi
	activeUids := make(map[int]bool)
	for uid := range metrics.UserMetrics {
		activeUids[uid] = true
	}
	m.prometheusExporter.CleanupUserMetrics(activeUids)

	// Aggiorna metriche di sistema
	if load, err := m.getLoadAverage(); err == nil {
		m.prometheusExporter.UpdateSystemMetrics(metrics.TotalCores, load)
	}
}

// writeDatabaseMetrics scrive le metriche nel database SQLite (se abilitato)
func (m *Manager) writeDatabaseMetrics(metrics *SystemMetrics) {
	if m.metricsCollector == nil {
		return
	}

	// Verifica se il DB writer è configurato
	writer := m.metricsCollector.GetDBWriter()
	if writer == nil {
		return
	}

	// Verifica se è il momento di scrivere
	if !writer.ShouldWrite() {
		return
	}

	// Scrivi le metriche
	m.metricsCollector.WriteMetricsToDatabase(
		metrics.UserMetrics,
		metrics.TotalCPUUsage,
		metrics.TotalCores,
		0.0, // systemLoad non sempre disponibile
		m.limitsActive,
		len(m.activeUsers),
	)

	m.logger.Debug("Metrics written to database",
		"users", len(metrics.UserMetrics),
		"limits_active", m.limitsActive,
	)
}

// getUsername restituisce il nome utente dato l'UID
func (m *Manager) getUsername(uid int) string {
	return strconv.Itoa(uid)
}

// shouldApplyRAMLimits verifica se i limiti RAM dovrebbero essere applicati a un utente.
func (m *Manager) shouldApplyRAMLimits(uid int) bool {
	if !m.cfg.RAMEnabled {
		return false
	}
	username := m.getUsername(uid)
	return m.cfg.IsUserWhitelistedForRAM(username)
}

// GetUIDFromUsername risolve un username a UID scansionando /proc
// Restituisce 0 se l'utente non è trovato
func (m *Manager) GetUIDFromUsername(username string) int {
	if username == "" {
		return 0
	}

	// Usa metrics collector per ottenere tutti gli utenti attivi e i loro username
	allMetrics := m.metricsCollector.GetAllUserMetrics()
	for uid, metrics := range allMetrics {
		if metrics.Username == username {
			return uid
		}
	}

	return 0
}

// isUserLimited verifica se un utente ha limiti attivi
func (m *Manager) isUserLimited(uid int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.activeUsers[uid]
	return exists
}

// getLoadAverage restituisce il load average di 1 minuto
func (m *Manager) getLoadAverage() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0.0, err
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0.0, fmt.Errorf("invalid loadavg format")
	}

	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0.0, err
	}

	return load1, nil
}

// boolToFloat converte un booleano in float64 (1.0 per true, 0.0 per false).
func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

// GetStatus restituisce lo stato corrente del manager.
func (m *Manager) GetStatus() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := map[string]interface{}{
		"limits_active":        m.limitsActive,
		"limits_applied_time":  m.limitsAppliedTime.Format(time.RFC3339),
		"active_users_count":   len(m.activeUsers),
		"active_users":         m.getActiveUsersList(),
		"shared_cgroup_path":   m.sharedCgroupPath,
		"shared_cgroup_active": m.sharedCgroupPath != "",
	}

	// Aggiungi info sul cgroup condiviso se attivo
	if m.sharedCgroupPath != "" {
		// Leggi la quota corrente del cgroup condiviso
		cpuMaxFile := filepath.Join(m.sharedCgroupPath, "cpu.max")
		if data, err := os.ReadFile(cpuMaxFile); err == nil {
			status["shared_cgroup_quota"] = strings.TrimSpace(string(data))
		}

		// Conta i sottocgroup (utenti)
		if entries, err := os.ReadDir(m.sharedCgroupPath); err == nil {
			userCount := 0
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "user_") {
					userCount++
				}
			}
			status["shared_cgroup_user_count"] = userCount
		}
	}

	return status
}

// getActiveUsersList restituisce la lista degli UID attualmente limitati.
func (m *Manager) getActiveUsersList() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	users := make([]int, 0, len(m.activeUsers))
	for uid := range m.activeUsers {
		users = append(users, uid)
	}
	return users
}

// Cleanup esegue la pulizia prima dello shutdown.
func (m *Manager) Cleanup() error {
	m.logger.Info("Cleaning up state manager")

	// Wait for any pending goroutines
	m.wg.Wait()

	// Rimuovi tutti i limiti attivi
	if m.limitsActive {
		if err := m.deactivateLimits(); err != nil {
			m.logger.Error("Error during cleanup deactivation", "error", err)
			return err
		}
	}

	// Pulisci i cgroups
	if m.cgroupManager != nil {
		if err := m.cgroupManager.CleanupAll(); err != nil {
			m.logger.Error("Error during cgroup cleanup", "error", err)
			return err
		}
	}

	// Ferma l'esportatore Prometheus
	if m.prometheusExporter != nil {
		m.prometheusExporter.Stop()
	}

	m.logger.Info("State manager cleanup completed")
	return nil
}

// ForceActivateLimits attiva forzatamente i limiti (per testing/admin).
func (m *Manager) ForceActivateLimits() error {
	metrics, err := m.collectSystemMetrics()
	if err != nil {
		return err
	}
	return m.activateLimits(metrics)
}

// ForceDeactivateLimits disattiva forzatamente i limiti (per testing/admin).
func (m *Manager) ForceDeactivateLimits() error {
	return m.deactivateLimits()
}

// GetConfig returns the current configuration
func (m *Manager) GetConfig() *config.Config {
	return m.cfg
}

// ControlCycleEntry represents a single control cycle entry in history
type ControlCycleEntry struct {
	Timestamp     time.Time `json:"timestamp"`
	Decision      string    `json:"decision"`
	Reason        string    `json:"reason"`
	TotalCPUUsage float64   `json:"total_cpu_usage"`
	UserCPUUsage  float64   `json:"user_cpu_usage"`
	ActiveUsers   int       `json:"active_users"`
	LimitsActive  bool      `json:"limits_active"`
	DurationMs    int64     `json:"duration_ms"`
}

// controlHistory stores recent control cycle entries
type controlHistory struct {
	entries []ControlCycleEntry
	mu      sync.RWMutex
	maxSize int
}

var controlHist = &controlHistory{
	entries: make([]ControlCycleEntry, 0),
	maxSize: 100,
}

// addControlHistoryEntry adds an entry to the control history
func (m *Manager) addControlHistoryEntry(entry ControlCycleEntry) {
	controlHist.mu.Lock()
	defer controlHist.mu.Unlock()

	controlHist.entries = append(controlHist.entries, entry)

	// Keep only the last maxSize entries
	if len(controlHist.entries) > controlHist.maxSize {
		controlHist.entries = controlHist.entries[len(controlHist.entries)-controlHist.maxSize:]
	}
}

// GetControlHistory returns the recent control cycle history
func (m *Manager) GetControlHistory(limit int) []ControlCycleEntry {
	controlHist.mu.RLock()
	defer controlHist.mu.RUnlock()

	if limit <= 0 || limit > len(controlHist.entries) {
		limit = len(controlHist.entries)
	}

	// Return the most recent entries
	start := len(controlHist.entries) - limit
	if start < 0 {
		start = 0
	}

	result := make([]ControlCycleEntry, limit)
	copy(result, controlHist.entries[start:])
	return result
}

// recordControlCycle records a control cycle in history
func (m *Manager) recordControlCycle(decision, reason string, metrics *SystemMetrics, duration time.Duration) {
	m.mu.RLock()
	limitsActive := m.limitsActive
	m.mu.RUnlock()

	entry := ControlCycleEntry{
		Timestamp:     time.Now(),
		Decision:      decision,
		Reason:        reason,
		TotalCPUUsage: metrics.TotalCPUUsage,
		UserCPUUsage:  metrics.LimitedUsersCPUUsage,
		ActiveUsers:   len(metrics.UserCPUUsage),
		LimitsActive:  limitsActive,
		DurationMs:    duration.Milliseconds(),
	}

	m.addControlHistoryEntry(entry)
}

// Reset resetta il tracker
func (t *ThresholdTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.firstOverThresholdTime = time.Time{}
	t.overThresholdCycles = 0
	t.totalCycles = 0
}

// ShouldActivateLimits verifica se i limiti devono essere attivati
func (t *ThresholdTracker) ShouldActivateLimits(
	currentCPU float64,
	threshold float64,
	requiredDuration time.Duration,
) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if currentCPU >= threshold {
		t.overThresholdCycles++

		if t.firstOverThresholdTime.IsZero() {
			t.firstOverThresholdTime = time.Now()
		}

		elapsed := time.Since(t.firstOverThresholdTime)
		t.totalCycles++

		// Attiva solo se il tempo trascorso >= durata richiesta
		if elapsed >= requiredDuration {
			return true
		}
	} else {
		// CPU sotto soglia, reset
		t.firstOverThresholdTime = time.Time{}
		t.overThresholdCycles = 0
	}

	t.totalCycles++
	return false
}

// GetElapsed restituisce il tempo trascorso dal primo superamento
func (t *ThresholdTracker) GetElapsed() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.firstOverThresholdTime.IsZero() {
		return 0
	}
	return time.Since(t.firstOverThresholdTime)
}
