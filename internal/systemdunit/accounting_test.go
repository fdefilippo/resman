package systemdunit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/cgroup"
)

func TestAdapterAccountingReadsResourcesIndependentlyAndReconfirmsIdentity(t *testing.T) {
	for _, scenario := range []string{"complete", "CPU unavailable", "RAM unavailable", "unit recreated during observation", "stale requested identity"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "user.slice", "user-1000.slice")
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{"cpu.stat": "usage_usec 100\nnr_periods 10\nnr_throttled 2\nthrottled_usec 30\n", "cpu.max": "max 100000\n", "cpu.weight": "9900\n", "memory.current": "100663296\n", "memory.high": "429494272\n", "memory.max": "536870912\n", "memory.swap.max": "max\n", "memory.events": "high 3\nmax 2\noom 1\noom_kill 1\n"}
			for name, value := range files {
				if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			transport := newFakeUnitTransport(1000)
			adapter := mustTestAdapter(t, transport, newCgroupVerifier(root))
			identity := identityFor(t, adapter, 1000)
			transport.unitReads = 0
			switch scenario {
			case "CPU unavailable":
				if err := os.Remove(filepath.Join(path, "cpu.stat")); err != nil {
					t.Fatal(err)
				}
			case "RAM unavailable":
				if err := os.Remove(filepath.Join(path, "memory.current")); err != nil {
					t.Fatal(err)
				}
			case "unit recreated during observation":
				transport.onUnitRead = func(f *fakeUnitTransport, unit string, reads int) {
					// Each readUnit has two internal identity reads. Change the
					// lifetime between readUnit calls, not inside the first one.
					if reads == 3 {
						f.units[unit].unit["InvocationID"] = invocationBytes(900)
					}
				}
			case "stale requested identity":
				identity.InvocationID[0]++
			}
			got, err := adapter.ObserveAccounting(context.Background(), identity)
			if scenario == "unit recreated during observation" || scenario == "stale requested identity" {
				if err == nil {
					t.Fatal("mixed unit lifetimes accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "CPU unavailable" {
				if got.CPU != nil || got.CPUError == nil || got.Memory == nil {
					t.Fatalf("independent CPU failure: %+v", got)
				}
				return
			}
			if scenario == "RAM unavailable" {
				if got.Memory != nil || got.MemoryError == nil || got.CPU == nil {
					t.Fatalf("independent RAM failure: %+v", got)
				}
				return
			}
			if got.CPU == nil || got.Memory == nil || got.CPU.CPUWeight != 9900 || got.Memory.CurrentBytes != 96<<20 || got.Memory.Events.High != 3 {
				t.Fatalf("observation: %+v", got)
			}
		})
	}
}

func TestObserveUnitAccountingRejectsMixedResourceCgroupLifetimes(t *testing.T) {
	identity := UnitIdentity{Name: "user-1000.slice"}
	cpuIdentity := cgroup.CgroupIdentity{Device: 1, Inode: 10}
	memoryIdentity := cgroup.CgroupIdentity{Device: 1, Inode: 11}
	_, err := observeUnitAccounting(
		identity,
		"/user.slice/user-1000.slice",
		func(string) (cgroup.CPUPointsNodeSnapshot, error) {
			return cgroup.CPUPointsNodeSnapshot{Identity: cpuIdentity}, nil
		},
		func(string) (cgroup.MemoryAccountingSnapshot, error) {
			return cgroup.MemoryAccountingSnapshot{Identity: memoryIdentity}, nil
		},
	)
	if err == nil {
		t.Fatal("CPU and memory observations from different cgroup lifetimes were accepted")
	}
}

func TestCPUAuthorityIncludesNestedContainersButRejectsSplitUIDs(t *testing.T) {
	for _, scenario := range []struct {
		name, path string
		covered    bool
	}{
		{"session", "/user.slice/user-1000.slice/session-1.scope", true},
		{"rootless container", "/user.slice/user-1000.slice/user@1000.service/libpod-example.scope/container", true},
		{"system service", "/system.slice/example.service", false},
		{"prefix collision", "/user.slice/user-1000.slice-other/session-1.scope", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "123")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "cgroup"), []byte("0::"+scenario.path+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			inspector := newProcCoverageInspector(root)
			inspector.ownerUID = func(os.FileInfo) uint32 { return 1000 }
			got, err := inspector.observeCPU(context.Background(), []resourceCoverageTarget{{uid: 1000, controlGroup: "/user.slice/user-1000.slice"}})
			if err != nil || got[1000] != scenario.covered {
				t.Fatalf("CPU coverage = %v, %v", got, err)
			}
		})
	}
}
