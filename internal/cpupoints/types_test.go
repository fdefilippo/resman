package cpupoints

import (
	"math"
	"testing"
)

func TestCPUPointConstructorsEnforceDistinctRanges(t *testing.T) {
	tests := []struct {
		name    string
		valid   []uint64
		invalid []uint64
		build   func(uint64) error
	}{
		{
			name:    "reserve",
			valid:   []uint64{0, 990},
			invalid: []uint64{991, 1000},
			build: func(value uint64) error {
				_, err := NewReservePoints(value)
				return err
			},
		},
		{
			name:    "parent pool",
			valid:   []uint64{10, 1000},
			invalid: []uint64{0, 9, 1001},
			build: func(value uint64) error {
				_, err := NewParentPoolPoints(value)
				return err
			},
		},
		{
			name:    "guaranteed",
			valid:   []uint64{1, 1000},
			invalid: []uint64{0, 1001},
			build: func(value uint64) error {
				_, err := NewConfiguredGuaranteePoints(value)
				return err
			},
		},
		{
			name:    "root",
			valid:   []uint64{1, 1000},
			invalid: []uint64{0, 1001},
			build: func(value uint64) error {
				_, err := NewRootPoints(value)
				return err
			},
		},
		{
			name:    "best effort",
			valid:   []uint64{1, 1000},
			invalid: []uint64{0, 1001},
			build: func(value uint64) error {
				_, err := NewBestEffortPoints(value)
				return err
			},
		},
		{
			name:    "acquired",
			valid:   []uint64{0, 1000},
			invalid: []uint64{1001},
			build: func(value uint64) error {
				_, err := NewAcquiredGuaranteePoints(value)
				return err
			},
		},
		{
			name:    "applied",
			valid:   []uint64{0, 1000},
			invalid: []uint64{1001},
			build: func(value uint64) error {
				_, err := NewAppliedGuaranteePoints(value)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, value := range tt.valid {
				if err := tt.build(value); err != nil {
					t.Errorf("value %d rejected: %v", value, err)
				}
			}
			for _, value := range tt.invalid {
				if err := tt.build(value); err == nil {
					t.Errorf("value %d accepted", value)
				}
			}
		})
	}
}

func TestReserveDerivesParentPool(t *testing.T) {
	tests := []struct {
		reserve uint64
		want    uint64
	}{
		{reserve: 0, want: 1000},
		{reserve: 100, want: 900},
		{reserve: 990, want: 10},
	}
	for _, tt := range tests {
		reserve, err := NewReservePoints(tt.reserve)
		if err != nil {
			t.Fatalf("NewReservePoints(%d): %v", tt.reserve, err)
		}
		if got := reserve.ParentPool().Value(); got != tt.want {
			t.Errorf("reserve %d produced pool %d, want %d", tt.reserve, got, tt.want)
		}
	}
}

func TestPlanParentQuotaUsesLiveDenominatorAndExactFloor(t *testing.T) {
	tests := []struct {
		name       string
		cpus       uint64
		pool       uint64
		wantQuota  uint64
		wantCPUmax string
	}{
		{name: "kernel minimum on one CPU", cpus: 1, pool: 10, wantQuota: 1000, wantCPUmax: "1000 100000"},
		{name: "full one CPU", cpus: 1, pool: 1000, wantQuota: 100000, wantCPUmax: "100000 100000"},
		{name: "moving denominator", cpus: 7, pool: 333, wantQuota: 233100, wantCPUmax: "233100 100000"},
		{name: "full multi CPU", cpus: 64, pool: 1000, wantQuota: 6400000, wantCPUmax: "6400000 100000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpus, err := NewOnlineCPUCount(tt.cpus)
			if err != nil {
				t.Fatal(err)
			}
			pool, err := NewParentPoolPoints(tt.pool)
			if err != nil {
				t.Fatal(err)
			}
			quota, err := PlanParentQuota(cpus, pool)
			if err != nil {
				t.Fatalf("PlanParentQuota(): %v", err)
			}
			if got := quota.QuotaMicroseconds(); got != tt.wantQuota {
				t.Errorf("quota = %d, want %d", got, tt.wantQuota)
			}
			if got := quota.CPUmax(); got != tt.wantCPUmax {
				t.Errorf("cpu.max = %q, want %q", got, tt.wantCPUmax)
			}
			if got := quota.OnlineCPUs().Value(); got != tt.cpus {
				t.Errorf("denominator = %d, want %d", got, tt.cpus)
			}
		})
	}
}

