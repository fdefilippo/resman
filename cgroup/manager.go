package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

const (
	defaultFilePerm = 0644
	// Note: cleanupRetryDelay, processMoveDelay, etc. are now configurable via config
)

// Manager gestisce tutte le operazioni sui cgroups v2.
type Manager struct {
	cfg    *config.Config
	logger *logging.Logger
	mu     sync.RWMutex
	wg     sync.WaitGroup

	// Tracciamento dei cgroups creati
	createdCgroups     map[int]string // UID -> cgroup path
	createdCgroupsFile string

	// Cache per le verifiche
	controllersAvailable bool
	cgroupRootWritable   bool
}

// NewManager crea un nuovo CgroupManager.
func NewManager(cfg *config.Config) (*Manager, error) {
	logger := logging.GetLogger()

	mgr := &Manager{
		cfg:                cfg,
		logger:             logger,
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: cfg.CreatedCgroupsFile,
	}

	// Verifica che i cgroups v2 siano disponibili e configurati correttamente
	if err := mgr.verifyCgroupSetup(); err != nil {
		return nil, fmt.Errorf("cgroup setup verification failed: %w", err)
	}

	// Carica i cgroups già creati (se presenti) dal file di tracciamento
	if err := mgr.loadExistingCgroups(); err != nil {
		logger.Warn("Could not load existing cgroups tracking file", "error", err)
	}

	logger.Info("Cgroup manager initialized",
		"cgroup_root", cfg.CgroupRoot,
		"base_cgroup", cfg.CgroupBase,
	)

	return mgr, nil
}

// verifyCgroupSetup verifica che i cgroups v2 siano configurati correttamente.
func (m *Manager) verifyCgroupSetup() error {
	// 1. Verifica che la root dei cgroups esista
	if _, err := os.Stat(m.cfg.CgroupRoot); os.IsNotExist(err) {
		return fmt.Errorf("cgroup root does not exist: %s (enable cgroups v2: grubby --update-kernel=ALL --args='systemd.unified_cgroup_hierarchy=1')", m.cfg.CgroupRoot)
	}

	// 2. Verifica che sia cgroups v2 (controlla cgroup.controllers)
	controllersFile := filepath.Join(m.cfg.CgroupRoot, "cgroup.controllers")
	controllersData, err := os.ReadFile(controllersFile)
	if err != nil {
		return fmt.Errorf("cannot read cgroup.controllers at %s: %w", controllersFile, err)
	}
	m.logger.Info("Available cgroup controllers",
		"controllers", strings.TrimSpace(string(controllersData)),
	)
	if !strings.Contains(string(controllersData), "cpu") {
		m.logger.Error("CPU controller not available in cgroup.controllers",
			"available_controllers", strings.TrimSpace(string(controllersData)),
		)
		return fmt.Errorf("cpu controller not available (available: %s)", strings.TrimSpace(string(controllersData)))
	}

	// 3. Verifica che i controller CPU siano abilitati
	subtreeControlFile := filepath.Join(m.cfg.CgroupRoot, "cgroup.subtree_control")
	data, err := os.ReadFile(subtreeControlFile)
	if err != nil {
		return fmt.Errorf("failed to read cgroup.subtree_control at %s: %w", subtreeControlFile, err)
	}

	controllers := string(data)
	m.controllersAvailable = strings.Contains(controllers, "cpu") &&
		strings.Contains(controllers, "cpuset")

	if !m.controllersAvailable {
		m.logger.Warn("CPU or cpuset controllers not enabled in subtree_control",
			"subtree_control", strings.TrimSpace(controllers),
		)
		// Tentativo di abilitarli automaticamente
		if err := m.enableCPUControllers(); err != nil {
			return fmt.Errorf("failed to enable CPU controllers (%s): %w", subtreeControlFile, err)
		}
		m.controllersAvailable = true
	}

	// 4. Verifica scrivibilità
	testFile := filepath.Join(m.cfg.CgroupRoot, "cgroup.procs")
	if err := os.WriteFile(testFile, []byte("0"), 0644); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("no write permission to cgroup root %s: %w", m.cfg.CgroupRoot, err)
		}
	}
	m.cgroupRootWritable = true

	// 5. Crea il cgroup base se non esiste
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

	// Abilita i controller nel nostro cgroup base
	baseSubtreeControl := filepath.Join(baseCgroupPath, "cgroup.subtree_control")
	if err := m.writeControllerIfMissing(baseSubtreeControl, "+cpu"); err != nil {
		return fmt.Errorf("failed to enable cpu controller in base cgroup %s: %w", baseCgroupPath, err)
	}
	if err := m.writeControllerIfMissing(baseSubtreeControl, "+cpuset"); err != nil {
		return fmt.Errorf("failed to enable cpuset controller in base cgroup %s: %w", baseCgroupPath, err)
	}
	// Enable io controller for block I/O limits (best-effort, non-fatal)
	// Only attempt if the io controller is available in the cgroup root
	if strings.Contains(string(controllersData), "io") {
		if err := m.writeControllerIfMissing(baseSubtreeControl, "+io"); err != nil {
			m.logger.Warn("Failed to enable io controller in base cgroup (IO limits will not work)",
				"path", baseCgroupPath,
				"error", err,
			)
		}
	} else {
		m.logger.Debug("io controller not available in cgroup.controllers, skipping",
			"available_controllers", strings.TrimSpace(string(controllersData)),
		)
	}

	m.logger.Debug("Cgroup setup verified successfully")
	return nil
}

// enableCPUControllers tenta di abilitare i controller CPU a livello di root.
func (m *Manager) enableCPUControllers() error {
	subtreeControlFile := filepath.Join(m.cfg.CgroupRoot, "cgroup.subtree_control")

	// Prova ad abilitare cpu
	if err := os.WriteFile(subtreeControlFile, []byte("+cpu"), 0644); err != nil {
		return fmt.Errorf("failed to enable cpu controller: %w", err)
	}

	// Prova ad abilitare cpuset
	if err := os.WriteFile(subtreeControlFile, []byte("+cpuset"), 0644); err != nil {
		// Se cpuset fallisce, continuiamo con solo cpu
		m.logger.Warn("Failed to enable cpuset controller", "error", err)
	}

	// Prova ad abilitare io (best-effort)
	if err := os.WriteFile(subtreeControlFile, []byte("+io"), 0644); err != nil {
		m.logger.Warn("Failed to enable io controller", "error", err)
	}

	m.logger.Info("CPU controllers enabled in cgroup subtree_control")
	return nil
}

// writeControllerIfMissing aggiunge un controller solo se non è già presente.
func (m *Manager) writeControllerIfMissing(filePath, controller string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	if strings.Contains(string(data), controller[1:]) { // controller[1:] rimuove il "+" o "-"
		return nil // Già presente
	}

	return os.WriteFile(filePath, []byte(controller), 0644)
}

// getBaseCgroupPath restituisce il percorso del cgroup base.
func (m *Manager) getBaseCgroupPath() string {
	return filepath.Join(m.cfg.CgroupRoot, m.cfg.CgroupBase)
}

// getUserCgroupPath restituisce il percorso del cgroup per un utente specifico.
func (m *Manager) getUserCgroupPath(uid int) string {
	return filepath.Join(m.getBaseCgroupPath(), fmt.Sprintf("user_%d", uid))
}
