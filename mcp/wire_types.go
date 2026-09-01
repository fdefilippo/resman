/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */
package mcp

import (
	"strconv"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
)

type activeUserPayload struct {
	UID      int    `json:"uid"`
	Username string `json:"username"`
}

type activeUsersPayload struct {
	Hostname   string              `json:"hostname"`
	ServerRole string              `json:"server_role"`
	Users      []activeUserPayload `json:"users"`
}

func newActiveUsersPayload(hostname, serverRole string, activeUIDs []int, samples map[int]*resmanmetrics.UserMetrics) activeUsersPayload {
	result := activeUsersPayload{
		Hostname:   hostname,
		ServerRole: serverRole,
		Users:      make([]activeUserPayload, 0, len(activeUIDs)),
	}
	for _, uid := range activeUIDs {
		username := ""
		if sample, ok := samples[uid]; ok {
			username = sample.Username
		}
		if username == "" {
			username = strconv.Itoa(uid)
		}
		result.Users = append(result.Users, activeUserPayload{UID: uid, Username: username})
	}
	return result
}

type resourcePolicyConfigurationPayload struct {
	Hostname             string  `json:"hostname"`
	ServerRole           string  `json:"server_role"`
	CPUThreshold         int     `json:"cpu_threshold"`
	CPUReleaseThreshold  int     `json:"cpu_release_threshold"`
	CPUThresholdDuration int     `json:"cpu_threshold_duration"`
	PollingInterval      int     `json:"polling_interval"`
	CPUReservePoints     int     `json:"cpu_reserve_points"`
	CPUBestEffortPoints  int     `json:"cpu_best_effort_points"`
	CPUPointsFile        string  `json:"cpu_points_file"`
	EnablePrometheus     bool    `json:"enable_prometheus"`
	PrometheusPort       int     `json:"prometheus_port"`
	IgnoreSystemLoad     bool    `json:"ignore_system_load"`
	SystemUIDMin         int     `json:"system_uid_min"`
	SystemUIDMax         int     `json:"system_uid_max"`
	RAMEnabled           bool    `json:"ram_enabled"`
	RAMThreshold         int     `json:"ram_threshold"`
	RAMReleaseThreshold  int     `json:"ram_release_threshold"`
	RAMQuotaPerUser      string  `json:"ram_quota_per_user"`
	DisableSwap          bool    `json:"disable_swap"`
	RAMHighRatio         float64 `json:"ram_high_ratio"`
	IOEnabled            bool    `json:"io_enabled"`
	IOThreshold          int     `json:"io_threshold"`
	IOReleaseThreshold   int     `json:"io_release_threshold"`
	IOThresholdDuration  int     `json:"io_threshold_duration"`
	IOReadBPS            string  `json:"io_read_bps"`
	IOWriteBPS           string  `json:"io_write_bps"`
	IOReadIOPS           int     `json:"io_read_iops"`
	IOWriteIOPS          int     `json:"io_write_iops"`
	IODeviceFilter       string  `json:"io_device_filter"`
}

func newResourcePolicyConfigurationPayload(hostname string, cfg *config.Config) resourcePolicyConfigurationPayload {
	return resourcePolicyConfigurationPayload{
		Hostname:             hostname,
		ServerRole:           cfg.ServerRole,
		CPUThreshold:         cfg.CPUThreshold,
		CPUReleaseThreshold:  cfg.CPUReleaseThreshold,
		CPUThresholdDuration: cfg.CPUThresholdDuration,
		PollingInterval:      cfg.PollingInterval,
		CPUReservePoints:     cfg.CPUReservePoints,
		CPUBestEffortPoints:  cfg.CPUBestEffortPoints,
		CPUPointsFile:        cfg.CPUPointsFile,
		EnablePrometheus:     cfg.EnablePrometheus,
		PrometheusPort:       cfg.PrometheusMetricsBindPort,
		IgnoreSystemLoad:     cfg.IgnoreSystemLoad,
		SystemUIDMin:         cfg.SystemUIDMin,
		SystemUIDMax:         cfg.SystemUIDMax,
		RAMEnabled:           cfg.RAMEnabled,
		RAMThreshold:         cfg.RAMThreshold,
		RAMReleaseThreshold:  cfg.RAMReleaseThreshold,
		RAMQuotaPerUser:      cfg.RAMQuotaPerUser,
		DisableSwap:          cfg.DisableSwap,
		RAMHighRatio:         cfg.RAMHighRatio,
		IOEnabled:            cfg.IOEnabled,
		IOThreshold:          cfg.IOThreshold,
		IOReleaseThreshold:   cfg.IOReleaseThreshold,
		IOThresholdDuration:  cfg.IOThresholdDuration,
		IOReadBPS:            cfg.IOReadBPS,
		IOWriteBPS:           cfg.IOWriteBPS,
		IOReadIOPS:           cfg.IOReadIOPS,
		IOWriteIOPS:          cfg.IOWriteIOPS,
		IODeviceFilter:       cfg.IODeviceFilter,
	}
}

