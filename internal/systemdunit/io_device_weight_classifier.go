package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// IODeviceWeightCapabilityOutcome is the read-only classification of an
// enabled weighted-I/O device set.
type IODeviceWeightCapabilityOutcome string

const (
	IODeviceWeightProbeCandidate       IODeviceWeightCapabilityOutcome = "probe_candidate"
	IODeviceWeightMechanismInactive    IODeviceWeightCapabilityOutcome = "mechanism_inactive"
	IODeviceWeightUnsupportedMechanism IODeviceWeightCapabilityOutcome = "unsupported_mechanism"
	IODeviceWeightEvidenceUnavailable  IODeviceWeightCapabilityOutcome = "evidence_unavailable"
	IODeviceWeightAmbiguousTopology    IODeviceWeightCapabilityOutcome = "ambiguous_topology"
	IODeviceWeightMechanismAmbiguous   IODeviceWeightCapabilityOutcome = "mechanism_ambiguous"
)

// IODeviceWeightCapabilityReason identifies why classification or confirmation
// did not produce an unchanged probe candidate.
type IODeviceWeightCapabilityReason string

const (
	IODeviceWeightReasonNone                  IODeviceWeightCapabilityReason = ""
	IODeviceWeightReasonEmptySelector         IODeviceWeightCapabilityReason = "empty_enabled_plan"
	IODeviceWeightReasonInvalidSelector       IODeviceWeightCapabilityReason = "invalid_selector"
	IODeviceWeightReasonDuplicateDevice       IODeviceWeightCapabilityReason = "duplicate_device"
	IODeviceWeightReasonDeviceMissing         IODeviceWeightCapabilityReason = "device_missing"
	IODeviceWeightReasonDeviceIdentityChanged IODeviceWeightCapabilityReason = "device_identity_changed"
	IODeviceWeightReasonEvidenceUnavailable   IODeviceWeightCapabilityReason = "evidence_unavailable"
	IODeviceWeightReasonAmbiguousTopology     IODeviceWeightCapabilityReason = "ambiguous_topology"
	IODeviceWeightReasonMechanismUnsupported  IODeviceWeightCapabilityReason = "mechanism_unsupported"
	IODeviceWeightReasonNoActiveMechanism     IODeviceWeightCapabilityReason = "no_active_mechanism"
	IODeviceWeightReasonMechanismAmbiguous    IODeviceWeightCapabilityReason = "mechanism_ambiguous"
	IODeviceWeightReasonSchedulerChanged      IODeviceWeightCapabilityReason = "scheduler_changed"
	IODeviceWeightReasonIOCostChanged         IODeviceWeightCapabilityReason = "io_cost_changed"
	IODeviceWeightReasonCapabilityChanged     IODeviceWeightCapabilityReason = "capability_changed"
)

// IODeviceWeightCapabilityError is a typed selector, discovery or confirmation
// failure. Negative runtime capability outcomes remain available in snapshots.
type IODeviceWeightCapabilityError struct {
	Reason IODeviceWeightCapabilityReason
	Device string
	Err    error
}

func (e *IODeviceWeightCapabilityError) Error() string {
	if e.Device != "" {
		return fmt.Sprintf("weighted I/O capability %s for %s: %v", e.Reason, e.Device, e.Err)
	}
	return fmt.Sprintf("weighted I/O capability %s: %v", e.Reason, e.Err)
}

func (e *IODeviceWeightCapabilityError) Unwrap() error { return e.Err }

func newIODeviceWeightCapabilityError(reason IODeviceWeightCapabilityReason, device string, err error) error {
	return &IODeviceWeightCapabilityError{Reason: reason, Device: device, Err: err}
}

// IODeviceWeightPlatformIdentity is best-effort diagnostic provenance. None of
// its fields authorize or refuse weighted I/O at runtime.
type IODeviceWeightPlatformIdentity struct {
	DistributionID    string
	DistributionMajor int
	KernelSeries      string
	KernelRelease     string
}

