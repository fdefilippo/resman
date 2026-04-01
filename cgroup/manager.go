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
// cgroup/manager.go
package cgroup

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

const (
	defaultFilePerm   = 0644
	sharedCgroupQuota = 100000
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

	// Abilita i controller nel nostro cgroup base
	baseSubtreeControl := filepath.Join(baseCgroupPath, "cgroup.subtree_control")
	if err := m.writeControllerIfMissing(baseSubtreeControl, "+cpu"); err != nil {
		return fmt.Errorf("failed to enable cpu controller in base cgroup %s: %w", baseCgroupPath, err)
	}
	if err := m.writeControllerIfMissing(baseSubtreeControl, "+cpuset"); err != nil {
		return fmt.Errorf("failed to enable cpuset controller in base cgroup %s: %w", baseCgroupPath, err)
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

// CreateUserCgroup crea un cgroup per un utente specifico.
func (m *Manager) CreateUserCgroup(uid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Verifica se esiste già
	if _, exists := m.createdCgroups[uid]; exists {
		m.logger.Debug("Cgroup already exists for user", "uid", uid)
		return nil
	}

	cgroupPath := m.getUserCgroupPath(uid)

	// Crea la directory del cgroup
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return fmt.Errorf("failed to create cgroup directory %s for UID %d: %w", cgroupPath, uid, err)
	}

	// Traccia il cgroup creato
	m.createdCgroups[uid] = cgroupPath

	// Salva nel file di tracciamento
	if err := m.saveCgroupToFile(uid, cgroupPath); err != nil {
		m.logger.Warn("Failed to save cgroup to tracking file",
			"uid", uid,
			"error", err,
		)
		// Non falliamo per questo errore
	}

	m.logger.Debug("Cgroup created for user", "uid", uid, "path", cgroupPath)
	return nil
}

// ApplyCPULimit applica un limite di CPU a un cgroup utente.
func (m *Manager) ApplyCPULimit(uid int, quota string) error {
	// Assicurati che il cgroup esista
	cgroupPath := m.getUserCgroupPath(uid)

	// Verifica che la directory esista
	if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
		// Crea il cgroup se non esiste
		if err := m.CreateUserCgroup(uid); err != nil {
			return fmt.Errorf("failed to create cgroup %s before applying limit for UID %d: %w", cgroupPath, uid, err)
		}
	}

	cpuMaxFile := filepath.Join(cgroupPath, "cpu.max")

	// Valida il formato della quota
	if !isValidCPUQuotaFormat(quota) {
		return fmt.Errorf("invalid CPU quota format '%s': expected 'quota period' (e.g., '50000 100000') or 'max period'", quota)
	}

	// DEBUG: Log prima di applicare
	m.logger.Debug("Applying CPU limit",
		"uid", uid,
		"quota", quota,
		"path", cpuMaxFile,
	)

	// Applica il limite
	if err := os.WriteFile(cpuMaxFile, []byte(quota), 0644); err != nil {
		// Prova con permessi diversi
		if os.IsPermission(err) {
			if chmodErr := os.Chmod(cpuMaxFile, 0644); chmodErr != nil {
				m.logger.Warn("Failed to chmod CPU max file",
					"path", cpuMaxFile,
					"error", chmodErr,
				)
			}
			time.Sleep(100 * time.Millisecond)
			err = os.WriteFile(cpuMaxFile, []byte(quota), 0644)
		}
		if err != nil {
			return fmt.Errorf("failed to apply CPU limit %s to %s for UID %d: %w", quota, cpuMaxFile, uid, err)
		}
	}

	// Verifica che il limite sia stato applicato
	time.Sleep(50 * time.Millisecond)
	if data, err := os.ReadFile(cpuMaxFile); err == nil {
		appliedQuota := strings.TrimSpace(string(data))
		if appliedQuota != quota {
			m.logger.Warn("CPU limit may not have been applied correctly",
				"uid", uid,
				"requested", quota,
				"applied", appliedQuota,
			)
			if retryErr := os.WriteFile(cpuMaxFile, []byte(quota), 0644); retryErr != nil {
				m.logger.Warn("Retry failed to apply CPU limit",
					"uid", uid,
					"error", retryErr,
				)
			}
		} else {
			m.logger.Debug("CPU limit verified",
				"uid", uid,
				"quota", appliedQuota,
			)
		}
	}

	// Sposta processi in modo sincrono con timeout configurabile
	timeout := time.Duration(m.cfg.GetCgroupOperationTimeout()) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer close(done)
		delay := time.Duration(m.cfg.GetCgroupRetryDelayMs()) * time.Millisecond
		time.Sleep(delay) // Breve delay per stabilizzazione
		done <- m.MoveAllUserProcesses(uid)
	}()

	select {
	case err := <-done:
		if err != nil {
			m.logger.Warn("Failed to move user processes to cgroup",
				"uid", uid,
				"error", err,
			)
			return err
		}
	case <-ctx.Done():
		m.logger.Warn("Timeout moving user processes to cgroup",
			"uid", uid,
			"timeout", timeout,
		)
		return fmt.Errorf("timeout (%v) moving processes to cgroup for UID %d", timeout, uid)
	}

	return nil
}

