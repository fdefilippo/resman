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
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkjsonrpc "github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/operationgate"
	"github.com/fdefilippo/resman/internal/tlsconfig"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

const (
	mcpProtocolVersion           = "2026-07-28"
	mcpProtocolVersionHeader     = "Mcp-Protocol-Version"
	mcpMethodHeader              = "Mcp-Method"
	mcpSessionIDHeader           = "Mcp-Session-Id"
	mcpLastEventIDHeader         = "Last-Event-ID"
	mcpMethodInitialize          = "initialize"
	mcpNotificationInitialized   = "notifications/initialized"
	mcpMethodDiscover            = "server/discover"
	mcpDefaultMaxRequestBodySize = 4 << 20
)

// ConfigurationReloader applies a persisted configuration and returns only
// after its runtime outcome is known.
type ConfigurationReloader interface {
	Reload(context.Context) error
}

type serverLogger interface {
	Info(string, ...interface{})
	Warn(string, ...interface{})
	Error(string, ...interface{})
}

type cgroupInfoReader interface {
	GetCgroupInfo(uid int) (cgroup.CgroupInfo, error)
	GetMemoryHighEvents(uid int) (uint64, error)
	GetIOStats(uid int) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error)
}

// Server wraps the MCP server and Resource Manager dependencies
type Server struct {
	mcpServer         *mcp.Server
	cfg               *config.MCPServerConfig
	stateManager      *state.Manager
	metricsCollector  *metrics.Collector
	cgroupManager     cgroupInfoReader
	dbManager         *database.DatabaseManager
	configReloader    ConfigurationReloader
	logger            serverLogger
	httpServer        *http.Server
	tlsConfig         *tls.Config
	httpListen        func(network, address string) (net.Listener, error)
	stdioTransport    mcp.Transport
	transportCancel   context.CancelFunc
	wg                sync.WaitGroup
	stopOnce          sync.Once
	stopErr           error
	stopped           bool
	mu                sync.RWMutex
	lifecycleGate     operationgate.Gate
	configWriteActive atomic.Bool
}

// NewServer creates a new MCP server instance
func NewServer(
	parentCfg *config.Config,
	sm *state.Manager,
	mc *metrics.Collector,
	cg *cgroup.Manager,
	dbm *database.DatabaseManager,
	configReloader ConfigurationReloader,
) (*Server, error) {
	logger := logging.GetLogger()

	// Snapshot the shared, centrally parsed MCP configuration contract.
	mcpCfg := parentCfg.MCPServerConfig()

	if err := mcpCfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid MCP configuration: %w", err)
	}

	var serverTLSConfig *tls.Config
	if mcpCfg.Enabled && mcpCfg.Transport == "http" {
		var err error
		serverTLSConfig, err = tlsconfig.BuildServer(tlsconfig.ServerOptions{
			CertFile:   mcpCfg.TLSCertFile,
			KeyFile:    mcpCfg.TLSKeyFile,
			CAFile:     mcpCfg.TLSCAFile,
			MinVersion: mcpCfg.TLSMinVersion,
		})
		if err != nil {
			return nil, fmt.Errorf("loading MCP TLS configuration: %w", err)
		}
	}

	// Create MCP server
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "resman",
		Version: getVersion(),
	}, nil)
	mcpServer.AddReceivingMiddleware(latestOnlyMCPMiddleware)

	s := &Server{
		mcpServer:        mcpServer,
		cfg:              &mcpCfg,
		stateManager:     sm,
		metricsCollector: mc,
		cgroupManager:    cg,
		dbManager:        dbm,
		configReloader:   configReloader,
		logger:           logger,
		tlsConfig:        serverTLSConfig,
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

	leaveLifecycle := s.lifecycleGate.Enter()
	defer leaveLifecycle()

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return fmt.Errorf("MCP server cannot be started after it has been stopped")
	}
	if s.transportCancel != nil {
		s.mu.Unlock()
		return fmt.Errorf("MCP server is already started")
	}

	transportCtx, cancel := context.WithCancel(ctx)
	s.transportCancel = cancel
	s.mu.Unlock()

	s.logger.Info("Starting MCP server",
		"transport", s.cfg.Transport,
	)

	var err error
	switch s.cfg.Transport {
	case "stdio":
		err = s.startStdioTransport(transportCtx)
	case "http":
		err = s.startHTTPTransport(transportCtx)
	default:
		err = fmt.Errorf("unsupported transport: %s (supported: stdio, http)", s.cfg.Transport)
	}
	if err != nil {
		cancel()
		s.mu.Lock()
		s.transportCancel = nil
		s.mu.Unlock()
	}
	return err
}

