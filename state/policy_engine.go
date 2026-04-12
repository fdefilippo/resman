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
// state/policy_engine.go
package state

import (
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

// UserPolicy contiene le policy applicate a un utente.
type UserPolicy struct {
	CPUQuota         int    // CPU quota in microseconds
	RAMQuota         string // RAM quota string (e.g., "1G")
	AppliedAt        time.Time
	LastChanged      time.Time
	PreviousCPUQuota int
	PreviousRAMQuota string
}

// PolicyEngine applica policy basate sui pattern rilevati.
type PolicyEngine struct {
	mu           sync.RWMutex
	logger       *logging.Logger
	userPolicies map[int]*UserPolicy // uid -> policy corrente
}

// NewPolicyEngine crea un nuovo PolicyEngine.
func NewPolicyEngine(logger *logging.Logger) *PolicyEngine {
	return &PolicyEngine{
		logger:       logger,
		userPolicies: make(map[int]*UserPolicy),
	}
}

// ApplyPolicy applica una policy a un utente basata sul pattern rilevato.
// Restituisce true se la policy e' stata effettivamente cambiata.
func (pe *PolicyEngine) ApplyPolicy(uid int, pattern WorkloadPattern, cfg *config.Config) bool {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	// Determina la policy target basata sul pattern
	targetCPUQuota, targetRAMQuota := pe.getQuotasForPattern(pattern, cfg)

	// Se non c'e' pattern riconosciuto, non applicare nulla
	if pattern == PatternUnknown {
		return false
	}

	existing, exists := pe.userPolicies[uid]
	if exists && existing.CPUQuota == targetCPUQuota && existing.RAMQuota == targetRAMQuota {
		// Policy gia' applicata, nessun cambiamento
		return false
	}

	// Applica nuova policy
	now := time.Now()
	if exists {
		existing.PreviousCPUQuota = existing.CPUQuota
		existing.PreviousRAMQuota = existing.RAMQuota
		existing.CPUQuota = targetCPUQuota
		existing.RAMQuota = targetRAMQuota
		existing.LastChanged = now
	} else {
		pe.userPolicies[uid] = &UserPolicy{
			CPUQuota:    targetCPUQuota,
			RAMQuota:    targetRAMQuota,
			AppliedAt:   now,
			LastChanged: now,
		}
	}

	pe.logger.Info("Workload pattern policy applied",
		"uid", uid,
		"pattern", pattern,
		"cpu_quota", targetCPUQuota,
		"ram_quota", targetRAMQuota,
	)

	return true
}

// GetPolicy restituisce la policy corrente per un utente.
func (pe *PolicyEngine) GetPolicy(uid int) (*UserPolicy, bool) {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	policy, exists := pe.userPolicies[uid]
	return policy, exists
}

// getQuotasForPattern restituisce le quote CPU/RAM per un pattern.
func (pe *PolicyEngine) getQuotasForPattern(pattern WorkloadPattern, cfg *config.Config) (int, string) {
	switch pattern {
	case PatternBatchNight:
		return cfg.GetBatchNightCPUQuota(), cfg.GetBatchNightRAMQuota()
	case PatternInteractiveDay:
		return cfg.GetInteractiveCPUQuota(), cfg.GetInteractiveRAMQuota()
	case PatternMixed:
		// Per pattern misti, usa valori intermedi
		batchCPU := cfg.GetBatchNightCPUQuota()
		interactiveCPU := cfg.GetInteractiveCPUQuota()
		return (batchCPU + interactiveCPU) / 2, cfg.GetInteractiveRAMQuota()
	case PatternAlwaysOn:
		// Utenti sempre attivi: quota moderata
		return cfg.GetInteractiveCPUQuota(), cfg.GetInteractiveRAMQuota()
	case PatternSporadic:
		// Utenti sporadici: quota bassa di default
		return cfg.GetInteractiveCPUQuota() / 2, cfg.GetInteractiveRAMQuota()
	default:
		return 0, ""
	}
}

// Cleanup rimuove policy non piu' attive.
func (pe *PolicyEngine) Cleanup(maxAge time.Duration) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	now := time.Now()
	for uid, policy := range pe.userPolicies {
		if now.Sub(policy.LastChanged) > maxAge {
			delete(pe.userPolicies, uid)
		}
	}
}
