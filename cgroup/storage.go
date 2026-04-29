package cgroup

import (
	"bufio"
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
	defer file.Close()

	_, err = file.WriteString(fmt.Sprintf("%d:%s\n", uid, cgroupPath))
	return err
}

// removeCgroupFromFile rimuove un cgroup dal file di tracciamento.
func (m *Manager) removeCgroupFromFile(uid int) error {
	// Leggi tutto il file, filtra e riscrivi
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

	// Risciivi il file
	return os.WriteFile(m.createdCgroupsFile, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

// loadExistingCgroups carica i cgroups esistenti dal file di tracciamento.
func (m *Manager) loadExistingCgroups() error {
	if _, err := os.Stat(m.createdCgroupsFile); os.IsNotExist(err) {
		return nil
	}

	file, err := os.Open(m.createdCgroupsFile)
	if err != nil {
		return err
	}
	defer file.Close()

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
		// Verifica che il cgroup esista ancora
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

// getCgroupPath restituisce il percorso del cgroup per un UID.
func (m *Manager) getCgroupPath(uid int) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	path, exists := m.createdCgroups[uid]
	return path, exists
}

// readPidsFromFile legge i PIDs da un file cgroup.procs.
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

// writePidsBatch scrive una slice di PIDs in un file cgroup.procs in batch.
func (m *Manager) writePidsBatch(filePath string, pids []int) error {
	if len(pids) == 0 {
		return nil
	}

	// Converti i PID in stringhe separate da newline
	var sb strings.Builder
	for i, pid := range pids {
		sb.WriteString(strconv.Itoa(pid))
		if i < len(pids)-1 {
			sb.WriteByte('\n')
		}
	}

	return os.WriteFile(filePath, []byte(sb.String()), 0644)
}

// isValidCPUQuotaFormat valida il formato della quota CPU.
func (m *Manager) GetCreatedCgroups() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	uids := make([]int, 0, len(m.createdCgroups))
	for uid := range m.createdCgroups {
		uids = append(uids, uid)
	}
	return uids
}

// GetCgroupInfo restituisce informazioni su un cgroup specifico.
func (m *Manager) GetCgroupInfo(uid int) (map[string]string, error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return nil, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	info := make(map[string]string)
	info["path"] = cgroupPath

	// Leggi il limite CPU corrente
	cpuMaxFile := filepath.Join(cgroupPath, "cpu.max")
	if data, err := os.ReadFile(cpuMaxFile); err == nil {
		info["cpu.max"] = strings.TrimSpace(string(data))
	}

	// Leggi il peso CPU corrente
	cpuWeightFile := filepath.Join(cgroupPath, "cpu.weight")
	if data, err := os.ReadFile(cpuWeightFile); err == nil {
		info["cpu.weight"] = strings.TrimSpace(string(data))
	}

	return info, nil
}

// GetUserCgroupMetrics legge tutte le metriche cgroup per un utente in una sola chiamata.
// Evita letture multiple di file cgroup separati.