// Stop stops the MCP server and cleans up resources
func (s *Server) Stop() error {
	s.stopOnce.Do(func() {
		s.stopErr = s.stop()
	})
	return s.stopErr
}

func (s *Server) stop() error {
	leaveLifecycle := s.lifecycleGate.Enter()
	defer leaveLifecycle()

	s.logger.Info("Stopping MCP server")

	s.mu.Lock()
	s.stopped = true
	cancel := s.transportCancel
	httpServer := s.httpServer
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	var shutdownErr error
	if httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(ctx); err != nil {
			s.logger.Error("Error shutting down HTTP server", "error", err)
			shutdownErr = fmt.Errorf("failed to shut down MCP HTTP server: %w", err)
		}
	}

	s.wg.Wait()

	s.logger.Info("MCP server stopped")
	return shutdownErr
}

// startStdioTransport starts the MCP server with stdio transport
func (s *Server) startStdioTransport(ctx context.Context) error {
	s.logger.Info("MCP server started with stdio transport")

	transport := s.stdioTransport
	if transport == nil {
		transport = &mcp.StdioTransport{}
	}

	// Run MCP server with stdio (stdin/stdout)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		// Use the MCP stdio transport
		if err := s.mcpServer.Run(ctx, transport); err != nil && ctx.Err() == nil {
			s.logger.Error("MCP stdio server error", "error", err)
		}
	}()

	return nil
}

// startHTTPTransport starts the MCP server with HTTP transport using Streamable HTTP
func (s *Server) startHTTPTransport(ctx context.Context) error {
	if s.tlsConfig == nil || len(s.tlsConfig.Certificates) == 0 {
		return fmt.Errorf("MCP HTTPS transport requires a loaded TLS certificate")
	}

	addr := net.JoinHostPort(s.cfg.HTTPHost, strconv.Itoa(s.cfg.HTTPPort))

	listen := s.httpListen
	if listen == nil {
		listen = net.Listen
	}
	listener, err := listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to bind MCP HTTPS to %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", s.newMCPHTTPHandler())

	// Health check endpoint (not part of MCP protocol)
	mux.HandleFunc("/health", s.handleHealthCheck)

	httpServer := newMCPHTTPServer(addr, mux, s.tlsConfig)
	s.mu.Lock()
	s.httpServer = httpServer
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.logger.Info("MCP HTTPS streamable server started",
			"address", addr,
			"endpoint", "/mcp",
			"tls_min_version", s.cfg.TLSMinVersion,
			"mtls_enabled", s.cfg.TLSCAFile != "",
		)

		if err := httpServer.ServeTLS(listener, "", ""); err != nil && err != http.ErrServerClosed {
			s.logger.Error("MCP HTTPS server error", "error", err)
		}
	}()

	return nil
}

// IsStarted reports lifecycle state without waiting for listener operations.
func (s *Server) IsStarted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.transportCancel != nil && !s.stopped
}

