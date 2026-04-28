package state

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

// ThresholdTracker monitora il superamento della soglia CPU nel tempo
type ThresholdTracker struct {
	firstOverThresholdTime time.Time // Primo superamento soglia
	overThresholdCycles    int       // Cicli sopra soglia
	totalCycles            int       // Cicli totali
	mu                     sync.RWMutex
}


type UserStabilityTracker struct {
	mu             sync.RWMutex
	underThreshold map[int]int // uid -> campioni consecutivi sotto soglia
}


func (m *Manager) makeDecision(metrics *SystemMetrics) (string, string) {
	cfg := m.GetConfig()
	m.mu.RLock()
	limitsActive := m.limitsActive
	limitsAppliedTime := m.limitsAppliedTime
	m.mu.RUnlock()

	// Get configuration values atomically to prevent inconsistency during reload
	minActiveTime := cfg.GetMinActiveTime()
	cpuReleaseThreshold := cfg.GetCPUReleaseThreshold()
	cpuThreshold := cfg.GetCPUThreshold()
	minSystemCores := cfg.GetMinSystemCores()
	ignoreSystemLoad := cfg.GetIgnoreSystemLoad()
	cpuThresholdDuration := cfg.GetCPUThresholdDuration()

	// Decisioni possibili
	const (
		DecisionActivate   = "ACTIVATE_LIMITS"
		DecisionMaintain   = "MAINTAIN_CURRENT_STATE"
		DecisionDeactivate = "DEACTIVATE_LIMITS"
	)

	// Calcola se ogni risorsa supera la soglia
	cpuExceeded := metrics.LimitedUsersCPUUsage >= float64(cpuThreshold)

	ramExceeded := false
	if cfg.RAMEnabled && cfg.RAMThreshold > 0 && metrics.TotalMemoryMB > 0 {
		limitedRAMMB := float64(metrics.LimitedUsersRAMUsageBytes) / (1024 * 1024)
		ramPercent := (limitedRAMMB / metrics.TotalMemoryMB) * 100
		ramExceeded = ramPercent >= float64(cfg.RAMThreshold)
	}

	ioExceeded := false
	ioPercent := 0.0
	ioThresholdDuration := cfg.GetIOThresholdDuration()
	if cfg.IOEnabled && cfg.IOThreshold > 0 && cfg.IOWriteBPS != "" && cfg.IOWriteBPS != "max" {
		writeLimit, err := config.ParseRAMQuota(cfg.IOWriteBPS)
		if err == nil && writeLimit > 0 {
			totalWriteLimit := writeLimit * uint64(metrics.LimitedUsersCount)
			if totalWriteLimit > 0 {
				ioPercent = float64(metrics.LimitedUsersIOWriteBytes) / float64(totalWriteLimit) * 100
				ioExceeded = ioPercent >= float64(cfg.IOThreshold)
			}
		}
	}

	// Applica IO threshold duration se configurata
	if ioThresholdDuration > 0 && ioExceeded {
		ioTrackerReady := m.ioThresholdTracker.ShouldActivateLimits(
			ioPercent,
			float64(cfg.IOThreshold),
			time.Duration(ioThresholdDuration)*time.Second,
		)
		if !ioTrackerReady {
			// IO sopra soglia ma non ancora per abbastanza tempo
			ioExceeded = false
		}
	} else {
		// IO sotto soglia o duration disabilitata: reset tracker
		m.ioThresholdTracker.Reset()
	}

	anyExceeded := cpuExceeded || ramExceeded || ioExceeded

	// Calcola se ogni risorsa è sotto la soglia di rilascio
	cpuBelow := metrics.LimitedUsersCPUUsage < float64(cpuReleaseThreshold)

	ramBelow := true
	if cfg.RAMEnabled && cfg.RAMReleaseThreshold > 0 && metrics.TotalMemoryMB > 0 {
		limitedRAMMB := float64(metrics.LimitedUsersRAMUsageBytes) / (1024 * 1024)
		ramPercent := (limitedRAMMB / metrics.TotalMemoryMB) * 100
		ramBelow = ramPercent < float64(cfg.RAMReleaseThreshold)
	}

	ioBelow := true
	if cfg.IOEnabled && cfg.IOReleaseThreshold > 0 && cfg.IOWriteBPS != "" && cfg.IOWriteBPS != "max" {
		writeLimit, err := config.ParseRAMQuota(cfg.IOWriteBPS)
		if err == nil && writeLimit > 0 {
			totalWriteLimit := writeLimit * uint64(metrics.LimitedUsersCount)
			if totalWriteLimit > 0 {
				ioPercent := float64(metrics.LimitedUsersIOWriteBytes) / float64(totalWriteLimit) * 100
				ioBelow = ioPercent < float64(cfg.IOReleaseThreshold)
			}
		}
	}

	allBelow := cpuBelow && ramBelow && ioBelow

	// Se i limiti sono attivi, controlliamo se possiamo disattivarli
	if limitsActive {
		// Verifica il tempo minimo di attivazione
		if time.Since(limitsAppliedTime) < time.Duration(minActiveTime)*time.Second {
			return DecisionMaintain, "Limits active, waiting for minimum activation time"
		}

		// Disattiva solo quando TUTTE le risorse sono sotto le soglie di rilascio
		if allBelow {
			if m.stabilityTracker == nil {
				m.stabilityTracker = &UserStabilityTracker{underThreshold: make(map[int]int)}
			}

			// Verifica stabilità per CPU (evita rilasci nervosi per singoli campioni a 0%)
			// Richiediamo 3 campionamenti consecutivi sotto soglia (~90 secondi)
			m.stabilityTracker.mu.Lock()
			defer m.stabilityTracker.mu.Unlock()

			// Troviamo l'utente con l'uso CPU più alto tra i limitati per decidere il rilascio globale
			var limitedUsers []int
			allUserMetrics := make(map[int]*resmanmetrics.UserMetrics)
			if m.metricsCollector != nil {
				limitedUsers = m.metricsCollector.GetLimitedUsers()
				allUserMetrics = m.metricsCollector.GetAllUserMetrics()
			}

			for _, uid := range limitedUsers {
				if um, ok := allUserMetrics[uid]; ok {
					if um.CPUUsageEMA < float64(cpuReleaseThreshold) {
						m.stabilityTracker.underThreshold[uid]++
					} else {
						m.stabilityTracker.underThreshold[uid] = 0
					}
				}
			}

			// Se tutti gli utenti limitati sono stabili sotto soglia per almeno 3 cicli
			stable := true
			for _, uid := range limitedUsers {
				if m.stabilityTracker.underThreshold[uid] < 3 {
					stable = false
					break
				}
			}

			if !metrics.SystemUnderLoad && stable {
				m.thresholdTracker.Reset()
				m.ioThresholdTracker.Reset()
				return DecisionDeactivate, m.buildDeactivateReason(cpuBelow, ramBelow, ioBelow, metrics, cpuReleaseThreshold)
			}
			if !stable {
				return DecisionMaintain, "Resources below thresholds but waiting for stability (cool-down period)"
			}
			return DecisionMaintain, "Resources below thresholds but system still under load"
		}

		return DecisionMaintain, "Limits active, at least one resource still above release threshold"
	}

	// Se i limiti non sono attivi, attiva se QUALSIASI risorsa supera la soglia
	if anyExceeded {
		// Verifica che ci siano abbastanza core per il sistema
		if metrics.TotalCores <= minSystemCores {
			m.thresholdTracker.Reset()
			m.ioThresholdTracker.Reset()
			return DecisionMaintain, fmt.Sprintf(
				"Threshold exceeded but insufficient cores (%d <= %d)",
				metrics.TotalCores, minSystemCores,
			)
		}

		// Verifica se dobbiamo ignorare il load average
		if !ignoreSystemLoad && metrics.SystemUnderLoad {
			m.thresholdTracker.Reset()
			m.ioThresholdTracker.Reset()
			return DecisionMaintain, "Threshold exceeded but system already under load from other factors"
		}

		// Verifica time window (solo per CPU, se configurata)
		// Blocca l'attivazione solo se CPU è l'unica risorsa sopra soglia
		if cpuExceeded && cpuThresholdDuration > 0 {
			shouldActivate := m.thresholdTracker.ShouldActivateLimits(
				metrics.LimitedUsersCPUUsage,
				float64(cpuThreshold),
				time.Duration(cpuThresholdDuration)*time.Second,
			)
			if !shouldActivate && !ramExceeded && !ioExceeded {
				// Solo CPU sopra soglia e non ancora per abbastanza tempo
				elapsed := m.thresholdTracker.GetElapsed()
				remaining := time.Duration(cpuThresholdDuration)*time.Second - elapsed
				return DecisionMaintain, fmt.Sprintf(
					"CPU threshold exceeded, waiting %s before activating limits (%.1f%% >= %d%%)",
					remaining.Round(time.Second),
					metrics.LimitedUsersCPUUsage, cpuThreshold,
				)
			}
		}

		return DecisionActivate, m.buildActivateReason(cpuExceeded, ramExceeded, ioExceeded, metrics, cpuThreshold)
	}

	// Nessuna risorsa supera la soglia, reset tracker
	m.thresholdTracker.Reset()
	m.ioThresholdTracker.Reset()
	return DecisionMaintain, "All resources within normal range"
}


