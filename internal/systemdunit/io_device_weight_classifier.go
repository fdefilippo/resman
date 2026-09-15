package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
)

// IODeviceWeightCapabilityOutcome is the read-only classification of an
// enabled weighted-I/O device set.
type IODeviceWeightCapabilityOutcome string

const (
	IODeviceWeightSupportedActive      IODeviceWeightCapabilityOutcome = "supported_active"
	IODeviceWeightSupportedInactive    IODeviceWeightCapabilityOutcome = "supported_inactive"
	IODeviceWeightUnsupportedPlatform  IODeviceWeightCapabilityOutcome = "unsupported_platform"
	IODeviceWeightUnsupportedMechanism IODeviceWeightCapabilityOutcome = "unsupported_mechanism"
	IODeviceWeightEvidenceUnavailable  IODeviceWeightCapabilityOutcome = "evidence_unavailable"
	IODeviceWeightAmbiguousTopology    IODeviceWeightCapabilityOutcome = "ambiguous_topology"
	IODeviceWeightMechanismAmbiguous   IODeviceWeightCapabilityOutcome = "mechanism_ambiguous"
)

// IODeviceWeightCapabilityReason identifies why classification or confirmation
// did not produce an unchanged supported-active target.
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
	IODeviceWeightReasonPlatformUnclaimed     IODeviceWeightCapabilityReason = "platform_unclaimed"
	IODeviceWeightReasonMechanismUnsupported  IODeviceWeightCapabilityReason = "mechanism_unsupported"
	IODeviceWeightReasonNoActiveMechanism     IODeviceWeightCapabilityReason = "no_active_mechanism"
	IODeviceWeightReasonMechanismAmbiguous    IODeviceWeightCapabilityReason = "mechanism_ambiguous"
	IODeviceWeightReasonSchedulerChanged      IODeviceWeightCapabilityReason = "scheduler_changed"
	IODeviceWeightReasonIOCostChanged         IODeviceWeightCapabilityReason = "io_cost_changed"
	IODeviceWeightReasonPlatformChanged       IODeviceWeightCapabilityReason = "platform_changed"
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

// IODeviceWeightKernelFamily identifies the kernel family independently from
// the Oracle Linux release that ships it.
type IODeviceWeightKernelFamily string

const (
	IODeviceWeightKernelRHCK IODeviceWeightKernelFamily = "rhck"
	IODeviceWeightKernelUEK  IODeviceWeightKernelFamily = "uek"
)

