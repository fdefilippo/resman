package cgroup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (m *Manager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	cgroupPath, err := m.ensureCgroupPath(uid)
	if err != nil {
		return fmt.Errorf("failed to resolve cgroup before applying IO limit: %w", err)
	}

	ioMaxFile := filepath.Join(cgroupPath, "io.max")

	// Normalizza valori bandwidth
	if readBPS == "" || readBPS == "0" {
		readBPS = "max"
	}
	if writeBPS == "" || writeBPS == "0" {
		writeBPS = "max"
	}

	// Normalizza valori IOPS
	readIOPSStr := "max"
	if readIOPS > 0 {
		readIOPSStr = strconv.Itoa(readIOPS)
	}
	writeIOPSStr := "max"
	if writeIOPS > 0 {
		writeIOPSStr = strconv.Itoa(writeIOPS)
	}

	devices, err := m.resolveIODevices(deviceFilter)
	if err != nil {
		return fmt.Errorf("failed to resolve IO devices for UID %d: %w", uid, err)
	}

	limits := fmt.Sprintf("rbps=%s wbps=%s riops=%s wiops=%s",
		readBPS, writeBPS, readIOPSStr, writeIOPSStr)
	if err := writeIOMax(ioMaxFile, devices, limits); err != nil {
		return fmt.Errorf("failed to apply IO limit for UID %d: %w", uid, err)
	}

	m.logger.Debug("IO limit applied",
		"uid", uid,
		"readBPS", readBPS,
		"writeBPS", writeBPS,
		"readIOPS", readIOPSStr,
		"writeIOPS", writeIOPSStr,
		"devices", strings.Join(devices, ","),
		"path", ioMaxFile,
	)

	return nil
}

// RemoveIOLimit rimuove i limiti di IO (imposta tutti i valori a "max").
func (m *Manager) RemoveIOLimit(uid int) error {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return fmt.Errorf("cgroup for UID %d not found", uid)
	}

	ioMaxFile := filepath.Join(cgroupPath, "io.max")
	data, err := os.ReadFile(ioMaxFile)
	if err != nil {
		return fmt.Errorf("failed to read IO limits for UID %d: %w", uid, err)
	}

	devices := ioMaxDevices(data)
	if len(devices) == 0 {
		return nil
	}
	if err := writeIOMax(ioMaxFile, devices, "rbps=max wbps=max riops=max wiops=max"); err != nil {
		return fmt.Errorf("failed to remove IO limits for UID %d: %w", uid, err)
	}
	return nil
}

func (m *Manager) resolveIODevices(deviceFilter string) ([]string, error) {
	deviceFilter = strings.TrimSpace(deviceFilter)
	if deviceFilter != "" && deviceFilter != "all" {
		return []string{deviceFilter}, nil
	}

	sysBlockRoot := m.sysBlockRoot
	if sysBlockRoot == "" {
		sysBlockRoot = "/sys/block"
	}
	entries, err := os.ReadDir(sysBlockRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to enumerate block devices in %s: %w", sysBlockRoot, err)
	}

	devices := make([]string, 0, len(entries))
	for _, entry := range entries {
		devicePath := filepath.Join(sysBlockRoot, entry.Name(), "dev")
		data, err := os.ReadFile(devicePath)
		if err != nil {
			m.logger.Debug("Skipping block device during IO enumeration",
				"path", devicePath,
				"error", err,
			)
			continue
		}
		device := strings.TrimSpace(string(data))
		if device == "" {
			m.logger.Debug("Skipping block device with empty device number",
				"path", devicePath,
			)
			continue
		}
		devices = append(devices, device)
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("no block devices found in %s", sysBlockRoot)
	}
	return devices, nil
}

