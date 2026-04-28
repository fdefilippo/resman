package cgroup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (m *Manager) CreateUserCgroup(uid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Verifica se esiste già
	if _, exists := m.createdCgroups[uid]; exists {
		m.logger.Debug("Cgroup already exists for user", "uid", uid)
		return nil
	}

	cgroupPath := m.getUserCgroupPath(uid)

	// Crea la directory del cgroup
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return fmt.Errorf("failed to create cgroup directory %s for UID %d: %w", cgroupPath, uid, err)
	}

	// Traccia il cgroup creato
	m.createdCgroups[uid] = cgroupPath

	// Salva nel file di tracciamento
	if err := m.saveCgroupToFile(uid, cgroupPath); err != nil {
		m.logger.Warn("Failed to save cgroup to tracking file",
			"uid", uid,
			"error", err,
		)
		// Non falliamo per questo errore
	}

	m.logger.Debug("Cgroup created for user", "uid", uid, "path", cgroupPath)
	return nil
}

// ApplyCPULimit applica un limite di CPU a un cgroup utente.
func (m *Manager) ApplyCPULimit(uid int, quota string) error {
	// Assicurati che il cgroup esista
	cgroupPath := m.getUserCgroupPath(uid)

	// Verifica che la directory esista
	if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
		// Crea il cgroup se non esiste
		if err := m.CreateUserCgroup(uid); err != nil {
			return fmt.Errorf("failed to create cgroup %s before applying limit for UID %d: %w", cgroupPath, uid, err)
		}
	}

	cpuMaxFile := filepath.Join(cgroupPath, "cpu.max")

	// Valida il formato della quota
	if !isValidCPUQuotaFormat(quota) {
		return fmt.Errorf("invalid CPU quota format '%s': expected 'quota period' (e.g., '50000 100000') or 'max period'", quota)
	}

	// Applica il limite
	if err := os.WriteFile(cpuMaxFile, []byte(quota), 0644); err != nil {
		// Prova con permessi diversi
		if os.IsPermission(err) {
			if chmodErr := os.Chmod(cpuMaxFile, 0644); chmodErr != nil {
				m.logger.Warn("Failed to chmod CPU max file",
					"path", cpuMaxFile,
					"error", chmodErr,
				)
			}
			time.Sleep(100 * time.Millisecond)
			err = os.WriteFile(cpuMaxFile, []byte(quota), 0644)
		}
		if err != nil {
			return fmt.Errorf("failed to apply CPU limit %s to %s for UID %d: %w", quota, cpuMaxFile, uid, err)
		}
	}

	// Verifica che il limite sia stato applicato
	time.Sleep(50 * time.Millisecond)
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

	// Sposta processi in modo sincrono con timeout configurabile
	timeout := time.Duration(m.cfg.GetCgroupOperationTimeout()) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer close(done)
		delay := time.Duration(m.cfg.GetCgroupRetryDelayMs()) * time.Millisecond
		time.Sleep(delay) // Breve delay per stabilizzazione
		done <- m.MoveAllUserProcesses(uid)
	}()

	select {
	case err := <-done:
		if err != nil {
			m.logger.Warn("Failed to move user processes to cgroup",
				"uid", uid,
				"error", err,
			)
			return err
		}
	case <-ctx.Done():
		m.logger.Warn("Timeout moving user processes to cgroup",
			"uid", uid,
			"timeout", timeout,
		)
		return fmt.Errorf("timeout (%v) moving processes to cgroup for UID %d", timeout, uid)
	}

	return nil
}

// ApplyCPUWeight applica un peso CPU (proporzionale) a un cgroup utente.
func (m *Manager) ApplyCPUWeight(uid int, weight int) error {
	cgroupPath := m.getUserCgroupPath(uid)

	// Verifica che la directory esista
	if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
		// Crea il cgroup se non esiste
		if err := m.CreateUserCgroup(uid); err != nil {
			return fmt.Errorf("failed to create cgroup before applying weight: %w", err)
		}
	}

	cpuWeightFile := filepath.Join(cgroupPath, "cpu.weight")

	// Il peso deve essere tra 1 e 10000
	if weight < 1 {
		weight = 1
	}
	if weight > 10000 {
		weight = 10000
	}

	// Applica il peso
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

// RemoveCPULimit rimuove il limite di CPU (imposta a "max").
func (m *Manager) RemoveCPULimit(uid int) error {
	return m.ApplyCPULimit(uid, "max 100000")
}

// MoveProcessToCgroup sposta un processo nel cgroup dell'utente.
func isValidCPUQuotaFormat(quota string) bool {
	parts := strings.Fields(quota)
	if len(parts) != 2 {
		return false
	}

	// La prima parte può essere "max" o un numero
	if parts[0] == "max" {
		_, err := strconv.Atoi(parts[1])
		return err == nil
	}

	// Altrimenti entrambe devono essere numeri
	_, err1 := strconv.Atoi(parts[0])
	_, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil
}

// GetCreatedCgroups restituisce una lista di UID con cgroups attivi.
