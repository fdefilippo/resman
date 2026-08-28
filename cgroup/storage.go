package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (m *Manager) saveCgroupToFile(uid int, cgroupPath string) error {
	file, err := os.OpenFile(m.createdCgroupsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	_, err = fmt.Fprintf(file, "%d:%s\n", uid, cgroupPath)
	return err
}

func (m *Manager) trackCgroupPath(uid int, cgroupPath string) error {
	m.mu.Lock()
	m.createdCgroups[uid] = cgroupPath
	m.mu.Unlock()

	if err := m.removeCgroupFromFile(uid); err != nil {
		return err
	}
	return m.saveCgroupToFile(uid, cgroupPath)
}

func (m *Manager) untrackCgroupPath(uid int) error {
	m.mu.Lock()
	delete(m.createdCgroups, uid)
	m.mu.Unlock()

	return m.removeCgroupFromFile(uid)
}

func (m *Manager) untrackCgroupPathIf(uid int, expectedPath string) error {
	m.mu.Lock()
	currentPath, exists := m.createdCgroups[uid]
	if !exists || filepath.Clean(currentPath) != filepath.Clean(expectedPath) {
		m.mu.Unlock()
		return nil
	}
	delete(m.createdCgroups, uid)
	m.mu.Unlock()

	return m.removeCgroupFromFile(uid)
}

// removeCgroupFromFile removes a cgroup from the tracking file.
func (m *Manager) removeCgroupFromFile(uid int) error {
	// Read the file, filter its entries, and rewrite it.
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

	// Rewrite the file.
	return os.WriteFile(m.createdCgroupsFile, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

// loadExistingCgroups loads existing cgroups from the tracking file.
func (m *Manager) loadExistingCgroups() error {
	if _, err := os.Stat(m.createdCgroupsFile); os.IsNotExist(err) {
		return nil
	}

	file, err := os.Open(m.createdCgroupsFile)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

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
		// Verify that the cgroup still exists.
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

// getCgroupPath returns the cgroup path for a UID.
func (m *Manager) getCgroupPath(uid int) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	path, exists := m.createdCgroups[uid]
	return path, exists
}

func (m *Manager) ensureCgroupPath(uid int) (string, error) {
	if cgroupPath, exists := m.getCgroupPath(uid); exists {
		if _, err := os.Stat(cgroupPath); err == nil {
			return cgroupPath, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to stat cgroup for UID %d at %s: %w", uid, cgroupPath, err)
		}
		if err := m.untrackCgroupPath(uid); err != nil {
			m.logger.Warn("Failed to remove stale cgroup tracking entry",
				"uid", uid,
				"path", cgroupPath,
				"error", err,
			)
		}
	}

	if err := m.CreateUserCgroup(uid); err != nil {
		return "", err
	}

	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return "", fmt.Errorf("cgroup for UID %d not found after creation", uid)
	}
	return cgroupPath, nil
}

// readPidsFromFile reads PIDs from a cgroup.procs file.
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

// GetCreatedCgroups returns the UIDs tracked by the cgroup manager.
func (m *Manager) GetCreatedCgroups() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	uids := make([]int, 0, len(m.createdCgroups))
	for uid := range m.createdCgroups {
		uids = append(uids, uid)
	}
	return uids
}

// CgroupFileUnavailableReason classifies why a cgroup interface could not be read.
type CgroupFileUnavailableReason string

const (
	CgroupFileNotPresent       CgroupFileUnavailableReason = "not_present"
	CgroupFilePermissionDenied CgroupFileUnavailableReason = "permission_denied"
	CgroupFileReadError        CgroupFileUnavailableReason = "read_error"
)

// CgroupFileValue reports the raw value, readability, and bounded failure reason of one cgroup interface.
type CgroupFileValue struct {
	Value             string
	Available         bool
	UnavailableReason CgroupFileUnavailableReason
}

// CgroupInfo is the typed observation contract for a managed user cgroup.
type CgroupInfo struct {
	Path          string
	CPUQuota      CgroupFileValue
	CPUWeight     CgroupFileValue
	MemoryCurrent CgroupFileValue
	MemoryMax     CgroupFileValue
	MemoryHigh    CgroupFileValue
}

// GetCgroupInfo returns typed information about a managed user cgroup.
func (m *Manager) GetCgroupInfo(uid int) (CgroupInfo, error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return CgroupInfo{}, fmt.Errorf("cgroup for UID %d not found", uid)
	}
	readFile := m.readCgroupFile
	if readFile == nil {
		readFile = os.ReadFile
	}

	return CgroupInfo{
		Path:          cgroupPath,
		CPUQuota:      readCgroupFileValue(filepath.Join(cgroupPath, "cpu.max"), readFile),
		CPUWeight:     readCgroupFileValue(filepath.Join(cgroupPath, "cpu.weight"), readFile),
		MemoryCurrent: readCgroupFileValue(filepath.Join(cgroupPath, "memory.current"), readFile),
		MemoryMax:     readCgroupFileValue(filepath.Join(cgroupPath, "memory.max"), readFile),
		MemoryHigh:    readCgroupFileValue(filepath.Join(cgroupPath, "memory.high"), readFile),
	}, nil
}

func readCgroupFileValue(path string, readFile func(string) ([]byte, error)) CgroupFileValue {
	data, err := readFile(path)
	if err != nil {
		return CgroupFileValue{UnavailableReason: classifyCgroupFileReadError(err)}
	}
	return CgroupFileValue{Value: strings.TrimSpace(string(data)), Available: true}
}

func classifyCgroupFileReadError(err error) CgroupFileUnavailableReason {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return CgroupFileNotPresent
	case errors.Is(err, os.ErrPermission):
		return CgroupFilePermissionDenied
	default:
		return CgroupFileReadError
	}
}

// GetUserCgroupMetrics reads all cgroup metrics for a user in one call.
// It avoids repeated reads of separate cgroup files.
