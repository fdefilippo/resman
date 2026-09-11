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
	Memory bool
	IO     bool
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
	var identity UnitIdentity
	if started {
		defer func() {
			if identity.Name != "" && len(a.Leases(identity)) != 0 {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), a.timeout)
				if _, cleanupErr := a.Restore(cleanupCtx, identity); cleanupErr != nil {
					retErr = errors.Join(retErr, fmt.Errorf("restore transient capability probe %s: %w", unit, cleanupErr))
				}
				cleanupCancel()
			}
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
	identity = snapshot.Identity
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
	result, err := a.Restore(ctx, applied.Identity)
	if err != nil {
		return fmt.Errorf("restore %s startup capability probe: %w", capability.feature, err)
	}
	if len(result.Restored) != len(capability.probeAssignments) || len(a.Leases(applied.Identity)) != 0 {
		return &AdapterError{Reason: ReasonCapabilityProbe, Operation: "startup_capabilities", Unit: unit,
			Err: fmt.Errorf("capability probe restoration did not release every temporary property lease")}
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
	return &AdapterError{
		Reason: ReasonCapabilityProbe, Operation: "startup_capabilities",
		Err: fmt.Errorf("enabled feature %s could not start capability probe executable %q: %w", feature, capabilityProbeExecutable, err),
	}
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
