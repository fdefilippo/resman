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
// mcp/server_test.go
package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.MCPServerConfig
		wantErr bool
	}{
		{
			name: "valid stdio config",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "stdio",
				LogLevel:        "INFO",
				AllowWriteOps:   false,
				ShutdownTimeout: 10,
			},
			wantErr: false,
		},
		{
			name: "valid http config",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "http",
				HTTPPort:        8080,
				HTTPHost:        "127.0.0.1",
				TLSEnabled:      true,
				TLSCertFile:     "server.crt",
				TLSKeyFile:      "server.key",
				TLSMinVersion:   "1.3",
				LogLevel:        "INFO",
				AuthToken:       "test-token",
				AllowWriteOps:   false,
				ShutdownTimeout: 10,
			},
			wantErr: false,
		},
		{
			name: "valid http editor principal",
			cfg: &config.MCPServerConfig{
				Enabled: true, Transport: "http", HTTPPort: 8080, TLSEnabled: true,
				TLSCertFile: "server.crt", TLSKeyFile: "server.key", TLSMinVersion: "1.3",
				LogLevel: "INFO", AuthToken: "operator-token", EditorAuthToken: "editor-token",
				AllowWriteOps: true, ShutdownTimeout: 10,
			},
		},
		{
			name: "write-enabled HTTP without editor token",
			cfg: &config.MCPServerConfig{
				Enabled: true, Transport: "http", HTTPPort: 8080, TLSEnabled: true,
				TLSCertFile: "server.crt", TLSKeyFile: "server.key", TLSMinVersion: "1.3",
				LogLevel: "INFO", AuthToken: "operator-token", AllowWriteOps: true, ShutdownTimeout: 10,
			},
			wantErr: true,
		},
		{
			name: "editor token equals operator token",
			cfg: &config.MCPServerConfig{
				Enabled: true, Transport: "http", HTTPPort: 8080, TLSEnabled: true,
				TLSCertFile: "server.crt", TLSKeyFile: "server.key", TLSMinVersion: "1.3",
				LogLevel: "INFO", AuthToken: "same-token", EditorAuthToken: "same-token",
				AllowWriteOps: true, ShutdownTimeout: 10,
			},
			wantErr: true,
		},
		{
			name: "enabled config with invalid shutdown timeout",
			cfg: &config.MCPServerConfig{
				Enabled:   true,
				Transport: "stdio",
				LogLevel:  "INFO",
			},
			wantErr: true,
		},
		{
			name: "http config without token",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "http",
				HTTPPort:        8080,
				HTTPHost:        "127.0.0.1",
				LogLevel:        "INFO",
				ShutdownTimeout: 10,
			},
			wantErr: true,
		},
		{
			name: "http config with whitespace token",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "http",
				HTTPPort:        8080,
				HTTPHost:        "127.0.0.1",
				LogLevel:        "INFO",
				AuthToken:       "   ",
				ShutdownTimeout: 10,
			},
			wantErr: true,
		},
		{
			name: "http config with TLS-protected non-loopback bind",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "http",
				HTTPPort:        8080,
				HTTPHost:        "0.0.0.0",
				TLSEnabled:      true,
				TLSCertFile:     "server.crt",
				TLSKeyFile:      "server.key",
				TLSMinVersion:   "1.3",
				LogLevel:        "INFO",
				AuthToken:       "test-token",
				ShutdownTimeout: 10,
			},
			wantErr: false,
		},
		{
			name: "http config with TLS disabled",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "http",
				HTTPPort:        8080,
				HTTPHost:        "127.0.0.1",
				TLSCertFile:     "server.crt",
				TLSKeyFile:      "server.key",
				TLSMinVersion:   "1.3",
				LogLevel:        "INFO",
				AuthToken:       "test-token",
				ShutdownTimeout: 10,
			},
			wantErr: true,
		},
		{
			name: "invalid transport",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "invalid",
				LogLevel:        "INFO",
				ShutdownTimeout: 10,
			},
			wantErr: true,
		},
		{
			name: "invalid port",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "http",
				HTTPPort:        70000,
				HTTPHost:        "127.0.0.1",
				LogLevel:        "INFO",
				ShutdownTimeout: 10,
			},
			wantErr: true,
		},
		{
			name: "invalid log level",
			cfg: &config.MCPServerConfig{
				Enabled:         true,
				Transport:       "stdio",
				LogLevel:        "INVALID",
				ShutdownTimeout: 10,
			},
			wantErr: true,
		},
		{
			name: "disabled config",
			cfg: &config.MCPServerConfig{
				Enabled:   false,
				Transport: "stdio",
				LogLevel:  "INFO",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Config.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewServer(t *testing.T) {
	parentCfg := config.DefaultConfig()
	parentCfg.MCPEnabled = false // Don't actually start the server
	parentCfg.MCPShutdownTimeout = 17

	// Create mock dependencies (nil for this test)
	// In a real test, you'd create proper mocks
	server, err := NewServer(parentCfg, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server == nil {
		t.Fatal("NewServer() returned nil server")
	}
	if server.cfg.Enabled != false {
		t.Error("Expected server to be disabled")
	}
	if server.cfg.ShutdownTimeout != 17 {
		t.Fatalf("MCP shutdown timeout = %d, want 17", server.cfg.ShutdownTimeout)
	}
}

func TestStopUsesConfiguredMCPShutdownTimeout(t *testing.T) {
	const timeout = 17 * time.Second
	observed := make(chan time.Duration, 1)
	server := &Server{
		cfg:    &config.MCPServerConfig{ShutdownTimeout: int(timeout / time.Second)},
		logger: &recordingServerLogger{},
		httpShutdown: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("MCP HTTP shutdown context has no deadline")
			}
			observed <- time.Until(deadline)
			return nil
		},
	}

	if err := server.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	remaining := <-observed
	if remaining < timeout-time.Second || remaining > timeout {
		t.Fatalf("MCP HTTP shutdown deadline = %s, want approximately %s", remaining, timeout)
	}
}

func TestNewServerRejectsUnauthenticatedHTTP(t *testing.T) {
	parentCfg := config.DefaultConfig()
	configureMCPTestTLS(t, parentCfg)
	parentCfg.MCPEnabled = true
	parentCfg.MCPTransport = "http"
	parentCfg.MCPHTTPHost = "127.0.0.1"
	parentCfg.MCPAuthToken = ""

	if _, err := NewServer(parentCfg, nil, nil, nil, nil); err == nil {
		t.Fatal("NewServer() accepted HTTP transport without MCP_AUTH_TOKEN")
	}

	parentCfg.MCPAuthToken = "test-token"
	if _, err := NewServer(parentCfg, nil, nil, nil, nil); err != nil {
		t.Fatalf("NewServer() rejected authenticated HTTP transport: %v", err)
	}
}

func TestNewServerRequiresTLSForHTTP(t *testing.T) {
	parentCfg := config.DefaultConfig()
	parentCfg.MCPEnabled = true
	parentCfg.MCPTransport = "http"
	parentCfg.MCPAuthToken = "test-token"
	parentCfg.MCPTLSEnabled = false

	if _, err := NewServer(parentCfg, nil, nil, nil, nil); err == nil {
		t.Fatal("NewServer() accepted cleartext MCP HTTP transport")
	}
}

func TestNewServerRejectsInvalidTLSCredentials(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T) (string, string)
	}{
		{
			name: "missing files",
			prepare: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				return filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key")
			},
		},
		{
			name: "invalid keypair",
			prepare: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				certFile := filepath.Join(dir, "server.crt")
				keyFile := filepath.Join(dir, "server.key")
				if err := os.WriteFile(certFile, []byte("not a certificate"), 0600); err != nil {
					t.Fatalf("write invalid certificate: %v", err)
				}
				if err := os.WriteFile(keyFile, []byte("not a key"), 0600); err != nil {
					t.Fatalf("write invalid key: %v", err)
				}
				return certFile, keyFile
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			certFile, keyFile := tt.prepare(t)
			parentCfg := config.DefaultConfig()
			parentCfg.MCPEnabled = true
			parentCfg.MCPTransport = "http"
			parentCfg.MCPAuthToken = "test-token"
			parentCfg.MCPTLSCertFile = certFile
			parentCfg.MCPTLSKeyFile = keyFile

			if server, err := NewServer(parentCfg, nil, nil, nil, nil); err == nil || server != nil {
				t.Fatalf("NewServer() = (%v, %v), want nil server and TLS credential error", server, err)
			}
		})
	}
}

