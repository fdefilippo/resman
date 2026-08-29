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
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg == nil {
		t.Fatal("DefaultConfig() returned nil")
	}

	// Verify representative default values.
	tests := []struct {
		name     string
		got      interface{}
		expected interface{}
	}{
		{"CgroupRoot", cfg.CgroupRoot, "/sys/fs/cgroup"},
		{"ConfigFile", cfg.ConfigFile, DefaultConfigPath},
		{"CreatedCgroupsFile", cfg.CreatedCgroupsFile, DefaultCreatedCgroupsPath},
		{"MetricsDBPath", cfg.MetricsDBPath, DefaultMetricsDBPath},
		{"LogFile", cfg.LogFile, "/var/log/resman.log"},
		{"PollingInterval", cfg.PollingInterval, 30},
		{"MinActiveTime", cfg.MinActiveTime, 60},
		{"CPUThreshold", cfg.CPUThreshold, 75},
		{"CPUReleaseThreshold", cfg.CPUReleaseThreshold, 40},
		{"CPUQuotaNormal", cfg.CPUQuotaNormal, "max 100000"},
		{"EnablePrometheus", cfg.EnablePrometheus, false},
		{"PrometheusMetricsBindPort", cfg.PrometheusMetricsBindPort, 1974},
		{"PrometheusMetricsBindHost", cfg.PrometheusMetricsBindHost, "127.0.0.1"}, // Secure default
		{"MCPHTTPHost", cfg.MCPHTTPHost, "127.0.0.1"},
		{"MCPTLSEnabled", cfg.MCPTLSEnabled, true},
		{"MCPTLSCertFile", cfg.MCPTLSCertFile, "/etc/resman/tls/server.crt"},
		{"MCPTLSKeyFile", cfg.MCPTLSKeyFile, "/etc/resman/tls/server.key"},
		{"MCPTLSMinVersion", cfg.MCPTLSMinVersion, "1.3"},
		{"LogLevel", cfg.LogLevel, "INFO"},
		{"SystemUIDMin", cfg.SystemUIDMin, 1000},
		{"IgnoreSystemLoad", cfg.IgnoreSystemLoad, false},
		{"LimitHookEnabled", cfg.LimitHookEnabled, false},
		{"LimitHookTimeout", cfg.LimitHookTimeout, 10},
	}

	for _, tt := range tests {
		if tt.got != tt.expected {
			t.Errorf("%s: got %v, expected %v", tt.name, tt.got, tt.expected)
		}
	}
	if cfg.MCPTLSCertFile != cfg.PrometheusTLSCertFile || cfg.MCPTLSKeyFile != cfg.PrometheusTLSKeyFile {
		t.Fatal("MCP and Prometheus TLS defaults do not point to the same certificate and key files")
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *Config
		expectError bool
	}{
		{
			name: "valid config",
			cfg: &Config{
				CPUThreshold:           75,
				CPUReleaseThreshold:    40,
				PollingInterval:        30,
				MetricsCacheTTL:        15,
				MetricsRefreshInterval: 30,
				CgroupOperationTimeout: 5,
				MCPShutdownTimeout:     10,
				CPUQuotaNormal:         "max 100000",
				BatchNightRAMQuota:     "4G",
				InteractiveRAMQuota:    "1G",
				LogLevel:               "INFO",
				LogMaxSize:             10 * 1024 * 1024,
				SystemUIDMin:           1000,
				SystemUIDMax:           60000,
				MetricsDBRetentionDays: 30,
				MetricsDBWriteInterval: 30,
				UsernameCacheTTL:       60,
			},
			expectError: false,
		},
		{
			name: "CPU_THRESHOLD too low",
			cfg: &Config{
				CPUThreshold:        0,
				CPUReleaseThreshold: 40,
				PollingInterval:     30,
				LogLevel:            "INFO",
				SystemUIDMin:        1000,
				SystemUIDMax:        60000,
			},
			expectError: true,
		},
		{
			name: "CPU_THRESHOLD too high",
			cfg: &Config{
				CPUThreshold:        101,
				CPUReleaseThreshold: 40,
				PollingInterval:     30,
				LogLevel:            "INFO",
				SystemUIDMin:        1000,
				SystemUIDMax:        60000,
			},
			expectError: true,
		},
		{
			name: "CPU_RELEASE_THRESHOLD too low",
			cfg: &Config{
				CPUThreshold:        75,
				CPUReleaseThreshold: 0,
				PollingInterval:     30,
				LogLevel:            "INFO",
				SystemUIDMin:        1000,
				SystemUIDMax:        60000,
			},
			expectError: true,
		},
		{
			name: "CPU_THRESHOLD <= CPU_RELEASE_THRESHOLD",
			cfg: &Config{
				CPUThreshold:        40,
				CPUReleaseThreshold: 40,
				PollingInterval:     30,
				LogLevel:            "INFO",
				SystemUIDMin:        1000,
				SystemUIDMax:        60000,
			},
			expectError: true,
		},
		{
			name: "POLLING_INTERVAL too low",
			cfg: &Config{
				CPUThreshold:        75,
				CPUReleaseThreshold: 40,
				PollingInterval:     4,
				LogLevel:            "INFO",
				SystemUIDMin:        1000,
				SystemUIDMax:        60000,
			},
			expectError: true,
		},
		{
			name: "invalid LOG_LEVEL",
			cfg: &Config{
				CPUThreshold:        75,
				CPUReleaseThreshold: 40,
				PollingInterval:     30,
				LogLevel:            "INVALID",
				SystemUIDMin:        1000,
				SystemUIDMax:        60000,
			},
			expectError: true,
		},
		{
			name: "negative SYSTEM_UID_MIN",
			cfg: &Config{
				CPUThreshold:        75,
				CPUReleaseThreshold: 40,
				PollingInterval:     30,
				LogLevel:            "INFO",
				SystemUIDMin:        -1,
				SystemUIDMax:        60000,
			},
			expectError: true,
		},
		{
			name: "SYSTEM_UID_MAX < SYSTEM_UID_MIN",
			cfg: &Config{
				CPUThreshold:        75,
				CPUReleaseThreshold: 40,
				PollingInterval:     30,
				LogLevel:            "INFO",
				SystemUIDMin:        2000,
				SystemUIDMax:        1000,
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.cfg)
			if tt.expectError && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

func TestValidateConfigIODeviceFilter(t *testing.T) {
	tests := []struct {
		name        string
		filter      string
		expectError bool
	}{
		{name: "all devices", filter: "all"},
		{name: "empty uses all devices", filter: ""},
		{name: "SCSI disk", filter: "8:0"},
		{name: "NVMe disk", filter: "259:12"},
		{name: "device name", filter: "sda", expectError: true},
		{name: "missing minor", filter: "8:", expectError: true},
		{name: "missing major", filter: ":0", expectError: true},
		{name: "negative major", filter: "-8:0", expectError: true},
		{name: "extra component", filter: "8:0:1", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.IODeviceFilter = tt.filter

			err := validateConfig(cfg)
			if tt.expectError && err == nil {
				t.Fatalf("validateConfig() expected an error for IO_DEVICE_FILTER=%q", tt.filter)
			}
			if !tt.expectError && err != nil {
				t.Fatalf("validateConfig() unexpected error for IO_DEVICE_FILTER=%q: %v", tt.filter, err)
			}
		})
	}
}

func TestValidateConfigPrometheusTLSMinVersion(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "1.2", "1.3"} {
		t.Run(version, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.PrometheusTLSMinVersion = version
			if err := validateConfig(cfg); err != nil {
				t.Fatalf("validateConfig() rejected TLS version %q: %v", version, err)
			}
		})
	}

	cfg := DefaultConfig()
	cfg.PrometheusTLSMinVersion = "SSLv3"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("validateConfig() accepted an unsupported TLS version")
	}

	cfg = DefaultConfig()
	cfg.PrometheusTLSEnabled = true
	cfg.PrometheusTLSMinVersion = ""
	if err := validateConfig(cfg); err == nil {
		t.Fatal("validateConfig() accepted an empty TLS version while TLS is enabled")
	}
}

