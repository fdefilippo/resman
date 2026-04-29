package state

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

const (
	ControlCycleTriggerInitial = "initial"
	ControlCycleTriggerTicker  = "ticker"
	ControlCycleTriggerManual  = "manual"
)

type controlCycleContext struct {
	ctx                context.Context
	cfg                *config.Config
	trigger            string
	startTime          time.Time
	cycleID            int64
	metrics            *SystemMetrics
	decision           string
	reason             string
	duration           time.Duration
	activeLimitedUsers int
	stopWithoutError   bool
}

type controlCycleStage func(*Manager, *controlCycleContext) error

var defaultControlCyclePipeline = []controlCycleStage{
	(*Manager).stageCheckBlackout,
	(*Manager).stageCollectMetrics,
	(*Manager).stageUpdatePrometheus,
	(*Manager).stageWriteDatabase,
	(*Manager).stageMakeDecision,
	(*Manager).stageExecuteDecision,
	(*Manager).stageRecordHistory,
	(*Manager).stageIORemediation,
	(*Manager).stageWorkloadPatternDetection,
	(*Manager).stageRevertPSIBoosts,
	(*Manager).stageLogCompletion,
}

func (m *Manager) RunControlCycle(ctx context.Context) error {
	return m.RunControlCycleWithTrigger(ctx, ControlCycleTriggerManual)
}

// RunMetricsRefresh aggiorna solo le metriche Prometheus/Grafana senza decisioni.
func (m *Manager) RunMetricsRefresh(ctx context.Context, trigger string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if trigger == "" {
		trigger = "metrics_refresh"
	}

	startTime := time.Now()
	metrics, err := m.collectSystemMetricsForRefresh()
	if err != nil {
		m.logger.Error("Failed to collect metrics for refresh",
			"trigger", trigger,
			"error", err,
		)
		return fmt.Errorf("failed to collect metrics for refresh: %w", err)
	}

	if m.prometheusExporter != nil {
		m.updatePrometheusMetrics(metrics)
	}

	m.logger.Debug("Metrics refresh completed",
		"trigger", trigger,
		"duration_ms", time.Since(startTime).Milliseconds(),
	)

	return nil
}

// RunControlCycleWithTrigger esegue un ciclo indicando il motivo che lo ha avviato.
func (m *Manager) RunControlCycleWithTrigger(ctx context.Context, trigger string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if trigger == "" {
		trigger = ControlCycleTriggerManual
	}

	run := &controlCycleContext{
		ctx:       ctx,
		cfg:       m.GetConfig(),
		trigger:   trigger,
		startTime: time.Now(),
	}
	run.cycleID = run.startTime.Unix()

	if m.prometheusExporter != nil {
		m.prometheusExporter.RecordControlCycleTrigger(trigger)
	}

	m.logger.Debug("Starting control cycle", "cycle_id", run.cycleID, "trigger", trigger)

	for _, stage := range defaultControlCyclePipeline {
		if err := stage(m, run); err != nil {
			return err
		}
		if run.stopWithoutError {
			return nil
		}
	}

	return nil
}

func (m *Manager) stageCheckBlackout(run *controlCycleContext) error {
	// Controlla se siamo in un blackout timeframe
	if run.cfg.IsInBlackout() {
		nextEnd := run.cfg.GetNextBlackoutEnd()
		if nextEnd != nil {
			m.logger.Info("Skipping control cycle - blackout timeframe active",
				"cycle_id", run.cycleID,
				"trigger", run.trigger,
				"next_check", nextEnd.Format("2006-01-02 15:04:05"),
			)
		} else {
			m.logger.Debug("Skipping control cycle - blackout timeframe active",
				"cycle_id", run.cycleID,
				"trigger", run.trigger,
			)
		}
		run.stopWithoutError = true
	}
	return nil
}

func (m *Manager) stageCollectMetrics(run *controlCycleContext) error {
	// 1. Raccogli metriche del sistema
	metrics, err := m.collectSystemMetrics()
	if err != nil {
		m.logger.Error("Failed to collect system metrics",
			"cycle_id", run.cycleID,
			"trigger", run.trigger,
			"error", err,
		)
		return fmt.Errorf("failed to collect system metrics (cycle %d): %w", run.cycleID, err)
	}
	run.metrics = metrics
	return nil
}

func (m *Manager) stageUpdatePrometheus(run *controlCycleContext) error {
	// 2. Aggiorna le metriche Prometheus (se abilitato)
	if m.prometheusExporter != nil {
		m.updatePrometheusMetrics(run.metrics)
	}
	return nil
}

func (m *Manager) stageWriteDatabase(run *controlCycleContext) error {
	// 3. Scrivi le metriche nel database (se abilitato)
	m.writeDatabaseMetrics(run.metrics)
	return nil
}