func TestNewServerEnablesMutualTLSWhenClientCAConfigured(t *testing.T) {
	parentCfg := config.DefaultConfig()
	caFile := configureMCPTestTLS(t, parentCfg)
	parentCfg.MCPEnabled = true
	parentCfg.MCPTransport = "http"
	parentCfg.MCPAuthToken = "test-token"
	parentCfg.MCPTLSCAFile = caFile

	server, err := NewServer(parentCfg, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	if server.tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert || server.tlsConfig.ClientCAs == nil {
		t.Fatal("MCP_TLS_CA_FILE did not enable required client-certificate authentication")
	}
	if server.tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MCP TLS minimum version = %d, want TLS 1.3", server.tlsConfig.MinVersion)
	}
}

func TestMCPHTTPServerProtectsNonLoopbackBindWithTLS(t *testing.T) {
	parentCfg := config.DefaultConfig()
	caFile := configureMCPTestTLS(t, parentCfg)
	parentCfg.MCPEnabled = true
	parentCfg.MCPTransport = "http"
	parentCfg.MCPHTTPHost = "0.0.0.0"
	parentCfg.MCPAuthToken = "test-token"

	mcpServer, err := NewServer(parentCfg, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	mcpServer.cfg.HTTPPort = 0
	var listener net.Listener
	mcpServer.httpListen = func(network, address string) (net.Listener, error) {
		var listenErr error
		listener, listenErr = net.Listen(network, address)
		return listener, listenErr
	}
	if err := mcpServer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() { _ = mcpServer.Stop() })

	port := listener.Addr().(*net.TCPAddr).Port
	plainURL := "http://127.0.0.1:" + strconv.Itoa(port) + "/health"
	plainClient := &http.Client{Timeout: time.Second}
	response, _ := plainClient.Get(plainURL)
	if response != nil {
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("cleartext request reached the MCP health handler")
		}
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read test CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("append test CA certificate")
	}
	httpsClient := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    roots,
			ServerName: "example.com",
		}},
	}
	httpsResponse, err := httpsClient.Get("https://127.0.0.1:" + strconv.Itoa(port) + "/health")
	if err != nil {
		t.Fatalf("TLS request: %v", err)
	}
	_ = httpsResponse.Body.Close()
	if httpsResponse.StatusCode != http.StatusOK {
		t.Fatalf("TLS health request status = %d, want %d", httpsResponse.StatusCode, http.StatusOK)
	}
}

