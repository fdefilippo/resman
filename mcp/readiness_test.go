package mcp

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/readiness"
)

func TestReadinessSeparatesAggregateDetailsAndLiveness(t *testing.T) {
	s := &Server{cfg: &config.MCPServerConfig{AuthToken: "operator", EditorAuthToken: "editor", Transport: "http"}, logger: &recordingServerLogger{}}
	for _, status := range []readiness.Status{readiness.NotReady, readiness.Ready, readiness.Degraded} {
		s.SetReadinessProvider(func() readiness.Snapshot {
			return readiness.Snapshot{Status: status, Database: readiness.Degraded, ConfigWatcher: readiness.Active, PSI: readiness.Disabled}
		})
		for _, tt := range []struct {
			name, path, token string
			handler           http.HandlerFunc
			want              int
			details           bool
		}{
			{"public aggregate", "/readyz", "", s.handleReadiness, 0, false},
			{"details reject anonymous", "/readyz/details", "", s.authMiddleware(s.handleReadinessDetails), 401, false},
			{"details reject wrong token", "/readyz/details", "wrong", s.authMiddleware(s.handleReadinessDetails), 401, false},
			{"operator details", "/readyz/details", "operator", s.authMiddleware(s.handleReadinessDetails), 0, true},
			{"editor read-only details", "/readyz/details", "editor", s.authMiddleware(s.handleReadinessDetails), 0, true},
			{"compatible liveness", "/health", "", s.handleHealthCheck, 200, false},
		} {
			t.Run(string(status)+"/"+tt.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, tt.path, nil)
				if tt.token != "" {
					req.Header.Set("Authorization", "Bearer "+tt.token)
				}
				w := httptest.NewRecorder()
				tt.handler(w, req)
				want := tt.want
				if want == 0 {
					want = 503
					if status != readiness.NotReady {
						want = 200
					}
				}
				if w.Code != want {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if want == 401 {
					return
				}
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if tt.path == "/readyz" && len(body) != 1 {
					t.Fatalf("public details leaked: %+v", body)
				}
				if tt.details && body["database"] != "degraded" {
					t.Fatalf("details=%+v", body)
				}
				if tt.path == "/health" && (len(body) != 2 || body["status"] != "healthy") {
					t.Fatalf("liveness changed: %+v", body)
				}
			})
		}
	}
}

func TestReadinessRoutesAreRegisteredOnHTTPSListener(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MCPEnabled = true
	cfg.MCPTransport = "http"
	cfg.MCPAuthToken = "operator"
	caFile := configureMCPTestTLS(t, cfg)
	s, err := NewServer(cfg, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.HTTPPort = 0
	var listener net.Listener
	s.httpListen = func(network, address string) (net.Listener, error) {
		var err error
		listener, err = net.Listen(network, address)
		return listener, err
	}
	s.SetReadinessProvider(func() readiness.Snapshot {
		return readiness.Snapshot{Status: readiness.Degraded, Database: readiness.Degraded}
	})
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	cert, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "example.com"}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	base := "https://127.0.0.1:" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	for _, tt := range []struct {
		path, token string
		want        int
	}{
		{"/health", "", 200}, {"/readyz", "", 200}, {"/readyz/details", "", 401}, {"/readyz/details", "operator", 200},
	} {
		req, err := http.NewRequest(http.MethodGet, base+tt.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tt.token != "" {
			req.Header.Set("Authorization", "Bearer "+tt.token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Fatalf("%s=%d want=%d", tt.path, resp.StatusCode, tt.want)
		}
	}
}
