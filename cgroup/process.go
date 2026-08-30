package cgroup

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/processidentity"
	"github.com/fdefilippo/resman/internal/processpolicy"
	"github.com/shirou/gopsutil/v3/process"
)

const processScanCacheTTL = time.Second

type cachedUsername struct {
	username string
	cachedAt time.Time
}

type processScanCache struct {
	createdAt time.Time
	pidsByUID map[int][]int
}

func (m *Manager) MoveProcessToCgroup(pid int, uid int) (ProcessMoveResult, error) {
	return m.moveProcessToCgroup(pid, uid, nil)
}

func (m *Manager) moveProcessToCgroup(pid int, uid int, processInfo map[string]string) (ProcessMoveResult, error) {
	var result ProcessMoveResult
	// SECURITY: Never move any process to UID 0 cgroup
	if uid == 0 {
		m.logger.Warn("Refusing to move process to root (UID 0) cgroup - security boundary",
			"pid", pid)
		return result, fmt.Errorf("processes cannot be moved to UID 0 (root) cgroups")
	}

	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return result, fmt.Errorf("cgroup for UID %d does not exist", uid)
	}

	var err error
	if processInfo == nil {
		processInfo, err = m.getProcessInfo(pid)
		if err != nil {
			m.logger.Warn("Failed to get process info", "pid", pid, "error", err)
		}
	}
	processName := processNameFromInfo(pid, processInfo)

	_, result, moveErrors, err := m.moveProcessBatch([]int{pid}, uid, cgroupPath)
	if err != nil {
		return result, fmt.Errorf("prepare PID %d for cgroup ingress for UID %d: %w", pid, uid, err)
	}
	if moveErr := moveErrors[pid]; moveErr != nil {
		return result, fmt.Errorf("failed to move PID %d to cgroup for UID %d: %w", pid, uid, moveErr)
	}
	if result.Moved == 0 {
		return result, nil
	}

	// Log the verified migration without changing the bounded warning contract.
	m.logger.Debug("Process moved to cgroup",
		"pid", pid,
		"uid", uid,
		"process_name", processName,
		"process_state", processInfo["state"],
		"username", processInfo["username"],
		"cgroup_path", cgroupPath,
	)

	return result, nil
}

// MoveAllUserProcesses moves every enforceable process owned by a user into its cgroup.
// Uses gopsutil for efficient process discovery.
func (m *Manager) MoveAllUserProcesses(uid int) (ProcessMoveResult, error) {
	return m.moveAllUserProcesses(context.Background(), uid)
}

func (m *Manager) moveAllUserProcesses(ctx context.Context, uid int) (ProcessMoveResult, error) {
	var result ProcessMoveResult
	m.logger.Debug("Moving all processes for user to cgroup", "uid", uid)

	// SECURITY: Never move UID 0 (root) processes to user cgroups
	if uid == 0 {
		m.logger.Warn("Refusing to move root (UID 0) processes to cgroup - security boundary")
		return result, fmt.Errorf("UID 0 (root) processes cannot be moved to user cgroups")
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("move processes for UID %d interrupted before discovery: %w", uid, err)
	}

	pids, err := m.processIDsForUID(uid)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("move processes for UID %d interrupted after discovery: %w", uid, err)
	}

	var totalProcesses int
	var processNames, errors []string
	var candidates []int
	processNameByPID := make(map[int]string)
	cfg := m.getConfig()

	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("move processes for UID %d interrupted after %d candidates: %w", uid, totalProcesses, err)
		}
		totalProcesses++
		processInfo, infoErr := m.getProcessInfo(pid)
		if os.IsNotExist(infoErr) {
			continue
		}
		if infoErr != nil {
			m.logger.Error("Trusted executable identity unavailable before migration; process remains enforceable",
				"pid", pid,
				"error", infoErr,
				"policy", "fail_closed",
			)
		}
		selection := processSelectionFromInfo(cfg, processInfo)

		// Excluded processes stay in their original cgroup and out of decision inputs.
		if !selection.Enforceable {
			continue
		}

		candidates = append(candidates, pid)
		processNameByPID[pid] = selection.Name
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("move processes for UID %d interrupted after %d candidates: %w", uid, totalProcesses, err)
	}
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return result, fmt.Errorf("cgroup for UID %d does not exist", uid)
	}
	moved, result, moveErrors, err := m.moveProcessBatch(candidates, uid, cgroupPath)
	if err != nil {
		errors = append(errors, err.Error())
	}
	for _, pid := range moved {
		processNames = append(processNames, processNameByPID[pid])
	}
	for pid, moveErr := range moveErrors {
		errors = append(errors, fmt.Sprintf("%s: %v", processNameByPID[pid], moveErr))
	}

	m.logProcessMoveSummary(uid, result.Moved, totalProcesses, processNames, errors)

	if len(errors) > 0 {
		return result, fmt.Errorf("some processes could not be moved: %d errors", len(errors))
	}
	return result, nil
}

