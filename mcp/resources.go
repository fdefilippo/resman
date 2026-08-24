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

	resmanmetrics "github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
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
	metrics := s.metricsCollector.GetObservationMetrics()
	result := newSystemStatusPayload(getHostname(), s.stateManager.GetConfig().ServerRole, metrics, status)

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
	result := newLimitsStatusPayload(getHostname(), s.stateManager.GetConfig().ServerRole, status)

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
		"cpu_threshold":          cfg.CPUThreshold,
		"cpu_release_threshold":  cfg.CPUReleaseThreshold,
		"cpu_threshold_duration": cfg.CPUThresholdDuration,
		"polling_interval":       cfg.PollingInterval,
		"min_system_cores":       cfg.MinSystemCores,
		"cpu_quota_normal":       cfg.CPUQuotaNormal,
		"enable_prometheus":      cfg.EnablePrometheus,
		"prometheus_port":        cfg.PrometheusMetricsBindPort,
		"ignore_system_load":     cfg.IgnoreSystemLoad,
		"system_uid_min":         cfg.SystemUIDMin,
		"system_uid_max":         cfg.SystemUIDMax,
		// RAM limits
		"ram_enabled":           cfg.RAMEnabled,
		"ram_threshold":         cfg.RAMThreshold,
		"ram_release_threshold": cfg.RAMReleaseThreshold,
		"ram_quota_per_user":    cfg.RAMQuotaPerUser,
		"disable_swap":          cfg.DisableSwap,
		"ram_high_ratio":        cfg.RAMHighRatio,
		// IO limits
		"io_enabled":            cfg.IOEnabled,
		"io_threshold":          cfg.IOThreshold,
		"io_release_threshold":  cfg.IOReleaseThreshold,
		"io_threshold_duration": cfg.IOThresholdDuration,
		"io_read_bps":           cfg.IOReadBPS,
		"io_write_bps":          cfg.IOWriteBPS,
		"io_read_iops":          cfg.IOReadIOPS,
		"io_write_iops":         cfg.IOWriteIOPS,
		"io_device_filter":      cfg.IODeviceFilter,
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

	limitState := s.stateManager.GetUserLimitState(uid, metrics.Username)
	result := newUserMetricsResourcePayload(uid, metrics, limitState)

	// Add RAM cgroup metrics and limits.
	if info, err := s.cgroupManager.GetCgroupInfo(uid); err == nil {
		current, hasCurrent, max, high := extractCgroupMemoryMetrics(info)
		if hasCurrent {
			result["cgroup_memory_current_bytes"] = current
		}
		if max != "" {
			result["memory_max"] = max
		}
		if high != "" {
			result["memory_high"] = high
		}
	}

	// Add memory.high events.
	if events, err := s.cgroupManager.GetMemoryHighEvents(uid); err == nil {
		result["memory_high_events"] = events
	}

	// Add I/O stats.
	if ioRead, ioWrite, ioROps, ioWOps, err := s.cgroupManager.GetIOStats(uid); err == nil {
		result["io_read_bytes"] = ioRead
		result["io_write_bytes"] = ioWrite
		result["io_read_ops"] = ioROps
		result["io_write_ops"] = ioWOps
	}

	jsonData := toJSON(result)

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     jsonData,
			},
		},
	}, nil
}

func newUserMetricsResourcePayload(uid int, sample *resmanmetrics.UserMetrics, limitState state.UserLimitState) map[string]any {
	return map[string]any{
		"uid":                 uid,
		"username":            sample.Username,
		"cpu_usage":           sample.CPUUsage,
		"memory_usage":        sample.MemoryUsage,
		"process_count":       sample.ProcessCount,
		"eligible_for_cpu":    limitState.EligibleForCPU,
		"eligible_for_ram":    limitState.EligibleForRAM,
		"eligible_for_io":     limitState.EligibleForIO,
		"cpu_limit_requested": limitState.CPULimitRequested,
		"cpu_limit_active":    limitState.CPULimitActive,
		"ram_limit_requested": limitState.RAMLimitRequested,
		"ram_limit_active":    limitState.RAMLimitActive,
		"io_limit_requested":  limitState.IOLimitRequested,
		"io_limit_active":     limitState.IOLimitActive,
	}
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