func (m *Manager) stageMakeDecision(run *controlCycleContext) error {
	// 4. Prendi decisione basata sulle metriche
	run.decision, run.reason = m.makeDecision(run.metrics)
	return nil
}

func (m *Manager) stageExecuteDecision(run *controlCycleContext) error {
	// 4. Esegui l'azione corrispondente
	if err := m.executeDecision(run.decision, run.metrics); err != nil {
		m.logger.Error("Failed to execute decision",
			"decision", run.decision,
			"reason", run.reason,
			"cycle_id", run.cycleID,
			"trigger", run.trigger,
			"error", err,
		)
		return fmt.Errorf("failed to execute decision %s (cycle %d): %w", run.decision, run.cycleID, err)
	}
	return nil
}

func (m *Manager) stageRecordHistory(run *controlCycleContext) error {
	// 6. Registra lo storico del ciclo
	run.duration = time.Since(run.startTime)
	m.recordControlCycle(run.decision, run.reason, run.metrics, run.duration)
	return nil
}

func (m *Manager) stageIORemediation(run *controlCycleContext) error {
	// 7. IO Starvation Auto-Remediation
	if m.ioRemediation != nil {
		limitedUsers := m.metricsCollector.GetLimitedUsers()
		m.ioRemediation.CheckAndRemediate(m.cgroupManager, run.cfg, limitedUsers)
		// Cleanup periodico stati vecchi
		m.ioRemediation.Cleanup(24 * time.Hour)
	}
	return nil
}

func (m *Manager) stageWorkloadPatternDetection(run *controlCycleContext) error {
	// 8. Workload Pattern Detection
	if run.cfg.GetAutodetectPatterns() && m.patternDetector != nil && m.policyEngine != nil {
		// Aggiorna statistiche per tutti gli utenti
		allMetrics := m.metricsCollector.GetAllUserMetrics()
		for uid, um := range allMetrics {
			m.patternDetector.Update(uid, um.CPUUsage)
		}

		// Analizza pattern ogni ora
		if time.Since(m.lastPatternAnalysis) > time.Hour {
			m.lastPatternAnalysis = time.Now()
			patterns := m.patternDetector.Analyze(run.cfg)
			for uid, result := range patterns {
				if m.prometheusExporter != nil {
					username := m.metricsCollector.GetUsernameFromUID(uid)
					m.prometheusExporter.UpdateUserWorkloadPattern(uid, username, string(result.Pattern), result.Confidence)
				}
				if result.Pattern != PatternUnknown {
					if m.policyEngine.ApplyPolicy(uid, result.Pattern, run.cfg) {
						// Policy cambiata, applica limiti
						policy, _ := m.policyEngine.GetPolicy(uid)
						if policy != nil {
							// Applica CPU quota
							if policy.CPUQuota > 0 {
								quotaStr := strconv.Itoa(policy.CPUQuota) + " 100000"
								if err := m.cgroupManager.ApplyCPULimit(uid, quotaStr); err != nil {
									m.logger.Warn("Failed to apply pattern-based CPU limit",
										"uid", uid,
										"pattern", result.Pattern,
										"error", err,
									)
								}
							}
							// Applica RAM quota
							if policy.RAMQuota != "" {
								if err := m.cgroupManager.ApplyRAMLimit(uid, policy.RAMQuota); err != nil {
									m.logger.Warn("Failed to apply pattern-based RAM limit",
										"uid", uid,
										"pattern", result.Pattern,
										"ram_quota", policy.RAMQuota,
										"error", err,
									)
								}
							}
						}
					}
				}
			}
			// Cleanup pattern detector
			m.patternDetector.Cleanup(time.Duration(run.cfg.GetPatternHistoryHours()) * time.Hour)
			m.policyEngine.Cleanup(24 * time.Hour)
		}
	}
	return nil
}

func (m *Manager) stageRevertPSIBoosts(run *controlCycleContext) error {
	// 9a. Revert PSI weight boosts that have expired
	if m.psiWatcher != nil {
		m.revertPSIBoosts()
	}
	return nil
}

func (m *Manager) stageLogCompletion(run *controlCycleContext) error {
	m.mu.RLock()
	run.activeLimitedUsers = len(m.activeUsers)
	m.mu.RUnlock()

	// 9. Logga il risultato del ciclo
	m.logger.Info("Control cycle completed",
		"cycle_id", run.cycleID,
		"trigger", run.trigger,
		"decision", run.decision,
		"reason", run.reason,
		"total_cpu_usage", run.metrics.TotalCPUUsage,
		"limited_users_cpu_usage", run.metrics.LimitedUsersCPUUsage,
		"eligible_users", run.metrics.LimitedUsersCount,
		"active_limited_users", run.activeLimitedUsers,
		"system_under_load", run.metrics.SystemUnderLoad,
		"ignore_system_load", run.cfg.GetIgnoreSystemLoad(),
		"duration_ms", run.duration.Milliseconds(),
	)

	return nil
}

