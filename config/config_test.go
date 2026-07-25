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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg == nil {
		t.Fatal("DefaultConfig() returned nil")
	}

	// Verifica valori default principali
	tests := []struct {
		name     string
		got      interface{}
		expected interface{}
	}{
		{"CgroupRoot", cfg.CgroupRoot, "/sys/fs/cgroup"},
		{"LogFile", cfg.LogFile, "/var/log/resman.log"},
		{"PollingInterval", cfg.PollingInterval, 30},
		{"MinActiveTime", cfg.MinActiveTime, 60},
		{"CPUThreshold", cfg.CPUThreshold, 75},
		{"CPUReleaseThreshold", cfg.CPUReleaseThreshold, 40},
		{"CPUQuotaNormal", cfg.CPUQuotaNormal, "max 100000"},
		{"CPUQuotaLimited", cfg.CPUQuotaLimited, "50000 100000"},
		{"EnablePrometheus", cfg.EnablePrometheus, false},
		{"PrometheusMetricsBindPort", cfg.PrometheusMetricsBindPort, 1974},
		{"PrometheusMetricsBindHost", cfg.PrometheusMetricsBindHost, "127.0.0.1"}, // Secure default
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
				MetricsRefreshInterval: 30,
				CgroupOperationTimeout: 5,
				MCPShutdownTimeout:     10,
				CPUQuotaNormal:         "max 100000",
				CPUQuotaLimited:        "50000 100000",
				LogLevel:               "INFO",
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
				CPUQuotaLimited:     "50000 100000",
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
				CPUQuotaLimited:     "50000 100000",
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
				CPUQuotaLimited:     "50000 100000",
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
				CPUQuotaLimited:     "50000 100000",
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
				CPUQuotaLimited:     "50000 100000",
				LogLevel:            "INFO",
				SystemUIDMin:        1000,
				SystemUIDMax:        60000,
			},
			expectError: true,
		},
		{
			name: "invalid CPU_QUOTA_LIMITED format",
			cfg: &Config{
				CPUThreshold:        75,
				CPUReleaseThreshold: 40,
				PollingInterval:     30,
				CPUQuotaLimited:     "invalid",
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
				CPUQuotaLimited:     "50000 100000",
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
				CPUQuotaLimited:     "50000 100000",
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
				CPUQuotaLimited:     "50000 100000",
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
PROMETHEUS_PORT=9102
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to create temp config file: %v", err)
	}

	cfg := DefaultConfig()
	err := loadFromFile(configPath, cfg)
	if err != nil {
		t.Fatalf("loadFromFile() error: %v", err)
	}

	// Verifica i valori caricati
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
		{name: "port", entry: "PROMETHEUS_PORT=invalid"},
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

func TestLoadFromEnvironmentSupportsPrometheusAliases(t *testing.T) {
	unsetEnvForTest(t, "PROMETHEUS_METRICS_BIND_HOST")
	unsetEnvForTest(t, "PROMETHEUS_METRICS_BIND_PORT")
	t.Setenv("PROMETHEUS_HOST", "192.0.2.10")
	t.Setenv("PROMETHEUS_PORT", "9191")

	cfg := DefaultConfig()
	if err := loadFromEnvironment(cfg); err != nil {
		t.Fatalf("loadFromEnvironment() error: %v", err)
	}
	if cfg.PrometheusMetricsBindHost != "192.0.2.10" {
		t.Errorf("PrometheusMetricsBindHost = %q, want 192.0.2.10", cfg.PrometheusMetricsBindHost)
	}
	if cfg.PrometheusMetricsBindPort != 9191 {
		t.Errorf("PrometheusMetricsBindPort = %d, want 9191", cfg.PrometheusMetricsBindPort)
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

func TestLoadFromEnvironmentPrefersCanonicalPrometheusKeys(t *testing.T) {
	t.Setenv("PROMETHEUS_METRICS_BIND_HOST", "127.0.0.2")
	t.Setenv("PROMETHEUS_HOST", "192.0.2.10")
	t.Setenv("PROMETHEUS_METRICS_BIND_PORT", "9192")
	t.Setenv("PROMETHEUS_PORT", "9191")

	cfg := DefaultConfig()
	if err := loadFromEnvironment(cfg); err != nil {
		t.Fatalf("loadFromEnvironment() error: %v", err)
	}
	if cfg.PrometheusMetricsBindHost != "127.0.0.2" {
		t.Errorf("PrometheusMetricsBindHost = %q, want canonical value", cfg.PrometheusMetricsBindHost)
	}
	if cfg.PrometheusMetricsBindPort != 9192 {
		t.Errorf("PrometheusMetricsBindPort = %d, want canonical value", cfg.PrometheusMetricsBindPort)
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
			name: "invalid limited CPU quota",
			mutate: func(cfg *Config) {
				cfg.CPUQuotaLimited = "50000 0"
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
			name:        "unknown key (should not error)",
			key:         "UNKNOWN_KEY",
			value:       "value",
			expectError: false,
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

func TestUserFilterConcurrentAccess(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	if err := os.WriteFile(configPath, []byte("USER_INCLUDE_LIST=.*\nUSER_EXCLUDE_LIST=\n"), 0644); err != nil {
		t.Fatalf("failed to create config file: %v", err)
	}

	cfg := DefaultConfig()
	const iterations = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			patterns := []string{"^include$", "^shared$"}
			if _, err := cfg.SetUserIncludeList(patterns, configPath, false); err != nil {
				errs <- err
				return
			}
			patterns[0] = "^mutated$"
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			patterns := []string{"^exclude$"}
			if _, err := cfg.SetUserExcludeList(patterns, configPath, false); err != nil {
				errs <- err
				return
			}
			patterns[0] = "^mutated$"
		}
	}()

	for i := 0; i < iterations*4; i++ {
		_ = cfg.IsUserIncluded("include")
		_ = cfg.IsUserExcluded("exclude")
		_ = cfg.IsUserWhitelisted("shared")
		_ = cfg.GetUserIncludeList()
		_ = cfg.GetUserExcludeList()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent setter failed: %v", err)
	}

	for _, pattern := range cfg.GetUserIncludeList() {
		if pattern == "^mutated$" {
			t.Fatal("SetUserIncludeList retained caller-owned slice")
		}
	}
	for _, pattern := range cfg.GetUserExcludeList() {
		if pattern == "^mutated$" {
			t.Fatal("SetUserExcludeList retained caller-owned slice")
		}
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