// ApplyCPUWeight applica un peso CPU (proporzionale) a un cgroup utente.
func (m *Manager) ApplyCPUWeight(uid int, weight int) error {
	cgroupPath := m.getUserCgroupPath(uid)

	// Verifica che la directory esista
	if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
		// Crea il cgroup se non esiste
		if err := m.CreateUserCgroup(uid); err != nil {
			return fmt.Errorf("failed to create cgroup before applying weight: %w", err)
		}
	}

	cpuWeightFile := filepath.Join(cgroupPath, "cpu.weight")

	// Il peso deve essere tra 1 e 10000
	if weight < 1 {
		weight = 1
	}
	if weight > 10000 {
		weight = 10000
	}

	// Applica il peso
	weightStr := strconv.Itoa(weight)
	if err := os.WriteFile(cpuWeightFile, []byte(weightStr), 0644); err != nil {
		return fmt.Errorf("failed to apply CPU weight for UID %d: %w", uid, err)
	}

	m.logger.Debug("CPU weight applied",
		"uid", uid,
		"weight", weight,
		"path", cpuWeightFile,
	)

	return nil
}

// RemoveCPULimit rimuove il limite di CPU (imposta a "max").
func (m *Manager) RemoveCPULimit(uid int) error {
	return m.ApplyCPULimit(uid, "max 100000")
}

// MoveProcessToCgroup sposta un processo nel cgroup dell'utente.
func (m *Manager) MoveProcessToCgroup(pid int, uid int) error {
	// SECURITY: Never move any process to UID 0 cgroup
	if uid == 0 {
		m.logger.Warn("Refusing to move process to root (UID 0) cgroup - security boundary",
			"pid", pid)
		return fmt.Errorf("processes cannot be moved to UID 0 (root) cgroups")
	}

	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return fmt.Errorf("cgroup for UID %d does not exist", uid)
	}

	cgroupProcsFile := filepath.Join(cgroupPath, "cgroup.procs")

	// Ottieni info sul processo PRIMA di spostarlo
	processName := m.getProcessName(pid)
	processInfo, _ := m.getProcessInfo(pid)

	// Scrivi il PID nel file cgroup.procs
	pidStr := strconv.Itoa(pid)
	if err := os.WriteFile(cgroupProcsFile, []byte(pidStr), 0644); err != nil {
		return fmt.Errorf("failed to move PID %d to cgroup for UID %d: %w", pid, uid, err)
	}

	// Log dettagliato
	m.logger.Debug("Process moved to cgroup",
		"pid", pid,
		"uid", uid,
		"process_name", processName,
		"process_state", processInfo["state"],
		"username", processInfo["username"],
		"cgroup_path", cgroupPath,
	)

	return nil
}

// MoveAllUserProcesses sposta tutti i processi di un utente nel suo cgroup.
func (m *Manager) MoveAllUserProcesses(uid int) error {
	m.logger.Debug("Moving all processes for user to cgroup", "uid", uid)

	// SECURITY: Never move UID 0 (root) processes to user cgroups
	if uid == 0 {
		m.logger.Warn("Refusing to move root (UID 0) processes to cgroup - security boundary")
		return fmt.Errorf("UID 0 (root) processes cannot be moved to user cgroups")
	}

	// Leggi tutti i PIDs dell'utente da /proc
	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return fmt.Errorf("failed to read /proc: %w", err)
	}

	var movedCount int
	var totalProcesses int
	var processNames []string
	var errors []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Verifica se è una directory PID numerica
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // Non è una directory PID
		}

		// Leggi il UID del processo
		statusFile := filepath.Join(procDir, entry.Name(), "status")
		if procUID, err := m.getUIDFromStatusFile(statusFile); err == nil && procUID == uid {
			totalProcesses++

			// Ottieni nome processo
			processName := m.getProcessName(pid)

			// Salta processi esclusi da PROCESS_EXCLUDE_LIST
			if m.cfg.IsProcessExcluded(processName) {
				continue
			}

			// Sposta il processo
			if err := m.MoveProcessToCgroup(pid, uid); err != nil {
				errors = append(errors, fmt.Sprintf("%s[%d]: %v", processName, pid, err))
			} else {
				movedCount++
				processNames = append(processNames, processName)
			}
		}
	}

	// Log riepilogativo con elenco processi
	if movedCount > 0 {
		if len(processNames) <= 10 {
			// Se pochi processi, mostra tutti
			m.logger.Info("User processes moved to cgroup",
				"uid", uid,
				"moved_count", movedCount,
				"total_found", totalProcesses,
				"processes", strings.Join(processNames, ", "),
				"error_count", len(errors),
				"success_rate", fmt.Sprintf("%.1f%%", float64(movedCount)/float64(totalProcesses)*100),
			)
		} else {
			// Se molti processi, mostra solo i primi 10
			m.logger.Info("User processes moved to cgroup",
				"uid", uid,
				"moved_count", movedCount,
				"total_found", totalProcesses,
				"sample_processes", strings.Join(processNames[:10], ", "),
				"and_more", fmt.Sprintf("%d more processes", len(processNames)-10),
				"error_count", len(errors),
				"success_rate", fmt.Sprintf("%.1f%%", float64(movedCount)/float64(totalProcesses)*100),
			)
		}
	} else {
		m.logger.Warn("No processes moved for user",
			"uid", uid,
			"total_processes_found", totalProcesses,
			"possible_reasons", "no processes found or permission issues",
		)
	}

	if len(errors) > 0 {
		m.logger.Warn("Some processes could not be moved",
			"uid", uid,
			"first_error", errors[0],
			"total_errors", len(errors),
			"success_rate", fmt.Sprintf("%.1f%%", float64(movedCount)/float64(totalProcesses)*100),
		)
	}

	return nil
}

