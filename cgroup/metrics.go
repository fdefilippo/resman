package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (m *Manager) GetUserCgroupMetrics(uid int) (cgroupPath, cpuQuota string, memoryHighEvents uint64, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64, err error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return "", "", 0, 0, 0, 0, 0, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	// Leggi cpu.max
	cpuMaxFile := filepath.Join(cgroupPath, "cpu.max")
	if data, readErr := os.ReadFile(cpuMaxFile); readErr == nil {
		cpuQuota = strings.TrimSpace(string(data))
	}

	// Leggi memory.high events
	if memEvents, memErr := m.GetMemoryHighEvents(uid); memErr == nil {
		memoryHighEvents = memEvents
	} else {
		m.logger.Debug("Failed to read memory high events (optional metric)", "uid", uid, "error", memErr)
	}

	// Leggi io.stat
	if rBytes, wBytes, rOps, wOps, ioErr := m.GetIOStats(uid); ioErr == nil {
		ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps = rBytes, wBytes, rOps, wOps
	} else {
		m.logger.Debug("Failed to read IO stats (optional metric)", "uid", uid, "error", ioErr)
	}

	return cgroupPath, cpuQuota, memoryHighEvents, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps, nil
}

// getProcessInfo restituisce informazioni dettagliate su un processo
