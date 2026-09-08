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
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/logging"
)

type stateConfigManager interface {
	BeginConfigUpdate() func()
	GetConfig() *config.Config
	UpdateConfig(*config.Config)
}

type cpuPointsStateManager interface {
	CurrentCPUPointsPolicy() cpupoints.PolicySnapshot
	ReconcileCPUPointsPolicy(cpupoints.PolicySnapshot, cpupoints.PolicySnapshot) error
	PublishCPUPointsPolicy(cpupoints.PolicySnapshot)
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

	applying         atomic.Bool
	policyLoader     *cpupoints.PolicyLoader
	identityResolver cpupoints.ExactIdentityResolver
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
		policyLoader:     cpupoints.NewPolicyLoader(),
		identityResolver: cpupoints.NSSIdentityResolver{},
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
	applyErrors = append(applyErrors, r.applyEffectiveConfig(newConfig)...)
	return errors.Join(applyErrors...)
}

// OnConfigCandidate atomically reconciles one main-config and guarantee-map epoch.
func (r *Reloader) OnConfigCandidate(newConfig *config.Config, confirm config.ReloadSourceConfirmation) config.ReloadApplyOutcome {
	outcome := config.ReloadApplyOutcome{}
	if newConfig == nil {
		outcome.Err = fmt.Errorf("new config cannot be nil")
		return outcome
	}
	if !r.applying.CompareAndSwap(false, true) {
		outcome.Err = fmt.Errorf("configuration reload already in progress")
		return outcome
	}
	defer r.applying.Store(false)
	if r.stateManager == nil {
		outcome.Err = fmt.Errorf("state manager is required for composite CPU Points reload")
		return outcome
	}

	currentConfig := r.stateManager.GetConfig()
	if currentConfig == nil {
		outcome.Err = fmt.Errorf("current configuration is required for composite CPU Points reload")
		return outcome
	}
	rejected, err := config.ApplyReloadLifecycle(currentConfig, newConfig)
	if err != nil {
		outcome.Err = fmt.Errorf("apply configuration lifecycle: %w", err)
		return outcome
	}
	var reloadErrors []error
	if len(rejected) > 0 {
		reloadErrors = append(reloadErrors, &config.RestartRequiredError{Fields: rejected})
	}

	reserve, err := cpupoints.NewReservePoints(uint64(newConfig.GetCPUReservePoints()))
	if err != nil {
		outcome.Err = fmt.Errorf("build CPU Points candidate reserve: %w", err)
		return outcome
	}
	root, err := cpupoints.NewRootPoints(uint64(newConfig.GetCPURootPoints()))
	if err != nil {
		outcome.Err = fmt.Errorf("build CPU Points candidate root entitlement: %w", err)
		return outcome
	}
	bestEffort, err := cpupoints.NewBestEffortPoints(uint64(newConfig.GetCPUBestEffortPoints()))
	if err != nil {
		outcome.Err = fmt.Errorf("build CPU Points candidate best effort: %w", err)
		return outcome
	}
	mapPath, err := cpupoints.NewPolicyMapPath(newConfig.GetCPUPointsFile())
	if err != nil {
		outcome.Err = fmt.Errorf("build CPU Points candidate map path: %w", err)
		return outcome
	}
	candidate, err := r.policyLoader.Load(cpupoints.PolicyInputs{Reserve: reserve, Root: root, BestEffort: bestEffort, MapPath: mapPath}, r.identityResolver)
	if err != nil {
		outcome.Err = fmt.Errorf("load composite CPU Points candidate: %w", err)
		return outcome
	}
	if confirm != nil {
		if err := confirm(); err != nil {
			outcome.Err = fmt.Errorf("confirm composite reload sources before reconciliation: %w", err)
			return outcome
		}
	}
	if err := r.policyLoader.ConfirmSource(candidate.Source()); err != nil {
		outcome.Err = err
		return outcome
	}

	finishEpochUpdate := r.stateManager.BeginConfigUpdate()
	defer finishEpochUpdate()
	policyManager, ok := r.stateManager.(cpuPointsStateManager)
	if !ok {
		outcome.Err = fmt.Errorf("state manager does not support CPU Points policy reconciliation")
		return outcome
	}
	oldPolicy := policyManager.CurrentCPUPointsPolicy()
	if err := policyManager.ReconcileCPUPointsPolicy(candidate, oldPolicy); err != nil {
		outcome.Processed = !isCPUPointsPreflightError(err)
		outcome.Err = err
		return outcome
	}

	if err := confirmCompositeSources(confirm, r.policyLoader, candidate.Source()); err != nil {
		restoreErr := policyManager.ReconcileCPUPointsPolicy(oldPolicy, oldPolicy)
		outcome.Processed = false
		outcome.Err = errors.Join(err, restoreErr)
		return outcome
	}

	applyErrors := r.applyEffectiveConfig(newConfig)
	if err := confirmCompositeSources(confirm, r.policyLoader, candidate.Source()); err != nil {
		rollbackErrors := r.applyEffectiveConfig(currentConfig)
		restoreErr := policyManager.ReconcileCPUPointsPolicy(oldPolicy, oldPolicy)
		outcome.Processed = false
		outcome.Err = errors.Join(err, errors.Join(applyErrors...), errors.Join(rollbackErrors...), restoreErr)
		return outcome
	}

	policyManager.PublishCPUPointsPolicy(candidate)
	outcome.Published = true
	outcome.Processed = true
	reloadErrors = append(reloadErrors, applyErrors...)
	outcome.Err = errors.Join(reloadErrors...)
	return outcome
}

func confirmCompositeSources(confirm config.ReloadSourceConfirmation, loader *cpupoints.PolicyLoader, source cpupoints.PolicySource) error {
	var errs []error
	if confirm != nil {
		errs = append(errs, confirm())
	}
	errs = append(errs, loader.ConfirmSource(source))
	return errors.Join(errs...)
}

func isCPUPointsPreflightError(err error) bool {
	type preflightError interface {
		error
		CPUPointsPreflight()
	}
	var target preflightError
	return errors.As(err, &target)
}

func (r *Reloader) applyEffectiveConfig(newConfig *config.Config) []error {
	r.logger.Info("Applying new configuration dynamically")
	var applyErrors []error

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

	return applyErrors
}
