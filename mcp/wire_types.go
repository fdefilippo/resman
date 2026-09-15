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
	"encoding/json"
	"strconv"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

type ioDeviceWeightPayload struct {
	State                         string                       `json:"state"`
	Reason                        string                       `json:"reason"`
	Selector                      string                       `json:"selector"`
	Mechanism                     string                       `json:"mechanism"`
	ClassificationAttempts        uint64                       `json:"classification_attempts"`
	ProbeAttempts                 uint64                       `json:"probe_attempts"`
	Programmed                    bool                         `json:"programmed"`
	ProgrammedState               string                       `json:"programmed_state"`
	ReadBack                      bool                         `json:"read_back"`
	ReadBackState                 string                       `json:"read_back_state"`
	FunctionallyAccepted          bool                         `json:"functionally_accepted"`
	EffectQualified               bool                         `json:"effect_qualified"`
	EffectQualificationProvenance string                       `json:"effect_qualification_provenance"`
	AuthorityCoverage             string                       `json:"authority_coverage"`
	CompleteUsers                 int                          `json:"complete_users"`
	PartialUsers                  int                          `json:"partial_users"`
	UnavailableUsers              int                          `json:"unavailable_users"`
	SiblingSlices                 int                          `json:"sibling_slices"`
	TotalPoints                   uint64                       `json:"total_points"`
	RequestedAt                   string                       `json:"requested_at,omitempty"`
	NextRetryAt                   string                       `json:"next_retry_at,omitempty"`
	Values                        []ioDeviceWeightValuePayload `json:"values"`
	ObservedDelivery              string                       `json:"observed_delivery"`
}

type ioDeviceWeightValuePayload struct {
	UID            int     `json:"uid"`
	Class          string  `json:"class"`
	Device         string  `json:"device"`
	Mechanism      string  `json:"mechanism"`
	Coverage       string  `json:"coverage"`
	RequestedValue uint64  `json:"requested_value"`
	SystemdValue   uint64  `json:"systemd_value,omitempty"`
	KernelValue    uint64  `json:"kernel_value,omitempty"`
	NominalShare   float64 `json:"nominal_share"`
	Programmed     bool    `json:"programmed"`
	ReadBack       bool    `json:"read_back"`
}

func newIODeviceWeightPayload(status state.IODeviceWeightStatus) ioDeviceWeightPayload {
	result := ioDeviceWeightPayload{
		State: string(status.State), Reason: status.Reason, Selector: status.Selector,
		Mechanism:              string(status.Mechanism),
		ClassificationAttempts: status.ClassificationAttempts, ProbeAttempts: status.ProbeAttempts,
		Programmed: status.Programmed, ReadBack: status.ReadBack,
		ProgrammedState: string(status.ProgrammedState), ReadBackState: string(status.ReadBackState),
		FunctionallyAccepted: status.State == state.IODeviceWeightFunctionallyAccepted,
		EffectQualified:      status.EffectQualified, PartialUsers: status.PartialUsers,
		EffectQualificationProvenance: string(status.EffectQualificationProvenance),
		AuthorityCoverage:             string(status.AuthorityCoverage), CompleteUsers: status.CompleteUsers,
		UnavailableUsers: status.UnavailableUsers, SiblingSlices: status.SiblingSlices, TotalPoints: status.TotalPoints,
		ObservedDelivery: status.ObservedDelivery,
	}
	if !status.RequestedAt.IsZero() {
		result.RequestedAt = status.RequestedAt.Format(time.RFC3339Nano)
	}
	if !status.NextRetryAt.IsZero() {
		result.NextRetryAt = status.NextRetryAt.Format(time.RFC3339Nano)
	}
	result.Values = make([]ioDeviceWeightValuePayload, 0, len(status.Values))
	for _, value := range status.Values {
		result.Values = append(result.Values, ioDeviceWeightValuePayload{
			UID: value.UID, Class: value.Class, Device: value.Device, Mechanism: value.Mechanism, Coverage: string(value.Coverage),
			RequestedValue: value.RequestedValue, SystemdValue: value.SystemdValue, KernelValue: value.KernelValue, NominalShare: value.NominalShare,
			Programmed: value.Programmed, ReadBack: value.ReadBack,
		})
	}
	return result
}

