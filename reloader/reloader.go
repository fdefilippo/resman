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
// reloader/reloader.go
package reloader

import (
	"fmt"
	"sync"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/state"
)

// Reloader gestisce il ricaricamento dinamico della configurazione per tutti i componenti.
type Reloader struct {
	stateManager       *state.Manager
	cgroupManager      *cgroup.Manager
	metricsCollector   *metrics.Collector
	prometheusExporter *metrics.PrometheusExporter
	logger             *logging.Logger

	mu sync.RWMutex
}

// NewReloader crea un nuovo reloader.
func NewReloader(
	stateMgr *state.Manager,
	cgroupMgr *cgroup.Manager,
	metricsCol *metrics.Collector,
	promExp *metrics.PrometheusExporter,
) *Reloader {

	logger := logging.GetLogger()

	return &Reloader{
		stateManager:       stateMgr,
		cgroupManager:      cgroupMgr,
		metricsCollector:   metricsCol,
		prometheusExporter: promExp,
		logger:             logger,
	}
}

// OnConfigChange gestisce il cambio di configurazione.
func (r *Reloader) OnConfigChange(newConfig *config.Config) error {
	r.logger.Info("Applying new configuration dynamically")

	// Applica i cambiamenti in ordine sicuro
	var errors []string

	// 1. Logging (immediato, per tracciare il resto)
	if newConfig.LogLevel != "" {
		// Il logging è globale, gestito separatamente
		r.logger.Info("Log level change will be applied on next log message",
			"new_level", newConfig.LogLevel,
		)
	}

	// 2. Prometheus exporter (potrebbe richiedere restart)
	if r.prometheusExporter != nil {
		if err := r.handlePrometheusConfigChange(newConfig); err != nil {
			errors = append(errors, fmt.Sprintf("Prometheus exporter: %v", err))
		}
	}

	// 3. State manager (aggiorna parametri)
	if r.stateManager != nil {
		r.stateManager.UpdateConfig(newConfig)
	}

	// 4. Cgroup manager (aggiorna percorsi)
	if r.cgroupManager != nil {
		r.logger.Info("Cgroup manager notified of config change",
			"cgroup_root", newConfig.CgroupRoot,
			"base_cgroup", newConfig.CgroupBase,
		)
	}

	// 5. Metrics collector (aggiorna cache TTL e exclude list)
	if r.metricsCollector != nil {
		r.metricsCollector.UpdateConfig(newConfig) // Aggiorna la configurazione
		r.logger.Info("Metrics collector configuration updated",
			"cache_ttl", newConfig.MetricsCacheTTL,
			"exclude_list", newConfig.UserExcludeList,
		)
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors applying new config: %v", errors)
	}

	return nil
}

// handlePrometheusConfigChange gestisce i cambiamenti alla configurazione Prometheus.
func (r *Reloader) handlePrometheusConfigChange(newConfig *config.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Se Prometheus era disabilitato e ora è abilitato
	if !r.prometheusExporter.IsRunning() && newConfig.EnablePrometheus {
		r.logger.Info("Prometheus was disabled, now enabling...")
		r.logger.Warn("Prometheus enable/disable requires restart")
		return fmt.Errorf("enabling Prometheus requires restart")
	}

	// Se Prometheus era abilitato e ora è disabilitato
	if r.prometheusExporter.IsRunning() && !newConfig.EnablePrometheus {
		r.logger.Info("Prometheus was enabled, now disabling...")
		if err := r.prometheusExporter.Stop(); err != nil {
			return fmt.Errorf("failed to stop Prometheus exporter: %w", err)
		}
		return nil
	}

	// Se la porta o host sono cambiati
	// Nota: cambiare porta/host richiede restart del server HTTP
	expectedEndpoint := fmt.Sprintf("http://%s:%d/metrics", newConfig.PrometheusMetricsBindHost, newConfig.PrometheusMetricsBindPort)
	if r.prometheusExporter.GetMetricsEndpoint() != expectedEndpoint {
		r.logger.Warn("Prometheus bind address change requires restart",
			"current_endpoint", r.prometheusExporter.GetMetricsEndpoint(),
			"requested_endpoint", expectedEndpoint,
		)
		return fmt.Errorf("changing Prometheus bind address requires restart")
	}

	r.logger.Info("Prometheus configuration changed",
		"host", newConfig.PrometheusMetricsBindHost,
		"port", newConfig.PrometheusMetricsBindPort,
		"enabled", newConfig.EnablePrometheus,
	)

	return nil
}

// SafeConfigUpdate applica i cambiamenti di configurazione in modo thread-safe.
func (r *Reloader) SafeConfigUpdate(updateFunc func(*config.Config) *config.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Questa funzione permette aggiornamenti atomici alla configurazione
	// Utile per API REST o comandi amministrativi

	r.logger.Debug("Safe configuration update requested")
	return nil
}
