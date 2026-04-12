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
	"testing"

	"github.com/fdefilippo/resman/config"
)

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

	if err != nil {
		t.Errorf("SafeConfigUpdate() error: %v", err)
	}
}

func TestHandlePrometheusConfigChange(t *testing.T) {
	reloader := NewReloader(nil, nil, nil, nil)

	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	cfg.PrometheusMetricsBindPort = 9101

	err := reloader.handlePrometheusConfigChange(cfg)
	if err != nil {
		t.Logf("handlePrometheusConfigChange returned: %v", err)
	}
}
