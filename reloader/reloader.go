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
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

type stateConfigManager interface {
	BeginConfigUpdate() func()
	GetConfig() *config.Config
	UpdateConfig(*config.Config)
}

type cgroupConfigManager interface {
	UpdateConfig(*config.Config) error
}

type metricsConfigCollector interface {
	UpdateConfig(*config.Config)
}

type ConfigApplyHook func(*config.Config) error

// Reloader applies one effective configuration epoch to every runtime component.
type Reloader struct {
	stateManager     stateConfigManager
	cgroupManager    cgroupConfigManager
	metricsCollector metricsConfigCollector
	applyHook        ConfigApplyHook
	logger           *logging.Logger

	applying atomic.Bool
}

// NewReloader creates a configuration reloader.
func NewReloader(
	stateMgr stateConfigManager,
	cgroupMgr cgroupConfigManager,
	metricsCol metricsConfigCollector,
	hooks ...ConfigApplyHook,
) *Reloader {

	logger := logging.GetLogger()

	reloader := &Reloader{
		stateManager:     stateMgr,
		cgroupManager:    cgroupMgr,
		metricsCollector: metricsCol,
		logger:           logger,
	}
	if len(hooks) > 0 {
		reloader.applyHook = hooks[0]
	}
	return reloader
}

// OnConfigChange validates and applies one configuration epoch.
func (r *Reloader) OnConfigChange(newConfig *config.Config) error {
	if newConfig == nil {
		return fmt.Errorf("new config cannot be nil")
	}
	if !r.applying.CompareAndSwap(false, true) {
		return fmt.Errorf("configuration reload already in progress")
	}
	defer r.applying.Store(false)

	var finishEpochUpdate func()
	if r.stateManager != nil {
		finishEpochUpdate = r.stateManager.BeginConfigUpdate()
		defer finishEpochUpdate()
	}

	r.logger.Info("Applying new configuration dynamically")

	var applyErrors []error
	var currentConfig *config.Config
	if r.stateManager != nil {
		currentConfig = r.stateManager.GetConfig()
	}
	if currentConfig != nil {
		rejected, err := config.ApplyReloadLifecycle(currentConfig, newConfig)
		if err != nil {
			return fmt.Errorf("apply configuration lifecycle: %w", err)
		}
		if len(rejected) > 0 {
			applyErrors = append(applyErrors, &config.RestartRequiredError{Fields: rejected})
		}
	}

	if newConfig.LogLevel != "" {
		r.logger.SetLevel(newConfig.LogLevel)
		r.logger.Info("Log level updated", "new_level", newConfig.LogLevel)
	}

	if r.cgroupManager != nil {
		if err := r.cgroupManager.UpdateConfig(newConfig); err != nil {
			applyErrors = append(applyErrors, fmt.Errorf("cgroup manager: %w", err))
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
			"exclude_list", newConfig.GetUserExcludeList(),
		)
	}

	if r.applyHook != nil {
		if err := r.applyHook(newConfig); err != nil {
			applyErrors = append(applyErrors, fmt.Errorf("application runtime: %w", err))
		}
	}

	return errors.Join(applyErrors...)
}