// CreateSharedCgroup crea un cgroup condiviso per tutti gli utenti limitati
func (m *Manager) CreateSharedCgroup() (string, error) {
	sharedPath := filepath.Join(m.getBaseCgroupPath(), "limited")

	// Se il cgroup condiviso esiste già, RIMUOVLO COMPLETAMENTE e ricreo
	if _, err := os.Stat(sharedPath); err == nil {
		m.logger.Info("Shared cgroup already exists, removing and recreating", "path", sharedPath)

		// Sposta prima tutti i processi al cgroup root
		cgroupProcsFile := filepath.Join(sharedPath, "cgroup.procs")
		if pids, err := m.readPidsFromFile(cgroupProcsFile); err == nil && len(pids) > 0 {
			m.logger.Info("Moving processes out of existing shared cgroup",
				"path", sharedPath,
				"count", len(pids),
			)
			rootCgroupProcs := filepath.Join(m.cfg.CgroupRoot, "cgroup.procs")
			for _, pid := range pids {
				if writeErr := os.WriteFile(rootCgroupProcs, []byte(fmt.Sprintf("%d", pid)), 0644); writeErr != nil {
					m.logger.Warn("Failed to move process to root cgroup",
						"pid", pid,
						"error", writeErr,
					)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Rimuovi il cgroup esistente con tutto il contenuto
		if err := os.RemoveAll(sharedPath); err != nil {
			m.logger.Warn("Failed to remove existing shared cgroup, will try to reuse",
				"path", sharedPath,
				"error", err,
			)
		} else {
			m.logger.Info("Existing shared cgroup removed", "path", sharedPath)
		}
	}

	// Crea la directory del cgroup condiviso
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create shared cgroup directory: %w", err)
	}

	// Abilita i controller nel cgroup condiviso
	subtreeControl := filepath.Join(sharedPath, "cgroup.subtree_control")
	if err := m.writeControllerIfMissing(subtreeControl, "+cpu"); err != nil {
		m.logger.Warn("Failed to enable cpu controller in shared cgroup", "error", err)
	}
	if err := m.writeControllerIfMissing(subtreeControl, "+cpuset"); err != nil {
		m.logger.Warn("Failed to enable cpuset controller in shared cgroup", "error", err)
	}

	m.logger.Info("Shared cgroup created and initialized", "path", sharedPath)
	return sharedPath, nil
}

// ApplySharedCPULimit applica un limite di CPU al cgroup condiviso
func (m *Manager) ApplySharedCPULimit(sharedPath string, quota string) error {
	cpuMaxFile := filepath.Join(sharedPath, "cpu.max")

	// Valida il formato della quota
	if !isValidCPUQuotaFormat(quota) {
		return fmt.Errorf("invalid CPU quota format: %s", quota)
	}

	// Applica il limite
	if err := os.WriteFile(cpuMaxFile, []byte(quota), 0644); err != nil {
		return fmt.Errorf("failed to apply shared CPU limit: %w", err)
	}

	m.logger.Debug("Shared CPU limit applied",
		"path", sharedPath,
		"quota", quota,
	)

	return nil
}

// CreateUserSubCgroup crea un sottocgroup utente dentro il cgroup condiviso
func (m *Manager) CreateUserSubCgroup(uid int, sharedPath string) (string, error) {
	userPath := filepath.Join(sharedPath, fmt.Sprintf("user_%d", uid))

	// Crea la directory del sottocgroup
	if err := os.MkdirAll(userPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create user sub-cgroup directory: %w", err)
	}

	// Imposta peso di default (100)
	weightFile := filepath.Join(userPath, "cpu.weight")
	if err := os.WriteFile(weightFile, []byte("100"), 0644); err != nil {
		// Non è fatale, logghiamo e continuiamo
		m.logger.Warn("Failed to set default CPU weight",
			"uid", uid,
			"path", userPath,
			"error", err,
		)
	}

	m.logger.Debug("User sub-cgroup created",
		"uid", uid,
		"path", userPath,
		"parent", sharedPath,
	)

	return userPath, nil
}

// MoveProcessToSharedCgroup sposta un processo nel cgroup condiviso
func (m *Manager) MoveProcessToSharedCgroup(pid int, sharedPath string, uid int) error {
	// Usa il sottocgroup specifico dell'utente
	userPath := filepath.Join(sharedPath, fmt.Sprintf("user_%d", uid))

	// Assicurati che il sottocgroup esista
	if _, err := os.Stat(userPath); os.IsNotExist(err) {
		if _, err := m.CreateUserSubCgroup(uid, sharedPath); err != nil {
			return fmt.Errorf("failed to create user sub-cgroup: %w", err)
		}
	}

	cgroupProcsFile := filepath.Join(userPath, "cgroup.procs")

	// Scrivi il PID nel file cgroup.procs
	pidStr := strconv.Itoa(pid)
	if err := os.WriteFile(cgroupProcsFile, []byte(pidStr), 0644); err != nil {
		return fmt.Errorf("failed to move PID %d to shared cgroup for UID %d: %w", pid, uid, err)
	}

	return nil
}

// MoveAllUserProcessesToSharedCgroup sposta tutti i processi di un utente nel cgroup condiviso
func (m *Manager) MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) error {
	m.logger.Debug("Moving all processes for user to shared cgroup",
		"uid", uid,
		"shared_path", sharedPath,
	)

	// Leggi tutti i PIDs dell'utente da /proc
	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return fmt.Errorf("failed to read /proc: %w", err)
	}

	var movedCount int
	var errors []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Verifica se è una directory PID numerica
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // Non è una directory PID
		}

		// Leggi il UID del processo
		statusFile := filepath.Join(procDir, entry.Name(), "status")
		if procUID, err := m.getUIDFromStatusFile(statusFile); err == nil && procUID == uid {
			// Ottieni nome processo
			processName := m.getProcessName(pid)

			// Salta processi esclusi da PROCESS_EXCLUDE_LIST
			if m.cfg.IsProcessExcluded(processName) {
				continue
			}

			// Sposta il processo
			if err := m.MoveProcessToSharedCgroup(pid, sharedPath, uid); err != nil {
				errors = append(errors, fmt.Sprintf("PID %d: %v", pid, err))
			} else {
				movedCount++
			}
		}
	}

	if movedCount > 0 {
		m.logger.Debug("Processes moved to shared cgroup",
			"uid", uid,
			"moved_count", movedCount,
			"error_count", len(errors),
		)
	} else {
		m.logger.Warn("No processes moved for user to shared cgroup",
			"uid", uid,
			"possible_reasons", "no processes found or permission issues",
		)
	}

	if len(errors) > 0 {
		m.logger.Warn("Some processes could not be moved to shared cgroup",
			"uid", uid,
			"first_error", errors[0],
			"total_errors", len(errors),
		)
	}

	return nil
}

