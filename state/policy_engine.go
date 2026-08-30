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

// UserPolicy contains the policies applied to a user.
type UserPolicy struct {
	CPUQuota         int    // CPU quota in microseconds
	RAMQuota         string // RAM quota string (e.g., "1G")
	AppliedAt        time.Time
	LastChanged      time.Time
	PreviousCPUQuota int
	PreviousRAMQuota string
}

// PolicyEngine applies policies based on detected patterns.
type PolicyEngine struct {
	mu           sync.RWMutex
	logger       *logging.Logger
	userPolicies map[int]*UserPolicy // uid -> current policy
}

// NewPolicyEngine creates a PolicyEngine.
func NewPolicyEngine(logger *logging.Logger) *PolicyEngine {
	return &PolicyEngine{
		logger:       logger,
		userPolicies: make(map[int]*UserPolicy),
	}
}

// ApplyPolicy updates one user's policy from a detected workload pattern.
// It returns true only when the stored policy changes.
func (pe *PolicyEngine) ApplyPolicy(uid int, pattern WorkloadPattern, cfg *config.Config) bool {
	// Resolve the external configuration before locking policy state.
	targetCPUQuota, targetRAMQuota := pe.getQuotasForPattern(pattern, cfg)

	// An unknown pattern does not select a policy.
	if pattern == PatternUnknown {
		return false
	}

	pe.mu.Lock()

	existing, exists := pe.userPolicies[uid]
	if exists && existing.CPUQuota == targetCPUQuota && existing.RAMQuota == targetRAMQuota {
		pe.mu.Unlock()
		return false
	}

	// Store the newly selected policy before enforcement reconciliation.
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
	pe.mu.Unlock()

	pe.logger.Info("Workload pattern policy selected",
		"uid", uid,
		"pattern", pattern,
		"cpu_quota", targetCPUQuota,
		"ram_quota", targetRAMQuota,
	)

	return true
}

// GetPolicy returns the current policy for a user.
func (pe *PolicyEngine) GetPolicy(uid int) (*UserPolicy, bool) {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	policy, exists := pe.userPolicies[uid]
	if !exists {
		return nil, false
	}
	copy := *policy
	return &copy, true
}

// RemovePolicy removes the policy associated with a user.
func (pe *PolicyEngine) RemovePolicy(uid int) bool {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	if _, exists := pe.userPolicies[uid]; !exists {
		return false
	}
	delete(pe.userPolicies, uid)
	return true
}

// RetainUsers removes policies for users that are no longer eligible.
func (pe *PolicyEngine) RetainUsers(eligible map[int]bool) []int {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	var removed []int
	for uid := range pe.userPolicies {
		if !eligible[uid] {
			delete(pe.userPolicies, uid)
			removed = append(removed, uid)
		}
	}
	return removed
}

// Clear removes all policies and returns the affected users.
func (pe *PolicyEngine) Clear() []int {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	removed := make([]int, 0, len(pe.userPolicies))
	for uid := range pe.userPolicies {
		removed = append(removed, uid)
	}
	pe.userPolicies = make(map[int]*UserPolicy)
	return removed
}

// getQuotasForPattern returns the CPU and RAM quotas for a pattern.
func (pe *PolicyEngine) getQuotasForPattern(pattern WorkloadPattern, cfg *config.Config) (int, string) {
	switch pattern {
	case PatternBatchNight:
		return cfg.GetBatchNightCPUQuota(), cfg.GetBatchNightRAMQuota()
	case PatternInteractiveDay:
		return cfg.GetInteractiveCPUQuota(), cfg.GetInteractiveRAMQuota()
	case PatternMixed:
		// Use intermediate values for mixed patterns.
		batchCPU := cfg.GetBatchNightCPUQuota()
		interactiveCPU := cfg.GetInteractiveCPUQuota()
		return (batchCPU + interactiveCPU) / 2, cfg.GetInteractiveRAMQuota()
	case PatternAlwaysOn:
		// Always-active users receive a moderate quota.
		return cfg.GetInteractiveCPUQuota(), cfg.GetInteractiveRAMQuota()
	case PatternSporadic:
		// Sporadic users receive a low quota by default.
		return cfg.GetInteractiveCPUQuota() / 2, cfg.GetInteractiveRAMQuota()
	default:
		return 0, ""
	}
}
