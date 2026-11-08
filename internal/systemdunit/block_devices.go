package systemdunit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ResolveBlockDevices converts the public major:minor filter into canonical
// block-device node paths accepted by systemd's per-device properties.
func ResolveBlockDevices(filter string) ([]string, error) {
	return resolveBlockDevices(filter, "/sys/block", "/sys/dev/block", "/dev", os.ReadDir, os.ReadFile, os.Stat)
}

func resolveBlockDevices(
	filter, sysBlockRoot, sysDevBlockRoot, devRoot string,
	readDir func(string) ([]os.DirEntry, error),
	readFile func(string) ([]byte, error),
	stat func(string) (os.FileInfo, error),
) ([]string, error) {
	filter = strings.TrimSpace(filter)
	var numbers []string
	if filter != "" && filter != "all" {
		numbers = []string{filter}
	} else {
		entries, err := readDir(sysBlockRoot)
		if err != nil {
			return nil, fmt.Errorf("enumerate block devices in %s: %w", sysBlockRoot, err)
		}
		for _, entry := range entries {
			data, err := readFile(filepath.Join(sysBlockRoot, entry.Name(), "dev"))
			if err != nil {
				continue
			}
			number := strings.TrimSpace(string(data))
			if number != "" {
				numbers = append(numbers, number)
			}
		}
	}
	sort.Strings(numbers)
	var result []string
	seen := make(map[string]bool, len(numbers))
	for _, number := range numbers {
		data, err := readFile(filepath.Join(sysDevBlockRoot, number, "uevent"))
		if err != nil {
			return nil, fmt.Errorf("resolve block device %s: %w", number, err)
		}
		name := ueventValue(string(data), "DEVNAME")
		if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "..") {
			return nil, fmt.Errorf("resolve block device %s: invalid DEVNAME %q", number, name)
		}
		path := filepath.Join(devRoot, name)
		info, err := stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect block device %s: %w", path, err)
		}
		if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			return nil, fmt.Errorf("path %s is not a block device", path)
		}
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no block devices matched %q", filter)
	}
	sort.Strings(result)
	return result, nil
}

func ueventValue(data, key string) string {
	for _, line := range strings.Split(data, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

// resolveHardIODevice follows systemd's hard-I/O lookup: one originating device
// layer (only when its slaves share a whole disk), then partition to whole disk.
// See systemd v239/v252 lookup_block_device and block_get_originating. Weighted
// I/O uses its separate explicitly characterized per-device contract.
func resolveHardIODevice(number, sysRoot string) (string, error) {
	whole := func(number string) (string, error) {
		path := filepath.Join(sysRoot, number)
		if _, err := os.Stat(filepath.Join(path, "partition")); err != nil {
			if os.IsNotExist(err) {
				return number, nil
			}
			return "", err
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(real), "dev"))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	slaves, err := os.ReadDir(filepath.Join(sysRoot, number, "slaves"))
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve hard-I/O device %s slaves: %w", number, err)
	}
	origin := ""
	ambiguous := false
	for _, slave := range slaves {
		data, err := os.ReadFile(filepath.Join(sysRoot, number, "slaves", slave.Name(), "dev"))
		if err != nil {
			return "", err
		}
		candidate, err := whole(strings.TrimSpace(string(data)))
		if err != nil {
			return "", err
		}
		if origin != "" && origin != candidate {
			ambiguous = true
		}
		origin = candidate
	}
	if origin != "" && !ambiguous {
		number = origin
	}
	result, err := whole(number)
	if err != nil {
		return "", fmt.Errorf("resolve hard-I/O whole device %s: %w", number, err)
	}
	return result, nil
}