// IODeviceWeightDeviceIdentity binds a major:minor request to one live sysfs
// lifetime and the canonical device node passed to systemd.
type IODeviceWeightDeviceIdentity struct {
	Number     IODeviceWeightDeviceNumber
	DeviceNode string
	SysfsPath  string
	SysfsInode uint64
}

// IODeviceWeightMechanismState is one independently observed mechanism state.
type IODeviceWeightMechanismState string

const (
	IODeviceWeightMechanismStateActive      IODeviceWeightMechanismState = "active"
	IODeviceWeightMechanismStateInactive    IODeviceWeightMechanismState = "inactive"
	IODeviceWeightMechanismStateUnsupported IODeviceWeightMechanismState = "unsupported"
	IODeviceWeightMechanismStateUnavailable IODeviceWeightMechanismState = "unavailable"
)

// IODeviceWeightMechanismEvidence records the independent read-only facts for
// one possible kernel consumer.
type IODeviceWeightMechanismEvidence struct {
	Mechanism           IODeviceWeightMechanism
	ControllerAvailable bool
	RuntimeAvailable    bool
	SchedulerAvailable  bool
	SchedulerSelected   bool
	IOCostEnabled       bool
	ObservedScheduler   string
	State               IODeviceWeightMechanismState
	UnavailableEvidence string
}

// IODeviceWeightDeviceCapability is the immutable per-device projection of a
// capability snapshot.
type IODeviceWeightDeviceCapability struct {
	Identity  IODeviceWeightDeviceIdentity
	Outcome   IODeviceWeightCapabilityOutcome
	Reason    IODeviceWeightCapabilityReason
	Detail    string
	Mechanism IODeviceWeightMechanism
	BFQ       IODeviceWeightMechanismEvidence
	IOCost    IODeviceWeightMechanismEvidence
}

// IODeviceWeightProbeTarget is the typed handoff to the owned mutating probe.
// It does not assert that the systemd-to-kernel path works.
type IODeviceWeightProbeTarget struct {
	Identity  IODeviceWeightDeviceIdentity
	Mechanism IODeviceWeightMechanism
}

// IODeviceWeightCapabilitySnapshot is immutable after construction. Slice
// accessors return defensive copies.
type IODeviceWeightCapabilitySnapshot struct {
	selector string
	platform IODeviceWeightPlatformIdentity
	outcome  IODeviceWeightCapabilityOutcome
	reason   IODeviceWeightCapabilityReason
	detail   string
	devices  []IODeviceWeightDeviceCapability
}

// Selector returns the canonical sorted major:minor selector.
func (s IODeviceWeightCapabilitySnapshot) Selector() string { return s.selector }

// Platform returns best-effort diagnostic runtime coordinates.
func (s IODeviceWeightCapabilitySnapshot) Platform() IODeviceWeightPlatformIdentity {
	return s.platform
}

// Outcome returns the atomic outcome for the complete selected device set.
func (s IODeviceWeightCapabilitySnapshot) Outcome() IODeviceWeightCapabilityOutcome {
	return s.outcome
}

// Reason returns the typed cause of a negative atomic outcome.
func (s IODeviceWeightCapabilitySnapshot) Reason() IODeviceWeightCapabilityReason {
	return s.reason
}

// Detail returns bounded diagnostic evidence for a negative outcome.
func (s IODeviceWeightCapabilitySnapshot) Detail() string { return s.detail }

// Devices returns a defensive copy of the per-device evidence.
func (s IODeviceWeightCapabilitySnapshot) Devices() []IODeviceWeightDeviceCapability {
	return append([]IODeviceWeightDeviceCapability(nil), s.devices...)
}