func TestExtractUIDFromURI(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		want    int
		wantErr bool
	}{
		{
			name:    "valid users URI",
			uri:     "resman://users/1000/metrics",
			want:    1000,
			wantErr: false,
		},
		{
			name:    "invalid URI",
			uri:     "resman://invalid",
			want:    0,
			wantErr: true,
		},
		{
			name:    "malformed UID",
			uri:     "resman://users/abc/metrics",
			want:    0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractUIDFromURI(tt.uri)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractUIDFromURI() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("extractUIDFromURI() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToJSON(t *testing.T) {
	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	obj := testStruct{Name: "test", Value: 42}
	jsonStr := toJSON(obj)

	expected := "{\n  \"name\": \"test\",\n  \"value\": 42\n}"
	if jsonStr != expected {
		t.Errorf("toJSON() = %v, want %v", jsonStr, expected)
	}
}

func TestServerStartStop(t *testing.T) {
	parentCfg := config.DefaultConfig()
	parentCfg.MCPEnabled = false

	server, err := NewServer(parentCfg, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	ctx := context.Background()

	// Start should be a no-op when disabled
	if err := server.Start(ctx); err != nil {
		t.Errorf("Server.Start() error = %v", err)
	}

	// Stop should work without errors
	if err := server.Stop(); err != nil {
		t.Errorf("Server.Stop() error = %v", err)
	}
	if err := server.Stop(); err != nil {
		t.Errorf("second Server.Stop() error = %v", err)
	}
}

func TestServerLifecycleStateRemainsAvailableWhileListenerCreationBlocks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := &Server{
		cfg:       &config.MCPServerConfig{Enabled: true, Transport: "http", HTTPHost: "127.0.0.1", HTTPPort: 1969},
		logger:    logging.GetLogger(),
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{{}}},
		httpListen: func(string, string) (net.Listener, error) {
			close(started)
			<-release
			return nil, os.ErrPermission
		},
	}
	startDone := make(chan error, 1)
	go func() { startDone <- server.Start(context.Background()) }()
	<-started

	stateDone := make(chan bool, 1)
	go func() { stateDone <- server.IsStarted() }()
	select {
	case running := <-stateDone:
		if !running {
			t.Fatal("IsStarted() = false while listener creation is in progress")
		}
	case <-time.After(time.Second):
		close(release)
		<-startDone
		t.Fatal("IsStarted() blocked behind listener creation")
	}
	close(release)
	if err := <-startDone; err == nil {
		t.Fatal("Start() succeeded after listener creation failed")
	}
}

func TestServerStopCancelsTransportContext(t *testing.T) {
	transportCtx, cancel := context.WithCancel(context.Background())
	server := &Server{
		logger:          logging.GetLogger(),
		transportCancel: cancel,
	}

	if err := server.Stop(); err != nil {
		t.Fatalf("Server.Stop() error = %v", err)
	}
	select {
	case <-transportCtx.Done():
	default:
		t.Fatal("Server.Stop() did not cancel the transport context")
	}
}

func TestServerStopTerminatesStdioTransport(t *testing.T) {
	serverTransport, _ := sdkmcp.NewInMemoryTransports()
	server := &Server{
		mcpServer: sdkmcp.NewServer(&sdkmcp.Implementation{
			Name:    "resman-test",
			Version: "test",
		}, nil),
		cfg:            &config.MCPServerConfig{Enabled: true, Transport: "stdio"},
		logger:         logging.GetLogger(),
		stdioTransport: serverTransport,
	}

	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}

	stopped := make(chan error, 1)
	go func() {
		stopped <- server.Stop()
	}()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Server.Stop() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Server.Stop() did not terminate the stdio transport")
	}
}

