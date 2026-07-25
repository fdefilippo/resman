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
package reloader

import (
	"errors"
	"testing"

	"github.com/fdefilippo/resman/config"
)

type testStateConfigManager struct {
	cfg     *config.Config
	updates int
}

func (m *testStateConfigManager) GetConfig() *config.Config {
	return m.cfg
}

func (m *testStateConfigManager) UpdateConfig(cfg *config.Config) {
	m.cfg = cfg
	m.updates++
}

type testCgroupConfigManager struct {
	cfg *config.Config
	err error
}

func (m *testCgroupConfigManager) UpdateConfig(cfg *config.Config) error {
	m.cfg = cfg
	return m.err
}

type testMetricsConfigCollector struct {
	cfg *config.Config
}

func (c *testMetricsConfigCollector) UpdateConfig(cfg *config.Config) {
	c.cfg = cfg
}

func TestNewReloader(t *testing.T) {
	reloader := NewReloader(nil, nil, nil, nil)

	if reloader == nil {
		t.Fatal("NewReloader() returned nil")
	}

	if reloader.stateManager != nil {
		t.Error("stateManager should be nil")
	}
	if reloader.cgroupManager != nil {
		t.Error("cgroupManager should be nil")
	}
	if reloader.metricsCollector != nil {
		t.Error("metricsCollector should be nil")
	}
	if reloader.prometheusExporter != nil {
		t.Error("prometheusExporter should be nil")
	}
}

func TestOnConfigChange(t *testing.T) {
	reloader := NewReloader(nil, nil, nil, nil)

	if reloader == nil {
		t.Fatal("NewReloader() returned nil")
	}

	cfg := config.DefaultConfig()
	err := reloader.OnConfigChange(cfg)

	// Should not error with nil components
	if err != nil {
		t.Logf("OnConfigChange returned: %v", err)
	}
}

func TestSafeConfigUpdate(t *testing.T) {
	reloader := NewReloader(nil, nil, nil, nil)

	err := reloader.SafeConfigUpdate(func(c *config.Config) *config.Config {
		c.PollingInterval = 60
		return c
	})

	if err == nil {
		t.Fatal("SafeConfigUpdate() expected error with nil state manager")
	}
}

func TestOnConfigChangeDefersRestartFieldsAndAppliesRuntimeFields(t *testing.T) {
	current := config.DefaultConfig()
	current.EnablePrometheus = true
	current.PrometheusMetricsBindHost = "127.0.0.1"
	current.PrometheusMetricsBindPort = 1974
	current.CgroupRoot = "/sys/fs/cgroup"
	current.CgroupBase = "resman"
	current.LogFile = "/var/log/resman.log"
	current.LogMaxSize = 10 * 1024 * 1024
	current.UseSyslog = false
	current.MCPEnabled = true
	current.MCPTransport = "http"
	current.MCPHTTPHost = "127.0.0.1"
	current.MCPHTTPPort = 1969
	current.MCPLogLevel = "INFO"
	current.MCPAuthToken = "current-token"
	current.MCPAllowWriteOps = false

	requested := config.DefaultConfig()
	requested.CPUThreshold = 88
	requested.LogLevel = "DEBUG"
	requested.EnablePrometheus = false
	requested.PrometheusMetricsBindHost = "0.0.0.0"
	requested.PrometheusMetricsBindPort = 9101
	requested.PrometheusTLSEnabled = true
	requested.CgroupRoot = "/other/cgroup"
	requested.CgroupBase = "other"
	requested.LogFile = "/var/log/resman-debug.log"
	requested.LogMaxSize = 20 * 1024 * 1024
	requested.UseSyslog = true
	requested.MCPEnabled = false
	requested.MCPTransport = "stdio"
	requested.MCPHTTPHost = "0.0.0.0"
	requested.MCPHTTPPort = 8080
	requested.MCPLogLevel = "DEBUG"
	requested.MCPAuthToken = "rotated-token"
	requested.MCPAllowWriteOps = true

	stateManager := &testStateConfigManager{cfg: current}
	cgroupManager := &testCgroupConfigManager{}
	metricsCollector := &testMetricsConfigCollector{}
	var hookConfig *config.Config
	reloader := NewReloader(
		stateManager,
		cgroupManager,
		metricsCollector,
		nil,
		func(cfg *config.Config) error {
			hookConfig = cfg
			return nil
		},
	)

	if err := reloader.OnConfigChange(requested); err != nil {
		t.Fatalf("OnConfigChange() error: %v", err)
	}

	if requested.CPUThreshold != 88 {
		t.Fatalf("CPUThreshold = %d, want 88", requested.CPUThreshold)
	}
	if requested.LogLevel != "DEBUG" {
		t.Fatalf("LogLevel = %q, want DEBUG", requested.LogLevel)
	}
	if requested.EnablePrometheus != current.EnablePrometheus ||
		requested.PrometheusMetricsBindHost != current.PrometheusMetricsBindHost ||
		requested.PrometheusMetricsBindPort != current.PrometheusMetricsBindPort ||
		requested.PrometheusTLSEnabled != current.PrometheusTLSEnabled {
		t.Fatal("Prometheus restart-required fields were applied at runtime")
	}
	if requested.CgroupRoot != current.CgroupRoot || requested.CgroupBase != current.CgroupBase {
		t.Fatal("cgroup restart-required fields were applied at runtime")
	}
	if requested.LogFile != current.LogFile ||
		requested.LogMaxSize != current.LogMaxSize ||
		requested.UseSyslog != current.UseSyslog {
		t.Fatal("logging restart-required fields were applied at runtime")
	}
	if requested.MCPEnabled != current.MCPEnabled ||
		requested.MCPTransport != current.MCPTransport ||
		requested.MCPHTTPHost != current.MCPHTTPHost ||
		requested.MCPHTTPPort != current.MCPHTTPPort ||
		requested.MCPLogLevel != current.MCPLogLevel ||
		requested.MCPAuthToken != current.MCPAuthToken ||
		requested.MCPAllowWriteOps != current.MCPAllowWriteOps {
		t.Fatal("MCP restart-required fields were applied at runtime")
	}
	if stateManager.cfg != requested || cgroupManager.cfg != requested ||
		metricsCollector.cfg != requested || hookConfig != requested {
		t.Fatal("components did not receive the same effective config")
	}
}

func TestOnConfigChangeContinuesAfterComponentError(t *testing.T) {
	current := config.DefaultConfig()
	requested := config.DefaultConfig()
	requested.CPUThreshold = 88

	stateManager := &testStateConfigManager{cfg: current}
	cgroupManager := &testCgroupConfigManager{err: errors.New("update failed")}
	metricsCollector := &testMetricsConfigCollector{}
	hookCalled := false
	reloader := NewReloader(
		stateManager,
		cgroupManager,
		metricsCollector,
		nil,
		func(*config.Config) error {
			hookCalled = true
			return nil
		},
	)

	if err := reloader.OnConfigChange(requested); err == nil {
		t.Fatal("OnConfigChange() should report the cgroup update error")
	}
	if stateManager.cfg != requested || metricsCollector.cfg != requested || !hookCalled {
		t.Fatal("component error aborted propagation to later components")
	}
}
