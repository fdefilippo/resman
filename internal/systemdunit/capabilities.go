package systemdunit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// StartupRequirements names the optional resource features enabled by the
// operator. CPU quota is always mandatory for systemd-native enforcement.
type StartupRequirements struct {
	Memory         bool
	IO             bool
	IODeviceFilter string
}

type startupCapability struct {
	feature       string
	controller    string
	interfaceName string
	assignment    PropertyAssignment
}

func (a *Adapter) requireStartupCapabilities(ctx context.Context, requirements StartupRequirements) error {
	transport, ok := a.transport.(startupCapabilityTransport)
	if !ok {
		return &AdapterError{
			Reason: ReasonRequiredCapability, Operation: "startup_capabilities",
			Err: fmt.Errorf("systemd transport cannot create an authoritative capability probe"),
		}
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
	cpu, err := NewPropertyAssignment(PropertyCPUQuotaPerSecUSec, 1_000_000)
	if err != nil {
		return nil, err
	}
	result := []startupCapability{{
		feature: "CPU limiting", controller: "cpu", interfaceName: "cpu.max", assignment: cpu,
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
				feature: "RAM limiting", controller: "memory", interfaceName: candidate.interfaceName, assignment: assignment,
			})
		}
	}
	if requirements.IO {
		devices, resolveErr := ResolveBlockDevices(requirements.IODeviceFilter)
		if resolveErr != nil {
			return nil, requiredCapabilityError("I/O limiting", "io", "io.max", PropertyIOReadBandwidthMax, resolveErr)
		}
		assignment, assignmentErr := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: devices[0], Value: 1 << 30}})
		if assignmentErr != nil {
			return nil, requiredCapabilityError("I/O limiting", "io", "io.max", PropertyIOReadBandwidthMax, assignmentErr)
		}
		result = append(result, startupCapability{
			feature: "I/O limiting", controller: "io", interfaceName: "io.max", assignment: assignment,
		})
	}
	return result, nil
}

func (a *Adapter) probeStartupCapability(ctx context.Context, transport startupCapabilityTransport, capability startupCapability) (retErr error) {
	unit, err := newCapabilityProbeUnit()
	if err != nil {
		return requiredCapabilityError(capability.feature, capability.controller, capability.interfaceName, capability.assignment.name, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	controlGroup, err := transport.startCapabilityProbe(callCtx, unit, []PropertyAssignment{capability.assignment})
	cancel()
	if err != nil {
		return requiredCapabilityError(capability.feature, capability.controller, capability.interfaceName, capability.assignment.name, err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), a.timeout)
		defer cleanupCancel()
		if cleanupErr := transport.stopCapabilityProbe(cleanupCtx, unit); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("stop transient capability probe %s: %w", unit, cleanupErr))
		}
	}()
	snapshot := UnitSnapshot{Identity: UnitIdentity{Name: unit}, ControlGroup: controlGroup}
	if err := a.verifier.preflight(snapshot, []PropertyAssignment{capability.assignment}); err != nil {
		return requiredCapabilityError(capability.feature, capability.controller, capability.interfaceName, capability.assignment.name, err)
	}
	return nil
}

func requiredCapabilityError(feature, controller, interfaceName string, property PropertyName, err error) error {
	return &AdapterError{
		Reason: ReasonRequiredCapability, Operation: "startup_capabilities", Property: property,
		Err: fmt.Errorf("enabled feature %s requires controller %q interface %q: %w", feature, controller, interfaceName, err),
	}
}

func newCapabilityProbeUnit() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate capability probe identity: %w", err)
	}
	return "resman-capability-probe-" + hex.EncodeToString(suffix[:]) + ".slice", nil
}
