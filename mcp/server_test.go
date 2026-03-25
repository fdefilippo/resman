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
	"testing"

	"github.com/fdefilippo/resman/config"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name: "valid stdio config",
			cfg: &Config{
				Enabled:       true,
				Transport:     "stdio",
				LogLevel:      "INFO",
				AllowWriteOps: false,
			},
			wantErr: false,
		},
		{
			name: "valid http config",
			cfg: &Config{
				Enabled:       true,
				Transport:     "http",
				HTTPPort:      8080,
				HTTPHost:      "127.0.0.1",
				LogLevel:      "INFO",
				AllowWriteOps: false,
			},
			wantErr: false,
		},
		{
			name: "invalid transport",
			cfg: &Config{
				Enabled:   true,
				Transport: "invalid",
				LogLevel:  "INFO",
			},
			wantErr: true,
		},
		{
			name: "invalid port",
			cfg: &Config{
				Enabled:   true,
				Transport: "http",
				HTTPPort:  70000,
				HTTPHost:  "127.0.0.1",
				LogLevel:  "INFO",
			},
			wantErr: true,
		},
		{
			name: "invalid log level",
			cfg: &Config{
				Enabled:   true,
				Transport: "stdio",
				LogLevel:  "INVALID",
			},
			wantErr: true,
		},
		{
			name: "disabled config",
			cfg: &Config{
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

func TestConfigLoadFromEnv(t *testing.T) {
	t.Setenv("MCP_ENABLED", "true")
	t.Setenv("MCP_TRANSPORT", "http")
	t.Setenv("MCP_HTTP_PORT", "9090")
	t.Setenv("MCP_HTTP_HOST", "0.0.0.0")
	t.Setenv("MCP_LOG_LEVEL", "DEBUG")
	t.Setenv("MCP_ALLOW_WRITE_OPS", "true")

	cfg := DefaultConfig()
	if err := cfg.LoadFromEnv(); err != nil {
		t.Fatalf("Config.LoadFromEnv() error = %v", err)
	}

	if !cfg.Enabled {
		t.Error("Expected MCP_ENABLED to be true")
	}
	if cfg.Transport != "http" {
		t.Errorf("Expected MCP_TRANSPORT to be http, got %s", cfg.Transport)
	}
	if cfg.HTTPPort != 9090 {
		t.Errorf("Expected MCP_HTTP_PORT to be 9090, got %d", cfg.HTTPPort)
	}
	if cfg.HTTPHost != "0.0.0.0" {
		t.Errorf("Expected MCP_HTTP_HOST to be 0.0.0.0, got %s", cfg.HTTPHost)
	}
	if cfg.LogLevel != "DEBUG" {
		t.Errorf("Expected MCP_LOG_LEVEL to be DEBUG, got %s", cfg.LogLevel)
	}
	if !cfg.AllowWriteOps {
		t.Error("Expected MCP_ALLOW_WRITE_OPS to be true")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Enabled != false {
		t.Error("Expected default Enabled to be false")
	}
	if cfg.Transport != "stdio" {
		t.Errorf("Expected default Transport to be stdio, got %s", cfg.Transport)
	}
	if cfg.HTTPPort != 8080 {
		t.Errorf("Expected default HTTPPort to be 8080, got %d", cfg.HTTPPort)
	}
	if cfg.HTTPHost != "127.0.0.1" {
		t.Errorf("Expected default HTTPHost to be 127.0.0.1, got %s", cfg.HTTPHost)
	}
	if cfg.LogLevel != "INFO" {
		t.Errorf("Expected default LogLevel to be INFO, got %s", cfg.LogLevel)
	}
	if cfg.AllowWriteOps != false {
		t.Error("Expected default AllowWriteOps to be false")
	}
}

func TestHelperFunctions(t *testing.T) {
	tests := []struct {
		name     string
		m        map[string]any
		key      string
		defaultF float64
		want     float64
	}{
		{
			name:     "existing float key",
			m:        map[string]any{"cpu": 45.5},
			key:      "cpu",
			defaultF: 0.0,
			want:     45.5,
		},
		{
			name:     "missing key",
			m:        map[string]any{"other": 1},
			key:      "cpu",
			defaultF: 0.0,
			want:     0.0,
		},
		{
			name:     "wrong type",
			m:        map[string]any{"cpu": "not a number"},
			key:      "cpu",
			defaultF: 0.0,
			want:     0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getFloatMetric(tt.m, tt.key, tt.defaultF)
			if got != tt.want {
				t.Errorf("getFloatMetric() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetBool(t *testing.T) {
	m := map[string]any{
		"active":   true,
		"inactive": false,
	}

	if !getBool(m, "active", false) {
		t.Error("Expected active to be true")
	}
	if getBool(m, "inactive", true) {
		t.Error("Expected inactive to be false")
	}
	if getBool(m, "missing", true) != true {
		t.Error("Expected missing to return default true")
	}
}

func TestGetString(t *testing.T) {
	m := map[string]any{
		"path": "/test/path",
	}

	if getString(m, "path", "default") != "/test/path" {
		t.Error("Expected to get test path")
	}
	if getString(m, "missing", "default") != "default" {
		t.Error("Expected to get default value")
	}
}

func TestGetInt(t *testing.T) {
	m := map[string]any{
		"count": 42,
	}

	if getInt(m, "count", 0) != 42 {
		t.Error("Expected to get 42")
	}
	if getInt(m, "missing", 10) != 10 {
		t.Error("Expected to get default 10")
	}
}

func TestNewServer(t *testing.T) {
	parentCfg := config.DefaultConfig()
	parentCfg.MCPEnabled = false // Don't actually start the server

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
			name:    "valid cgroups URI",
			uri:     "resman://cgroups/999",
			want:    999,
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
}
