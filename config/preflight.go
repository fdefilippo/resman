package config

import (
	"fmt"
	"os"
	"sort"

	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/ioweights"
)

// LoadAndValidateFile requires a regular source and shares defaults, environment
// precedence and validation with startup. It never creates a missing file.
func LoadAndValidateFile(path string) (*Config, error) {
	_, data, exists, err := readConfigSource(path, os.OpenFile)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("configuration file %s does not exist", path)
	}
	return loadAndValidateSnapshot(path, data)
}

// RestartRequiredFields returns names from the authoritative lifecycle table,
// without values or a claim about divergence from a running daemon.
func RestartRequiredFields() []string {
	var fields []string
	for name, lifecycle := range configFieldLifecycles {
		if lifecycle == LifecycleRestartRequired {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}

// LoadCPUPointsPolicy constructs the same detached policy for startup and checks.
func LoadCPUPointsPolicy(cfg *Config) (cpupoints.PolicySnapshot, error) {
	reserve, err := cpupoints.NewReservePoints(uint64(cfg.GetCPUReservePoints()))
	if err != nil {
		return cpupoints.PolicySnapshot{}, fmt.Errorf("CPU reserve: %w", err)
	}
	root, err := cpupoints.NewRootPoints(uint64(cfg.GetCPURootPoints()))
	if err != nil {
		return cpupoints.PolicySnapshot{}, fmt.Errorf("CPU root: %w", err)
	}
	best, err := cpupoints.NewBestEffortPoints(uint64(cfg.GetCPUBestEffortPoints()))
	if err != nil {
		return cpupoints.PolicySnapshot{}, fmt.Errorf("CPU best effort: %w", err)
	}
	path, err := cpupoints.NewPolicyMapPath(cfg.GetCPUPointsFile())
	if err != nil {
		return cpupoints.PolicySnapshot{}, fmt.Errorf("CPU map path: %w", err)
	}
	return cpupoints.NewPolicyLoader().Load(cpupoints.PolicyInputs{Reserve: reserve, Root: root, BestEffort: best, MapPath: path}, cpupoints.NSSIdentityResolver{})
}

// LoadIOWeightPolicy constructs the enabled detached weighted-I/O policy.
func LoadIOWeightPolicy(cfg *Config) (ioweights.PolicySnapshot, error) {
	root, err := ioweights.NewWeight(uint64(cfg.GetIORootWeight()))
	if err != nil {
		return ioweights.PolicySnapshot{}, fmt.Errorf("I/O root weight: %w", err)
	}
	def, err := ioweights.NewWeight(uint64(cfg.GetIODefaultWeight()))
	if err != nil {
		return ioweights.PolicySnapshot{}, fmt.Errorf("I/O default weight: %w", err)
	}
	if cfg.GetIOWeightDevices() == "" {
		return ioweights.NewEmptyPolicySnapshot(root, def), nil
	}
	path, err := ioweights.NewPolicyMapPath(cfg.GetIOUserWeightFile())
	if err != nil {
		return ioweights.PolicySnapshot{}, fmt.Errorf("I/O weight map path: %w", err)
	}
	return ioweights.NewPolicyLoader().Load(ioweights.PolicyInputs{Root: root, Default: def, MapPath: path}, ioweights.NSSIdentityResolver{})
}