func TestIsValidCPUQuota(t *testing.T) {
	tests := []struct {
		name     string
		quota    string
		expected bool
	}{
		{"valid max format", "max 100000", true},
		{"valid numeric format", "50000 100000", true},
		{"missing period", "50000", false},
		{"invalid format", "invalid", false},
		{"empty string", "", false},
		{"three parts", "50000 100000 extra", false},
		{"max without period", "max", false},
		{"period without quota", " 100000", false},
		{"quota below kernel minimum", "999 100000", false},
		{"zero quota", "0 100000", false},
		{"negative quota", "-1000 100000", false},
		{"zero period", "50000 0", false},
		{"negative period", "50000 -1", false},
		{"max with zero period", "max 0", false},
		{"extra whitespace", " 50000   100000 ", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidCPUQuota(tt.quota)
			if got != tt.expected {
				t.Errorf("isValidCPUQuota(%q): got %v, expected %v", tt.quota, got, tt.expected)
			}
		})
	}
}

func TestParseByteQuota(t *testing.T) {
	maxUint64 := ^uint64(0)
	tests := []struct {
		name    string
		quota   string
		want    uint64
		wantErr bool
	}{
		{name: "plain bytes", quota: "4096", want: 4096},
		{name: "uppercase suffix", quota: "2G", want: 2 * 1024 * 1024 * 1024},
		{name: "lowercase suffix", quota: "512m", want: 512 * 1024 * 1024},
		{
			name:  "largest safe kilobyte value",
			quota: strconv.FormatUint(maxUint64/1024, 10) + "K",
			want:  (maxUint64 / 1024) * 1024,
		},
		{
			name:    "multiplication overflow",
			quota:   strconv.FormatUint(maxUint64, 10) + "K",
			wantErr: true,
		},
		{name: "invalid suffix", quota: "1P", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseByteQuota(tt.quota)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseByteQuota(%q) = %d, want error", tt.quota, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseByteQuota(%q) error: %v", tt.quota, err)
			}
			if got != tt.want {
				t.Fatalf("ParseByteQuota(%q) = %d, want %d", tt.quota, got, tt.want)
			}
		})
	}
}

func TestLoadFromFile(t *testing.T) {
	// Crea un file di configurazione temporaneo
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test.conf")

	configContent := `# Test configuration
CPU_THRESHOLD=80
CPU_RELEASE_THRESHOLD=50
POLLING_INTERVAL=60
LOG_LEVEL=DEBUG
ENABLE_PROMETHEUS=true
PROMETHEUS_METRICS_BIND_PORT=9102
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to create temp config file: %v", err)
	}

	cfg := DefaultConfig()
	err := loadFromFile(configPath, cfg)
	if err != nil {
		t.Fatalf("loadFromFile() error: %v", err)
	}

	// Verify loaded values.
	if cfg.CPUThreshold != 80 {
		t.Errorf("CPUThreshold: got %d, expected 80", cfg.CPUThreshold)
	}
	if cfg.CPUReleaseThreshold != 50 {
		t.Errorf("CPUReleaseThreshold: got %d, expected 50", cfg.CPUReleaseThreshold)
	}
	if cfg.PollingInterval != 60 {
		t.Errorf("PollingInterval: got %d, expected 60", cfg.PollingInterval)
	}
	if cfg.LogLevel != "DEBUG" {
		t.Errorf("LogLevel: got %s, expected DEBUG", cfg.LogLevel)
	}
	if !cfg.EnablePrometheus {
		t.Errorf("EnablePrometheus: got %v, expected true", cfg.EnablePrometheus)
	}
	if cfg.PrometheusMetricsBindPort != 9102 {
		t.Errorf("PrometheusMetricsBindPort: got %d, expected 9102", cfg.PrometheusMetricsBindPort)
	}
}

func TestLoadFromFilePreservesHashesOutsideInlineComments(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "hashes.conf")
	configContent := `USER_EXCLUDE_LIST="^svc#backup$" # exact service account
LIMIT_HOOK_URL="https://hooks.example.test/resman#limited"
SERVER_ROLE=api#blue
LOG_FILE=/var/log/resman.log # destination
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to create config file: %v", err)
	}

	cfg := DefaultConfig()
	if err := loadFromFile(configPath, cfg); err != nil {
		t.Fatalf("loadFromFile() error: %v", err)
	}
	if got := cfg.UserExcludeList; len(got) != 1 || got[0] != "^svc#backup$" {
		t.Fatalf("UserExcludeList = %v, want quoted hash pattern", got)
	}
	if cfg.LimitHookURL != "https://hooks.example.test/resman#limited" {
		t.Fatalf("LimitHookURL = %q, want URL fragment preserved", cfg.LimitHookURL)
	}
	if cfg.ServerRole != "api#blue" {
		t.Fatalf("ServerRole = %q, want unquoted hash preserved", cfg.ServerRole)
	}
	if cfg.LogFile != "/var/log/resman.log" {
		t.Fatalf("LogFile = %q, want inline comment removed", cfg.LogFile)
	}
}

