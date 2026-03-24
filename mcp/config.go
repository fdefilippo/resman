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
// mcp/config.go
package mcp

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config contains MCP server configuration
type Config struct {
	Enabled       bool
	Transport     string // stdio, http, sse
	HTTPPort      int
	HTTPHost      string
	LogLevel      string
	AuthToken     string // Optional authentication token for HTTP/SSE
	AllowWriteOps bool   // Allow write operations (activate/deactivate limits)
}

// DefaultConfig returns default MCP configuration
func DefaultConfig() *Config {
	return &Config{
		Enabled:       false,
		Transport:     "stdio",
		HTTPPort:      8080,
		HTTPHost:      "127.0.0.1",
		LogLevel:      "INFO",
		AuthToken:     "",
		AllowWriteOps: false,
	}
}

// LoadFromParentConfig loads MCP config from parent config
func LoadFromParentConfig(cfg interface{}) *Config {
	mcpCfg := DefaultConfig()

	// Use reflection or type assertion to extract MCP-related fields
	// This is a simplified version - in production you'd use reflection
	if config, ok := cfg.(*Config); ok {
		return config
	}

	return mcpCfg
}

// LoadFromEnv loads MCP configuration from environment variables
func (c *Config) LoadFromEnv() error {
	if val := os.Getenv("MCP_ENABLED"); val != "" {
		c.Enabled = strings.ToLower(val) == "true" || val == "1"
	}

	if val := os.Getenv("MCP_TRANSPORT"); val != "" {
		c.Transport = strings.ToLower(val)
		if c.Transport != "stdio" && c.Transport != "http" {
			return fmt.Errorf("invalid MCP_TRANSPORT: %s (must be stdio or http)", c.Transport)
		}
	}

	if val := os.Getenv("MCP_HTTP_PORT"); val != "" {
		port, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid MCP_HTTP_PORT: %s", val)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid MCP_HTTP_PORT: %d (must be 1-65535)", port)
		}
		c.HTTPPort = port
	}

	if val := os.Getenv("MCP_HTTP_HOST"); val != "" {
		c.HTTPHost = val
	}

	if val := os.Getenv("MCP_LOG_LEVEL"); val != "" {
		c.LogLevel = strings.ToUpper(val)
	}

	if val := os.Getenv("MCP_AUTH_TOKEN"); val != "" {
		c.AuthToken = val
	}

	if val := os.Getenv("MCP_ALLOW_WRITE_OPS"); val != "" {
		c.AllowWriteOps = strings.ToLower(val) == "true" || val == "1"
	}

	return nil
}

// Validate validates MCP configuration
func (c *Config) Validate() error {
	if c.Enabled {
		if c.Transport != "stdio" && c.Transport != "http" {
			return fmt.Errorf("invalid transport: %s (must be stdio or http)", c.Transport)
		}

		if c.Transport == "http" {
			if c.HTTPPort < 1 || c.HTTPPort > 65535 {
				return fmt.Errorf("invalid HTTP port: %d", c.HTTPPort)
			}
		}

		validLogLevels := map[string]bool{
			"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true,
		}
		if !validLogLevels[c.LogLevel] {
			return fmt.Errorf("invalid log level: %s", c.LogLevel)
		}
	}

	return nil
}