func (s *Server) newMCPHTTPHandler() http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return s.mcpServer
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		MaxRequestBodyBytes:          mcpDefaultMaxRequestBodySize,
		PropagateRequestCancellation: true,
	})

	// Authentication stays outermost so unauthenticated callers cannot fingerprint
	// the supported protocol revision.
	return s.authMiddleware(s.loggingMiddleware(latestOnlyHTTPMiddleware(mcpHandler.ServeHTTP)))
}

type protocolVersionRequest interface {
	ProtocolVersion() string
}

func latestOnlyMCPMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		version := ""
		if versioned, ok := req.(protocolVersionRequest); ok {
			version = versioned.ProtocolVersion()
		}
		if err := validateLatestOnlyMCPRequest(method, version); err != nil {
			return nil, err
		}

		result, err := next(ctx, method, req)
		if err != nil {
			return nil, err
		}
		if method == mcpMethodDiscover {
			if discovery, ok := result.(*mcp.DiscoverResult); ok {
				discovery.SupportedVersions = []string{mcpProtocolVersion}
			}
		}
		return result, nil
	}
}

func validateLatestOnlyMCPRequest(method, version string) error {
	switch method {
	case mcpMethodInitialize, mcpNotificationInitialized:
		return &sdkjsonrpc.Error{
			Code:    sdkjsonrpc.CodeMethodNotFound,
			Message: fmt.Sprintf("%q is not supported; use stateless MCP %s requests", method, mcpProtocolVersion),
		}
	}
	if version != mcpProtocolVersion {
		return unsupportedProtocolVersionError(version)
	}
	return nil
}

func unsupportedProtocolVersionError(requested string) error {
	data, err := json.Marshal(mcp.UnsupportedProtocolVersionData{
		Supported: []string{mcpProtocolVersion},
		Requested: requested,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal static MCP protocol error data: %v", err))
	}
	return &sdkjsonrpc.Error{
		Code:    mcp.CodeUnsupportedProtocolVersion,
		Message: fmt.Sprintf("unsupported MCP protocol version %q; only %s is supported", requested, mcpProtocolVersion),
		Data:    data,
	}
}

func latestOnlyHTTPMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeMCPHTTPError(w, http.StatusMethodNotAllowed, &sdkjsonrpc.Error{
				Code:    sdkjsonrpc.CodeInvalidRequest,
				Message: "the MCP endpoint accepts POST requests only",
			})
			return
		}
		if r.Header.Get(mcpSessionIDHeader) != "" {
			writeMCPHTTPError(w, http.StatusBadRequest, &sdkjsonrpc.Error{
				Code:    sdkjsonrpc.CodeInvalidRequest,
				Message: "Mcp-Session-Id is not supported by the stateless MCP endpoint",
			})
			return
		}
		if r.Header.Get(mcpLastEventIDHeader) != "" {
			writeMCPHTTPError(w, http.StatusBadRequest, &sdkjsonrpc.Error{
				Code:    sdkjsonrpc.CodeInvalidRequest,
				Message: "Last-Event-ID resumability is not supported by the stateless MCP endpoint",
			})
			return
		}
		version := r.Header.Get(mcpProtocolVersionHeader)
		if version != mcpProtocolVersion {
			writeMCPHTTPError(w, http.StatusBadRequest, unsupportedProtocolVersionError(version))
			return
		}
		method := r.Header.Get(mcpMethodHeader)
		if method == mcpMethodInitialize || method == mcpNotificationInitialized {
			writeMCPHTTPError(w, http.StatusNotFound, &sdkjsonrpc.Error{
				Code:    sdkjsonrpc.CodeMethodNotFound,
				Message: fmt.Sprintf("%q is not supported; use stateless MCP %s requests", method, mcpProtocolVersion),
			})
			return
		}
		next(w, r)
	}
}