func TestStripInlineComment(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "whitespace before comment", value: "value # comment", want: "value "},
		{name: "tab before comment", value: "value\t# comment", want: "value\t"},
		{name: "hash in double quotes", value: `"value # literal" # comment`, want: `"value # literal" `},
		{name: "hash in single quotes", value: `'value # literal' # comment`, want: `'value # literal' `},
		{name: "unquoted adjacent hash", value: "value#literal", want: "value#literal"},
		{name: "URL fragment", value: "https://example.test/hook#event", want: "https://example.test/hook#event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripInlineComment(tt.value); got != tt.want {
				t.Fatalf("stripInlineComment(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestLoadFromFileNonExistent(t *testing.T) {
	cfg := DefaultConfig()
	originalPollingInterval := cfg.PollingInterval

	// Caricamento da file inesistente non dovrebbe fallire
	err := loadFromFile("/nonexistent/path/config.conf", cfg)
	if err != nil {
		t.Errorf("loadFromFile() with non-existent file should not error: %v", err)
	}

	// I valori dovrebbero rimanere quelli di default
	if cfg.PollingInterval != originalPollingInterval {
		t.Errorf("config values should remain at defaults")
	}
}

func TestLoadFromFileMalformed(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "malformed.conf")

	// Config con linea malformata (senza =)
	configContent := `CPU_THRESHOLD=80
MALFORMED_LINE
CPU_RELEASE_THRESHOLD=50
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to create temp config file: %v", err)
	}

	cfg := DefaultConfig()
	err := loadFromFile(configPath, cfg)
	if err == nil {
		t.Error("loadFromFile() with malformed line should error")
	}
}

func TestLoadFromFileRejectsInvalidPrimitiveValues(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{name: "integer", entry: "POLLING_INTERVAL=invalid"},
		{name: "positive integer", entry: "METRICS_REFRESH_INTERVAL=0"},
		{name: "float", entry: "RAM_HIGH_RATIO=invalid"},
		{name: "boolean", entry: "ENABLE_PROMETHEUS=invalid"},
		{name: "port", entry: "PROMETHEUS_METRICS_BIND_PORT=invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "invalid.conf")
			if err := os.WriteFile(configPath, []byte(tt.entry+"\n"), 0644); err != nil {
				t.Fatalf("failed to create config file: %v", err)
			}

			if err := loadFromFile(configPath, DefaultConfig()); err == nil {
				t.Fatalf("loadFromFile() accepted %q", tt.entry)
			}
		})
	}
}

func TestLoadFromEnvironment(t *testing.T) {
	// Imposta variabili d'ambiente (t.Setenv ripristina automaticamente a fine test)
	t.Setenv("CPU_THRESHOLD", "85")
	t.Setenv("CPU_RELEASE_THRESHOLD", "55")
	t.Setenv("POLLING_INTERVAL", "45")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("ENABLE_PROMETHEUS", "true")
	t.Setenv("PROMETHEUS_METRICS_BIND_PORT", "1974")
	t.Setenv("PROMETHEUS_AUTH_TYPE", "BASIC")

	cfg := DefaultConfig()
	if err := loadFromEnvironment(cfg); err != nil {
		t.Fatalf("loadFromEnvironment() error: %v", err)
	}

	if cfg.CPUThreshold != 85 {
		t.Errorf("CPUThreshold: got %d, expected 85", cfg.CPUThreshold)
	}
	if cfg.CPUReleaseThreshold != 55 {
		t.Errorf("CPUReleaseThreshold: got %d, expected 55", cfg.CPUReleaseThreshold)
	}
	if cfg.PollingInterval != 45 {
		t.Errorf("PollingInterval: got %d, expected 45", cfg.PollingInterval)
	}
	if cfg.LogLevel != "DEBUG" {
		t.Errorf("LogLevel: got %s, expected DEBUG", cfg.LogLevel)
	}
	if !cfg.EnablePrometheus {
		t.Errorf("EnablePrometheus: got %v, expected true", cfg.EnablePrometheus)
	}
	if cfg.PrometheusMetricsBindPort != 1974 {
		t.Errorf("PrometheusMetricsBindPort: got %d, expected 1974", cfg.PrometheusMetricsBindPort)
	}
	if cfg.PrometheusAuthType != "basic" {
		t.Errorf("PrometheusAuthType: got %s, expected basic", cfg.PrometheusAuthType)
	}
}

func TestLoadFromEnvironmentUsesValidatedHandlers(t *testing.T) {
	t.Setenv("USER_INCLUDE_LIST", "[")
	t.Setenv("CGROUP_BASE", "../escape")
	t.Setenv("PROMETHEUS_METRICS_BIND_PORT", "invalid")
	t.Setenv("ENABLE_PROMETHEUS", "invalid")

	cfg := DefaultConfig()
	err := loadFromEnvironment(cfg)
	if err == nil {
		t.Fatal("loadFromEnvironment() accepted invalid overrides")
	}
	errorMessage := err.Error()

	for _, key := range []string{
		"USER_INCLUDE_LIST",
		"CGROUP_BASE",
		"PROMETHEUS_METRICS_BIND_PORT",
		"ENABLE_PROMETHEUS",
	} {
		if !strings.Contains(errorMessage, key) {
			t.Errorf("loadFromEnvironment() did not report invalid %s: %v", key, err)
		}
	}
	if cfg.UserIncludeList != nil {
		t.Errorf("invalid USER_INCLUDE_LIST changed value to %v", cfg.UserIncludeList)
	}
	if cfg.CgroupBase != "resman" {
		t.Errorf("invalid CGROUP_BASE changed value to %q", cfg.CgroupBase)
	}
	if cfg.PrometheusMetricsBindPort != 1974 {
		t.Errorf("invalid Prometheus port changed value to %d", cfg.PrometheusMetricsBindPort)
	}
	if cfg.EnablePrometheus {
		t.Error("invalid ENABLE_PROMETHEUS changed its default")
	}
}

func TestLoadFromEnvironmentAppliesEmptyOverrides(t *testing.T) {
	t.Setenv("USER_EXCLUDE_LIST", "")
	t.Setenv("BLACKOUT", "")

	cfg := DefaultConfig()
	cfg.UserExcludeList = []string{"^batch$"}
	cfg.BlackoutSpec = "1-5 22-06"
	cfg.BlackoutTimeframes = []Timeframe{{}}

	if err := loadFromEnvironment(cfg); err != nil {
		t.Fatalf("loadFromEnvironment() error: %v", err)
	}
	if cfg.UserExcludeList != nil {
		t.Fatalf("UserExcludeList = %v, want nil after empty override", cfg.UserExcludeList)
	}
	if cfg.BlackoutSpec != "" {
		t.Fatalf("BlackoutSpec = %q, want empty after override", cfg.BlackoutSpec)
	}
	if cfg.BlackoutTimeframes != nil {
		t.Fatalf("BlackoutTimeframes = %v, want nil after empty override", cfg.BlackoutTimeframes)
	}
}

func TestEveryEnvironmentFieldUsesAValidatedHandler(t *testing.T) {
	cfgType := reflect.TypeOf(Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		key := cfgType.Field(i).Tag.Get("config")
		if key == "" || key == "-" {
			continue
		}
		if _, ok := configFieldHandlers[key]; !ok {
			t.Errorf("config field %s has no handler for %s", cfgType.Field(i).Name, key)
		}
	}
}

func TestLoadFromEnvironmentParsesBlackout(t *testing.T) {
	t.Setenv("BLACKOUT", "1-5 22-06")

	cfg := DefaultConfig()
	if err := loadFromEnvironment(cfg); err != nil {
		t.Fatalf("loadFromEnvironment() error: %v", err)
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig() error: %v", err)
	}

	if len(cfg.BlackoutTimeframes) != 1 {
		t.Fatalf("BlackoutTimeframes length = %d, want 1", len(cfg.BlackoutTimeframes))
	}
	timeframe := cfg.BlackoutTimeframes[0]
	if timeframe.HourStart != 22 || timeframe.HourEnd != 6 {
		t.Fatalf("blackout hours = %02d-%02d, want 22-06", timeframe.HourStart, timeframe.HourEnd)
	}
}

func TestLoadFromEnvironmentRejectsInvalidBlackout(t *testing.T) {
	t.Setenv("BLACKOUT", "1-5 24-06")

	cfg := DefaultConfig()
	if err := loadFromEnvironment(cfg); err == nil {
		t.Fatal("loadFromEnvironment() accepted invalid BLACKOUT")
	}
}

func TestValidateConfigRequiresMCPAuthTokenForHTTP(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MCPEnabled = true
	cfg.MCPTransport = "http"
	cfg.MCPAuthToken = ""

	if err := validateConfig(cfg); err == nil {
		t.Fatal("validateConfig() accepted HTTP MCP transport without MCP_AUTH_TOKEN")
	}

	cfg.MCPAuthToken = "test-token"
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig() rejected authenticated HTTP MCP transport: %v", err)
	}
}

func TestMCPServerConfigUsesValidatedEnvironmentHandlers(t *testing.T) {
	t.Setenv("MCP_ENABLED", "yes")
	t.Setenv("MCP_TRANSPORT", "HTTP")
	t.Setenv("MCP_HTTP_PORT", "9090")
	t.Setenv("MCP_HTTP_HOST", "192.0.2.10")
	t.Setenv("MCP_TLS_ENABLED", "on")
	t.Setenv("MCP_TLS_CERT_FILE", "/test/server.crt")
	t.Setenv("MCP_TLS_KEY_FILE", "/test/server.key")
	t.Setenv("MCP_TLS_CA_FILE", "/test/ca.crt")
	t.Setenv("MCP_TLS_MIN_VERSION", "1.3")
	t.Setenv("MCP_LOG_LEVEL", "debug")
	t.Setenv("MCP_AUTH_TOKEN", "test-token")
	t.Setenv("MCP_ALLOW_WRITE_OPS", "1")

	cfg := DefaultConfig()
	if err := loadFromEnvironment(cfg); err != nil {
		t.Fatalf("loadFromEnvironment() error = %v", err)
	}
	got := cfg.MCPServerConfig()
	want := MCPServerConfig{
		Enabled:       true,
		Transport:     "http",
		HTTPPort:      9090,
		HTTPHost:      "192.0.2.10",
		TLSEnabled:    true,
		TLSCertFile:   "/test/server.crt",
		TLSKeyFile:    "/test/server.key",
		TLSCAFile:     "/test/ca.crt",
		TLSMinVersion: "1.3",
		LogLevel:      "DEBUG",
		AuthToken:     "test-token",
		AllowWriteOps: true,
	}
	if got != want {
		t.Fatalf("MCPServerConfig() = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("MCPServerConfig.Validate() error = %v", err)
	}
}

func TestValidateConfigRequiresTLSForMCPHTTP(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		tlsEnabled bool
		certFile   string
		keyFile    string
		minVersion string
		wantErr    bool
	}{
		{name: "default loopback TLS", host: "127.0.0.1", tlsEnabled: true, certFile: "server.crt", keyFile: "server.key", minVersion: "1.3"},
		{name: "non-loopback protected by TLS", host: "0.0.0.0", tlsEnabled: true, certFile: "server.crt", keyFile: "server.key", minVersion: "1.3"},
		{name: "TLS disabled", host: "127.0.0.1", certFile: "server.crt", keyFile: "server.key", minVersion: "1.3", wantErr: true},
		{name: "missing certificate", host: "127.0.0.1", tlsEnabled: true, keyFile: "server.key", minVersion: "1.3", wantErr: true},
		{name: "missing key", host: "127.0.0.1", tlsEnabled: true, certFile: "server.crt", minVersion: "1.3", wantErr: true},
		{name: "invalid minimum version", host: "127.0.0.1", tlsEnabled: true, certFile: "server.crt", keyFile: "server.key", minVersion: "SSLv3", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.MCPEnabled = true
			cfg.MCPTransport = "http"
			cfg.MCPAuthToken = "test-token"
			cfg.MCPHTTPHost = tt.host
			cfg.MCPTLSEnabled = tt.tlsEnabled
			cfg.MCPTLSCertFile = tt.certFile
			cfg.MCPTLSKeyFile = tt.keyFile
			cfg.MCPTLSMinVersion = tt.minVersion

			err := validateConfig(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateConfig() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidateConfigRejectsNonPositiveLogMaxSize(t *testing.T) {
	tests := []struct {
		name    string
		maxSize int
	}{
		{name: "zero", maxSize: 0},
		{name: "negative", maxSize: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.LogMaxSize = tt.maxSize
			if err := validateConfig(cfg); err == nil {
				t.Fatalf("validateConfig() accepted LOG_MAX_SIZE=%d", tt.maxSize)
			}
		})
	}
}

func TestBlackoutTimeframes(t *testing.T) {
	location := time.UTC
	monday := time.Date(2026, time.July, 20, 0, 0, 0, 0, location)
	if monday.Weekday() != time.Monday {
		t.Fatalf("test fixture is %s, want Monday", monday.Weekday())
	}

	tests := []struct {
		name        string
		spec        string
		at          time.Time
		active      bool
		expectedEnd time.Time
	}{
		{
			name:        "business window starts inclusively",
			spec:        "1-5 08-18",
			at:          monday.Add(8 * time.Hour),
			active:      true,
			expectedEnd: monday.Add(18 * time.Hour),
		},
		{
			name:   "business window ends exclusively",
			spec:   "1-5 08-18",
			at:     monday.Add(18 * time.Hour),
			active: false,
		},
		{
			name:        "full day includes final hour",
			spec:        "* 00-24",
			at:          monday.Add(23*time.Hour + 59*time.Minute),
			active:      true,
			expectedEnd: monday.Add(24 * time.Hour),
		},
		{
			name:        "overnight start day",
			spec:        "1-5 22-06",
			at:          monday.Add(23 * time.Hour),
			active:      true,
			expectedEnd: monday.Add(30 * time.Hour),
		},
		{
			name:        "overnight following day",
			spec:        "1-5 22-06",
			at:          monday.Add(29 * time.Hour),
			active:      true,
			expectedEnd: monday.Add(30 * time.Hour),
		},
		{
			name:        "Friday overnight includes Saturday morning",
			spec:        "1-5 22-06",
			at:          monday.Add(5*24*time.Hour + 5*time.Hour),
			active:      true,
			expectedEnd: monday.Add(5*24*time.Hour + 6*time.Hour),
		},
		{
			name:   "Saturday does not start weekday overnight",
			spec:   "1-5 22-06",
			at:     monday.Add(6*24*time.Hour + 5*time.Hour),
			active: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timeframes, err := ParseTimeframe(tt.spec)
			if err != nil {
				t.Fatalf("ParseTimeframe(%q) error: %v", tt.spec, err)
			}
			cfg := DefaultConfig()
			cfg.BlackoutTimeframes = timeframes

			end, active := cfg.blackoutEndAt(tt.at)
			if active != tt.active {
				t.Fatalf("blackout active = %v, want %v at %s", active, tt.active, tt.at)
			}
			if tt.active && !end.Equal(tt.expectedEnd) {
				t.Fatalf("blackout end = %s, want %s", end, tt.expectedEnd)
			}
		})
	}
}

func TestParseTimeframeRejectsAmbiguousOrOutOfRangeHours(t *testing.T) {
	for _, spec := range []string{
		"* 00-00",
		"* 24-06",
		"* 00-25",
	} {
		t.Run(spec, func(t *testing.T) {
			if _, err := ParseTimeframe(spec); err == nil {
				t.Fatalf("ParseTimeframe(%q) expected an error", spec)
			}
		})
	}
}

func TestParseTimeframeRejectsReversedDayRange(t *testing.T) {
	if _, err := ParseTimeframe("6-0 00-24"); err == nil {
		t.Fatal("ParseTimeframe() accepted reversed day range 6-0")
	}
}

func TestValidateConfigRejectsInvalidCPUQuotaAndTimeouts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "invalid normal CPU quota",
			mutate: func(cfg *Config) {
				cfg.CPUQuotaNormal = "0 100000"
			},
		},
		{
			name: "zero cgroup operation timeout",
			mutate: func(cfg *Config) {
				cfg.CgroupOperationTimeout = 0
			},
		},
		{
			name: "negative MCP shutdown timeout",
			mutate: func(cfg *Config) {
				cfg.MCPShutdownTimeout = -1
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(cfg)
			if err := validateConfig(cfg); err == nil {
				t.Fatal("validateConfig() expected an error")
			}
		})
	}
}

func TestValidateConfigRejectsInvalidPatternRAMQuotas(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "batch night quota",
			mutate: func(cfg *Config) {
				cfg.BatchNightRAMQuota = "invalid"
			},
		},
		{
			name: "interactive quota overflow",
			mutate: func(cfg *Config) {
				cfg.InteractiveRAMQuota = strconv.FormatUint(^uint64(0), 10) + "T"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(cfg)
			if err := validateConfig(cfg); err == nil {
				t.Fatal("validateConfig() accepted invalid pattern RAM quota")
			}
		})
	}
}

func TestValidateConfigRequiresPositiveMetricsCacheTTL(t *testing.T) {
	tests := []struct {
		name      string
		ttl       int
		wantError bool
	}{
		{name: "negative", ttl: -1, wantError: true},
		{name: "zero", ttl: 0, wantError: true},
		{name: "one second", ttl: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.MetricsCacheTTL = tt.ttl
			err := validateConfig(cfg)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "METRICS_CACHE_TTL must be at least 1 second") {
					t.Fatalf("validateConfig() error = %v, want METRICS_CACHE_TTL minimum", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateConfig() error = %v, want accepted TTL", err)
			}
		})
	}
}

func TestLoadAndValidate(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test.conf")

	configContent := `CPU_THRESHOLD=80
CPU_RELEASE_THRESHOLD=50
POLLING_INTERVAL=60
LOG_LEVEL=INFO
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to create temp config file: %v", err)
	}

	cfg, err := LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate() error: %v", err)
	}

	if cfg == nil {
		t.Fatal("LoadAndValidate() returned nil config")
	}

	if cfg.CPUThreshold != 80 {
		t.Errorf("CPUThreshold: got %d, expected 80", cfg.CPUThreshold)
	}
}

