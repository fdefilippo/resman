package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v3/process"
)

func (m *Manager) MoveProcessToCgroup(pid int, uid int) error {
	// SECURITY: Never move any process to UID 0 cgroup
	if uid == 0 {
		m.logger.Warn("Refusing to move process to root (UID 0) cgroup - security boundary",
			"pid", pid)
		return fmt.Errorf("processes cannot be moved to UID 0 (root) cgroups")
	}

	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return fmt.Errorf("cgroup for UID %d does not exist", uid)
	}

	cgroupProcsFile := filepath.Join(cgroupPath, "cgroup.procs")

	// Ottieni info sul processo PRIMA di spostarlo
	processName := m.getProcessName(pid)
	processInfo, err := m.getProcessInfo(pid)
	if err != nil {
		m.logger.Warn("Failed to get process info", "pid", pid, "error", err)
	}

	// Scrivi il PID nel file cgroup.procs
	pidStr := strconv.Itoa(pid)
	if err := os.WriteFile(cgroupProcsFile, []byte(pidStr), 0644); err != nil {
		return fmt.Errorf("failed to move PID %d to cgroup for UID %d: %w", pid, uid, err)
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

	return nil
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

	// Try gopsutil first
	procs, err := process.Processes()
	if err != nil {
		m.logger.Debug("gopsutil failed, falling back to /proc scan", "error", err)
		return m.moveAllUserProcessesFallback(uid)
	}

	var movedCount, totalProcesses int
	var processNames, errors []string

	for _, p := range procs {
		uids, err := p.Uids()
		if err != nil || len(uids) == 0 || int(uids[0]) != uid {
			continue
		}

		totalProcesses++
		pid := int(p.Pid)
		processName := m.getProcessName(pid)

		// Salta processi esclusi
		if m.cfg.IsProcessExcluded(processName) {
			continue
		}

		// Sposta il processo
		if err := m.MoveProcessToCgroup(pid, uid); err != nil {
			errors = append(errors, fmt.Sprintf("%s[%d]: %v", processName, pid, err))
		} else {
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

// moveAllUserProcessesFallback scans /proc manually if gopsutil fails.
func (m *Manager) moveAllUserProcessesFallback(uid int) error {
	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return fmt.Errorf("failed to read /proc: %w", err)
	}

	var movedCount, totalProcesses int
	var processNames, errors []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		statusFile := filepath.Join(procDir, entry.Name(), "status")
		if procUID, err := m.getUIDFromStatusFile(statusFile); err == nil && procUID == uid {
			totalProcesses++
			processName := m.getProcessName(pid)

			if m.cfg.IsProcessExcluded(processName) {
				continue
			}

			if err := m.MoveProcessToCgroup(pid, uid); err != nil {
				errors = append(errors, fmt.Sprintf("%s[%d]: %v", processName, pid, err))
			} else {
				movedCount++
				processNames = append(processNames, processName)
			}
		}
	}

	m.logProcessMoveSummary(uid, movedCount, totalProcesses, processNames, errors)

	if len(errors) > 0 {
		return fmt.Errorf("some processes could not be moved: %d errors", len(errors))
	}
	return nil
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
	} else {
		m.logger.Warn("No processes moved for user",
			"uid", uid,
			"total_processes_found", totalProcesses,
			"possible_reasons", "no processes found or permission issues",
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
	defer file.Close()

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

	// Nome del processo da /proc/[pid]/comm
	commFile := fmt.Sprintf("/proc/%d/comm", pid)
	if data, err := os.ReadFile(commFile); err == nil {
		info["name"] = strings.TrimSpace(string(data))
	} else {
		info["name"] = "unknown"
	}

	// Command line da /proc/[pid]/cmdline
	cmdlineFile := fmt.Sprintf("/proc/%d/cmdline", pid)
	if data, err := os.ReadFile(cmdlineFile); err == nil {
		cmdline := strings.ReplaceAll(string(data), "\x00", " ")
		cmdline = strings.TrimSpace(cmdline)
		if cmdline != "" {
			info["cmdline"] = cmdline
		}
	}

	// Username da /proc/[pid]/status (campo Uid:) + cache lookup
	// Evita exec.Command("ps") che è costoso (fork+exec per ogni processo)
	statusFile := fmt.Sprintf("/proc/%d/status", pid)
	if data, err := os.ReadFile(statusFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					uidStr := fields[1]
					// Usa os/user.LookupId per supportare LDAP/NIS con CGO
					if u, lookupErr := user.LookupId(uidStr); lookupErr == nil {
						info["username"] = u.Username
					} else {
						info["username"] = uidStr
					}
				}
				break
			}
		}
	}

	// CPU usage corrente
	statFile := fmt.Sprintf("/proc/%d/stat", pid)
	if data, err := os.ReadFile(statFile); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 13 {
			info["state"] = fields[2] // Stato del processo (R, S, D, Z, etc.)
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
		processes = append(processes, fmt.Sprintf("%s[%d]", processName, pid))
	}

	return processes, nil
}

// ApplyRAMLimit applica un limite di RAM a un cgroup utente.
// limit: bytes (es. "536870912") o suffissi (es. "512M", "1G", "2T")