func (m *Manager) processIDsForUID(uid int) ([]int, error) {
	leaveScan := m.processScanGate.Enter()
	defer leaveScan()

	now := time.Now()
	if now.Sub(m.processScan.createdAt) <= processScanCacheTTL {
		return append([]int(nil), m.processScan.pidsByUID[uid]...), nil
	}

	var pidsByUID map[int][]int
	var err error
	if m.scanProcessIDs != nil {
		pidsByUID, err = m.scanProcessIDs()
	} else {
		pidsByUID, err = m.scanProcessIDsByUID()
	}
	if err != nil {
		return nil, err
	}
	m.processScan = processScanCache{
		createdAt: time.Now(),
		pidsByUID: pidsByUID,
	}
	return append([]int(nil), pidsByUID[uid]...), nil
}

func (m *Manager) scanProcessIDsByUID() (map[int][]int, error) {
	procs, err := process.Processes()
	if err == nil {
		pidsByUID := make(map[int][]int)
		for _, p := range procs {
			uids, uidErr := p.Uids()
			if uidErr != nil || len(uids) == 0 {
				continue
			}
			uid := int(uids[0])
			pidsByUID[uid] = append(pidsByUID[uid], int(p.Pid))
		}
		return pidsByUID, nil
	}

	m.logger.Debug("gopsutil failed, falling back to /proc scan", "error", err)
	procDir := m.getProcRoot()
	entries, readErr := os.ReadDir(procDir)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read %s: %w", procDir, readErr)
	}

	pidsByUID := make(map[int][]int)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		statusFile := filepath.Join(procDir, entry.Name(), "status")
		procUID, uidErr := m.getUIDFromStatusFile(statusFile)
		if uidErr == nil {
			pidsByUID[procUID] = append(pidsByUID[procUID], pid)
		}
	}
	return pidsByUID, nil
}

// logProcessMoveSummary logs a summary of process movement.
func (m *Manager) logProcessMoveSummary(uid, movedCount, totalProcesses int, processNames, errors []string) {
	if movedCount > 0 {
		if len(processNames) <= 10 {
			m.logger.Info("User processes moved to cgroup",
				"uid", uid,
				"moved_count", movedCount,
				"total_found", totalProcesses,
				"processes", strings.Join(processNames, ", "),
				"error_count", len(errors),
				"success_rate", fmt.Sprintf("%.1f%%", float64(movedCount)/float64(totalProcesses)*100),
			)
		} else {
			m.logger.Info("User processes moved to cgroup",
				"uid", uid,
				"moved_count", movedCount,
				"total_found", totalProcesses,
				"sample_processes", strings.Join(processNames[:10], ", "),
				"and_more", fmt.Sprintf("%d more processes", len(processNames)-10),
				"error_count", len(errors),
				"success_rate", fmt.Sprintf("%.1f%%", float64(movedCount)/float64(totalProcesses)*100),
			)
		}
	} else if totalProcesses == 0 {
		m.logger.Warn("No processes moved for user",
			"uid", uid,
			"total_processes_found", totalProcesses,
			"possible_reasons", "no processes found or permission issues",
		)
	} else {
		m.logger.Debug("No process migration required for user",
			"uid", uid,
			"total_processes_found", totalProcesses,
		)
	}

	if len(errors) > 0 {
		m.logger.Warn("Some processes could not be moved",
			"uid", uid,
			"first_error", errors[0],
			"total_errors", len(errors),
		)
	}
}