func TestLoadAndValidateRejectsLegacyConfigurationOnlyAtNewDefault(t *testing.T) {
	tests := []struct {
		name         string
		selected     string
		createNew    bool
		createLegacy bool
		createSaved  bool
		createBackup bool
		createTemp   bool
		backupNames  []string
		danglingLink string
		wantError    bool
	}{
		{name: "legacy only", selected: "default", createLegacy: true, wantError: true},
		{name: "RPM-saved legacy only", selected: "default", createSaved: true, wantError: true},
		{name: "legacy backup only", selected: "default", createBackup: true, wantError: true},
		{name: "legacy temporary only", selected: "default", createTemp: true, wantError: true},
		{name: "multiple generated legacy backups", selected: "default", backupNames: []string{"20260822_020202", "20260822_010101"}, wantError: true},
		{name: "many matching legacy backups", selected: "default", backupNames: []string{"20260822_000006", "20260822_000002", "20260822_000005", "20260822_000001", "20260822_000004", "20260822_000003"}, wantError: true},
		{name: "operator-named legacy backup candidate", selected: "default", backupNames: []string{"prima_della_migrazione"}, wantError: true},
		{name: "dangling legacy backup", selected: "default", danglingLink: "backup", wantError: true},
		{name: "dangling matching legacy backup", selected: "default", danglingLink: "matching", wantError: true},
		{name: "legacy artifacts and packaged default", selected: "default", createNew: true, createSaved: true, createBackup: true, createTemp: true, backupNames: []string{"20260822_030303"}, wantError: true},
		{name: "new default only", selected: "default", createNew: true},
		{name: "custom path with legacy artifacts present", selected: "custom", createLegacy: true, createBackup: true, createTemp: true, backupNames: []string{"20260822_040404"}},
		{name: "explicit legacy path", selected: "legacy", createLegacy: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			layout := diskLayout{
				defaultConfigPath:  filepath.Join(dir, "etc", "resman", "resman.conf"),
				legacyConfigPath:   filepath.Join(dir, "etc", "resman.conf"),
				legacySavedPath:    filepath.Join(dir, "etc", "resman.conf.rpmsave"),
				legacyBackupPath:   filepath.Join(dir, "etc", "resman.conf.backup"),
				legacyTempPath:     filepath.Join(dir, "etc", "resman.conf.tmp"),
				legacyBackupPrefix: filepath.Join(dir, "etc", "resman.conf.backup_"),
				defaultDBPath:      filepath.Join(dir, "var", "lib", "resman", "metrics.db"),
				legacyDBPath:       filepath.Join(dir, "etc", "resman", "metrics.db"),
			}
			customPath := filepath.Join(dir, "custom", "resman.conf")
			if tt.createNew {
				writeConfigFixture(t, layout.defaultConfigPath, "USER_INCLUDE_LIST=.*\n")
			}
			if tt.createLegacy {
				writeConfigFixture(t, layout.legacyConfigPath, "USER_INCLUDE_LIST=^legacy$\n")
			}
			if tt.createSaved {
				writeConfigFixture(t, layout.legacySavedPath, "USER_INCLUDE_LIST=^rpm-saved$\n")
			}
			if tt.createBackup {
				writeConfigFixture(t, layout.legacyBackupPath, "MCP_AUTH_TOKEN=legacy-secret\n")
			}
			if tt.createTemp {
				writeConfigFixture(t, layout.legacyTempPath, "MCP_AUTH_TOKEN=legacy-temp-secret\n")
			}
			for _, name := range tt.backupNames {
				writeConfigFixture(t, layout.legacyBackupPrefix+name, "MCP_AUTH_TOKEN=legacy-backup-secret\n")
			}
			if tt.danglingLink == "backup" {
				if err := os.MkdirAll(filepath.Dir(layout.legacyBackupPath), 0700); err != nil {
					t.Fatalf("os.MkdirAll(legacy backup parent) error = %v", err)
				}
				if err := os.Symlink(filepath.Join(dir, "missing-backup-target"), layout.legacyBackupPath); err != nil {
					t.Fatalf("os.Symlink(legacy backup) error = %v", err)
				}
			}
			if tt.danglingLink == "matching" {
				path := layout.legacyBackupPrefix + "20260822_050505"
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatalf("os.MkdirAll(matching backup parent) error = %v", err)
				}
				if err := os.Symlink(filepath.Join(dir, "missing-matching-backup-target"), path); err != nil {
					t.Fatalf("os.Symlink(matching backup) error = %v", err)
				}
			}
			selectedPath := layout.defaultConfigPath
			switch tt.selected {
			case "custom":
				selectedPath = customPath
				writeConfigFixture(t, selectedPath, "USER_INCLUDE_LIST=.*\n")
			case "legacy":
				selectedPath = layout.legacyConfigPath
			}

			cfg, err := loadAndValidateWithLayout(selectedPath, layout)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), layout.defaultConfigPath) {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want both layout paths", err)
				}
				if tt.createLegacy && !strings.Contains(err.Error(), layout.legacyConfigPath) {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want legacy config path", err)
				}
				if tt.createSaved && (!strings.Contains(err.Error(), layout.legacySavedPath) ||
					!strings.Contains(err.Error(), "authoritative authored contents")) {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want RPM-saved recovery guidance", err)
				}
				if (tt.createBackup || tt.danglingLink == "backup") && (!strings.Contains(err.Error(), layout.legacyBackupPath) ||
					!strings.Contains(err.Error(), "securely remove")) {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want secure backup removal", err)
				}
				if tt.createBackup && len(tt.backupNames) == 0 && strings.Count(err.Error(), layout.legacyBackupPath) != 1 {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want legacy backup path exactly once", err)
				}
				if tt.createTemp && !strings.Contains(err.Error(), layout.legacyTempPath) {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want legacy temporary path", err)
				}
				if tt.createTemp && strings.Count(err.Error(), layout.legacyTempPath) != 1 {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want legacy temporary path exactly once", err)
				}
				if len(tt.backupNames) > 0 {
					sortedNames := slices.Clone(tt.backupNames)
					slices.Sort(sortedNames)
					wantSummary := fmt.Sprintf("%d potential configuration-copy entries matching %s*", len(sortedNames), layout.legacyBackupPrefix)
					if !strings.Contains(err.Error(), wantSummary) {
						t.Fatalf("loadAndValidateWithLayout() error = %v, want bounded summary %q", err, wantSummary)
					}
					for _, required := range []string{"operator-managed copies", "protected archive", "securely remove generated or unneeded"} {
						if !strings.Contains(err.Error(), required) {
							t.Fatalf("loadAndValidateWithLayout() error = %v, want non-destructive backup-candidate remedy %q", err, required)
						}
					}
					if strings.Contains(err.Error(), "timestamped backup entries") {
						t.Fatalf("loadAndValidateWithLayout() error = %v, want no false timestamp claim", err)
					}
					previousIndex := -1
					for i, name := range sortedNames {
						path := layout.legacyBackupPrefix + name
						index := strings.Index(err.Error(), path)
						if i < legacyBackupExampleLimit {
							if index <= previousIndex || strings.Count(err.Error(), path) != 1 {
								t.Fatalf("loadAndValidateWithLayout() error = %v, want unique deterministic example %q", err, path)
							}
							previousIndex = index
						} else if index >= 0 {
							t.Fatalf("loadAndValidateWithLayout() error = %v, want examples capped before %q", err, path)
						}
					}
					if remaining := len(sortedNames) - legacyBackupExampleLimit; remaining > 0 &&
						!strings.Contains(err.Error(), fmt.Sprintf("%d more", remaining)) {
						t.Fatalf("loadAndValidateWithLayout() error = %v, want omitted backup count %d", err, remaining)
					}
				}
				if tt.danglingLink == "matching" && !strings.Contains(err.Error(), layout.legacyBackupPrefix+"20260822_050505") {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want dangling matching backup path", err)
				}
				if cfg != nil {
					t.Fatalf("loadAndValidateWithLayout() config = %+v, want nil after legacy refusal", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadAndValidateWithLayout() error = %v", err)
			}
			if cfg.ConfigFile != filepath.Clean(selectedPath) {
				t.Fatalf("ConfigFile = %q, want %q", cfg.ConfigFile, filepath.Clean(selectedPath))
			}
		})
	}
}