type SystemMetrics struct {
	Timestamp     time.Time
	TotalCores    int
	TotalCPUUsage float64 // Percentuale

	// ALL USERS metrics (tutti gli utenti non-system, UID >= SYSTEM_UID_MIN)
	AllUsersCPUUsage    float64
	AllUsersMemoryUsage uint64
	AllUsersCount       int

	// LIMITED USERS metrics (solo utenti che passano i filtri)
	LimitedUsersCPUUsage    float64
	LimitedUsersMemoryUsage uint64
	LimitedUsersCount       int

	// RAM/IO aggregates for limited users (for threshold decisions)
	LimitedUsersRAMUsageBytes uint64
	LimitedUsersIOWriteBytes  uint64

	MemoryUsage     float64 // MB
	TotalMemoryMB   float64 // MB
	CachedMemoryMB  float64 // MB
	SystemUnderLoad bool
	UserCPUUsage    map[int]float64                    // UID -> percentuale
	UserMetrics     map[int]*resmanmetrics.UserMetrics // Metriche dettagliate per utente
	EligibleUsers   []int                              // Users passing USER_INCLUDE/USER_EXCLUDE filters
}

func (m *Manager) collectSystemMetrics() (*SystemMetrics, error) {
	return m.collectSystemMetricsWithIOState(true)
}

func (m *Manager) collectSystemMetricsForRefresh() (*SystemMetrics, error) {
	return m.collectSystemMetricsWithIOState(false)
}

