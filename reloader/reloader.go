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

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

type stateConfigManager interface {
	GetConfig() *config.Config
	UpdateConfig(*config.Config)
}

type cgroupConfigManager interface {
	UpdateConfig(*config.Config) error
}

type metricsConfigCollector interface {
	UpdateConfig(*config.Config)
}

type prometheusConfigExporter interface {
	IsRunning() bool
	GetMetricsEndpoint() string
}

type ConfigApplyHook func(*config.Config) error

// Reloader gestisce il ricaricamento dinamico della configurazione per tutti i componenti.
type Reloader struct {
	stateManager       stateConfigManager
	cgroupManager      cgroupConfigManager
	metricsCollector   metricsConfigCollector
	prometheusExporter prometheusConfigExporter
	applyHook          ConfigApplyHook
	logger             *logging.Logger

	mu sync.Mutex
}

// NewReloader crea un nuovo reloader.
func NewReloader(
	stateMgr stateConfigManager,
	cgroupMgr cgroupConfigManager,
	metricsCol metricsConfigCollector,
	promExp prometheusConfigExporter,
	hooks ...ConfigApplyHook,
) *Reloader {

	logger := logging.GetLogger()

	reloader := &Reloader{
		stateManager:       stateMgr,
		cgroupManager:      cgroupMgr,
		metricsCollector:   metricsCol,
		prometheusExporter: promExp,
		logger:             logger,
	}
	if len(hooks) > 0 {
		reloader.applyHook = hooks[0]
	}
	return reloader
}

