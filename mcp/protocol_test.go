package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	resmanmetrics "github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
	sdkjsonrpc "github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const protocolTestToken = "protocol-test-token"

type protocolTestResponse struct {
	JSONRPC string `json:"jsonrpc"`
	Result  struct {
		SupportedVersions []string       `json:"supportedVersions"`
		Meta              map[string]any `json:"_meta"`
	} `json:"result"`
	Error *sdkjsonrpc.Error `json:"error"`
}

func TestValidateLatestOnlyMCPRequest(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		version  string
		wantCode int64
	}{
		{name: "latest request", method: "tools/list", version: mcpProtocolVersion},
		{name: "missing revision", method: "tools/list", wantCode: sdkmcp.CodeUnsupportedProtocolVersion},
		{name: "pre-2026 revision", method: "tools/list", version: "2025-11-25", wantCode: sdkmcp.CodeUnsupportedProtocolVersion},
		{name: "initialize removed", method: mcpMethodInitialize, version: mcpProtocolVersion, wantCode: sdkjsonrpc.CodeMethodNotFound},
		{name: "initialized removed", method: mcpNotificationInitialized, version: mcpProtocolVersion, wantCode: sdkjsonrpc.CodeMethodNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLatestOnlyMCPRequest(tt.method, tt.version)
			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("validateLatestOnlyMCPRequest() error = %v", err)
				}
				return
			}
			var protocolErr *sdkjsonrpc.Error
			if !errors.As(err, &protocolErr) {
				t.Fatalf("validateLatestOnlyMCPRequest() error = %T %v, want JSON-RPC error", err, err)
			}
			if protocolErr.Code != tt.wantCode {
				t.Fatalf("error code = %d, want %d", protocolErr.Code, tt.wantCode)
			}
			if protocolErr.Code == sdkmcp.CodeUnsupportedProtocolVersion {
				var data sdkmcp.UnsupportedProtocolVersionData
				if err := json.Unmarshal(protocolErr.Data, &data); err != nil {
					t.Fatalf("decode unsupported-version data: %v", err)
				}
				if len(data.Supported) != 1 || data.Supported[0] != mcpProtocolVersion {
					t.Fatalf("supported versions = %v, want [%s]", data.Supported, mcpProtocolVersion)
				}
			}
		})
	}
}