func TestAuthMiddlewareFailsClosed(t *testing.T) {
	tests := []struct {
		name          string
		serverToken   string
		editorToken   string
		authHeader    string
		wantStatus    int
		wantCalled    bool
		wantPrincipal principalKind
	}{
		{
			name:       "authentication not configured",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:        "missing authorization header",
			serverToken: "test-token",
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:        "invalid authorization scheme",
			serverToken: "test-token",
			authHeader:  "Basic test-token",
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:        "invalid token",
			serverToken: "test-token",
			authHeader:  "Bearer wrong-token",
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:          "valid token",
			serverToken:   "test-token",
			authHeader:    "Bearer test-token",
			wantStatus:    http.StatusNoContent,
			wantCalled:    true,
			wantPrincipal: principalOperator,
		},
		{
			name:          "valid editor token",
			serverToken:   "test-token",
			editorToken:   "editor-token",
			authHeader:    "Bearer editor-token",
			wantStatus:    http.StatusNoContent,
			wantCalled:    true,
			wantPrincipal: principalEditor,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				cfg:    &config.MCPServerConfig{AuthToken: tt.serverToken, EditorAuthToken: tt.editorToken},
				logger: logging.GetLogger(),
			}
			called := false
			handler := server.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if principal, _ := r.Context().Value(principalContextKey{}).(principalKind); principal != tt.wantPrincipal {
					t.Errorf("principal = %v, want %v", principal, tt.wantPrincipal)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tt.authHeader != "" {
				request.Header.Set("Authorization", tt.authHeader)
			}
			recorder := httptest.NewRecorder()

			handler(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if called != tt.wantCalled {
				t.Fatalf("next handler called = %t, want %t", called, tt.wantCalled)
			}
		})
	}
}

