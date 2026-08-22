package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fdefilippo/resman/internal/processpolicy"
)

// CreateSharedCgroup creates the shared hierarchy used for CPU-limited users.
func (m *Manager) CreateSharedCgroup() (string, error) {
	sharedPath := filepath.Join(m.getBaseCgroupPath(), "limited")

	if _, err := os.Stat(sharedPath); err == nil {
		m.logger.Info("Shared cgroup already exists, restoring contained processes before recreation",
			"path", sharedPath,
		)
		if err := m.recoverSharedCgroup(sharedPath); err != nil {
			return "", fmt.Errorf("failed to recover existing shared cgroup %s: %w", sharedPath, err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to inspect shared cgroup %s: %w", sharedPath, err)
	}

	// Create the shared cgroup directory.
	if err := os.MkdirAll(sharedPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create shared cgroup directory: %w", err)
	}

	// A startup probe has already proved these interfaces are usable. Keep
	// controller enablement fatal here so the decision engine cannot enter a
	// permanent apply-fail loop if the hierarchy changes at runtime.
	subtreeControl := filepath.Join(sharedPath, "cgroup.subtree_control")
	requirements := enabledControllerInterfaces(m.getConfig())
	if _, err := m.enableControllerInterfaces(subtreeControl, requirements, requirements); err != nil {
		cleanupErr := os.Remove(sharedPath)
		return "", errors.Join(err, cleanupErr)
	}
	controllersData, err := os.ReadFile(filepath.Join(sharedPath, "cgroup.controllers"))
	if err != nil {
		m.logger.Warn("Could not inspect optional controllers in shared cgroup",
			"path", sharedPath,
			"error", err,
		)
	} else {
		m.enableOptionalCPUSet(subtreeControl, string(controllersData), "shared cgroup")
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

	if err := m.trackCgroupPath(uid, userPath); err != nil {
		m.logger.Warn("Failed to track user sub-cgroup",
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
	movable, err := m.captureProcessOrigin(pid, uid, userPath)
	if err != nil {
		return fmt.Errorf("failed to persist cgroup origin for PID %d: %w", pid, err)
	}
	if !movable {
		return nil
	}

	if err := m.writePIDToCgroup(cgroupProcsFile, pid); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			_ = m.removeProcessOrigins(map[int]bool{pid: true})
			return nil
		}
		return fmt.Errorf("failed to move PID %d to shared cgroup for UID %d: %w", pid, uid, err)
	}

	return nil
}

// MoveAllUserProcessesToSharedCgroup moves every enforceable user process into
// its shared-cgroup child.
// Uses gopsutil for efficient process discovery.
func (m *Manager) MoveAllUserProcessesToSharedCgroup(uid int, sharedPath string) error {
	m.logger.Debug("Moving all processes for user to shared cgroup",
		"uid", uid,
		"shared_path", sharedPath,
	)
	userPath := filepath.Join(sharedPath, fmt.Sprintf("user_%d", uid))
	if _, err := os.Stat(userPath); os.IsNotExist(err) {
		if _, err := m.CreateUserSubCgroup(uid, sharedPath); err != nil {
			return fmt.Errorf("failed to create user sub-cgroup: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to inspect user sub-cgroup %s: %w", userPath, err)
	}

	pidsForUID, err := m.processIDsForUID(uid)
	if err != nil {
		return err
	}

	var movedCount int
	var errors []string
	var pids []int
	cfg := m.getConfig()

	for _, pid := range pidsForUID {
		processInfo, infoErr := m.getProcessInfo(pid)
		if infoErr != nil {
			m.logger.Debug("Failed to read process details before shared-cgroup migration",
				"pid", pid,
				"error", infoErr,
			)
		}
		selection := processpolicy.Evaluate(cfg, processInfo["executable"], processInfo["name"])
		if !selection.Enforceable {
			continue
		}
		pids = append(pids, pid)
	}

	moved, moveErrors, err := m.moveProcessBatch(pids, uid, userPath)
	if err != nil {
		errors = append(errors, err.Error())
	} else {
		movedCount = len(moved)
		for pid, moveErr := range moveErrors {
			errors = append(errors, fmt.Sprintf("PID %d: %v", pid, moveErr))
		}
	}

	m.logSharedProcessMoveSummary(uid, movedCount, len(pids), errors)

	if len(errors) > 0 {
		return fmt.Errorf("some processes could not be moved: %d errors", len(errors))
	}
	return nil
}

// ReleaseUserFromSharedCgroup sposta i processi fuori dal sottocgroup condiviso e lo rimuove.
func (m *Manager) ReleaseUserFromSharedCgroup(uid int, sharedPath, normalQuota string) error {
	userPath := filepath.Join(sharedPath, fmt.Sprintf("user_%d", uid))
	userProcsFile := filepath.Join(userPath, "cgroup.procs")

	if _, err := os.Stat(userPath); os.IsNotExist(err) {
		return m.untrackCgroupPathIf(uid, userPath)
	}

	pids, err := m.readPidsFromFile(userProcsFile)
	if err != nil {
		return fmt.Errorf("failed to read user shared cgroup processes for UID %d: %w", uid, err)
	}

	usedRecovery, err := m.restoreProcesses(uid, pids, normalQuota)
	if err != nil {
		return fmt.Errorf("failed to restore processes from shared cgroup for UID %d: %w", uid, err)
	}
	if len(pids) > 0 {
		time.Sleep(100 * time.Millisecond)
	}

	if err := os.Remove(userPath); err != nil {
		return fmt.Errorf("failed to remove user shared cgroup for UID %d: %w", uid, err)
	}

	if err := m.untrackCgroupPathIf(uid, userPath); err != nil {
		m.logger.Warn("Failed to untrack user shared cgroup",
			"uid", uid,
			"path", userPath,
			"error", err,
		)
	}
	if usedRecovery {
		recoveryPath := m.getRecoveryCgroupPath(uid)
		if err := m.trackCgroupPath(uid, recoveryPath); err != nil {
			return fmt.Errorf("failed to track recovery cgroup for UID %d: %w", uid, err)
		}
		m.logger.Warn("Processes restored to resman recovery cgroup because their original cgroup was unavailable",
			"uid", uid,
			"path", recoveryPath,
			"normal_quota", normalQuota,
		)
	}

	m.logger.Debug("User released from shared cgroup",
		"uid", uid,
		"path", userPath,
		"processes_moved", len(pids),
	)
	return nil
}

// logSharedProcessMoveSummary logs a summary of shared cgroup process movement.
func (m *Manager) logSharedProcessMoveSummary(uid, movedCount, candidateCount int, errors []string) {
	if movedCount > 0 {
		m.logger.Debug("Processes moved to shared cgroup",
			"uid", uid,
			"moved_count", movedCount,
			"error_count", len(errors),
		)
	} else if candidateCount == 0 {
		m.logger.Warn("No processes moved for user to shared cgroup",
			"uid", uid,
			"possible_reasons", "no processes found or permission issues",
		)
	} else {
		m.logger.Debug("No process migration required for shared cgroup",
			"uid", uid,
			"candidate_count", candidateCount,
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