type cpuReportPayload struct {
	Hostname                     string  `json:"hostname"`
	ServerRole                   string  `json:"server_role"`
	Report                       string  `json:"report"`
	TotalCPU                     float64 `json:"total_cpu"`
	AverageCPU                   float64 `json:"avg_cpu"`
	PeakCPU                      float64 `json:"peak_cpu"`
	ObservedUsersCount           int     `json:"observed_users_count"`
	CPUActivelyLimitedUsersCount int     `json:"cpu_actively_limited_users_count"`
	CPULimitsActive              bool    `json:"cpu_limits_active"`
}

type memoryReportPayload struct {
	Hostname                     string  `json:"hostname"`
	ServerRole                   string  `json:"server_role"`
	Report                       string  `json:"report"`
	TotalMemoryMB                float64 `json:"total_memory_mb"`
	AverageMemoryMB              float64 `json:"avg_memory_mb"`
	PeakMemoryMB                 float64 `json:"peak_memory_mb"`
	ObservedUsersCount           int     `json:"observed_users_count"`
	RAMActivelyLimitedUsersCount int     `json:"ram_actively_limited_users_count"`
	ResourceLimitsActive         bool    `json:"resource_limits_active"`
}

type limitActionResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type userFiltersPayload struct {
	UserIncludeList []string `json:"user_include_list"`
	UserExcludeList []string `json:"user_exclude_list"`
	ConfigFile      string   `json:"config_file"`
}

