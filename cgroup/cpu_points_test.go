package cgroup

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/logging"
)

func TestEnsureCPUPointsHierarchyProgramsAndVerifiesEverySchedulingLevel(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	base := filepath.Join(root, cfg.CgroupBase)
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		cfg:                cfg,
		logger:             logging.GetLogger(),
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: filepath.Join(root, "created-cgroups"),
		writeController: func(path, value string) error {
			return os.WriteFile(path, []byte(value), 0644)
		},
		createManagedCgroup: func(path string) error {
			if err := os.Mkdir(path, 0755); err != nil {
				return err
			}
			for _, name := range []string{"cpu.max", "cpu.weight", "cgroup.procs", "cgroup.subtree_control"} {
				if err := os.WriteFile(filepath.Join(path, name), nil, 0644); err != nil {
					return err
				}
			}
			return nil
		},
	}
	online, _ := cpupoints.NewOnlineCPUCount(2)
	reserve, _ := cpupoints.NewReservePoints(100)
	quota, err := cpupoints.PlanParentQuota(online, reserve.ParentPool())
	if err != nil {
		t.Fatal(err)
	}
	bestEffort, _ := cpupoints.NewKernelCPUWeight(100)

	hierarchy, err := manager.EnsureCPUPointsHierarchy(quota, bestEffort)
	if err != nil {
		t.Fatalf("EnsureCPUPointsHierarchy(): %v", err)
	}
	wantParent := filepath.Join(base, "limited")
	if hierarchy != (CPUPointsHierarchy{
		Parent: wantParent, Guaranteed: filepath.Join(wantParent, "guaranteed"), BestEffort: filepath.Join(wantParent, "best_effort"),
	}) {
		t.Fatalf("hierarchy = %+v", hierarchy)
	}
	for path, want := range map[string]string{
		filepath.Join(hierarchy.Parent, "cpu.max"):                    "180000 100000",
		filepath.Join(hierarchy.Guaranteed, "cpu.weight"):             "1",
		filepath.Join(hierarchy.BestEffort, "cpu.weight"):             "100",
		filepath.Join(hierarchy.Guaranteed, "cgroup.procs"):           "",
		filepath.Join(hierarchy.BestEffort, "cgroup.procs"):           "",
		filepath.Join(hierarchy.Guaranteed, "cgroup.subtree_control"): "+cpu",
		filepath.Join(hierarchy.BestEffort, "cgroup.subtree_control"): "+cpu",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestRemoveCPUPointsHierarchyRemovesLeavesBeforeParent(t *testing.T) {
	var removed []string
	manager := &Manager{removeManagedCgroup: func(path string) (cgroupRemovalResult, error) {
		removed = append(removed, path)
		return cgroupRemovalResult{}, nil
	}}
	hierarchy := CPUPointsHierarchy{Parent: "/limited", Guaranteed: "/limited/guaranteed", BestEffort: "/limited/best_effort"}
	if err := manager.RemoveCPUPointsHierarchy(hierarchy); err != nil {
		t.Fatal(err)
	}
	if want := []string{hierarchy.Guaranteed, hierarchy.BestEffort, hierarchy.Parent}; !reflect.DeepEqual(removed, want) {
		t.Fatalf("removal order = %v, want %v", removed, want)
	}
}

func TestCPUPointsLeafIsFullyConfiguredBeforeIngress(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	manager := &Manager{
		cfg:            cfg,
		logger:         logging.GetLogger(),
		createdCgroups: make(map[int]string),
	}
	weight, err := cpupoints.NewKernelCPUWeight(375)
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(root, "resman", "limited", "guaranteed", "user_1000")
	if err := manager.createUserCgroupDirectory(1000, leaf, &weight); err != nil {
		t.Fatalf("createUserCgroupDirectory(): %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(leaf, "cpu.weight"): "375",
		filepath.Join(leaf, "cpu.max"):    normalCPUQuota,
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if got := string(data); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestCPUPointsLeafRejectsMissingEnabledResourceInterfaceBeforeIngress(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	cfg.RAMEnabled = true
	manager := &Manager{
		cfg:            cfg,
		logger:         logging.GetLogger(),
		createdCgroups: make(map[int]string),
	}
	weight, err := cpupoints.NewKernelCPUWeight(200)
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(root, "resman", "limited", "guaranteed", "user_1000")
	err = manager.createUserCgroupDirectory(1000, leaf, &weight)
	if err == nil {
		t.Fatal("createUserCgroupDirectory() accepted a leaf without memory.max")
	}
	for _, fragment := range []string{"RAM limiting", "memory", "memory.max", "final leaf"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not name %q", err, fragment)
		}
	}
}

func TestRecoverSharedCgroupDiscoversNestedLeavesWithoutTrackingState(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "resman", "limited")
	guaranteedLeaf := filepath.Join(shared, "guaranteed", "user_1000")
	bestEffortLeaf := filepath.Join(shared, "best_effort", "user_1001")
	for _, path := range []string{shared, guaranteedLeaf, bestEffortLeaf} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	var removed []string
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	manager := &Manager{
		cfg:                cfg,
		logger:             logging.GetLogger(),
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: filepath.Join(root, "created-cgroups"),
		blockIOAccounting:  make(map[int]blockIOAccountingState),
		removeManagedCgroup: func(path string) (cgroupRemovalResult, error) {
			removed = append(removed, path)
			return cgroupRemovalResult{}, nil
		},
	}

	if err := manager.recoverSharedCgroup(shared); err != nil {
		t.Fatalf("recoverSharedCgroup(): %v", err)
	}
	want := []string{
		guaranteedLeaf,
		bestEffortLeaf,
		filepath.Join(shared, "guaranteed"),
		filepath.Join(shared, "best_effort"),
		shared,
	}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("recovery removal order = %v, want %v", removed, want)
	}
}
