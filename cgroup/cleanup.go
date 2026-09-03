package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (m *Manager) CleanupUserCgroup(uid int) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		cgroupPath = m.getUserCgroupPath(uid)
		if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
			return nil
		}
	}

	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	pids, err := m.readPidsFromFile(procsFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read cgroup processes for UID %d: %w", uid, err)
	}

	if m.isRecoveryPath(cgroupPath) && len(pids) > 0 {
		m.logger.Info("Preserving populated recovery cgroup during cleanup",
			"uid", uid,
			"path", cgroupPath,
			"process_count", len(pids),
		)
		return nil
	}

	if len(pids) > 0 {
		usedRecovery, err := m.restoreProcesses(uid, pids, normalCPUQuota)
		if err != nil {
			return fmt.Errorf("failed to restore processes before cleanup for UID %d: %w", uid, err)
		}
		m.logger.Info("Processes restored before cgroup cleanup",
			"uid", uid,
			"process_count", len(pids),
			"used_recovery", usedRecovery,
		)
	}

	if err := m.removeManagedCgroupPath(cgroupPath); err != nil {
		return fmt.Errorf("failed to remove cgroup for UID %d: %w", uid, err)
	}

	if err := m.untrackCgroupPath(uid); err != nil {
		return fmt.Errorf("failed to untrack cleaned cgroup for UID %d: %w", uid, err)
	}
	m.blockIOMu.Lock()
	delete(m.blockIOAccounting, uid)
	m.blockIOMu.Unlock()
	m.logger.Debug("Cgroup cleaned up for user",
		"uid", uid,
		"processes_moved", len(pids),
	)
	return nil
}

// RecoverExistingCgroups restores processes left by a previous daemon instance.
func (m *Manager) RecoverExistingCgroups() error {
	_, err := m.RecoverExistingCgroupsWithResult()
	return err
}

// RecoverExistingCgroupsWithResult restores processes while preserving every
// per-process disposition for startup diagnostics and bounded metrics.
func (m *Manager) RecoverExistingCgroupsWithResult() (ProcessRestoreResult, error) {
	sharedPath := filepath.Join(m.getBaseCgroupPath(), "limited")
	if _, err := os.Stat(sharedPath); os.IsNotExist(err) {
		return ProcessRestoreResult{}, nil
	} else if err != nil {
		return ProcessRestoreResult{}, fmt.Errorf("failed to inspect shared cgroup %s: %w", sharedPath, err)
	}
	return m.recoverSharedCgroupWithResult(sharedPath)
}

func (m *Manager) recoverSharedCgroup(sharedPath string) error {
	_, err := m.recoverSharedCgroupWithResult(sharedPath)
	return err
}