// getUIDFromStatusFile estrae il UID dal file /proc/[pid]/status.
func (m *Manager) getUIDFromStatusFile(statusFile string) (int, error) {
	file, err := os.Open(statusFile)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				// Il primo campo dopo "Uid:" è l'UID reale
				uid, err := strconv.Atoi(fields[1])
				if err != nil {
					return 0, err
				}
				return uid, nil
			}
		}
	}

	return 0, fmt.Errorf("UID not found in status file")
}

// CleanupUserCgroup rimuove il cgroup di un utente (dopo aver spostato i processi fuori).
func (m *Manager) CleanupUserCgroup(uid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cgroupPath, exists := m.createdCgroups[uid]
	if !exists {
		// Se non è nel nostro tracciamento, prova comunque a trovare il path
		cgroupPath = m.getUserCgroupPath(uid)
		if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
			return nil // Già non esiste
		}
	}

	// 1. Leggi e logga i processi prima di spostarli
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	pids, err := m.readPidsFromFile(procsFile)
	if err == nil && len(pids) > 0 {
		var processNames []string
		for _, pid := range pids {
			processNames = append(processNames, m.getProcessName(pid))
		}

		m.logger.Info("Moving processes out of cgroup before cleanup",
			"uid", uid,
			"process_count", len(pids),
			"processes", strings.Join(processNames, ", "),
		)
	}
	// 2. Rimuovi la directory del cgroup
	if err := os.Remove(cgroupPath); err != nil {
		// Se fallisce a causa di processi rimanenti, prova a forzare
		m.logger.Warn("Failed to remove cgroup directory, retrying",
			"uid", uid,
			"path", cgroupPath,
			"error", err,
		)
		time.Sleep(100 * time.Millisecond)
		if err := os.Remove(cgroupPath); err != nil {
			return fmt.Errorf("failed to remove cgroup for UID %d: %w", uid, err)
		}
	}

	// 3. Rimuovi dal tracciamento
	delete(m.createdCgroups, uid)

	// 4. Aggiorna il file di tracciamento
	if err := m.removeCgroupFromFile(uid); err != nil {
		m.logger.Warn("Failed to update cgroup tracking file",
			"uid", uid,
			"error", err,
		)
	}

	m.logger.Debug("Cgroup cleaned up for user",
		"uid", uid,
		"processes_moved", len(pids),
	)
	return nil
}