func writeMCPHTTPError(w http.ResponseWriter, status int, protocolErr error) {
	var wireErr *sdkjsonrpc.Error
	if !errors.As(protocolErr, &wireErr) {
		wireErr = &sdkjsonrpc.Error{
			Code:    sdkjsonrpc.CodeInternalError,
			Message: protocolErr.Error(),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		JSONRPC string            `json:"jsonrpc"`
		ID      any               `json:"id"`
		Error   *sdkjsonrpc.Error `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      nil,
		Error:   wireErr,
	}); err != nil {
		logging.GetLogger().Error("Failed to write MCP protocol error response", "error", err)
	}
}

func newMCPHTTPServer(addr string, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
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

// authMiddleware validates the HTTP transport authentication token.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(s.cfg.AuthToken) == "" {
			http.Error(w, `{"error": "Authentication is not configured"}`, http.StatusServiceUnavailable)
			s.logger.Error("MCP request rejected: HTTP authentication is not configured",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path,
			)
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
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AuthToken)) != 1 {
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
	_, _ = fmt.Fprintf(w, `{"status": "healthy", "transport": "%s"}`, s.cfg.Transport)
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
	metrics := s.metricsCollector.GetObservationMetrics()
	status := s.stateManager.GetStatus()

	text := fmt.Sprintf(`# System Health Check

## CPU Usage
- **Total CPU**: %.1f%%
- **Observed Users CPU**: %.1f%%
- **Total Cores**: %d

## Memory
- **Usage**: %.1f MB

## Status
- **Observed Users**: %d
- **Actively Limited Users**: %d
- **Any Limits Active**: %v
- **CPU Limits Active**: %v
- **Resource Limits Active**: %v
- **System Under Load**: %v

## Assessment
`,
		metrics.TotalCPUUsage,
		metrics.ObservedUsersCPUUsage,
		metrics.TotalCores,
		metrics.MemoryUsageMB,
		metrics.ObservedUsersCount,
		status.ActivelyLimitedUsersCount,
		status.AnyLimitsActive,
		status.CPULimitsActive,
		status.ResourceLimitsActive,
		metrics.SystemUnderLoad,
	)

	// Add assessment
	if metrics.ObservedUsersCPUUsage > 70 {
		text += "**HIGH CPU USAGE** - Consider activating CPU limits\n"
	} else if metrics.ObservedUsersCPUUsage < 30 {
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
	metrics := s.metricsCollector.GetObservationMetrics()

	text := `# Resource Manager Troubleshooting

## Current Status
`
	text += fmt.Sprintf("- **Any Limits Active**: %v\n", status.AnyLimitsActive)
	text += fmt.Sprintf("- **CPU Limits Active**: %v\n", status.CPULimitsActive)
	text += fmt.Sprintf("- **Resource Limits Active**: %v\n", status.ResourceLimitsActive)
	text += fmt.Sprintf("- **Total CPU Usage**: %.1f%%\n", metrics.TotalCPUUsage)
	text += fmt.Sprintf("- **Observed Users CPU Usage**: %.1f%%\n", metrics.ObservedUsersCPUUsage)
	text += fmt.Sprintf("- **Observed Users**: %d\n", metrics.ObservedUsersCount)
	text += fmt.Sprintf("- **Actively Limited Users**: %d\n", status.ActivelyLimitedUsersCount)

	text += "\n## Diagnostic Steps\n\n"

	// Check 1: CPU Usage
	if metrics.ObservedUsersCPUUsage > 70 {
		text += "1. **HIGH CPU USAGE DETECTED**\n"
		text += "   - Check which users are consuming the most CPU\n"
		text += "   - Consider running `activate_limits` if not already active\n"
	} else {
		text += "1. **CPU Usage Normal** - No immediate action needed\n"
	}

	// Check 2: Limits Status
	if status.AnyLimitsActive {
		text += "2. **Enforcement Active** - At least one resource limit is being enforced\n"
		if count := status.ActivelyLimitedUsersCount; count > 0 {
			text += fmt.Sprintf("   - %d users currently limited\n", count)
		}
	} else {
		text += "2. **Enforcement Inactive** - No limits currently enforced\n"
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
