package systemdunit

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultCgroupRoot = "/sys/fs/cgroup"

type cgroupVerifier struct {
	root     string
	readFile func(string) ([]byte, error)
}

func newCgroupVerifier(root string) cgroupVerifier {
	if root == "" {
		root = defaultCgroupRoot
	}
	return cgroupVerifier{root: filepath.Clean(root), readFile: os.ReadFile}
}

func (v cgroupVerifier) verify(snapshot UnitSnapshot, assignments []PropertyAssignment) error {
	path, err := v.controlGroupPath(snapshot.ControlGroup)
	if err != nil {
		return err
	}
	touched := make(map[PropertyName]bool, len(assignments))
	for _, assignment := range assignments {
		touched[assignment.name] = true
	}
	for _, property := range []PropertyName{PropertyCPUWeight, PropertyMemoryHigh, PropertyMemoryMax, PropertyMemorySwapMax, PropertyIOWeight} {
		if !touched[property] {
			continue
		}
		if err := v.verifyScalarFile(path, snapshot, property); err != nil {
			return err
		}
	}
	if touched[PropertyCPUQuotaPerSecUSec] || touched[PropertyCPUQuotaPeriodUSec] {
		if err := v.verifyCPUQuota(path, snapshot); err != nil {
			return err
		}
	}
	return nil
}

func (v cgroupVerifier) controlGroupPath(controlGroup string) (string, error) {
	if !validControlGroup(controlGroup) {
		return "", fmt.Errorf("invalid authoritative control-group path %q", controlGroup)
	}
	relative := strings.TrimPrefix(filepath.Clean(controlGroup), string(filepath.Separator))
	path := filepath.Join(v.root, relative)
	rel, err := filepath.Rel(v.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("control-group path %q escapes %s", controlGroup, v.root)
	}
	return path, nil
}

func (v cgroupVerifier) verifyScalarFile(path string, snapshot UnitSnapshot, property PropertyName) error {
	filename, defaultWhenUnset := kernelFileForProperty(property)
	expected, ok := snapshot.Properties.Value(property)
	if !ok {
		return fmt.Errorf("systemd readback omitted %s", property)
	}
	data, err := v.readFile(filepath.Join(path, filename))
	if err != nil {
		if expected == SystemdUnset && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read effective %s for %s: %w", filename, snapshot.Identity.Name, err)
	}
	var actual uint64
	if property == PropertyIOWeight {
		actual, err = parseKernelDefaultIOWeight(string(data))
	} else {
		actual, err = parseKernelScalar(string(data))
	}
	if err != nil {
		return fmt.Errorf("parse effective %s for %s: %w", filename, snapshot.Identity.Name, err)
	}
	if expected == SystemdUnset {
		expected = defaultWhenUnset
	}
	if actual != expected {
		return fmt.Errorf("effective %s mismatch for %s: systemd=%d kernel=%d", property, snapshot.Identity.Name, expected, actual)
	}
	return nil
}

func kernelFileForProperty(property PropertyName) (string, uint64) {
	switch property {
	case PropertyCPUWeight:
		return "cpu.weight", 100
	case PropertyMemoryHigh:
		return "memory.high", math.MaxUint64
	case PropertyMemoryMax:
		return "memory.max", math.MaxUint64
	case PropertyMemorySwapMax:
		return "memory.swap.max", math.MaxUint64
	case PropertyIOWeight:
		return "io.weight", 100
	default:
		panic("unapproved scalar kernel property: " + string(property))
	}
}

func (v cgroupVerifier) verifyCPUQuota(path string, snapshot UnitSnapshot) error {
	perSecond, ok := snapshot.Properties.Value(PropertyCPUQuotaPerSecUSec)
	if !ok {
		return fmt.Errorf("systemd readback omitted %s", PropertyCPUQuotaPerSecUSec)
	}
	configuredPeriod, ok := snapshot.Properties.Value(PropertyCPUQuotaPeriodUSec)
	if !ok {
		return fmt.Errorf("systemd readback omitted %s", PropertyCPUQuotaPeriodUSec)
	}
	data, err := v.readFile(filepath.Join(path, "cpu.max"))
	if err != nil {
		if perSecond == SystemdUnset && configuredPeriod == SystemdUnset && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read effective cpu.max for %s: %w", snapshot.Identity.Name, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return fmt.Errorf("parse effective cpu.max for %s: expected two fields", snapshot.Identity.Name)
	}
	actualPeriod, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || actualPeriod == 0 {
		return fmt.Errorf("parse effective cpu.max period for %s", snapshot.Identity.Name)
	}
	if configuredPeriod != SystemdUnset && actualPeriod != configuredPeriod {
		return fmt.Errorf("effective CPU quota period mismatch for %s: systemd=%d kernel=%d", snapshot.Identity.Name, configuredPeriod, actualPeriod)
	}
	if perSecond == SystemdUnset {
		if fields[0] != "max" {
			return fmt.Errorf("effective CPU quota mismatch for %s: systemd=infinity kernel=%q", snapshot.Identity.Name, fields[0])
		}
		return nil
	}
	actualQuota, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return fmt.Errorf("parse effective cpu.max quota for %s: %w", snapshot.Identity.Name, err)
	}
	expectedQuota, err := scalePerSecondQuota(perSecond, actualPeriod)
	if err != nil {
		return fmt.Errorf("derive effective cpu.max quota for %s: %w", snapshot.Identity.Name, err)
	}
	if expectedQuota < 1_000 {
		expectedQuota = 1_000
	}
	if actualQuota != expectedQuota {
		return fmt.Errorf("effective CPU quota mismatch for %s: systemd=%d/%d kernel=%d/%d", snapshot.Identity.Name, perSecond, configuredPeriod, actualQuota, actualPeriod)
	}
	return nil
}

func scalePerSecondQuota(perSecond, period uint64) (uint64, error) {
	high, low := bits.Mul64(perSecond, period)
	if high >= 1_000_000 {
		return 0, fmt.Errorf("scaled CPU quota overflows uint64")
	}
	quotient, _ := bits.Div64(high, low, 1_000_000)
	return quotient, nil
}

func parseKernelScalar(value string) (uint64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "max" {
		return math.MaxUint64, nil
	}
	if strings.ContainsAny(trimmed, " \t\r\n") || trimmed == "" {
		return 0, fmt.Errorf("expected one unsigned value, got %q", value)
	}
	parsed, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", trimmed, err)
	}
	return parsed, nil
}

func parseKernelDefaultIOWeight(value string) (uint64, error) {
	for _, line := range strings.Split(strings.TrimSpace(value), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "default" {
			parsed, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse default I/O weight %q: %w", fields[1], err)
			}
			return parsed, nil
		}
	}
	return 0, fmt.Errorf("default I/O weight is absent")
}