func writeIOMax(path string, devices []string, limits string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, defaultFilePerm)
	if err != nil {
		return err
	}

	for _, device := range devices {
		if _, err := io.WriteString(file, device+" "+limits+"\n"); err != nil {
			_ = file.Close()
			return fmt.Errorf("failed to write limits for device %s: %w", device, err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", path, err)
	}
	return nil
}

func ioMaxDevices(data []byte) []string {
	lines := strings.Split(string(data), "\n")
	devices := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if _, exists := seen[fields[0]]; exists {
			continue
		}
		seen[fields[0]] = struct{}{}
		devices = append(devices, fields[0])
	}
	return devices
}

// GetIOStats restituisce le statistiche di IO aggregate per tutti i dispositivi.
// Legge da io.stat e somma rbytes, wbytes, rios, wios.
func (m *Manager) GetIOStats(uid int) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return 0, 0, 0, 0, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	ioStatFile := filepath.Join(cgroupPath, "io.stat")
	data, err := os.ReadFile(ioStatFile)
	if err != nil {
		// Se il file non esiste (nessun IO), restituisci zero
		if os.IsNotExist(err) {
			return 0, 0, 0, 0, nil
		}
		return 0, 0, 0, 0, fmt.Errorf("failed to read io.stat for UID %d: %w", uid, err)
	}

	// Parse lines like: "8:0 rios=1234 wios=567 rbytes=104857600 wbytes=52428800"
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip device prefix (e.g., "8:0"), parse key=value pairs
		parts := strings.Fields(line)
		for _, part := range parts {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			val, parseErr := strconv.ParseUint(kv[1], 10, 64)
			if parseErr != nil {
				continue
			}
			switch kv[0] {
			case "rios":
				readOps += val
			case "wios":
				writeOps += val
			case "rbytes":
				readBytes += val
			case "wbytes":
				writeBytes += val
			}
		}
	}

	return readBytes, writeBytes, readOps, writeOps, nil
}

// ApplyTemporaryIOLimit applica limiti IO temporanei con un moltiplicatore.
// Salva i limiti originali per permettere il revert.
func (m *Manager) ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error {
	if _, exists := m.getCgroupPath(uid); !exists {
		return fmt.Errorf("cgroup for UID %d not found", uid)
	}

	// Applica limiti boostati (moltiplicati)
	boostedReadBPS := applyMultiplierToBPS(readBPS, multiplier)
	boostedWriteBPS := applyMultiplierToBPS(writeBPS, multiplier)
	boostedReadIOPS := int(float64(readIOPS) * multiplier)
	boostedWriteIOPS := int(float64(writeIOPS) * multiplier)

	return m.ApplyIOLimit(uid, boostedReadBPS, boostedWriteBPS, boostedReadIOPS, boostedWriteIOPS, deviceFilter)
}

// applyMultiplierToBPS applica un moltiplicatore a una stringa BPS.
func applyMultiplierToBPS(bps string, multiplier float64) string {
	if bps == "" || bps == "max" || bps == "0" {
		return "max"
	}
	// Parse byte value (supports K, M, G, T suffixes)
	val := parseBPSValue(bps)
	if val == 0 {
		return "max"
	}
	boosted := uint64(float64(val) * multiplier)
	return strconv.FormatUint(boosted, 10)
}

// parseBPSValue converte una stringa BPS in bytes.
func parseBPSValue(s string) uint64 {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return 0
	}

	// Check for suffix
	lastChar := strings.ToUpper(s[len(s)-1:])
	multiplier := uint64(1)
	numStr := s

	switch lastChar {
	case "K":
		multiplier = 1024
		numStr = s[:len(s)-1]
	case "M":
		multiplier = 1024 * 1024
		numStr = s[:len(s)-1]
	case "G":
		multiplier = 1024 * 1024 * 1024
		numStr = s[:len(s)-1]
	case "T":
		multiplier = 1024 * 1024 * 1024 * 1024
		numStr = s[:len(s)-1]
	}

	val, err := strconv.ParseUint(numStr, 10, 64)
	if err != nil {
		return 0
	}
	return val * multiplier
}
