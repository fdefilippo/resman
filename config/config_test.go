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
	"testing"
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
		{"PrometheusMetricsBindHost", cfg.PrometheusMetricsBindHost, "127.0.0.1"},  // Secure default
		{"LogLevel", cfg.LogLevel, "INFO"},
		{"SystemUIDMin", cfg.SystemUIDMin, 1000},
		{"IgnoreSystemLoad", cfg.IgnoreSystemLoad, false},
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

func TestLoadFromEnvironment(t *testing.T) {
	// Imposta variabili d'ambiente
	os.Setenv("CPU_THRESHOLD", "85")
	os.Setenv("CPU_RELEASE_THRESHOLD", "55")
	os.Setenv("POLLING_INTERVAL", "45")
	os.Setenv("LOG_LEVEL", "DEBUG")
	os.Setenv("ENABLE_PROMETHEUS", "true")
	os.Setenv("PROMETHEUS_METRICS_BIND_PORT", "1974")

	// Pulisci dopo il test
	defer func() {
		os.Unsetenv("CPU_THRESHOLD")
		os.Unsetenv("CPU_RELEASE_THRESHOLD")
		os.Unsetenv("POLLING_INTERVAL")
		os.Unsetenv("LOG_LEVEL")
		os.Unsetenv("ENABLE_PROMETHEUS")
		os.Unsetenv("PROMETHEUS_METRICS_BIND_PORT")
	}()

	cfg := DefaultConfig()
	loadFromEnvironment(cfg)

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