func TestPlanParentQuotaRejectsZeroAndOverflowingCapacity(t *testing.T) {
	if _, err := NewOnlineCPUCount(0); err == nil {
		t.Fatal("zero online CPUs accepted")
	}

	pool, err := NewParentPoolPoints(1000)
	if err != nil {
		t.Fatal(err)
	}
	overflowing, err := NewOnlineCPUCount(math.MaxUint64/ParentPeriodMicroseconds + 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanParentQuota(overflowing, pool); err == nil {
		t.Fatal("overflowing parent quota accepted")
	}
}

func TestFloorScaledQuotaRoundsDownWithoutIntermediateOverflow(t *testing.T) {
	tests := []struct {
		capacity uint64
		points   uint64
		want     uint64
	}{
		{capacity: 100001, points: 333, want: 33300},
		{capacity: 100999, points: 999, want: 100898},
		{capacity: math.MaxUint64, points: 1000, want: math.MaxUint64},
	}
	for _, tt := range tests {
		if got := floorScaledQuota(tt.capacity, tt.points); got != tt.want {
			t.Errorf("floorScaledQuota(%d, %d) = %d, want %d", tt.capacity, tt.points, got, tt.want)
		}
	}
}

func TestGuaranteeWeightsPreserveRatiosAndRejectZeroParticipation(t *testing.T) {
	one, _ := NewConfiguredGuaranteePoints(1)
	maximum, _ := NewConfiguredGuaranteePoints(1000)
	bestEffort, _ := NewBestEffortPoints(73)
	root, _ := NewRootPoints(101)
	acquired, _ := NewAcquiredGuaranteePoints(400)
	applied, _ := NewAppliedGuaranteePoints(300)

	if got, err := one.KernelWeight(); err != nil || got.Value() != 1 {
		t.Errorf("one point weight = %d, err=%v", got.Value(), err)
	}
	if got, err := maximum.KernelWeight(); err != nil || got.Value() != 1000 {
		t.Errorf("maximum point weight = %d, err=%v", got.Value(), err)
	}
	if got, err := bestEffort.KernelWeight(); err != nil || got.Value() != 73 {
		t.Errorf("best-effort weight = %d, err=%v", got.Value(), err)
	}
	if got, err := root.KernelWeight(); err != nil || got.Value() != 101 {
		t.Errorf("root weight = %d, err=%v", got.Value(), err)
	}
	if got, err := acquired.KernelWeight(); err != nil || got.Value() != 400 {
		t.Errorf("acquired weight = %d, err=%v", got.Value(), err)
	}
	if got, err := applied.KernelWeight(); err != nil || got.Value() != 300 {
		t.Errorf("applied weight = %d, err=%v", got.Value(), err)
	}
	zero, _ := NewAppliedGuaranteePoints(0)
	if _, err := zero.KernelWeight(); err == nil {
		t.Fatal("zero applied points produced a participating weight")
	}
}

func TestKernelCPUWeightRejectsInsteadOfClamping(t *testing.T) {
	for _, value := range []int{-1, 0, 10001} {
		if _, err := NewKernelCPUWeight(value); err == nil {
			t.Errorf("NewKernelCPUWeight(%d) accepted", value)
		}
	}
	for _, value := range []int{1, 10000} {
		weight, err := NewKernelCPUWeight(value)
		if err != nil {
			t.Errorf("NewKernelCPUWeight(%d): %v", value, err)
			continue
		}
		if weight.Value() != value {
			t.Errorf("weight = %d, want %d", weight.Value(), value)
		}
	}
}