// ProbeTargets returns the adapter handoff only when the complete atomic device
// set has one mechanism candidate per device. The adapter must still prove the
// systemd-to-kernel path before production policy may use these targets.
func (s IODeviceWeightCapabilitySnapshot) ProbeTargets() []IODeviceWeightProbeTarget {
	if s.outcome != IODeviceWeightProbeCandidate {
		return nil
	}
	result := make([]IODeviceWeightProbeTarget, 0, len(s.devices))
	for _, device := range s.devices {
		if device.Outcome != IODeviceWeightProbeCandidate || !validIODeviceWeightMechanism(device.Mechanism) {
			return nil
		}
		result = append(result, IODeviceWeightProbeTarget{Identity: device.Identity, Mechanism: device.Mechanism})
	}
	return result
}

type ioDeviceWeightClassifierIO struct {
	osReleasePath   string
	sysRoot         string
	sysDevBlockRoot string
	cgroupRoot      string
	devRoot         string
	readFile        func(string) ([]byte, error)
	readDir         func(string) ([]os.DirEntry, error)
	evalSymlinks    func(string) (string, error)
	stat            func(string) (os.FileInfo, error)
	kernelRelease   func() (string, error)
}

// IODeviceWeightCapabilityClassifier resolves and classifies weighted-I/O
// devices without changing systemd, scheduler or io.cost state.
type IODeviceWeightCapabilityClassifier struct {
	io ioDeviceWeightClassifierIO
}

// NewIODeviceWeightCapabilityClassifier constructs the production read-only
// classifier.
func NewIODeviceWeightCapabilityClassifier() *IODeviceWeightCapabilityClassifier {
	return &IODeviceWeightCapabilityClassifier{io: ioDeviceWeightClassifierIO{
		osReleasePath:   "/etc/os-release",
		sysRoot:         "/sys",
		sysDevBlockRoot: "/sys/dev/block",
		cgroupRoot:      defaultCgroupRoot,
		devRoot:         "/dev",
		readFile:        os.ReadFile,
		readDir:         os.ReadDir,
		evalSymlinks:    filepath.EvalSymlinks,
		stat:            os.Stat,
		kernelRelease:   readKernelRelease,
	}}
}

// Classify parses one enabled selector and returns an immutable atomic
// capability snapshot. Negative runtime results are outcomes, not zero values.
func (c *IODeviceWeightCapabilityClassifier) Classify(ctx context.Context, selector string) (IODeviceWeightCapabilitySnapshot, error) {
	devices, err := ParseIODeviceWeightDevices(selector)
	if err != nil {
		return IODeviceWeightCapabilitySnapshot{}, err
	}
	canonical := canonicalIODeviceWeightSelector(devices)
	platform := c.observePlatformDiagnostics()
	environment := c.readMechanismEnvironment()
	classified := make([]IODeviceWeightDeviceCapability, 0, len(devices))
	for _, number := range devices {
		if err := ctx.Err(); err != nil {
			classified = append(classified, IODeviceWeightDeviceCapability{
				Identity: IODeviceWeightDeviceIdentity{Number: number}, Outcome: IODeviceWeightEvidenceUnavailable, Reason: IODeviceWeightReasonEvidenceUnavailable,
			})
			continue
		}
		resolved, resolveErr := c.resolveDevice(number)
		if resolveErr != nil {
			classified = append(classified, deviceCapabilityFromError(number, resolveErr))
			continue
		}
		classified = append(classified, c.classifyResolvedDevice(resolved, environment))
	}
	outcome, reason, detail := aggregateIODeviceWeightCapability(classified)
	return newIODeviceWeightCapabilitySnapshot(canonical, platform, outcome, reason, detail, classified), nil
}

// Confirm repeats read-only discovery and rejects changes relevant to the
// selected mechanism. Diagnostic platform text is deliberately ignored.
func (c *IODeviceWeightCapabilityClassifier) Confirm(ctx context.Context, previous IODeviceWeightCapabilitySnapshot) (IODeviceWeightCapabilitySnapshot, error) {
	current, err := c.Classify(ctx, previous.selector)
	if err != nil {
		return IODeviceWeightCapabilitySnapshot{}, err
	}
	if ioDeviceWeightSnapshotsEquivalent(previous, current) {
		return current, nil
	}
	reason, device := classifyIODeviceWeightSnapshotChange(previous, current)
	return current, newIODeviceWeightCapabilityError(reason, device, fmt.Errorf("read-only capability evidence changed"))
}

