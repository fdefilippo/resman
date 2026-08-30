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
	"strings"
)

// MCPServerConfig is the validated configuration contract consumed by the MCP
// server. Public MCP keys are parsed only by configFieldHandlers before this
// snapshot is constructed.
type MCPServerConfig struct {
	Enabled       bool
	Transport     string
	HTTPPort      int
	HTTPHost      string
	TLSEnabled    bool
	TLSCertFile   string
	TLSKeyFile    string
	TLSCAFile     string
	TLSMinVersion string
	LogLevel      string
	AuthToken     string
	AllowWriteOps bool
}

// MCPServerConfig returns the complete MCP server configuration snapshot.
func (cfg *Config) MCPServerConfig() MCPServerConfig {
	return MCPServerConfig{
		Enabled:       cfg.MCPEnabled,
		Transport:     cfg.MCPTransport,
		HTTPPort:      cfg.MCPHTTPPort,
		HTTPHost:      cfg.MCPHTTPHost,
		TLSEnabled:    cfg.MCPTLSEnabled,
		TLSCertFile:   cfg.MCPTLSCertFile,
		TLSKeyFile:    cfg.MCPTLSKeyFile,
		TLSCAFile:     cfg.MCPTLSCAFile,
		TLSMinVersion: cfg.MCPTLSMinVersion,
		LogLevel:      cfg.MCPLogLevel,
		AuthToken:     cfg.MCPAuthToken,
		AllowWriteOps: cfg.MCPAllowWriteOps,
	}
}

// Validate enforces the single MCP transport and security contract used at
// both configuration load and server construction.
func (cfg MCPServerConfig) Validate() error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Transport != "stdio" && cfg.Transport != "http" {
		return fmt.Errorf("MCP_TRANSPORT must be stdio or http, got %q", cfg.Transport)
	}
	if cfg.Transport == "http" {
		if cfg.HTTPPort < 1 || cfg.HTTPPort > 65535 {
			return fmt.Errorf("MCP_HTTP_PORT must be between 1 and 65535, got %d", cfg.HTTPPort)
		}
		if strings.TrimSpace(cfg.AuthToken) == "" {
			return fmt.Errorf("MCP_AUTH_TOKEN must be set when MCP_ENABLED=true and MCP_TRANSPORT=http")
		}
		if !cfg.TLSEnabled {
			return fmt.Errorf("MCP_TLS_ENABLED must be true when MCP_ENABLED=true and MCP_TRANSPORT=http")
		}
		if strings.TrimSpace(cfg.TLSCertFile) == "" || strings.TrimSpace(cfg.TLSKeyFile) == "" {
			return fmt.Errorf("MCP_TLS_CERT_FILE and MCP_TLS_KEY_FILE must be set when MCP HTTP is enabled")
		}
		if !isValidTLSVersion(cfg.TLSMinVersion) {
			return fmt.Errorf("MCP_TLS_MIN_VERSION must be one of: 1.0, 1.1, 1.2, 1.3")
		}
	}

	switch cfg.LogLevel {
	case "DEBUG", "INFO", "WARN", "ERROR":
		return nil
	default:
		return fmt.Errorf("MCP_LOG_LEVEL must be one of: DEBUG, INFO, WARN, ERROR")
	}
}
