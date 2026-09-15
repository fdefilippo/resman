package systemdunit

import (
	"fmt"
	"sort"
)

const (
	minimumIODeviceWeight = uint64(1)
	maximumIODeviceWeight = uint64(1_000)
)

// IODeviceWeightMechanism identifies the qualified kernel consumer for one
// systemd IODeviceWeight device tuple.
type IODeviceWeightMechanism string

const (
	IODeviceWeightMechanismBFQ    IODeviceWeightMechanism = "bfq"
	IODeviceWeightMechanismIOCost IODeviceWeightMechanism = "io_cost"
)

// IODeviceWeightRequest is one typed operator-domain weight and its qualified
// kernel delivery mechanism. Weight uses ResMan's injective 1..1000 domain.
type IODeviceWeightRequest struct {
	Path      string
	Weight    uint64
	Mechanism IODeviceWeightMechanism
}

type ioDeviceWeightTarget struct {
	path      string
	mechanism IODeviceWeightMechanism
}

// ioDeviceWeightReset is the durable, exact kernel tuple that must be removed
// after systemd has acknowledged removal of the corresponding D-Bus entry.
type ioDeviceWeightReset struct {
	path      string
	device    string
	mechanism IODeviceWeightMechanism
	expected  uint64
}

// NewIODeviceWeightAssignment validates, canonicalizes and converts typed
// per-device weights into systemd's native IODeviceWeight a(st) value.
func NewIODeviceWeightAssignment(requests []IODeviceWeightRequest) (PropertyAssignment, error) {
	if len(requests) == 0 {
		return PropertyAssignment{}, &AdapterError{Reason: ReasonInvalidValue, Property: PropertyIODeviceWeight,
			Err: fmt.Errorf("at least one device weight is required")}
	}
	canonical := append([]IODeviceWeightRequest(nil), requests...)
	sort.Slice(canonical, func(left, right int) bool { return canonical[left].Path < canonical[right].Path })
	limits := make([]DeviceLimit, len(canonical))
	targets := make([]ioDeviceWeightTarget, len(canonical))
	for index, request := range canonical {
		if request.Weight < minimumIODeviceWeight || request.Weight > maximumIODeviceWeight {
			return PropertyAssignment{}, &AdapterError{Reason: ReasonInvalidValue, Property: PropertyIODeviceWeight,
				Err: fmt.Errorf("device weight %d for %s is outside %d..%d", request.Weight, request.Path, minimumIODeviceWeight, maximumIODeviceWeight)}
		}
		if !validIODeviceWeightMechanism(request.Mechanism) {
			return PropertyAssignment{}, &AdapterError{Reason: ReasonInvalidValue, Property: PropertyIODeviceWeight,
				Err: fmt.Errorf("device %s has unsupported mechanism %q", request.Path, request.Mechanism)}
		}
		limits[index] = DeviceLimit{Path: request.Path, Value: systemdIODeviceWeight(request.Weight, request.Mechanism)}
		targets[index] = ioDeviceWeightTarget{path: request.Path, mechanism: request.Mechanism}
	}
	if err := validateDeviceLimits(limits); err != nil {
		return PropertyAssignment{}, &AdapterError{Reason: ReasonInvalidValue, Property: PropertyIODeviceWeight, Err: err}
	}
	return PropertyAssignment{name: PropertyIODeviceWeight, value: devicePropertyValue(limits), ioDeviceWeightTargets: targets}, nil
}

// IODeviceWeightRequests returns the normalized typed requests represented by
// this assignment. It returns nil for every other property.
func (a PropertyAssignment) IODeviceWeightRequests() []IODeviceWeightRequest {
	if a.name != PropertyIODeviceWeight || len(a.ioDeviceWeightTargets) != len(a.value.devices) {
		return nil
	}
	result := make([]IODeviceWeightRequest, len(a.value.devices))
	for index, target := range a.ioDeviceWeightTargets {
		result[index] = IODeviceWeightRequest{
			Path: target.path, Weight: kernelIODeviceWeight(a.value.devices[index].Value, target.mechanism), Mechanism: target.mechanism,
		}
	}
	return result
}

func systemdIODeviceWeight(weight uint64, mechanism IODeviceWeightMechanism) uint64 {
	if mechanism == IODeviceWeightMechanismBFQ && weight > 100 {
		return 100 + 11*(weight-100)
	}
	return weight
}

func kernelIODeviceWeight(systemdValue uint64, mechanism IODeviceWeightMechanism) uint64 {
	if mechanism == IODeviceWeightMechanismBFQ {
		return bfqWeight(systemdValue)
	}
	return systemdValue
}

func validIODeviceWeightMechanism(mechanism IODeviceWeightMechanism) bool {
	return mechanism == IODeviceWeightMechanismBFQ || mechanism == IODeviceWeightMechanismIOCost
}

func cloneIODeviceWeightTargets(targets []ioDeviceWeightTarget) []ioDeviceWeightTarget {
	return append([]ioDeviceWeightTarget(nil), targets...)
}

func cloneIODeviceWeightResets(resets []ioDeviceWeightReset) []ioDeviceWeightReset {
	return append([]ioDeviceWeightReset(nil), resets...)
}

func ioDeviceWeightTargetByPath(targets []ioDeviceWeightTarget, path string) (ioDeviceWeightTarget, bool) {
	for _, target := range targets {
		if target.path == path {
			return target, true
		}
	}
	return ioDeviceWeightTarget{}, false
}

func ioDeviceWeightTargetsEqual(left, right []ioDeviceWeightTarget) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func ioDeviceWeightTargetMechanismsCompatible(left, right []ioDeviceWeightTarget) bool {
	leftByPath := make(map[string]IODeviceWeightMechanism, len(left))
	for _, target := range left {
		leftByPath[target.path] = target.mechanism
	}
	for _, target := range right {
		if mechanism, present := leftByPath[target.path]; present && mechanism != target.mechanism {
			return false
		}
	}
	return true
}

func validateIODeviceWeightAssignment(assignment PropertyAssignment) error {
	if assignment.name != PropertyIODeviceWeight {
		return nil
	}
	if len(assignment.ioDeviceWeightTargets) == 0 {
		return fmt.Errorf("typed mechanism context must identify at least one owned device")
	}
	if err := validateIODeviceWeightValues(assignment.value.devices); err != nil {
		return err
	}
	for index, target := range assignment.ioDeviceWeightTargets {
		if !validIODeviceWeightMechanism(target.mechanism) || !validAbsolutePath(target.path) ||
			(index > 0 && assignment.ioDeviceWeightTargets[index-1].path >= target.path) {
			return fmt.Errorf("typed mechanism context has an invalid or non-canonical target at index %d", index)
		}
	}
	return nil
}

func validateIODeviceWeightValues(values []DeviceLimit) error {
	for _, value := range values {
		if value.Value > 10_000 {
			return fmt.Errorf("systemd device weight %d for %s is outside 1..10000", value.Value, value.Path)
		}
	}
	return nil
}
