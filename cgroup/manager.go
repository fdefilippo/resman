package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/operationgate"
	"github.com/fdefilippo/resman/logging"
)

const (
	defaultFilePerm = 0644
	// Note: cleanupRetryDelay, processMoveDelay, etc. are now configurable via config
)

// Manager owns cgroup v2 discovery, lifecycle, and enforcement operations.
type Manager struct {
	cfg             *config.Config
	logger          *logging.Logger
	cfgMu           sync.RWMutex
	mu              sync.RWMutex
	wg              sync.WaitGroup
	originMu        sync.Mutex
	originGate      operationgate.Gate
	blockIOMu       sync.Mutex
	processScanGate operationgate.Gate
	usernameMu      sync.RWMutex

	// Managed cgroup tracking.
	createdCgroups      map[int]string // UID -> cgroup path
	createdCgroupsFile  string
	processOrigins      map[int]processOrigin
	processOriginsFile  string
	blockIOAccounting   map[int]blockIOAccountingState
	procRoot            string
	sysBlockRoot        string
	writePID            func(string, int) error
	persistOrigins      func() error
	processScan         processScanCache
	usernameCache       map[string]cachedUsername
	resolveUsername     func(string) (string, error)
	scanProcessIDs      func() (map[int][]int, error)
	createCgroupProbe   func(string, string) (string, error)
	removeCgroupProbe   func(string) error
	writeController     func(string, string) error
	removeManagedCgroup func(string) (cgroupRemovalResult, error)
	observeRemovalRetry func()
	readBlockIOStats    func(string) (blockIOCounters, error)
	readCgroupFile      func(string) ([]byte, error)
	readPIDNamespace    func(int) (pidNamespaceIdentity, error)
	moveUserProcesses   func(context.Context, int) error
	operationTimeout    func() time.Duration
	pidNamespace        pidNamespaceIdentity

	// Cached verification state.
	cgroupRootWritable         bool
	usableControllerInterfaces map[string]bool
}

type controllerRequirement struct {
	feature       string
	controller    string
	interfaceFile string
}

type requiredCapabilityError struct {
	err error
}

func (e *requiredCapabilityError) Error() string {
	return e.err.Error()
}

func (e *requiredCapabilityError) Unwrap() error {
	return e.err
}

func newRequiredCapabilityError(err error) error {
	return &requiredCapabilityError{err: err}
}

// IsRequiredCapabilityError reports a structural cgroup capability absence
// that cannot be repaired by retrying the same startup configuration.
func IsRequiredCapabilityError(err error) bool {
	var target *requiredCapabilityError
	return errors.As(err, &target)
}

