// Package systemdunit provides the narrow systemd D-Bus boundary used to
// inspect and control systemd-owned user slices without moving processes.
package systemdunit

import (
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultCallTimeout bounds every D-Bus operation performed by the adapter.
	DefaultCallTimeout = 5 * time.Second
	// SystemdUnset is systemd's normalized uint64 sentinel for an unset limit or weight.
	SystemdUnset = uint64(math.MaxUint64)
)

// PropertyName is an approved systemd resource-control property.
type PropertyName string

const (
	PropertyCPUWeight           PropertyName = "CPUWeight"
	PropertyCPUQuotaPerSecUSec  PropertyName = "CPUQuotaPerSecUSec"
	PropertyCPUQuotaPeriodUSec  PropertyName = "CPUQuotaPeriodUSec"
	PropertyMemoryHigh          PropertyName = "MemoryHigh"
	PropertyMemoryMax           PropertyName = "MemoryMax"
	PropertyMemorySwapMax       PropertyName = "MemorySwapMax"
	PropertyIOWeight            PropertyName = "IOWeight"
	PropertyIOReadBandwidthMax  PropertyName = "IOReadBandwidthMax"
	PropertyIOWriteBandwidthMax PropertyName = "IOWriteBandwidthMax"
	PropertyIOReadIOPSMax       PropertyName = "IOReadIOPSMax"
	PropertyIOWriteIOPSMax      PropertyName = "IOWriteIOPSMax"
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

var approvedDeviceProperties = map[PropertyName]struct{}{
	PropertyIOReadBandwidthMax:  {},
	PropertyIOWriteBandwidthMax: {},
	PropertyIOReadIOPSMax:       {},
	PropertyIOWriteIOPSMax:      {},
}

func isApprovedProperty(name PropertyName) bool {
	if _, ok := approvedScalarProperties[name]; ok {
		return true
	}
	_, ok := approvedDeviceProperties[name]
	return ok
}

// DeviceLimit is one normalized systemd block-device limit.
type DeviceLimit struct {
	Path  string
	Value uint64
}

type propertyValue struct {
	scalar  uint64
	devices []DeviceLimit
}

func scalarPropertyValue(value uint64) propertyValue { return propertyValue{scalar: value} }

func devicePropertyValue(values []DeviceLimit) propertyValue {
	return propertyValue{devices: cloneDeviceLimits(values)}
}

func cloneDeviceLimits(values []DeviceLimit) []DeviceLimit {
	if values == nil {
		return nil
	}
	return append([]DeviceLimit(nil), values...)
}

func (v propertyValue) equal(other propertyValue, deviceProperty bool) bool {
	if !deviceProperty {
		return v.scalar == other.scalar
	}
	if len(v.devices) != len(other.devices) {
		return false
	}
	for index := range v.devices {
		if v.devices[index] != other.devices[index] {
			return false
		}
	}
	return true
}

func propertyValuesEqual(name PropertyName, left, right propertyValue) bool {
	_, deviceProperty := approvedDeviceProperties[name]
	return left.equal(right, deviceProperty)
}

func clonePropertyValue(name PropertyName, value propertyValue) propertyValue {
	if _, deviceProperty := approvedDeviceProperties[name]; deviceProperty {
		return devicePropertyValue(value.devices)
	}
	return scalarPropertyValue(value.scalar)
}

func newPropertyLeaseState(name PropertyName, baseline propertyValue) propertyLeaseState {
	baseline = clonePropertyValue(name, baseline)
	return propertyLeaseState{
		lease:       publicPropertyLease(name, baseline, baseline),
		baseline:    baseline,
		lastApplied: clonePropertyValue(name, baseline),
	}
}

func publicPropertyLease(name PropertyName, baseline, lastApplied propertyValue) PropertyLease {
	lease := PropertyLease{Property: name}
	if _, deviceProperty := approvedDeviceProperties[name]; deviceProperty {
		lease.BaselineDeviceLimits = cloneDeviceLimits(baseline.devices)
		lease.LastAppliedDeviceLimits = cloneDeviceLimits(lastApplied.devices)
		return lease
	}
	lease.Baseline = baseline.scalar
	lease.LastApplied = lastApplied.scalar
	return lease
}

func publicPropertyConflict(name PropertyName, lastApplied, current propertyValue) PropertyConflict {
	conflict := PropertyConflict{Property: name}
	if _, deviceProperty := approvedDeviceProperties[name]; deviceProperty {
		conflict.LastAppliedDeviceLimits = cloneDeviceLimits(lastApplied.devices)
		conflict.CurrentDeviceLimits = cloneDeviceLimits(current.devices)
		return conflict
	}
	conflict.LastApplied = lastApplied.scalar
	conflict.Current = current.scalar
	return conflict
}

func formatPropertyValue(name PropertyName, value propertyValue) string {
	if _, deviceProperty := approvedDeviceProperties[name]; deviceProperty {
		return fmt.Sprintf("%v", value.devices)
	}
	return fmt.Sprintf("%d", value.scalar)
}

func parseDevicePropertyValue(raw any) ([]DeviceLimit, error) {
	if raw == nil {
		return nil, fmt.Errorf("value is absent")
	}
	value := reflect.ValueOf(raw)
	if value.Kind() != reflect.Slice && value.Kind() != reflect.Array {
		return nil, fmt.Errorf("value has type %T, expected an array of device tuples", raw)
	}
	result := make([]DeviceLimit, 0, value.Len())
	for index := 0; index < value.Len(); index++ {
		path, limit, err := parseDeviceLimitTuple(value.Index(index))
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", index, err)
		}
		result = append(result, DeviceLimit{Path: path, Value: limit})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	if err := validateDeviceLimits(result); err != nil {
		return nil, err
	}
	return result, nil
}

func parseDeviceLimitTuple(item reflect.Value) (string, uint64, error) {
	for item.IsValid() && item.Kind() == reflect.Interface {
		if item.IsNil() {
			return "", 0, fmt.Errorf("value is nil, expected (string,uint64)")
		}
		item = item.Elem()
	}
	if !item.IsValid() {
		return "", 0, fmt.Errorf("value is invalid, expected (string,uint64)")
	}
	var pathValue, limitValue reflect.Value
	switch item.Kind() {
	case reflect.Struct:
		if item.NumField() != 2 {
			return "", 0, fmt.Errorf("value has type %s, expected (string,uint64)", item.Type())
		}
		pathValue, limitValue = item.Field(0), item.Field(1)
	case reflect.Slice, reflect.Array:
		if item.Len() != 2 {
			return "", 0, fmt.Errorf("value has type %s and length %d, expected (string,uint64)", item.Type(), item.Len())
		}
		pathValue, limitValue = item.Index(0), item.Index(1)
	default:
		return "", 0, fmt.Errorf("value has type %s, expected (string,uint64)", item.Type())
	}
	for pathValue.IsValid() && pathValue.Kind() == reflect.Interface {
		pathValue = pathValue.Elem()
	}
	for limitValue.IsValid() && limitValue.Kind() == reflect.Interface {
		limitValue = limitValue.Elem()
	}
	if !pathValue.IsValid() || pathValue.Kind() != reflect.String || !limitValue.IsValid() || limitValue.Kind() != reflect.Uint64 {
		return "", 0, fmt.Errorf("value has type %s, expected (string,uint64)", item.Type())
	}
	return pathValue.String(), limitValue.Uint(), nil
}

// PropertyAssignment is one validated runtime-only resource property mutation.
type PropertyAssignment struct {
	name  PropertyName
	value propertyValue
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
	return PropertyAssignment{name: name, value: scalarPropertyValue(value)}, nil
}

// NewDevicePropertyAssignment validates and canonicalizes one per-device property.
func NewDevicePropertyAssignment(name PropertyName, values []DeviceLimit) (PropertyAssignment, error) {
	if _, ok := approvedDeviceProperties[name]; !ok {
		return PropertyAssignment{}, &AdapterError{
			Reason: ReasonPropertyNotAllowed, Property: name,
			Err: fmt.Errorf("property is outside the approved per-device resource-control set"),
		}
	}
	canonical := cloneDeviceLimits(values)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Path < canonical[j].Path })
	if err := validateDeviceLimits(canonical); err != nil {
		return PropertyAssignment{}, &AdapterError{Reason: ReasonInvalidValue, Property: name, Err: err}
	}
	return PropertyAssignment{name: name, value: devicePropertyValue(canonical)}, nil
}

