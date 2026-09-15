package systemdunit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// StartupRequirements names the optional resource features enabled by the
// operator. CPU quota is always mandatory for systemd-native enforcement.
type StartupRequirements struct {
	Memory          bool
	IO              bool
	IODeviceWeights []IODeviceWeightRequest
}

// ProbeIODeviceWeights proves the complete systemd-to-kernel path for one
// read-only classifier snapshot. The probe owns a disposable transient unit,
// uses non-default values and restores every temporary property before return.
func (a *Adapter) ProbeIODeviceWeights(ctx context.Context, targets []IODeviceWeightProbeTarget) error {
	if len(targets) == 0 {
		return &AdapterError{
			Reason: ReasonCapabilityProbe, Operation: "io_device_weight_probe",
			Err: fmt.Errorf("weighted I/O probe requires at least one classified target"),
		}
	}
	requests := make([]IODeviceWeightRequest, 0, len(targets))
	for _, target := range targets {
		if target.Identity.DeviceNode == "" || !validIODeviceWeightMechanism(target.Mechanism) {
			return &AdapterError{
				Reason: ReasonCapabilityProbe, Operation: "io_device_weight_probe",
				Err: fmt.Errorf("weighted I/O probe target %s has incomplete typed identity or mechanism", target.Identity.Number),
			}
		}
		requests = append(requests, IODeviceWeightRequest{
			Path: target.Identity.DeviceNode, Weight: 100, Mechanism: target.Mechanism,
		})
	}
	if err := a.requireCapabilities(ctx, StartupRequirements{IODeviceWeights: requests}, func(capability startupCapability) bool {
		return capability.feature == "weighted I/O"
	}); err != nil {
		return fmt.Errorf("probe weighted I/O capability: %w", err)
	}
	return nil
}

type startupCapability struct {
	feature             string
	controller          string
	interfaceName       string
	initialAssignments  []PropertyAssignment
	probeAssignments    []PropertyAssignment
	requiredAssignments []PropertyAssignment
}

func (a *Adapter) requireStartupCapabilities(ctx context.Context, requirements StartupRequirements) error {
	return a.requireCapabilities(ctx, requirements, nil)
}

func (a *Adapter) requireCapabilities(ctx context.Context, requirements StartupRequirements, include func(startupCapability) bool) error {
	transport, ok := a.transport.(startupCapabilityTransport)
	if !ok {
		return &AdapterError{
			Reason: ReasonRequiredCapability, Operation: "startup_capabilities",
			Err: fmt.Errorf("systemd transport cannot create an authoritative capability probe"),
		}
	}
	if err := a.cleanInterruptedCapabilityProbes(ctx, transport); err != nil {
		return err
	}
	if err := a.validateStartupParent(ctx); err != nil {
		return err
	}
	capabilities, err := startupCapabilities(requirements)
	if err != nil {
		return err
	}
	for _, capability := range capabilities {
		if include != nil && !include(capability) {
			continue
		}
		if err := a.probeStartupCapability(ctx, transport, capability); err != nil {
			return err
		}
	}
	return nil
}