func (m *Manager) buildActivateReason(cpuExceeded, ramExceeded, ioExceeded bool, metrics *SystemMetrics, cpuThreshold int) string {
	cfg := m.GetConfig()
	reasons := []string{}
	if cpuExceeded {
		reasons = append(reasons, fmt.Sprintf("CPU %.1f%% >= %d%%", metrics.LimitedUsersCPUUsage, cpuThreshold))
	}
	if ramExceeded {
		reasons = append(reasons, fmt.Sprintf("RAM >= %d%%", cfg.RAMThreshold))
	}
	if ioExceeded {
		reasons = append(reasons, fmt.Sprintf("IO >= %d%%", cfg.IOThreshold))
	}
	return fmt.Sprintf("Threshold exceeded: %s", strings.Join(reasons, ", "))
}


func (m *Manager) buildDeactivateReason(cpuBelow, ramBelow, ioBelow bool, metrics *SystemMetrics, cpuReleaseThreshold int) string {
	return fmt.Sprintf(
		"All resources below release thresholds (CPU %.1f%% < %d%%)",
		metrics.LimitedUsersCPUUsage, cpuReleaseThreshold,
	)
}


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
		return fmt.Errorf("unknown decision '%s': expected ACTIVATE_LIMITS, DEACTIVATE_LIMITS, or MAINTAIN_CURRENT_STATE", decision)
	}
}

// releaseIdleUsers rilascia gli utenti che non stanno usando CPU mentre i limiti sono attivi

func (t *ThresholdTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.firstOverThresholdTime = time.Time{}
	t.overThresholdCycles = 0
	t.totalCycles = 0
}

// ShouldActivateLimits checks if limits should be activated based on threshold duration.
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

		// Activate only if elapsed time >= required duration
		if elapsed >= requiredDuration {
			return true
		}
	} else {
		// CPU below threshold, reset tracker
		t.firstOverThresholdTime = time.Time{}
		t.overThresholdCycles = 0
	}

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

