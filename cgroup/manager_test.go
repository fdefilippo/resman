package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/config"
)

func TestNewManagerRequiresOnlyReadableCgroupV2Observation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory io\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	if manager.EnforcementStatus().Mode != EnforcementModeObservationOnly {
		t.Fatalf("status=%+v", manager.EnforcementStatus())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cgroup.controllers" {
		t.Fatalf("observation startup created cgroup state: %v", entries)
	}
}

func TestNewManagerFailsClosedWithoutCgroupV2Observation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = t.TempDir()
	_, err := NewManager(cfg)
	if err == nil || !IsRequiredCapabilityError(err) {
		t.Fatalf("error=%v, want required capability error", err)
	}
}

func TestUpdateConfigNeverCreatesManagedHierarchy(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	manager := &Manager{cfg: cfg}
	reloaded := config.DefaultConfig()
	reloaded.CgroupRoot = root
	reloaded.RAMEnabled = !cfg.RAMEnabled
	if err := manager.UpdateConfig(reloaded); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("reload created cgroup state: %v", entries)
	}

	changedRoot := config.DefaultConfig()
	changedRoot.RAMEnabled = reloaded.RAMEnabled
	changedRoot.CgroupRoot = filepath.Join(root, "other")
	if err := manager.UpdateConfig(changedRoot); err == nil {
		t.Fatal("live CGROUP_ROOT change was accepted")
	}
	if manager.getConfig().CgroupRoot != root {
		t.Fatal("rejected update changed effective configuration")
	}
}

func TestCleanupAllHasNoManagedStateToMutate(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{cfg: &config.Config{CgroupRoot: root}}
	if err := manager.CleanupAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "resman")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup created managed state: %v", err)
	}
}
