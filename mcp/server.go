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
// mcp/server.go
package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

// Server wraps the MCP server and Resource Manager dependencies
type Server struct {
	mcpServer        *mcp.Server
	cfg              *Config
	parentCfg        *config.Config
	stateManager     *state.Manager
	metricsCollector *metrics.Collector
	cgroupManager    *cgroup.Manager
	dbManager        *database.DatabaseManager
	logger           *logging.Logger
	httpServer       *http.Server
	shutdownChan     chan struct{}
	wg               sync.WaitGroup
	mu               sync.RWMutex
}

// NewServer creates a new MCP server instance
func NewServer(
	parentCfg *config.Config,
	sm *state.Manager,
	mc *metrics.Collector,
	cg *cgroup.Manager,
	dbm *database.DatabaseManager,
) (*Server, error) {
	logger := logging.GetLogger()

	// Load MCP configuration
	mcpCfg := &Config{
		Enabled:       parentCfg.MCPEnabled,
		Transport:     parentCfg.MCPTransport,
		HTTPPort:      parentCfg.MCPHTTPPort,
		HTTPHost:      parentCfg.MCPHTTPHost,
		LogLevel:      parentCfg.MCPLogLevel,
		AuthToken:     parentCfg.MCPAuthToken,
		AllowWriteOps: parentCfg.MCPAllowWriteOps,
	}

	if err := mcpCfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid MCP configuration: %w", err)
	}

	// Create MCP server
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "resman",
		Version: getVersion(),
	}, nil)

	s := &Server{
		mcpServer:        mcpServer,
		cfg:              mcpCfg,
		parentCfg:        parentCfg,
		stateManager:     sm,
		metricsCollector: mc,
		cgroupManager:    cg,
		dbManager:        dbm,
		logger:           logger,
		shutdownChan:     make(chan struct{}),
	}

	// Register tools and resources
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	logger.Info("MCP server initialized",
		"enabled", mcpCfg.Enabled,
		"transport", mcpCfg.Transport,
		"allow_write_ops", mcpCfg.AllowWriteOps,
	)

	return s, nil
}

// Start starts the MCP server with the configured transport
func (s *Server) Start(ctx context.Context) error {
	if !s.cfg.Enabled {
		s.logger.Info("MCP server is disabled, skipping start")
		return nil
	}

	s.logger.Info("Starting MCP server",
		"transport", s.cfg.Transport,
	)

	switch s.cfg.Transport {
	case "stdio":
		return s.startStdioTransport(ctx)
	case "http":
		return s.startHTTPTransport(ctx)
	default:
		return fmt.Errorf("unsupported transport: %s (supported: stdio, http)", s.cfg.Transport)
	}
}

// Stop stops the MCP server and cleans up resources
func (s *Server) Stop() error {
	s.logger.Info("Stopping MCP server")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Signal shutdown
	close(s.shutdownChan)

	// Stop HTTP server if running
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.Error("Error shutting down HTTP server", "error", err)
			return err
		}
	}

	// Wait for goroutines to finish
	s.wg.Wait()

	s.logger.Info("MCP server stopped")
	return nil
}

// startStdioTransport starts the MCP server with stdio transport
func (s *Server) startStdioTransport(ctx context.Context) error {
	s.logger.Info("MCP server started with stdio transport")

	// Run MCP server with stdio (stdin/stdout)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		// Use the MCP stdio transport
		if err := s.mcpServer.Run(ctx, &mcp.StdioTransport{}); err != nil {
			s.logger.Error("MCP stdio server error", "error", err)
		}
	}()

	return nil
}

