package cgroup

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fdefilippo/resman/config"
)

var errCounterOverflow = errors.New("uint64 counter overflow")

func checkedCounterAdd(current, value uint64) (uint64, error) {
	if current > math.MaxUint64-value {
		return 0, errCounterOverflow
	}
	return current + value, nil
}

func (m *Manager) ApplyIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string) error {
	cgroupPath, err := m.ensureCgroupPath(uid)
	if err != nil {
		return fmt.Errorf("failed to resolve cgroup before applying IO limit: %w", err)
	}

	ioMaxFile := filepath.Join(cgroupPath, "io.max")

	readBPS, err = normalizeBPSLimit(readBPS)
	if err != nil {
		return fmt.Errorf("invalid read BPS limit for UID %d: %w", uid, err)
	}
	writeBPS, err = normalizeBPSLimit(writeBPS)
	if err != nil {
		return fmt.Errorf("invalid write BPS limit for UID %d: %w", uid, err)
	}

	// Normalize IOPS values.
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

// normalizeBPSLimit converts the configured byte-quota syntax into the decimal
// byte count required by the cgroup v2 io.max interface.
func normalizeBPSLimit(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" || value == "max" {
		return "max", nil
	}

	bytes, err := config.ParseByteQuota(value)
	if err != nil {
		return "", err
	}
	if bytes == 0 {
		return "max", nil
	}
	return strconv.FormatUint(bytes, 10), nil
}

// RemoveIOLimit removes I/O limits by setting every value to "max".
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

// GetIOStats returns logical I/O counters aggregated across every block device.
// The logical counters preserve continuity when a user changes managed cgroups.
func (m *Manager) GetIOStats(uid int) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error) {
	counters, err := m.logicalBlockIOCounters(uid)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("read logical block I/O counters for UID %d: %w", uid, err)
	}
	return counters.readBytes, counters.writeBytes, counters.readOps, counters.writeOps, nil
}

func readIOStatsFile(ioStatFile string) (readBytes, writeBytes uint64, readOps, writeOps uint64, err error) {
	data, err := os.ReadFile(ioStatFile)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	// Parse lines like: "8:0 rios=1234 wios=567 rbytes=104857600 wbytes=52428800".
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip the device prefix (for example, "8:0") and parse key=value pairs.
		parts := strings.Fields(line)
		device := parts[0]
		for _, part := range parts {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			val, parseErr := strconv.ParseUint(kv[1], 10, 64)
			if parseErr != nil {
				continue
			}
			var counter *uint64
			switch kv[0] {
			case "rios":
				counter = &readOps
			case "wios":
				counter = &writeOps
			case "rbytes":
				counter = &readBytes
			case "wbytes":
				counter = &writeBytes
			default:
				continue
			}
			next, addErr := checkedCounterAdd(*counter, val)
			if addErr != nil {
				return 0, 0, 0, 0, fmt.Errorf(
					"accumulate %s for device %s from %s: %w",
					kv[0], device, ioStatFile, addErr,
				)
			}
			*counter = next
		}
	}

	return readBytes, writeBytes, readOps, writeOps, nil
}

// ApplyTemporaryIOLimit applies temporary I/O limits with a multiplier.
// It preserves the original inputs so the caller can restore them later.
func (m *Manager) ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error {
	if _, exists := m.getCgroupPath(uid); !exists {
		return fmt.Errorf("cgroup for UID %d not found", uid)
	}

	// Apply the multiplied boost limits.
	boostedReadBPS := applyMultiplierToBPS(readBPS, multiplier)
	boostedWriteBPS := applyMultiplierToBPS(writeBPS, multiplier)
	boostedReadIOPS := int(float64(readIOPS) * multiplier)
	boostedWriteIOPS := int(float64(writeIOPS) * multiplier)

	return m.ApplyIOLimit(uid, boostedReadBPS, boostedWriteBPS, boostedReadIOPS, boostedWriteIOPS, deviceFilter)
}

// applyMultiplierToBPS applies a multiplier to a BPS value.
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

// parseBPSValue converts a BPS value into bytes.
func parseBPSValue(s string) uint64 {
	val, err := config.ParseByteQuota(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return val
}
