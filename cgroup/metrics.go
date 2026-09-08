package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// CgroupIdentity identifies one cgroup directory lifetime.
type CgroupIdentity struct {
	Device uint64
	Inode  uint64
}

// CPUStatCounters contains cumulative cgroup v2 cpu.stat counters.
type CPUStatCounters struct {
	UsageUsec     uint64
	NrPeriods     uint64
	NrThrottled   uint64
	ThrottledUsec uint64
}

// CPUPointsNodeSnapshot contains one verified CPU Points node observation.
type CPUPointsNodeSnapshot struct {
	Identity  CgroupIdentity
	CPUStat   CPUStatCounters
	CPUQuota  string
	CPUWeight uint64
}

// MemoryEventCounters contains cumulative cgroup v2 memory.events counters.
type MemoryEventCounters struct {
	High    uint64
	Max     uint64
	OOM     uint64
	OOMKill uint64
}

// MemoryAccountingSnapshot contains one managed leaf's RAM accounting state.
type MemoryAccountingSnapshot struct {
	Path         string
	Identity     CgroupIdentity
	CurrentBytes uint64
	HighLimit    string
	MaxLimit     string
	SwapMax      string
	Events       MemoryEventCounters
}

// ReadCPUAccountingSnapshot reads counters without creating or mutating a cgroup.
func ReadCPUAccountingSnapshot(path string) (CPUPointsNodeSnapshot, error) {
	identity, err := readCgroupIdentity(path)
	if err != nil {
		return CPUPointsNodeSnapshot{}, fmt.Errorf("inspect CPU Points cgroup %s: %w", path, err)
	}
	values, err := readCgroupCounterFile(filepath.Join(path, "cpu.stat"))
	if err != nil {
		return CPUPointsNodeSnapshot{}, err
	}
	stat := CPUStatCounters{}
	for key, target := range map[string]*uint64{
		"usage_usec":     &stat.UsageUsec,
		"nr_periods":     &stat.NrPeriods,
		"nr_throttled":   &stat.NrThrottled,
		"throttled_usec": &stat.ThrottledUsec,
	} {
		value, ok := values[key]
		if !ok {
			return CPUPointsNodeSnapshot{}, fmt.Errorf("CPU Points cgroup %s cpu.stat is missing %s", path, key)
		}
		*target = value
	}
	quota, err := readCgroupValue(filepath.Join(path, "cpu.max"))
	if err != nil {
		return CPUPointsNodeSnapshot{}, err
	}
	weightText, err := readCgroupValue(filepath.Join(path, "cpu.weight"))
	if err != nil {
		return CPUPointsNodeSnapshot{}, err
	}
	weight, err := strconv.ParseUint(weightText, 10, 64)
	if err != nil {
		return CPUPointsNodeSnapshot{}, fmt.Errorf("parse CPU Points cgroup %s cpu.weight %q: %w", path, weightText, err)
	}
	if err := confirmCgroupIdentity(path, identity); err != nil {
		return CPUPointsNodeSnapshot{}, err
	}
	return CPUPointsNodeSnapshot{Identity: identity, CPUStat: stat, CPUQuota: quota, CPUWeight: weight}, nil
}

// ReadMemoryAccountingSnapshot reads identity-stable memory accounting at an authorized path.
func ReadMemoryAccountingSnapshot(path string) (MemoryAccountingSnapshot, error) {
	identity, err := readCgroupIdentity(path)
	if err != nil {
		return MemoryAccountingSnapshot{}, fmt.Errorf("inspect RAM cgroup %s: %w", path, err)
	}
	currentText, err := readCgroupValue(filepath.Join(path, "memory.current"))
	if err != nil {
		return MemoryAccountingSnapshot{}, err
	}
	current, err := strconv.ParseUint(currentText, 10, 64)
	if err != nil {
		return MemoryAccountingSnapshot{}, fmt.Errorf("parse memory.current for %s: %w", path, err)
	}
	high, err := readCgroupValue(filepath.Join(path, "memory.high"))
	if err != nil {
		return MemoryAccountingSnapshot{}, err
	}
	max, err := readCgroupValue(filepath.Join(path, "memory.max"))
	if err != nil {
		return MemoryAccountingSnapshot{}, err
	}
	swapMax, err := readCgroupValue(filepath.Join(path, "memory.swap.max"))
	if err != nil {
		return MemoryAccountingSnapshot{}, err
	}
	values, err := readCgroupCounterFile(filepath.Join(path, "memory.events"))
	if err != nil {
		return MemoryAccountingSnapshot{}, err
	}
	events := MemoryEventCounters{}
	for key, target := range map[string]*uint64{
		"high":     &events.High,
		"max":      &events.Max,
		"oom":      &events.OOM,
		"oom_kill": &events.OOMKill,
	} {
		value, ok := values[key]
		if !ok {
			return MemoryAccountingSnapshot{}, fmt.Errorf("RAM cgroup %s memory.events is missing %s", path, key)
		}
		*target = value
	}
	if err := confirmCgroupIdentity(path, identity); err != nil {
		return MemoryAccountingSnapshot{}, err
	}
	return MemoryAccountingSnapshot{
		Path: path, Identity: identity, CurrentBytes: current, HighLimit: high, MaxLimit: max, SwapMax: swapMax, Events: events,
	}, nil
}

func confirmCgroupIdentity(path string, opened CgroupIdentity) error {
	current, err := readCgroupIdentity(path)
	if err != nil {
		return fmt.Errorf("reinspect cgroup %s after reading interfaces: %w", path, err)
	}
	if current != opened {
		return fmt.Errorf("cgroup %s changed identity while its interfaces were being read", path)
	}
	return nil
}

func readCgroupIdentity(path string) (CgroupIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return CgroupIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return CgroupIdentity{}, fmt.Errorf("unsupported cgroup metadata type %T", info.Sys())
	}
	return CgroupIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func readCgroupValue(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read cgroup interface %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func readCgroupCounterFile(path string) (map[string]uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cgroup counter file %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, fmt.Errorf("cgroup counter file %s contains malformed line %q", path, scanner.Text())
		}
		if _, duplicate := values[fields[0]]; duplicate {
			return nil, fmt.Errorf("cgroup counter file %s repeats field %s", path, fields[0])
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse cgroup counter %s in %s: %w", fields[0], path, err)
		}
		values[fields[0]] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read cgroup counter file %s: %w", path, err)
	}
	return values, nil
}
