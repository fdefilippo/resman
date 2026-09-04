// Package systemdunit provides the narrow systemd D-Bus boundary used to
// inspect and control systemd-owned user slices without moving processes.
package systemdunit

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	// DefaultCallTimeout bounds every D-Bus operation performed by the adapter.
	DefaultCallTimeout = 5 * time.Second
	// SystemdUnset is systemd's normalized uint64 sentinel for an unset limit or weight.
	SystemdUnset = uint64(math.MaxUint64)
)

// PropertyName is an approved scalar systemd resource-control property.
type PropertyName string

const (
	PropertyCPUWeight          PropertyName = "CPUWeight"
	PropertyCPUQuotaPerSecUSec PropertyName = "CPUQuotaPerSecUSec"
	PropertyCPUQuotaPeriodUSec PropertyName = "CPUQuotaPeriodUSec"
	PropertyMemoryHigh         PropertyName = "MemoryHigh"
	PropertyMemoryMax          PropertyName = "MemoryMax"
	PropertyMemorySwapMax      PropertyName = "MemorySwapMax"
	PropertyIOWeight           PropertyName = "IOWeight"
)

var approvedScalarProperties = map[PropertyName]struct{}{
	PropertyCPUWeight:          {},
	PropertyCPUQuotaPerSecUSec: {},
	PropertyCPUQuotaPeriodUSec: {},
	PropertyMemoryHigh:         {},
	PropertyMemoryMax:          {},
	PropertyMemorySwapMax:      {},
	PropertyIOWeight:           {},
}

// PropertyAssignment is one validated runtime-only resource property mutation.
type PropertyAssignment struct {
	name  PropertyName
	value uint64
}

// NewPropertyAssignment validates an assignment against the positive property allowlist.
func NewPropertyAssignment(name PropertyName, value uint64) (PropertyAssignment, error) {
	if _, ok := approvedScalarProperties[name]; !ok {
		return PropertyAssignment{}, &AdapterError{
			Reason:   ReasonPropertyNotAllowed,
			Property: name,
			Err:      fmt.Errorf("property is outside the approved scalar resource-control set"),
		}
	}
	if err := validatePropertyValue(name, value); err != nil {
		return PropertyAssignment{}, &AdapterError{
			Reason:   ReasonInvalidValue,
			Property: name,
			Err:      err,
		}
	}
	return PropertyAssignment{name: name, value: value}, nil
}

// Name returns the approved property name.
func (a PropertyAssignment) Name() PropertyName { return a.name }

// Value returns the normalized uint64 D-Bus value.
func (a PropertyAssignment) Value() uint64 { return a.value }

func validatePropertyValue(name PropertyName, value uint64) error {
	switch name {
	case PropertyCPUWeight, PropertyIOWeight:
		if value != SystemdUnset && (value < 1 || value > 10_000) {
			return fmt.Errorf("weight %d is outside 1..10000 or the unset sentinel", value)
		}
	case PropertyCPUQuotaPerSecUSec:
		if value != SystemdUnset && value == 0 {
			return fmt.Errorf("finite CPU quota per second must be positive")
		}
	case PropertyCPUQuotaPeriodUSec:
		if value != SystemdUnset && (value < 1_000 || value > 1_000_000) {
			return fmt.Errorf("CPU quota period %d is outside 1000..1000000 microseconds", value)
		}
	case PropertyMemoryHigh, PropertyMemoryMax, PropertyMemorySwapMax:
		// Every uint64 value is a valid normalized byte limit; MaxUint64 means infinity.
	default:
		return fmt.Errorf("property %q is not approved", name)
	}
	return nil
}

// PropertySet is an immutable normalized resource-property snapshot.
type PropertySet struct {
	values map[PropertyName]uint64
}

func newPropertySet(values map[PropertyName]uint64) PropertySet {
	copyValues := make(map[PropertyName]uint64, len(values))
	for name, value := range values {
		copyValues[name] = value
	}
	return PropertySet{values: copyValues}
}

// Value returns one normalized property value.
func (s PropertySet) Value(name PropertyName) (uint64, bool) {
	value, ok := s.values[name]
	return value, ok
}

// Assignments returns a sorted defensive copy of the property snapshot.
func (s PropertySet) Assignments() []PropertyAssignment {
	result := make([]PropertyAssignment, 0, len(s.values))
	for name, value := range s.values {
		result = append(result, PropertyAssignment{name: name, value: value})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result
}

// UnitIdentity identifies one particular lifetime of a loaded systemd unit.
// ObjectPath alone is reusable; InvocationID and ControlGroupID detect recreation.
type UnitIdentity struct {
	Name           string
	ObjectPath     string
	InvocationID   [16]byte
	ControlGroupID uint64
}

// InvocationIDString returns the canonical lower-case systemd invocation ID.
func (i UnitIdentity) InvocationIDString() string { return hex.EncodeToString(i.InvocationID[:]) }

// UnitSnapshot is one authoritative systemd view of an active slice.
type UnitSnapshot struct {
	Identity     UnitIdentity
	ControlGroup string
	Properties   PropertySet
}

// UserSliceSnapshot binds an active user slice to its numeric UID.
type UserSliceSnapshot struct {
	UID  uint32
	Unit UnitSnapshot
}

// TopologySnapshot contains the active systemd user hierarchy visible at one discovery pass.
type TopologySnapshot struct {
	Parent UnitSnapshot
	Users  []UserSliceSnapshot
}

// PropertyLease records the original value and the last value applied by ResMan.
type PropertyLease struct {
	Property    PropertyName
	Baseline    uint64
	LastApplied uint64
}

// PropertyConflict reports an external mutation that ResMan deliberately preserved.
type PropertyConflict struct {
	Property    PropertyName
	LastApplied uint64
	Current     uint64
}

// RestoreResult reports restored properties and external conflicts independently.
type RestoreResult struct {
	Restored  []PropertyName
	Conflicts []PropertyConflict
}
