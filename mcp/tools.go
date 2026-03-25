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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fdefilippo/resman/database"
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

type GetSystemStatusArgs struct{}

type GetSystemStatusResult struct {
	TotalCPUUsage       float64 `json:"total_cpu_usage"`
	UserCPUUsage        float64 `json:"user_cpu_usage"`
	MemoryUsageMB       float64 `json:"memory_usage_mb"`
	ActiveUsersCount    int     `json:"active_users_count"`
	TotalCores          int     `json:"total_cores"`
	SystemUnderLoad     bool    `json:"system_under_load"`
	LimitsActive        bool    `json:"limits_active"`
	LimitsAppliedTime   string  `json:"limits_applied_time"`
	SharedCgroupActive  bool    `json:"shared_cgroup_active"`
}

type GetUserMetricsArgs struct {
	UIDs     []int  `json:"uids,omitempty"`
	Username string `json:"username,omitempty"`
}

type UserMetric struct {
	UID          int    `json:"uid"`
	Username     string `json:"username"`
	CPUUsage     float64 `json:"cpu_usage"`
	MemoryUsage  uint64  `json:"memory_usage"`
	ProcessCount int     `json:"process_count"`
        IsLimited    bool   `json:"is_limited"`
}

type GetUserMetricsResult struct {
	Users []UserMetric `json:"users"`
}

type GetLimitsStatusArgs struct{}

type GetLimitsStatusResult struct {
	LimitsActive           bool   `json:"limits_active"`
	LimitsAppliedTime      string `json:"limits_applied_time"`
	ActiveUsersCount       int    `json:"active_users_count"`
	ActiveUsers            []int  `json:"active_users"`
	SharedCgroupPath       string `json:"shared_cgroup_path"`
	SharedCgroupActive     bool   `json:"shared_cgroup_active"`
	SharedCgroupQuota      string `json:"shared_cgroup_quota"`
	SharedCgroupUserCount  int    `json:"shared_cgroup_user_count"`
}

type GetCgroupInfoArgs struct {
	UID int `json:"uid"`
}