func (c *IODeviceWeightCapabilityClassifier) observePlatformDiagnostics() IODeviceWeightPlatformIdentity {
	platform := IODeviceWeightPlatformIdentity{}
	if data, err := c.io.readFile(c.io.osReleasePath); err == nil {
		if values, parseErr := parseOSReleaseIdentity(string(data)); parseErr == nil {
			platform.DistributionID = values["ID"]
			platform.DistributionMajor, _ = parseVersionMajor(values["VERSION_ID"])
		}
	}
	if release, err := c.io.kernelRelease(); err == nil {
		platform.KernelRelease = release
		platform.KernelSeries, _ = parseKernelSeries(release)
	}
	return platform
}

type ioDeviceWeightMechanismEnvironment struct {
	controllers          []byte
	controllersAvailable bool
	controllersErr       error
	ioCostQOS            []byte
	ioCostQOSAvailable   bool
	ioCostQOSErr         error
}

func (c *IODeviceWeightCapabilityClassifier) readMechanismEnvironment() ioDeviceWeightMechanismEnvironment {
	environment := ioDeviceWeightMechanismEnvironment{}
	environment.controllers, environment.controllersAvailable, environment.controllersErr = c.readOptionalFile(filepath.Join(c.io.cgroupRoot, "cgroup.controllers"))
	environment.ioCostQOS, environment.ioCostQOSAvailable, environment.ioCostQOSErr = c.readOptionalFile(filepath.Join(c.io.cgroupRoot, "io.cost.qos"))
	return environment
}