func startupCapabilities(requirements StartupRequirements) ([]startupCapability, error) {
	activation, err := NewPropertyAssignment(PropertyCPUWeight, 100)
	if err != nil {
		return nil, err
	}
	cpuProbe, err := NewCPUQuotaAssignmentsFromCgroupMax(50_000, 100_000)
	if err != nil {
		return nil, err
	}
	result := []startupCapability{{
		feature: "CPU limiting", controller: "cpu", interfaceName: "cpu.max",
		initialAssignments: []PropertyAssignment{activation},
		probeAssignments:   cpuProbe, requiredAssignments: append([]PropertyAssignment(nil), cpuProbe...),
	}}
	if requirements.Memory {
		for _, candidate := range []struct {
			interfaceName string
			property      PropertyName
		}{
			{interfaceName: "memory.high", property: PropertyMemoryHigh},
			{interfaceName: "memory.max", property: PropertyMemoryMax},
		} {
			assignment, assignmentErr := NewPropertyAssignment(candidate.property, 1<<30)
			if assignmentErr != nil {
				return nil, assignmentErr
			}
			result = append(result, startupCapability{
				feature: "RAM limiting", controller: "memory", interfaceName: candidate.interfaceName,
				initialAssignments: []PropertyAssignment{activation},
				probeAssignments:   []PropertyAssignment{assignment}, requiredAssignments: []PropertyAssignment{assignment},
			})
		}
	}
	if requirements.IO {
		ioActivation, assignmentErr := NewPropertyAssignment(PropertyIOWeight, 100)
		if assignmentErr != nil {
			return nil, requiredCapabilityError("I/O limiting", "io", "io.max", PropertyIOWeight, assignmentErr)
		}
		// Keep both the direct cgroup-v2 representation and the scaled BFQ
		// representation distinct from the default. A value close to 100 can
		// collapse back to the default when an older kernel scales the weight.
		ioProbe, assignmentErr := NewPropertyAssignment(PropertyIOWeight, 1_000)
		if assignmentErr != nil {
			return nil, requiredCapabilityError("I/O limiting", "io", "io.max", PropertyIOWeight, assignmentErr)
		}
		requiredIO := PropertyAssignment{name: PropertyIOReadBandwidthMax}
		result = append(result, startupCapability{
			feature: "I/O limiting", controller: "io", interfaceName: "io.max",
			// systemd 239 does not materialize io.max for a new slice until an
			// I/O property enables the controller in the parent hierarchy.
			initialAssignments: []PropertyAssignment{activation, ioActivation},
			probeAssignments:   []PropertyAssignment{ioProbe}, requiredAssignments: []PropertyAssignment{ioProbe, requiredIO},
		})
	}
	if len(requirements.IODeviceWeights) != 0 {
		ioActivation, assignmentErr := NewPropertyAssignment(PropertyIOWeight, 100)
		if assignmentErr != nil {
			return nil, assignmentErr
		}
		probeRequests := make([]IODeviceWeightRequest, len(requirements.IODeviceWeights))
		for index, request := range requirements.IODeviceWeights {
			request.Weight = 333
			if request.Mechanism == IODeviceWeightMechanismBFQ {
				request.Weight = 121
			}
			probeRequests[index] = request
		}
		deviceWeights, assignmentErr := NewIODeviceWeightAssignment(probeRequests)
		if assignmentErr != nil {
			return nil, assignmentErr
		}
		result = append(result, startupCapability{
			feature: "weighted I/O", controller: "io", interfaceName: "qualified per-device weight interface",
			initialAssignments:  []PropertyAssignment{activation, ioActivation},
			probeAssignments:    []PropertyAssignment{deviceWeights},
			requiredAssignments: []PropertyAssignment{deviceWeights},
		})
	}
	return result, nil
}

func (a *Adapter) probeStartupCapability(ctx context.Context, transport startupCapabilityTransport, capability startupCapability) (retErr error) {
	unit, err := newCapabilityProbeUnit()
	if err != nil {
		return capabilityProbePreparationError(capability.feature, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	listed, started, err := transport.startCapabilityProbe(callCtx, unit, capability.initialAssignments)
	cancel()
	if started {
		defer func() {
			// Stop the owned transient cgroup before reconciling its durable
			// property lease. A full active-unit Restore includes daemon-reload
			// and can consume the bounded adapter deadline when io.cost is active;
			// the disappearing probe cgroup has no baseline that must remain live.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), a.timeout)
			if cleanupErr := transport.stopCapabilityProbe(cleanupCtx, unit); cleanupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("stop transient capability probe %s: %w", unit, cleanupErr))
			}
			cleanupCancel()
			if a.ownsUnitName(unit) {
				cleanupCtx, cleanupCancel = context.WithTimeout(context.Background(), a.timeout)
				if cleanupErr := a.ReconcileOwned(cleanupCtx); cleanupErr != nil {
					retErr = errors.Join(retErr, fmt.Errorf("reconcile transient capability probe %s: %w", unit, cleanupErr))
				}
				cleanupCancel()
			}
			if paths, cleanupErr := a.unitFiles.mutablePaths(unit); cleanupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("inspect transient capability probe %s after cleanup: %w", unit, cleanupErr))
			} else if len(paths) != 0 {
				retErr = errors.Join(retErr, fmt.Errorf("transient capability probe %s left mutable unit files %v", unit, paths))
			}
			if a.ownsUnitName(unit) {
				retErr = errors.Join(retErr, fmt.Errorf("transient capability probe %s left a durable property lease", unit))
			}
		}()
	}
	if err != nil {
		return capabilityProbeStartError(capability.feature, err)
	}
	callCtx, cancel = context.WithTimeout(ctx, a.timeout)
	snapshot, err := a.readUnit(callCtx, listed.name, listed.objectPath)
	cancel()
	if err != nil {
		return fmt.Errorf("read transient capability probe %s through the production adapter: %w", unit, err)
	}
	if err := a.verifier.preflight(snapshot, capability.requiredAssignments); err != nil {
		return requiredCapabilityError(capability.feature, capability.controller, capability.interfaceName, capability.requiredAssignments[0].name, err)
	}
	applied, err := a.Apply(ctx, snapshot.Identity, capability.probeAssignments)
	if err != nil {
		return fmt.Errorf("apply %s startup capability probe: %w", capability.feature, err)
	}
	if _, err := a.ConfirmApplied(ctx, applied.Identity, capability.probeAssignments); err != nil {
		return fmt.Errorf("confirm %s startup capability probe: %w", capability.feature, err)
	}
	return nil
}

