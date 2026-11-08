// Package blockiops defines the evidence used for block-operation decisions.
// It keeps attribution uncertainty separate from source unavailability.
package blockiops

import (
	"fmt"
	"math"
	"math/big"
	"time"
)

// Identity identifies one source, executable-policy and systemd-unit lifetime.
// A recreated unit cannot inherit the previous unit's decision baseline.
type Identity struct {
	SourceGeneration uint64
	PolicyGeneration uint64
	WatchGeneration  uint64
	UID              uint32
	InvocationID     string
	CgroupID         uint64
}

// Device identifies the kernel device at the quota's accounting layer.
type Device struct {
	Major uint32
	Minor uint32
	// Generation is the configured sysfs device lifetime, not a process identity.
	Generation uint64
}

// Partition contains cumulative counts from one accounting boundary. Unresolved
// operations may belong to either side of the executable-selection policy.
type Partition struct {
	Selected   uint64
	Excluded   uint64
	Unresolved uint64
}

func (p Partition) total() (uint64, error) {
	if p.Excluded > math.MaxUint64-p.Selected || p.Unresolved > math.MaxUint64-p.Selected-p.Excluded {
		return 0, fmt.Errorf("block-operation partition overflows uint64")
	}
	return p.Selected + p.Excluded + p.Unresolved, nil
}

// Counters contains directional counts for one device, with no syscall proxy.
type Counters struct {
	Read  Partition
	Write Partition
}

// Sample brackets a consistent counter read using a monotonic clock. The source
// must reject integrity failures before constructing this sample. An absent
// device does not constitute a measured zero for that device.
type Sample struct {
	Identity Identity
	Started  time.Time
	Finished time.Time
	Devices  map[Device]Counters
}

// RateBounds is a certified lower bound for activation and upper bound for
// release. The bounds deliberately have different measurement windows: counter
// reads have finite duration and must not claim an atomic clock instant.
type RateBounds struct {
	ActivationMinimum float64
	ReleaseMaximum    float64
}

// Interval is detached evidence for one device. Activation uses the outer read
// window, release the inner window. Read jitter can only weaken either proof.
type Interval struct {
	identity                                                 Identity
	device                                                   Device
	activationStart                                          time.Time
	activationEnd                                            time.Time
	releaseStart                                             time.Time
	releaseEnd                                               time.Time
	read                                                     RateBounds
	write                                                    RateBounds
	unresolved                                               bool
	readSelected, readPossible, writeSelected, writePossible uint64
}

// Between rejects mixed lifetimes, missing baselines, resets, overlapping reads
// and overflow. Callers must preserve the resulting error as unavailable coverage.
// A subsequent successful read establishes a fresh baseline, not a zero sample.
func Between(previous, current Sample, device Device) (Interval, error) {
	if err := validateSample(previous); err != nil {
		return Interval{}, fmt.Errorf("previous block-operation sample: %w", err)
	}
	if err := validateSample(current); err != nil {
		return Interval{}, fmt.Errorf("current block-operation sample: %w", err)
	}
	if previous.Identity != current.Identity {
		return Interval{}, fmt.Errorf("block-operation source, policy or unit lifetime changed")
	}
	if !previous.Finished.Before(current.Started) {
		return Interval{}, fmt.Errorf("block-operation reads overlap or do not advance")
	}
	before, ok := previous.Devices[device]
	if !ok {
		return Interval{}, fmt.Errorf("block-operation device %d:%d has no baseline", device.Major, device.Minor)
	}
	after, ok := current.Devices[device]
	if !ok {
		return Interval{}, fmt.Errorf("block-operation device %d:%d is unavailable", device.Major, device.Minor)
	}
	outer := current.Finished.Sub(previous.Started)
	inner := current.Started.Sub(previous.Finished)
	read, readAmbiguous, err := partitionRates(before.Read, after.Read, outer, inner)
	if err != nil {
		return Interval{}, fmt.Errorf("read operations: %w", err)
	}
	write, writeAmbiguous, err := partitionRates(before.Write, after.Write, outer, inner)
	if err != nil {
		return Interval{}, fmt.Errorf("write operations: %w", err)
	}
	return Interval{
		identity: current.Identity, device: device,
		activationStart: previous.Started, activationEnd: current.Finished,
		releaseStart: previous.Finished, releaseEnd: current.Started,
		read: read, write: write, unresolved: readAmbiguous || writeAmbiguous,
		readSelected:  after.Read.Selected - before.Read.Selected,
		readPossible:  after.Read.Selected - before.Read.Selected + after.Read.Unresolved - before.Read.Unresolved,
		writeSelected: after.Write.Selected - before.Write.Selected,
		writePossible: after.Write.Selected - before.Write.Selected + after.Write.Unresolved - before.Write.Unresolved,
	}, nil
}

func validateSample(sample Sample) error {
	id := sample.Identity
	if id.SourceGeneration == 0 || id.PolicyGeneration == 0 || id.WatchGeneration == 0 || id.InvocationID == "" || id.CgroupID == 0 {
		return fmt.Errorf("incomplete source, policy or unit identity")
	}
	if sample.Started.IsZero() || sample.Finished.Before(sample.Started) {
		return fmt.Errorf("invalid counter-read window")
	}
	return nil
}

