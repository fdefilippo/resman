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
// mcp/resources.go
package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerResources registers all MCP resources
func (s *Server) registerResources() {
	s.mcpServer.AddResource(&mcp.Resource{
		URI:         "resman://system/status",
		Name:        "System Status",
		Description: "Real-time system CPU and Memory status",
		MIMEType:    "application/json",
	}, s.handleSystemStatusResource)

	s.mcpServer.AddResource(&mcp.Resource{
		URI:         "resman://users/active",
		Name:        "Active Users",
		Description: "List of active non-system users",
		MIMEType:    "application/json",
	}, s.handleActiveUsersResource)

	s.mcpServer.AddResource(&mcp.Resource{
		URI:         "resman://limits/status",
		Name:        "Limits Status",
		Description: "Current CPU limits status",
		MIMEType:    "application/json",
	}, s.handleLimitsStatusResource)

	s.mcpServer.AddResource(&mcp.Resource{
		URI:         "resman://config",
		Name:        "Configuration",
		Description: "Current Resource Manager configuration",
		MIMEType:    "application/json",
	}, s.handleConfigResource)

	// Template for per-user resources
	s.mcpServer.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "resman://users/{uid}/metrics",
		Name:        "User Metrics",
		Description: "Metrics for a specific user",
		MIMEType:    "application/json",
	}, s.handleUserMetricsResource)

	s.mcpServer.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "resman://cgroups/{uid}",
		Name:        "Cgroup Info",
		Description: "Cgroup information for a specific user",
		MIMEType:    "application/json",
	}, s.handleCgroupResource)
}

// handleSystemStatusResource handles resman://system/status
func (s *Server) handleSystemStatusResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	status := s.stateManager.GetStatus()
	metrics := s.metricsCollector.GetDetailedMetrics()

	result := map[string]any{
		"total_cpu_usage":      getFloatMetric(metrics, "total_cpu_usage", 0.0),
		"user_cpu_usage":       getFloatMetric(metrics, "total_user_cpu_usage", 0.0),
		"memory_usage_mb":      getFloatMetric(metrics, "memory_usage_mb", 0.0),
		"active_users_count":   getIntMetric(metrics, "active_users_count", 0),
		"total_cores":          getIntMetric(metrics, "total_cores", 0),
		"system_under_load":    getBoolMetric(metrics, "system_under_load", false),
		"limits_active":        getBool(status, "limits_active", false),
		"limits_applied_time":  getString(status, "limits_applied_time", ""),
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     toJSON(result),
			},
		},
	}, nil
}

// handleActiveUsersResource handles resman://users/active
func (s *Server) handleActiveUsersResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	activeUsers := s.metricsCollector.GetAllUsers()
	result := make([]map[string]any, 0, len(activeUsers))

	for _, uid := range activeUsers {
		result = append(result, map[string]any{
			"uid": uid,
		})
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     toJSON(result),
			},
		},
	}, nil
}

// handleLimitsStatusResource handles resman://limits/status
func (s *Server) handleLimitsStatusResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	status := s.stateManager.GetStatus()

	result := map[string]any{
		"limits_active":        getBool(status, "limits_active", false),
		"limits_applied_time":  getString(status, "limits_applied_time", ""),
		"active_users_count":   getInt(status, "active_users_count", 0),
		"active_users":         getIntSlice(status, "active_users", []int{}),
		"shared_cgroup_path":   getString(status, "shared_cgroup_path", ""),
		"shared_cgroup_active": getBool(status, "shared_cgroup_active", false),
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     toJSON(result),
			},
		},
	}, nil
}

// handleConfigResource handles resman://config
func (s *Server) handleConfigResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	cfg := s.stateManager.GetConfig()

	result := map[string]any{
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

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     toJSON(result),
			},
		},
	}, nil
}

// handleUserMetricsResource handles resman://users/{uid}/metrics
func (s *Server) handleUserMetricsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	// Extract UID from URI
	uid, err := extractUIDFromURI(req.Params.URI)
	if err != nil {
		return nil, fmt.Errorf("invalid URI: %w", err)
	}

	allMetrics := s.metricsCollector.GetAllUserMetrics()
	metrics, exists := allMetrics[uid]
	if !exists {
		return nil, fmt.Errorf("no metrics found for UID %d", uid)
	}

	result := map[string]any{
		"uid":           uid,
		"username":      metrics.Username,
		"cpu_usage":     metrics.CPUUsage,
		"memory_usage":  metrics.MemoryUsage,
		"process_count": metrics.ProcessCount,
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     toJSON(result),
			},
		},
	}, nil
}

// handleCgroupResource handles resman://cgroups/{uid}
func (s *Server) handleCgroupResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	// Extract UID from URI
	uid, err := extractUIDFromURI(req.Params.URI)
	if err != nil {
		return nil, fmt.Errorf("invalid URI: %w", err)
	}

	info, err := s.cgroupManager.GetCgroupInfo(uid)
	if err != nil {
		return nil, fmt.Errorf("failed to get cgroup info: %w", err)
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     toJSON(info),
			},
		},
	}, nil
}

// extractUIDFromURI extracts the UID from a resource URI
func extractUIDFromURI(uri string) (int, error) {
	// Parse resman://users/{uid}/metrics or resman://cgroups/{uid}
	if strings.Contains(uri, "/users/") {
		// Format: resman://users/{uid}/metrics
		parts := strings.Split(uri, "/")
		for i, part := range parts {
			if part == "users" && i+1 < len(parts) {
				uid, err := strconv.Atoi(parts[i+1])
				if err != nil {
					return 0, fmt.Errorf("could not extract UID from URI: %s", uri)
				}
				return uid, nil
			}
		}
	} else if strings.Contains(uri, "/cgroups/") {
		// Format: resman://cgroups/{uid}
		parts := strings.Split(uri, "/")
		for i, part := range parts {
			if part == "cgroups" && i+1 < len(parts) {
				uid, err := strconv.Atoi(parts[i+1])
				if err != nil {
					return 0, fmt.Errorf("could not extract UID from URI: %s", uri)
				}
				return uid, nil
			}
		}
	}

	return 0, fmt.Errorf("could not extract UID from URI: %s", uri)
}
