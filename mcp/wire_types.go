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
	MinSystemCores       int     `json:"min_system_cores"`
	CPUQuotaNormal       string  `json:"cpu_quota_normal"`
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
		MinSystemCores:       cfg.MinSystemCores,
		CPUQuotaNormal:       cfg.CPUQuotaNormal,
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
	Timestamp         string  `json:"timestamp"`
	UID               int     `json:"uid"`
	Username          string  `json:"username"`
	CPUUsage          float64 `json:"cpu_usage"`
	MemoryUsage       int64   `json:"memory_usage"`
	ProcessCount      int     `json:"process_count"`
	CgroupPath        string  `json:"cgroup_path"`
	CPUQuota          string  `json:"cpu_quota"`
	EligibleForCPU    bool    `json:"eligible_for_cpu"`
	EligibleForRAM    bool    `json:"eligible_for_ram"`
	EligibleForIO     bool    `json:"eligible_for_io"`
	CPULimitRequested bool    `json:"cpu_limit_requested"`
	CPULimitActive    bool    `json:"cpu_limit_active"`
	RAMLimitRequested bool    `json:"ram_limit_requested"`
	RAMLimitActive    bool    `json:"ram_limit_active"`
	IOLimitRequested  bool    `json:"io_limit_requested"`
	IOLimitActive     bool    `json:"io_limit_active"`
}

type getUserHistoryResult struct {
	Records   []userHistoryRecord `json:"records"`
	Count     int                 `json:"count"`
	StartTime string              `json:"start_time"`
	EndTime   string              `json:"end_time"`
}

func newUserHistoryRecord(record database.UserMetricsRecord) userHistoryRecord {
	return userHistoryRecord{
		Timestamp:         record.Timestamp.Format(time.RFC3339),
		UID:               record.UID,
		Username:          record.Username,
		CPUUsage:          record.CPUUsagePercent,
		MemoryUsage:       record.MemoryUsageBytes,
		ProcessCount:      record.ProcessCount,
		CgroupPath:        record.CgroupPath,
		CPUQuota:          record.CPUQuota,
		EligibleForCPU:    record.EligibleForCPU,
		EligibleForRAM:    record.EligibleForRAM,
		EligibleForIO:     record.EligibleForIO,
		CPULimitRequested: record.CPULimitRequested,
		CPULimitActive:    record.CPULimitActive,
		RAMLimitRequested: record.RAMLimitRequested,
		RAMLimitActive:    record.RAMLimitActive,
		IOLimitRequested:  record.IOLimitRequested,
		IOLimitActive:     record.IOLimitActive,
	}
}

type systemHistoryRecord struct {
	Timestamp                    string  `json:"timestamp"`
	TotalCPUUsage                float64 `json:"total_cpu_usage"`
	TotalCores                   int     `json:"total_cores"`
	SystemLoad                   float64 `json:"system_load"`
	CPULimitsActive              bool    `json:"cpu_limits_active"`
	ResourceLimitsActive         bool    `json:"resource_limits_active"`
	AnyLimitsActive              bool    `json:"any_limits_active"`
	CPUActivelyLimitedUsersCount int     `json:"cpu_actively_limited_users_count"`
	ActivelyLimitedUsersCount    int     `json:"actively_limited_users_count"`
}

type getSystemHistoryResult struct {
	Records   []systemHistoryRecord `json:"records"`
	Count     int                   `json:"count"`
	StartTime string                `json:"start_time"`
	EndTime   string                `json:"end_time"`
}

func newSystemHistoryRecord(record database.SystemMetricsRecord) systemHistoryRecord {
	return systemHistoryRecord{
		Timestamp:                    record.Timestamp.Format(time.RFC3339),
		TotalCPUUsage:                record.TotalCPUUsagePercent,
		TotalCores:                   record.TotalCores,
		SystemLoad:                   record.SystemLoad,
		CPULimitsActive:              record.CPULimitsActive,
		ResourceLimitsActive:         record.ResourceLimitsActive,
		AnyLimitsActive:              record.AnyLimitsActive,
		CPUActivelyLimitedUsersCount: record.CPUActivelyLimitedUsersCount,
		ActivelyLimitedUsersCount:    record.ActivelyLimitedUsersCount,
	}
}
