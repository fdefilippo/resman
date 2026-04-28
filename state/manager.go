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

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

// Manager coordina tutta la logica di gestione della CPU.
type Manager struct {
	cfg    *config.Config
	logger *logging.Logger
	mu     sync.RWMutex
	opMu   sync.Mutex
	wg     sync.WaitGroup

	// Stato interno
	limitsActive      bool
	limitsAppliedTime time.Time
	activeUsers       map[int]bool // UID -> se limitato
	sharedCgroupPath  string       // Percorso del cgroup condiviso

	// Threshold monitoring
	thresholdTracker    *ThresholdTracker
	ioThresholdTracker  *ThresholdTracker
	stabilityTracker    *UserStabilityTracker
	lastPatternAnalysis time.Time

	// Dipendenze (saranno iniettate)
	metricsCollector   MetricsCollector
	cgroupManager      CgroupManager
	prometheusExporter PrometheusExporter
	ioRemediation      *IORemediation
	patternDetector    *PatternDetector
	policyEngine       *PolicyEngine

	// Cache per le metriche (per performance)
	metricsCache     map[string]interface{}
	metricsCacheTime map[string]time.Time
	cacheMutex       sync.RWMutex

	// Control cycle history (inizializzato in NewManager)
	controlHist *controlHistory

	// IO rate tracking: cumulative bytes from /proc/[pid]/io -> per-second rate
	prevIOBytes map[int]uint64 // uid -> previous cycle cumulative write bytes
	prevIOTime  time.Time

	// PSI watcher for per-user adaptive CPU weight boosting
	psiWatcher   *cgroup.PSIWatcher
	psiBoostedAt map[int]time.Time // uid -> when last boosted
}

// ThresholdTracker monitora il superamento della soglia CPU nel tempo
// UserStabilityTracker monitora la stabilità dell'utente sotto soglia per il rilascio
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
	GetTotalMemoryMB() float64
	GetCachedMemoryMB() float64
	IsSystemUnderLoad() bool
	GetAllUserMetrics() map[int]*resmanmetrics.UserMetrics
	GetDBWriter() *resmanmetrics.DBWriter
	WriteMetricsToDatabase(userMetrics map[int]*resmanmetrics.UserMetrics, totalCPUUsage float64, totalCores int, systemLoad float64, limitsActive bool, limitedUsersCount int)
	GetUsernameFromUID(uid int) string
}

// CgroupManager è l'interfaccia per gestire i cgroups.
type CgroupManager interface {
	CreateUserCgroup(uid int) error
	ApplyCPULimit(uid int, quota string) error
	ApplyCPUWeight(uid int, weight int) error
	RemoveCPULimit(uid int) error
	ApplyRAMLimit(uid int, limit string) error
	ApplyRAMLimitWithSwapDisabled(uid int, limit string) error
	ApplyRAMHigh(uid int, limit string) error
	ApplyRAMLimitWithHigh(uid int, maxLimit string, highLimit string) error
	ApplyRAMLimitWithHighAndSwapDisabled(uid int, maxLimit string, highLimit string) error
	RemoveRAMLimit(uid int) error
	RemoveRAMHigh(uid int) error
	GetCgroupRAMUsage(uid int) (uint64, error)
	GetMemoryHighEvents(uid int) (uint64, error)
	ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error
	RemoveIOLimit(uid int) error
	GetIOStats(uid int) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error)
	GetUserCgroupMetrics(uid int) (cgroupPath, cpuQuota string, memoryHighEvents uint64, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64, err error)
	GetPSIStats(uid int) (cgroup.PSIStats, error)
	ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error
	CleanupUserCgroup(uid int) error
	MoveProcessToCgroup(pid int, uid int) error
	MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) error
	ReleaseUserFromSharedCgroup(uid int, sharedPath string) error
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
	UpdateUserMetrics(uid int, username string, cpuUsage float64, cpuUsageAverage float64, cpuUsageEMA float64, memoryUsage uint64, processCount int, isLimited bool, cgroupPath, cpuQuota string, memoryHighEvents uint64, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64)
	UpdateSystemMetrics(totalCores int, actionCores int, systemLoad float64)
	UpdateUserWorkloadPattern(uid int, username string, pattern string, confidence float64)
	RecordControlCycleTrigger(trigger string)
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
		return nil, fmt.Errorf("config cannot be nil: required for state manager initialization")
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
		stabilityTracker:   &UserStabilityTracker{underThreshold: make(map[int]int)},
		ioThresholdTracker: &ThresholdTracker{},
		metricsCollector:   metrics,
		cgroupManager:      cgroups,
		prometheusExporter: prometheus,
		ioRemediation:      NewIORemediation(logger),
		patternDetector:    NewPatternDetector(logger),
		policyEngine:       NewPolicyEngine(logger),
		metricsCache:       make(map[string]interface{}),
		metricsCacheTime:   make(map[string]time.Time),
		controlHist: &controlHistory{
			entries: make([]ControlCycleEntry, 0),
			maxSize: 100,
		},
		prevIOBytes:  make(map[int]uint64),
		psiBoostedAt: make(map[int]time.Time),
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
// SystemMetrics contiene tutte le metriche raccolte in un ciclo.
// collectSystemMetrics raccoglie tutte le metriche di sistema necessarie.
// makeDecision prende la decisione se attivare, mantenere o disattivare i limiti.
// buildActivateReason costruisce la ragione di attivazione basata sulle risorse che superano la soglia.
// buildDeactivateReason costruisce la ragione di disattivazione.
// executeDecision esegue l'azione corrispondente alla decisione presa.
// e riaggiunge utenti che hanno superato la soglia di idle dopo essere stati rilasciati.
// activateLimits attiva i limiti di CPU per gli utenti attivi usando pesi proporzionali.
// deactivateLimits rimuove i limiti di CPU da tutti gli utenti.
// updatePrometheusMetrics aggiorna le metriche per Prometheus.
// writeDatabaseMetrics scrive le metriche nel database SQLite (se abilitato)
// getUsername restituisce il nome utente dato l'UID
func (m *Manager) getUsername(uid int) string {
	if m.metricsCollector != nil {
		return m.metricsCollector.GetUsernameFromUID(uid)
	}
	return strconv.Itoa(uid)
}