func (a *Adapter) cleanInterruptedCapabilityProbes(ctx context.Context, transport startupCapabilityTransport) error {
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	listed, err := a.transport.listUserSlices(callCtx)
	cancel()
	if err != nil {
		return classifyTransportError("startup_capabilities", "", err)
	}
	var cleanupErrors []error
	stopped := false
	for _, candidate := range listed {
		if !isCapabilityProbeUnit(candidate.name) {
			continue
		}
		stopped = true
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), a.timeout)
		if cleanupErr := transport.stopCapabilityProbe(cleanupCtx, candidate.name); cleanupErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("stop interrupted capability probe %s: %w", candidate.name, cleanupErr))
		}
		cleanupCancel()
	}
	if stopped {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), a.timeout)
		if cleanupErr := a.ReconcileOwned(cleanupCtx); cleanupErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("reconcile interrupted capability probes: %w", cleanupErr))
		}
		cleanupCancel()
	}
	if err := errors.Join(cleanupErrors...); err != nil {
		return &AdapterError{Reason: ReasonCapabilityProbe, Operation: "startup_capabilities", Err: err}
	}
	return nil
}

func (a *Adapter) validateStartupParent(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	listed, err := a.transport.listUserSlices(callCtx)
	if err != nil {
		return classifyTransportError("startup_capabilities", parentUserSlice, err)
	}
	var parent *listedUnit
	for index := range listed {
		candidate := &listed[index]
		if candidate.name != parentUserSlice {
			continue
		}
		if parent != nil {
			return malformedReply("startup_capabilities", parentUserSlice, "duplicate parent slice in ListUnitsByPatterns reply")
		}
		parent = candidate
	}
	if parent == nil {
		return &AdapterError{Reason: ReasonUnitMissing, Operation: "startup_capabilities", Unit: parentUserSlice, Err: fmt.Errorf("active parent slice was not returned by systemd")}
	}
	if parent.loadState != "loaded" || parent.activeState != "active" || !strings.HasPrefix(parent.objectPath, "/org/freedesktop/systemd1/unit/") {
		return malformedReply("startup_capabilities", parentUserSlice, "parent slice has an invalid state or object path")
	}
	if _, err := a.readUnit(callCtx, parent.name, parent.objectPath); err != nil {
		return fmt.Errorf("validate parent slice through the production adapter: %w", err)
	}
	return nil
}

func (a *Adapter) ownsUnitName(unit string) bool {
	for _, identity := range a.OwnedUnits() {
		if identity.Name == unit {
			return true
		}
	}
	return false
}

func requiredCapabilityError(feature, controller, interfaceName string, property PropertyName, err error) error {
	return &AdapterError{
		Reason: ReasonRequiredCapability, Operation: "startup_capabilities", Property: property,
		Err: fmt.Errorf("enabled feature %s requires controller %q interface %q: %w", feature, controller, interfaceName, err),
	}
}

func capabilityProbePreparationError(feature string, err error) error {
	return &AdapterError{
		Reason: ReasonCapabilityProbe, Operation: "startup_capabilities",
		Err: fmt.Errorf("enabled feature %s could not prepare its startup capability probe: %w", feature, err),
	}
}

func capabilityProbeStartError(feature string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("enabled feature %s could not start capability probe executable %q: %w",
			feature, capabilityProbeExecutable, classifyTransportError("startup_capabilities", "", err))
	}
	var adapterErr *AdapterError
	if errors.As(err, &adapterErr) && (adapterErr.Reason == ReasonBusUnavailable || adapterErr.Reason == ReasonTimeout) {
		return fmt.Errorf("enabled feature %s could not start capability probe executable %q: %w",
			feature, capabilityProbeExecutable, err)
	}
	return fmt.Errorf("enabled feature %s could not start capability probe executable %q: %w",
		feature, capabilityProbeExecutable, &AdapterError{Reason: ReasonCapabilityProbe, Operation: "startup_capabilities", Err: err})
}

func newCapabilityProbeUnit() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate capability probe identity: %w", err)
	}
	// Place the probe directly under user.slice so it verifies and materializes
	// the controller interface in the same hierarchy used by native enforcement.
	// The single separator names the existing parent and creates no implicit slice.
	return capabilityProbeUnitPrefix + hex.EncodeToString(suffix[:]) + ".slice", nil
}

const capabilityProbeUnitPrefix = "user-resmancapprobe"

func isCapabilityProbeUnit(unit string) bool {
	if !strings.HasPrefix(unit, capabilityProbeUnitPrefix) || !strings.HasSuffix(unit, ".slice") {
		return false
	}
	suffix := strings.TrimSuffix(strings.TrimPrefix(unit, capabilityProbeUnitPrefix), ".slice")
	if len(suffix) != 16 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func capabilityProbeServiceUnit(parent string) string {
	base := strings.TrimSuffix(strings.TrimPrefix(parent, "user-"), ".slice")
	return base + ".service"
}
