package systemdunit

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKernelIdentityResolvesCanonicalCgroupWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1001.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := info.Sys().(*syscall.Stat_t).Ino
	got, err := newCgroupVerifier(root).identity("/user.slice/user-1001.slice")
	if err != nil {
		t.Fatalf("identity() error = %v", err)
	}
	if got != want {
		t.Fatalf("identity = %d, want inode %d", got, want)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "user.slice", "escape")); err != nil {
		t.Fatal(err)
	}
	for _, controlGroup := range []string{
		"user.slice/user-1001.slice",
		"/user.slice/../outside",
		"/user.slice/./user-1001.slice",
		"/user.slice//user-1001.slice",
		"/user.slice/escape",
	} {
		t.Run(strings.ReplaceAll(controlGroup, "/", "_"), func(t *testing.T) {
			if identity, err := newCgroupVerifier(root).identity(controlGroup); err == nil {
				t.Fatalf("identity(%q) = %d, want fail-closed path rejection", controlGroup, identity)
			}
		})
	}
}

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
	properties := newScalarTestPropertySet(map[PropertyName]uint64{
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
		Properties:   newScalarTestPropertySet(map[PropertyName]uint64{PropertyCPUWeight: 500}),
	}
	assignment := mustAssignment(t, PropertyCPUWeight, 500)
	if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{assignment}); err == nil {
		t.Fatal("verify() error = nil, want effective-value mismatch")
	}
}

func TestReadOnlyKernelVerifierRejectsDivergentMemoryHighAndMax(t *testing.T) {
	for _, test := range []struct {
		name     string
		property PropertyName
		file     string
	}{
		{name: "high", property: PropertyMemoryHigh, file: "memory.high"},
		{name: "max", property: PropertyMemoryMax, file: "memory.max"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "user.slice", "user-1000.slice")
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, test.file), []byte("67108863\n"), 0600); err != nil {
				t.Fatal(err)
			}
			snapshot := UnitSnapshot{
				Identity:     UnitIdentity{Name: "user-1000.slice"},
				ControlGroup: "/user.slice/user-1000.slice",
				Properties:   newScalarTestPropertySet(map[PropertyName]uint64{test.property: 64 << 20}),
			}
			if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{mustAssignment(t, test.property, 64<<20)}); err == nil {
				t.Fatalf("verify() accepted divergent %s", test.property)
			}
		})
	}
}

func TestReadOnlyKernelVerifierAcceptsKernelPageRoundingForMemoryLimits(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	const requested = uint64(429_496_729)
	pageSize := uint64(os.Getpagesize())
	effective := requested - requested%pageSize
	if err := os.WriteFile(filepath.Join(path, "memory.high"), []byte(fmt.Sprintf("%d\n", effective)), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := UnitSnapshot{
		Identity:     UnitIdentity{Name: "user-1000.slice"},
		ControlGroup: "/user.slice/user-1000.slice",
		Properties:   newScalarTestPropertySet(map[PropertyName]uint64{PropertyMemoryHigh: requested}),
	}
	if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{mustAssignment(t, PropertyMemoryHigh, requested)}); err != nil {
		t.Fatalf("verify() rejected kernel page rounding: %v", err)
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
				Properties: newScalarTestPropertySet(map[PropertyName]uint64{
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
		Properties:   newScalarTestPropertySet(map[PropertyName]uint64{PropertyCPUWeight: SystemdUnset}),
	}
	if err := newCgroupVerifier(root).verify(unset, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, SystemdUnset)}); err != nil {
		t.Fatalf("unset verification error = %v", err)
	}
	finite := unset
	finite.Properties = newScalarTestPropertySet(map[PropertyName]uint64{PropertyCPUWeight: 200})
	if err := newCgroupVerifier(root).verify(finite, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 200)}); err == nil {
		t.Fatal("finite verification accepted a missing controller interface")
	}
}

func TestScalePerSecondQuotaRejectsOverflow(t *testing.T) {
	if _, err := scalePerSecondQuota(math.MaxUint64, math.MaxUint64); err == nil {
		t.Fatal("scalePerSecondQuota() error = nil, want overflow")
	}
}