// CleanupAll removes all created cgroups (used during shutdown).
func (m *Manager) CleanupAll() error {
	m.mu.Lock()
	m.logger.Info("Starting cgroup cleanup", "tracked_count", len(m.createdCgroups))

	// Wait for any pending goroutines
	m.wg.Wait()

	// CRITICAL FIX: Make atomic copy of createdCgroups before releasing lock
	// This prevents concurrent map iteration and write panic
	uids := make([]int, 0, len(m.createdCgroups))
	for uid := range m.createdCgroups {
		uids = append(uids, uid)
	}
	m.mu.Unlock()

	var errors []string

	// Clean up all known cgroups from the atomic copy
	for _, uid := range uids {
		if err := m.CleanupUserCgroup(uid); err != nil {
			errors = append(errors, fmt.Sprintf("UID %d: %v", uid, err))
		}
	}

	// Rimuovi il cgroup condiviso "limited" se esiste
	sharedPath := filepath.Join(m.getBaseCgroupPath(), "limited")
	if _, err := os.Stat(sharedPath); err == nil {
		m.logger.Info("Cleaning up shared cgroup", "path", sharedPath)

		// STEP 1: Leggi TUTTI i sottocgroup utente dalla directory (non solo dal tracciamento)
		entries, err := os.ReadDir(sharedPath)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "user_") {
					userPath := filepath.Join(sharedPath, entry.Name())
					m.logger.Info("Removing user sub-cgroup", "path", userPath)

					// Sposta i processi fuori prima di rimuovere
					userProcsFile := filepath.Join(userPath, "cgroup.procs")
					if pids, err := m.readPidsFromFile(userProcsFile); err == nil && len(pids) > 0 {
						m.logger.Info("Moving processes out of user cgroup",
							"path", userPath,
							"count", len(pids),
						)
						rootCgroupProcs := filepath.Join(m.cfg.CgroupRoot, "cgroup.procs")
						for _, pid := range pids {
							os.WriteFile(rootCgroupProcs, []byte(fmt.Sprintf("%d", pid)), 0644)
						}
					}

					if err := os.RemoveAll(userPath); err != nil {
						m.logger.Warn("Failed to remove user sub-cgroup",
							"path", userPath,
							"error", err,
						)
						errors = append(errors, fmt.Sprintf("user cgroup %s: %v", userPath, err))
					} else {
						m.logger.Info("User sub-cgroup removed", "path", userPath)
					}
				}
			}
		}

		// STEP 2: Sposta TUTTI i processi fuori dal cgroup condiviso
		cgroupProcsFile := filepath.Join(sharedPath, "cgroup.procs")
		if pids, err := m.readPidsFromFile(cgroupProcsFile); err == nil && len(pids) > 0 {
			m.logger.Info("Moving processes out of shared cgroup",
				"path", sharedPath,
				"count", len(pids),
			)
			rootCgroupProcs := filepath.Join(m.cfg.CgroupRoot, "cgroup.procs")
			for _, pid := range pids {
				if err := os.WriteFile(rootCgroupProcs, []byte(fmt.Sprintf("%d", pid)), 0644); err != nil {
					m.logger.Debug("Failed to move process", "pid", pid, "error", err)
				}
			}
			// Aspetta che il kernel process lo spostamento
			time.Sleep(200 * time.Millisecond)
		}

		// STEP 3: Verifica che il cgroup sia vuoto
		if pids, _ := m.readPidsFromFile(cgroupProcsFile); len(pids) > 0 {
			m.logger.Warn("Shared cgroup still has processes after cleanup",
				"path", sharedPath,
				"remaining_count", len(pids),
			)
			errors = append(errors, fmt.Sprintf("shared cgroup still has %d processes", len(pids)))
		}

		// STEP 4: Ora prova a rimuovere il cgroup condiviso
		m.logger.Info("Removing shared cgroup directory", "path", sharedPath)
		if err := os.RemoveAll(sharedPath); err != nil {
			m.logger.Error("Failed to remove shared cgroup",
				"path", sharedPath,
				"error", err,
			)
			errors = append(errors, fmt.Sprintf("shared cgroup: %v", err))
		} else {
			m.logger.Info("Shared cgroup removed successfully", "path", sharedPath)
		}
	} else {
		m.logger.Debug("Shared cgroup does not exist, skipping cleanup", "path", sharedPath)
	}

	// Poi prova a rimuovere il cgroup base (se vuoto)
	baseCgroupPath := m.getBaseCgroupPath()
	if _, err := os.Stat(baseCgroupPath); err == nil {
		if err := os.Remove(baseCgroupPath); err != nil {
			m.logger.Debug("Could not remove base cgroup (may not be empty)",
				"path", baseCgroupPath,
				"error", err,
			)
		} else {
			m.logger.Info("Base cgroup removed", "path", baseCgroupPath)
		}
	}

	// Pulisci il file di tracciamento
	if err := os.Remove(m.createdCgroupsFile); err != nil && !os.IsNotExist(err) {
		errors = append(errors, fmt.Sprintf("tracking file: %v", err))
	}

	if len(errors) > 0 {
		m.logger.Warn("Cleanup completed with errors", "error_count", len(errors))
		return fmt.Errorf("errors during cleanup: %s", strings.Join(errors, "; "))
	}

	m.logger.Info("All cgroups cleaned up successfully")
	return nil
}