func (m *Manager) recoverSharedCgroupWithResult(sharedPath string) (ProcessRestoreResult, error) {
	var restoreResult ProcessRestoreResult
	if err := m.removeSharedCPUQuota(sharedPath); err != nil {
		return restoreResult, fmt.Errorf("failed to remove shared CPU quota before recovery: %w", err)
	}

	var recoveryErrors []error
	parents := []string{
		sharedPath,
		filepath.Join(sharedPath, cpuPointsGuaranteedDomain),
		filepath.Join(sharedPath, cpuPointsBestEffortDomain),
	}
	for _, parent := range parents {
		entries, err := os.ReadDir(parent)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("failed to list managed cgroup %s: %w", parent, err))
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "user_") {
				continue
			}
			uid, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "user_"))
			if err != nil {
				recoveryErrors = append(recoveryErrors, fmt.Errorf("invalid managed user cgroup %s", entry.Name()))
				continue
			}
			result, err := m.ReleaseUserFromSharedCgroupWithResult(uid, parent, normalCPUQuota)
			restoreResult.Add(result)
			if err != nil {
				recoveryErrors = append(recoveryErrors, err)
			}
		}
	}

	topLevelProcs := filepath.Join(sharedPath, "cgroup.procs")
	pids, err := m.readPidsFromFile(topLevelProcs)
	if err != nil && !os.IsNotExist(err) {
		recoveryErrors = append(recoveryErrors, fmt.Errorf("failed to read top-level shared processes: %w", err))
	}
	pidsByUID := make(map[int][]int)
	for _, pid := range pids {
		statusPath := filepath.Join(m.getProcRoot(), strconv.Itoa(pid), "status")
		uid, err := m.getUIDFromStatusFile(statusPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("failed to resolve UID for PID %d: %w", pid, err))
			continue
		}
		pidsByUID[uid] = append(pidsByUID[uid], pid)
	}
	for uid, userPIDs := range pidsByUID {
		result, _, err := m.restoreProcessesExpectedResult(uid, userPIDs, normalCPUQuota, nil, "", true)
		restoreResult.Add(result)
		if err != nil {
			recoveryErrors = append(recoveryErrors, err)
			continue
		}
		if result.Count(ProcessRestoreRecovery) > 0 {
			if err := m.trackCgroupPath(uid, m.getRecoveryCgroupPath(uid)); err != nil {
				recoveryErrors = append(recoveryErrors, err)
			}
		}
	}

	if len(recoveryErrors) > 0 {
		return restoreResult, errors.Join(recoveryErrors...)
	}
	for _, domain := range []string{filepath.Join(sharedPath, cpuPointsGuaranteedDomain), filepath.Join(sharedPath, cpuPointsBestEffortDomain)} {
		if err := m.removeManagedCgroupPath(domain); err != nil && !os.IsNotExist(err) {
			return restoreResult, fmt.Errorf("failed to remove recovered CPU Points domain %s: %w", domain, err)
		}
	}
	if err := m.removeManagedCgroupPath(sharedPath); err != nil {
		return restoreResult, fmt.Errorf("failed to remove recovered shared cgroup %s: %w", sharedPath, err)
	}
	m.logger.Info("Recovered processes from existing shared cgroup", "path", sharedPath)
	return restoreResult, nil
}

const cgroupRemovalRetryDelay = 25 * time.Millisecond

type cgroupRemovalResult struct {
	retried bool
}

func removeCgroupWithRetry(path string) (cgroupRemovalResult, error) {
	return removeCgroupWithRetryUsing(path, os.Remove, waitBeforeCgroupRemovalRetry)
}

func removeCgroupWithRetryUsing(path string, remove func(string) error, backoff func()) (cgroupRemovalResult, error) {
	if err := remove(path); err == nil || os.IsNotExist(err) {
		return cgroupRemovalResult{}, nil
	}
	result := cgroupRemovalResult{retried: true}
	backoff()
	if err := remove(path); err != nil && !os.IsNotExist(err) {
		return result, err
	}
	return result, nil
}

func waitBeforeCgroupRemovalRetry() {
	time.Sleep(cgroupRemovalRetryDelay)
}

// CleanupAll removes empty managed cgroups while preserving populated recovery cgroups.
func (m *Manager) CleanupAll() error {
	m.logger.Info("Starting cgroup cleanup", "tracked_count", len(m.GetCreatedCgroups()))
	m.wg.Wait()

	var cleanupErrors []error
	if err := m.RecoverExistingCgroups(); err != nil {
		return fmt.Errorf("failed to recover shared cgroup during cleanup: %w", err)
	}

	for _, uid := range m.GetCreatedCgroups() {
		if err := m.CleanupUserCgroup(uid); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("UID %d: %w", uid, err))
		}
	}

	recoveryRoot := m.getRecoveryRootPath()
	if hasChildren, err := hasChildCgroups(recoveryRoot); err == nil && !hasChildren {
		if err := os.Remove(recoveryRoot); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove empty recovery root: %w", err))
		}
	}

	baseCgroupPath := m.getBaseCgroupPath()
	if hasChildren, err := hasChildCgroups(baseCgroupPath); err == nil && !hasChildren {
		if err := os.Remove(baseCgroupPath); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove empty base cgroup: %w", err))
		}
	}

	if len(cleanupErrors) > 0 {
		return fmt.Errorf("errors during cleanup: %w", errors.Join(cleanupErrors...))
	}
	m.logger.Info("Cgroup cleanup completed")
	return nil
}

func hasChildCgroups(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return true, nil
		}
	}
	return false, nil
}
