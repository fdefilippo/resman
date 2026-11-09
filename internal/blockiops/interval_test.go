package blockiops

import (
	"math"
	"testing"
	"time"
)

func samplePair() (Sample, Sample, Device) {
	device := Device{Major: 8}
	now := time.Now()
	id := Identity{SourceGeneration: 1, PolicyGeneration: 1, WatchGeneration: 1, UID: 1000, InvocationID: "unit-a", CgroupID: 42}
	return Sample{Identity: id, Started: now, Finished: now, Devices: map[Device]Counters{device: {}}},
		Sample{Identity: id, Started: now.Add(10 * time.Second), Finished: now.Add(10 * time.Second), Devices: map[Device]Counters{device: {}}}, device
}

func TestIntervalPreservesExcludedAndAmbiguousOperationMeaning(t *testing.T) {
	for _, tc := range []struct {
		name       string
		before     Partition
		after      Partition
		lower      float64
		upper      float64
		unresolved bool
	}{
		{name: "selected", after: Partition{Selected: 100}, lower: 10, upper: 10},
		{name: "excluded traffic", after: Partition{Excluded: 10000}},
		{name: "mixed operation", after: Partition{Selected: 100, Excluded: 50, Unresolved: 20}, lower: 10, upper: 12, unresolved: true},
		{name: "unknown origin is not zero", after: Partition{Unresolved: 100}, upper: 10, unresolved: true},
		{name: "idle after mixed traffic", before: Partition{Unresolved: 100}, after: Partition{Unresolved: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after, device := samplePair()
			before.Devices[device] = Counters{Read: tc.before, Write: tc.before}
			after.Devices[device] = Counters{Read: tc.after, Write: tc.after}
			interval, err := Between(before, after, device)
			if err != nil {
				t.Fatal(err)
			}
			for _, rates := range []RateBounds{interval.Read(), interval.Write()} {
				if rates.ActivationMinimum != tc.lower || rates.ReleaseMaximum != tc.upper {
					t.Fatalf("bounds = %+v, want [%g, %g]", rates, tc.lower, tc.upper)
				}
			}
			if interval.HasUnresolvedOperations() != tc.unresolved {
				t.Fatalf("attribution uncertainty = %v, want %v", interval.HasUnresolvedOperations(), tc.unresolved)
			}
		})
	}
}

func TestReadJitterWeakensBothDecisionProofs(t *testing.T) {
	before, after, device := samplePair()
	before.Finished = before.Started.Add(time.Second)
	after.Started = before.Started.Add(11 * time.Second)
	after.Finished = before.Started.Add(12 * time.Second)
	after.Devices[device] = Counters{Read: Partition{Selected: 120, Unresolved: 80}}
	interval, err := Between(before, after, device)
	if err != nil {
		t.Fatal(err)
	}
	if got := interval.Read(); got.ActivationMinimum != 10 || got.ReleaseMaximum != 20 {
		t.Fatalf("read-jitter bounds = %+v, want [10, 20]", got)
	}
	activationStart, activationEnd := interval.ActivationWindow()
	releaseStart, releaseEnd := interval.ReleaseWindow()
	if activationEnd.Sub(activationStart) != 12*time.Second || releaseEnd.Sub(releaseStart) != 10*time.Second {
		t.Fatal("activation must use the outer window and release the inner window")
	}
}

func TestInvalidEvidenceCannotBecomeReleaseCoverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Sample, *Sample, Device)
	}{
		{"source restart", func(_, b *Sample, _ Device) { b.Identity.SourceGeneration++ }},
		{"watch retired and registered again", func(_, b *Sample, _ Device) { b.Identity.WatchGeneration++ }},
		{"policy reload", func(_, b *Sample, _ Device) { b.Identity.PolicyGeneration++ }},
		{"UID changed", func(_, b *Sample, _ Device) { b.Identity.UID++ }},
		{"unit recreated", func(_, b *Sample, _ Device) { b.Identity.InvocationID = "unit-b" }},
		{"cgroup replaced", func(_, b *Sample, _ Device) { b.Identity.CgroupID++ }},
		{"missing identity", func(a, b *Sample, _ Device) { a.Identity.SourceGeneration = 0; b.Identity.SourceGeneration = 0 }},
		{"new device", func(a, _ *Sample, d Device) { delete(a.Devices, d) }},
		{"missing device", func(_, b *Sample, d Device) { delete(b.Devices, d) }},
		{"overlapping reads", func(a, b *Sample, _ Device) { b.Started = a.Finished }},
		{"backward clock", func(_, b *Sample, _ Device) { b.Finished = b.Started.Add(-time.Second) }},
		{"counter reset", func(a, _ *Sample, d Device) { a.Devices[d] = Counters{Read: Partition{Selected: 1}} }},
		{"excluded counter reset", func(a, _ *Sample, d Device) { a.Devices[d] = Counters{Write: Partition{Excluded: 1}} }},
		{"overflow", func(_, b *Sample, d Device) {
			b.Devices[d] = Counters{Read: Partition{Selected: math.MaxUint64, Unresolved: 1}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after, device := samplePair()
			tc.mutate(&before, &after, device)
			if _, err := Between(before, after, device); err == nil {
				t.Fatal("invalid evidence was accepted as measured coverage")
			}
		})
	}
}

func TestAggregateUsesCommonWindowWithoutSummingDeviceLayers(t *testing.T) {
	a, b, d := samplePair()
	b.Devices[d] = Counters{Write: Partition{Selected: 100}}
	first, err := Between(a, b, d)
	if err != nil {
		t.Fatal(err)
	}
	a.Identity.UID++
	b.Identity = a.Identity
	a.Started = a.Started.Add(5 * time.Second)
	a.Finished = a.Started
	b.Started = b.Started.Add(5 * time.Second)
	b.Finished = b.Started
	second, err := Between(a, b, d)
	if err != nil {
		t.Fatal(err)
	}
	another := Device{Major: 253}
	a.Devices[another] = a.Devices[d]
	b.Devices[another] = b.Devices[d]
	layered, err := Between(a, b, another)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Aggregate([]Interval{first, second, layered})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("layers collapsed: %v", got)
	}
	rate := got[d].Write
	// 200 selected operations / 15-second outer window; / 5-second inner.
	if math.Abs(rate.ActivationMinimum-200.0/15) > 1e-10 || rate.ActivationMinimum > 200.0/15 || math.Abs(rate.ReleaseMaximum-40) > 1e-10 || rate.ReleaseMaximum < 40 {
		t.Fatalf("common-window bounds: %+v", rate)
	}
	if got[another].Write.ActivationMinimum > 10 {
		t.Fatal("separate device includes another device's traffic")
	}
	if _, err := Aggregate([]Interval{first, first}); err == nil {
		t.Fatal("duplicate interval accepted")
	}
}