// OnConfigChange gestisce il cambio di configurazione.
func (r *Reloader) OnConfigChange(newConfig *config.Config) error {
	if newConfig == nil {
		return fmt.Errorf("new config cannot be nil")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.logger.Info("Applying new configuration dynamically")

	var errors []string
	var currentConfig *config.Config
	if r.stateManager != nil {
		currentConfig = r.stateManager.GetConfig()
	}
	if currentConfig != nil {
		r.preserveRestartRequiredConfig(currentConfig, newConfig)
	}

	if newConfig.LogLevel != "" {
		r.logger.SetLevel(newConfig.LogLevel)
		r.logger.Info("Log level updated", "new_level", newConfig.LogLevel)
	}

	if r.cgroupManager != nil {
		if err := r.cgroupManager.UpdateConfig(newConfig); err != nil {
			errors = append(errors, fmt.Sprintf("Cgroup manager: %v", err))
		} else {
			r.logger.Info("Cgroup manager configuration updated",
				"cgroup_root", newConfig.CgroupRoot,
				"base_cgroup", newConfig.CgroupBase,
			)
		}
	}

	if r.stateManager != nil {
		r.stateManager.UpdateConfig(newConfig)
	}

	if r.metricsCollector != nil {
		r.metricsCollector.UpdateConfig(newConfig)
		r.logger.Info("Metrics collector configuration updated",
			"cache_ttl", newConfig.MetricsCacheTTL,
			"exclude_list", newConfig.UserExcludeList,
		)
	}

	if r.applyHook != nil {
		if err := r.applyHook(newConfig); err != nil {
			errors = append(errors, fmt.Sprintf("Application runtime: %v", err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors applying new config: %v", errors)
	}

	return nil
}

func (r *Reloader) preserveRestartRequiredConfig(currentConfig, newConfig *config.Config) {
	r.preserveRestartBool("ENABLE_PROMETHEUS", currentConfig.EnablePrometheus, &newConfig.EnablePrometheus)
	r.preserveRestartString("PROMETHEUS_METRICS_BIND_HOST", currentConfig.PrometheusMetricsBindHost, &newConfig.PrometheusMetricsBindHost)
	r.preserveRestartInt("PROMETHEUS_METRICS_BIND_PORT", currentConfig.PrometheusMetricsBindPort, &newConfig.PrometheusMetricsBindPort)
	r.preserveRestartBool("PROMETHEUS_TLS_ENABLED", currentConfig.PrometheusTLSEnabled, &newConfig.PrometheusTLSEnabled)
	r.preserveRestartString("PROMETHEUS_TLS_CERT_FILE", currentConfig.PrometheusTLSCertFile, &newConfig.PrometheusTLSCertFile)
	r.preserveRestartString("PROMETHEUS_TLS_KEY_FILE", currentConfig.PrometheusTLSKeyFile, &newConfig.PrometheusTLSKeyFile)
	r.preserveRestartString("PROMETHEUS_TLS_CA_FILE", currentConfig.PrometheusTLSCAFile, &newConfig.PrometheusTLSCAFile)
	r.preserveRestartString("PROMETHEUS_TLS_MIN_VERSION", currentConfig.PrometheusTLSMinVersion, &newConfig.PrometheusTLSMinVersion)
	r.preserveRestartString("PROMETHEUS_AUTH_TYPE", currentConfig.PrometheusAuthType, &newConfig.PrometheusAuthType)
	r.preserveRestartString("PROMETHEUS_AUTH_USERNAME", currentConfig.PrometheusAuthUsername, &newConfig.PrometheusAuthUsername)
	r.preserveRestartString("PROMETHEUS_AUTH_PASSWORD_FILE", currentConfig.PrometheusAuthPasswordFile, &newConfig.PrometheusAuthPasswordFile)
	r.preserveRestartString("PROMETHEUS_JWT_SECRET_FILE", currentConfig.PrometheusJWTSecretFile, &newConfig.PrometheusJWTSecretFile)
	r.preserveRestartString("PROMETHEUS_JWT_ISSUER", currentConfig.PrometheusJWTIssuer, &newConfig.PrometheusJWTIssuer)
	r.preserveRestartString("PROMETHEUS_JWT_AUDIENCE", currentConfig.PrometheusJWTAudience, &newConfig.PrometheusJWTAudience)
	r.preserveRestartInt("PROMETHEUS_JWT_EXPIRY", currentConfig.PrometheusJWTExpiry, &newConfig.PrometheusJWTExpiry)
	r.preserveRestartString("CGROUP_ROOT", currentConfig.CgroupRoot, &newConfig.CgroupRoot)
	r.preserveRestartString("CGROUP_BASE", currentConfig.CgroupBase, &newConfig.CgroupBase)
}

func (r *Reloader) preserveRestartString(field, current string, requested *string) {
	if *requested == current {
		return
	}
	r.logRestartRequired(field, current, *requested)
	*requested = current
}

func (r *Reloader) preserveRestartInt(field string, current int, requested *int) {
	if *requested == current {
		return
	}
	r.logRestartRequired(field, current, *requested)
	*requested = current
}

func (r *Reloader) preserveRestartBool(field string, current bool, requested *bool) {
	if *requested == current {
		return
	}
	r.logRestartRequired(field, current, *requested)
	*requested = current
}

func (r *Reloader) logRestartRequired(field string, current, requested any) {
	r.logger.Warn("Configuration change deferred until restart",
		"field", field,
		"current", current,
		"requested", requested,
	)
}

// SafeConfigUpdate applica i cambiamenti di configurazione in modo thread-safe.
func (r *Reloader) SafeConfigUpdate(updateFunc func(*config.Config) *config.Config) error {
	r.logger.Debug("Safe configuration update requested")

	if r.stateManager == nil {
		return fmt.Errorf("state manager not initialized")
	}

	// Ottieni la configurazione corrente dal state manager
	currentCfg := r.stateManager.GetConfig()

	// Applica la funzione di update
	newCfg := updateFunc(currentCfg)
	if newCfg == nil {
		return fmt.Errorf("updateFunc returned nil config")
	}

	// Applica la nuova configurazione a tutti i componenti
	return r.OnConfigChange(newCfg)
}