func (c *IODeviceWeightCapabilityClassifier) readOptionalFile(path string) ([]byte, bool, error) {
	data, err := c.io.readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (c *IODeviceWeightCapabilityClassifier) classifyResolvedDevice(resolved ioDeviceWeightResolvedDevice, environment ioDeviceWeightMechanismEnvironment) IODeviceWeightDeviceCapability {
	bfq := c.inspectBFQ(resolved, environment)
	ioCost := c.inspectIOCost(resolved, environment)
	result := IODeviceWeightDeviceCapability{Identity: resolved.identity, BFQ: bfq, IOCost: ioCost}
	bfqState := bfq.State
	ioCostState := ioCost.State
	switch {
	case bfqState == IODeviceWeightMechanismStateUnavailable || ioCostState == IODeviceWeightMechanismStateUnavailable:
		result.Outcome = IODeviceWeightEvidenceUnavailable
		result.Reason = IODeviceWeightReasonEvidenceUnavailable
	case bfqState == IODeviceWeightMechanismStateActive && ioCostState == IODeviceWeightMechanismStateActive:
		result.Outcome = IODeviceWeightMechanismAmbiguous
		result.Reason = IODeviceWeightReasonMechanismAmbiguous
	case bfqState == IODeviceWeightMechanismStateActive:
		result.Outcome = IODeviceWeightProbeCandidate
		result.Mechanism = IODeviceWeightMechanismBFQ
	case ioCostState == IODeviceWeightMechanismStateActive:
		result.Outcome = IODeviceWeightProbeCandidate
		result.Mechanism = IODeviceWeightMechanismIOCost
	case bfqState == IODeviceWeightMechanismStateInactive || ioCostState == IODeviceWeightMechanismStateInactive:
		result.Outcome = IODeviceWeightMechanismInactive
		result.Reason = IODeviceWeightReasonNoActiveMechanism
	default:
		result.Outcome = IODeviceWeightUnsupportedMechanism
		result.Reason = IODeviceWeightReasonMechanismUnsupported
	}
	return result
}

func (c *IODeviceWeightCapabilityClassifier) inspectBFQ(resolved ioDeviceWeightResolvedDevice, environment ioDeviceWeightMechanismEnvironment) IODeviceWeightMechanismEvidence {
	evidence := IODeviceWeightMechanismEvidence{Mechanism: IODeviceWeightMechanismBFQ}
	scheduler, err := c.io.readFile(filepath.Join(resolved.sysfs, "queue", "scheduler"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			evidence.State = IODeviceWeightMechanismStateUnsupported
			return evidence
		}
		evidence.State = IODeviceWeightMechanismStateUnavailable
		evidence.UnavailableEvidence = err.Error()
		return evidence
	}
	evidence.ObservedScheduler = strings.TrimSpace(string(scheduler))
	evidence.SchedulerAvailable, evidence.SchedulerSelected = schedulerBFQState(evidence.ObservedScheduler)
	evidence.ControllerAvailable = environment.controllersAvailable && containsWord(string(environment.controllers), "io")
	evidence.RuntimeAvailable = evidence.SchedulerAvailable
	if environment.controllersErr != nil {
		evidence.State = IODeviceWeightMechanismStateUnavailable
		evidence.UnavailableEvidence = environment.controllersErr.Error()
		return evidence
	}
	if !evidence.ControllerAvailable || !evidence.SchedulerAvailable {
		evidence.State = IODeviceWeightMechanismStateUnsupported
		return evidence
	}
	if evidence.SchedulerSelected {
		evidence.State = IODeviceWeightMechanismStateActive
	} else {
		evidence.State = IODeviceWeightMechanismStateInactive
	}
	return evidence
}

func (c *IODeviceWeightCapabilityClassifier) inspectIOCost(resolved ioDeviceWeightResolvedDevice, environment ioDeviceWeightMechanismEnvironment) IODeviceWeightMechanismEvidence {
	evidence := IODeviceWeightMechanismEvidence{Mechanism: IODeviceWeightMechanismIOCost}
	evidence.ControllerAvailable = environment.controllersAvailable && containsWord(string(environment.controllers), "io")
	evidence.RuntimeAvailable = environment.ioCostQOSAvailable
	if environment.controllersErr != nil || environment.ioCostQOSErr != nil {
		evidence.State = IODeviceWeightMechanismStateUnavailable
		evidence.UnavailableEvidence = firstErrorText(environment.controllersErr, environment.ioCostQOSErr)
		return evidence
	}
	if !evidence.ControllerAvailable || !evidence.RuntimeAvailable {
		evidence.State = IODeviceWeightMechanismStateUnsupported
		return evidence
	}
	enabled, err := ioCostEnabledForDevice(string(environment.ioCostQOS), resolved.identity.Number.String())
	if err != nil {
		evidence.State = IODeviceWeightMechanismStateUnavailable
		evidence.UnavailableEvidence = err.Error()
		return evidence
	}
	evidence.IOCostEnabled = enabled
	if enabled {
		evidence.State = IODeviceWeightMechanismStateActive
	} else {
		evidence.State = IODeviceWeightMechanismStateInactive
	}
	return evidence
}

func schedulerBFQState(value string) (available, selected bool) {
	for _, field := range strings.Fields(value) {
		name := strings.TrimSuffix(strings.TrimPrefix(field, "["), "]")
		if name != "bfq" {
			continue
		}
		available = true
		selected = strings.HasPrefix(field, "[") && strings.HasSuffix(field, "]")
	}
	return available, selected
}

func ioCostEnabledForDevice(data, device string) (bool, error) {
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != device {
			continue
		}
		for _, field := range fields[1:] {
			name, value, ok := strings.Cut(field, "=")
			if name != "enable" || !ok {
				continue
			}
			switch value {
			case "0":
				return false, nil
			case "1":
				return true, nil
			default:
				return false, fmt.Errorf("device %s has invalid io.cost enable value %q", device, value)
			}
		}
		return false, fmt.Errorf("device %s io.cost.qos entry omits enable", device)
	}
	return false, nil
}