// saveCgroupToFile salva un cgroup nel file di tracciamento.
func (m *Manager) saveCgroupToFile(uid int, cgroupPath string) error {
	file, err := os.OpenFile(m.createdCgroupsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.WriteString(fmt.Sprintf("%d:%s\n", uid, cgroupPath))
	return err
}

// removeCgroupFromFile rimuove un cgroup dal file di tracciamento.
func (m *Manager) removeCgroupFromFile(uid int) error {
	// Leggi tutto il file, filtra e riscrivi
	if _, err := os.Stat(m.createdCgroupsFile); os.IsNotExist(err) {
		return nil
	}

	data, err := os.ReadFile(m.createdCgroupsFile)
	if err != nil {
		return err
	}

	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) >= 1 {
			lineUID, err := strconv.Atoi(parts[0])
			if err != nil || lineUID != uid {
				lines = append(lines, line)
			}
		}
	}

	// Risciivi il file
	return os.WriteFile(m.createdCgroupsFile, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

// loadExistingCgroups carica i cgroups esistenti dal file di tracciamento.
func (m *Manager) loadExistingCgroups() error {
	if _, err := os.Stat(m.createdCgroupsFile); os.IsNotExist(err) {
		return nil
	}

	file, err := os.Open(m.createdCgroupsFile)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		uid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}

		cgroupPath := parts[1]
		// Verifica che il cgroup esista ancora
		if _, err := os.Stat(cgroupPath); err == nil {
			m.createdCgroups[uid] = cgroupPath
		}
	}

	m.logger.Debug("Loaded existing cgroups from file",
		"count", len(m.createdCgroups),
		"file", m.createdCgroupsFile,
	)

	return scanner.Err()
}

// getCgroupPath restituisce il percorso del cgroup per un UID.
func (m *Manager) getCgroupPath(uid int) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	path, exists := m.createdCgroups[uid]
	return path, exists
}

// readPidsFromFile legge i PIDs da un file cgroup.procs.
func (m *Manager) readPidsFromFile(filePath string) ([]int, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var pids []int
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		pidStr := strings.TrimSpace(scanner.Text())
		if pidStr == "" {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}

	return pids, nil
}

// isValidCPUQuotaFormat valida il formato della quota CPU.
func isValidCPUQuotaFormat(quota string) bool {
	parts := strings.Fields(quota)
	if len(parts) != 2 {
		return false
	}

	// La prima parte può essere "max" o un numero
	if parts[0] == "max" {
		_, err := strconv.Atoi(parts[1])
		return err == nil
	}

	// Altrimenti entrambe devono essere numeri
	_, err1 := strconv.Atoi(parts[0])
	_, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil
}

// GetCreatedCgroups restituisce una lista di UID con cgroups attivi.
func (m *Manager) GetCreatedCgroups() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	uids := make([]int, 0, len(m.createdCgroups))
	for uid := range m.createdCgroups {
		uids = append(uids, uid)
	}
	return uids
}

// GetCgroupInfo restituisce informazioni su un cgroup specifico.
func (m *Manager) GetCgroupInfo(uid int) (map[string]string, error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return nil, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	info := make(map[string]string)
	info["path"] = cgroupPath

	// Leggi il limite CPU corrente
	cpuMaxFile := filepath.Join(cgroupPath, "cpu.max")
	if data, err := os.ReadFile(cpuMaxFile); err == nil {
		info["cpu.max"] = strings.TrimSpace(string(data))
	}

	// Leggi il peso CPU corrente
	cpuWeightFile := filepath.Join(cgroupPath, "cpu.weight")
	if data, err := os.ReadFile(cpuWeightFile); err == nil {
		info["cpu.weight"] = strings.TrimSpace(string(data))
	}

	// Conta i processi nel cgroup
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	if pids, err := m.readPidsFromFile(procsFile); err == nil {
		info["process_count"] = strconv.Itoa(len(pids))
	}

	return info, nil
}

