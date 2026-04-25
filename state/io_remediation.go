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
// state/io_remediation.go
package state

import (
	"sync"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

// IOBoostState tiene traccia dello stato di boost per un utente.
type IOBoostState struct {
	IsActive          bool
	StartTime         time.Time
	OriginalReadBPS   string
	OriginalWriteBPS  string
	OriginalReadIOPS  int
	OriginalWriteIOPS int
	BoostCount        int       // Numero di boost nell'ultima ora
	LastBoostTime     time.Time // Ultimo boost applicato
	StarvationStart   time.Time // Quando e' iniziata la starvation
}

// IORemediation gestisce il rilevamento e la remediation della IO starvation.
type IORemediation struct {
	mu          sync.RWMutex
	logger      *logging.Logger
	boostStates map[int]*IOBoostState // uid -> stato boost
	lastCheck   time.Time
}

// NewIORemediation crea una nuova istanza di IORemediation.
func NewIORemediation(logger *logging.Logger) *IORemediation {
	return &IORemediation{
		logger:      logger,
		boostStates: make(map[int]*IOBoostState),
	}
}

// IORemediationDeps contiene le dipendenze necessarie per la remediation.
type IORemediationDeps interface {
	GetPSIStats(uid int) (cgroup.PSIStats, error)
	GetIOStats(uid int) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error)
	ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error
	RemoveIOLimit(uid int) error
}

// CheckAndRemediate verifica la IO starvation per tutti gli utenti e applica remediation se necessario.
// Deve essere chiamato periodicamente dal control cycle.
func (r *IORemediation) CheckAndRemediate(deps IORemediationDeps, cfg *config.Config, limitedUsers []int) {
	if !cfg.GetIORemediationEnabled() {
		return
	}

	now := time.Now()
	checkInterval := time.Duration(cfg.GetIOStarvationCheckInterval()) * time.Second

	// Rispetta il check interval
	if now.Sub(r.lastCheck) < checkInterval {
		return
	}
	r.lastCheck = now

	starvationThreshold := cfg.GetIOStarvationThreshold()
	psiThreshold := cfg.GetIOPSIThreshold()
	boostMultiplier := cfg.GetIOBoostMultiplier()
	boostDuration := time.Duration(cfg.GetIOBoostDuration()) * time.Second
	boostMaxPerHour := cfg.GetIOBoostMaxPerHour()
	revertOnNormal := cfg.GetIORevertOnNormal()
	deviceFilter := cfg.GetIODeviceFilter()

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, uid := range limitedUsers {
		state, exists := r.boostStates[uid]
		if !exists {
			state = &IOBoostState{}
			r.boostStates[uid] = state
		}

		// Leggi PSI
		psiStats, err := deps.GetPSIStats(uid)
		if err != nil {
			// PSI non disponibile, skip
			continue
		}

		// Controlla se PSI supera la soglia
		isStarved := psiStats.SomeAvg10 >= psiThreshold

		if isStarved {
			// Inizia o continua il tracking della starvation
			if state.StarvationStart.IsZero() {
				state.StarvationStart = now
			}

			starvationDuration := now.Sub(state.StarvationStart)

			// Se la starvation dura abbastanza e non siamo gia' in boost
			if starvationDuration >= time.Duration(starvationThreshold)*time.Second && !state.IsActive {
				// Controlla se abbiamo superato il max boost per ora
				if state.BoostCount >= boostMaxPerHour {
					r.logger.Warn("IO starvation detected but max boosts per hour reached, skipping",
						"uid", uid,
						"boosts_this_hour", state.BoostCount,
						"psi_avg10", psiStats.SomeAvg10,
					)
					continue
				}

				// Applica boost temporaneo
				r.applyBoost(deps, uid, state, boostMultiplier, boostDuration, deviceFilter, now)
			} else if state.IsActive {
				// Siamo gia' in boost, controlla se e' scaduto
				if now.Sub(state.StartTime) >= boostDuration {
					r.revertBoost(deps, uid, state, deviceFilter)
				}
			}
		} else {
			// PSI sotto soglia, reset starvation timer
			state.StarvationStart = time.Time{}

			// Se siamo in boost e revertOnNormal e' true, revert subito
			if state.IsActive && revertOnNormal {
				r.revertBoost(deps, uid, state, deviceFilter)
			}
		}
	}
}

// applyBoost applica un boost temporaneo dei limiti IO per un utente.
func (r *IORemediation) applyBoost(deps IORemediationDeps, uid int, state *IOBoostState, multiplier float64, duration time.Duration, deviceFilter string, now time.Time) {
	// Salva limiti originali (per ora usiamo RemoveIOLimit come fallback)
	// In una implementazione completa, leggeremmo io.max corrente

	// Applica limiti boostati (usando RemoveIOLimit come "boost" temporaneo)
	// In pratica, rimuoviamo i limiti per permettere all'utente di recuperare
	if err := deps.RemoveIOLimit(uid); err != nil {
		r.logger.Warn("Failed to apply IO boost for user",
			"uid", uid,
			"error", err,
		)
		return
	}

	state.IsActive = true
	state.StartTime = now
	state.BoostCount++
	state.LastBoostTime = now

	r.logger.Info("IO starvation remediation: applied temporary boost",
		"uid", uid,
		"multiplier", multiplier,
		"duration", duration,
		"boosts_this_hour", state.BoostCount,
	)
}

// revertBoost ripristina i limiti IO originali dopo un boost.
func (r *IORemediation) revertBoost(deps IORemediationDeps, uid int, state *IOBoostState, deviceFilter string) {
	// In una implementazione completa, ripristineremmo i limiti originali salvati
	// Rimuove il boost; i limiti correnti verranno riconciliati dal control cycle.

	state.IsActive = false
	state.StarvationStart = time.Time{}

	r.logger.Info("IO starvation remediation: reverted boost",
		"uid", uid,
	)
}

// Cleanup rimuove stati di boost scaduti o non piu' attivi.
func (r *IORemediation) Cleanup(maxAge time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for uid, state := range r.boostStates {
		// Rimuovi stati vecchi
		if !state.IsActive && now.Sub(state.LastBoostTime) > maxAge {
			delete(r.boostStates, uid)
		}
		// Reset boost count se e' passata un'ora dall'ultimo boost
		if now.Sub(state.LastBoostTime) > time.Hour {
			state.BoostCount = 0
		}
	}
}