// CreateSharedCgroup creates a shared cgroup for all limited users.
func (m *Manager) getUIDFromStatusFile(statusFile string) (int, error) {
	file, err := os.Open(statusFile)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				// The first field after "Uid:" is the real UID.
				uid, err := strconv.Atoi(fields[1])
				if err != nil {
					return 0, err
				}
				return uid, nil
			}
		}
	}

	return 0, fmt.Errorf("UID not found in status file")
}

// getProcessInfo returns process identity and diagnostic fields from procfs.
func (m *Manager) getProcessInfo(pid int) (map[string]string, error) {
	info := make(map[string]string)
	processPath := filepath.Join(m.getProcRoot(), strconv.Itoa(pid))

	identity, identityErr := processidentity.Read(m.getProcRoot(), pid)
	info["name"] = strings.TrimSpace(identity.Comm)
	if info["name"] == "" {
		info["name"] = "unknown"
	}
	info["executable"] = identity.Executable

	// Resolve the username from the real UID and cache the lookup. This avoids
	// spawning ps once per process.
	statusFile := filepath.Join(processPath, "status")
	if data, err := os.ReadFile(statusFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(line, "Uid:"):
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					info["username"] = m.usernameForUID(fields[1])
				}
			case strings.HasPrefix(line, "State:"):
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					info["state"] = fields[1]
				}
			}
		}
	}

	return info, identityErr
}

// getProcessName returns the normalized policy identity for a process.
func (m *Manager) getProcessName(pid int) string {
	info, err := m.getProcessInfo(pid)
	if err != nil && len(info) == 0 {
		return fmt.Sprintf("PID-%d", pid)
	}
	return processNameFromInfo(pid, info)
}

func processNameFromInfo(_ int, info map[string]string) string {
	return processpolicy.CanonicalName(info["executable"], info["name"])
}

func processSelectionFromInfo(cfg *config.Config, info map[string]string) processpolicy.Selection {
	return processpolicy.Evaluate(cfg, info["executable"], info["name"])
}

func (m *Manager) usernameForUID(uid string) string {
	ttl := time.Duration(m.getConfig().UsernameCacheTTL) * time.Minute
	now := time.Now()

	m.usernameMu.RLock()
	entry, exists := m.usernameCache[uid]
	m.usernameMu.RUnlock()
	if exists && now.Sub(entry.cachedAt) <= ttl {
		return entry.username
	}

	username := uid
	if m.resolveUsername != nil {
		if resolved, err := m.resolveUsername(uid); err == nil && resolved != "" {
			username = resolved
		}
	} else if resolved, err := user.LookupId(uid); err == nil && resolved.Username != "" {
		username = resolved.Username
	}

	m.usernameMu.Lock()
	if m.usernameCache == nil {
		m.usernameCache = make(map[string]cachedUsername)
	}
	m.usernameCache[uid] = cachedUsername{username: username, cachedAt: now}
	m.usernameMu.Unlock()
	return username
}

// ListProcessesInCgroup returns the processes in a cgroup.
func (m *Manager) ListProcessesInCgroup(uid int) ([]string, error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return nil, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	pids, err := m.readPidsFromFile(procsFile)
	if err != nil {
		return nil, err
	}

	var processes []string
	for _, pid := range pids {
		processName := m.getProcessName(pid)
		processes = append(processes, processName)
	}

	return processes, nil
}

// ApplyRAMLimit applies a RAM limit to a user cgroup.
// limit: bytes (es. "536870912") o suffissi (es. "512M", "1G", "2T")