// getProcessInfo restituisce informazioni dettagliate su un processo
func (m *Manager) getProcessInfo(pid int) (map[string]string, error) {
	info := make(map[string]string)

	// Nome del processo da /proc/[pid]/comm
	commFile := fmt.Sprintf("/proc/%d/comm", pid)
	if data, err := os.ReadFile(commFile); err == nil {
		info["name"] = strings.TrimSpace(string(data))
	} else {
		info["name"] = "unknown"
	}

	// Command line da /proc/[pid]/cmdline
	cmdlineFile := fmt.Sprintf("/proc/%d/cmdline", pid)
	if data, err := os.ReadFile(cmdlineFile); err == nil {
		cmdline := strings.ReplaceAll(string(data), "\x00", " ")
		cmdline = strings.TrimSpace(cmdline)
		if cmdline != "" {
			info["cmdline"] = cmdline
		}
	}

	// Username da getent
	cmd := exec.Command("ps", "-o", "user=", "-p", strconv.Itoa(pid))
	if output, err := cmd.Output(); err == nil {
		info["username"] = strings.TrimSpace(string(output))
	}

	// CPU usage corrente
	statFile := fmt.Sprintf("/proc/%d/stat", pid)
	if data, err := os.ReadFile(statFile); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 13 {
			info["state"] = fields[2] // Stato del processo (R, S, D, Z, etc.)
		}
	}

	return info, nil
}

// getProcessName cerca di ottenere il nome migliore per un processo
func (m *Manager) getProcessName(pid int) string {
	info, err := m.getProcessInfo(pid)
	if err != nil {
		return fmt.Sprintf("PID-%d", pid)
	}

	// Preferisci cmdline se disponibile e non troppo lungo
	if cmdline, ok := info["cmdline"]; ok && cmdline != "" && len(cmdline) < 100 {
		// Prendi solo il primo comando (prima dello spazio)
		parts := strings.Fields(cmdline)
		if len(parts) > 0 {
			// Estrai solo il nome del comando (senza path)
			base := filepath.Base(parts[0])
			return fmt.Sprintf("%s[%d]", base, pid)
		}
	}

	// Altrimenti usa il nome dal comm
	if name, ok := info["name"]; ok && name != "" {
		return fmt.Sprintf("%s[%d]", name, pid)
	}

	return fmt.Sprintf("PID-%d", pid)
}

// ListProcessesInCgroup restituisce l'elenco dei processi in un cgroup
func (m *Manager) ListProcessesInCgroup(uid int) ([]string, error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return nil, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	pids, err := m.readPidsFromFile(procsFile)
	if err != nil {
		return nil, err
	}

	var processes []string
	for _, pid := range pids {
		processName := m.getProcessName(pid)
		processes = append(processes, fmt.Sprintf("%s[%d]", processName, pid))
	}

	return processes, nil
}

// ApplyRAMLimit applica un limite di RAM a un cgroup utente.
// limit: bytes (es. "536870912") o suffissi (es. "512M", "1G", "2T")
func (m *Manager) ApplyRAMLimit(uid int, limit string) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		if err := m.CreateUserCgroup(uid); err != nil {
			return fmt.Errorf("failed to create cgroup before applying RAM limit: %w", err)
		}
		cgroupPath, _ = m.getCgroupPath(uid)
	}

	memoryMaxFile := filepath.Join(cgroupPath, "memory.max")

	limitValue := limit
	if limit == "" || limit == "0" {
		limitValue = "max"
	}

	if err := os.WriteFile(memoryMaxFile, []byte(limitValue), defaultFilePerm); err != nil {
		return fmt.Errorf("failed to apply RAM limit for UID %d: %w", uid, err)
	}

	m.logger.Debug("RAM limit applied",
		"uid", uid,
		"limit", limitValue,
		"path", memoryMaxFile,
	)

	return nil
}

// ApplyRAMLimitWithSwapDisabled applica un limite di RAM con swap disabilitato.
func (m *Manager) ApplyRAMLimitWithSwapDisabled(uid int, limit string) error {
	if err := m.ApplyRAMLimit(uid, limit); err != nil {
		return err
	}

	cgroupPath, _ := m.getCgroupPath(uid)
	swapMaxFile := filepath.Join(cgroupPath, "memory.swap.max")

	if err := os.WriteFile(swapMaxFile, []byte("0"), defaultFilePerm); err != nil {
		return fmt.Errorf("failed to disable swap for UID %d: %w", uid, err)
	}

	m.logger.Debug("Swap disabled for UID",
		"uid", uid,
		"path", swapMaxFile,
	)

	return nil
}