// NewManager creates a cgroup v2 manager.
func NewManager(cfg *config.Config) (*Manager, error) {
	logger := logging.GetLogger()
	pidNamespace, err := readPIDNamespaceIdentity("/proc/self/ns/pid")
	if err != nil {
		return nil, fmt.Errorf("failed to identify the ResMan PID namespace at /proc/self/ns/pid: %w", err)
	}

	mgr := &Manager{
		cfg:                 cfg,
		logger:              logger,
		createdCgroups:      make(map[int]string),
		createdCgroupsFile:  cfg.CreatedCgroupsFile,
		processOrigins:      make(map[int]processOrigin),
		blockIOAccounting:   make(map[int]blockIOAccountingState),
		processOriginsFile:  processOriginsPath(cfg.CreatedCgroupsFile),
		procRoot:            "/proc",
		sysBlockRoot:        "/sys/block",
		usernameCache:       make(map[string]cachedUsername),
		createCgroupProbe:   os.MkdirTemp,
		removeCgroupProbe:   os.Remove,
		removeManagedCgroup: removeCgroupWithRetry,
		observeRemovalRetry: func() {
			logger.Debug("Managed cgroup removal entered retry", "operation", "remove_managed_cgroup")
		},
		readBlockIOStats: readBlockIOCounters,
		readCgroupFile:   os.ReadFile,
		pidNamespace:     pidNamespace,
	}
	mgr.moveUserProcesses = func(ctx context.Context, uid int) error {
		_, err := mgr.moveAllUserProcesses(ctx, uid)
		return err
	}
	mgr.operationTimeout = func() time.Duration {
		return time.Duration(mgr.getConfig().GetCgroupOperationTimeout()) * time.Second
	}

	// Verify that cgroups v2 provides every interface required by enabled features.
	if err := mgr.verifyCgroupSetup(); err != nil {
		return nil, fmt.Errorf("cgroup setup verification failed: %w", err)
	}

	// Load cgroups created by a previous daemon instance, if any.
	if err := mgr.loadExistingCgroups(); err != nil {
		logger.Warn("Could not load existing cgroups tracking file", "error", err)
	}
	if err := mgr.loadProcessOrigins(); err != nil {
		return nil, fmt.Errorf("failed to load process origin state: %w", err)
	}
	if err := mgr.pruneInactiveProcessOrigins(-1); err != nil {
		return nil, fmt.Errorf("failed to reconcile process origin state: %w", err)
	}
	if isFiniteCPUQuota(cfg.CPUQuotaNormal) {
		logger.Warn("Finite CPU_QUOTA_NORMAL applies only to resman recovery cgroups",
			"quota", cfg.CPUQuotaNormal,
			"recovery_path", mgr.getRecoveryRootPath(),
		)
	}

	logger.Info("Cgroup manager initialized",
		"cgroup_root", cfg.CgroupRoot,
		"base_cgroup", cfg.CgroupBase,
	)

	return mgr, nil
}

// verifyCgroupSetup verifies and prepares the cgroup v2 hierarchy.
func (m *Manager) verifyCgroupSetup() error {
	cfg := m.getConfig()
	requirements := enabledControllerInterfaces(cfg)
	controllerInterfaces := allControllerInterfaces()

	// 1. Verify that the cgroup root exists.
	if _, err := os.Stat(cfg.CgroupRoot); err != nil {
		if os.IsNotExist(err) {
			return newRequiredCapabilityError(fmt.Errorf("cgroup root does not exist: %s (enable cgroups v2 and PSI on Enterprise Linux compatible systems: grubby --update-kernel=ALL --args='systemd.unified_cgroup_hierarchy=1 psi=1')", cfg.CgroupRoot))
		}
		return fmt.Errorf("cannot inspect cgroup root %s: %w", cfg.CgroupRoot, err)
	}

	// 2. Verify cgroups v2 and the controllers required by enabled features.
	controllersFile := filepath.Join(cfg.CgroupRoot, "cgroup.controllers")
	controllersData, err := os.ReadFile(controllersFile)
	if err != nil {
		readErr := fmt.Errorf("cannot read cgroup.controllers at %s: %w", controllersFile, err)
		if os.IsNotExist(err) {
			return newRequiredCapabilityError(readErr)
		}
		return readErr
	}
	m.logger.Info("Available cgroup controllers",
		"controllers", strings.TrimSpace(string(controllersData)),
	)
	if err := verifyRequiredControllers(string(controllersData), requirements); err != nil {
		return err
	}

	// 3. Enable every mandatory controller at the hierarchy root.
	subtreeControlFile := filepath.Join(cfg.CgroupRoot, "cgroup.subtree_control")
	if _, err := os.ReadFile(subtreeControlFile); err != nil {
		return fmt.Errorf("failed to read cgroup.subtree_control at %s: %w", subtreeControlFile, err)
	}
	rootCandidates := availableControllerInterfaces(string(controllersData), controllerInterfaces)
	rootEnabled, err := m.enableControllerInterfaces(subtreeControlFile, rootCandidates, requirements)
	if err != nil {
		return err
	}
	m.enableOptionalCPUSet(subtreeControlFile, string(controllersData), "cgroup root")

	// 4. Verify write access without migrating the daemon out of its service cgroup.
	if err := m.verifyCgroupRootWriteAccess(); err != nil {
		return err
	}

	// 5. Create the base cgroup if necessary.
	baseCgroupPath := m.getBaseCgroupPath()
	if err := os.MkdirAll(baseCgroupPath, 0755); err != nil {
		return fmt.Errorf("failed to create base cgroup directory %s: %w", baseCgroupPath, err)
	}

	// Verify the directory is a valid cgroup (kernel populates control files)
	// If cgroup.subtree_control is missing, the directory is stale (not a cgroup).
	// Remove and recreate with plain Mkdir to trigger kernel cgroup creation.
	subtreeCheck := filepath.Join(baseCgroupPath, "cgroup.subtree_control")
	if _, err := os.Stat(subtreeCheck); os.IsNotExist(err) {
		m.logger.Warn("Base cgroup directory exists but is not a valid cgroup, recreating",
			"path", baseCgroupPath,
		)
		if err := os.Remove(baseCgroupPath); err != nil {
			return fmt.Errorf("failed to remove stale cgroup directory %s: %w", baseCgroupPath, err)
		}
		if err := os.Mkdir(baseCgroupPath, 0755); err != nil {
			return fmt.Errorf("failed to recreate base cgroup %s: %w", baseCgroupPath, err)
		}
	}

	// Enable mandatory controllers in the resman base cgroup.
	baseSubtreeControl := filepath.Join(baseCgroupPath, "cgroup.subtree_control")
	baseEnabled, err := m.enableControllerInterfaces(baseSubtreeControl, rootEnabled, requirements)
	if err != nil {
		return err
	}
	m.enableOptionalCPUSet(baseSubtreeControl, string(controllersData), "resman base cgroup")

	// Controller names are only declarations. A real child proves that the kernel
	// exposes the interface files that enforcement will write.
	usable, err := m.probeControllerInterfaces(baseCgroupPath, baseEnabled)
	if err != nil {
		return err
	}
	if err := verifyUsableControllerInterfaces(usable, requirements); err != nil {
		return err
	}
	m.usableControllerInterfaces = usable

	m.logger.Debug("Cgroup setup verified successfully")
	return nil
}