func TestEditorPrincipalUsesPositiveToolAllowlist(t *testing.T) {
	expectedAllowlist := map[string]struct{}{
		"get_system_status": {}, "get_user_metrics": {}, "get_active_users": {}, "get_limits_status": {},
		"get_configuration": {}, "get_configuration_editor": {},
		"update_configuration": {}, "update_cpu_points": {}, "get_cpu_report": {}, "get_mem_report": {},
		"get_control_history": {}, "get_user_filters": {}, "validate_user_filter_pattern": {},
		"get_user_history": {}, "get_system_history": {}, "get_user_summary": {}, "get_metrics_database_info": {},
	}
	if len(editorToolAllowlist) != len(expectedAllowlist) {
		t.Fatalf("editor allowlist size = %d, want %d", len(editorToolAllowlist), len(expectedAllowlist))
	}
	for name := range expectedAllowlist {
		if _, ok := editorToolAllowlist[name]; !ok {
			t.Errorf("editor allowlist omits %s", name)
		}
	}
	for _, name := range []string{"get_configuration_editor", "update_configuration", "get_system_status"} {
		t.Run("allowed_"+name, func(t *testing.T) {
			called := false
			handler := authorizeEditorPrincipalMiddleware(func(_ context.Context, _ string, _ sdkmcp.Request) (sdkmcp.Result, error) {
				called = true
				return &sdkmcp.CallToolResult{}, nil
			})
			ctx := context.WithValue(context.Background(), principalContextKey{}, principalEditor)
			_, err := handler(ctx, "tools/call", &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: name}})
			if err != nil || !called {
				t.Fatalf("allowed tool = (%t, %v), want true, nil", called, err)
			}
		})
	}
	for _, name := range []string{"activate_limits", "deactivate_limits", "set_user_include_list", "future_write_tool"} {
		t.Run("denied_"+name, func(t *testing.T) {
			called := false
			handler := authorizeEditorPrincipalMiddleware(func(_ context.Context, _ string, _ sdkmcp.Request) (sdkmcp.Result, error) {
				called = true
				return &sdkmcp.CallToolResult{}, nil
			})
			ctx := context.WithValue(context.Background(), principalContextKey{}, principalEditor)
			_, err := handler(ctx, "tools/call", &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: name}})
			if err == nil || called {
				t.Fatalf("denied tool = (%t, %v), want false, error", called, err)
			}
		})
	}
}

func TestMCPHTTPServerAllowsLongLivedResponses(t *testing.T) {
	server := newMCPHTTPServer("127.0.0.1:0", http.NewServeMux(), &tls.Config{MinVersion: tls.VersionTLS13})

	if server.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want 0 for streaming responses", server.WriteTimeout)
	}
	if server.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %s, want a positive slow-loris deadline", server.ReadHeaderTimeout)
	}
}

func configureMCPTestTLS(t testing.TB, cfg *config.Config) string {
	t.Helper()
	testServer := httptest.NewTLSServer(nil)
	t.Cleanup(testServer.Close)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: testServer.Certificate().Raw})
	keyDER, err := x509.MarshalPKCS8PrivateKey(testServer.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatalf("encode test TLS key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	for path, contents := range map[string][]byte{certFile: certPEM, keyFile: keyPEM} {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatalf("write test TLS material %s: %v", path, err)
		}
	}

	cfg.MCPTLSEnabled = true
	cfg.MCPTLSCertFile = certFile
	cfg.MCPTLSKeyFile = keyFile
	cfg.MCPTLSCAFile = ""
	cfg.MCPTLSMinVersion = "1.3"
	return certFile
}