// startHTTPTransport starts the MCP server with HTTP transport using Streamable HTTP
func (s *Server) startHTTPTransport(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.HTTPHost, s.cfg.HTTPPort)

	// Check if port is available
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to bind to %s: %w", addr, err)
	}

	// Create streamable HTTP handler for MCP
	// This handler properly processes JSON-RPC messages over HTTP
	mux := http.NewServeMux()

	// MCP streamable endpoint - handles all MCP JSON-RPC messages
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return s.mcpServer
	}, nil)

	// Wrap MCP handler with authentication and logging middleware
	handler := s.authMiddleware(s.loggingMiddleware(mcpHandler.ServeHTTP))
	mux.HandleFunc("/mcp", handler)

	// Health check endpoint (not part of MCP protocol)
	mux.HandleFunc("/health", s.handleHealthCheck)

	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.logger.Info("MCP HTTP streamable server started",
			"address", addr,
			"endpoint", "/mcp",
		)

		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.logger.Error("MCP HTTP server error", "error", err)
		}
	}()

	return nil
}

// loggingMiddleware logs HTTP requests before passing them to the handler
func (s *Server) loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Log request at INFO level to ensure visibility
		s.logger.Info("MCP HTTP request received",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"content_type", r.Header.Get("Content-Type"),
			"content_length", r.ContentLength,
		)

		// Create response wrapper to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Call next handler
		next(wrapped, r)

		// Log response
		duration := time.Since(start)
		s.logger.Info("MCP HTTP response sent",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", duration.Milliseconds(),
		)
	}
}

// authMiddleware validates authentication token if configured
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Skip auth if no token configured
		if s.cfg.AuthToken == "" {
			next(w, r)
			return
		}

		// Check Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, `{"error": "Missing Authorization header"}`, http.StatusUnauthorized)
			s.logger.Warn("MCP request rejected: missing Authorization header",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path,
			)
			return
		}

		// Validate Bearer token
		if !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, `{"error": "Invalid Authorization header format. Use: Bearer <token>"}`, http.StatusUnauthorized)
			s.logger.Warn("MCP request rejected: invalid Authorization format",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path,
			)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token != s.cfg.AuthToken {
			http.Error(w, `{"error": "Invalid authentication token"}`, http.StatusUnauthorized)
			s.logger.Warn("MCP request rejected: invalid token",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path,
			)
			return
		}

		// Token valid, proceed
		next(w, r)
	}
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// handleHealthCheck handles health check requests with logging
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("MCP health check requested",
		"method", r.Method,
		"remote_addr", r.RemoteAddr,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status": "healthy", "transport": "%s"}`, s.cfg.Transport)
}

// registerPrompts registers MCP prompts (pre-built queries)
func (s *Server) registerPrompts() {
	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "system-health",
		Description: "Quick system health check",
		Arguments:   []*mcp.PromptArgument{},
	}, s.handleSystemHealthPrompt)

	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "user-analysis",
		Description: "Analyze resource usage by user",
		Arguments: []*mcp.PromptArgument{
			{
				Name:        "uid",
				Description: "Specific user ID to analyze (optional)",
				Required:    false,
			},
		},
	}, s.handleUserAnalysisPrompt)

	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "troubleshooting",
		Description: "Diagnose CPU limit issues",
		Arguments:   []*mcp.PromptArgument{},
	}, s.handleTroubleshootingPrompt)
}

// handleSystemHealthPrompt handles the system-health prompt
func (s *Server) handleSystemHealthPrompt(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	metrics := s.metricsCollector.GetDetailedMetrics()
	status := s.stateManager.GetStatus()

	text := fmt.Sprintf(`# System Health Check

## CPU Usage
- **Total CPU**: %.1f%%
- **User CPU**: %.1f%%
- **Total Cores**: %d

## Memory
- **Usage**: %.1f MB

## Status
- **Active Users**: %d
- **Limits Active**: %v
- **System Under Load**: %v

## Assessment
`,
		getFloatMetric(metrics, "total_cpu_usage", 0.0),
		getFloatMetric(metrics, "total_user_cpu_usage", 0.0),
		getIntMetric(metrics, "total_cores", 0),
		getFloatMetric(metrics, "memory_usage_mb", 0.0),
		getIntMetric(metrics, "active_users_count", 0),
		getBool(status, "limits_active", false),
		getBoolMetric(metrics, "system_under_load", false),
	)

	// Add assessment
	if getFloatMetric(metrics, "total_user_cpu_usage", 0.0) > 70 {
		text += "**HIGH CPU USAGE** - Consider activating CPU limits\n"
	} else if getFloatMetric(metrics, "total_user_cpu_usage", 0.0) < 30 {
		text += "**LOW CPU USAGE** - System is running smoothly\n"
	} else {
		text += "**MODERATE CPU USAGE** - System is operating normally\n"
	}

	return &mcp.GetPromptResult{
		Description: "System health check results",
		Messages: []*mcp.PromptMessage{
			{
				Role:    "user",
				Content: &mcp.TextContent{Text: text},
			},
		},
	}, nil
}