// IODeviceWeightPlatformIdentity is the exact runtime line evidence used by
// the first-release Oracle Linux support matrix.
type IODeviceWeightPlatformIdentity struct {
	DistributionID    string
	DistributionMajor int
	SystemdMajor      int
	KernelFamily      IODeviceWeightKernelFamily
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
	PlatformSupported   bool
	Compiled            bool
	InterfaceAvailable  bool
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

// IODeviceWeightQualifiedTarget is the typed handoff to the mutating adapter.
type IODeviceWeightQualifiedTarget struct {
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

// Platform returns the observed runtime platform coordinates.
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

// QualifiedTargets returns the adapter handoff only for a completely
// supported-active atomic device set.
func (s IODeviceWeightCapabilitySnapshot) QualifiedTargets() []IODeviceWeightQualifiedTarget {
	if s.outcome != IODeviceWeightSupportedActive {
		return nil
	}
	result := make([]IODeviceWeightQualifiedTarget, 0, len(s.devices))
	for _, device := range s.devices {
		if device.Outcome != IODeviceWeightSupportedActive || !validIODeviceWeightMechanism(device.Mechanism) {
			return nil
		}
		result = append(result, IODeviceWeightQualifiedTarget{Identity: device.Identity, Mechanism: device.Mechanism})
	}
	return result
}

type ioDeviceWeightClassifierIO struct {
	osReleasePath   string
	bootConfigRoot  string
	sysRoot         string
	sysDevBlockRoot string
	cgroupRoot      string
	devRoot         string
	readFile        func(string) ([]byte, error)
	readDir         func(string) ([]os.DirEntry, error)
	evalSymlinks    func(string) (string, error)
	stat            func(string) (os.FileInfo, error)
	systemdVersion  func(context.Context) (string, error)
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
		bootConfigRoot:  "/boot",
		sysRoot:         "/sys",
		sysDevBlockRoot: "/sys/dev/block",
		cgroupRoot:      defaultCgroupRoot,
		devRoot:         "/dev",
		readFile:        os.ReadFile,
		readDir:         os.ReadDir,
		evalSymlinks:    filepath.EvalSymlinks,
		stat:            os.Stat,
		systemdVersion:  readSystemdVersion,
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
	platform, platformOutcome, platformReason, platformErr := c.classifyPlatform(ctx)
	if platformOutcome != IODeviceWeightSupportedActive {
		detail := ""
		if platformErr != nil {
			detail = platformErr.Error()
		}
		return uniformIODeviceWeightSnapshot(canonical, platform, devices, platformOutcome, platformReason, detail), nil
	}
	environment := c.readMechanismEnvironment(platform.KernelRelease)
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
		classified = append(classified, c.classifyResolvedDevice(resolved, platform, environment))
	}
	outcome, reason, detail := aggregateIODeviceWeightCapability(classified)
	return newIODeviceWeightCapabilitySnapshot(canonical, platform, outcome, reason, detail, classified), nil
}

// Confirm repeats read-only discovery and rejects every changed identity,
// scheduler, io.cost state, platform coordinate or capability result.
func (c *IODeviceWeightCapabilityClassifier) Confirm(ctx context.Context, previous IODeviceWeightCapabilitySnapshot) (IODeviceWeightCapabilitySnapshot, error) {
	current, err := c.Classify(ctx, previous.selector)
	if err != nil {
		return IODeviceWeightCapabilitySnapshot{}, err
	}
	if reflect.DeepEqual(previous, current) {
		return current, nil
	}
	reason, device := classifyIODeviceWeightSnapshotChange(previous, current)
	return current, newIODeviceWeightCapabilityError(reason, device, fmt.Errorf("read-only capability evidence changed"))
}

type ioDeviceWeightPlatformSupport struct {
	claimed bool
	bfq     bool
	ioCost  bool
}

func (c *IODeviceWeightCapabilityClassifier) classifyPlatform(ctx context.Context) (IODeviceWeightPlatformIdentity, IODeviceWeightCapabilityOutcome, IODeviceWeightCapabilityReason, error) {
	data, err := c.io.readFile(c.io.osReleasePath)
	if err != nil {
		return IODeviceWeightPlatformIdentity{}, IODeviceWeightEvidenceUnavailable, IODeviceWeightReasonEvidenceUnavailable, fmt.Errorf("read %s: %w", c.io.osReleasePath, err)
	}
	values, err := parseOSReleaseIdentity(string(data))
	if err != nil {
		return IODeviceWeightPlatformIdentity{}, IODeviceWeightEvidenceUnavailable, IODeviceWeightReasonEvidenceUnavailable, err
	}
	platform := IODeviceWeightPlatformIdentity{DistributionID: values["ID"]}
	major, err := parseVersionMajor(values["VERSION_ID"])
	if err != nil {
		return platform, IODeviceWeightEvidenceUnavailable, IODeviceWeightReasonEvidenceUnavailable, err
	}
	platform.DistributionMajor = major
	if platform.DistributionID != "ol" {
		return platform, IODeviceWeightUnsupportedPlatform, IODeviceWeightReasonPlatformUnclaimed, nil
	}
	systemdVersion, err := c.io.systemdVersion(ctx)
	if err != nil {
		return platform, IODeviceWeightEvidenceUnavailable, IODeviceWeightReasonEvidenceUnavailable, err
	}
	platform.SystemdMajor, err = parseSystemdMajor(systemdVersion)
	if err != nil {
		return platform, IODeviceWeightEvidenceUnavailable, IODeviceWeightReasonEvidenceUnavailable, err
	}
	platform.KernelRelease, err = c.io.kernelRelease()
	if err != nil {
		return platform, IODeviceWeightEvidenceUnavailable, IODeviceWeightReasonEvidenceUnavailable, err
	}
	platform.KernelSeries, err = parseKernelSeries(platform.KernelRelease)
	if err != nil {
		return platform, IODeviceWeightEvidenceUnavailable, IODeviceWeightReasonEvidenceUnavailable, err
	}
	platform.KernelFamily = IODeviceWeightKernelRHCK
	if strings.Contains(strings.ToLower(platform.KernelRelease), "uek") {
		platform.KernelFamily = IODeviceWeightKernelUEK
	}
	support := ioDeviceWeightSupport(platform)
	if !support.claimed {
		return platform, IODeviceWeightUnsupportedPlatform, IODeviceWeightReasonPlatformUnclaimed, nil
	}
	if !support.bfq && !support.ioCost {
		return platform, IODeviceWeightUnsupportedMechanism, IODeviceWeightReasonMechanismUnsupported, nil
	}
	return platform, IODeviceWeightSupportedActive, IODeviceWeightReasonNone, nil
}

func ioDeviceWeightSupport(platform IODeviceWeightPlatformIdentity) ioDeviceWeightPlatformSupport {
	if platform.DistributionID != "ol" || platform.KernelFamily != IODeviceWeightKernelRHCK {
		return ioDeviceWeightPlatformSupport{}
	}
	switch {
	case platform.DistributionMajor == 8 && platform.SystemdMajor == 239 && platform.KernelSeries == "4.18":
		return ioDeviceWeightPlatformSupport{claimed: true}
	case platform.DistributionMajor == 9 && platform.SystemdMajor == 252 && platform.KernelSeries == "5.14":
		return ioDeviceWeightPlatformSupport{claimed: true, bfq: true, ioCost: true}
	case platform.DistributionMajor == 10 && platform.SystemdMajor == 257 && platform.KernelSeries == "6.12":
		return ioDeviceWeightPlatformSupport{claimed: true, bfq: true, ioCost: true}
	default:
		return ioDeviceWeightPlatformSupport{}
	}
}

type ioDeviceWeightMechanismEnvironment struct {
	config               []byte
	configErr            error
	controllers          []byte
	controllersAvailable bool
	controllersErr       error
	bfqInterface         bool
	bfqInterfaceErr      error
	ioCostQOS            []byte
	ioCostQOSAvailable   bool
	ioCostQOSErr         error
	ioCostModel          bool
	ioCostModelErr       error
	ioWeightInterface    bool
	ioWeightInterfaceErr error
}

func (c *IODeviceWeightCapabilityClassifier) readMechanismEnvironment(kernelRelease string) ioDeviceWeightMechanismEnvironment {
	environment := ioDeviceWeightMechanismEnvironment{}
	environment.config, environment.configErr = c.io.readFile(filepath.Join(c.io.bootConfigRoot, "config-"+kernelRelease))
	environment.controllers, environment.controllersAvailable, environment.controllersErr = c.readOptionalFile(filepath.Join(c.io.cgroupRoot, "cgroup.controllers"))
	_, environment.bfqInterface, environment.bfqInterfaceErr = c.readOptionalFile(filepath.Join(c.io.cgroupRoot, "io.bfq.weight"))
	environment.ioCostQOS, environment.ioCostQOSAvailable, environment.ioCostQOSErr = c.readOptionalFile(filepath.Join(c.io.cgroupRoot, "io.cost.qos"))
	_, environment.ioCostModel, environment.ioCostModelErr = c.readOptionalFile(filepath.Join(c.io.cgroupRoot, "io.cost.model"))
	_, environment.ioWeightInterface, environment.ioWeightInterfaceErr = c.readOptionalFile(filepath.Join(c.io.cgroupRoot, "io.weight"))
	return environment
}

func (c *IODeviceWeightCapabilityClassifier) readOptionalFile(path string) ([]byte, bool, error) {
	data, err := c.io.readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (c *IODeviceWeightCapabilityClassifier) classifyResolvedDevice(resolved ioDeviceWeightResolvedDevice, platform IODeviceWeightPlatformIdentity, environment ioDeviceWeightMechanismEnvironment) IODeviceWeightDeviceCapability {
	support := ioDeviceWeightSupport(platform)
	bfq := c.inspectBFQ(resolved, support.bfq, environment)
	ioCost := c.inspectIOCost(resolved, support.ioCost, environment)
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
		result.Outcome = IODeviceWeightSupportedActive
		result.Mechanism = IODeviceWeightMechanismBFQ
	case ioCostState == IODeviceWeightMechanismStateActive:
		result.Outcome = IODeviceWeightSupportedActive
		result.Mechanism = IODeviceWeightMechanismIOCost
	case bfqState == IODeviceWeightMechanismStateInactive || ioCostState == IODeviceWeightMechanismStateInactive:
		result.Outcome = IODeviceWeightSupportedInactive
		result.Reason = IODeviceWeightReasonNoActiveMechanism
	default:
		result.Outcome = IODeviceWeightUnsupportedMechanism
		result.Reason = IODeviceWeightReasonMechanismUnsupported
	}
	return result
}

func (c *IODeviceWeightCapabilityClassifier) inspectBFQ(resolved ioDeviceWeightResolvedDevice, platformSupported bool, environment ioDeviceWeightMechanismEnvironment) IODeviceWeightMechanismEvidence {
	evidence := IODeviceWeightMechanismEvidence{Mechanism: IODeviceWeightMechanismBFQ, PlatformSupported: platformSupported}
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
	evidence.Compiled = kernelConfigEnabled(environment.config, "CONFIG_BFQ_GROUP_IOSCHED")
	evidence.InterfaceAvailable = environment.bfqInterface && environment.controllersAvailable && containsWord(string(environment.controllers), "io")
	if environment.configErr != nil || environment.controllersErr != nil || environment.bfqInterfaceErr != nil {
		evidence.State = IODeviceWeightMechanismStateUnavailable
		evidence.UnavailableEvidence = firstErrorText(environment.configErr, environment.controllersErr, environment.bfqInterfaceErr)
		return evidence
	}
	if !platformSupported || !evidence.Compiled || !evidence.SchedulerAvailable || !evidence.InterfaceAvailable {
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

func (c *IODeviceWeightCapabilityClassifier) inspectIOCost(resolved ioDeviceWeightResolvedDevice, platformSupported bool, environment ioDeviceWeightMechanismEnvironment) IODeviceWeightMechanismEvidence {
	evidence := IODeviceWeightMechanismEvidence{Mechanism: IODeviceWeightMechanismIOCost, PlatformSupported: platformSupported}
	evidence.Compiled = kernelConfigEnabled(environment.config, "CONFIG_BLK_CGROUP_IOCOST")
	evidence.InterfaceAvailable = environment.ioCostQOSAvailable && environment.ioCostModel && environment.ioWeightInterface && environment.controllersAvailable && containsWord(string(environment.controllers), "io")
	if environment.configErr != nil || environment.controllersErr != nil || environment.ioCostQOSErr != nil || environment.ioCostModelErr != nil || environment.ioWeightInterfaceErr != nil {
		evidence.State = IODeviceWeightMechanismStateUnavailable
		evidence.UnavailableEvidence = firstErrorText(environment.configErr, environment.controllersErr, environment.ioCostQOSErr, environment.ioCostModelErr, environment.ioWeightInterfaceErr)
		return evidence
	}
	if !platformSupported || !evidence.Compiled || !evidence.InterfaceAvailable {
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

func kernelConfigEnabled(data []byte, key string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == key+"=y" {
			return true
		}
	}
	return false
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

func uniformIODeviceWeightSnapshot(selector string, platform IODeviceWeightPlatformIdentity, devices []IODeviceWeightDeviceNumber, outcome IODeviceWeightCapabilityOutcome, reason IODeviceWeightCapabilityReason, detail string) IODeviceWeightCapabilitySnapshot {
	classified := make([]IODeviceWeightDeviceCapability, len(devices))
	for index, number := range devices {
		classified[index] = IODeviceWeightDeviceCapability{Identity: IODeviceWeightDeviceIdentity{Number: number}, Outcome: outcome, Reason: reason, Detail: detail}
	}
	return newIODeviceWeightCapabilitySnapshot(selector, platform, outcome, reason, detail, classified)
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
		IODeviceWeightEvidenceUnavailable,
		IODeviceWeightAmbiguousTopology,
		IODeviceWeightMechanismAmbiguous,
		IODeviceWeightUnsupportedMechanism,
		IODeviceWeightSupportedInactive,
	} {
		for _, device := range devices {
			if device.Outcome == candidate {
				return candidate, device.Reason, device.Detail
			}
		}
	}
	return IODeviceWeightSupportedActive, IODeviceWeightReasonNone, ""
}

func classifyIODeviceWeightSnapshotChange(previous, current IODeviceWeightCapabilitySnapshot) (IODeviceWeightCapabilityReason, string) {
	if previous.platform != current.platform {
		return IODeviceWeightReasonPlatformChanged, ""
	}
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
		if before.BFQ.ObservedScheduler != after.BFQ.ObservedScheduler || before.BFQ.SchedulerSelected != after.BFQ.SchedulerSelected {
			return IODeviceWeightReasonSchedulerChanged, device
		}
		if before.IOCost.IOCostEnabled != after.IOCost.IOCostEnabled {
			return IODeviceWeightReasonIOCostChanged, device
		}
		if before != after {
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

func parseSystemdMajor(value string) (int, error) {
	fields := strings.Fields(value)
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0, fmt.Errorf("invalid systemd version output %q", value)
	}
	major := strings.TrimPrefix(fields[1], "v")
	major = strings.SplitN(major, ".", 2)[0]
	parsed, err := strconv.Atoi(major)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid systemd major %q", fields[1])
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

func readSystemdVersion(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "systemctl", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("read systemd version: %w", err)
	}
	return string(output), nil
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