// Name returns the approved property name.
func (a PropertyAssignment) Name() PropertyName { return a.name }

// Value returns the normalized uint64 D-Bus value.
func (a PropertyAssignment) Value() uint64 { return a.value.scalar }

// DeviceLimits returns a defensive copy of a per-device assignment.
func (a PropertyAssignment) DeviceLimits() []DeviceLimit { return cloneDeviceLimits(a.value.devices) }

// NewCPUQuotaAssignmentsFromCgroupMax converts one exact cgroup v2 quota and
// period into systemd's normalized per-second quota plus explicit period.
func NewCPUQuotaAssignmentsFromCgroupMax(quota, period uint64) ([]PropertyAssignment, error) {
	if quota == 0 || period == 0 || 1_000_000%period != 0 {
		return nil, &AdapterError{
			Reason: ReasonInvalidValue, Operation: "convert_cpu_quota",
			Err: fmt.Errorf("cgroup quota %d period %d is not exactly representable by systemd", quota, period),
		}
	}
	factor := uint64(1_000_000) / period
	if quota > math.MaxUint64/factor {
		return nil, &AdapterError{
			Reason: ReasonInvalidValue, Operation: "convert_cpu_quota",
			Err: fmt.Errorf("cgroup quota %d period %d overflows systemd per-second quota", quota, period),
		}
	}
	perSecond, err := NewPropertyAssignment(PropertyCPUQuotaPerSecUSec, quota*factor)
	if err != nil {
		return nil, err
	}
	configuredPeriod, err := NewPropertyAssignment(PropertyCPUQuotaPeriodUSec, period)
	if err != nil {
		return nil, err
	}
	return []PropertyAssignment{perSecond, configuredPeriod}, nil
}

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
	values map[PropertyName]propertyValue
}