// handleUserAnalysisPrompt handles the user-analysis prompt
func (s *Server) handleUserAnalysisPrompt(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	allMetrics := s.metricsCollector.GetAllUserMetrics()

	text := "# User Resource Analysis\n\n"
	text += "| UID | Username | CPU % | Memory (MB) | Processes |\n"
	text += "|-----|----------|-------|-------------|----------|\n"

	for uid, metrics := range allMetrics {
		text += fmt.Sprintf("| %d | %s | %.1f | %.1f | %d |\n",
			uid,
			metrics.Username,
			metrics.CPUUsage,
			float64(metrics.MemoryUsage)/1024/1024,
			metrics.ProcessCount,
		)
	}

	return &mcp.GetPromptResult{
		Description: "User resource analysis",
		Messages: []*mcp.PromptMessage{
			{
				Role:    "user",
				Content: &mcp.TextContent{Text: text},
			},
		},
	}, nil
}

// handleTroubleshootingPrompt handles the troubleshooting prompt
func (s *Server) handleTroubleshootingPrompt(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	status := s.stateManager.GetStatus()
	metrics := s.metricsCollector.GetDetailedMetrics()

	text := `# Resource Manager Troubleshooting

## Current Status
`
	text += fmt.Sprintf("- **Limits Active**: %v\n", getBool(status, "limits_active", false))
	text += fmt.Sprintf("- **Total CPU Usage**: %.1f%%\n", getFloatMetric(metrics, "total_cpu_usage", 0.0))
	text += fmt.Sprintf("- **User CPU Usage**: %.1f%%\n", getFloatMetric(metrics, "total_user_cpu_usage", 0.0))
	text += fmt.Sprintf("- **Active Users**: %d\n", getIntMetric(metrics, "active_users_count", 0))

	text += "\n## Diagnostic Steps\n\n"

	// Check 1: CPU Usage
	if getFloatMetric(metrics, "total_user_cpu_usage", 0.0) > 70 {
		text += "1. **HIGH CPU USAGE DETECTED**\n"
		text += "   - Check which users are consuming the most CPU\n"
		text += "   - Consider running `activate_limits` if not already active\n"
	} else {
		text += "1. **CPU Usage Normal** - No immediate action needed\n"
	}

	// Check 2: Limits Status
	if getBool(status, "limits_active", false) {
		text += "2. **CPU Limits Active** - Limits are being enforced\n"
		if count := getInt(status, "active_users_count", 0); count > 0 {
			text += fmt.Sprintf("   - %d users currently limited\n", count)
		}
	} else {
		text += "2. **CPU Limits Inactive** - No limits currently enforced\n"
	}

	text += "\n## Recommended Actions\n"
	text += "- Use `get_user_metrics` to identify high CPU users\n"
	text += "- Use `get_limits_status` to check current limit state\n"
	text += "- Use `get_configuration` to review thresholds\n"

	return &mcp.GetPromptResult{
		Description: "Troubleshooting diagnostic results",
		Messages: []*mcp.PromptMessage{
			{
				Role:    "user",
				Content: &mcp.TextContent{Text: text},
			},
		},
	}, nil
}

// getVersion returns the MCP server version
func getVersion() string {
	// This could be set via build flags
	version := "1.0.0"
	return version
}
