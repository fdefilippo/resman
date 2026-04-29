package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/shirou/gopsutil/v3/process"
)

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
// Uses gopsutil for efficient process discovery.
func (m *Manager) MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) error {
	m.logger.Debug("Moving all processes for user to shared cgroup",
		"uid", uid,
		"shared_path", sharedPath,
	)

	// Try gopsutil first
	procs, err := process.Processes()
	if err != nil {
		m.logger.Debug("gopsutil failed, falling back to /proc scan", "error", err)
		return m.moveAllUserProcessesToSharedCgroupFallback(uid, sharedPath)
	}

	var movedCount int
	var errors []string

	for _, p := range procs {
		uids, err := p.Uids()
		if err != nil || len(uids) == 0 || int(uids[0]) != uid {
			continue
		}

		pid := int(p.Pid)
		processName := m.getProcessName(pid)

		if m.cfg.IsProcessExcluded(processName) {
			continue
		}

		if err := m.MoveProcessToSharedCgroup(pid, sharedPath, uid); err != nil {
			errors = append(errors, fmt.Sprintf("PID %d: %v", pid, err))
		} else {
			movedCount++
		}
	}

	m.logSharedProcessMoveSummary(uid, movedCount, errors)

	if len(errors) > 0 {
		return fmt.Errorf("some processes could not be moved: %d errors", len(errors))
	}
	return nil
}

// ReleaseUserFromSharedCgroup sposta i processi fuori dal sottocgroup condiviso e lo rimuove.
func (m *Manager) ReleaseUserFromSharedCgroup(uid int, sharedPath string) error {
	userPath := filepath.Join(sharedPath, fmt.Sprintf("user_%d", uid))
	userProcsFile := filepath.Join(userPath, "cgroup.procs")

	if _, err := os.Stat(userPath); os.IsNotExist(err) {
		return nil
	}

	pids, err := m.readPidsFromFile(userProcsFile)
	if err != nil {
		return fmt.Errorf("failed to read user shared cgroup processes for UID %d: %w", uid, err)
	}

	if len(pids) > 0 {
		rootCgroupProcs := filepath.Join(m.cfg.CgroupRoot, "cgroup.procs")
		if err := m.writePidsBatch(rootCgroupProcs, pids); err != nil {
			return fmt.Errorf("failed to move processes out of shared cgroup for UID %d: %w", uid, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := os.Remove(userPath); err != nil {
		return fmt.Errorf("failed to remove user shared cgroup for UID %d: %w", uid, err)
	}

	m.logger.Debug("User released from shared cgroup",
		"uid", uid,
		"path", userPath,
		"processes_moved", len(pids),
	)
	return nil
}

// moveAllUserProcessesToSharedCgroupFallback scans /proc manually if gopsutil fails.
func (m *Manager) moveAllUserProcessesToSharedCgroupFallback(uid int, sharedPath string) error {
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

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		statusFile := filepath.Join(procDir, entry.Name(), "status")
		if procUID, err := m.getUIDFromStatusFile(statusFile); err == nil && procUID == uid {
			processName := m.getProcessName(pid)

			if m.cfg.IsProcessExcluded(processName) {
				continue
			}

			if err := m.MoveProcessToSharedCgroup(pid, sharedPath, uid); err != nil {
				errors = append(errors, fmt.Sprintf("PID %d: %v", pid, err))
			} else {
				movedCount++
			}
		}
	}

	m.logSharedProcessMoveSummary(uid, movedCount, errors)

	if len(errors) > 0 {
		return fmt.Errorf("some processes could not be moved: %d errors", len(errors))
	}
	return nil
}

// logSharedProcessMoveSummary logs a summary of shared cgroup process movement.
func (m *Manager) logSharedProcessMoveSummary(uid, movedCount int, errors []string) {
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
}

// getUIDFromStatusFile estrae il UID dal file /proc/[pid]/status.