func hasController(controllers, wanted string) bool {
	for _, controller := range strings.Fields(controllers) {
		if controller == wanted {
			return true
		}
	}
	return false
}

func allControllerInterfaces() []controllerRequirement {
	return []controllerRequirement{
		{feature: "CPU limiting", controller: "cpu", interfaceFile: "cpu.max"},
		{feature: "RAM limiting", controller: "memory", interfaceFile: "memory.max"},
		{feature: "I/O limiting", controller: "io", interfaceFile: "io.max"},
	}
}

func enabledControllerInterfaces(cfg *config.Config) []controllerRequirement {
	all := allControllerInterfaces()
	requirements := []controllerRequirement{all[0]}
	if cfg.RAMEnabled {
		requirements = append(requirements, all[1])
	}
	if cfg.IOEnabled {
		requirements = append(requirements, all[2])
	}
	return requirements
}

func availableControllerInterfaces(available string, interfaces []controllerRequirement) []controllerRequirement {
	result := make([]controllerRequirement, 0, len(interfaces))
	for _, candidate := range interfaces {
		if hasController(available, candidate.controller) {
			result = append(result, candidate)
		}
	}
	return result
}

func verifyRequiredControllers(available string, requirements []controllerRequirement) error {
	for _, requirement := range requirements {
		if hasController(available, requirement.controller) {
			continue
		}
		return newRequiredCapabilityError(fmt.Errorf("enabled feature %s requires cgroup controller %q and interface %q, but the controller is unavailable (available: %s)",
			requirement.feature,
			requirement.controller,
			requirement.interfaceFile,
			strings.TrimSpace(available),
		))
	}
	return nil
}

