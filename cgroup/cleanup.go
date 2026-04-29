package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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

	var cleanupErrs []string

	// Clean up all known cgroups from the atomic copy
	for _, uid := range uids {
		if err := m.CleanupUserCgroup(uid); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("UID %d: %v", uid, err))
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
						if err := m.writePidsBatch(rootCgroupProcs, pids); err != nil {
							m.logger.Debug("Failed to move some processes out of user cgroup",
								"from", userPath,
								"error", err,
							)
						}
					}

					if err := os.RemoveAll(userPath); err != nil {
						m.logger.Warn("Failed to remove user sub-cgroup",
							"path", userPath,
							"error", err,
						)
						cleanupErrs = append(cleanupErrs, fmt.Sprintf("user cgroup %s: %v", userPath, err))
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
			if err := m.writePidsBatch(rootCgroupProcs, pids); err != nil {
				m.logger.Debug("Failed to move some processes out of shared cgroup",
					"error", err,
				)
			}
			// Aspetta che il kernel process lo spostamento
			time.Sleep(200 * time.Millisecond)
		}

		// STEP 3: Verifica che il cgroup sia vuoto
		pids, err := m.readPidsFromFile(cgroupProcsFile)
		if err != nil {
			m.logger.Warn("Failed to read pids from cgroup", "path", cgroupProcsFile, "error", err)
		} else if len(pids) > 0 {
			m.logger.Warn("Shared cgroup still has processes after cleanup",
				"path", sharedPath,
				"remaining_count", len(pids),
			)
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("shared cgroup still has %d processes", len(pids)))
		}

		// STEP 4: Ora prova a rimuovere il cgroup condiviso
		m.logger.Info("Removing shared cgroup directory", "path", sharedPath)
		if err := os.RemoveAll(sharedPath); err != nil {
			m.logger.Error("Failed to remove shared cgroup",
				"path", sharedPath,
				"error", err,
			)
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("shared cgroup: %v", err))
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
		cleanupErrs = append(cleanupErrs, fmt.Sprintf("tracking file: %v", err))
	}

	if len(cleanupErrs) > 0 {
		m.logger.Warn("Cleanup completed with errors", "error_count", len(cleanupErrs))
		// Convert string errors to error type for errors.Join
		errs := make([]error, len(cleanupErrs))
		for i, e := range cleanupErrs {
			errs[i] = fmt.Errorf("%s", e)
		}
		return fmt.Errorf("errors during cleanup: %w", errors.Join(errs...))
	}

	m.logger.Info("All cgroups cleaned up successfully")
	return nil
}

// saveCgroupToFile salva un cgroup nel file di tracciamento.
