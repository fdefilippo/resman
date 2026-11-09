// Package metricrollup defines availability-aware hourly observation aggregates.
package metricrollup

import "time"

// Scope identifies the fixed raw observation family.
type Scope string

const (
	User   Scope = "user"
	System Scope = "system"
)

// Metric identifies a documented raw observation; names include their unit.
type Metric string

const (
	CPUUsagePercent              Metric = "cpu_usage_percent"
	MemoryUsageBytes             Metric = "memory_usage_bytes"
	ProcessCount                 Metric = "process_count"
	RAMCgroupUsageBytes          Metric = "ram_cgroup_usage_bytes"
	UnitCPUUsageUsecDelta        Metric = "unit_cpu_usage_usec_delta"
	MemoryHighEventsDelta        Metric = "memory_high_events_delta"
	MemoryOOMKillEventsDelta     Metric = "memory_oom_kill_events_delta"
	CPULimitActive               Metric = "cpu_limit_active"
	RAMLimitActive               Metric = "ram_limit_active"
	IOLimitActive                Metric = "io_limit_active"
	ParentCPUUsageUsecDelta      Metric = "parent_cpu_usage_usec_delta"
	ParentCPUThrottledUsecDelta  Metric = "parent_cpu_throttled_usec_delta"
	CPUActivelyLimitedUsersCount Metric = "cpu_actively_limited_users_count"
	ResourceLimitsActive         Metric = "resource_limits_active"
	AnyLimitsActive              Metric = "any_limits_active"
)

// Entry preserves sufficient statistics; averages are sample-weighted, never time coverage.
type Entry struct {
	HourStart        string   `json:"hour_start"`
	Scope            Scope    `json:"scope"`
	UID              int      `json:"uid"`
	Username         string   `json:"username"`
	EnforcementMode  string   `json:"enforcement_mode"`
	DenominatorState string   `json:"denominator_state"`
	TotalCores       int      `json:"total_cores"`
	Metric           Metric   `json:"metric"`
	Samples          int64    `json:"samples"`
	KnownSamples     int64    `json:"known_samples"`
	UnknownSamples   int64    `json:"unknown_samples"`
	Sum              *float64 `json:"sum"`
	SampleAverage    *float64 `json:"sample_average"`
	Minimum          *float64 `json:"minimum"`
	Maximum          *float64 `json:"maximum"`
	FirstSampleAt    string   `json:"first_sample_at"`
	LastSampleAt     string   `json:"last_sample_at"`
}

// Query selects bounded hourly buckets overlapping an explicit time range.
type Query struct {
	Scope      Scope
	UID        *int
	Start, End time.Time
	Limit      int
}

const MaxEntries = 1000

func Limit(n int) int {
	if n <= 0 {
		return 100
	}
	return min(n, MaxEntries)
}
