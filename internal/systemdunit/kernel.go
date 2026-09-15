package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/fdefilippo/resman/cgroup"
	"golang.org/x/sys/unix"
)

// UnitAccounting is an observation of one authoritative unit lifetime. Missing
// resource observations remain absent; they are never substituted with zero.
type UnitAccounting struct {
	Identity    UnitIdentity
	CPU         *cgroup.CPUPointsNodeSnapshot
	Memory      *cgroup.MemoryAccountingSnapshot
	CPUError    error
	MemoryError error
}

// ObserveAccounting reads CPU and memory independently, then reconfirms the
// systemd unit identity. The kernel readers also reconfirm directory identity.
func (a *Adapter) ObserveAccounting(ctx context.Context, identity UnitIdentity) (UnitAccounting, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("observe_accounting"); err != nil {
		return UnitAccounting{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	snapshot, err := a.readUnit(ctx, identity.Name, identity.ObjectPath)
	if err != nil {
		return UnitAccounting{}, err
	}
	if err := requireSameIdentity("observe_accounting", identity, snapshot.Identity); err != nil {
		return UnitAccounting{}, err
	}
	reader, ok := a.verifier.(interface {
		observe(UnitSnapshot) (UnitAccounting, error)
	})
	if !ok {
		return UnitAccounting{}, fmt.Errorf("kernel accounting reader is unavailable")
	}
	result, err := reader.observe(snapshot)
	if err != nil {
		return UnitAccounting{}, err
	}
	confirmed, err := a.readUnit(ctx, identity.Name, identity.ObjectPath)
	if err != nil {
		return UnitAccounting{}, err
	}
	if err := requireSameIdentity("observe_accounting", identity, confirmed.Identity); err != nil {
		return UnitAccounting{}, err
	}
	return result, nil
}

func (v cgroupVerifier) observe(snapshot UnitSnapshot) (UnitAccounting, error) {
	path, err := v.controlGroupPath(snapshot.ControlGroup)
	if err != nil {
		return UnitAccounting{}, err
	}
	return observeUnitAccounting(snapshot.Identity, path, cgroup.ReadCPUAccountingSnapshot, cgroup.ReadMemoryAccountingSnapshot)
}

func observeUnitAccounting(
	identity UnitIdentity,
	path string,
	readCPU func(string) (cgroup.CPUPointsNodeSnapshot, error),
	readMemory func(string) (cgroup.MemoryAccountingSnapshot, error),
) (UnitAccounting, error) {
	result := UnitAccounting{Identity: identity}
	cpu, err := readCPU(path)
	if err != nil {
		result.CPUError = err
	} else {
		result.CPU = &cpu
	}
	memory, err := readMemory(path)
	if err != nil {
		result.MemoryError = err
	} else {
		result.Memory = &memory
	}
	if result.CPU != nil && result.Memory != nil && result.CPU.Identity != result.Memory.Identity {
		return UnitAccounting{}, fmt.Errorf("unit accounting changed cgroup identity between resource reads")
	}
	return result, nil
}

const defaultCgroupRoot = "/sys/fs/cgroup"

// kernelValueMismatch distinguishes a parsed but not yet converged controller
// value from read, permission and parse failures, which are never retried.
type kernelValueMismatch struct{ err error }

func (e *kernelValueMismatch) Error() string { return e.err.Error() }
func (e *kernelValueMismatch) Unwrap() error { return e.err }

type cgroupVerifier struct {
	root                string
	readFile            func(string) ([]byte, error)
	stat                func(string) (os.FileInfo, error)
	resetIODeviceWeight func(string, string, uint64, ioDeviceWeightReset) error
	pageSize            uint64
}

func newCgroupVerifier(root string) cgroupVerifier {
	if root == "" {
		root = defaultCgroupRoot
	}
	return cgroupVerifier{
		root: filepath.Clean(root), readFile: os.ReadFile, stat: os.Stat,
		resetIODeviceWeight: writeIODeviceWeightReset,
		pageSize:            uint64(os.Getpagesize()),
	}
}

// identity opens every component beneath the configured cgroup root without
// following symlinks and returns the kernel cgroup inode used as its stable ID.
// Component-wise openat is available on the older kernels supported by EL8.
func (v cgroupVerifier) identity(controlGroup string) (uint64, error) {
	if !validControlGroup(controlGroup) {
		return 0, fmt.Errorf("invalid authoritative control-group path %q", controlGroup)
	}
	rootFD, err := unix.Open(v.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, fmt.Errorf("open cgroup root %s without following symlinks: %w", v.root, err)
	}
	currentFD := rootFD
	for _, component := range strings.Split(strings.TrimPrefix(controlGroup, "/"), "/") {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if openErr != nil {
			_ = unix.Close(rootFD)
			return 0, fmt.Errorf("open cgroup component %q beneath %s without following symlinks: %w", component, v.root, openErr)
		}
		currentFD = nextFD
	}
	if currentFD != rootFD {
		defer func() { _ = unix.Close(currentFD) }()
	}
	defer func() { _ = unix.Close(rootFD) }()
	var stat unix.Stat_t
	if err := unix.Fstat(currentFD, &stat); err != nil {
		return 0, fmt.Errorf("stat authoritative control-group path %q: %w", controlGroup, err)
	}
	if stat.Ino == 0 {
		return 0, fmt.Errorf("authoritative control-group path %q has a zero inode", controlGroup)
	}
	return stat.Ino, nil
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
	for _, property := range []PropertyName{PropertyCPUWeight, PropertyMemoryHigh, PropertyMemoryMax, PropertyMemorySwapMax} {
		if !touched[property] {
			continue
		}
		if err := v.verifyScalarFile(path, snapshot, property); err != nil {
			return err
		}
	}
	if touched[PropertyIOWeight] {
		if err := v.verifyIOWeight(path, snapshot); err != nil {
			return err
		}
	}
	if touched[PropertyCPUQuotaPerSecUSec] || touched[PropertyCPUQuotaPeriodUSec] {
		if err := v.verifyCPUQuota(path, snapshot); err != nil {
			return err
		}
	}
	for property := range approvedDeviceProperties {
		if touched[property] {
			if property == PropertyIODeviceWeight {
				assignment := findAssignmentByProperty(assignments, property)
				if err := v.verifyIODeviceWeight(path, snapshot, assignment); err != nil {
					return err
				}
				continue
			}
			if err := v.verifyDeviceLimits(path, snapshot, property); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v cgroupVerifier) preflight(snapshot UnitSnapshot, assignments []PropertyAssignment) error {
	return v.preflightWithMaterialization(snapshot, assignments, false)
}

// preflightApply permits systemd to materialize io.max through the first
// transactional I/O property write. Startup probing remains strict and proves
// this host capability before any production resource plan can reach here.
func (v cgroupVerifier) preflightApply(snapshot UnitSnapshot, assignments []PropertyAssignment) error {
	return v.preflightWithMaterialization(snapshot, assignments, true)
}

func (v cgroupVerifier) preflightWithMaterialization(snapshot UnitSnapshot, assignments []PropertyAssignment, allowMissingIO bool) error {
	path, err := v.controlGroupPath(snapshot.ControlGroup)
	if err != nil {
		return err
	}
	required := make(map[string]bool)
	requiresIO := false
	for _, assignment := range assignments {
		switch assignment.name {
		case PropertyCPUQuotaPerSecUSec, PropertyCPUQuotaPeriodUSec:
			required["cpu.max"] = true
		case PropertyMemoryHigh:
			required["memory.high"] = true
		case PropertyMemoryMax:
			required["memory.max"] = true
		case PropertyMemorySwapMax:
			required["memory.swap.max"] = true
		case PropertyIOWeight:
			requiresIO = true
		case PropertyIODeviceWeight:
			requiresIO = true
			if err := v.preflightIODeviceWeightTargets(assignment); err != nil {
				return err
			}
			for _, target := range assignment.ioDeviceWeightTargets {
				required[ioDeviceWeightKernelFile(target.mechanism)] = true
			}
		case PropertyIOReadBandwidthMax, PropertyIOWriteBandwidthMax, PropertyIOReadIOPSMax, PropertyIOWriteIOPSMax:
			required["io.max"] = true
			requiresIO = true
		}
	}
	for filename := range required {
		if _, err := v.readFile(filepath.Join(path, filename)); err != nil {
			if allowMissingIO && strings.HasPrefix(filename, "io.") && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("required controller interface %s is unavailable for %s: %w", filename, snapshot.Identity.Name, err)
		}
	}
	if requiresIO {
		controllerOwner := filepath.Dir(path)
		if allowMissingIO {
			// systemd 239 may not expose io in an intermediate slice until
			// the first child I/O property asks PID 1 to enable it there.
			// Root support is the precondition for that materialization; the
			// strict post-Apply readback remains the acknowledgement.
			controllerOwner = v.root
		}
		controllers, err := v.readFile(filepath.Join(controllerOwner, "cgroup.controllers"))
		if err != nil {
			return fmt.Errorf("inspect available I/O controller for %s: %w", snapshot.Identity.Name, err)
		}
		if !containsWord(string(controllers), "io") {
			return fmt.Errorf("required I/O controller is unavailable for %s", snapshot.Identity.Name)
		}
	}
	return nil
}

func (v cgroupVerifier) preflightIODeviceWeightTargets(assignment PropertyAssignment) error {
	seen := make(map[string]string, len(assignment.ioDeviceWeightTargets))
	for _, target := range assignment.ioDeviceWeightTargets {
		device, err := v.deviceNumber(target.path)
		if err != nil {
			return fmt.Errorf("resolve block device %s for %s preflight: %w", target.path, PropertyIODeviceWeight, err)
		}
		if previous, duplicated := seen[device]; duplicated {
			return fmt.Errorf("%s paths %s and %s resolve to the same device %s", PropertyIODeviceWeight, previous, target.path, device)
		}
		seen[device] = target.path
	}
	return nil
}

func containsWord(value, wanted string) bool {
	for _, field := range strings.Fields(value) {
		if field == wanted {
			return true
		}
	}
	return false
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
	actual, err := parseKernelScalar(string(data))
	if err != nil {
		return fmt.Errorf("parse effective %s for %s: %w", filename, snapshot.Identity.Name, err)
	}
	if expected == SystemdUnset {
		expected = defaultWhenUnset
	} else if property == PropertyMemoryHigh || property == PropertyMemoryMax || property == PropertyMemorySwapMax {
		expected = pageAlignedMemoryLimit(expected, v.pageSize)
	}
	if actual != expected {
		return &kernelValueMismatch{fmt.Errorf("effective %s mismatch for %s: systemd=%d kernel=%d", property, snapshot.Identity.Name, expected, actual)}
	}
	return nil
}

func pageAlignedMemoryLimit(value, pageSize uint64) uint64 {
	if value == SystemdUnset || pageSize == 0 {
		return value
	}
	return value - value%pageSize
}

func (v cgroupVerifier) verifyIOWeight(path string, snapshot UnitSnapshot) error {
	systemdExpected, ok := snapshot.Properties.Value(PropertyIOWeight)
	if !ok {
		return fmt.Errorf("systemd readback omitted %s", PropertyIOWeight)
	}
	expectedUnset := systemdExpected == SystemdUnset
	if systemdExpected == SystemdUnset {
		systemdExpected = 100
	}
	filename := "io.weight"
	data, err := v.readFile(filepath.Join(path, filename))
	bfq := false
	if errors.Is(err, os.ErrNotExist) {
		filename = "io.bfq.weight"
		data, err = v.readFile(filepath.Join(path, filename))
		bfq = true
	}
	if expectedUnset && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read effective I/O weight for %s: %w", snapshot.Identity.Name, err)
	}
	actual, err := parseKernelDefaultIOWeight(string(data))
	if err != nil {
		return fmt.Errorf("parse effective %s for %s: %w", filename, snapshot.Identity.Name, err)
	}
	scaledExpected := systemdExpected
	if bfq {
		scaledExpected = bfqWeight(systemdExpected)
	}
	if actual != systemdExpected && actual != scaledExpected {
		return &kernelValueMismatch{fmt.Errorf("effective %s mismatch for %s: systemd=%d bfq_scaled=%d kernel=%d", PropertyIOWeight, snapshot.Identity.Name, systemdExpected, scaledExpected, actual)}
	}
	return nil
}

func bfqWeight(ioWeight uint64) uint64 {
	const (
		defaultWeight = uint64(100)
		minimumBFQ    = uint64(1)
		maximumBFQ    = uint64(1_000)
		maximumWeight = uint64(10_000)
	)
	if ioWeight <= defaultWeight {
		return defaultWeight - (defaultWeight-ioWeight)*(defaultWeight-minimumBFQ)/(defaultWeight-minimumBFQ)
	}
	return defaultWeight + (ioWeight-defaultWeight)*(maximumBFQ-defaultWeight)/(maximumWeight-defaultWeight)
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
		return &kernelValueMismatch{fmt.Errorf("effective CPU quota period mismatch for %s: systemd=%d kernel=%d", snapshot.Identity.Name, configuredPeriod, actualPeriod)}
	}
	if perSecond == SystemdUnset {
		if fields[0] != "max" {
			return &kernelValueMismatch{fmt.Errorf("effective CPU quota mismatch for %s: systemd=infinity kernel=%q", snapshot.Identity.Name, fields[0])}
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
		return &kernelValueMismatch{fmt.Errorf("effective CPU quota mismatch for %s: systemd=%d/%d kernel=%d/%d", snapshot.Identity.Name, perSecond, configuredPeriod, actualQuota, actualPeriod)}
	}
	return nil
}

func (v cgroupVerifier) verifyDeviceLimits(path string, snapshot UnitSnapshot, property PropertyName) error {
	expected, ok := snapshot.Properties.DeviceLimits(property)
	if !ok {
		return fmt.Errorf("systemd readback omitted %s", property)
	}
	data, err := v.readFile(filepath.Join(path, "io.max"))
	if err != nil {
		if len(expected) == 0 && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read effective io.max for %s: %w", snapshot.Identity.Name, err)
	}
	actual, err := parseKernelIOMax(string(data))
	if err != nil {
		return fmt.Errorf("parse effective io.max for %s: %w", snapshot.Identity.Name, err)
	}
	field := map[PropertyName]string{
		PropertyIOReadBandwidthMax:  "rbps",
		PropertyIOWriteBandwidthMax: "wbps",
		PropertyIOReadIOPSMax:       "riops",
		PropertyIOWriteIOPSMax:      "wiops",
	}[property]
	wantedDevices := make(map[string]uint64, len(expected))
	for _, limit := range expected {
		device, err := v.deviceNumber(limit.Path)
		if err != nil {
			return fmt.Errorf("resolve block device %s for %s: %w", limit.Path, property, err)
		}
		wantedDevices[device] = limit.Value
	}
	for device, fields := range actual {
		value, exists := fields[field]
		wanted, constrained := wantedDevices[device]
		if constrained {
			if !exists || value != strconv.FormatUint(wanted, 10) {
				return &kernelValueMismatch{fmt.Errorf("effective %s mismatch for %s device %s: systemd=%d kernel=%q", property, snapshot.Identity.Name, device, wanted, value)}
			}
			delete(wantedDevices, device)
			continue
		}
		if exists && value != "max" {
			return &kernelValueMismatch{fmt.Errorf("effective %s retains unexpected finite limit for %s device %s: %s", property, snapshot.Identity.Name, device, value)}
		}
	}
	if len(wantedDevices) != 0 {
		return &kernelValueMismatch{fmt.Errorf("effective %s is absent for %s on %d devices", property, snapshot.Identity.Name, len(wantedDevices))}
	}
	return nil
}

func findAssignmentByProperty(assignments []PropertyAssignment, property PropertyName) PropertyAssignment {
	for _, assignment := range assignments {
		if assignment.name == property {
			return assignment
		}
	}
	return PropertyAssignment{}
}

func ioDeviceWeightKernelFile(mechanism IODeviceWeightMechanism) string {
	if mechanism == IODeviceWeightMechanismBFQ {
		return "io.bfq.weight"
	}
	return "io.weight"
}

func (v cgroupVerifier) verifyIODeviceWeight(path string, snapshot UnitSnapshot, assignment PropertyAssignment) error {
	expected, ok := snapshot.Properties.DeviceLimits(PropertyIODeviceWeight)
	if !ok {
		return fmt.Errorf("systemd readback omitted %s", PropertyIODeviceWeight)
	}
	if err := validateIODeviceWeightAssignment(assignment); err != nil {
		return fmt.Errorf("invalid typed %s verification context: %w", PropertyIODeviceWeight, err)
	}
	valuesByPath := make(map[string]uint64, len(expected))
	for _, value := range expected {
		valuesByPath[value.Path] = value.Value
	}
	parsedFiles := make(map[string]map[string]uint64)
	seenDevices := make(map[string]string, len(assignment.ioDeviceWeightTargets))
	for _, target := range assignment.ioDeviceWeightTargets {
		device, err := v.deviceNumber(target.path)
		if err != nil {
			return fmt.Errorf("resolve block device %s for %s: %w", target.path, PropertyIODeviceWeight, err)
		}
		if previous, duplicated := seenDevices[device]; duplicated {
			return fmt.Errorf("%s paths %s and %s resolve to the same device %s", PropertyIODeviceWeight, previous, target.path, device)
		}
		seenDevices[device] = target.path
		filename := ioDeviceWeightKernelFile(target.mechanism)
		actual, loaded := parsedFiles[filename]
		if !loaded {
			data, readErr := v.readFile(filepath.Join(path, filename))
			if readErr != nil {
				if errors.Is(readErr, os.ErrNotExist) && !ioDeviceWeightMechanismHasExpectedOverride(valuesByPath, assignment.ioDeviceWeightTargets, target.mechanism) {
					actual = map[string]uint64{}
					parsedFiles[filename] = actual
					continue
				}
				return fmt.Errorf("read effective %s for %s: %w", filename, snapshot.Identity.Name, readErr)
			}
			actual, err = parseKernelIODeviceWeights(string(data))
			if err != nil {
				return fmt.Errorf("parse effective %s for %s: %w", filename, snapshot.Identity.Name, err)
			}
			parsedFiles[filename] = actual
		}
		systemdValue, present := valuesByPath[target.path]
		kernelValue, kernelPresent := actual[device]
		if !present {
			if kernelPresent {
				return &kernelValueMismatch{fmt.Errorf("effective %s retains unexpected device override for %s device %s: %d", PropertyIODeviceWeight, snapshot.Identity.Name, device, kernelValue)}
			}
			continue
		}
		wanted := kernelIODeviceWeight(systemdValue, target.mechanism)
		if !kernelPresent || kernelValue != wanted {
			return &kernelValueMismatch{fmt.Errorf("effective %s mismatch for %s device %s through %s: systemd=%d kernel_expected=%d kernel=%d present=%t", PropertyIODeviceWeight, snapshot.Identity.Name, device, target.mechanism, systemdValue, wanted, kernelValue, kernelPresent)}
		}
	}
	return nil
}

func ioDeviceWeightMechanismHasExpectedOverride(valuesByPath map[string]uint64, targets []ioDeviceWeightTarget, mechanism IODeviceWeightMechanism) bool {
	for _, target := range targets {
		if target.mechanism == mechanism {
			if _, present := valuesByPath[target.path]; present {
				return true
			}
		}
	}
	return false
}

func (v cgroupVerifier) deviceNumber(path string) (string, error) {
	stat := v.stat
	if stat == nil {
		stat = os.Stat
	}
	info, err := stat(path)
	if err != nil {
		return "", err
	}
	data, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return "", fmt.Errorf("path is not a block device")
	}
	return fmt.Sprintf("%d:%d", unix.Major(uint64(data.Rdev)), unix.Minor(uint64(data.Rdev))), nil
}

func parseKernelIOMax(value string) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(value), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !strings.Contains(fields[0], ":") {
			return nil, fmt.Errorf("invalid device field %q", fields[0])
		}
		values := make(map[string]string, len(fields)-1)
		for _, field := range fields[1:] {
			parts := strings.SplitN(field, "=", 2)
			if len(parts) != 2 || values[parts[0]] != "" {
				return nil, fmt.Errorf("invalid or duplicate limit field %q", field)
			}
			values[parts[0]] = parts[1]
		}
		result[fields[0]] = values
	}
	return result, nil
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

func parseKernelIODeviceWeights(value string) (map[string]uint64, error) {
	result := make(map[string]uint64)
	for _, line := range strings.Split(strings.TrimSpace(value), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("expected a device and weight, got %q", line)
		}
		if fields[0] == "default" {
			if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
				return nil, fmt.Errorf("parse default I/O weight %q: %w", fields[1], err)
			}
			continue
		}
		parts := strings.Split(fields[0], ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid device field %q", fields[0])
		}
		for _, part := range parts {
			if _, err := strconv.ParseUint(part, 10, 32); err != nil {
				return nil, fmt.Errorf("invalid device field %q", fields[0])
			}
		}
		if _, duplicate := result[fields[0]]; duplicate {
			return nil, fmt.Errorf("duplicate device field %q", fields[0])
		}
		weight, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || weight == 0 {
			return nil, fmt.Errorf("invalid device weight %q", fields[1])
		}
		result[fields[0]] = weight
	}
	return result, nil
}