type validateUserFilterResult struct {
	Valid       bool      `json:"valid"`
	Pattern     string    `json:"pattern,omitempty"`
	Type        string    `json:"type,omitempty"`
	TestMatches *[]string `json:"test_matches,omitempty"`
	MatchCount  *int      `json:"match_count,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type userHistoryRecord struct {
	Timestamp                         string  `json:"timestamp"`
	SampleEpochID                     int64   `json:"sample_epoch_id"`
	IntervalStart                     *string `json:"interval_start"`
	IntervalEnd                       string  `json:"interval_end"`
	UID                               int     `json:"uid"`
	Username                          string  `json:"username"`
	CPUUsage                          float64 `json:"cpu_usage"`
	MemoryUsage                       int64   `json:"memory_usage"`
	ProcessCount                      int     `json:"process_count"`
	EnforceableProcessCount           int     `json:"enforceable_process_count"`
	CgroupPath                        string  `json:"cgroup_path"`
	CPUQuota                          string  `json:"cpu_quota"`
	ConfiguredGuaranteePoints         *uint64 `json:"configured_guarantee_points"`
	ConfiguredCPUClass                string  `json:"configured_cpu_class"`
	CPUPointsLifecycleState           string  `json:"cpu_points_lifecycle_state"`
	AppliedCPUClass                   *string `json:"applied_cpu_class"`
	AppliedCPUWeight                  *uint64 `json:"applied_cpu_weight"`
	CPUWeight                         *uint64 `json:"cpu_weight"`
	LeafCPUUsageUsecDelta             *uint64 `json:"leaf_cpu_usage_usec_delta"`
	PIDNamespaceMismatchCount         int     `json:"pid_namespace_mismatch_count"`
	PIDNamespaceUnavailableCount      int     `json:"pid_namespace_unavailable_count"`
	RAMCgroupUsageBytes               *uint64 `json:"ram_cgroup_usage_bytes"`
	RAMCoverage                       *string `json:"ram_coverage"`
	RAMCoverageIncompleteProcessCount int     `json:"ram_coverage_incomplete_process_count"`
	RAMSwapDisabled                   *bool   `json:"ram_swap_disabled"`
	MemoryHighLimit                   *string `json:"memory_high_limit"`
	MemoryMaxLimit                    *string `json:"memory_max_limit"`
	MemorySwapMax                     *string `json:"memory_swap_max"`
	MemoryHighEventsDelta             *uint64 `json:"memory_high_events_delta"`
	MemoryMaxEventsDelta              *uint64 `json:"memory_max_events_delta"`
	MemoryOOMEventsDelta              *uint64 `json:"memory_oom_events_delta"`
	MemoryOOMKillEventsDelta          *uint64 `json:"memory_oom_kill_events_delta"`
	EligibleForCPU                    bool    `json:"eligible_for_cpu"`
	EligibleForRAM                    bool    `json:"eligible_for_ram"`
	EligibleForIO                     bool    `json:"eligible_for_io"`
	CPULimitRequested                 bool    `json:"cpu_limit_requested"`
	CPULimitActive                    bool    `json:"cpu_limit_active"`
	RAMLimitRequested                 bool    `json:"ram_limit_requested"`
	RAMLimitActive                    bool    `json:"ram_limit_active"`
	IOLimitRequested                  bool    `json:"io_limit_requested"`
	IOLimitActive                     bool    `json:"io_limit_active"`
}

type getUserHistoryResult struct {
	Records   []userHistoryRecord `json:"records"`
	Count     int                 `json:"count"`
	StartTime string              `json:"start_time"`
	EndTime   string              `json:"end_time"`
}

func newUserHistoryRecord(record database.UserMetricsRecord) userHistoryRecord {
	return userHistoryRecord{
		Timestamp:                         record.Timestamp.Format(time.RFC3339),
		SampleEpochID:                     record.SampleEpochID,
		IntervalStart:                     formatOptionalHistoryTime(record.IntervalStart),
		IntervalEnd:                       record.IntervalEnd.Format(time.RFC3339),
		UID:                               record.UID,
		Username:                          record.Username,
		CPUUsage:                          record.CPUUsagePercent,
		MemoryUsage:                       record.MemoryUsageBytes,
		ProcessCount:                      record.ProcessCount,
		EnforceableProcessCount:           record.EnforceableProcessCount,
		CgroupPath:                        record.CgroupPath,
		CPUQuota:                          record.CPUQuota,
		ConfiguredGuaranteePoints:         record.ConfiguredGuaranteePoints,
		ConfiguredCPUClass:                record.ConfiguredCPUClass,
		CPUPointsLifecycleState:           record.CPUPointsLifecycleState,
		AppliedCPUClass:                   record.AppliedCPUClass,
		AppliedCPUWeight:                  record.AppliedCPUWeight,
		CPUWeight:                         record.CPUWeight,
		LeafCPUUsageUsecDelta:             record.LeafCPUUsageUsecDelta,
		PIDNamespaceMismatchCount:         record.PIDNamespaceMismatchCount,
		PIDNamespaceUnavailableCount:      record.PIDNamespaceUnavailableCount,
		RAMCgroupUsageBytes:               record.RAMCgroupUsageBytes,
		RAMCoverage:                       record.RAMCoverage,
		RAMCoverageIncompleteProcessCount: record.RAMCoverageIncompleteProcessCount,
		RAMSwapDisabled:                   record.RAMSwapDisabled,
		MemoryHighLimit:                   record.MemoryHighLimit,
		MemoryMaxLimit:                    record.MemoryMaxLimit,
		MemorySwapMax:                     record.MemorySwapMax,
		MemoryHighEventsDelta:             record.MemoryHighEventsDelta,
		MemoryMaxEventsDelta:              record.MemoryMaxEventsDelta,
		MemoryOOMEventsDelta:              record.MemoryOOMEventsDelta,
		MemoryOOMKillEventsDelta:          record.MemoryOOMKillEventsDelta,
		EligibleForCPU:                    record.EligibleForCPU,
		EligibleForRAM:                    record.EligibleForRAM,
		EligibleForIO:                     record.EligibleForIO,
		CPULimitRequested:                 record.CPULimitRequested,
		CPULimitActive:                    record.CPULimitActive,
		RAMLimitRequested:                 record.RAMLimitRequested,
		RAMLimitActive:                    record.RAMLimitActive,
		IOLimitRequested:                  record.IOLimitRequested,
		IOLimitActive:                     record.IOLimitActive,
	}
}

type systemHistoryRecord struct {
	Timestamp                         string  `json:"timestamp"`
	SampleEpochID                     int64   `json:"sample_epoch_id"`
	IntervalStart                     *string `json:"interval_start"`
	IntervalEnd                       string  `json:"interval_end"`
	TotalCPUUsage                     float64 `json:"total_cpu_usage"`
	TotalCores                        int     `json:"total_cores"`
	SystemLoad                        float64 `json:"system_load"`
	CPULimitsActive                   bool    `json:"cpu_limits_active"`
	ResourceLimitsActive              bool    `json:"resource_limits_active"`
	AnyLimitsActive                   bool    `json:"any_limits_active"`
	CPUActivelyLimitedUsersCount      int     `json:"cpu_actively_limited_users_count"`
	ActivelyLimitedUsersCount         int     `json:"actively_limited_users_count"`
	NominalParentPoolPoints           uint64  `json:"nominal_parent_pool_points"`
	CPUCapacityAvailable              bool    `json:"cpu_capacity_available"`
	OnlineCPUs                        *uint64 `json:"online_cpus"`
	ProgrammedParentQuotaUsec         *uint64 `json:"programmed_parent_quota_usec"`
	ProgrammedParentPeriodUsec        *uint64 `json:"programmed_parent_period_usec"`
	CPUPointsDegraded                 bool    `json:"cpu_points_degraded"`
	AppliedGuaranteePoints            uint64  `json:"applied_guarantee_points"`
	ProgrammedGuaranteeWeight         uint64  `json:"programmed_guarantee_weight"`
	ConfiguredBestEffortWeight        uint64  `json:"configured_best_effort_weight"`
	ParentCPUQuota                    *string `json:"parent_cpu_quota"`
	GuaranteedDomainCPUWeight         *uint64 `json:"guaranteed_domain_cpu_weight"`
	BestEffortDomainCPUWeight         *uint64 `json:"best_effort_domain_cpu_weight"`
	ParentCPUUsageUsecDelta           *uint64 `json:"parent_cpu_usage_usec_delta"`
	GuaranteedDomainCPUUsageUsecDelta *uint64 `json:"guaranteed_domain_cpu_usage_usec_delta"`
	BestEffortDomainCPUUsageUsecDelta *uint64 `json:"best_effort_domain_cpu_usage_usec_delta"`
	ParentCPUPeriodsDelta             *uint64 `json:"parent_cpu_periods_delta"`
	ParentCPUThrottledPeriodsDelta    *uint64 `json:"parent_cpu_throttled_periods_delta"`
	ParentCPUThrottledUsecDelta       *uint64 `json:"parent_cpu_throttled_usec_delta"`
}

type getSystemHistoryResult struct {
	Records   []systemHistoryRecord `json:"records"`
	Count     int                   `json:"count"`
	StartTime string                `json:"start_time"`
	EndTime   string                `json:"end_time"`
}

func newSystemHistoryRecord(record database.SystemMetricsRecord) systemHistoryRecord {
	return systemHistoryRecord{
		Timestamp:                         record.Timestamp.Format(time.RFC3339),
		SampleEpochID:                     record.SampleEpochID,
		IntervalStart:                     formatOptionalHistoryTime(record.IntervalStart),
		IntervalEnd:                       record.IntervalEnd.Format(time.RFC3339),
		TotalCPUUsage:                     record.TotalCPUUsagePercent,
		TotalCores:                        record.TotalCores,
		SystemLoad:                        record.SystemLoad,
		CPULimitsActive:                   record.CPULimitsActive,
		ResourceLimitsActive:              record.ResourceLimitsActive,
		AnyLimitsActive:                   record.AnyLimitsActive,
		CPUActivelyLimitedUsersCount:      record.CPUActivelyLimitedUsersCount,
		ActivelyLimitedUsersCount:         record.ActivelyLimitedUsersCount,
		NominalParentPoolPoints:           record.NominalParentPoolPoints,
		CPUCapacityAvailable:              record.CPUCapacityAvailable,
		OnlineCPUs:                        record.OnlineCPUs,
		ProgrammedParentQuotaUsec:         record.ProgrammedParentQuotaUsec,
		ProgrammedParentPeriodUsec:        record.ProgrammedParentPeriodUsec,
		CPUPointsDegraded:                 record.CPUPointsDegraded,
		AppliedGuaranteePoints:            record.AppliedGuaranteePoints,
		ProgrammedGuaranteeWeight:         record.ProgrammedGuaranteeWeight,
		ConfiguredBestEffortWeight:        record.ConfiguredBestEffortWeight,
		ParentCPUQuota:                    record.ParentCPUQuota,
		GuaranteedDomainCPUWeight:         record.GuaranteedDomainCPUWeight,
		BestEffortDomainCPUWeight:         record.BestEffortDomainCPUWeight,
		ParentCPUUsageUsecDelta:           record.ParentCPUUsageUsecDelta,
		GuaranteedDomainCPUUsageUsecDelta: record.GuaranteedDomainCPUUsageUsecDelta,
		BestEffortDomainCPUUsageUsecDelta: record.BestEffortDomainCPUUsageUsecDelta,
		ParentCPUPeriodsDelta:             record.ParentCPUPeriodsDelta,
		ParentCPUThrottledPeriodsDelta:    record.ParentCPUThrottledPeriodsDelta,
		ParentCPUThrottledUsecDelta:       record.ParentCPUThrottledUsecDelta,
	}
}

func formatOptionalHistoryTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.Format(time.RFC3339)
	return &formatted
}