func canonicalIODeviceWeightSelector(devices []IODeviceWeightDeviceNumber) string {
	values := make([]string, len(devices))
	for index, device := range devices {
		values[index] = device.String()
	}
	return strings.Join(values, ",")
}

func newIODeviceWeightCapabilitySnapshot(selector string, platform IODeviceWeightPlatformIdentity, outcome IODeviceWeightCapabilityOutcome, reason IODeviceWeightCapabilityReason, detail string, devices []IODeviceWeightDeviceCapability) IODeviceWeightCapabilitySnapshot {
	return IODeviceWeightCapabilitySnapshot{selector: selector, platform: platform, outcome: outcome, reason: reason, detail: detail, devices: append([]IODeviceWeightDeviceCapability(nil), devices...)}
}

func deviceCapabilityFromError(number IODeviceWeightDeviceNumber, err error) IODeviceWeightDeviceCapability {
	result := IODeviceWeightDeviceCapability{Identity: IODeviceWeightDeviceIdentity{Number: number}, Outcome: IODeviceWeightEvidenceUnavailable, Reason: IODeviceWeightReasonEvidenceUnavailable}
	var capabilityErr *IODeviceWeightCapabilityError
	if !errors.As(err, &capabilityErr) {
		result.Detail = err.Error()
		return result
	}
	result.Reason = capabilityErr.Reason
	result.Detail = capabilityErr.Error()
	if capabilityErr.Reason == IODeviceWeightReasonAmbiguousTopology {
		result.Outcome = IODeviceWeightAmbiguousTopology
	}
	return result
}

func aggregateIODeviceWeightCapability(devices []IODeviceWeightDeviceCapability) (IODeviceWeightCapabilityOutcome, IODeviceWeightCapabilityReason, string) {
	for _, candidate := range []IODeviceWeightCapabilityOutcome{
		IODeviceWeightAmbiguousTopology,
		IODeviceWeightMechanismAmbiguous,
		IODeviceWeightUnsupportedMechanism,
		IODeviceWeightEvidenceUnavailable,
		IODeviceWeightMechanismInactive,
	} {
		for _, device := range devices {
			if device.Outcome == candidate {
				return candidate, device.Reason, device.Detail
			}
		}
	}
	return IODeviceWeightProbeCandidate, IODeviceWeightReasonNone, ""
}

func ioDeviceWeightSnapshotsEquivalent(previous, current IODeviceWeightCapabilitySnapshot) bool {
	if previous.selector != current.selector || previous.outcome != current.outcome || previous.reason != current.reason || len(previous.devices) != len(current.devices) {
		return false
	}
	for index := range previous.devices {
		if !ioDeviceWeightDeviceCapabilitiesEquivalent(previous.devices[index], current.devices[index]) {
			return false
		}
	}
	return true
}

func ioDeviceWeightDeviceCapabilitiesEquivalent(before, after IODeviceWeightDeviceCapability) bool {
	if before.Identity != after.Identity || before.Outcome != after.Outcome || before.Reason != after.Reason || before.Mechanism != after.Mechanism {
		return false
	}
	switch before.Mechanism {
	case IODeviceWeightMechanismBFQ:
		return before.BFQ.State == after.BFQ.State && before.BFQ.SchedulerSelected == after.BFQ.SchedulerSelected && before.BFQ.ControllerAvailable == after.BFQ.ControllerAvailable && before.BFQ.RuntimeAvailable == after.BFQ.RuntimeAvailable
	case IODeviceWeightMechanismIOCost:
		return before.IOCost.State == after.IOCost.State && before.IOCost.IOCostEnabled == after.IOCost.IOCostEnabled && before.IOCost.ControllerAvailable == after.IOCost.ControllerAvailable && before.IOCost.RuntimeAvailable == after.IOCost.RuntimeAvailable
	default:
		return ioDeviceWeightMechanismEvidenceEquivalent(before.BFQ, after.BFQ) && ioDeviceWeightMechanismEvidenceEquivalent(before.IOCost, after.IOCost)
	}
}

