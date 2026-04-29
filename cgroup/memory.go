package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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

// ApplyIOLimit applica limiti di IO (bandwidth e IOPS) a un cgroup utente.
// Scrive nel file io.max del cgroup.
// readBPS, writeBPS: bytes per secondo (stringa, es. "100M", "max")
// readIOPS, writeIOPS: operazioni per secondo (int, 0 = unlimited)