type cpuPointsSystemPayload struct {
	EnforcementMode                string  `json:"enforcement_mode"`
	SampleEpochID                  int64   `json:"sample_epoch_id,omitempty"`
	IntervalStart                  *string `json:"interval_start,omitempty"`
	IntervalEnd                    string  `json:"interval_end,omitempty"`
	ReservePoints                  uint64  `json:"reserve_points"`
	NominalParentPoolPoints        uint64  `json:"nominal_parent_pool_points"`
	ConfiguredBestEffortPoints     uint64  `json:"configured_best_effort_points"`
	CapacityAvailable              bool    `json:"capacity_available"`
	CapacityUnavailableReason      string  `json:"capacity_unavailable_reason,omitempty"`
	OnlineCPUs                     *uint64 `json:"online_cpus,omitempty"`
	ProgrammedParentQuotaUsec      *uint64 `json:"programmed_parent_quota_usec,omitempty"`
	ProgrammedParentPeriodUsec     *uint64 `json:"programmed_parent_period_usec,omitempty"`
	ReconciliationDegraded         bool    `json:"reconciliation_degraded"`
	AppliedGuaranteePoints         uint64  `json:"applied_guarantee_points"`
	ProgrammedGuaranteeWeight      uint64  `json:"programmed_guarantee_weight"`
	ProgrammedSiblingWeightSum     *uint64 `json:"programmed_sibling_weight_sum,omitempty"`
	ProgrammedBestEffortWeight     *uint64 `json:"programmed_best_effort_weight,omitempty"`
	ParentCPUUsageUsecDelta        *uint64 `json:"parent_cpu_usage_usec_delta,omitempty"`
	ObservedSiblingWeightSum       *uint64 `json:"observed_sibling_weight_sum,omitempty"`
	ConfiguredRootPoints           *uint64 `json:"configured_root_points,omitempty"`
	ParentCPUPeriodsDelta          *uint64 `json:"parent_cpu_periods_delta,omitempty"`
	ParentCPUThrottledPeriodsDelta *uint64 `json:"parent_cpu_throttled_periods_delta,omitempty"`
	ParentCPUThrottledUsecDelta    *uint64 `json:"parent_cpu_throttled_usec_delta,omitempty"`
	DeliveryState                  string  `json:"delivery_state"`
	DenominatorState               string  `json:"denominator_state"`
}

type cpuPointsUserPayload struct {
	ObservedWeight                    *uint64 `json:"observed_weight,omitempty"`
	IOCoverage                        *string `json:"io_coverage,omitempty"`
	UID                               int     `json:"uid"`
	Username                          string  `json:"username"`
	ConfiguredClass                   string  `json:"configured_class"`
	ConfiguredGuaranteePoints         *uint64 `json:"configured_guarantee_points,omitempty"`
	CPUEnforcementRequested           bool    `json:"cpu_enforcement_requested"`
	LifecycleState                    string  `json:"lifecycle_state"`
	AppliedClass                      *string `json:"applied_class,omitempty"`
	AppliedWeight                     *uint64 `json:"applied_weight,omitempty"`
	AppliedToProcesses                bool    `json:"applied_to_processes"`
	CompleteUIDWorkloadGuaranteed     bool    `json:"complete_uid_workload_guaranteed"`
	ReconciliationDegraded            bool    `json:"reconciliation_degraded"`
	ProcessCoverage                   string  `json:"process_coverage"`
	ObservedProcessCount              *int    `json:"observed_process_count"`
	EnforceableProcessCount           *int    `json:"enforceable_process_count"`
	LeafCPUUsageUsecDelta             *uint64 `json:"leaf_cpu_usage_usec_delta,omitempty"`
	RAMCgroupUsageBytes               *uint64 `json:"ram_cgroup_memory_current_bytes,omitempty"`
	RAMCoverage                       *string `json:"ram_coverage,omitempty"`
	RAMCoverageIncompleteProcessCount int     `json:"ram_coverage_incomplete_process_count"`
	RAMSwapDisabled                   *bool   `json:"ram_swap_disabled,omitempty"`
	MemoryHighLimit                   *string `json:"memory_high_limit,omitempty"`
	MemoryMaxLimit                    *string `json:"memory_max_limit,omitempty"`
	MemorySwapMax                     *string `json:"memory_swap_max,omitempty"`
	MemoryHighEventsDelta             *uint64 `json:"memory_high_events_delta,omitempty"`
	MemoryMaxEventsDelta              *uint64 `json:"memory_max_events_delta,omitempty"`
	MemoryOOMEventsDelta              *uint64 `json:"memory_oom_events_delta,omitempty"`
	MemoryOOMKillEventsDelta          *uint64 `json:"memory_oom_kill_events_delta,omitempty"`
}