func TestReadOnlyKernelVerifierChecksHardAndWeightedIO(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "io.weight"), []byte("default 321\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "io.max"), []byte("8:0 rbps=1048576 wbps=2097152 riops=100 wiops=200\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[PropertyName]propertyValue{PropertyIOWeight: scalarPropertyValue(321)}
	assignments := []PropertyAssignment{mustAssignment(t, PropertyIOWeight, 321)}
	for _, candidate := range []struct {
		name  PropertyName
		value uint64
	}{
		{name: PropertyIOReadBandwidthMax, value: 1 << 20},
		{name: PropertyIOWriteBandwidthMax, value: 2 << 20},
		{name: PropertyIOReadIOPSMax, value: 100},
		{name: PropertyIOWriteIOPSMax, value: 200},
	} {
		assignment, err := NewDevicePropertyAssignment(candidate.name, []DeviceLimit{{Path: "/dev/vda", Value: candidate.value}})
		if err != nil {
			t.Fatal(err)
		}
		values[candidate.name] = assignment.value
		assignments = append(assignments, assignment)
	}
	verifier := newCgroupVerifier(root)
	verifier.stat = func(string) (os.FileInfo, error) { return fakeBlockDeviceInfo{}, nil }
	snapshot := UnitSnapshot{Identity: UnitIdentity{Name: "user-1000.slice"}, ControlGroup: "/user.slice/user-1000.slice", Properties: newPropertySet(values)}
	if err := verifier.verify(snapshot, assignments); err != nil {
		t.Fatalf("verify() error: %v", err)
	}

	if err := os.WriteFile(filepath.Join(path, "io.max"), []byte("8:0 rbps=1048575 wbps=2097152 riops=100 wiops=200\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifier.verify(snapshot, assignments); err == nil {
		t.Fatal("verify() accepted a divergent hard I/O limit")
	}
}

func TestReadOnlyKernelVerifierChecksSystemdWeightThroughBFQInterface(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "io.bfq.weight"), []byte("default 132\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := UnitSnapshot{
		Identity:     UnitIdentity{Name: "user-1000.slice"},
		ControlGroup: "/user.slice/user-1000.slice",
		Properties:   newScalarTestPropertySet(map[PropertyName]uint64{PropertyIOWeight: 456}),
	}
	if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{mustAssignment(t, PropertyIOWeight, 456)}); err != nil {
		t.Fatalf("verify(scaled BFQ weight) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "io.bfq.weight"), []byte("default 456\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{mustAssignment(t, PropertyIOWeight, 456)}); err != nil {
		t.Fatalf("verify(direct BFQ weight) error = %v", err)
	}
	snapshot = UnitSnapshot{
		Identity:     UnitIdentity{Name: "user-1000.slice"},
		ControlGroup: "/user.slice/user-1000.slice",
		Properties:   newScalarTestPropertySet(map[PropertyName]uint64{PropertyIOWeight: 1_000}),
	}
	if err := os.WriteFile(filepath.Join(path, "io.bfq.weight"), []byte("default 100\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{mustAssignment(t, PropertyIOWeight, 1_000)}); err == nil {
		t.Fatal("verify() accepted the unchanged BFQ baseline for the startup probe")
	}
	if err := os.WriteFile(filepath.Join(path, "io.bfq.weight"), []byte("default 131\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{mustAssignment(t, PropertyIOWeight, 456)}); err == nil {
		t.Fatal("verify() accepted a divergent BFQ weight")
	}
}

func TestIOPreflightAcceptsControllerThatSystemdCanEnableOnTheParent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.slice", "cgroup.controllers"), []byte("cpu io memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := UnitSnapshot{Identity: UnitIdentity{Name: "user-1000.slice"}, ControlGroup: "/user.slice/user-1000.slice"}
	if err := newCgroupVerifier(root).preflight(snapshot, []PropertyAssignment{mustAssignment(t, PropertyIOWeight, 456)}); err != nil {
		t.Fatalf("preflight() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.slice", "cgroup.controllers"), []byte("cpu memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := newCgroupVerifier(root).preflight(snapshot, []PropertyAssignment{mustAssignment(t, PropertyIOWeight, 456)}); err == nil {
		t.Fatal("preflight() accepted an unavailable I/O controller")
	}
}

func TestApplyPreflightOnlyDefersAnAbsentMaterializableIOInterface(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	rootControllers := filepath.Join(root, "cgroup.controllers")
	if err := os.WriteFile(rootControllers, []byte("cpu io memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.slice", "cgroup.controllers"), []byte("cpu memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := UnitSnapshot{Identity: UnitIdentity{Name: "user-1000.slice"}, ControlGroup: "/user.slice/user-1000.slice"}
	ioLimit, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	verifier := newCgroupVerifier(root)
	if err := verifier.preflight(snapshot, []PropertyAssignment{ioLimit}); err == nil {
		t.Fatal("strict startup preflight accepted an absent io.max")
	}
	if err := verifier.preflightApply(snapshot, []PropertyAssignment{ioLimit}); err != nil {
		t.Fatalf("preflightApply() rejected materializable io.max: %v", err)
	}

	if err := os.WriteFile(rootControllers, []byte("cpu memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifier.preflightApply(snapshot, []PropertyAssignment{ioLimit}); err == nil {
		t.Fatal("preflightApply() accepted a missing root I/O controller")
	}
	if err := os.WriteFile(rootControllers, []byte("cpu io memory\n"), 0600); err != nil {
		t.Fatal(err)
	}

	readFile := verifier.readFile
	verifier.readFile = func(filename string) ([]byte, error) {
		if filepath.Base(filename) == "io.max" {
			return nil, os.ErrPermission
		}
		return readFile(filename)
	}
	if err := verifier.preflightApply(snapshot, []PropertyAssignment{ioLimit}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("preflightApply() error = %v, want io.max permission failure", err)
	}

	verifier = newCgroupVerifier(root)
	for _, assignment := range []PropertyAssignment{
		mustAssignment(t, PropertyCPUQuotaPerSecUSec, 50_000),
		mustAssignment(t, PropertyMemoryHigh, 64<<20),
	} {
		if err := verifier.preflightApply(snapshot, []PropertyAssignment{assignment}); err == nil {
			t.Fatalf("preflightApply() accepted absent interface for %s", assignment.Name())
		}
	}
}

func TestPostApplyVerificationRejectsMissingIOMaxWhenDeviceLimitsAreExpected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	assignment, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := UnitSnapshot{
		Identity:     UnitIdentity{Name: "user-1000.slice"},
		ControlGroup: "/user.slice/user-1000.slice",
		Properties:   newPropertySet(map[PropertyName]propertyValue{assignment.Name(): assignment.value}),
	}
	if err := newCgroupVerifier(root).verify(snapshot, []PropertyAssignment{assignment}); err == nil {
		t.Fatal("post-Apply verification accepted absent io.max with a device limit expected")
	}
}

func TestPreflightRequiresEveryEnabledControllerInterface(t *testing.T) {
	for _, test := range []struct {
		name          string
		property      PropertyName
		interfaceName string
	}{
		{name: "cpu quota", property: PropertyCPUQuotaPerSecUSec, interfaceName: "cpu.max"},
		{name: "memory high", property: PropertyMemoryHigh, interfaceName: "memory.high"},
		{name: "memory max", property: PropertyMemoryMax, interfaceName: "memory.max"},
		{name: "strong io", property: PropertyIOReadBandwidthMax, interfaceName: "io.max"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "user.slice")
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu io memory\n"), 0600); err != nil {
				t.Fatal(err)
			}
			snapshot := UnitSnapshot{Identity: UnitIdentity{Name: parentUserSlice}, ControlGroup: "/user.slice"}
			assignment := PropertyAssignment{name: test.property}
			verifier := newCgroupVerifier(root)
			if err := verifier.preflight(snapshot, []PropertyAssignment{assignment}); err == nil || !strings.Contains(err.Error(), test.interfaceName) {
				t.Fatalf("preflight() error = %v, want missing %s", err, test.interfaceName)
			}
			if err := os.WriteFile(filepath.Join(path, test.interfaceName), []byte("available\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifier.preflight(snapshot, []PropertyAssignment{assignment}); err != nil {
				t.Fatalf("preflight() after creating %s error = %v", test.interfaceName, err)
			}
		})
	}
}

func TestResolveBlockDevicesReturnsCanonicalNodesForFilterAndAll(t *testing.T) {
	root := t.TempDir()
	sysBlock := filepath.Join(root, "sys", "block")
	sysDevBlock := filepath.Join(root, "sys", "dev", "block")
	devRoot := filepath.Join(root, "dev")
	for _, fixture := range []struct {
		name   string
		number string
	}{
		{name: "sda", number: "8:0"},
		{name: "sdb", number: "8:16"},
	} {
		if err := os.MkdirAll(filepath.Join(sysBlock, fixture.name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sysBlock, fixture.name, "dev"), []byte(fixture.number+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sysDevBlock, fixture.number), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sysDevBlock, fixture.number, "uevent"), []byte("DEVNAME="+fixture.name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(string) (os.FileInfo, error) { return fakeBlockDeviceInfo{}, nil }
	got, err := resolveBlockDevices("8:16", sysBlock, sysDevBlock, devRoot, os.ReadDir, os.ReadFile, stat)
	if err != nil {
		t.Fatalf("resolveBlockDevices(filter) error = %v", err)
	}
	want := []string{filepath.Join(devRoot, "sdb")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveBlockDevices(filter) = %v, want %v", got, want)
	}
	got, err = resolveBlockDevices("all", sysBlock, sysDevBlock, devRoot, os.ReadDir, os.ReadFile, stat)
	if err != nil {
		t.Fatalf("resolveBlockDevices(all) error = %v", err)
	}
	want = []string{filepath.Join(devRoot, "sda"), filepath.Join(devRoot, "sdb")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveBlockDevices(all) = %v, want %v", got, want)
	}
}

type fakeBlockDeviceInfo struct{}

func newScalarTestPropertySet(values map[PropertyName]uint64) PropertySet {
	normalized := make(map[PropertyName]propertyValue, len(values))
	for name, value := range values {
		normalized[name] = scalarPropertyValue(value)
	}
	return newPropertySet(normalized)
}

func (fakeBlockDeviceInfo) Name() string       { return "vda" }
func (fakeBlockDeviceInfo) Size() int64        { return 0 }
func (fakeBlockDeviceInfo) Mode() os.FileMode  { return os.ModeDevice }
func (fakeBlockDeviceInfo) ModTime() time.Time { return time.Time{} }
func (fakeBlockDeviceInfo) IsDir() bool        { return false }
func (fakeBlockDeviceInfo) Sys() any {
	return &syscall.Stat_t{Rdev: uint64(unix.Mkdev(8, 0))}
}