func TestLatestOnlyHTTPConformance(t *testing.T) {
	server := newProtocolTestServer(t)
	handler := server.newMCPHTTPHandler()

	tests := []struct {
		name       string
		method     string
		params     map[string]any
		mutate     func(*http.Request)
		wantStatus int
		wantCode   int64
	}{
		{
			name:       "discovery",
			method:     mcpMethodDiscover,
			params:     latestProtocolParams(nil),
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing protocol header",
			method:     mcpMethodDiscover,
			params:     latestProtocolParams(nil),
			mutate:     func(r *http.Request) { r.Header.Del(mcpProtocolVersionHeader) },
			wantStatus: http.StatusBadRequest,
			wantCode:   sdkmcp.CodeUnsupportedProtocolVersion,
		},
		{
			name:       "GET is not a protocol transport",
			method:     mcpMethodDiscover,
			params:     latestProtocolParams(nil),
			mutate:     func(r *http.Request) { r.Method = http.MethodGet },
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   sdkjsonrpc.CodeInvalidRequest,
		},
		{
			name:   "pre-2026 revision",
			method: mcpMethodDiscover,
			params: protocolParamsForVersion("2025-11-25", nil),
			mutate: func(r *http.Request) {
				r.Header.Set(mcpProtocolVersionHeader, "2025-11-25")
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   sdkmcp.CodeUnsupportedProtocolVersion,
		},
		{
			name:   "legacy session identifier",
			method: mcpMethodDiscover,
			params: latestProtocolParams(nil),
			mutate: func(r *http.Request) {
				r.Header.Set(mcpSessionIDHeader, "legacy-session")
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   sdkjsonrpc.CodeInvalidRequest,
		},
		{
			name:   "legacy resumability header",
			method: mcpMethodDiscover,
			params: latestProtocolParams(nil),
			mutate: func(r *http.Request) {
				r.Header.Set(mcpLastEventIDHeader, "legacy-event")
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   sdkjsonrpc.CodeInvalidRequest,
		},
		{
			name:   "initialize removed",
			method: mcpMethodInitialize,
			params: latestProtocolParams(map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "legacy", "version": "1"},
			}),
			wantStatus: http.StatusNotFound,
			wantCode:   sdkjsonrpc.CodeMethodNotFound,
		},
		{
			name:       "initialized removed",
			method:     mcpNotificationInitialized,
			params:     latestProtocolParams(nil),
			wantStatus: http.StatusNotFound,
			wantCode:   sdkjsonrpc.CodeMethodNotFound,
		},
		{
			name:   "missing client capabilities metadata",
			method: mcpMethodDiscover,
			params: map[string]any{
				"_meta": map[string]any{
					sdkmcp.MetaKeyProtocolVersion: mcpProtocolVersion,
				},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   sdkjsonrpc.CodeInvalidParams,
		},
		{
			name:       "missing method header",
			method:     mcpMethodDiscover,
			params:     latestProtocolParams(nil),
			mutate:     func(r *http.Request) { r.Header.Del(mcpMethodHeader) },
			wantStatus: http.StatusBadRequest,
			wantCode:   sdkmcp.CodeHeaderMismatch,
		},
		{
			name:       "method header mismatch",
			method:     mcpMethodDiscover,
			params:     latestProtocolParams(nil),
			mutate:     func(r *http.Request) { r.Header.Set(mcpMethodHeader, "tools/list") },
			wantStatus: http.StatusBadRequest,
			wantCode:   sdkmcp.CodeHeaderMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := newProtocolHTTPRequest(t, tt.method, tt.params)
			if tt.mutate != nil {
				tt.mutate(request)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("HTTP status = %d, want %d; body: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			var response protocolTestResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v; body: %s", err, recorder.Body.String())
			}
			if tt.wantCode == 0 {
				if response.Error != nil {
					t.Fatalf("unexpected protocol error: %+v", response.Error)
				}
				if len(response.Result.SupportedVersions) != 1 || response.Result.SupportedVersions[0] != mcpProtocolVersion {
					t.Fatalf("discovery versions = %v, want [%s]", response.Result.SupportedVersions, mcpProtocolVersion)
				}
				if recorder.Header().Get(mcpSessionIDHeader) != "" {
					t.Fatalf("stateless response set %s", mcpSessionIDHeader)
				}
				if response.Result.Meta[sdkmcp.MetaKeyServerInfo] == nil {
					t.Fatalf("response is missing %s metadata", sdkmcp.MetaKeyServerInfo)
				}
				return
			}
			if response.Error == nil || response.Error.Code != tt.wantCode {
				t.Fatalf("protocol error = %+v, want code %d", response.Error, tt.wantCode)
			}
		})
	}
}

func TestMCPHTTPAuthenticationPrecedesProtocolValidation(t *testing.T) {
	server := newProtocolTestServer(t)
	handler := server.newMCPHTTPHandler()

	tests := []struct {
		name    string
		version string
	}{
		{name: "current protocol", version: mcpProtocolVersion},
		{name: "unsupported protocol", version: "2025-11-25"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := newProtocolHTTPRequest(t, mcpMethodDiscover, protocolParamsForVersion(tt.version, nil))
			request.Header.Del("Authorization")
			request.Header.Set(mcpProtocolVersionHeader, tt.version)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("HTTP status = %d, want 401; body: %s", recorder.Code, recorder.Body.String())
			}
			body := strings.ToLower(recorder.Body.String())
			if !strings.Contains(body, "missing authorization header") {
				t.Fatalf("unauthenticated response did not come from authentication middleware: %s", recorder.Body.String())
			}
			for _, detail := range []string{
				strings.ToLower(mcpProtocolVersion),
				strings.ToLower(tt.version),
				"protocol",
				"supported",
				"requested",
			} {
				if strings.Contains(body, detail) {
					t.Errorf("unauthenticated response disclosed protocol detail %q: %s", detail, recorder.Body.String())
				}
			}
		})
	}
}

func TestLatestOnlyHTTPIsStatelessAcrossInstances(t *testing.T) {
	servers := []*Server{newProtocolTestServer(t), newProtocolTestServer(t)}
	for index, server := range servers {
		request := newProtocolHTTPRequest(t, "tools/list", latestProtocolParams(nil))
		recorder := httptest.NewRecorder()
		server.newMCPHTTPHandler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("instance %d status = %d, want 200; body: %s", index, recorder.Code, recorder.Body.String())
		}
		if recorder.Header().Get(mcpSessionIDHeader) != "" {
			t.Fatalf("instance %d set a session identifier", index)
		}
	}
}