type GetCgroupInfoResult struct {
	Path    string `json:"path"`
	CPUQuota string `json:"cpu_max"`
	Weight  string `json:"cpu_weight"`
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

type GetHistoryResult struct {
	Records   []map[string]any `json:"records"`
	Count     int              `json:"count"`
	StartTime string           `json:"start_time"`
	EndTime   string           `json:"end_time"`
}

type GetUserSummaryResult struct {
	UID          int     `json:"uid"`
	Username     string  `json:"username"`
	PeriodStart  string  `json:"period_start"`
	PeriodEnd    string  `json:"period_end"`
	CPUAvg       float64 `json:"cpu_avg"`
	CPUMin       float64 `json:"cpu_min"`
	CPUMax       float64 `json:"cpu_max"`
	MemoryAvg    float64 `json:"memory_avg"`
	MemoryMin    float64 `json:"memory_min"`
	MemoryMax    float64 `json:"memory_max"`
	ProcessCountAvg float64 `json:"process_count_avg"`
	LimitedTimePercent float64 `json:"limited_time_percent"`
	Samples      int     `json:"samples"`
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

type GetConfigurationArgs struct{}

type GetConfigurationResult struct {
	CPUThreshold        int    `json:"cpu_threshold"`
	CPUReleaseThreshold int    `json:"cpu_release_threshold"`
	PollingInterval     int    `json:"polling_interval"`
	MinSystemCores      int    `json:"min_system_cores"`
	CPUQuotaNormal      string `json:"cpu_quota_normal"`
	CPUQuotaLimited     string `json:"cpu_quota_limited"`
	EnablePrometheus    bool   `json:"enable_prometheus"`
	PrometheusMetricsBindPort      int    `json:"prometheus_port"`
	IgnoreSystemLoad    bool   `json:"ignore_system_load"`
	SystemUIDMin        int    `json:"system_uid_min"`
	SystemUIDMax        int    `json:"system_uid_max"`
}

type GetControlHistoryArgs struct {
	Limit int `json:"limit"`
}

type ControlHistoryEntry struct {
	Timestamp     string  `json:"timestamp"`
	Decision      string  `json:"decision"`
	Reason        string  `json:"reason"`
	TotalCPUUsage float64 `json:"total_cpu_usage"`
	UserCPUUsage  float64 `json:"user_cpu_usage"`
	ActiveUsers   int     `json:"active_users"`
	LimitsActive  bool    `json:"limits_active"`
	DurationMs    int64   `json:"duration_ms"`
}

type GetControlHistoryResult struct {
	Entries []ControlHistoryEntry `json:"entries"`
}

type ActivateLimitsArgs struct {
	Force bool `json:"force"`
}

type ActivateLimitsResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type DeactivateLimitsArgs struct{}

type DeactivateLimitsResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
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
		metrics := s.metricsCollector.GetDetailedMetrics()
		hostname := getHostname()
		serverRole := s.stateManager.GetConfig().ServerRole

		result := map[string]any{
			"hostname":              hostname,
			"server_role":           serverRole,
			"total_cpu_usage":       getFloatMetric(metrics, "total_cpu_usage", 0.0),
			"user_cpu_usage":        getFloatMetric(metrics, "total_user_cpu_usage", 0.0),
			"memory_usage_mb":       getFloatMetric(metrics, "memory_usage_mb", 0.0),
			"active_users_count":    getIntMetric(metrics, "active_users_count", 0),
			"total_cores":           getIntMetric(metrics, "total_cores", 0),
			"system_under_load":     getBoolMetric(metrics, "system_under_load", false),
			"limits_active":         getBool(status, "limits_active", false),
			"limits_applied_time":   getString(status, "limits_applied_time", ""),
			"shared_cgroup_active":  getBool(status, "shared_cgroup_active", false),
		}

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
		hostname := getHostname()
		serverRole := s.stateManager.GetConfig().ServerRole

		users := make([]map[string]any, 0, len(activeUsers))
		for _, uid := range activeUsers {
			username := fmt.Sprintf("%d", uid)
			if metrics, ok := allMetrics[uid]; ok && metrics.Username != "" {
				username = metrics.Username
			}
			users = append(users, map[string]any{
				"uid":      uid,
				"username": username,
			})
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(map[string]any{"hostname": hostname, "server_role": serverRole, "users": users})},
			},
			StructuredContent: map[string]any{"hostname": hostname, "server_role": serverRole, "users": users},
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

		result := map[string]any{
			"hostname":               hostname,
			"server_role":            serverRole,
			"limits_active":          getBool(status, "limits_active", false),
			"limits_applied_time":    getString(status, "limits_applied_time", ""),
			"active_users_count":     getInt(status, "active_users_count", 0),
			"active_users":           getIntSlice(status, "active_users", []int{}),
			"shared_cgroup_path":     getString(status, "shared_cgroup_path", ""),
			"shared_cgroup_active":   getBool(status, "shared_cgroup_active", false),
			"shared_cgroup_quota":    getString(status, "shared_cgroup_quota", ""),
			"shared_cgroup_user_count": getInt(status, "shared_cgroup_user_count", 0),
		}

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
		Description: "Get current CPU Manager configuration",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cfg := s.stateManager.GetConfig()
		hostname := getHostname()

		result := map[string]any{
			"hostname":              hostname,
			"server_role":           cfg.ServerRole,
			"cpu_threshold":         cfg.CPUThreshold,
			"cpu_release_threshold": cfg.CPUReleaseThreshold,
			"polling_interval":      cfg.PollingInterval,
			"min_system_cores":      cfg.MinSystemCores,
			"cpu_quota_normal":      cfg.CPUQuotaNormal,
			"cpu_quota_limited":     cfg.CPUQuotaLimited,
			"enable_prometheus":     cfg.EnablePrometheus,
			"prometheus_port":       cfg.PrometheusMetricsBindPort,
			"ignore_system_load":    cfg.IgnoreSystemLoad,
			"system_uid_min":        cfg.SystemUIDMin,
			"system_uid_max":        cfg.SystemUIDMax,
		}

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
		metrics := s.metricsCollector.GetDetailedMetrics()
		status := s.stateManager.GetStatus()
		allUserMetrics := s.metricsCollector.GetAllUserMetrics()
		hostname := getHostname()
		serverRole := s.stateManager.GetConfig().ServerRole

		// Build user list with details
		var users []string
		var totalCPU, peakCPU float64
		limitedCount := 0

		for uid, userMetrics := range allUserMetrics {
			isLimited := false
			if activeUsers, ok := status["active_users"].([]int); ok {
				for _, activeUID := range activeUsers {
					if activeUID == uid {
						isLimited = true
						limitedCount++
						break
					}
				}
			}

			limitStatus := "Non attivi"
			if isLimited {
				limitStatus = "Attivi"
			}

			userLine := fmt.Sprintf("%s\n    Utilizzo CPU: %.1f%%\n    Limiti: %s",
				userMetrics.Username,
				userMetrics.CPUUsage,
				limitStatus,
			)
			users = append(users, userLine)

			if userMetrics.CPUUsage > totalCPU {
				totalCPU = userMetrics.CPUUsage
			}
			if userMetrics.CPUUsage > peakCPU {
				peakCPU = userMetrics.CPUUsage
			}
		}

		// Calculate average CPU usage
		avgCPU := 0.0
		if len(allUserMetrics) > 0 {
			for _, m := range allUserMetrics {
				avgCPU += m.CPUUsage
			}
			avgCPU /= float64(len(allUserMetrics))
		}

		// Get limits active time
		limitsActive := getBool(status, "limits_active", false)
		limitsStatus := "Non attivi"
		if limitsActive {
			limitsStatus = "Attivi"
		}

		// Build report text
		report := fmt.Sprintf(`Report Utilizzo CPU
Hostname: %s
Server Role: %s
Data: %s
Totale CPU disponibile: %.1f%%
Utilizzo attuale: %.1f%%

Utenti Attivi:
%s

Stato delle Risorse:
Media Utilizzo CPU: %.1f%%
Picco Utilizzo CPU: %.1f%%
Limiti CPU: %s
Utenti limitati: %d su %d
`,
			hostname,
			serverRole,
			time.Now().Format("2006-01-02 15:04:05"),
			getFloatMetric(metrics, "total_cores", 0.0)*100,
			getFloatMetric(metrics, "total_cpu_usage", 0.0),
			joinStrings(users, "\n"),
			avgCPU,
			peakCPU,
			limitsStatus,
			limitedCount,
			len(allUserMetrics),
		)

		result := map[string]any{
			"hostname":      hostname,
			"server_role":   serverRole,
			"report":        report,
			"total_cpu":     getFloatMetric(metrics, "total_cpu_usage", 0.0),
			"avg_cpu":       avgCPU,
			"peak_cpu":      peakCPU,
			"active_users":  len(allUserMetrics),
			"limited_users": limitedCount,
			"limits_active": limitsActive,
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
		metrics := s.metricsCollector.GetDetailedMetrics()
		status := s.stateManager.GetStatus()
		allUserMetrics := s.metricsCollector.GetAllUserMetrics()
		hostname := getHostname()
		serverRole := s.stateManager.GetConfig().ServerRole

		// Build user list with memory details
		var users []string
		var totalMem, peakMem uint64
		limitedCount := 0

		for uid, userMetrics := range allUserMetrics {
			isLimited := false
			if activeUsers, ok := status["active_users"].([]int); ok {
				for _, activeUID := range activeUsers {
					if activeUID == uid {
						isLimited = true
						limitedCount++
						break
					}
				}
			}

			limitStatus := "Non attivi"
			if isLimited {
				limitStatus = "Attivi"
			}

			// Convert bytes to MB for readability
			memMB := float64(userMetrics.MemoryUsage) / 1024 / 1024

			userLine := fmt.Sprintf("%s\n    Memoria: %.1f MB (%d bytes)\n    Processi: %d\n    Limiti: %s",
				userMetrics.Username,
				memMB,
				userMetrics.MemoryUsage,
				userMetrics.ProcessCount,
				limitStatus,
			)
			users = append(users, userLine)

			if userMetrics.MemoryUsage > totalMem {
				totalMem = userMetrics.MemoryUsage
			}
			if userMetrics.MemoryUsage > peakMem {
				peakMem = userMetrics.MemoryUsage
			}
		}

		// Calculate average memory usage
		avgMem := uint64(0)
		if len(allUserMetrics) > 0 {
			for _, m := range allUserMetrics {
				avgMem += m.MemoryUsage
			}
			avgMem /= uint64(len(allUserMetrics))
		}

		// Get system memory info
		totalMemMB := getFloatMetric(metrics, "memory_usage_mb", 0.0)

		// Get limits status
		limitsActive := getBool(status, "limits_active", false)
		limitsStatus := "Non attivi"
		if limitsActive {
			limitsStatus = "Attivi"
		}

		// Build report text
		report := fmt.Sprintf(`Report Utilizzo Memoria
Hostname: %s
Server Role: %s
Data: %s
Memoria Totale di Sistema: %.1f MB

Utenti Attivi:
%s

Stato delle Risorse:
Media Utilizzo Memoria: %.1f MB
Picco Utilizzo Memoria: %.1f MB
Limiti CPU: %s
Utenti limitati: %d su %d
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

		result := map[string]any{
			"hostname":        hostname,
			"server_role":     serverRole,
			"report":          report,
			"total_memory_mb": totalMemMB,
			"avg_memory_mb":   float64(avgMem) / 1024 / 1024,
			"peak_memory_mb":  float64(peakMem) / 1024 / 1024,
			"active_users":    len(allUserMetrics),
			"limited_users":   limitedCount,
			"limits_active":   limitsActive,
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

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: toJSON(map[string]any{
						"success": success,
						"message": message,
					})},
				},
				StructuredContent: map[string]any{
					"success": success,
					"message": message,
				},
			}, nil
		})
	}

	// set_user_exclude_list - registered manually with explicit schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "set_user_exclude_list",
		Description: "Set the list of users to exclude from CPU limits (regex patterns supported)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"patterns": map[string]any{
					"type": "array",
					"items": map[string]any{"type": "string"},
					"description": "List of regex patterns for users to exclude",
				},
				"reload": map[string]any{
					"type": "boolean",
					"description": "Automatically reload configuration after change",
					"default": true,
				},
			},
			"required": []string{"patterns"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Unmarshal arguments from json.RawMessage
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return &mcp.CallToolResult{}, fmt.Errorf("invalid parameters: %w", err)
		}
		
		// Extract patterns
		patternsRaw, ok := args["patterns"].([]interface{})
		if !ok {
			return &mcp.CallToolResult{}, fmt.Errorf("invalid patterns parameter")
		}
		
		// Convert []interface{} to []string
		patterns := make([]string, len(patternsRaw))
		for i, p := range patternsRaw {
			if s, ok := p.(string); ok {
				patterns[i] = s
			} else {
				return &mcp.CallToolResult{}, fmt.Errorf("pattern must be string")
			}
		}
		
		// Get reload parameter (default true)
		reload := true
		if reloadRaw, ok := args["reload"].(bool); ok {
			reload = reloadRaw
		}
		
		// Check if write operations are allowed
		if !s.cfg.AllowWriteOps {
			return &mcp.CallToolResult{}, fmt.Errorf("write operations not allowed. Set MCP_ALLOW_WRITE_OPS=true")
		}
		
		// Get current config
		cfg := s.stateManager.GetConfig()
		
		// Save previous value
		previousValue := make([]string, len(cfg.UserExcludeList))
		copy(previousValue, cfg.UserExcludeList)
		
		// Set new exclude list
		_, err := cfg.SetUserExcludeList(patterns, cfg.ConfigFile, reload)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: toJSON(map[string]any{
						"success": false,
						"error": err.Error(),
						"previous_value": previousValue,
					})},
				},
			}, nil
		}
		
		// Trigger reload if requested
		if reload {
			time.Sleep(1 * time.Second)
		}
		
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(map[string]any{
					"success": true,
					"message": "User exclude list updated successfully",
					"previous_value": previousValue,
					"new_value": patterns,
					"reload_triggered": reload,
				})},
			},
			StructuredContent: map[string]any{
				"success": true,
				"previous_value": previousValue,
				"new_value": patterns,
				"reload_triggered": reload,
			},
		}, nil
	})

	// set_user_include_list - registered manually with explicit schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "set_user_include_list",
		Description: "Set the list of users to include in monitoring (regex patterns supported)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"patterns": map[string]any{
					"type": "array",
					"items": map[string]any{"type": "string"},
					"description": "List of regex patterns for users to include",
				},
				"reload": map[string]any{
					"type": "boolean",
					"description": "Automatically reload configuration after change",
					"default": true,
				},
			},
			"required": []string{"patterns"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Unmarshal arguments from json.RawMessage
		var args map[string]interface{}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return &mcp.CallToolResult{}, fmt.Errorf("invalid parameters: %w", err)
		}
		
		// Extract patterns
		patternsRaw, ok := args["patterns"].([]interface{})
		if !ok {
			return &mcp.CallToolResult{}, fmt.Errorf("invalid patterns parameter")
		}
		
		// Convert []interface{} to []string
		patterns := make([]string, len(patternsRaw))
		for i, p := range patternsRaw {
			if s, ok := p.(string); ok {
				patterns[i] = s
			} else {
				return &mcp.CallToolResult{}, fmt.Errorf("pattern must be string")
			}
		}
		
		// Get reload parameter (default true)
		reload := true
		if reloadRaw, ok := args["reload"].(bool); ok {
			reload = reloadRaw
		}
		
		// Check if write operations are allowed
		if !s.cfg.AllowWriteOps {
			return &mcp.CallToolResult{}, fmt.Errorf("write operations not allowed. Set MCP_ALLOW_WRITE_OPS=true")
		}
		
		// Get current config
		cfg := s.stateManager.GetConfig()
		
		// Save previous value
		previousValue := make([]string, len(cfg.UserIncludeList))
		copy(previousValue, cfg.UserIncludeList)
		
		// Set new include list
		_, err := cfg.SetUserIncludeList(patterns, cfg.ConfigFile, reload)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: toJSON(map[string]any{
						"success": false,
						"error": err.Error(),
						"previous_value": previousValue,
					})},
				},
			}, nil
		}
		
		// Trigger reload if requested
		if reload {
			time.Sleep(1 * time.Second)
		}
		
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(map[string]any{
					"success": true,
					"message": "User include list updated successfully",
					"previous_value": previousValue,
					"new_value": patterns,
					"reload_triggered": reload,
				})},
			},
			StructuredContent: map[string]any{
				"success": true,
				"previous_value": previousValue,
				"new_value": patterns,
				"reload_triggered": reload,
			},
		}, nil
	})

	// get_user_filters - registered manually with explicit empty schema
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_user_filters",
		Description: "Get current user include and exclude filter configurations",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cfg := s.stateManager.GetConfig()
		
		result := map[string]any{
			"user_include_list": cfg.UserIncludeList,
			"user_exclude_list": cfg.UserExcludeList,
			"config_file": cfg.ConfigFile,
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
					"type": "string",
					"description": "Regex pattern to validate",
				},
				"type": map[string]any{
					"type": "string",
					"description": "Filter type: 'include' or 'exclude'",
					"enum": []string{"include", "exclude"},
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
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: toJSON(map[string]any{
						"valid": false,
						"error": err.Error(),
					})},
				},
				StructuredContent: map[string]any{
					"valid": false,
					"error": err.Error(),
				},
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
		
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: toJSON(map[string]any{
					"valid": true,
					"pattern": pattern,
					"type": filterType,
					"test_matches": matches,
					"match_count": len(matches),
				})},
			},
			StructuredContent: map[string]any{
				"valid": true,
				"pattern": pattern,
				"type": filterType,
				"test_matches": matches,
				"match_count": len(matches),
			},
		}, nil
	})

	// get_user_history - Get historical metrics for a specific user
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_user_history",
		Description: "Get historical CPU and memory metrics for a specific user. Supports time ranges via startTime/endTime, period (today, yesterday, last_24_hours, last_7_days, last_30_days), or hours parameter",
	}, s.handleGetUserHistory)

	// get_system_history - Get historical system metrics
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_system_history",
		Description: "Get historical system-wide CPU and memory metrics. Supports time ranges via startTime/endTime, period (today, yesterday, last_24_hours, last_7_days, last_30_days), or hours parameter",
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

// handleGetSystemStatus handles get_system_status tool requests
func (s *Server) handleGetSystemStatus(ctx context.Context, req *mcp.CallToolRequest, args GetSystemStatusArgs) (*mcp.CallToolResult, GetSystemStatusResult, error) {
	status := s.stateManager.GetStatus()
	metrics := s.metricsCollector.GetDetailedMetrics()

	result := GetSystemStatusResult{
		TotalCPUUsage:      getFloatMetric(metrics, "total_cpu_usage", 0.0),
		UserCPUUsage:       getFloatMetric(metrics, "total_user_cpu_usage", 0.0),
		MemoryUsageMB:      getFloatMetric(metrics, "memory_usage_mb", 0.0),
		ActiveUsersCount:   getIntMetric(metrics, "active_users_count", 0),
		TotalCores:         getIntMetric(metrics, "total_cores", 0),
		SystemUnderLoad:    getBoolMetric(metrics, "system_under_load", false),
		LimitsActive:       getBool(status, "limits_active", false),
		LimitsAppliedTime:  getString(status, "limits_applied_time", ""),
		SharedCgroupActive: getBool(status, "shared_cgroup_active", false),
	}

	return &mcp.CallToolResult{}, result, nil
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

		result.Users = append(result.Users, UserMetric{
			UID:          uid,
			Username:     metrics.Username,
			CPUUsage:     metrics.CPUUsage,
			MemoryUsage:  metrics.MemoryUsage,
			ProcessCount: metrics.ProcessCount,
                        IsLimited:    metrics.IsLimited,
		})
	}

	return &mcp.CallToolResult{}, result, nil
}

// handleGetLimitsStatus handles get_limits_status tool requests
func (s *Server) handleGetLimitsStatus(ctx context.Context, req *mcp.CallToolRequest, args GetLimitsStatusArgs) (*mcp.CallToolResult, GetLimitsStatusResult, error) {
	status := s.stateManager.GetStatus()

	result := GetLimitsStatusResult{
		LimitsActive:          getBool(status, "limits_active", false),
		LimitsAppliedTime:     getString(status, "limits_applied_time", ""),
		ActiveUsersCount:      getInt(status, "active_users_count", 0),
		ActiveUsers:           getIntSlice(status, "active_users", []int{}),
		SharedCgroupPath:      getString(status, "shared_cgroup_path", ""),
		SharedCgroupActive:    getBool(status, "shared_cgroup_active", false),
		SharedCgroupQuota:     getString(status, "shared_cgroup_quota", ""),
		SharedCgroupUserCount: getInt(status, "shared_cgroup_user_count", 0),
	}

	return &mcp.CallToolResult{}, result, nil
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

	result := GetCgroupInfoResult{
		Path:     info["path"],
		CPUQuota: info["cpu.max"],
		Weight:   info["cpu.weight"],
	}

	return &mcp.CallToolResult{}, result, nil
}

// handleGetConfiguration handles get_configuration tool requests
func (s *Server) handleGetConfiguration(ctx context.Context, req *mcp.CallToolRequest, args GetConfigurationArgs) (*mcp.CallToolResult, GetConfigurationResult, error) {
	cfg := s.stateManager.GetConfig()

	result := GetConfigurationResult{
		CPUThreshold:        cfg.CPUThreshold,
		CPUReleaseThreshold: cfg.CPUReleaseThreshold,
		PollingInterval:     cfg.PollingInterval,
		MinSystemCores:      cfg.MinSystemCores,
		CPUQuotaNormal:      cfg.CPUQuotaNormal,
		CPUQuotaLimited:     cfg.CPUQuotaLimited,
		EnablePrometheus:    cfg.EnablePrometheus,
		PrometheusMetricsBindPort:      cfg.PrometheusMetricsBindPort,
		IgnoreSystemLoad:    cfg.IgnoreSystemLoad,
		SystemUIDMin:        cfg.SystemUIDMin,
		SystemUIDMax:        cfg.SystemUIDMax,
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
			Timestamp:     entry.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
			Decision:      entry.Decision,
			Reason:        entry.Reason,
			TotalCPUUsage: entry.TotalCPUUsage,
			UserCPUUsage:  entry.UserCPUUsage,
			ActiveUsers:   entry.ActiveUsers,
			LimitsActive:  entry.LimitsActive,
			DurationMs:    entry.DurationMs,
		})
	}

	return &mcp.CallToolResult{}, result, nil
}

// handleActivateLimits handles activate_limits tool requests
func (s *Server) handleActivateLimits(ctx context.Context, req *mcp.CallToolRequest, args ActivateLimitsArgs) (*mcp.CallToolResult, ActivateLimitsResult, error) {
	if !s.cfg.AllowWriteOps {
		return &mcp.CallToolResult{}, ActivateLimitsResult{Success: false, Message: "write operations are not allowed"}, nil
	}

	var err error
	if args.Force {
		err = s.stateManager.ForceActivateLimits()
	} else {
		status := s.stateManager.GetStatus()
		if getBool(status, "limits_active", false) {
			return &mcp.CallToolResult{}, ActivateLimitsResult{
				Success: false,
				Message: "Limits are already active",
			}, nil
		}
		err = s.stateManager.RunControlCycle(ctx)
	}

	success := err == nil
	message := "Limits activated successfully"
	if err != nil {
		message = "Failed to activate limits: " + err.Error()
	}

	return &mcp.CallToolResult{}, ActivateLimitsResult{
		Success: success,
		Message: message,
	}, nil
}

// handleDeactivateLimits handles deactivate_limits tool requests
func (s *Server) handleDeactivateLimits(ctx context.Context, req *mcp.CallToolRequest, args DeactivateLimitsArgs) (*mcp.CallToolResult, DeactivateLimitsResult, error) {
	if !s.cfg.AllowWriteOps {
		return &mcp.CallToolResult{}, DeactivateLimitsResult{Success: false, Message: "write operations are not allowed"}, nil
	}

	err := s.stateManager.ForceDeactivateLimits()

	success := err == nil
	message := "Limits deactivated successfully"
	if err != nil {
		message = "Failed to deactivate limits: " + err.Error()
	}

	return &mcp.CallToolResult{}, DeactivateLimitsResult{
		Success: success,
		Message: message,
	}, nil
}

// handleGetUserHistory handles get_user_history tool requests
func (s *Server) handleGetUserHistory(ctx context.Context, req *mcp.CallToolRequest, args GetHistoryArgs) (*mcp.CallToolResult, GetHistoryResult, error) {
	if s.dbManager == nil {
		return nil, GetHistoryResult{}, fmt.Errorf("metrics database is not enabled")
	}

	// Determine time range
	now := time.Now()
	startTime, endTime, err := database.ParseTimeRange(args.Period, now)
	if err != nil && args.Period != "" {
		return nil, GetHistoryResult{}, err
	}

	// Override with explicit startTime/endTime if provided
	if args.StartTime != "" {
		if t, err := time.Parse(time.RFC3339, args.StartTime); err == nil {
			startTime = t
		}
	}
	if args.EndTime != "" {
		if t, err := time.Parse(time.RFC3339, args.EndTime); err == nil {
			endTime = t
		}
	}

	// Handle hours parameter
	if args.Hours > 0 {
		startTime = now.Add(-time.Duration(args.Hours) * time.Hour)
	}

	// Default limit
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}

	// Get UID from username if needed
	uid := 0
	if args.UID != nil {
		uid = *args.UID
	} else if args.Username != "" {
		// Try to resolve username to UID
		uid = s.stateManager.GetUIDFromUsername(args.Username)
		if uid == 0 {
			return nil, GetHistoryResult{}, fmt.Errorf("user not found: %s", args.Username)
		}
	}

	if uid == 0 {
		return nil, GetHistoryResult{}, fmt.Errorf("either uid or username must be provided")
	}

	// Query database
	records, err := s.dbManager.GetUserHistory(uid, startTime, endTime, limit)
	if err != nil {
		return nil, GetHistoryResult{}, err
	}

	// Convert to map for JSON
	resultRecords := make([]map[string]any, len(records))
	for i, r := range records {
		resultRecords[i] = map[string]any{
			"timestamp":        r.Timestamp.Format(time.RFC3339),
			"uid":              r.UID,
			"username":         r.Username,
			"cpu_usage":        r.CPUUsagePercent,
			"memory_usage":     r.MemoryUsageBytes,
			"process_count":    r.ProcessCount,
			"cgroup_path":      r.CgroupPath,
			"cpu_quota":        r.CPUQuota,
			"is_limited":       r.IsLimited,
		}
	}

	result := GetHistoryResult{
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
func (s *Server) handleGetSystemHistory(ctx context.Context, req *mcp.CallToolRequest, args GetHistoryArgs) (*mcp.CallToolResult, GetHistoryResult, error) {
	if s.dbManager == nil {
		return nil, GetHistoryResult{}, fmt.Errorf("metrics database is not enabled")
	}

	// Determine time range
	now := time.Now()
	startTime, endTime, err := database.ParseTimeRange(args.Period, now)
	if err != nil && args.Period != "" {
		return nil, GetHistoryResult{}, err
	}

	// Override with explicit startTime/endTime if provided
	if args.StartTime != "" {
		if t, err := time.Parse(time.RFC3339, args.StartTime); err == nil {
			startTime = t
		}
	}
	if args.EndTime != "" {
		if t, err := time.Parse(time.RFC3339, args.EndTime); err == nil {
			endTime = t
		}
	}

	// Handle hours parameter
	if args.Hours > 0 {
		startTime = now.Add(-time.Duration(args.Hours) * time.Hour)
	}

	// Default limit
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}

	// Query database
	records, err := s.dbManager.GetSystemHistory(startTime, endTime, limit)
	if err != nil {
		return nil, GetHistoryResult{}, err
	}

	// Convert to map for JSON
	resultRecords := make([]map[string]any, len(records))
	for i, r := range records {
		resultRecords[i] = map[string]any{
			"timestamp":         r.Timestamp.Format(time.RFC3339),
			"total_cpu_usage":   r.TotalCPUUsagePercent,
			"total_cores":       r.TotalCores,
			"system_load":       r.SystemLoad,
			"limits_active":     r.LimitsActive,
			"limited_users":     r.LimitedUsersCount,
		}
	}

	result := GetHistoryResult{
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

	// Determine time range
	now := time.Now()
	startTime, endTime, err := database.ParseTimeRange(args.Period, now)
	if err != nil && args.Period != "" {
		return &mcp.CallToolResult{}, GetUserSummaryResult{}, err
	}

	// Override with explicit startTime/endTime if provided
	if args.StartTime != "" {
		if t, err := time.Parse(time.RFC3339, args.StartTime); err == nil {
			startTime = t
		}
	}
	if args.EndTime != "" {
		if t, err := time.Parse(time.RFC3339, args.EndTime); err == nil {
			endTime = t
		}
	}

	// Get UID from username if needed
	uid := 0
	if args.UID != nil {
		uid = *args.UID
	} else if args.Username != "" {
		uid = s.stateManager.GetUIDFromUsername(args.Username)
		if uid == 0 {
			return &mcp.CallToolResult{}, GetUserSummaryResult{}, fmt.Errorf("user not found: %s", args.Username)
		}
	}

	if uid == 0 {
		return &mcp.CallToolResult{}, GetUserSummaryResult{}, fmt.Errorf("either uid or username must be provided")
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
		UID:          summary.UID,
		Username:     summary.Username,
		PeriodStart:  summary.PeriodStart,
		PeriodEnd:    summary.PeriodEnd,
		CPUAvg:       summary.CPUAvg,
		CPUMin:       summary.CPUMin,
		CPUMax:       summary.CPUMax,
		MemoryAvg:    summary.MemoryAvg,
		MemoryMin:    summary.MemoryMin,
		MemoryMax:    summary.MemoryMax,
		ProcessCountAvg: summary.ProcessCountAvg,
		LimitedTimePercent: summary.LimitedTimePercent,
		Samples:      summary.Samples,
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

	// Get retention from config
	retention := 30
	if s.parentCfg != nil && s.parentCfg.MetricsDBRetentionDays > 0 {
		retention = s.parentCfg.MetricsDBRetentionDays
	}

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

// Helper functions

func getFloatMetric(metrics map[string]any, key string, defaultVal float64) float64 {
	if val, ok := metrics[key]; ok {
		if f, ok := val.(float64); ok {
			return f
		}
	}
	return defaultVal
}

func getIntMetric(metrics map[string]any, key string, defaultVal int) int {
	if val, ok := metrics[key]; ok {
		switch v := val.(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return defaultVal
}

func getBoolMetric(metrics map[string]any, key string, defaultVal bool) bool {
	if val, ok := metrics[key]; ok {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return defaultVal
}

func getBool(m map[string]any, key string, defaultVal bool) bool {
	if val, ok := m[key]; ok {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return defaultVal
}

func getString(m map[string]any, key string, defaultVal string) string {
	if val, ok := m[key]; ok {
		if s, ok := val.(string); ok {
			return s
		}
	}
	return defaultVal
}

func getInt(m map[string]any, key string, defaultVal int) int {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return defaultVal
}

func getIntSlice(m map[string]any, key string, defaultVal []int) []int {
	if val, ok := m[key]; ok {
		if slice, ok := val.([]int); ok {
			return slice
		}
		if slice, ok := val.([]any); ok {
			result := make([]int, 0, len(slice))
			for _, v := range slice {
				if i, ok := v.(float64); ok {
					result = append(result, int(i))
				}
			}
			return result
		}
	}
	return defaultVal
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