func ioDeviceWeightMechanismEvidenceEquivalent(left, right IODeviceWeightMechanismEvidence) bool {
	return left.Mechanism == right.Mechanism &&
		left.ControllerAvailable == right.ControllerAvailable &&
		left.RuntimeAvailable == right.RuntimeAvailable &&
		left.SchedulerAvailable == right.SchedulerAvailable &&
		left.SchedulerSelected == right.SchedulerSelected &&
		left.IOCostEnabled == right.IOCostEnabled &&
		left.State == right.State
}

func classifyIODeviceWeightSnapshotChange(previous, current IODeviceWeightCapabilitySnapshot) (IODeviceWeightCapabilityReason, string) {
	if len(previous.devices) != len(current.devices) {
		return IODeviceWeightReasonCapabilityChanged, ""
	}
	for index := range previous.devices {
		before := previous.devices[index]
		after := current.devices[index]
		device := before.Identity.Number.String()
		if after.Reason == IODeviceWeightReasonDeviceMissing {
			return IODeviceWeightReasonDeviceMissing, device
		}
		if before.Identity != after.Identity {
			return IODeviceWeightReasonDeviceIdentityChanged, device
		}
		if before.BFQ.SchedulerSelected != after.BFQ.SchedulerSelected {
			return IODeviceWeightReasonSchedulerChanged, device
		}
		if before.IOCost.IOCostEnabled != after.IOCost.IOCostEnabled {
			return IODeviceWeightReasonIOCostChanged, device
		}
		if !ioDeviceWeightDeviceCapabilitiesEquivalent(before, after) {
			return IODeviceWeightReasonCapabilityChanged, device
		}
	}
	return IODeviceWeightReasonCapabilityChanged, ""
}

func parseOSReleaseIdentity(data string) (map[string]string, error) {
	result := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid os-release line %q", line)
		}
		name = strings.TrimSpace(name)
		if name != "ID" && name != "VERSION_ID" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("invalid os-release %s: %w", name, err)
			}
			value = decoded
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}
		result[name] = value
	}
	if result["ID"] == "" || result["VERSION_ID"] == "" {
		return nil, fmt.Errorf("os-release omits ID or VERSION_ID")
	}
	return result, nil
}

func parseVersionMajor(value string) (int, error) {
	major := strings.SplitN(value, ".", 2)[0]
	parsed, err := strconv.Atoi(major)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid distribution major %q", value)
	}
	return parsed, nil
}

func parseKernelSeries(release string) (string, error) {
	fields := strings.Split(release, ".")
	if len(fields) < 2 {
		return "", fmt.Errorf("invalid kernel release %q", release)
	}
	major, err := strconv.Atoi(fields[0])
	if err != nil || major <= 0 {
		return "", fmt.Errorf("invalid kernel release %q", release)
	}
	minor, err := strconv.Atoi(fields[1])
	if err != nil || minor < 0 {
		return "", fmt.Errorf("invalid kernel release %q", release)
	}
	return fmt.Sprintf("%d.%d", major, minor), nil
}

func firstErrorText(errors ...error) string {
	for _, err := range errors {
		if err != nil {
			return err.Error()
		}
	}
	return ""
}

func readKernelRelease() (string, error) {
	var data syscall.Utsname
	if err := syscall.Uname(&data); err != nil {
		return "", fmt.Errorf("read kernel release: %w", err)
	}
	value := make([]byte, 0, len(data.Release))
	for _, character := range data.Release {
		if character == 0 {
			break
		}
		value = append(value, byte(character))
	}
	if len(value) == 0 {
		return "", fmt.Errorf("kernel release is empty")
	}
	return string(value), nil
}
