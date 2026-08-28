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
// mcp/tools.go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

const (
	defaultHistoryLimit                   = 100
	maxHistoryLimit                       = 10000
	maxLegacyArtifactNamesInCleanupNotice = 3
)

// getHostname returns the current hostname
func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// Tool input/output structures

type GetUserMetricsArgs struct {
	UIDs     []int  `json:"uids,omitempty"`
	Username string `json:"username,omitempty"`
}

type UserMetric struct {
	UID               int     `json:"uid"`
	Username          string  `json:"username"`
	CPUUsage          float64 `json:"cpu_usage"`
	MemoryUsage       uint64  `json:"memory_usage"`
	ProcessCount      int     `json:"process_count"`
	EligibleForCPU    bool    `json:"eligible_for_cpu"`
	EligibleForRAM    bool    `json:"eligible_for_ram"`
	EligibleForIO     bool    `json:"eligible_for_io"`
	CPULimitRequested bool    `json:"cpu_limit_requested"`
	CPULimitActive    bool    `json:"cpu_limit_active"`
	RAMLimitRequested bool    `json:"ram_limit_requested"`
	RAMLimitActive    bool    `json:"ram_limit_active"`
	IOLimitRequested  bool    `json:"io_limit_requested"`
	IOLimitActive     bool    `json:"io_limit_active"`
	// RAM cgroup metrics
	CgroupMemoryCurrentBytes uint64 `json:"cgroup_memory_current_bytes,omitempty"`
	MemoryMax                string `json:"memory_max,omitempty"`
	MemoryHigh               string `json:"memory_high,omitempty"`
	MemoryHighEvents         uint64 `json:"memory_high_events,omitempty"`
	// IO cgroup metrics
	IOReadBytes  uint64 `json:"io_read_bytes,omitempty"`
	IOWriteBytes uint64 `json:"io_write_bytes,omitempty"`
	IOReadOps    uint64 `json:"io_read_ops,omitempty"`
	IOWriteOps   uint64 `json:"io_write_ops,omitempty"`
}

func newUserMetric(uid int, sample *resmanmetrics.UserMetrics, limitState state.UserLimitState) UserMetric {
	return UserMetric{
		UID:               uid,
		Username:          sample.Username,
		CPUUsage:          sample.CPUUsage,
		MemoryUsage:       sample.MemoryUsage,
		ProcessCount:      sample.ProcessCount,
		EligibleForCPU:    limitState.EligibleForCPU,
		EligibleForRAM:    limitState.EligibleForRAM,
		EligibleForIO:     limitState.EligibleForIO,
		CPULimitRequested: limitState.CPULimitRequested,
		CPULimitActive:    limitState.CPULimitActive,
		RAMLimitRequested: limitState.RAMLimitRequested,
		RAMLimitActive:    limitState.RAMLimitActive,
		IOLimitRequested:  limitState.IOLimitRequested,
		IOLimitActive:     limitState.IOLimitActive,
	}
}

type GetUserMetricsResult struct {
	Users []UserMetric `json:"users"`
}

type GetCgroupInfoArgs struct {
	UID int `json:"uid"`
}

// GetCgroupInfoResult reports cgroup values and their explicit availability.
type GetCgroupInfoResult struct {
	Path                           string                             `json:"path"`
	CPUQuota                       string                             `json:"cpu_max,omitempty"`
	CPUQuotaAvailable              bool                               `json:"cpu_max_available"`
	CPUQuotaUnavailableReason      cgroup.CgroupFileUnavailableReason `json:"cpu_max_unavailable_reason,omitempty"`
	CPUWeight                      string                             `json:"cpu_weight,omitempty"`
	CPUWeightAvailable             bool                               `json:"cpu_weight_available"`
	CPUWeightUnavailableReason     cgroup.CgroupFileUnavailableReason `json:"cpu_weight_unavailable_reason,omitempty"`
	MemoryCurrent                  string                             `json:"memory_current,omitempty"`
	MemoryCurrentAvailable         bool                               `json:"memory_current_available"`
	MemoryCurrentUnavailableReason cgroup.CgroupFileUnavailableReason `json:"memory_current_unavailable_reason,omitempty"`
	MemoryMax                      string                             `json:"memory_max,omitempty"`
	MemoryMaxAvailable             bool                               `json:"memory_max_available"`
	MemoryMaxUnavailableReason     cgroup.CgroupFileUnavailableReason `json:"memory_max_unavailable_reason,omitempty"`
	MemoryHigh                     string                             `json:"memory_high,omitempty"`
	MemoryHighAvailable            bool                               `json:"memory_high_available"`
	MemoryHighUnavailableReason    cgroup.CgroupFileUnavailableReason `json:"memory_high_unavailable_reason,omitempty"`
	IOReadBPS                      string                             `json:"io_read_bps,omitempty"`
	IOWriteBPS                     string                             `json:"io_write_bps,omitempty"`
	IOReadIOPS                     string                             `json:"io_read_iops,omitempty"`
	IOWriteIOPS                    string                             `json:"io_write_iops,omitempty"`
}

type userFilterKind string

const (
	userFilterInclude userFilterKind = "include"
	userFilterExclude userFilterKind = "exclude"
)

type userFilterUpdateResult struct {
	Success       bool     `json:"success"`
	Message       string   `json:"message"`
	PreviousValue []string `json:"previous_value"`
	NewValue      []string `json:"new_value"`
	Persisted     bool     `json:"persisted"`
	Applied       bool     `json:"applied"`
	Error         string   `json:"error,omitempty"`
}

// Historical metrics tools structures

