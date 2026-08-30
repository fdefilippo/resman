package cgroup

import (
	"context"
	"errors"
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

// ApplyCPUQuota writes cpu.max for an already tracked user cgroup without moving processes.
func (m *Manager) ApplyCPUQuota(uid int, quota string) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return fmt.Errorf("cgroup for UID %d not found", uid)
	}

	cpuMaxFile := filepath.Join(cgroupPath, "cpu.max")

	// Validate the quota format.
	if !isValidCPUQuotaFormat(quota) {
		return fmt.Errorf("invalid CPU quota format '%s': expected 'quota period' (e.g., '50000 100000') or 'max period'", quota)
	}

	// Apply the limit.
	if err := os.WriteFile(cpuMaxFile, []byte(quota), 0644); err != nil {
		// Retry after making the control file writable.
		if os.IsPermission(err) {
			if chmodErr := os.Chmod(cpuMaxFile, 0644); chmodErr != nil {
				m.logger.Warn("Failed to chmod CPU max file",
					"path", cpuMaxFile,
					"error", chmodErr,
				)
			}
			err = os.WriteFile(cpuMaxFile, []byte(quota), 0644)
		}
		if err != nil {
			return fmt.Errorf("failed to apply CPU limit %s to %s for UID %d: %w", quota, cpuMaxFile, uid, err)
		}
	}

	// Verify that the limit was applied.
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

	return nil
}

// ApplyCPULimit applies cpu.max and moves the user's processes into the cgroup.
func (m *Manager) ApplyCPULimit(uid int, quota string) error {
	if _, err := m.ensureCgroupPath(uid); err != nil {
		return fmt.Errorf("failed to resolve cgroup before applying CPU limit for UID %d: %w", uid, err)
	}
	if err := m.ApplyCPUQuota(uid, quota); err != nil {
		return err
	}

	// Move processes synchronously. The context stops the loop between process
	// migrations, and no worker remains able to mutate cgroup membership after
	// this method returns.
	timeout := m.operationTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := m.moveUserProcesses(ctx, uid); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			m.logger.Warn("Timed out moving user processes to cgroup",
				"uid", uid,
				"timeout", timeout,
			)
			return fmt.Errorf("move processes to cgroup for UID %d exceeded %v: %w", uid, timeout, context.DeadlineExceeded)
		}
		m.logger.Warn("Failed to move user processes to cgroup",
			"uid", uid,
			"error", err,
		)
		return fmt.Errorf("move processes to cgroup for UID %d: %w", uid, err)
	}

	return nil
}

// ApplyCPUWeight applies a proportional CPU weight to a user cgroup.
func (m *Manager) ApplyCPUWeight(uid int, weight int) error {
	cgroupPath, err := m.ensureCgroupPath(uid)
	if err != nil {
		return fmt.Errorf("failed to resolve cgroup before applying weight: %w", err)
	}

	cpuWeightFile := filepath.Join(cgroupPath, "cpu.weight")

	// Clamp the weight to the kernel-supported range of 1 through 10000.
	if weight < 1 {
		weight = 1
	}
	if weight > 10000 {
		weight = 10000
	}

	// Apply the weight.
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

// RemoveCPULimit removes the CPU limit by setting it to "max".
func (m *Manager) RemoveCPULimit(uid int) error {
	return m.ApplyCPULimit(uid, "max 100000")
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