func newPropertySet(values map[PropertyName]propertyValue) PropertySet {
	copyValues := make(map[PropertyName]propertyValue, len(values))
	for name, value := range values {
		if _, deviceProperty := approvedDeviceProperties[name]; deviceProperty {
			copyValues[name] = devicePropertyValue(value.devices)
		} else {
			copyValues[name] = scalarPropertyValue(value.scalar)
		}
	}
	return PropertySet{values: copyValues}
}

// Value returns one normalized property value.
func (s PropertySet) Value(name PropertyName) (uint64, bool) {
	value, ok := s.values[name]
	if !ok {
		return 0, false
	}
	if _, deviceProperty := approvedDeviceProperties[name]; deviceProperty {
		return 0, false
	}
	return value.scalar, true
}

// DeviceLimits returns one normalized per-device property value.
func (s PropertySet) DeviceLimits(name PropertyName) ([]DeviceLimit, bool) {
	value, ok := s.values[name]
	if !ok {
		return nil, false
	}
	if _, deviceProperty := approvedDeviceProperties[name]; !deviceProperty {
		return nil, false
	}
	return cloneDeviceLimits(value.devices), true
}

func (s PropertySet) propertyValue(name PropertyName) (propertyValue, bool) {
	value, ok := s.values[name]
	if !ok {
		return propertyValue{}, false
	}
	if _, deviceProperty := approvedDeviceProperties[name]; deviceProperty {
		return devicePropertyValue(value.devices), true
	}
	return scalarPropertyValue(value.scalar), true
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

func validateDeviceLimits(values []DeviceLimit) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !filepath.IsAbs(value.Path) || filepath.Clean(value.Path) != value.Path || strings.Contains(value.Path, "//") {
			return fmt.Errorf("device path %q is not a canonical absolute path", value.Path)
		}
		if seen[value.Path] {
			return fmt.Errorf("device path %q is duplicated", value.Path)
		}
		seen[value.Path] = true
		if value.Value == 0 || value.Value == SystemdUnset {
			return fmt.Errorf("device limit for %s must be finite and positive", value.Path)
		}
	}
	return nil
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

