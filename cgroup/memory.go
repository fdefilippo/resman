package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (m *Manager) ApplyRAMLimit(uid int, limit string) error {
	cgroupPath, err := m.ensureCgroupPath(uid)
	if err != nil {
		return fmt.Errorf("failed to resolve cgroup before applying RAM limit: %w", err)
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

// ApplyRAMLimitWithSwapDisabled applies a RAM limit and disables swap.
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

// RemoveRAMLimit removes the RAM limit by setting it to "max".
func (m *Manager) RemoveRAMLimit(uid int) error {
	return m.ApplyRAMLimit(uid, "max")
}

// RemoveRAMSwapLimit removes the swap limit by setting memory.swap.max to max.
func (m *Manager) RemoveRAMSwapLimit(uid int) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return fmt.Errorf("cgroup for UID %d not found", uid)
	}

	swapMaxFile := filepath.Join(cgroupPath, "memory.swap.max")
	if err := os.WriteFile(swapMaxFile, []byte("max"), defaultFilePerm); err != nil {
		return fmt.Errorf("failed to remove swap limit for UID %d: %w", uid, err)
	}
	return nil
}

// GetCgroupRAMUsage returns the current RAM usage of a user cgroup in bytes.
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

// ApplyRAMHigh applies a soft RAM limit through memory.high to a user cgroup.
// Above memory.high, the kernel throttles and aggressively reclaims memory without
// invoking the OOM killer. The limit accepts bytes or K/M/G/T suffixes.
func (m *Manager) ApplyRAMHigh(uid int, limit string) error {
	cgroupPath, err := m.ensureCgroupPath(uid)
	if err != nil {
		return fmt.Errorf("failed to resolve cgroup before applying RAM high: %w", err)
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

// ApplyRAMLimitWithHigh applies memory.high as a soft limit and memory.max as a hard limit.
// memory.high triggers throttling and aggressive reclaim; memory.max may invoke the OOM killer.
func (m *Manager) ApplyRAMLimitWithHigh(uid int, maxLimit string, highLimit string) error {
	// Apply the soft limit first.
	if err := m.ApplyRAMHigh(uid, highLimit); err != nil {
		return fmt.Errorf("failed to apply RAM high: %w", err)
	}

	// Apply the hard limit.
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

// ApplyRAMLimitWithHighAndSwapDisabled applies memory.high and memory.max and disables swap.
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

// RemoveRAMHigh removes the soft RAM limit by setting it to "max".
func (m *Manager) RemoveRAMHigh(uid int) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return fmt.Errorf("cgroup for UID %d not found", uid)
	}

	memoryHighFile := filepath.Join(cgroupPath, "memory.high")
	return os.WriteFile(memoryHighFile, []byte("max"), defaultFilePerm)
}

// GetMemoryHighEvents returns how many times the cgroup exceeded memory.high.
// It reads the "high" field from memory.events.
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

	// Parse entries such as "high 123" from memory.events.
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

// ApplyIOLimit applies I/O bandwidth and IOPS limits to a user cgroup.
// It writes the cgroup's io.max file. Bandwidth values are strings such as
// "100M" or "max"; IOPS values are integers where zero means unlimited.