func TestLoadAndValidateRejectsLegacyDatabaseOnlyWhenDefaultIsEnabled(t *testing.T) {
	tests := []struct {
		name         string
		enabled      bool
		selectedDB   string
		createNew    bool
		createLegacy bool
		danglingLink bool
		wantError    bool
	}{
		{name: "enabled default with legacy only", enabled: true, selectedDB: "default", createLegacy: true, wantError: true},
		{name: "enabled default with dangling legacy", enabled: true, selectedDB: "default", danglingLink: true, wantError: true},
		{name: "enabled default with both databases", enabled: true, selectedDB: "default", createNew: true, createLegacy: true, wantError: true},
		{name: "enabled default with new database only", enabled: true, selectedDB: "default", createNew: true},
		{name: "disabled default with legacy present", selectedDB: "default", createLegacy: true},
		{name: "enabled memory database with legacy present", enabled: true, selectedDB: "memory", createLegacy: true},
		{name: "enabled custom database with legacy present", enabled: true, selectedDB: "custom", createLegacy: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			layout := diskLayout{
				defaultConfigPath:  filepath.Join(dir, "etc", "resman", "resman.conf"),
				legacyConfigPath:   filepath.Join(dir, "etc", "resman.conf"),
				legacySavedPath:    filepath.Join(dir, "etc", "resman.conf.rpmsave"),
				legacyBackupPath:   filepath.Join(dir, "etc", "resman.conf.backup"),
				legacyTempPath:     filepath.Join(dir, "etc", "resman.conf.tmp"),
				legacyBackupPrefix: filepath.Join(dir, "etc", "resman.conf.backup_"),
				defaultDBPath:      filepath.Join(dir, "var", "lib", "resman", "metrics.db"),
				legacyDBPath:       filepath.Join(dir, "etc", "resman", "metrics.db"),
			}
			selectedDB := layout.defaultDBPath
			switch tt.selectedDB {
			case "memory":
				selectedDB = ":memory:"
			case "custom":
				selectedDB = filepath.Join(dir, "custom", "metrics.db")
			}
			configContent := fmt.Sprintf(
				"USER_INCLUDE_LIST=.*\nMETRICS_DB_ENABLED=%t\nMETRICS_DB_PATH=%s\n",
				tt.enabled,
				selectedDB,
			)
			writeConfigFixture(t, layout.defaultConfigPath, configContent)
			if tt.createNew {
				writeConfigFixture(t, layout.defaultDBPath, "new database marker")
			}
			if tt.createLegacy {
				writeConfigFixture(t, layout.legacyDBPath, "legacy database marker")
			}
			if tt.danglingLink {
				if err := os.Symlink(filepath.Join(dir, "missing-database-target"), layout.legacyDBPath); err != nil {
					t.Fatalf("os.Symlink(legacy database) error = %v", err)
				}
			}

			cfg, err := loadAndValidateWithLayout(layout.defaultConfigPath, layout)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), layout.legacyDBPath) ||
					!strings.Contains(err.Error(), layout.defaultDBPath) ||
					!strings.Contains(err.Error(), "archive or delete") {
					t.Fatalf("loadAndValidateWithLayout() error = %v, want database reset guidance", err)
				}
				if cfg != nil {
					t.Fatalf("loadAndValidateWithLayout() config = %+v, want nil after legacy refusal", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadAndValidateWithLayout() error = %v", err)
			}
			if cfg.MetricsDBPath != selectedDB || cfg.MetricsDBEnabled != tt.enabled {
				t.Fatalf("database config = enabled:%t path:%q, want enabled:%t path:%q", cfg.MetricsDBEnabled, cfg.MetricsDBPath, tt.enabled, selectedDB)
			}
		})
	}
}

func writeConfigFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("os.MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("os.WriteFile(%s) error = %v", path, err)
	}
}

func TestLoadAndValidateUsesAuthoritativeConfigPathForWrites(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "active.conf")
	configContent := "USER_INCLUDE_LIST=.*\n"
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to create config file: %v", err)
	}

	cfg, err := LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate() error: %v", err)
	}
	resolvedPath, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatalf("filepath.Abs() error: %v", err)
	}
	if cfg.ConfigFile != resolvedPath {
		t.Fatalf("ConfigFile = %q, want active path %q", cfg.ConfigFile, resolvedPath)
	}

	if _, err := cfg.PersistUserExcludeList([]string{"^service$"}, cfg.ConfigFile); err != nil {
		t.Fatalf("PersistUserExcludeList() error: %v", err)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read active config: %v", err)
	}
	if !strings.Contains(string(content), "USER_EXCLUDE_LIST=^service$") {
		t.Fatalf("active config was not updated: %q", content)
	}
}

func TestRemovedConfigurationKeysAreRejected(t *testing.T) {
	for key := range removedConfigKeys {
		unsetEnvForTest(t, key)
	}

	for key := range removedConfigKeys {
		key := key
		t.Run(key+" in file", func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "removed.conf")
			if err := os.WriteFile(configPath, []byte(key+"=value\n"), 0600); err != nil {
				t.Fatalf("write removed-key config: %v", err)
			}
			cfg, err := LoadAndValidate(configPath)
			if err == nil || cfg != nil {
				t.Fatalf("LoadAndValidate() = (%#v, %v), want removed-key error", cfg, err)
			}
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), configPath) ||
				!strings.Contains(err.Error(), "was removed") {
				t.Fatalf("LoadAndValidate() error = %v, want key, file, and removal reason", err)
			}
		})

		t.Run(key+" in environment", func(t *testing.T) {
			t.Setenv(key, "value")
			err := loadFromEnvironment(DefaultConfig())
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "was removed") {
				t.Fatalf("loadFromEnvironment() error = %v, want explicit removed-key error", err)
			}
		})
	}
}

