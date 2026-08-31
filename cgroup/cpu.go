package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (m *Manager) CreateUserCgroup(uid int) error {
	// Check whether the cgroup already exists.
	if existingPath, exists := m.getCgroupPath(uid); exists {
		if _, err := os.Stat(existingPath); err == nil {
			m.logger.Debug("Cgroup already exists for user", "uid", uid)
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat existing cgroup %s for UID %d: %w", existingPath, uid, err)
		}
		if err := m.untrackCgroupPath(uid); err != nil {
			m.logger.Warn("Failed to remove stale cgroup tracking entry",
				"uid", uid,
				"path", existingPath,
				"error", err,
			)
		}
	}

	cgroupPath := m.getUserCgroupPath(uid)

	// Create the cgroup directory.
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return fmt.Errorf("failed to create cgroup directory %s for UID %d: %w", cgroupPath, uid, err)
	}

	if err := m.trackCgroupPath(uid, cgroupPath); err != nil {
		m.logger.Warn("Failed to save cgroup to tracking file",
			"uid", uid,
			"error", err,
		)
		// Do not fail solely because the tracking file could not be updated.
	}

	m.logger.Debug("Cgroup created for user", "uid", uid, "path", cgroupPath)
	return nil
}

// EnsureUnlimitedCPUQuota verifies that one tracked non-CPU-policy cgroup does
// not inherit a finite CPU ceiling. Finite quotas are owned by CPU Points.
func (m *Manager) EnsureUnlimitedCPUQuota(uid int) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return fmt.Errorf("cgroup for UID %d not found", uid)
	}
	if err := writeCPUPointsValue(filepath.Join(cgroupPath, "cpu.max"), normalCPUQuota); err != nil {
		return fmt.Errorf("verify unlimited CPU quota for UID %d: %w", uid, err)
	}
	return nil
}

// isValidCPUQuotaFormat validates a cpu.max quota value.
func isValidCPUQuotaFormat(quota string) bool {
	parts := strings.Fields(quota)
	if len(parts) != 2 {
		return false
	}

	// The first field may be "max" or a number.
	if parts[0] == "max" {
		_, err := strconv.Atoi(parts[1])
		return err == nil
	}

	// Otherwise, both fields must be numeric.
	_, err1 := strconv.Atoi(parts[0])
	_, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil
}

// GetCreatedCgroups returns the UIDs with active cgroups.