func partitionRates(before, after Partition, outer, inner time.Duration) (RateBounds, bool, error) {
	if _, err := before.total(); err != nil {
		return RateBounds{}, false, err
	}
	if _, err := after.total(); err != nil {
		return RateBounds{}, false, err
	}
	if after.Selected < before.Selected || after.Excluded < before.Excluded || after.Unresolved < before.Unresolved {
		return RateBounds{}, false, fmt.Errorf("block-operation counter reset")
	}
	selected := after.Selected - before.Selected
	unresolved := after.Unresolved - before.Unresolved
	return RateBounds{
		ActivationMinimum: operationRate(selected, outer, false),
		ReleaseMaximum:    operationRate(selected+unresolved, inner, true),
	}, unresolved > 0, nil
}

func (i Interval) Identity() Identity { return i.identity }
func (i Interval) Device() Device     { return i.device }
func (i Interval) Read() RateBounds   { return i.read }
func (i Interval) Write() RateBounds  { return i.write }

// HasUnresolvedOperations distinguishes attribution uncertainty from missing
// source coverage. A complete upper bound can authorize release despite it.
func (i Interval) HasUnresolvedOperations() bool { return i.unresolved }

// ActivationWindow is the outer measurement interval proving the lower bound.
func (i Interval) ActivationWindow() (time.Time, time.Time) {
	return i.activationStart, i.activationEnd
}

// ReleaseWindow is the inner measurement interval proving the upper bound.
func (i Interval) ReleaseWindow() (time.Time, time.Time) {
	return i.releaseStart, i.releaseEnd
}

// DeviceBounds aggregates a device's participating users over common windows.
// Different devices remain separate accounting layers.
type DeviceBounds struct {
	Read, Write RateBounds
	Uncertain   bool
}

// Aggregate normalizes user intervals to a common outer activation window and
// inner release window before adding them. Adding rates measured over different
// windows would not certify the aggregate. No overlapping release window is an
// explicit coverage error, never a below-threshold result.
func Aggregate(intervals []Interval) (map[Device]DeviceBounds, error) {
	type window struct{ outerStart, outerEnd, innerStart, innerEnd time.Time }
	windows := make(map[Device]window)
	seen := make(map[struct {
		device   Device
		identity Identity
	}]bool)
	for _, i := range intervals {
		key := struct {
			device   Device
			identity Identity
		}{i.device, i.identity}
		if seen[key] {
			return nil, fmt.Errorf("duplicate IOPS user/device interval")
		}
		seen[key] = true
		w, ok := windows[i.device]
		if !ok {
			w = window{i.activationStart, i.activationEnd, i.releaseStart, i.releaseEnd}
		} else {
			if i.activationStart.Before(w.outerStart) {
				w.outerStart = i.activationStart
			}
			if i.activationEnd.After(w.outerEnd) {
				w.outerEnd = i.activationEnd
			}
			if i.releaseStart.After(w.innerStart) {
				w.innerStart = i.releaseStart
			}
			if i.releaseEnd.Before(w.innerEnd) {
				w.innerEnd = i.releaseEnd
			}
		}
		windows[i.device] = w
	}

	type totals struct {
		readSelected, readPossible, writeSelected, writePossible uint64
		uncertain                                                bool
	}
	sums := make(map[Device]totals)
	for _, i := range intervals {
		t := sums[i.device]
		add := func(target *uint64, value uint64) error {
			if value > math.MaxUint64-*target {
				return fmt.Errorf("aggregate IOPS counter overflow")
			}
			*target += value
			return nil
		}
		for _, pair := range []struct {
			target *uint64
			value  uint64
		}{{&t.readSelected, i.readSelected}, {&t.readPossible, i.readPossible}, {&t.writeSelected, i.writeSelected}, {&t.writePossible, i.writePossible}} {
			if err := add(pair.target, pair.value); err != nil {
				return nil, err
			}
		}
		t.uncertain = t.uncertain || i.unresolved
		sums[i.device] = t
	}
	result := make(map[Device]DeviceBounds, len(windows))
	for device, w := range windows {
		outer, inner := w.outerEnd.Sub(w.outerStart), w.innerEnd.Sub(w.innerStart)
		if inner <= 0 {
			return nil, fmt.Errorf("IOPS users have no common release interval")
		}
		t := sums[device]
		result[device] = DeviceBounds{Read: RateBounds{operationRate(t.readSelected, outer, false), operationRate(t.readPossible, inner, true)}, Write: RateBounds{operationRate(t.writeSelected, outer, false), operationRate(t.writePossible, inner, true)}, Uncertain: t.uncertain}
	}
	return result, nil
}

// operationRate computes the rational count/elapsed rate before converting to a
// float. Inexact conversion rounds outwards so floating-point error cannot turn
// an uncertain threshold equality into a false activation or release proof.
func operationRate(count uint64, elapsed time.Duration, upper bool) float64 {
	numerator := new(big.Int).SetUint64(count)
	numerator.Mul(numerator, big.NewInt(int64(time.Second)))
	ratio := new(big.Rat).SetFrac(numerator, big.NewInt(int64(elapsed)))
	value, exact := ratio.Float64()
	if exact {
		return value
	}
	if upper {
		return math.Nextafter(value, math.Inf(1))
	}
	return math.Nextafter(value, 0)
}