func (m *Manager) enableControllerInterfaces(subtreeControlFile string, candidates, requirements []controllerRequirement) ([]controllerRequirement, error) {
	enabled := make([]controllerRequirement, 0, len(candidates))
	writeController := m.writeController
	if writeController == nil {
		writeController = m.writeControllerIfMissing
	}
	for _, candidate := range candidates {
		if err := writeController(subtreeControlFile, "+"+candidate.controller); err != nil {
			if controllerInterfaceRequired(candidate, requirements) {
				return nil, fmt.Errorf("enabled feature %s requires cgroup controller %q and interface %q: failed to enable the controller through %s: %w",
					candidate.feature,
					candidate.controller,
					candidate.interfaceFile,
					subtreeControlFile,
					err,
				)
			}
			m.logger.Debug("Unused cgroup controller could not be enabled for capability discovery",
				"controller", candidate.controller,
				"interface", candidate.interfaceFile,
				"subtree_control", subtreeControlFile,
				"error", err,
			)
			continue
		}
		enabled = append(enabled, candidate)
	}
	return enabled, nil
}

func controllerInterfaceRequired(candidate controllerRequirement, requirements []controllerRequirement) bool {
	for _, requirement := range requirements {
		if candidate.interfaceFile == requirement.interfaceFile {
			return true
		}
	}
	return false
}

func (m *Manager) enableOptionalCPUSet(subtreeControlFile, available, scope string) {
	if !hasController(available, "cpuset") {
		m.logger.Warn("Optional cpuset controller unavailable; CPU limiting remains enabled",
			"scope", scope,
			"interface", "cpuset.cpus",
		)
		return
	}
	if err := m.writeControllerIfMissing(subtreeControlFile, "+cpuset"); err != nil {
		m.logger.Warn("Optional cpuset controller could not be enabled; CPU limiting remains enabled",
			"scope", scope,
			"interface", "cpuset.cpus",
			"error", err,
		)
	}
}

func (m *Manager) probeControllerInterfaces(baseCgroupPath string, candidates []controllerRequirement) (usable map[string]bool, retErr error) {
	createProbe := m.createCgroupProbe
	if createProbe == nil {
		createProbe = os.MkdirTemp
	}
	probePath, err := createProbe(baseCgroupPath, ".resman-capability-probe-")
	if err != nil {
		return nil, fmt.Errorf("failed to create cgroup capability probe below %s: %w", baseCgroupPath, err)
	}
	removeProbe := m.removeCgroupProbe
	if removeProbe == nil {
		removeProbe = os.Remove
	}
	defer func() {
		if err := removeProbe(probePath); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("failed to remove cgroup capability probe %s: %w", probePath, err))
		}
	}()

	usable = make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		interfacePath := filepath.Join(probePath, candidate.interfaceFile)
		_, err := os.Stat(interfacePath)
		usable[candidate.interfaceFile] = err == nil
	}
	return usable, nil
}

func verifyUsableControllerInterfaces(usable map[string]bool, requirements []controllerRequirement) error {
	for _, requirement := range requirements {
		if usable[requirement.interfaceFile] {
			continue
		}
		return newRequiredCapabilityError(fmt.Errorf("enabled feature %s requires cgroup controller %q and interface %q, but the interface is unusable in a real child cgroup",
			requirement.feature,
			requirement.controller,
			requirement.interfaceFile,
		))
	}
	return nil
}

