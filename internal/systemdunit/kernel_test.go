package systemdunit

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyKernelVerifierChecksEveryApprovedScalarInterface(t *testing.T) {
	root := t.TempDir()
	controlGroup := "/user.slice/user-1001.slice"
	path := filepath.Join(root, "user.slice", "user-1001.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	files := map[string]string{
		"cpu.weight":      "321\n",
		"cpu.max":         "90000 100000\n",
		"memory.high":     "67108864\n",
		"memory.max":      "max\n",
		"memory.swap.max": "0\n",
		"io.weight":       "default 456\n8:0 200\n",
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	properties := newPropertySet(map[PropertyName]uint64{
		PropertyCPUWeight:          321,
		PropertyCPUQuotaPerSecUSec: 900_000,
		PropertyCPUQuotaPeriodUSec: 100_000,
		PropertyMemoryHigh:         64 << 20,
		PropertyMemoryMax:          math.MaxUint64,
		PropertyMemorySwapMax:      0,
		PropertyIOWeight:           456,
	})
	snapshot := UnitSnapshot{Identity: UnitIdentity{Name: "user-1001.slice"}, ControlGroup: controlGroup, Properties: properties}
	assignments := properties.Assignments()

	if err := newCgroupVerifier(root).verify(snapshot, assignments); err != nil {
		t.Fatalf("verify() error = %v", err)
	}
}

func TestReadOnlyKernelVerifierRejectsDivergentEffectiveValue(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "cpu.weight"), []byte("100\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	snapshot := UnitSnapshot{
		Identity:     UnitIdentity{Name: parentUserSlice},
		ControlGroup: "/user.slice",
		Properties:   newPropertySet(map[PropertyName]uint64{PropertyCPUWeight: 500}),
	}
	assignment := mustAssignment(t, PropertyCPUWeight, 500)
	if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{assignment}); err == nil {
		t.Fatal("verify() error = nil, want effective-value mismatch")
	}
}

func TestReadOnlyKernelVerifierRejectsDivergentCPUQuotaAndPeriod(t *testing.T) {
	tests := []struct {
		name   string
		cpuMax string
		perSec uint64
		period uint64
	}{
		{name: "quota", cpuMax: "80000 100000\n", perSec: 900_000, period: 100_000},
		{name: "period", cpuMax: "225000 250000\n", perSec: 900_000, period: 100_000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "user.slice")
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(test.cpuMax), 0600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			snapshot := UnitSnapshot{
				Identity:     UnitIdentity{Name: parentUserSlice},
				ControlGroup: "/user.slice",
				Properties: newPropertySet(map[PropertyName]uint64{
					PropertyCPUQuotaPerSecUSec: test.perSec,
					PropertyCPUQuotaPeriodUSec: test.period,
				}),
			}
			assignments := []PropertyAssignment{
				mustAssignment(t, PropertyCPUQuotaPerSecUSec, test.perSec),
				mustAssignment(t, PropertyCPUQuotaPeriodUSec, test.period),
			}
			if err := newCgroupVerifier(root).verify(snapshot, assignments); err == nil {
				t.Fatal("verify() error = nil, want cpu.max mismatch")
			}
		})
	}
}

func TestReadOnlyKernelVerifierAcceptsMissingControllerOnlyForUnsetProperty(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	unset := UnitSnapshot{
		Identity:     UnitIdentity{Name: parentUserSlice},
		ControlGroup: "/user.slice",
		Properties:   newPropertySet(map[PropertyName]uint64{PropertyCPUWeight: SystemdUnset}),
	}
	if err := newCgroupVerifier(root).verify(unset, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, SystemdUnset)}); err != nil {
		t.Fatalf("unset verification error = %v", err)
	}
	finite := unset
	finite.Properties = newPropertySet(map[PropertyName]uint64{PropertyCPUWeight: 200})
	if err := newCgroupVerifier(root).verify(finite, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 200)}); err == nil {
		t.Fatal("finite verification accepted a missing controller interface")
	}
}

func TestScalePerSecondQuotaRejectsOverflow(t *testing.T) {
	if _, err := scalePerSecondQuota(math.MaxUint64, math.MaxUint64); err == nil {
		t.Fatal("scalePerSecondQuota() error = nil, want overflow")
	}
}