func newCPUPointsSystemPayload(snapshot resmanmetrics.CPUPointsSystemSnapshot) cpuPointsSystemPayload {
	result := cpuPointsSystemPayload{
		EnforcementMode: snapshot.EnforcementMode,
		SampleEpochID:   snapshot.SampleEpochID, ReservePoints: snapshot.ReservePoints,
		NominalParentPoolPoints: snapshot.NominalParentPoolPoints, ConfiguredBestEffortPoints: snapshot.ConfiguredBestEffortPoints,
		CapacityAvailable: snapshot.CapacityAvailable, CapacityUnavailableReason: snapshot.CapacityUnavailableReason,
		OnlineCPUs: snapshot.OnlineCPUs, ProgrammedParentQuotaUsec: snapshot.ProgrammedParentQuotaUsec,
		ProgrammedParentPeriodUsec: snapshot.ProgrammedParentPeriodUsec, ReconciliationDegraded: snapshot.ReconciliationDegraded,
		AppliedGuaranteePoints: snapshot.AppliedGuaranteePoints, ProgrammedGuaranteeWeight: snapshot.ProgrammedGuaranteeWeight,
		ProgrammedSiblingWeightSum: snapshot.ProgrammedSiblingWeightSum, ProgrammedBestEffortWeight: snapshot.ProgrammedBestEffortWeight,
		ParentCPUUsageUsecDelta:  snapshot.ParentCPUUsageUsecDelta,
		ObservedSiblingWeightSum: snapshot.ObservedSiblingWeightSum,
		ConfiguredRootPoints:     snapshot.ConfiguredRootPoints,
		ParentCPUPeriodsDelta:    snapshot.ParentCPUPeriodsDelta, ParentCPUThrottledPeriodsDelta: snapshot.ParentCPUThrottledPeriodsDelta,
		ParentCPUThrottledUsecDelta: snapshot.ParentCPUThrottledUsecDelta,
		DeliveryState:               string(snapshot.DeliveryState), DenominatorState: string(snapshot.DenominatorState),
	}
	if snapshot.IntervalStart != nil {
		formatted := snapshot.IntervalStart.Format(time.RFC3339Nano)
		result.IntervalStart = &formatted
	}
	if !snapshot.IntervalEnd.IsZero() {
		result.IntervalEnd = snapshot.IntervalEnd.Format(time.RFC3339Nano)
	}
	return result
}

func newCPUPointsUserPayload(snapshot resmanmetrics.CPUPointsUserSnapshot) cpuPointsUserPayload {
	return cpuPointsUserPayload{
		ObservedWeight: snapshot.ObservedWeight, IOCoverage: snapshot.IOCoverage,
		UID: snapshot.UID, Username: snapshot.Username, ConfiguredClass: snapshot.ConfiguredClass,
		ConfiguredGuaranteePoints: snapshot.ConfiguredGuaranteePoints, CPUEnforcementRequested: snapshot.CPUEnforcementRequested,
		LifecycleState: string(snapshot.LifecycleState), AppliedClass: snapshot.AppliedClass, AppliedWeight: snapshot.AppliedWeight,
		AppliedToProcesses: snapshot.AppliedToProcesses, CompleteUIDWorkloadGuaranteed: snapshot.CompleteUIDWorkloadGuaranteed,
		ReconciliationDegraded: snapshot.ReconciliationDegraded, ProcessCoverage: string(snapshot.ProcessCoverage),
		ObservedProcessCount: optionalProcessValue(snapshot.ObservedProcessCount, snapshot.ProcessObservationUnavailable), EnforceableProcessCount: optionalProcessValue(snapshot.EnforceableProcessCount, snapshot.ProcessObservationUnavailable),
		LeafCPUUsageUsecDelta: snapshot.LeafCPUUsageUsecDelta, RAMCgroupUsageBytes: snapshot.RAMCgroupUsageBytes,
		RAMCoverage: snapshot.RAMCoverage, RAMCoverageIncompleteProcessCount: snapshot.RAMCoverageIncompleteProcessCount,
		RAMSwapDisabled: snapshot.RAMSwapDisabled, MemoryHighLimit: snapshot.MemoryHighLimit, MemoryMaxLimit: snapshot.MemoryMaxLimit,
		MemorySwapMax: snapshot.MemorySwapMax, MemoryHighEventsDelta: snapshot.MemoryHighEventsDelta,
		MemoryMaxEventsDelta: snapshot.MemoryMaxEventsDelta, MemoryOOMEventsDelta: snapshot.MemoryOOMEventsDelta,
		MemoryOOMKillEventsDelta: snapshot.MemoryOOMKillEventsDelta,
	}
}

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
	IOWeightDevices      string  `json:"io_weight_devices"`
	IORootWeight         int     `json:"io_root_weight"`
	IODefaultWeight      int     `json:"io_default_weight"`
	IOUserWeightFile     string  `json:"io_user_weight_file"`
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
		IOWeightDevices:      cfg.IOWeightDevices,
		IORootWeight:         cfg.IORootWeight,
		IODefaultWeight:      cfg.IODefaultWeight,
		IOUserWeightFile:     cfg.IOUserWeightFile,
	}
}