func TestLatestOnlyHTTPListsAllFeatureClasses(t *testing.T) {
	server := newProtocolTestServer(t)
	handler := server.newMCPHTTPHandler()
	for _, method := range []string{"tools/list", "resources/list", "prompts/list"} {
		t.Run(method, func(t *testing.T) {
			request := newProtocolHTTPRequest(t, method, latestProtocolParams(nil))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s status = %d, want 200; body: %s", method, recorder.Code, recorder.Body.String())
			}
			var response struct {
				Result json.RawMessage   `json:"result"`
				Error  *sdkjsonrpc.Error `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode %s response: %v", method, err)
			}
			if response.Error != nil || len(response.Result) == 0 {
				t.Fatalf("%s response = result %s, error %+v", method, response.Result, response.Error)
			}
		})
	}
}

func TestLatestOnlyHTTPRejectsBatchAndOversizedBodies(t *testing.T) {
	server := newProtocolTestServer(t)
	handler := server.newMCPHTTPHandler()

	t.Run("batch", func(t *testing.T) {
		body := []byte(`[{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}]`)
		request := newRawProtocolHTTPRequest(t, mcpMethodDiscover, body)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("batch status = %d, want 400; body: %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("body limit", func(t *testing.T) {
		body := bytes.Repeat([]byte(" "), mcpDefaultMaxRequestBodySize+1)
		request := newRawProtocolHTTPRequest(t, mcpMethodDiscover, body)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized status = %d, want 413; body: %s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestLatestOnlyHTTPPropagatesCancellation(t *testing.T) {
	server := newProtocolTestServer(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server.mcpServer.AddTool(&sdkmcp.Tool{
		Name:        "wait_for_cancellation",
		Description: "Wait until the request is cancelled",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})

	request := newProtocolHTTPRequest(t, "tools/call", latestProtocolParams(map[string]any{
		"name":      "wait_for_cancellation",
		"arguments": map[string]any{},
	}))
	request.Header.Set("Mcp-Name", "wait_for_cancellation")
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.newMCPHTTPHandler().ServeHTTP(httptest.NewRecorder(), request)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool handler did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request cancellation did not reach the tool handler")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler did not return after cancellation")
	}
}

func TestLatestOnlyStdioConformance(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		params        map[string]any
		wantCode      int64
		wantError     bool
		wantDiscovery bool
	}{
		{name: "discovery", method: mcpMethodDiscover, params: latestProtocolParams(nil), wantDiscovery: true},
		{name: "latest tools request", method: "tools/list", params: latestProtocolParams(nil)},
		{
			name:   "legacy initialize",
			method: mcpMethodInitialize,
			params: map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "legacy", "version": "1"},
			},
			wantCode: sdkjsonrpc.CodeMethodNotFound,
		},
		{
			name:      "pre-2026 request",
			method:    "tools/list",
			params:    protocolParamsForVersion("2025-11-25", nil),
			wantError: true,
		},
		{
			name:      "missing protocol metadata",
			method:    "tools/list",
			params:    map[string]any{},
			wantError: true,
		},
		{
			name:     "initialized removed",
			method:   mcpNotificationInitialized,
			params:   latestProtocolParams(nil),
			wantCode: sdkjsonrpc.CodeMethodNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newProtocolTestServer(t)
			clientWriter, clientReader, closeTransport := connectRawStdio(t, server.mcpServer)
			defer closeTransport()

			request := map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  tt.method,
				"params":  tt.params,
			}
			if err := json.NewEncoder(clientWriter).Encode(request); err != nil {
				t.Fatalf("write stdio request: %v", err)
			}
			var response protocolTestResponse
			if err := json.NewDecoder(clientReader).Decode(&response); err != nil {
				t.Fatalf("read stdio response: %v", err)
			}
			if tt.wantCode == 0 && !tt.wantError {
				if response.Error != nil {
					t.Fatalf("unexpected stdio protocol error: %+v", response.Error)
				}
				if tt.wantDiscovery && (len(response.Result.SupportedVersions) != 1 || response.Result.SupportedVersions[0] != mcpProtocolVersion) {
					t.Fatalf("stdio discovery versions = %v, want [%s]", response.Result.SupportedVersions, mcpProtocolVersion)
				}
				return
			}
			if tt.wantError {
				if response.Error == nil {
					t.Fatal("stdio request unexpectedly succeeded")
				}
				return
			}
			if response.Error == nil || response.Error.Code != tt.wantCode {
				t.Fatalf("stdio protocol error = %+v, want code %d", response.Error, tt.wantCode)
			}
		})
	}
}

func TestMCPStatusContractsAgreeAcrossSurfacesAndTransports(t *testing.T) {
	server := newStatusProtocolTestServer(t)

	httpTool := callStatusOverHTTP(t, server, "tools/call", "get_system_status", map[string]any{
		"name":      "get_system_status",
		"arguments": map[string]any{},
	})
	stdioTool := callStatusOverStdio(t, server, "tools/call", map[string]any{
		"name":      "get_system_status",
		"arguments": map[string]any{},
	})
	assertCurrentStatusFields(t, httpTool)
	assertCurrentStatusFields(t, stdioTool)
	if got, want := sortedMapKeys(httpTool), sortedMapKeys(stdioTool); !slices.Equal(got, want) {
		t.Fatalf("HTTP status fields = %v, stdio fields = %v", got, want)
	}

	resourceResult := callStatusOverHTTP(t, server, "resources/read", "resman://system/status", map[string]any{
		"uri": "resman://system/status",
	})
	assertCurrentStatusFields(t, resourceResult)
	if got, want := sortedMapKeys(resourceResult), sortedMapKeys(httpTool); !slices.Equal(got, want) {
		t.Fatalf("resource status fields = %v, tool fields = %v", got, want)
	}

	promptText := callPromptOverHTTP(t, server, "system-health")
	for _, term := range []string{"Observed Users CPU", "Observed Users", "Actively Limited Users", "CPU Limits Active", "Resource Limits Active"} {
		if !strings.Contains(promptText, term) {
			t.Errorf("system-health prompt is missing %q: %s", term, promptText)
		}
	}
}

func TestMCPSharedWireContractsAgreeAcrossSurfacesAndTransports(t *testing.T) {
	server := newStatusProtocolTestServer(t)

	tests := []struct {
		name         string
		tool         string
		resource     string
		expectedKeys []string
	}{
		{
			name:         "active users",
			tool:         "get_active_users",
			resource:     "resman://users/active",
			expectedKeys: []string{"hostname", "server_role", "users"},
		},
		{
			name:     "resource policy configuration",
			tool:     "get_configuration",
			resource: "resman://config",
			expectedKeys: []string{
				"cpu_quota_normal", "cpu_release_threshold", "cpu_threshold", "cpu_threshold_duration",
				"disable_swap", "enable_prometheus", "hostname", "ignore_system_load", "io_device_filter",
				"io_enabled", "io_read_bps", "io_read_iops", "io_release_threshold", "io_threshold",
				"io_threshold_duration", "io_write_bps", "io_write_iops", "min_system_cores", "polling_interval",
				"prometheus_port", "ram_enabled", "ram_high_ratio", "ram_quota_per_user", "ram_release_threshold",
				"ram_threshold", "server_role", "system_uid_max", "system_uid_min",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpTool := callStatusOverHTTP(t, server, "tools/call", tt.tool, map[string]any{
				"name": tt.tool, "arguments": map[string]any{},
			})
			stdioTool := callStatusOverStdio(t, server, "tools/call", map[string]any{
				"name": tt.tool, "arguments": map[string]any{},
			})
			httpResource := callStatusOverHTTP(t, server, "resources/read", tt.resource, map[string]any{"uri": tt.resource})
			stdioResource := callStatusOverStdio(t, server, "resources/read", map[string]any{"uri": tt.resource})

			slices.Sort(tt.expectedKeys)
			for surface, payload := range map[string]map[string]any{
				"HTTP tool": httpTool, "stdio tool": stdioTool,
				"HTTP resource": httpResource, "stdio resource": stdioResource,
			} {
				if got := sortedMapKeys(payload); !slices.Equal(got, tt.expectedKeys) {
					t.Errorf("%s keys = %v, want %v", surface, got, tt.expectedKeys)
				}
			}
			if !reflect.DeepEqual(httpTool, stdioTool) || !reflect.DeepEqual(httpTool, httpResource) || !reflect.DeepEqual(httpTool, stdioResource) {
				t.Fatalf("shared contract differs: HTTP tool=%+v stdio tool=%+v HTTP resource=%+v stdio resource=%+v",
					httpTool, stdioTool, httpResource, stdioResource)
			}
		})
	}
}

func TestMCPToolOnlyWireContractsAgreeAcrossTransports(t *testing.T) {
	server := newStatusProtocolTestServer(t)
	tests := []struct {
		tool string
		keys []string
	}{
		{tool: "get_cpu_report", keys: []string{"avg_cpu", "cpu_actively_limited_users_count", "cpu_limits_active", "hostname", "observed_users_count", "peak_cpu", "report", "server_role", "total_cpu"}},
		{tool: "get_mem_report", keys: []string{"avg_memory_mb", "hostname", "observed_users_count", "peak_memory_mb", "ram_actively_limited_users_count", "report", "resource_limits_active", "server_role", "total_memory_mb"}},
		{tool: "get_user_filters", keys: []string{"config_file", "user_exclude_list", "user_include_list"}},
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			httpResult := callStatusOverHTTP(t, server, "tools/call", tt.tool, map[string]any{
				"name": tt.tool, "arguments": map[string]any{},
			})
			stdioResult := callStatusOverStdio(t, server, "tools/call", map[string]any{
				"name": tt.tool, "arguments": map[string]any{},
			})
			slices.Sort(tt.keys)
			if got := sortedMapKeys(httpResult); !slices.Equal(got, tt.keys) {
				t.Errorf("HTTP keys = %v, want %v", got, tt.keys)
			}
			if got := sortedMapKeys(stdioResult); !slices.Equal(got, tt.keys) {
				t.Errorf("stdio keys = %v, want %v", got, tt.keys)
			}
		})
	}
}

func newStatusProtocolTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.DefaultConfig()
	configureMCPTestTLS(t, cfg)
	cfg.MCPEnabled = true
	cfg.MCPTransport = "http"
	cfg.MCPHTTPHost = "127.0.0.1"
	cfg.MCPAuthToken = protocolTestToken
	collector, err := resmanmetrics.NewCollector(cfg)
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}
	manager, err := state.NewManager(cfg, collector, nil, nil)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	server, err := NewServer(cfg, manager, collector, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func callStatusOverHTTP(t *testing.T, server *Server, method, name string, fields map[string]any) map[string]any {
	t.Helper()
	request := newProtocolHTTPRequest(t, method, latestProtocolParams(fields))
	request.Header.Set("Mcp-Name", name)
	recorder := httptest.NewRecorder()
	server.newMCPHTTPHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s HTTP status = %d, want 200; body: %s", method, recorder.Code, recorder.Body.String())
	}
	return decodeStatusResult(t, method, recorder.Body.Bytes())
}

func callStatusOverStdio(t *testing.T, server *Server, method string, fields map[string]any) map[string]any {
	t.Helper()
	clientWriter, clientReader, closeTransport := connectRawStdio(t, server.mcpServer)
	defer closeTransport()
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  latestProtocolParams(fields),
	}
	if err := json.NewEncoder(clientWriter).Encode(request); err != nil {
		t.Fatalf("write stdio status request: %v", err)
	}
	var response json.RawMessage
	if err := json.NewDecoder(clientReader).Decode(&response); err != nil {
		t.Fatalf("read stdio status response: %v", err)
	}
	return decodeStatusResult(t, method, response)
}

func decodeStatusResult(t *testing.T, method string, response []byte) map[string]any {
	t.Helper()
	var wire struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
			Contents          []struct {
				Text string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
		Error *sdkjsonrpc.Error `json:"error"`
	}
	if err := json.Unmarshal(response, &wire); err != nil {
		t.Fatalf("decode %s response: %v; body: %s", method, err, response)
	}
	if wire.Error != nil {
		t.Fatalf("%s protocol error: %+v", method, wire.Error)
	}
	if wire.Result.StructuredContent != nil {
		return wire.Result.StructuredContent
	}
	if len(wire.Result.Contents) != 1 {
		t.Fatalf("%s content count = %d, want 1; body: %s", method, len(wire.Result.Contents), response)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(wire.Result.Contents[0].Text), &result); err != nil {
		t.Fatalf("decode %s resource text: %v", method, err)
	}
	return result
}

func callPromptOverHTTP(t *testing.T, server *Server, name string) string {
	t.Helper()
	request := newProtocolHTTPRequest(t, "prompts/get", latestProtocolParams(map[string]any{
		"name":      name,
		"arguments": map[string]any{},
	}))
	request.Header.Set("Mcp-Name", name)
	recorder := httptest.NewRecorder()
	server.newMCPHTTPHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("prompts/get HTTP status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	var wire struct {
		Result struct {
			Messages []struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"result"`
		Error *sdkjsonrpc.Error `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode prompt response: %v", err)
	}
	if wire.Error != nil || len(wire.Result.Messages) != 1 {
		t.Fatalf("prompt response error = %+v, messages = %d", wire.Error, len(wire.Result.Messages))
	}
	return wire.Result.Messages[0].Content.Text
}

func assertCurrentStatusFields(t *testing.T, status map[string]any) {
	t.Helper()
	for _, key := range []string{
		"observed_users_cpu_usage",
		"observed_users_count",
		"actively_limited_users_count",
		"cpu_limits_active",
		"resource_limits_active",
	} {
		if _, exists := status[key]; !exists {
			t.Errorf("status is missing %q: %+v", key, status)
		}
	}
	for _, key := range []string{"total_user_cpu_usage", "user_cpu_usage", "active_users_count", "limits_active", "limits_applied_time"} {
		if _, exists := status[key]; exists {
			t.Errorf("status contains removed field %q: %+v", key, status)
		}
	}
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func newProtocolTestServer(t testing.TB) *Server {
	t.Helper()
	cfg := config.DefaultConfig()
	configureMCPTestTLS(t, cfg)
	cfg.MCPEnabled = true
	cfg.MCPTransport = "http"
	cfg.MCPHTTPHost = "127.0.0.1"
	cfg.MCPAuthToken = protocolTestToken
	server, err := NewServer(cfg, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func latestProtocolParams(fields map[string]any) map[string]any {
	return protocolParamsForVersion(mcpProtocolVersion, fields)
}

func protocolParamsForVersion(version string, fields map[string]any) map[string]any {
	params := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		params[key] = value
	}
	params["_meta"] = map[string]any{
		sdkmcp.MetaKeyProtocolVersion:    version,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{},
		sdkmcp.MetaKeyClientInfo: map[string]any{
			"name":    "resman-protocol-test",
			"version": "1",
		},
	}
	return params
}

func newProtocolHTTPRequest(t *testing.T, method string, params map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return newRawProtocolHTTPRequest(t, method, body)
}

func newRawProtocolHTTPRequest(t *testing.T, method string, body []byte) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+protocolTestToken)
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(mcpProtocolVersionHeader, mcpProtocolVersion)
	request.Header.Set(mcpMethodHeader, method)
	return request
}

func connectRawStdio(t *testing.T, server *sdkmcp.Server) (io.Writer, io.Reader, func()) {
	t.Helper()
	serverReader, clientWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create client-to-server pipe: %v", err)
	}
	clientReader, serverWriter, err := os.Pipe()
	if err != nil {
		closeProtocolTestPipe(t, "server reader", serverReader)
		closeProtocolTestPipe(t, "client writer", clientWriter)
		t.Fatalf("create server-to-client pipe: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	session, err := server.Connect(ctx, &sdkmcp.IOTransport{
		Reader: serverReader,
		Writer: serverWriter,
	}, nil)
	if err != nil {
		cancel()
		closeProtocolTestPipe(t, "server reader", serverReader)
		closeProtocolTestPipe(t, "server writer", serverWriter)
		closeProtocolTestPipe(t, "client reader", clientReader)
		closeProtocolTestPipe(t, "client writer", clientWriter)
		t.Fatalf("connect stdio server: %v", err)
	}
	cleanup := func() {
		cancel()
		closeProtocolTestPipe(t, "client writer", clientWriter)
		closeProtocolTestPipe(t, "client reader", clientReader)
		if err := session.Close(); err != nil && !errors.Is(err, sdkmcp.ErrConnectionClosed) {
			t.Errorf("close stdio server session: %v", err)
		}
	}
	return clientWriter, clientReader, cleanup
}

func closeProtocolTestPipe(t *testing.T, name string, pipe *os.File) {
	t.Helper()
	if err := pipe.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close %s: %v", name, err)
	}
}
