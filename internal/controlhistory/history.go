// Package controlhistory defines the shared terminal control-cycle history contract.
package controlhistory

import (
	"sort"
	"time"
)

// Outcome describes a completed attempt, independently of enforcement success.
type Outcome string

const (
	Succeeded Outcome = "success"
	Degraded  Outcome = "degraded"
	Failed    Outcome = "failed"
	Cancelled Outcome = "cancelled"
	Suspended Outcome = "suspended"
)

// PersistenceStatus reports access to the optional durable observation sink.
type PersistenceStatus string

const (
	PersistenceDisabled    PersistenceStatus = "disabled"
	PersistenceAvailable   PersistenceStatus = "available"
	PersistenceUnavailable PersistenceStatus = "unavailable"
)

// Details adds explicit availability and execution evidence to the legacy fields.
// Missing samples never represent measured zeros. FailedStage is a bounded stage
// name; raw errors, which can contain credentials, are deliberately not persisted.
type Details struct {
	SessionID                 string    `json:"session_id"`
	CycleID                   int64     `json:"cycle_id"`
	StartedAt                 time.Time `json:"started_at"`
	Trigger                   string    `json:"trigger"`
	Outcome                   Outcome   `json:"outcome"`
	FailedStage               string    `json:"failed_stage,omitempty"`
	FailureKind               string    `json:"failure_kind,omitempty"`
	Suspended                 bool      `json:"suspended"`
	HostCPUUsageAvailable     bool      `json:"host_cpu_usage_available"`
	HostCPUUnavailableReason  string    `json:"host_cpu_unavailable_reason,omitempty"`
	SampleAvailable           bool      `json:"sample_available"`
	SampleTimestamp           string    `json:"sample_timestamp,omitempty"`
	EnforcementMode           string    `json:"enforcement_mode"`
	ExecutionAttempted        bool      `json:"execution_attempted"`
	RequestedIntent           string    `json:"requested_intent"`
	AppliedAction             string    `json:"applied_action"`
	BlockReason               string    `json:"block_reason"`
	CPUDecisionAvailable      bool      `json:"cpu_decision_available"`
	CPUActivationRequested    bool      `json:"cpu_activation_requested"`
	CPUActivationApplied      bool      `json:"cpu_activation_applied"`
	BlockIOPSSourceRequired   bool      `json:"block_iops_source_required"`
	BlockIOPSSourceAvailable  bool      `json:"block_iops_source_available"`
	BlockIOPSUnavailableUsers int       `json:"block_iops_unavailable_users"`
	IOUnavailableProcesses    int       `json:"io_unavailable_processes"`
}

// Entry is one terminal attempt, with additive metadata and legacy observations.
type Entry struct {
	Details
	Timestamp                    time.Time `json:"timestamp"`
	Decision                     string    `json:"decision"`
	Reason                       string    `json:"reason"`
	TotalCPUUsage                float64   `json:"total_cpu_usage"`
	CPUEligibleCPUUsage          float64   `json:"cpu_eligible_users_cpu_usage"`
	ObservedUsersCount           int       `json:"observed_users_count"`
	CPUActivelyLimitedUsersCount int       `json:"cpu_actively_limited_users_count"`
	CPULimitsActive              bool      `json:"cpu_limits_active"`
	DurationMs                   int64     `json:"duration_ms"`
}

// MaxQueryEntries bounds every durable-history query and MCP response.
const MaxQueryEntries = 1000

// QueryLimit normalizes an externally supplied query limit.
func QueryLimit(limit int) int {
	if limit <= 0 {
		return 10
	}
	if limit > MaxQueryEntries {
		return MaxQueryEntries
	}
	return limit
}

// Merge combines overlapping sources once per restart-safe identity, preferring
// the in-memory entry, and returns the latest bounded window chronologically.
func Merge(durable, memory []Entry, limit int) []Entry {
	type key struct {
		session string
		cycle   int64
	}
	entries := make(map[key]Entry, len(durable)+len(memory))
	for _, source := range [][]Entry{durable, memory} {
		for _, e := range source {
			entries[key{e.SessionID, e.CycleID}] = e
		}
	}
	result := make([]Entry, 0, len(entries))
	for _, e := range entries {
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].Timestamp.Equal(result[j].Timestamp) {
			return result[i].Timestamp.Before(result[j].Timestamp)
		}
		if result[i].SessionID != result[j].SessionID {
			return result[i].SessionID < result[j].SessionID
		}
		return result[i].CycleID < result[j].CycleID
	})
	limit = QueryLimit(limit)
	if len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result
}
