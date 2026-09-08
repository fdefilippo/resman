package systemdunit

import (
	"context"
	"fmt"
)

// StartupRequirements names the optional resource features enabled by the
// operator. CPU quota is always mandatory for systemd-native enforcement.
type StartupRequirements struct {
	Memory bool
	IO     bool
}

type startupCapability struct {
	feature       string
	controller    string
	interfaceName string
	property      PropertyName
}

func (a *Adapter) requireStartupCapabilities(ctx context.Context, requirements StartupRequirements) error {
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	listed, err := a.transport.listUserSlices(callCtx)
	if err != nil {
		return classifyTransportError("startup_capabilities", parentUserSlice, err)
	}
	var parent *listedUnit
	for index := range listed {
		if listed[index].name != parentUserSlice {
			continue
		}
		if parent != nil {
			return malformedReply("startup_capabilities", parentUserSlice, "duplicate active parent slice")
		}
		parent = &listed[index]
	}
	if parent == nil {
		return &AdapterError{
			Reason: ReasonUnitMissing, Operation: "startup_capabilities", Unit: parentUserSlice,
			Err: fmt.Errorf("active parent slice was not returned by systemd"),
		}
	}
	snapshot, err := a.readUnit(callCtx, parent.name, parent.objectPath)
	if err != nil {
		return err
	}

	required := []startupCapability{{
		feature: "CPU limiting", controller: "cpu", interfaceName: "cpu.max",
		property: PropertyCPUQuotaPerSecUSec,
	}}
	if requirements.Memory {
		required = append(required,
			startupCapability{feature: "RAM limiting", controller: "memory", interfaceName: "memory.high", property: PropertyMemoryHigh},
			startupCapability{feature: "RAM limiting", controller: "memory", interfaceName: "memory.max", property: PropertyMemoryMax},
		)
	}
	if requirements.IO {
		required = append(required, startupCapability{
			feature: "I/O limiting", controller: "io", interfaceName: "io.max",
			property: PropertyIOReadBandwidthMax,
		})
	}
	for _, capability := range required {
		assignment := PropertyAssignment{name: capability.property}
		if err := a.verifier.preflight(snapshot, []PropertyAssignment{assignment}); err != nil {
			return &AdapterError{
				Reason: ReasonRequiredCapability, Operation: "startup_capabilities", Unit: parentUserSlice,
				Property: capability.property,
				Err: fmt.Errorf("enabled feature %s requires controller %q interface %q: %w",
					capability.feature, capability.controller, capability.interfaceName, err),
			}
		}
	}
	return nil
}