type GetHistoryArgs struct {
	UID       *int   `json:"uid,omitempty"`
	Username  string `json:"username,omitempty"`
	StartTime string `json:"startTime,omitempty"`
	EndTime   string `json:"endTime,omitempty"`
	Period    string `json:"period,omitempty"`
	Hours     int    `json:"hours,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type GetUserSummaryResult struct {
	UID                       int     `json:"uid"`
	Username                  string  `json:"username"`
	PeriodStart               string  `json:"period_start"`
	PeriodEnd                 string  `json:"period_end"`
	CPUAvg                    float64 `json:"cpu_avg"`
	CPUMin                    float64 `json:"cpu_min"`
	CPUMax                    float64 `json:"cpu_max"`
	MemoryAvg                 float64 `json:"memory_avg"`
	MemoryMin                 float64 `json:"memory_min"`
	MemoryMax                 float64 `json:"memory_max"`
	ProcessCountAvg           float64 `json:"process_count_avg"`
	CPULimitActiveTimePercent float64 `json:"cpu_limit_active_time_percent"`
	Samples                   int     `json:"samples"`
}

type GetMetricsDatabaseInfoResult struct {
	Path               string  `json:"path"`
	SizeMB             float64 `json:"size_mb"`
	UserMetricsCount   int64   `json:"user_metrics_count"`
	SystemMetricsCount int64   `json:"system_metrics_count"`
	OldestRecord       string  `json:"oldest_record"`
	NewestRecord       string  `json:"newest_record"`
	RetentionDays      int     `json:"retention_days"`
	UsersTracked       int64   `json:"users_tracked"`
}

type GetControlHistoryArgs struct {
	Limit int `json:"limit"`
}

type ControlHistoryEntry struct {
	Timestamp                    string  `json:"timestamp"`
	Decision                     string  `json:"decision"`
	Reason                       string  `json:"reason"`
	TotalCPUUsage                float64 `json:"total_cpu_usage"`
	CPUEligibleCPUUsage          float64 `json:"cpu_eligible_users_cpu_usage"`
	ObservedUsersCount           int     `json:"observed_users_count"`
	CPUActivelyLimitedUsersCount int     `json:"cpu_actively_limited_users_count"`
	CPULimitsActive              bool    `json:"cpu_limits_active"`
	DurationMs                   int64   `json:"duration_ms"`
}

type GetControlHistoryResult struct {
	Entries []ControlHistoryEntry `json:"entries"`
}

type ActivateLimitsArgs struct {
	Force bool `json:"force"`
}

type systemStatusPayload struct {
	Hostname                  string  `json:"hostname"`
	ServerRole                string  `json:"server_role"`
	TotalCPUUsage             float64 `json:"total_cpu_usage"`
	ObservedUsersCPUUsage     float64 `json:"observed_users_cpu_usage"`
	MemoryUsageMB             float64 `json:"memory_usage_mb"`
	ObservedUsersCount        int     `json:"observed_users_count"`
	ActivelyLimitedUsersCount int     `json:"actively_limited_users_count"`
	TotalCores                int     `json:"total_cores"`
	SystemUnderLoad           bool    `json:"system_under_load"`
	AnyLimitsActive           bool    `json:"any_limits_active"`
	CPULimitsActive           bool    `json:"cpu_limits_active"`
	ResourceLimitsActive      bool    `json:"resource_limits_active"`
	CPULimitsAppliedTime      string  `json:"cpu_limits_applied_time"`
	ResourceLimitsAppliedTime string  `json:"resource_limits_applied_time"`
	SharedCgroupActive        bool    `json:"shared_cgroup_active"`
}

type limitsStatusPayload struct {
	Hostname                     string `json:"hostname"`
	ServerRole                   string `json:"server_role"`
	AnyLimitsActive              bool   `json:"any_limits_active"`
	CPULimitsActive              bool   `json:"cpu_limits_active"`
	ResourceLimitsActive         bool   `json:"resource_limits_active"`
	CPULimitsAppliedTime         string `json:"cpu_limits_applied_time"`
	ResourceLimitsAppliedTime    string `json:"resource_limits_applied_time"`
	ActivelyLimitedUsersCount    int    `json:"actively_limited_users_count"`
	ActivelyLimitedUsers         []int  `json:"actively_limited_users"`
	CPUActivelyLimitedUsersCount int    `json:"cpu_actively_limited_users_count"`
	CPUActivelyLimitedUsers      []int  `json:"cpu_actively_limited_users"`
	SharedCgroupPath             string `json:"shared_cgroup_path"`
	SharedCgroupActive           bool   `json:"shared_cgroup_active"`
	SharedCgroupQuota            string `json:"shared_cgroup_quota,omitempty"`
	SharedCgroupUserCount        int    `json:"shared_cgroup_user_count"`
}

func newSystemStatusPayload(hostname, serverRole string, observation resmanmetrics.ObservationMetrics, runtime state.RuntimeStatus) systemStatusPayload {
	return systemStatusPayload{
		Hostname:                  hostname,
		ServerRole:                serverRole,
		TotalCPUUsage:             observation.TotalCPUUsage,
		ObservedUsersCPUUsage:     observation.ObservedUsersCPUUsage,
		MemoryUsageMB:             observation.MemoryUsageMB,
		ObservedUsersCount:        observation.ObservedUsersCount,
		ActivelyLimitedUsersCount: runtime.ActivelyLimitedUsersCount,
		TotalCores:                observation.TotalCores,
		SystemUnderLoad:           observation.SystemUnderLoad,
		AnyLimitsActive:           runtime.AnyLimitsActive,
		CPULimitsActive:           runtime.CPULimitsActive,
		ResourceLimitsActive:      runtime.ResourceLimitsActive,
		CPULimitsAppliedTime:      formatOptionalTime(runtime.CPULimitsAppliedTime),
		ResourceLimitsAppliedTime: formatOptionalTime(runtime.ResourceLimitsAppliedTime),
		SharedCgroupActive:        runtime.SharedCgroupActive,
	}
}

func newLimitsStatusPayload(hostname, serverRole string, runtime state.RuntimeStatus) limitsStatusPayload {
	return limitsStatusPayload{
		Hostname:                     hostname,
		ServerRole:                   serverRole,
		AnyLimitsActive:              runtime.AnyLimitsActive,
		CPULimitsActive:              runtime.CPULimitsActive,
		ResourceLimitsActive:         runtime.ResourceLimitsActive,
		CPULimitsAppliedTime:         formatOptionalTime(runtime.CPULimitsAppliedTime),
		ResourceLimitsAppliedTime:    formatOptionalTime(runtime.ResourceLimitsAppliedTime),
		ActivelyLimitedUsersCount:    runtime.ActivelyLimitedUsersCount,
		ActivelyLimitedUsers:         runtime.ActivelyLimitedUsers,
		CPUActivelyLimitedUsersCount: runtime.CPUActivelyLimitedUsersCount,
		CPUActivelyLimitedUsers:      runtime.CPUActivelyLimitedUsers,
		SharedCgroupPath:             runtime.SharedCgroupPath,
		SharedCgroupActive:           runtime.SharedCgroupActive,
		SharedCgroupQuota:            runtime.SharedCgroupQuota,
		SharedCgroupUserCount:        runtime.SharedCgroupUserCount,
	}
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

// registerTools registers all MCP tools
func (s *Server) registerTools() {
	// get_system_status - registered manually with explicit empty schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_system_status",
		Description: "Get current CPU and memory status of the system",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		status := s.stateManager.GetStatus()
		metrics := s.metricsCollector.GetObservationMetrics()
		hostname := getHostname()
		serverRole := s.stateManager.GetConfig().ServerRole
		result := newSystemStatusPayload(hostname, serverRole, metrics, status)

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(result)},
			},
			StructuredContent: result,
		}, nil
	})

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_user_metrics",
		Description: "Get CPU, memory, and process metrics for specific user(s)",
	}, s.handleGetUserMetrics)

	// get_active_users - registered manually with explicit empty schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_active_users",
		Description: "List all active non-system users currently running processes",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		activeUsers := s.metricsCollector.GetAllUsers()
		allMetrics := s.metricsCollector.GetAllUserMetrics()
		result := newActiveUsersPayload(getHostname(), s.stateManager.GetConfig().ServerRole, activeUsers, allMetrics)

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(result)},
			},
			StructuredContent: result,
		}, nil
	})

	// get_limits_status - registered manually with explicit empty schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_limits_status",
		Description: "Check if CPU limits are currently active and get details",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		status := s.stateManager.GetStatus()
		hostname := getHostname()
		serverRole := s.stateManager.GetConfig().ServerRole

		result := newLimitsStatusPayload(hostname, serverRole, status)

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(result)},
			},
			StructuredContent: result,
		}, nil
	})

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_cgroup_info",
		Description: "Get cgroup details for a specific user",
	}, s.handleGetCgroupInfo)

	// get_configuration - registered manually with explicit empty schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_configuration",
		Description: "Get current CPU, RAM, and I/O resource-policy configuration",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cfg := s.stateManager.GetConfig()
		result := newResourcePolicyConfigurationPayload(getHostname(), cfg)

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(result)},
			},
			StructuredContent: result,
		}, nil
	})

	// get_cpu_report - registered manually with explicit empty schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_cpu_report",
		Description: "Generate a comprehensive CPU usage report with active users and their limits status",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		metrics := s.metricsCollector.GetObservationMetrics()
		status := s.stateManager.GetStatus()
		allUserMetrics := s.metricsCollector.GetAllUserMetrics()
		hostname := getHostname()
		serverRole := s.stateManager.GetConfig().ServerRole

		// Build the user list with CPU enforcement details.
		var users []string
		var peakCPU float64
		limitedCount := 0

		for uid, userMetrics := range allUserMetrics {
			isLimited := s.stateManager.GetUserLimitState(uid, userMetrics.Username).CPULimitActive
			if isLimited {
				limitedCount++
			}

			limitStatus := "Inactive"
			if isLimited {
				limitStatus = "Active"
			}

			userLine := fmt.Sprintf("%s\n    CPU usage: %.1f%%\n    CPU limits: %s",
				userMetrics.Username,
				userMetrics.CPUUsage,
				limitStatus,
			)
			users = append(users, userLine)

			if userMetrics.CPUUsage > peakCPU {
				peakCPU = userMetrics.CPUUsage
			}
		}

		// Calculate average CPU usage.
		avgCPU := 0.0
		if len(allUserMetrics) > 0 {
			for _, m := range allUserMetrics {
				avgCPU += m.CPUUsage
			}
			avgCPU /= float64(len(allUserMetrics))
		}

		// Read observed CPU enforcement state.
		limitsActive := status.CPULimitsActive
		limitsStatus := "Inactive"
		if limitsActive {
			limitsStatus = "Active"
		}

		// Build the report text.
		report := fmt.Sprintf(`CPU Usage Report
Hostname: %s
Server Role: %s
Date: %s
Total CPU capacity: %.1f%%
Current usage: %.1f%%

Observed Users:
%s

Resource Status:
Average CPU usage: %.1f%%
Peak CPU usage: %.1f%%
CPU limits: %s
CPU-limited users: %d of %d
`,
			hostname,
			serverRole,
			time.Now().Format("2006-01-02 15:04:05"),
			totalCPUCapacityPercent(metrics),
			metrics.TotalCPUUsage,
			joinStrings(users, "\n"),
			avgCPU,
			peakCPU,
			limitsStatus,
			limitedCount,
			len(allUserMetrics),
		)

		result := cpuReportPayload{
			Hostname:                     hostname,
			ServerRole:                   serverRole,
			Report:                       report,
			TotalCPU:                     metrics.TotalCPUUsage,
			AverageCPU:                   avgCPU,
			PeakCPU:                      peakCPU,
			ObservedUsersCount:           len(allUserMetrics),
			CPUActivelyLimitedUsersCount: limitedCount,
			CPULimitsActive:              limitsActive,
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: report},
			},
			StructuredContent: result,
		}, nil
	})

	// get_mem_report - registered manually with explicit empty schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_mem_report",
		Description: "Generate a comprehensive memory usage report with active users and their memory consumption",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		metrics := s.metricsCollector.GetObservationMetrics()
		status := s.stateManager.GetStatus()
		allUserMetrics := s.metricsCollector.GetAllUserMetrics()
		hostname := getHostname()
		serverRole := s.stateManager.GetConfig().ServerRole

		// Build the user list with RAM enforcement details.
		var users []string
		var peakMem uint64
		limitedCount := 0

		for uid, userMetrics := range allUserMetrics {
			limitState := s.stateManager.GetUserLimitState(uid, userMetrics.Username)
			isLimited := limitState.RAMLimitActive
			if isLimited {
				limitedCount++
			}

			limitStatus := "Inactive"
			if isLimited {
				limitStatus = "Active"
			}

			// Convert bytes to MB for readability.
			memMB := float64(userMetrics.MemoryUsage) / 1024 / 1024

			userLine := fmt.Sprintf("%s\n    Memory: %.1f MB (%d bytes)\n    Processes: %d\n    RAM limits: %s",
				userMetrics.Username,
				memMB,
				userMetrics.MemoryUsage,
				userMetrics.ProcessCount,
				limitStatus,
			)
			users = append(users, userLine)

			if userMetrics.MemoryUsage > peakMem {
				peakMem = userMetrics.MemoryUsage
			}
		}

		// Calculate average memory usage.
		avgMem := uint64(0)
		if len(allUserMetrics) > 0 {
			for _, m := range allUserMetrics {
				avgMem += m.MemoryUsage
			}
			avgMem /= uint64(len(allUserMetrics))
		}

		// Read system memory information.
		totalMemMB := totalSystemMemoryMB(metrics)

		// Read observed resource enforcement state.
		limitsActive := status.ResourceLimitsActive
		limitsStatus := "Inactive"
		if limitsActive {
			limitsStatus = "Active"
		}

		// Build the report text.
		report := fmt.Sprintf(`Memory Usage Report
Hostname: %s
Server Role: %s
Date: %s
Total System Memory: %.1f MB

Observed Users:
%s

Resource Status:
Average memory usage: %.1f MB
Peak memory usage: %.1f MB
RAM limits: %s
RAM-limited users: %d of %d
`,
			hostname,
			serverRole,
			time.Now().Format("2006-01-02 15:04:05"),
			totalMemMB,
			joinStrings(users, "\n"),
			float64(avgMem)/1024/1024,
			float64(peakMem)/1024/1024,
			limitsStatus,
			limitedCount,
			len(allUserMetrics),
		)

		result := memoryReportPayload{
			Hostname:                     hostname,
			ServerRole:                   serverRole,
			Report:                       report,
			TotalMemoryMB:                totalMemMB,
			AverageMemoryMB:              float64(avgMem) / 1024 / 1024,
			PeakMemoryMB:                 float64(peakMem) / 1024 / 1024,
			ObservedUsersCount:           len(allUserMetrics),
			RAMActivelyLimitedUsersCount: limitedCount,
			ResourceLimitsActive:         limitsActive,
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: report},
			},
			StructuredContent: result,
		}, nil
	})

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_control_history",
		Description: "Get recent control cycle history",
	}, s.handleGetControlHistory)

	// Write operation tools (only if allowed)
	if s.cfg.AllowWriteOps {
		mcp.AddTool(s.mcpServer, &mcp.Tool{
			Name:        "activate_limits",
			Description: "Manually activate CPU limits for active users",
		}, s.handleActivateLimits)

		// deactivate_limits - registered manually with explicit empty schema
		s.mcpServer.AddTool(&mcp.Tool{
			Name:        "deactivate_limits",
			Description: "Manually deactivate CPU limits",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			err := s.stateManager.ForceDeactivateLimits()

			success := err == nil
			message := "Limits deactivated successfully"
			if err != nil {
				message = "Failed to deactivate limits: " + err.Error()
			}

			result := limitActionResult{Success: success, Message: message}
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: toJSON(result)},
				},
				StructuredContent: result,
			}, nil
		})
	}

	s.registerUserFilterTool(
		"set_user_exclude_list",
		"Persist and apply the users excluded from CPU limits (regex patterns supported)",
		"List of regex patterns for users to exclude",
		userFilterExclude,
	)
	s.registerUserFilterTool(
		"set_user_include_list",
		"Persist and apply CPU-limit eligibility patterns; an empty list disables CPU limiting and .* includes every non-excluded user",
		"CPU eligibility regex patterns; use an empty array for no users or [\".*\"] for all non-excluded users",
		userFilterInclude,
	)

	// get_user_filters - registered manually with explicit empty schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_user_filters",
		Description: "Get current user include and exclude filter configurations",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cfg := s.stateManager.GetConfig()

		result := userFiltersPayload{
			UserIncludeList: cfg.GetUserIncludeList(),
			UserExcludeList: cfg.GetUserExcludeList(),
			ConfigFile:      cfg.ConfigFile,
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(result)},
			},
			StructuredContent: result,
		}, nil
	})

	// validate_user_filter_pattern - registered manually with explicit schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "validate_user_filter_pattern",
		Description: "Validate if a regex pattern is valid and show example matches",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Regex pattern to validate",
				},
				"type": map[string]any{
					"type":        "string",
					"description": "Filter type: 'include' or 'exclude'",
					"enum":        []string{"include", "exclude"},
				},
			},
			"required": []string{"pattern"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Unmarshal arguments from json.RawMessage
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return &mcp.CallToolResult{}, fmt.Errorf("invalid parameters: %w", err)
		}

		// Extract pattern
		pattern, ok := args["pattern"].(string)
		if !ok {
			return &mcp.CallToolResult{}, fmt.Errorf("invalid or missing pattern parameter")
		}

		// Get type parameter (optional)
		filterType := "unspecified"
		if typeRaw, ok := args["type"].(string); ok {
			filterType = typeRaw
		}

		// Validate regex pattern
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			result := validateUserFilterResult{Valid: false, Error: err.Error()}
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: toJSON(result)},
				},
				StructuredContent: result,
			}, nil
		}

		// Test against some example usernames
		testUsers := []string{"francesco", "www-data", "mysql", "nobody", "root",
			"test-user", "dev-web", "app-prod", "svc-db", "admin"}

		matches := make([]string, 0)
		for _, user := range testUsers {
			if compiled.MatchString(user) {
				matches = append(matches, user)
			}
		}

		matchCount := len(matches)
		result := validateUserFilterResult{
			Valid:       true,
			Pattern:     pattern,
			Type:        filterType,
			TestMatches: &matches,
			MatchCount:  &matchCount,
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(result)},
			},
			StructuredContent: result,
		}, nil
	})

	// get_user_history - Get historical metrics for a specific user
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_user_history",
		Description: "Get historical CPU and memory metrics for a specific user. Supports time ranges via startTime/endTime, period (today, yesterday, last_24_hours, last_7_days, last_30_days), or hours parameter. The limit defaults to 100 and is capped at 10000 records",
	}, s.handleGetUserHistory)

	// get_system_history - Get historical system metrics
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_system_history",
		Description: "Get historical system-wide CPU and memory metrics. Supports time ranges via startTime/endTime, period (today, yesterday, last_24_hours, last_7_days, last_30_days), or hours parameter. The limit defaults to 100 and is capped at 10000 records",
	}, s.handleGetSystemHistory)

	// get_user_summary - Get aggregated statistics for a user
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_user_summary",
		Description: "Get aggregated statistics (avg, min, max) for a specific user over a time period",
	}, s.handleGetUserSummary)

	// get_metrics_database_info - Get information about the metrics database
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_metrics_database_info",
		Description: "Get information about the metrics database including size, record counts, and retention",
	}, s.handleGetMetricsDatabaseInfo)
}

func (s *Server) registerUserFilterTool(name, description, patternDescription string, kind userFilterKind) {
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        name,
		Description: description,
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"patterns": map[string]any{
					"type":        "array",
					"description": patternDescription,
					"items": map[string]any{
						"type": "string",
					},
				},
			},
			"required": []string{"patterns"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return userFilterCallResult(userFilterUpdateResult{}, fmt.Errorf("invalid parameters: %w", err)), nil
		}

		patterns, err := parseUserFilterPatterns(args)
		if err != nil {
			return userFilterCallResult(userFilterUpdateResult{}, err), nil
		}

		result, err := s.updateUserFilter(ctx, kind, patterns)
		return userFilterCallResult(result, err), nil
	})
}

func parseUserFilterPatterns(args map[string]any) ([]string, error) {
	if _, exists := args["reload"]; exists {
		return nil, fmt.Errorf("parameter %q was removed; filter updates are always persisted and synchronously applied", "reload")
	}
	for key := range args {
		if key != "patterns" {
			return nil, fmt.Errorf("unknown parameter %q", key)
		}
	}

	rawPatterns, exists := args["patterns"]
	if !exists {
		return nil, fmt.Errorf("missing required parameter %q", "patterns")
	}
	values, ok := rawPatterns.([]any)
	if !ok {
		return nil, fmt.Errorf("parameter %q must be an array of strings", "patterns")
	}
	patterns := make([]string, len(values))
	for index, value := range values {
		pattern, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("parameter %q item %d must be a string", "patterns", index)
		}
		patterns[index] = pattern
	}
	return patterns, nil
}

func (s *Server) updateUserFilter(ctx context.Context, kind userFilterKind, patterns []string) (userFilterUpdateResult, error) {
	result := userFilterUpdateResult{
		PreviousValue: []string{},
		NewValue:      append([]string{}, patterns...),
	}
	if !s.cfg.AllowWriteOps {
		return result, fmt.Errorf("MCP write operations are disabled")
	}
	if s.stateManager == nil {
		return result, fmt.Errorf("state manager is not available")
	}
	if s.configReloader == nil {
		return result, fmt.Errorf("configuration reloader is not available")
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("before persisting user %s filters: %w", kind, err)
	}
	if !s.configWriteActive.CompareAndSwap(false, true) {
		return result, fmt.Errorf("another MCP configuration update is already in progress")
	}
	defer s.configWriteActive.Store(false)

	cfg := s.stateManager.GetConfig()
	if cfg == nil {
		return result, fmt.Errorf("runtime configuration is not available")
	}

	var (
		persistenceResult config.UserFilterPersistenceResult
		err               error
	)
	switch kind {
	case userFilterInclude:
		persistenceResult, err = cfg.PersistUserIncludeList(patterns, cfg.ConfigFile)
	case userFilterExclude:
		persistenceResult, err = cfg.PersistUserExcludeList(patterns, cfg.ConfigFile)
	default:
		return result, fmt.Errorf("unsupported user filter kind %q", kind)
	}
	result.PreviousValue = append([]string{}, persistenceResult.PreviousValue...)
	s.reportLegacyArtifactCleanup(persistenceResult.PersistenceResult)
	if err != nil {
		return result, fmt.Errorf("persist user %s filters: %w", kind, err)
	}
	result.Persisted = true

	if err := s.configReloader.Reload(ctx); err != nil {
		result.Applied = s.userFilterMatchesRuntime(kind, patterns)
		return result, fmt.Errorf("configuration persisted but runtime reload was not confirmed: %w", err)
	}
	result.Applied = s.userFilterMatchesRuntime(kind, patterns)
	if !result.Applied {
		return result, fmt.Errorf("reload completed without applying the requested user %s filters", kind)
	}

	result.Success = true
	result.Message = fmt.Sprintf("User %s filters persisted and applied successfully", kind)
	return result, nil
}

func (s *Server) reportLegacyArtifactCleanup(result config.PersistenceResult) {
	if len(result.RemovedLegacyArtifacts) == 0 {
		return
	}
	visibleCount := min(len(result.RemovedLegacyArtifacts), maxLegacyArtifactNamesInCleanupNotice)
	visibleNames := strings.Join(result.RemovedLegacyArtifacts[:visibleCount], ",")
	s.logger.Warn(
		"Removed insecure legacy configuration artifacts during persistence",
		"removed_count", len(result.RemovedLegacyArtifacts),
		"removed_basenames", visibleNames,
		"omitted_count", len(result.RemovedLegacyArtifacts)-visibleCount,
	)
}

func (s *Server) userFilterMatchesRuntime(kind userFilterKind, patterns []string) bool {
	cfg := s.stateManager.GetConfig()
	if cfg == nil {
		return false
	}
	switch kind {
	case userFilterInclude:
		return slices.Equal(cfg.GetUserIncludeList(), patterns)
	case userFilterExclude:
		return slices.Equal(cfg.GetUserExcludeList(), patterns)
	default:
		return false
	}
}

func userFilterCallResult(result userFilterUpdateResult, err error) *mcp.CallToolResult {
	if err != nil {
		result.Success = false
		result.Error = err.Error()
		result.Message = "User filter update failed: " + err.Error()
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: toJSON(result)},
		},
		StructuredContent: result,
		IsError:           err != nil,
	}
}

// handleGetUserMetrics handles get_user_metrics tool requests
func (s *Server) handleGetUserMetrics(ctx context.Context, req *mcp.CallToolRequest, args GetUserMetricsArgs) (*mcp.CallToolResult, GetUserMetricsResult, error) {
	allMetrics := s.metricsCollector.GetAllUserMetrics()
	result := GetUserMetricsResult{
		Users: make([]UserMetric, 0),
	}

	for uid, metrics := range allMetrics {
		// Filter by UIDs if provided
		if len(args.UIDs) > 0 {
			found := false
			for _, id := range args.UIDs {
				if id == uid {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Filter by username if provided
		if args.Username != "" && metrics.Username != args.Username {
			continue
		}

		result.Users = append(result.Users, s.newUserMetricPayload(uid, metrics))
	}

	return &mcp.CallToolResult{}, result, nil
}

func (s *Server) newUserMetricPayload(uid int, sample *resmanmetrics.UserMetrics) UserMetric {
	result := newUserMetric(uid, sample, s.stateManager.GetUserLimitState(uid, sample.Username))

	if info, err := s.cgroupManager.GetCgroupInfo(uid); err == nil {
		current, hasCurrent, max, high := extractCgroupMemoryMetrics(info)
		if hasCurrent {
			result.CgroupMemoryCurrentBytes = current
		}
		result.MemoryMax = max
		result.MemoryHigh = high
	}
	if highEvents, err := s.cgroupManager.GetMemoryHighEvents(uid); err == nil {
		result.MemoryHighEvents = highEvents
	}
	if ioRead, ioWrite, ioROps, ioWOps, err := s.cgroupManager.GetIOStats(uid); err == nil {
		result.IOReadBytes = ioRead
		result.IOWriteBytes = ioWrite
		result.IOReadOps = ioROps
		result.IOWriteOps = ioWOps
	}

	return result
}

// handleGetCgroupInfo handles get_cgroup_info tool requests
func (s *Server) handleGetCgroupInfo(ctx context.Context, req *mcp.CallToolRequest, args GetCgroupInfoArgs) (*mcp.CallToolResult, GetCgroupInfoResult, error) {
	if args.UID == 0 {
		return &mcp.CallToolResult{}, GetCgroupInfoResult{}, fmt.Errorf("uid is required")
	}

	info, err := s.cgroupManager.GetCgroupInfo(args.UID)
	if err != nil {
		return &mcp.CallToolResult{}, GetCgroupInfoResult{}, fmt.Errorf("failed to get cgroup info: %w", err)
	}

	result := newCgroupInfoResult(info)

	// Read IO limits from cgroup.
	if cgroupPath := info.Path; cgroupPath != "" {
		if data, err := os.ReadFile(cgroupPath + "/io.max"); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "default ") || strings.Contains(line, "rbps=") {
					fields := strings.Fields(line)
					for _, f := range fields {
						switch {
						case strings.HasPrefix(f, "rbps="):
							result.IOReadBPS = strings.TrimPrefix(f, "rbps=")
						case strings.HasPrefix(f, "wbps="):
							result.IOWriteBPS = strings.TrimPrefix(f, "wbps=")
						case strings.HasPrefix(f, "riops="):
							result.IOReadIOPS = strings.TrimPrefix(f, "riops=")
						case strings.HasPrefix(f, "wiops="):
							result.IOWriteIOPS = strings.TrimPrefix(f, "wiops=")
						}
					}
					break
				}
			}
		}
	}

	return &mcp.CallToolResult{}, result, nil
}

// handleGetControlHistory handles get_control_history tool requests
func (s *Server) handleGetControlHistory(ctx context.Context, req *mcp.CallToolRequest, args GetControlHistoryArgs) (*mcp.CallToolResult, GetControlHistoryResult, error) {
	if args.Limit <= 0 {
		args.Limit = 10
	}

	history := s.stateManager.GetControlHistory(args.Limit)
	result := GetControlHistoryResult{
		Entries: make([]ControlHistoryEntry, 0, len(history)),
	}

	for _, entry := range history {
		result.Entries = append(result.Entries, ControlHistoryEntry{
			Timestamp:                    entry.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
			Decision:                     entry.Decision,
			Reason:                       entry.Reason,
			TotalCPUUsage:                entry.TotalCPUUsage,
			CPUEligibleCPUUsage:          entry.CPUEligibleCPUUsage,
			ObservedUsersCount:           entry.ObservedUsersCount,
			CPUActivelyLimitedUsersCount: entry.CPUActivelyLimitedUsersCount,
			CPULimitsActive:              entry.CPULimitsActive,
			DurationMs:                   entry.DurationMs,
		})
	}

	return &mcp.CallToolResult{}, result, nil
}

// handleActivateLimits handles activate_limits tool requests
func (s *Server) handleActivateLimits(ctx context.Context, req *mcp.CallToolRequest, args ActivateLimitsArgs) (*mcp.CallToolResult, limitActionResult, error) {
	if !s.cfg.AllowWriteOps {
		return &mcp.CallToolResult{}, limitActionResult{Success: false, Message: "write operations are not allowed"}, nil
	}

	var err error
	if args.Force {
		err = s.stateManager.ForceActivateLimits()
	} else {
		status := s.stateManager.GetStatus()
		if status.CPULimitsActive {
			return &mcp.CallToolResult{}, limitActionResult{
				Success: false,
				Message: "Limits are already active",
			}, nil
		}
		err = s.stateManager.RunControlCycle(ctx)
	}

	limitsActive := s.stateManager.GetStatus().CPULimitsActive
	return &mcp.CallToolResult{}, activationResult(args.Force, limitsActive, err), nil
}

// handleGetUserHistory handles get_user_history tool requests
func (s *Server) handleGetUserHistory(ctx context.Context, req *mcp.CallToolRequest, args GetHistoryArgs) (*mcp.CallToolResult, getUserHistoryResult, error) {
	if s.dbManager == nil {
		return nil, getUserHistoryResult{}, fmt.Errorf("metrics database is not enabled")
	}

	now := time.Now()
	startTime, endTime, err := resolveHistoryTimeRange(args, now)
	if err != nil {
		return nil, getUserHistoryResult{}, err
	}
	if args.Hours > 0 {
		startTime = now.Add(-time.Duration(args.Hours) * time.Hour)
	}

	limit := normalizeHistoryLimit(args.Limit)

	uid, err := s.resolveHistoricalUID(args, startTime, endTime)
	if err != nil {
		return nil, getUserHistoryResult{}, err
	}

	// Query database
	records, err := s.dbManager.GetUserHistory(uid, startTime, endTime, limit)
	if err != nil {
		return nil, getUserHistoryResult{}, err
	}

	resultRecords := make([]userHistoryRecord, len(records))
	for i, r := range records {
		resultRecords[i] = newUserHistoryRecord(r)
	}

	result := getUserHistoryResult{
		Records:   resultRecords,
		Count:     len(resultRecords),
		StartTime: startTime.Format(time.RFC3339),
		EndTime:   endTime.Format(time.RFC3339),
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: toJSON(result)},
		},
		StructuredContent: result,
	}, result, nil
}

// handleGetSystemHistory handles get_system_history tool requests
func (s *Server) handleGetSystemHistory(ctx context.Context, req *mcp.CallToolRequest, args GetHistoryArgs) (*mcp.CallToolResult, getSystemHistoryResult, error) {
	if s.dbManager == nil {
		return nil, getSystemHistoryResult{}, fmt.Errorf("metrics database is not enabled")
	}

	now := time.Now()
	startTime, endTime, err := resolveHistoryTimeRange(args, now)
	if err != nil {
		return nil, getSystemHistoryResult{}, err
	}
	if args.Hours > 0 {
		startTime = now.Add(-time.Duration(args.Hours) * time.Hour)
	}

	limit := normalizeHistoryLimit(args.Limit)

	// Query database
	records, err := s.dbManager.GetSystemHistory(startTime, endTime, limit)
	if err != nil {
		return nil, getSystemHistoryResult{}, err
	}

	resultRecords := make([]systemHistoryRecord, len(records))
	for i, r := range records {
		resultRecords[i] = newSystemHistoryRecord(r)
	}

	result := getSystemHistoryResult{
		Records:   resultRecords,
		Count:     len(resultRecords),
		StartTime: startTime.Format(time.RFC3339),
		EndTime:   endTime.Format(time.RFC3339),
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: toJSON(result)},
		},
		StructuredContent: result,
	}, result, nil
}

// handleGetUserSummary handles get_user_summary tool requests
func (s *Server) handleGetUserSummary(ctx context.Context, req *mcp.CallToolRequest, args GetHistoryArgs) (*mcp.CallToolResult, GetUserSummaryResult, error) {
	if s.dbManager == nil {
		return nil, GetUserSummaryResult{}, fmt.Errorf("metrics database is not enabled")
	}

	startTime, endTime, err := resolveHistoryTimeRange(args, time.Now())
	if err != nil {
		return &mcp.CallToolResult{}, GetUserSummaryResult{}, err
	}

	uid, err := s.resolveHistoricalUID(args, startTime, endTime)
	if err != nil {
		return &mcp.CallToolResult{}, GetUserSummaryResult{}, err
	}

	// Query database
	summary, err := s.dbManager.GetUserSummary(uid, startTime, endTime)
	if err != nil {
		return &mcp.CallToolResult{}, GetUserSummaryResult{}, err
	}

	if summary == nil {
		return &mcp.CallToolResult{}, GetUserSummaryResult{}, fmt.Errorf("no data found for user %d in specified time range", uid)
	}

	result := GetUserSummaryResult{
		UID:                       summary.UID,
		Username:                  summary.Username,
		PeriodStart:               summary.PeriodStart,
		PeriodEnd:                 summary.PeriodEnd,
		CPUAvg:                    summary.CPUAvg,
		CPUMin:                    summary.CPUMin,
		CPUMax:                    summary.CPUMax,
		MemoryAvg:                 summary.MemoryAvg,
		MemoryMin:                 summary.MemoryMin,
		MemoryMax:                 summary.MemoryMax,
		ProcessCountAvg:           summary.ProcessCountAvg,
		CPULimitActiveTimePercent: summary.CPULimitActiveTimePercent,
		Samples:                   summary.Samples,
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: toJSON(result)},
		},
		StructuredContent: result,
	}, result, nil
}

// handleGetMetricsDatabaseInfo handles get_metrics_database_info tool requests
func (s *Server) handleGetMetricsDatabaseInfo(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, GetMetricsDatabaseInfoResult, error) {
	if s.dbManager == nil {
		return nil, GetMetricsDatabaseInfoResult{}, fmt.Errorf("metrics database is not enabled")
	}

	retention := s.effectiveMetricsDBRetentionDays()

	// Query database info
	info, err := s.dbManager.GetDatabaseInfo(retention)
	if err != nil {
		return nil, GetMetricsDatabaseInfoResult{}, err
	}

	result := GetMetricsDatabaseInfoResult{
		Path:               info.Path,
		SizeMB:             info.SizeMB,
		UserMetricsCount:   info.UserMetricsCount,
		SystemMetricsCount: info.SystemMetricsCount,
		OldestRecord:       info.OldestRecord,
		NewestRecord:       info.NewestRecord,
		RetentionDays:      info.RetentionDays,
		UsersTracked:       info.UsersTracked,
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: toJSON(result)},
		},
		StructuredContent: result,
	}, result, nil
}

func (s *Server) effectiveMetricsDBRetentionDays() int {
	const defaultRetentionDays = 30
	if s.stateManager == nil {
		return defaultRetentionDays
	}
	cfg := s.stateManager.GetConfig()
	if cfg == nil || cfg.MetricsDBRetentionDays <= 0 {
		return defaultRetentionDays
	}
	return cfg.MetricsDBRetentionDays
}

// Helper functions

func resolveHistoryTimeRange(args GetHistoryArgs, now time.Time) (time.Time, time.Time, error) {
	startTime, endTime, err := database.ParseTimeRange(args.Period, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid period %q: %w", args.Period, err)
	}

	if args.StartTime != "" {
		startTime, err = time.Parse(time.RFC3339, args.StartTime)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid startTime %q: expected RFC3339: %w", args.StartTime, err)
		}
	}
	if args.EndTime != "" {
		endTime, err = time.Parse(time.RFC3339, args.EndTime)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid endTime %q: expected RFC3339: %w", args.EndTime, err)
		}
	}

	return startTime, endTime, nil
}

func normalizeHistoryLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultHistoryLimit
	case limit > maxHistoryLimit:
		return maxHistoryLimit
	default:
		return limit
	}
}

func totalCPUCapacityPercent(metrics resmanmetrics.ObservationMetrics) float64 {
	return float64(metrics.TotalCores) * 100
}

func totalSystemMemoryMB(metrics resmanmetrics.ObservationMetrics) float64 {
	return metrics.TotalMemoryMB
}

func activationResult(force, limitsActive bool, err error) limitActionResult {
	if err != nil {
		return limitActionResult{
			Success: false,
			Message: "Failed to activate limits: " + err.Error(),
		}
	}
	if limitsActive {
		return limitActionResult{
			Success: true,
			Message: "Limits activated successfully",
		}
	}
	if force {
		return limitActionResult{
			Success: false,
			Message: "Forced activation completed without error, but limits are not active",
		}
	}
	return limitActionResult{
		Success: false,
		Message: "Control cycle completed, but limits were not activated because activation conditions were not met",
	}
}

func (s *Server) resolveHistoricalUID(args GetHistoryArgs, startTime, endTime time.Time) (int, error) {
	if args.UID != nil {
		if *args.UID <= 0 {
			return 0, fmt.Errorf("uid must be greater than zero")
		}
		return *args.UID, nil
	}
	if args.Username == "" {
		return 0, fmt.Errorf("either uid or username must be provided")
	}

	uid, found, err := s.dbManager.ResolveUserUID(args.Username, startTime, endTime)
	if err != nil {
		return 0, err
	}
	if found {
		return uid, nil
	}

	if s.stateManager != nil {
		uid = s.stateManager.GetUIDFromUsername(args.Username)
		if uid != 0 {
			return uid, nil
		}
	}
	return 0, fmt.Errorf("user not found: %s", args.Username)
}

// toJSON converts a value to JSON string
func toJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("{\"error\": \"failed to marshal: %v\"}", err)
	}
	return string(b)
}

// joinStrings joins a slice of strings with the given separator
func joinStrings(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for i := 1; i < len(strs); i++ {
		result += sep + strs[i]
	}
	return result
}