func (m *Manager) verifyCgroupRootWriteAccess() error {
	cfg := m.getConfig()
	procsPath := filepath.Join(cfg.CgroupRoot, "cgroup.procs")

	file, err := os.OpenFile(procsPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("cannot open cgroup root process file %s for writing: %w", procsPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close cgroup root process file %s: %w", procsPath, err)
	}

	m.cgroupRootWritable = true
	return nil
}

// writeControllerIfMissing enables a controller unless it is already enabled.
func (m *Manager) writeControllerIfMissing(filePath, controller string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	if controller == "" || len(controller) < 2 {
		return fmt.Errorf("invalid controller %q", controller)
	}
	controllerName := controller[1:]
	for _, enabled := range strings.Fields(string(data)) {
		if enabled == controllerName {
			return nil
		}
	}

	return os.WriteFile(filePath, []byte(controller), 0644)
}

func (m *Manager) getConfig() *config.Config {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cfg
}

// UpdateConfig publishes a runtime configuration after capability checks.
func (m *Manager) UpdateConfig(newConfig *config.Config) error {
	if newConfig == nil {
		return nil
	}

	currentConfig := m.getConfig()
	if currentConfig != nil {
		if newConfig.CgroupRoot != currentConfig.CgroupRoot {
			return fmt.Errorf("CGROUP_ROOT change requires restart: current=%s requested=%s", currentConfig.CgroupRoot, newConfig.CgroupRoot)
		}
		if newConfig.CgroupBase != currentConfig.CgroupBase {
			return fmt.Errorf("CGROUP_BASE change requires restart: current=%s requested=%s", currentConfig.CgroupBase, newConfig.CgroupBase)
		}
	}
	var capabilityErrors []error
	for _, requirement := range enabledControllerInterfaces(newConfig) {
		if m.usableControllerInterfaces[requirement.interfaceFile] {
			continue
		}
		switch requirement.interfaceFile {
		case "memory.max":
			newConfig.RAMEnabled = false
		case "io.max":
			newConfig.IOEnabled = false
		default:
			return fmt.Errorf("enabled feature %s requires cgroup controller %q and interface %q, but startup capability discovery marked it unusable",
				requirement.feature,
				requirement.controller,
				requirement.interfaceFile,
			)
		}
		capabilityErrors = append(capabilityErrors,
			fmt.Errorf("disabled requested feature %s because cgroup controller %q interface %q was unusable at startup",
				requirement.feature,
				requirement.controller,
				requirement.interfaceFile,
			),
		)
	}

	if currentConfig != nil {
		newRequirements := newlyEnabledControllerInterfaces(currentConfig, newConfig)
		if len(newRequirements) > 0 {
			sharedPath := filepath.Join(currentConfig.CgroupRoot, currentConfig.CgroupBase, "limited")
			if _, err := os.Stat(sharedPath); err == nil {
				subtreeControl := filepath.Join(sharedPath, "cgroup.subtree_control")
				for _, requirement := range newRequirements {
					if _, err := m.enableControllerInterfaces(subtreeControl, []controllerRequirement{requirement}, []controllerRequirement{requirement}); err != nil {
						disableControllerFeature(newConfig, requirement)
						capabilityErrors = append(capabilityErrors,
							fmt.Errorf("could not enable newly requested feature in existing shared cgroup %s: %w", sharedPath, err),
						)
					}
				}
			} else if !os.IsNotExist(err) {
				for _, requirement := range newRequirements {
					disableControllerFeature(newConfig, requirement)
				}
				capabilityErrors = append(capabilityErrors,
					fmt.Errorf("could not inspect existing shared cgroup %s before enabling resource features: %w", sharedPath, err),
				)
			}
		}
	}

	m.cfgMu.Lock()
	if m.cfg != currentConfig {
		m.cfgMu.Unlock()
		return errors.Join(append(capabilityErrors,
			fmt.Errorf("cgroup configuration changed concurrently while controller capabilities were being reconciled; retry the update"),
		)...)
	}
	m.cfg = newConfig
	m.cfgMu.Unlock()
	return errors.Join(capabilityErrors...)
}

func newlyEnabledControllerInterfaces(currentConfig, newConfig *config.Config) []controllerRequirement {
	all := allControllerInterfaces()
	var requirements []controllerRequirement
	if !currentConfig.RAMEnabled && newConfig.RAMEnabled {
		requirements = append(requirements, all[1])
	}
	if !currentConfig.IOEnabled && newConfig.IOEnabled {
		requirements = append(requirements, all[2])
	}
	return requirements
}

func disableControllerFeature(cfg *config.Config, requirement controllerRequirement) {
	switch requirement.interfaceFile {
	case "memory.max":
		cfg.RAMEnabled = false
	case "io.max":
		cfg.IOEnabled = false
	}
}

// getBaseCgroupPath returns the base cgroup path.
func (m *Manager) getBaseCgroupPath() string {
	cfg := m.getConfig()
	return filepath.Join(cfg.CgroupRoot, cfg.CgroupBase)
}

// getUserCgroupPath returns the cgroup path for a user.
func (m *Manager) getUserCgroupPath(uid int) string {
	return filepath.Join(m.getBaseCgroupPath(), fmt.Sprintf("user_%d", uid))
}