// IsParentUserSlice reports whether this identity names the authoritative
// parent of the flat systemd user-slice topology.
func (i UnitIdentity) IsParentUserSlice() bool { return i.Name == parentUserSlice }

// UnitSnapshot is one authoritative systemd view of an active slice.
type UnitSnapshot struct {
	Identity     UnitIdentity
	ControlGroup string
	Properties   PropertySet
	unitFiles    unitFileSnapshot
}

type unitFileSnapshot struct {
	fragmentPath string
	dropInPaths  []string
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
	Property                PropertyName
	Baseline                uint64
	LastApplied             uint64
	BaselineDeviceLimits    []DeviceLimit
	LastAppliedDeviceLimits []DeviceLimit
}

// Active reports whether the last applied value still differs from the baseline.
func (l PropertyLease) Active() bool {
	if _, deviceProperty := approvedDeviceProperties[l.Property]; deviceProperty {
		return !propertyValuesEqual(l.Property, devicePropertyValue(l.BaselineDeviceLimits), devicePropertyValue(l.LastAppliedDeviceLimits))
	}
	return l.Baseline != l.LastApplied
}

// Resource reports the independently governed resource for this property.
func (n PropertyName) Resource() (ResourceKind, bool) {
	switch n {
	case PropertyMemoryHigh, PropertyMemoryMax, PropertyMemorySwapMax:
		return ResourceMemory, true
	case PropertyIOWeight, PropertyIOReadBandwidthMax, PropertyIOWriteBandwidthMax, PropertyIOReadIOPSMax, PropertyIOWriteIOPSMax:
		return ResourceIO, true
	default:
		return "", false
	}
}

// IsCPU reports whether the property belongs to CPU scheduling or bandwidth.
func (n PropertyName) IsCPU() bool {
	switch n {
	case PropertyCPUWeight, PropertyCPUQuotaPerSecUSec, PropertyCPUQuotaPeriodUSec:
		return true
	default:
		return false
	}
}

// PropertyConflict reports an external mutation that ResMan deliberately preserved.
type PropertyConflict struct {
	Property                PropertyName
	LastApplied             uint64
	Current                 uint64
	LastAppliedDeviceLimits []DeviceLimit
	CurrentDeviceLimits     []DeviceLimit
}

// RestoreResult reports restored properties and external conflicts independently.
type RestoreResult struct {
	Restored  []PropertyName
	Conflicts []PropertyConflict
}

// LeaseRecoveryState is a bounded startup-reconciliation outcome.
type LeaseRecoveryState string

const (
	LeaseRecoveryReclaimed LeaseRecoveryState = "reclaimed"
	LeaseRecoveryOrphaned  LeaseRecoveryState = "orphaned_resman_footprint"
	LeaseRecoveryInactive  LeaseRecoveryState = "inactive_unit_cleaned"
	LeaseRecoveryPending   LeaseRecoveryState = "pending_inactive_unit"
	LeaseRecoveryConflict  LeaseRecoveryState = "external_property_conflict"
)

// LeaseRecoveryOutcome describes one durable unit lease after startup reconciliation.
type LeaseRecoveryOutcome struct {
	Unit  string
	State LeaseRecoveryState
}
