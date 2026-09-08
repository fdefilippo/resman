package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

// Manager owns read-only cgroup observation and host-authority detection.
// Runtime enforcement belongs exclusively to the authoritative systemd adapter.
type Manager struct {
	cfg               *config.Config
	cfgMu             sync.RWMutex
	enforcementStatus EnforcementStatus
}

type requiredCapabilityError struct {
	err error
}

func (e *requiredCapabilityError) Error() string { return e.err.Error() }
func (e *requiredCapabilityError) Unwrap() error { return e.err }

func newRequiredCapabilityError(err error) error {
	return &requiredCapabilityError{err: err}
}

// IsRequiredCapabilityError reports a structural cgroup capability absence
// that cannot be repaired by retrying the same startup configuration.
func IsRequiredCapabilityError(err error) bool {
	var target *requiredCapabilityError
	return errors.As(err, &target)
}

// NewManager creates the read-only cgroup observation manager.
func NewManager(cfg *config.Config) (*Manager, error) {
	logger := logging.GetLogger()
	mgr := &Manager{
		cfg:               cfg,
		enforcementStatus: DetectEnforcementStatus(defaultSystemdRuntimePath),
	}

	if err := mgr.verifyCgroupObservationSetup(); err != nil {
		return nil, fmt.Errorf("cgroup observation setup verification failed: %w", err)
	}
	logger.Info("Cgroup observation manager initialized",
		"cgroup_root", cfg.CgroupRoot,
		"enforcement_mode", mgr.enforcementStatus.Mode,
		"enforcement_reason", mgr.enforcementStatus.Reason,
	)
	return mgr, nil
}

func (m *Manager) verifyCgroupObservationSetup() error {
	cfg := m.getConfig()
	info, err := os.Stat(cfg.CgroupRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newRequiredCapabilityError(fmt.Errorf("cgroup root does not exist: %s", cfg.CgroupRoot))
		}
		return fmt.Errorf("cannot inspect cgroup root %s: %w", cfg.CgroupRoot, err)
	}
	if !info.IsDir() {
		return newRequiredCapabilityError(fmt.Errorf("cgroup root is not a directory: %s", cfg.CgroupRoot))
	}
	controllers := filepath.Join(cfg.CgroupRoot, "cgroup.controllers")
	if _, err := os.ReadFile(controllers); err != nil {
		wrapped := fmt.Errorf("cannot read cgroup.controllers at %s: %w", controllers, err)
		if errors.Is(err, os.ErrNotExist) {
			return newRequiredCapabilityError(wrapped)
		}
		return wrapped
	}
	return nil
}

// EnforcementStatus returns the immutable host ownership decision made at startup.
func (m *Manager) EnforcementStatus() EnforcementStatus {
	if m.enforcementStatus.Mode == "" {
		return EnforcementStatus{Mode: EnforcementModeObservationOnly, Reason: EnforcementReasonAuthorityUnverifiable}
	}
	return m.enforcementStatus
}

func (m *Manager) getConfig() *config.Config {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cfg
}

// UpdateConfig publishes observation configuration.
func (m *Manager) UpdateConfig(newConfig *config.Config) error {
	if newConfig == nil {
		return nil
	}
	current := m.getConfig()
	if current != nil {
		if newConfig.CgroupRoot != current.CgroupRoot {
			return fmt.Errorf("CGROUP_ROOT change requires restart: current=%s requested=%s", current.CgroupRoot, newConfig.CgroupRoot)
		}
	}
	m.cfgMu.Lock()
	defer m.cfgMu.Unlock()
	if m.cfg != current {
		return fmt.Errorf("cgroup configuration changed concurrently; retry the update")
	}
	m.cfg = newConfig
	return nil
}

// CleanupAll is intentionally empty because ResMan never owns a cgroup hierarchy.
// Authoritative systemd properties are restored by the systemd adapter first.
func (m *Manager) CleanupAll() error {
	return nil
}