// RemoveRAMLimit rimuove il limite di RAM (imposta a "max").
func (m *Manager) RemoveRAMLimit(uid int) error {
	return m.ApplyRAMLimit(uid, "max")
}

// GetCgroupRAMUsage restituisce l'uso corrente di RAM del cgroup utente in bytes.
func (m *Manager) GetCgroupRAMUsage(uid int) (uint64, error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return 0, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	memoryCurrentFile := filepath.Join(cgroupPath, "memory.current")
	data, err := os.ReadFile(memoryCurrentFile)
	if err != nil {
		return 0, fmt.Errorf("failed to read RAM usage for UID %d: %w", uid, err)
	}

	usage, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse RAM usage for UID %d: %w", uid, err)
	}

	return usage, nil
}

// ApplyRAMHigh applica un limite soft di RAM (memory.high) a un cgroup utente.
// Quando un cgroup supera memory.high, il kernel applica throttling e reclaim aggressivo,
// ma NON invoca l'OOM killer. Utile per segnalare pressione di memoria senza uccidere processi.
// limit: bytes (es. "536870912") o suffissi (es. "512M", "1G", "2T")
func (m *Manager) ApplyRAMHigh(uid int, limit string) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		if err := m.CreateUserCgroup(uid); err != nil {
			return fmt.Errorf("failed to create cgroup before applying RAM high: %w", err)
		}
		cgroupPath, _ = m.getCgroupPath(uid)
	}

	memoryHighFile := filepath.Join(cgroupPath, "memory.high")

	limitValue := limit
	if limit == "" || limit == "0" {
		limitValue = "max"
	}

	if err := os.WriteFile(memoryHighFile, []byte(limitValue), defaultFilePerm); err != nil {
		return fmt.Errorf("failed to apply RAM high limit for UID %d: %w", uid, err)
	}

	m.logger.Debug("RAM high limit applied",
		"uid", uid,
		"limit", limitValue,
		"path", memoryHighFile,
	)

	return nil
}

// ApplyRAMLimitWithHigh applica sia memory.high (soft limit) che memory.max (hard limit).
// memory.high: throttling e reclaim aggressivo quando superato
// memory.max: OOM killer quando superato
func (m *Manager) ApplyRAMLimitWithHigh(uid int, maxLimit string, highLimit string) error {
	// Applica prima il soft limit (memory.high)
	if err := m.ApplyRAMHigh(uid, highLimit); err != nil {
		return fmt.Errorf("failed to apply RAM high: %w", err)
	}

	// Applica il hard limit (memory.max)
	if err := m.ApplyRAMLimit(uid, maxLimit); err != nil {
		return fmt.Errorf("failed to apply RAM max: %w", err)
	}

	m.logger.Info("RAM limits applied (high + max)",
		"uid", uid,
		"high", highLimit,
		"max", maxLimit,
	)

	return nil
}

// ApplyRAMLimitWithHighAndSwapDisabled applica memory.high, memory.max e disabilita swap.
func (m *Manager) ApplyRAMLimitWithHighAndSwapDisabled(uid int, maxLimit string, highLimit string) error {
	if err := m.ApplyRAMLimitWithHigh(uid, maxLimit, highLimit); err != nil {
		return err
	}

	cgroupPath, _ := m.getCgroupPath(uid)
	swapMaxFile := filepath.Join(cgroupPath, "memory.swap.max")

	if err := os.WriteFile(swapMaxFile, []byte("0"), defaultFilePerm); err != nil {
		return fmt.Errorf("failed to disable swap for UID %d: %w", uid, err)
	}

	m.logger.Debug("Swap disabled for UID",
		"uid", uid,
		"path", swapMaxFile,
	)

	return nil
}

// RemoveRAMHigh rimuove il limite soft di RAM (imposta a "max").
func (m *Manager) RemoveRAMHigh(uid int) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return fmt.Errorf("cgroup for UID %d not found", uid)
	}

	memoryHighFile := filepath.Join(cgroupPath, "memory.high")
	return os.WriteFile(memoryHighFile, []byte("max"), defaultFilePerm)
}

// GetMemoryHighEvents restituisce il numero di volte che il cgroup ha superato memory.high.
// Legge da memory.events il campo "high".
func (m *Manager) GetMemoryHighEvents(uid int) (uint64, error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return 0, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	memoryEventsFile := filepath.Join(cgroupPath, "memory.events")
	data, err := os.ReadFile(memoryEventsFile)
	if err != nil {
		return 0, fmt.Errorf("failed to read memory.events for UID %d: %w", uid, err)
	}

	// Parse "high 123" da memory.events
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "high ") {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				return strconv.ParseUint(parts[1], 10, 64)
			}
		}
	}

	return 0, nil
}
