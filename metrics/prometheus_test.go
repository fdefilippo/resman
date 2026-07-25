package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/golang-jwt/jwt/v5"
)

func TestParseTLSVersion(t *testing.T) {
	tests := []struct {
		value string
		want  uint16
	}{
		{value: "1.0", want: tls.VersionTLS10},
		{value: "1.1", want: tls.VersionTLS11},
		{value: "1.2", want: tls.VersionTLS12},
		{value: "1.3", want: tls.VersionTLS13},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseTLSVersion(tt.value)
			if err != nil {
				t.Fatalf("parseTLSVersion(%q) error: %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("parseTLSVersion(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}

	if _, err := parseTLSVersion("SSLv3"); err == nil {
		t.Fatal("parseTLSVersion() accepted an unsupported version")
	}
}

func TestGetMetricsEndpointUsesConfiguredScheme(t *testing.T) {
	cfg := config.DefaultConfig()
	exporter := &PrometheusExporter{cfg: cfg}
	if got := exporter.GetMetricsEndpoint(); got != "http://127.0.0.1:1974/metrics" {
		t.Fatalf("HTTP metrics endpoint = %q", got)
	}

	cfg.PrometheusTLSEnabled = true
	if got := exporter.GetMetricsEndpoint(); got != "https://127.0.0.1:1974/metrics" {
		t.Fatalf("HTTPS metrics endpoint = %q", got)
	}
}

func TestPrometheusStartReportsBindFailureSynchronously(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve test port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusMetricsBindHost = "127.0.0.1"
	cfg.PrometheusMetricsBindPort = listener.Addr().(*net.TCPAddr).Port
	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}

	if err := exporter.Start(context.Background()); err == nil {
		t.Fatal("Start() succeeded on an occupied port")
	}
	if exporter.IsRunning() {
		t.Fatal("exporter remained marked running after bind failure")
	}
}

func TestNewPrometheusExporterAppliesTLSAndClientCA(t *testing.T) {
	certFile, keyFile, caFile := writeTestTLSMaterial(t)

	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusTLSEnabled = true
	cfg.PrometheusTLSCertFile = certFile
	cfg.PrometheusTLSKeyFile = keyFile
	cfg.PrometheusTLSCAFile = caFile
	cfg.PrometheusTLSMinVersion = "1.3"

	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}
	if exporter.tlsConfig == nil {
		t.Fatal("TLS configuration was not created")
	}
	if exporter.tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("TLS minimum version = %d, want TLS 1.3", exporter.tlsConfig.MinVersion)
	}
	if exporter.tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("TLS client authentication = %d, want RequireAndVerifyClientCert", exporter.tlsConfig.ClientAuth)
	}
	if exporter.tlsConfig.ClientCAs == nil {
		t.Fatal("TLS client CA pool was not configured")
	}
}

func TestNewPrometheusExporterRejectsInvalidClientCA(t *testing.T) {
	certFile, keyFile, _ := writeTestTLSMaterial(t)
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusTLSEnabled = true
	cfg.PrometheusTLSCertFile = certFile
	cfg.PrometheusTLSKeyFile = keyFile
	cfg.PrometheusTLSCAFile = writeCredentialFile(t, "ca.crt", "not a certificate")

	if exporter, err := NewPrometheusExporter(cfg); err == nil || exporter != nil {
		t.Fatalf("NewPrometheusExporter() = (%v, %v), want nil exporter and error", exporter, err)
	}
}

func TestNewPrometheusExporterRejectsInvalidTLSKeyPair(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusTLSEnabled = true
	cfg.PrometheusTLSCertFile = writeCredentialFile(t, "server.crt", "not a certificate")
	cfg.PrometheusTLSKeyFile = writeCredentialFile(t, "server.key", "not a key")

	if exporter, err := NewPrometheusExporter(cfg); err == nil || exporter != nil {
		t.Fatalf("NewPrometheusExporter() = (%v, %v), want nil exporter and error", exporter, err)
	}
}

func writeTestTLSMaterial(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	testServer := httptest.NewTLSServer(nil)
	t.Cleanup(testServer.Close)

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: testServer.Certificate().Raw,
	})
	keyDER, err := x509.MarshalPKCS8PrivateKey(testServer.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatalf("failed to encode test TLS key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	caFile = filepath.Join(dir, "ca.crt")
	for path, contents := range map[string][]byte{
		certFile: certPEM,
		keyFile:  keyPEM,
		caFile:   certPEM,
	} {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatalf("failed to write TLS material %s: %v", path, err)
		}
	}
	return certFile, keyFile, caFile
}

