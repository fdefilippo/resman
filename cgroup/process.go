package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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

func (m *Manager) MoveProcessToCgroup(pid int, uid int) error {
	_, err := m.moveProcessToCgroup(pid, uid, nil)
	return err
}

func (m *Manager) moveProcessToCgroup(pid int, uid int, processInfo map[string]string) (bool, error) {
	// SECURITY: Never move any process to UID 0 cgroup
	if uid == 0 {
		m.logger.Warn("Refusing to move process to root (UID 0) cgroup - security boundary",
			"pid", pid)
		return false, fmt.Errorf("processes cannot be moved to UID 0 (root) cgroups")
	}

	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return false, fmt.Errorf("cgroup for UID %d does not exist", uid)
	}

	cgroupProcsFile := filepath.Join(cgroupPath, "cgroup.procs")
	movable, err := m.captureProcessOrigin(pid, uid, cgroupPath)
	if err != nil {
		return false, fmt.Errorf("failed to persist cgroup origin for PID %d: %w", pid, err)
	}
	if !movable {
		return false, nil
	}

	if processInfo == nil {
		processInfo, err = m.getProcessInfo(pid)
		if err != nil {
			m.logger.Warn("Failed to get process info", "pid", pid, "error", err)
		}
	}
	processName := processNameFromInfo(pid, processInfo)

	// Scrivi il PID nel file cgroup.procs
	if err := m.writePIDToCgroup(cgroupProcsFile, pid); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			_ = m.removeProcessOrigins(map[int]bool{pid: true})
			return false, nil
		}
		return false, fmt.Errorf("failed to move PID %d to cgroup for UID %d: %w", pid, uid, err)
	}

	// Log dettagliato
	m.logger.Debug("Process moved to cgroup",
		"pid", pid,
		"uid", uid,
		"process_name", processName,
		"process_state", processInfo["state"],
		"username", processInfo["username"],
		"cgroup_path", cgroupPath,
	)

	return true, nil
}

// MoveAllUserProcesses sposta tutti i processi di un utente nel suo cgroup.
// Uses gopsutil for efficient process discovery.
func (m *Manager) MoveAllUserProcesses(uid int) error {
	m.logger.Debug("Moving all processes for user to cgroup", "uid", uid)

	// SECURITY: Never move UID 0 (root) processes to user cgroups
	if uid == 0 {
		m.logger.Warn("Refusing to move root (UID 0) processes to cgroup - security boundary")
		return fmt.Errorf("UID 0 (root) processes cannot be moved to user cgroups")
	}

	pids, err := m.processIDsForUID(uid)
	if err != nil {
		return err
	}

	var movedCount, totalProcesses int
	var processNames, errors []string

	for _, pid := range pids {
		totalProcesses++
		processInfo, infoErr := m.getProcessInfo(pid)
		if infoErr != nil {
			m.logger.Debug("Failed to read process details before migration",
				"pid", pid,
				"error", infoErr,
			)
		}
		processName := processNameFromInfo(pid, processInfo)

		// Salta processi esclusi
		if m.getConfig().IsProcessExcluded(processName) {
			continue
		}

		// Sposta il processo
		moved, err := m.moveProcessToCgroup(pid, uid, processInfo)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", processName, err))
		} else if moved {
			movedCount++
			processNames = append(processNames, processName)
		}
	}

	m.logProcessMoveSummary(uid, movedCount, totalProcesses, processNames, errors)

	if len(errors) > 0 {
		return fmt.Errorf("some processes could not be moved: %d errors", len(errors))
	}
	return nil
}

func (m *Manager) processIDsForUID(uid int) ([]int, error) {
	m.processScanMu.Lock()
	defer m.processScanMu.Unlock()

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

// CreateSharedCgroup crea un cgroup condiviso per tutti gli utenti limitati
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
				// Il primo campo dopo "Uid:" è l'UID reale
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

// CleanupUserCgroup rimuove il cgroup di un utente (dopo aver spostato i processi fuori).
func (m *Manager) getProcessInfo(pid int) (map[string]string, error) {
	info := make(map[string]string)
	processPath := filepath.Join(m.getProcRoot(), strconv.Itoa(pid))

	// Nome del processo da /proc/[pid]/comm
	commFile := filepath.Join(processPath, "comm")
	if data, err := os.ReadFile(commFile); err == nil {
		info["name"] = strings.TrimSpace(string(data))
	} else {
		info["name"] = "unknown"
	}

	// Command line da /proc/[pid]/cmdline
	cmdlineFile := filepath.Join(processPath, "cmdline")
	if data, err := os.ReadFile(cmdlineFile); err == nil {
		cmdline := strings.ReplaceAll(string(data), "\x00", " ")
		cmdline = strings.TrimSpace(cmdline)
		if cmdline != "" {
			info["cmdline"] = cmdline
		}
	}

	// Username da /proc/[pid]/status (campo Uid:) + cache lookup
	// Evita exec.Command("ps") che è costoso (fork+exec per ogni processo)
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

	return info, nil
}

// getProcessName cerca di ottenere il nome migliore per un processo
func (m *Manager) getProcessName(pid int) string {
	info, err := m.getProcessInfo(pid)
	if err != nil {
		return fmt.Sprintf("PID-%d", pid)
	}
	return processNameFromInfo(pid, info)
}

func processNameFromInfo(pid int, info map[string]string) string {
	// Preferisci cmdline se disponibile e non troppo lungo
	if cmdline, ok := info["cmdline"]; ok && cmdline != "" && len(cmdline) < 100 {
		// Prendi solo il primo comando (prima dello spazio)
		parts := strings.Fields(cmdline)
		if len(parts) > 0 {
			// Estrai solo il nome del comando (senza path)
			base := filepath.Base(parts[0])
			return fmt.Sprintf("%s[%d]", base, pid)
		}
	}

	// Altrimenti usa il nome dal comm
	if name, ok := info["name"]; ok && name != "" {
		return fmt.Sprintf("%s[%d]", name, pid)
	}

	return fmt.Sprintf("PID-%d", pid)
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

// ListProcessesInCgroup restituisce l'elenco dei processi in un cgroup
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

// ApplyRAMLimit applica un limite di RAM a un cgroup utente.
// limit: bytes (es. "536870912") o suffissi (es. "512M", "1G", "2T")