func TestLoadFromFileRejectsUnknownKeyWithPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "unknown.conf")
	if err := os.WriteFile(configPath, []byte("MISSPELLED_THRESHOLD=75\n"), 0600); err != nil {
		t.Fatalf("write unknown-key config: %v", err)
	}

	err := loadFromFile(configPath, DefaultConfig())
	if err == nil || !strings.Contains(err.Error(), "MISSPELLED_THRESHOLD") ||
		!strings.Contains(err.Error(), configPath) || !strings.Contains(err.Error(), "unknown configuration key") {
		t.Fatalf("loadFromFile() error = %v, want unknown key and file path", err)
	}
}

func TestLoadAndValidateRejectsInvalidEnvironmentOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "test.conf")
	if err := os.WriteFile(configPath, []byte("CPU_THRESHOLD=80\n"), 0644); err != nil {
		t.Fatalf("failed to create config file: %v", err)
	}
	t.Setenv("CPU_THRESHOLD", "8O")

	cfg, err := LoadAndValidate(configPath)
	if err == nil {
		t.Fatal("LoadAndValidate() accepted invalid CPU_THRESHOLD environment override")
	}
	if cfg != nil {
		t.Fatalf("LoadAndValidate() returned config after environment error: %#v", cfg)
	}
	if !strings.Contains(err.Error(), "loading environment overrides") ||
		!strings.Contains(err.Error(), "CPU_THRESHOLD") {
		t.Fatalf("LoadAndValidate() returned unexpected error: %v", err)
	}
}