type cpuReportPayload struct {
	Hostname                     string                 `json:"hostname"`
	ServerRole                   string                 `json:"server_role"`
	Report                       string                 `json:"report"`
	TotalCPU                     float64                `json:"total_cpu"`
	TotalCPUAvailable            bool                   `json:"total_cpu_available"`
	TotalCPUUnavailableReason    string                 `json:"total_cpu_unavailable_reason"`
	AverageCPU                   float64                `json:"avg_cpu"`
	PeakCPU                      float64                `json:"peak_cpu"`
	ObservedUsersCount           int                    `json:"observed_users_count"`
	CPUActivelyLimitedUsersCount int                    `json:"cpu_actively_limited_users_count"`
	CPULimitsActive              bool                   `json:"cpu_limits_active"`
	CPUPoints                    cpuPointsSystemPayload `json:"cpu_points"`
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
	CPUAuthorityCoverage              *string  `json:"cpu_authority_coverage"`
	IOCoverage                        *string  `json:"io_coverage"`
	Timestamp                         string   `json:"timestamp"`
	SampleEpochID                     int64    `json:"sample_epoch_id"`
	IntervalStart                     *string  `json:"interval_start"`
	IntervalEnd                       string   `json:"interval_end"`
	UID                               int      `json:"uid"`
	Username                          string   `json:"username"`
	CPUUsage                          *float64 `json:"cpu_usage"`
	MemoryUsage                       *int64   `json:"memory_usage"`
	ProcessCount                      *int     `json:"process_count"`
	EnforceableProcessCount           *int     `json:"enforceable_process_count"`
	CgroupPath                        *string  `json:"cgroup_path"`
	CPUQuota                          *string  `json:"cpu_quota"`
	ConfiguredGuaranteePoints         *uint64  `json:"configured_guarantee_points"`
	ConfiguredCPUClass                string   `json:"configured_cpu_class"`
	CPUPointsLifecycleState           string   `json:"cpu_points_lifecycle_state"`
	AppliedCPUClass                   *string  `json:"applied_cpu_class"`
	AppliedCPUWeight                  *uint64  `json:"applied_cpu_weight"`
	CPUWeight                         *uint64  `json:"cpu_weight"`
	LeafCPUUsageUsecDelta             *uint64  `json:"leaf_cpu_usage_usec_delta"`
	RAMCgroupUsageBytes               *uint64  `json:"ram_cgroup_usage_bytes"`
	RAMCoverage                       *string  `json:"ram_coverage"`
	RAMCoverageIncompleteProcessCount int      `json:"ram_coverage_incomplete_process_count"`
	RAMSwapDisabled                   *bool    `json:"ram_swap_disabled"`
	MemoryHighLimit                   *string  `json:"memory_high_limit"`
	MemoryMaxLimit                    *string  `json:"memory_max_limit"`
	MemorySwapMax                     *string  `json:"memory_swap_max"`
	MemoryHighEventsDelta             *uint64  `json:"memory_high_events_delta"`
	MemoryMaxEventsDelta              *uint64  `json:"memory_max_events_delta"`
	MemoryOOMEventsDelta              *uint64  `json:"memory_oom_events_delta"`
	MemoryOOMKillEventsDelta          *uint64  `json:"memory_oom_kill_events_delta"`
	EligibleForCPU                    bool     `json:"eligible_for_cpu"`
	EligibleForRAM                    bool     `json:"eligible_for_ram"`
	EligibleForIO                     bool     `json:"eligible_for_io"`
	CPULimitRequested                 bool     `json:"cpu_limit_requested"`
	CPULimitActive                    bool     `json:"cpu_limit_active"`
	RAMLimitRequested                 bool     `json:"ram_limit_requested"`
	RAMLimitActive                    bool     `json:"ram_limit_active"`
	IOLimitRequested                  bool     `json:"io_limit_requested"`
	IOLimitActive                     bool     `json:"io_limit_active"`
}

