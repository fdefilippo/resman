package metrics

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/golang-jwt/jwt/v5"
)

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

func writeCredentialFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("failed to write credential file: %v", err)
	}
	return path
}