func (m *Manager) collectSystemMetricsWithIOState(updateIOState bool) (*SystemMetrics, error) {
	metrics := &SystemMetrics{
		Timestamp:    time.Now(),
		UserCPUUsage: make(map[int]float64),
		UserMetrics:  make(map[int]*resmanmetrics.UserMetrics),
	}

	// Raccogli metriche di base
	metrics.TotalCores = m.metricsCollector.GetTotalCores()
	metrics.TotalCPUUsage = m.metricsCollector.GetTotalCPUUsage()

	metrics.MemoryUsage = m.metricsCollector.GetMemoryUsage()
	metrics.TotalMemoryMB = m.metricsCollector.GetTotalMemoryMB()
	metrics.CachedMemoryMB = m.metricsCollector.GetCachedMemoryMB()
	metrics.SystemUnderLoad = m.metricsCollector.IsSystemUnderLoad()

	// Raccogli metriche dettagliate per ogni utente (CPU, memoria, processi) in una sola chiamata
	allUserMetrics := m.metricsCollector.GetAllUserMetrics()

	// Singola passata per calcolare tutti gli aggregati:
	// - AllUsers (CPU, memory, count)
	// - EligibleUsers (IsLimited == true dal collector)
	// - LimitedUsers (runtime active)
	// - UserMetrics e UserCPUUsage sovrascrivendo IsLimited con stato runtime
	for uid, um := range allUserMetrics {
		metrics.AllUsersCPUUsage += um.CPUUsage
		metrics.AllUsersMemoryUsage += um.MemoryUsage
		metrics.AllUsersCount++

		metrics.UserCPUUsage[uid] = um.CPUUsage

		// FIX M2: Override IsLimited based on actual runtime state, not config
		m.mu.RLock()
		actuallyLimited := m.activeUsers[uid]
		m.mu.RUnlock()

		corrected := &resmanmetrics.UserMetrics{
			UID:             um.UID,
			Username:        um.Username,
			CPUUsage:        um.CPUUsage,
			CPUUsageAverage: um.CPUUsageAverage,
			CPUUsageEMA:     um.CPUUsageEMA,
			MemoryUsage:     um.MemoryUsage,
			ProcessCount:    um.ProcessCount,
			IsLimited:       actuallyLimited,
			IOReadBytes:     um.IOReadBytes,
			IOWriteBytes:    um.IOWriteBytes,
			IOReadOps:       um.IOReadOps,
			IOWriteOps:      um.IOWriteOps,
		}
		metrics.UserMetrics[uid] = corrected

		// Eligible users: quelli che superano i filtri di configurazione
		if um.IsLimited {
			metrics.EligibleUsers = append(metrics.EligibleUsers, uid)
			metrics.LimitedUsersCPUUsage += um.CPUUsage
			metrics.LimitedUsersMemoryUsage += um.MemoryUsage
			metrics.LimitedUsersRAMUsageBytes += um.MemoryUsage

			if updateIOState {
				// Calcola IO rate (bytes/sec) dal delta rispetto al ciclo decisionale precedente.
				ioDelta := um.IOWriteBytes
				if prev, ok := m.prevIOBytes[uid]; ok && !m.prevIOTime.IsZero() {
					elapsed := time.Since(m.prevIOTime).Seconds()
					if elapsed > 0 && ioDelta >= prev {
						ioRate := float64(ioDelta-prev) / elapsed
						metrics.LimitedUsersIOWriteBytes += uint64(ioRate)
					}
				}
				m.prevIOBytes[uid] = ioDelta
			}
		}
	}
	metrics.LimitedUsersCount = len(metrics.EligibleUsers)

	if updateIOState {
		m.prevIOTime = time.Now()

		// Pulisci prevIOBytes per utenti non più attivi
		for uid := range m.prevIOBytes {
			if _, exists := allUserMetrics[uid]; !exists {
				delete(m.prevIOBytes, uid)
			}
		}
	}

	return metrics, nil
}

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
		"memory_usage_mb":  metrics.MemoryUsage,
		"total_memory_mb":  metrics.TotalMemoryMB,
		"cached_memory_mb": metrics.CachedMemoryMB,
		"limited_users":    float64(len(m.activeUsers)),
		"limits_active":    boolToFloat(m.limitsActive),
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

		// Batch cgroup reads: single call instead of 3 separate ones
		var cgroupPath, cpuQuota string
		var memoryHighEvents uint64
		var cgroupIOReadBytes, cgroupIOWriteBytes, cgroupIOReadOps, cgroupIOWriteOps uint64
		if m.cgroupManager != nil {
			var err error
			cgroupPath, cpuQuota, memoryHighEvents, cgroupIOReadBytes, cgroupIOWriteBytes, cgroupIOReadOps, cgroupIOWriteOps, err = m.cgroupManager.GetUserCgroupMetrics(uid)
			if err != nil {
				if isMissingUserCgroupError(err) {
					m.logger.Debug("Cgroup metrics unavailable for user without cgroup", "uid", uid)
				} else {
					m.logger.Warn("Failed to get cgroup metrics for user", "uid", uid, "error", err)
				}
			}
		}

		// Use per-user IO from GetAllUserMetrics
		ioReadBytes := userMetrics.IOReadBytes
		ioWriteBytes := userMetrics.IOWriteBytes
		ioReadOps := userMetrics.IOReadOps
		ioWriteOps := userMetrics.IOWriteOps
		if ioReadBytes == 0 && ioWriteBytes == 0 && cgroupIOReadBytes > 0 {
			ioReadBytes = cgroupIOReadBytes
			ioWriteBytes = cgroupIOWriteBytes
			ioReadOps = cgroupIOReadOps
			ioWriteOps = cgroupIOWriteOps
		}

		// Usa UpdateUserMetrics con tutti i parametri
		m.prometheusExporter.UpdateUserMetrics(
			uid,
			username,
			userMetrics.CPUUsage,
			userMetrics.CPUUsageAverage,
			userMetrics.CPUUsageEMA,
			userMetrics.MemoryUsage,
			userMetrics.ProcessCount,
			isLimited,
			cgroupPath,
			cpuQuota,
			memoryHighEvents,
			ioReadBytes,
			ioWriteBytes,
			ioReadOps,
			ioWriteOps,
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
		actionCores := metrics.TotalCores - m.GetConfig().GetMinSystemCores()
		if actionCores < 1 {
			actionCores = 1
		}
		m.prometheusExporter.UpdateSystemMetrics(metrics.TotalCores, actionCores, load)
	}
}

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

func (m *Manager) addControlHistoryEntry(entry ControlCycleEntry) {
	m.controlHist.mu.Lock()
	defer m.controlHist.mu.Unlock()

	m.controlHist.entries = append(m.controlHist.entries, entry)

	// Keep only the last maxSize entries
	if len(m.controlHist.entries) > m.controlHist.maxSize {
		m.controlHist.entries = m.controlHist.entries[len(m.controlHist.entries)-m.controlHist.maxSize:]
	}
}

func (m *Manager) GetControlHistory(limit int) []ControlCycleEntry {
	m.controlHist.mu.RLock()
	defer m.controlHist.mu.RUnlock()

	if limit <= 0 || limit > len(m.controlHist.entries) {
		limit = len(m.controlHist.entries)
	}

	// Return the most recent entries
	start := len(m.controlHist.entries) - limit
	if start < 0 {
		start = 0
	}

	result := make([]ControlCycleEntry, limit)
	copy(result, m.controlHist.entries[start:])
	return result
}

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