func TestSetConfigField(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		value       string
		expectError bool
		checkFunc   func(*Config) bool
	}{
		{
			name:        "set CGROUP_ROOT",
			key:         "CGROUP_ROOT",
			value:       "/test/cgroup",
			expectError: false,
			checkFunc:   func(c *Config) bool { return c.CgroupRoot == "/test/cgroup" },
		},
		{
			name:        "set POLLING_INTERVAL",
			key:         "POLLING_INTERVAL",
			value:       "120",
			expectError: false,
			checkFunc:   func(c *Config) bool { return c.PollingInterval == 120 },
		},
		{
			name:        "set ENABLE_PROMETHEUS true",
			key:         "ENABLE_PROMETHEUS",
			value:       "true",
			expectError: false,
			checkFunc:   func(c *Config) bool { return c.EnablePrometheus == true },
		},
		{
			name:        "set ENABLE_PROMETHEUS false",
			key:         "ENABLE_PROMETHEUS",
			value:       "false",
			expectError: false,
			checkFunc:   func(c *Config) bool { return c.EnablePrometheus == false },
		},
		{
			name:        "set LOG_LEVEL",
			key:         "LOG_LEVEL",
			value:       "DEBUG",
			expectError: false,
			checkFunc:   func(c *Config) bool { return c.LogLevel == "DEBUG" },
		},
		{
			name:        "set IGNORE_SYSTEM_LOAD",
			key:         "IGNORE_SYSTEM_LOAD",
			value:       "yes",
			expectError: false,
			checkFunc:   func(c *Config) bool { return c.IgnoreSystemLoad == true },
		},
		{
			name:        "unknown key",
			key:         "UNKNOWN_KEY",
			value:       "value",
			expectError: true,
			checkFunc:   func(c *Config) bool { return true },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			err := setConfigField(cfg, tt.key, tt.value)

			if tt.expectError && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tt.checkFunc(cfg) {
				t.Errorf("checkFunc failed for key %s with value %s", tt.key, tt.value)
			}
		})
	}
}

func TestPersistUserFiltersDoesNotPublishLiveState(t *testing.T) {
	tests := []struct {
		name     string
		persist  func(*Config, []string, string) (UserFilterPersistenceResult, error)
		fileKey  string
		liveList func(*Config) []string
	}{
		{
			name: "include",
			persist: func(cfg *Config, patterns []string, path string) (UserFilterPersistenceResult, error) {
				return cfg.PersistUserIncludeList(patterns, path)
			},
			fileKey:  "USER_INCLUDE_LIST=^new$",
			liveList: (*Config).GetUserIncludeList,
		},
		{
			name: "exclude",
			persist: func(cfg *Config, patterns []string, path string) (UserFilterPersistenceResult, error) {
				return cfg.PersistUserExcludeList(patterns, path)
			},
			fileKey:  "USER_EXCLUDE_LIST=^new$",
			liveList: (*Config).GetUserExcludeList,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "resman.conf")
			if err := os.WriteFile(configPath, []byte("USER_INCLUDE_LIST=^old$\nUSER_EXCLUDE_LIST=^old$\n"), 0600); err != nil {
				t.Fatalf("WriteFile() error: %v", err)
			}
			cfg, err := LoadAndValidate(configPath)
			if err != nil {
				t.Fatalf("LoadAndValidate() error: %v", err)
			}
			patterns := []string{"^new$"}
			result, err := tt.persist(cfg, patterns, configPath)
			if err != nil {
				t.Fatalf("persist() error: %v", err)
			}
			patterns[0] = "^caller-mutated$"
			if !slices.Equal(result.PreviousValue, []string{"^old$"}) {
				t.Fatalf("previous value = %v, want [^old$]", result.PreviousValue)
			}
			if !slices.Equal(tt.liveList(cfg), []string{"^old$"}) {
				t.Fatalf("live configuration was published before acknowledgement: %v", tt.liveList(cfg))
			}
			content, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile() error: %v", err)
			}
			if !strings.Contains(string(content), tt.fileKey) || strings.Contains(string(content), "caller-mutated") {
				t.Fatalf("persisted content = %q, want detached requested value", content)
			}
		})
	}
}

func TestCPUUserIncludeListSemantics(t *testing.T) {
	tests := []struct {
		name        string
		includeList []string
		excludeList []string
		username    string
		wantInclude bool
		wantAllowed bool
	}{
		{
			name:        "empty list disables CPU limiting",
			username:    "alice",
			wantInclude: false,
			wantAllowed: false,
		},
		{
			name:        "match all enables CPU limiting",
			includeList: []string{".*"},
			username:    "alice",
			wantInclude: true,
			wantAllowed: true,
		},
		{
			name:        "specific pattern includes matching user",
			includeList: []string{"^app-.*$"},
			username:    "app-worker",
			wantInclude: true,
			wantAllowed: true,
		},
		{
			name:        "specific pattern rejects non-matching user",
			includeList: []string{"^app-.*$"},
			username:    "alice",
			wantInclude: false,
			wantAllowed: false,
		},
		{
			name:        "exclude list takes precedence",
			includeList: []string{".*"},
			excludeList: []string{"^root$"},
			username:    "root",
			wantInclude: true,
			wantAllowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.UserIncludeList = tt.includeList
			cfg.UserExcludeList = tt.excludeList

			if got := cfg.IsUserIncluded(tt.username); got != tt.wantInclude {
				t.Errorf("IsUserIncluded(%q) = %v, want %v", tt.username, got, tt.wantInclude)
			}
			if got := cfg.IsUserWhitelisted(tt.username); got != tt.wantAllowed {
				t.Errorf("IsUserWhitelisted(%q) = %v, want %v", tt.username, got, tt.wantAllowed)
			}
		})
	}
}

func TestEmptyRAMAndIOIncludeListsStillIncludeAllUsers(t *testing.T) {
	cfg := DefaultConfig()

	if !cfg.IsUserIncludedForRAM("alice") {
		t.Fatal("empty RAM_USER_INCLUDE_LIST excluded user")
	}
	if !cfg.IsUserIncludedForIO("alice") {
		t.Fatal("empty IO_USER_INCLUDE_LIST excluded user")
	}
}

func TestEvaluateUserEligibilityKeepsResourcePoliciesIndependent(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		username  string
		want      UserEligibility
	}{
		{
			name:     "empty lists disable CPU and include RAM and IO",
			username: "alice",
			want: UserEligibility{
				EligibleForRAM: true,
				EligibleForIO:  true,
			},
		},
		{
			name: "CPU include does not override RAM and IO excludes",
			configure: func(cfg *Config) {
				cfg.UserIncludeList = []string{"^alice$"}
				cfg.RAMUserExcludeList = []string{"^alice$"}
				cfg.IOUserExcludeList = []string{"^alice$"}
			},
			username: "alice",
			want:     UserEligibility{EligibleForCPU: true},
		},
		{
			name: "CPU exclude does not disable RAM and IO",
			configure: func(cfg *Config) {
				cfg.UserIncludeList = []string{".*"}
				cfg.UserExcludeList = []string{"^alice$"}
			},
			username: "alice",
			want: UserEligibility{
				EligibleForRAM: true,
				EligibleForIO:  true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if tt.configure != nil {
				tt.configure(cfg)
			}
			if got := cfg.EvaluateUserEligibility(tt.username); got != tt.want {
				t.Fatalf("EvaluateUserEligibility(%q) = %+v, want %+v", tt.username, got, tt.want)
			}
		})
	}
}

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	value, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("failed to unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, value)
			return
		}
		_ = os.Unsetenv(key)
	})
}