func TestNewPrometheusExporterRejectsInvalidCredentials(t *testing.T) {
	tests := []struct {
		name      string
		authType  string
		username  string
		password  string
		jwtSecret string
	}{
		{name: "missing basic password file", authType: "basic", username: "prometheus"},
		{name: "missing basic username", authType: "basic", password: "secret"},
		{name: "empty basic password", authType: "basic", username: "prometheus", password: " \n"},
		{name: "missing JWT secret file", authType: "jwt"},
		{name: "empty JWT secret", authType: "jwt", jwtSecret: " \n"},
		{name: "both requires basic credentials", authType: "both", jwtSecret: "jwt-secret"},
		{name: "both requires JWT credentials", authType: "both", username: "prometheus", password: "secret"},
		{name: "unsupported auth type", authType: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.EnablePrometheus = true
			cfg.PrometheusAuthType = tt.authType
			cfg.PrometheusAuthUsername = tt.username
			if tt.password != "" {
				cfg.PrometheusAuthPasswordFile = writeCredentialFile(t, "password", tt.password)
			}
			if tt.jwtSecret != "" {
				cfg.PrometheusJWTSecretFile = writeCredentialFile(t, "jwt", tt.jwtSecret)
			}

			if exporter, err := NewPrometheusExporter(cfg); err == nil || exporter != nil {
				t.Fatalf("NewPrometheusExporter() = (%v, %v), want nil exporter and error", exporter, err)
			}
		})
	}
}

func TestNewPrometheusExporterRejectsUnreadableCredentialFile(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusAuthType = "basic"
	cfg.PrometheusAuthUsername = "prometheus"
	cfg.PrometheusAuthPasswordFile = filepath.Join(t.TempDir(), "missing")

	if exporter, err := NewPrometheusExporter(cfg); err == nil || exporter != nil {
		t.Fatalf("NewPrometheusExporter() = (%v, %v), want nil exporter and error", exporter, err)
	}
}

func TestCheckJWTAuthRequiresExpiration(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PrometheusAuthType = "jwt"
	cfg.PrometheusJWTIssuer = "resman"
	cfg.PrometheusJWTAudience = "prometheus"
	secret := []byte("test-secret")
	exporter := &PrometheusExporter{
		cfg:       cfg,
		logger:    logging.GetLogger(),
		jwtSecret: secret,
	}

	tests := []struct {
		name   string
		claims jwt.MapClaims
		want   bool
	}{
		{
			name: "missing expiration",
			claims: jwt.MapClaims{
				"iss": "resman",
				"aud": "prometheus",
			},
			want: false,
		},
		{
			name: "future expiration",
			claims: jwt.MapClaims{
				"iss": "resman",
				"aud": "prometheus",
				"exp": time.Now().Add(time.Hour).Unix(),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, tt.claims).SignedString(secret)
			if err != nil {
				t.Fatalf("failed to sign JWT: %v", err)
			}
			request := httptest.NewRequest("GET", "/metrics", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			if got := exporter.checkJWTAuth(request); got != tt.want {
				t.Fatalf("checkJWTAuth() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestAuthenticationChecksFailClosedWithoutLoadedSecrets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PrometheusAuthType = "basic"
	request := httptest.NewRequest("GET", "/metrics", nil)
	request.SetBasicAuth("", "")
	exporter := &PrometheusExporter{
		cfg:    cfg,
		logger: logging.GetLogger(),
	}
	if exporter.checkBasicAuth(request) {
		t.Fatal("empty Basic credentials were accepted")
	}

	cfg.PrometheusAuthType = "jwt"
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte{})
	if err != nil {
		t.Fatalf("failed to sign empty-secret JWT: %v", err)
	}
	request = httptest.NewRequest("GET", "/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	if exporter.checkJWTAuth(request) {
		t.Fatal("JWT signed with an empty secret was accepted")
	}
}

func TestCleanupUserMetricsRemovesCPUAverageAndEMASeries(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusAuthType = "none"

	exporter, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusExporter() error: %v", err)
	}
	exporter.UpdateUserMetrics(
		1000,
		"testuser",
		25,
		20,
		22,
		1024,
		2,
		false,
		"",
		"",
		0,
		0,
		0,
		0,
		0,
	)

	before, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() before cleanup error: %v", err)
	}
	beforeCounts := make(map[string]int, len(before))
	for _, family := range before {
		beforeCounts[family.GetName()] = len(family.Metric)
	}
	for _, name := range []string{
		"resman_user_cpu_usage_average_percent",
		"resman_user_cpu_usage_ema_percent",
	} {
		if beforeCounts[name] != 1 {
			t.Fatalf("%s series before cleanup = %d, want 1", name, beforeCounts[name])
		}
	}

	exporter.CleanupUserMetrics(map[int]bool{})
	after, err := exporter.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() after cleanup error: %v", err)
	}
	afterCounts := make(map[string]int, len(after))
	for _, family := range after {
		afterCounts[family.GetName()] = len(family.Metric)
	}
	for _, name := range []string{
		"resman_user_cpu_usage_average_percent",
		"resman_user_cpu_usage_ema_percent",
	} {
		if afterCounts[name] != 0 {
			t.Fatalf("%s series after cleanup = %d, want 0", name, afterCounts[name])
		}
	}
}

func writeCredentialFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("failed to write credential file: %v", err)
	}
	return path
}