type getUserHistoryResult struct {
	Records   []userHistoryRecord `json:"records"`
	Count     int                 `json:"count"`
	StartTime string              `json:"start_time"`
	EndTime   string              `json:"end_time"`
}

func newUserHistoryRecord(record database.UserMetricsRecord) userHistoryRecord {
	return userHistoryRecord{
		CPUAuthorityCoverage: record.CPUAuthorityCoverage, IOCoverage: record.IOCoverage,
		Timestamp:                         record.Timestamp.Format(time.RFC3339),
		SampleEpochID:                     record.SampleEpochID,
		IntervalStart:                     formatOptionalHistoryTime(record.IntervalStart),
		IntervalEnd:                       record.IntervalEnd.Format(time.RFC3339),
		UID:                               record.UID,
		Username:                          record.Username,
		CPUUsage:                          optionalProcessValue(record.CPUUsagePercent, record.ProcessObservationUnavailable),
		MemoryUsage:                       optionalProcessValue(record.MemoryUsageBytes, record.ProcessObservationUnavailable),
		ProcessCount:                      optionalProcessValue(record.ProcessCount, record.ProcessObservationUnavailable),
		EnforceableProcessCount:           optionalProcessValue(record.EnforceableProcessCount, record.ProcessObservationUnavailable),
		CgroupPath:                        optionalCgroupText(record.CgroupPath),
		CPUQuota:                          optionalCgroupText(record.CPUQuota),
		ConfiguredGuaranteePoints:         record.ConfiguredGuaranteePoints,
		ConfiguredCPUClass:                record.ConfiguredCPUClass,
		CPUPointsLifecycleState:           record.CPUPointsLifecycleState,
		AppliedCPUClass:                   record.AppliedCPUClass,
		AppliedCPUWeight:                  record.AppliedCPUWeight,
		CPUWeight:                         record.CPUWeight,
		LeafCPUUsageUsecDelta:             record.LeafCPUUsageUsecDelta,
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
	IODeviceWeightState                         string          `json:"io_device_weight_state"`
	IODeviceWeightReason                        string          `json:"io_device_weight_reason"`
	IODeviceWeightSelector                      string          `json:"io_device_weight_selector"`
	IODeviceWeightMechanism                     string          `json:"io_device_weight_mechanism"`
	IODeviceWeightClassificationAttempts        uint64          `json:"io_device_weight_classification_attempts"`
	IODeviceWeightProbeAttempts                 uint64          `json:"io_device_weight_probe_attempts"`
	IODeviceWeightProgrammed                    bool            `json:"io_device_weight_programmed"`
	IODeviceWeightProgrammedState               string          `json:"io_device_weight_programmed_state"`
	IODeviceWeightReadBack                      bool            `json:"io_device_weight_read_back"`
	IODeviceWeightReadBackState                 string          `json:"io_device_weight_read_back_state"`
	IODeviceWeightFunctionallyAccepted          bool            `json:"io_device_weight_functionally_accepted"`
	IODeviceWeightEffectQualified               bool            `json:"io_device_weight_effect_qualified"`
	IODeviceWeightEffectQualificationProvenance string          `json:"io_device_weight_effect_qualification_provenance"`
	IODeviceWeightAuthorityCoverage             string          `json:"io_device_weight_authority_coverage"`
	IODeviceWeightCompleteUsers                 int             `json:"io_device_weight_complete_users"`
	IODeviceWeightPartialUsers                  int             `json:"io_device_weight_partial_users"`
	IODeviceWeightUnavailableUsers              int             `json:"io_device_weight_unavailable_users"`
	IODeviceWeightSiblingSlices                 int             `json:"io_device_weight_sibling_slices"`
	IODeviceWeightTotalPoints                   uint64          `json:"io_device_weight_total_points"`
	IODeviceWeightRequestedAt                   *string         `json:"io_device_weight_requested_at"`
	IODeviceWeightNextRetryAt                   *string         `json:"io_device_weight_next_retry_at"`
	IODeviceWeightValues                        json.RawMessage `json:"io_device_weight_values"`
	IODeviceWeightObservedDelivery              string          `json:"io_device_weight_observed_delivery"`
	DenominatorState                            string          `json:"denominator_state"`
	EnforcementMode                             string          `json:"enforcement_mode"`
	Timestamp                                   string          `json:"timestamp"`
	SampleEpochID                               int64           `json:"sample_epoch_id"`
	IntervalStart                               *string         `json:"interval_start"`
	IntervalEnd                                 string          `json:"interval_end"`
	TotalCPUUsage                               float64         `json:"total_cpu_usage"`
	TotalCores                                  int             `json:"total_cores"`
	SystemLoad                                  float64         `json:"system_load"`
	CPULimitsActive                             bool            `json:"cpu_limits_active"`
	ResourceLimitsActive                        bool            `json:"resource_limits_active"`
	AnyLimitsActive                             bool            `json:"any_limits_active"`
	CPUActivelyLimitedUsersCount                int             `json:"cpu_actively_limited_users_count"`
	ActivelyLimitedUsersCount                   int             `json:"actively_limited_users_count"`
	NominalParentPoolPoints                     uint64          `json:"nominal_parent_pool_points"`
	CPUCapacityAvailable                        bool            `json:"cpu_capacity_available"`
	OnlineCPUs                                  *uint64         `json:"online_cpus"`
	ProgrammedParentQuotaUsec                   *uint64         `json:"programmed_parent_quota_usec"`
	ProgrammedParentPeriodUsec                  *uint64         `json:"programmed_parent_period_usec"`
	CPUPointsDegraded                           bool            `json:"cpu_points_degraded"`
	AppliedGuaranteePoints                      uint64          `json:"applied_guarantee_points"`
	ProgrammedGuaranteeWeight                   uint64          `json:"programmed_guarantee_weight"`
	ConfiguredBestEffortPoints                  uint64          `json:"configured_best_effort_points"`
	ParentCPUQuota                              *string         `json:"parent_cpu_quota"`
	ProgrammedSiblingWeightSum                  *uint64         `json:"programmed_sibling_weight_sum"`
	ProgrammedBestEffortWeight                  *uint64         `json:"programmed_best_effort_weight"`
	ParentCPUUsageUsecDelta                     *uint64         `json:"parent_cpu_usage_usec_delta"`
	ObservedSiblingWeightSum                    *uint64         `json:"observed_sibling_weight_sum"`
	ConfiguredRootPoints                        *uint64         `json:"configured_root_points"`
	ParentCPUPeriodsDelta                       *uint64         `json:"parent_cpu_periods_delta"`
	ParentCPUThrottledPeriodsDelta              *uint64         `json:"parent_cpu_throttled_periods_delta"`
	ParentCPUThrottledUsecDelta                 *uint64         `json:"parent_cpu_throttled_usec_delta"`
}

type getSystemHistoryResult struct {
	Records   []systemHistoryRecord `json:"records"`
	Count     int                   `json:"count"`
	StartTime string                `json:"start_time"`
	EndTime   string                `json:"end_time"`
}

func newSystemHistoryRecord(record database.SystemMetricsRecord) systemHistoryRecord {
	return systemHistoryRecord{
		IODeviceWeightState: record.IODeviceWeightState, IODeviceWeightReason: record.IODeviceWeightReason,
		IODeviceWeightSelector:                      record.IODeviceWeightSelector,
		IODeviceWeightMechanism:                     record.IODeviceWeightMechanism,
		IODeviceWeightClassificationAttempts:        record.IODeviceWeightClassificationAttempts,
		IODeviceWeightProbeAttempts:                 record.IODeviceWeightProbeAttempts,
		IODeviceWeightProgrammed:                    record.IODeviceWeightProgrammed,
		IODeviceWeightProgrammedState:               record.IODeviceWeightProgrammedState,
		IODeviceWeightReadBack:                      record.IODeviceWeightReadBack,
		IODeviceWeightReadBackState:                 record.IODeviceWeightReadBackState,
		IODeviceWeightFunctionallyAccepted:          record.IODeviceWeightFunctionallyAccepted,
		IODeviceWeightEffectQualified:               record.IODeviceWeightEffectQualified,
		IODeviceWeightEffectQualificationProvenance: record.IODeviceWeightEffectQualificationProvenance,
		IODeviceWeightAuthorityCoverage:             record.IODeviceWeightAuthorityCoverage,
		IODeviceWeightCompleteUsers:                 record.IODeviceWeightCompleteUsers,
		IODeviceWeightPartialUsers:                  record.IODeviceWeightPartialUsers,
		IODeviceWeightUnavailableUsers:              record.IODeviceWeightUnavailableUsers,
		IODeviceWeightSiblingSlices:                 record.IODeviceWeightSiblingSlices,
		IODeviceWeightTotalPoints:                   record.IODeviceWeightTotalPoints,
		IODeviceWeightRequestedAt:                   formatOptionalHistoryTime(record.IODeviceWeightRequestedAt),
		IODeviceWeightNextRetryAt:                   formatOptionalHistoryTime(record.IODeviceWeightNextRetryAt),
		IODeviceWeightValues:                        json.RawMessage(record.IODeviceWeightValuesJSON),
		IODeviceWeightObservedDelivery:              record.IODeviceWeightObservedDelivery,
		DenominatorState:                            record.DenominatorState, EnforcementMode: record.EnforcementMode,
		Timestamp:                      record.Timestamp.Format(time.RFC3339),
		SampleEpochID:                  record.SampleEpochID,
		IntervalStart:                  formatOptionalHistoryTime(record.IntervalStart),
		IntervalEnd:                    record.IntervalEnd.Format(time.RFC3339),
		TotalCPUUsage:                  record.TotalCPUUsagePercent,
		TotalCores:                     record.TotalCores,
		SystemLoad:                     record.SystemLoad,
		CPULimitsActive:                record.CPULimitsActive,
		ResourceLimitsActive:           record.ResourceLimitsActive,
		AnyLimitsActive:                record.AnyLimitsActive,
		CPUActivelyLimitedUsersCount:   record.CPUActivelyLimitedUsersCount,
		ActivelyLimitedUsersCount:      record.ActivelyLimitedUsersCount,
		NominalParentPoolPoints:        record.NominalParentPoolPoints,
		CPUCapacityAvailable:           record.CPUCapacityAvailable,
		OnlineCPUs:                     record.OnlineCPUs,
		ProgrammedParentQuotaUsec:      record.ProgrammedParentQuotaUsec,
		ProgrammedParentPeriodUsec:     record.ProgrammedParentPeriodUsec,
		CPUPointsDegraded:              record.CPUPointsDegraded,
		AppliedGuaranteePoints:         record.AppliedGuaranteePoints,
		ProgrammedGuaranteeWeight:      record.ProgrammedGuaranteeWeight,
		ConfiguredBestEffortPoints:     record.ConfiguredBestEffortPoints,
		ParentCPUQuota:                 record.ParentCPUQuota,
		ProgrammedSiblingWeightSum:     record.ProgrammedSiblingWeightSum,
		ProgrammedBestEffortWeight:     record.ProgrammedBestEffortWeight,
		ParentCPUUsageUsecDelta:        record.ParentCPUUsageUsecDelta,
		ObservedSiblingWeightSum:       record.ObservedSiblingWeightSum,
		ConfiguredRootPoints:           record.ConfiguredRootPoints,
		ParentCPUPeriodsDelta:          record.ParentCPUPeriodsDelta,
		ParentCPUThrottledPeriodsDelta: record.ParentCPUThrottledPeriodsDelta,
		ParentCPUThrottledUsecDelta:    record.ParentCPUThrottledUsecDelta,
	}
}

func formatOptionalHistoryTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.Format(time.RFC3339)
	return &formatted
}

func optionalCgroupText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalProcessValue[T any](value T, unavailable bool) *T {
	if unavailable {
		return nil
	}
	return &value
}