// shouldApplyRAMLimits verifica se i limiti RAM dovrebbero essere applicati a un utente.
// shouldApplyIOLimits verifica se i limiti IO devono essere applicati per l'utente.
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
// ForceDeactivateLimits disattiva forzatamente i limiti (per testing/admin).
// UpdateConfig aggiorna la configurazione del manager.
func (m *Manager) UpdateConfig(newConfig *config.Config) {
	if newConfig == nil {
		return
	}
	m.mu.Lock()
	m.cfg = newConfig
	m.mu.Unlock()

	m.logger.Info("State manager configuration updated",
		"polling_interval", newConfig.PollingInterval,
		"cpu_threshold", newConfig.CPUThreshold,
		"cpu_release_threshold", newConfig.CPUReleaseThreshold,
		"cpu_threshold_duration", newConfig.CPUThresholdDuration,
	)
}

// RegisterPSIWatcher sets the PSI watcher for per-user cgroup monitoring.
func (m *Manager) RegisterPSIWatcher(w *cgroup.PSIWatcher) {
	m.psiWatcher = w
}

// OnUserPSIEvent handles a per-user PSI pressure event by boosting CPU weight.
func (m *Manager) OnUserPSIEvent(event cgroup.PSIEvent) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if event.UID <= 0 {
		return
	}
	cfg := m.GetConfig()
	boostWeight := cfg.GetPSIBoostWeight()

	m.mu.RLock()
	active := m.activeUsers[event.UID]
	m.mu.RUnlock()
	if !active {
		m.logger.Debug("Ignoring PSI event for user without active limits",
			"uid", event.UID, "type", event.Type)
		return
	}

	if err := m.cgroupManager.ApplyCPUWeight(event.UID, boostWeight); err != nil {
		m.logger.Warn("Failed to boost CPU weight on PSI event",
			"uid", event.UID, "type", event.Type,
			"weight", boostWeight, "error", err)
		return
	}

	m.mu.Lock()
	m.psiBoostedAt[event.UID] = time.Now()
	m.mu.Unlock()

	m.logger.Info("CPU weight boosted for user due to PSI pressure",
		"uid", event.UID, "type", event.Type,
		"psi_avg10", event.SomeAvg10, "weight", boostWeight)
}

// revertPSIBoosts reverts CPU weight for users whose boost duration has expired.
func (m *Manager) revertPSIBoosts() {
	cfg := m.GetConfig()
	duration := time.Duration(cfg.GetPSIBoostDuration()) * time.Second
	now := time.Now()

	// Collect expired UIDs under lock, do cgroup IO outside lock
	m.mu.Lock()
	var expired []int
	for uid, boostedAt := range m.psiBoostedAt {
		if now.Sub(boostedAt) >= duration {
			expired = append(expired, uid)
		}
	}
	m.mu.Unlock()

	if len(expired) == 0 {
		return
	}

	for _, uid := range expired {
		if err := m.cgroupManager.ApplyCPUWeight(uid, 100); err != nil {
			m.logger.Warn("Failed to revert CPU weight after PSI boost",
				"uid", uid, "error", err)
			continue
		}
		m.logger.Debug("CPU weight reverted to normal after PSI boost expired",
			"uid", uid, "boost_duration_s", cfg.GetPSIBoostDuration())
	}

	// Clean up expired entries
	m.mu.Lock()
	for _, uid := range expired {
		delete(m.psiBoostedAt, uid)
	}
	m.mu.Unlock()
}

// GetConfig returns the current configuration
func (m *Manager) GetConfig() *config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func isMissingUserCgroupError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cgroup for UID") && strings.Contains(err.Error(), "not found")
}

// ControlCycleEntry represents a single control cycle entry in history
// GetControlHistory returns the recent control cycle history
// recordControlCycle records a control cycle in history
// Reset resetta il tracker
// ShouldActivateLimits checks if limits should be activated based on threshold duration.
// GetElapsed restituisce il tempo trascorso dal primo superamento
