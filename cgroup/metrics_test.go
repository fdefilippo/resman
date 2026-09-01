package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCPUPointsAndMemoryAccountingSnapshotsReadTypedKernelState(t *testing.T) {
	root := t.TempDir()
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(root, "cpu.stat"), "usage_usec 123\nuser_usec 80\nsystem_usec 43\nnr_periods 10\nnr_throttled 4\nthrottled_usec 19\n")
	write(filepath.Join(root, "cpu.max"), "90000 100000\n")
	write(filepath.Join(root, "cpu.weight"), "300\n")

	manager := &Manager{createdCgroups: map[int]string{1000: root}}
	cpu, err := manager.GetCPUPointsNodeSnapshot(root)
	if err != nil {
		t.Fatalf("GetCPUPointsNodeSnapshot() error = %v", err)
	}
	if cpu.CPUStat.UsageUsec != 123 || cpu.CPUStat.NrPeriods != 10 || cpu.CPUStat.NrThrottled != 4 || cpu.CPUStat.ThrottledUsec != 19 {
		t.Fatalf("CPU counters = %+v", cpu.CPUStat)
	}
	if cpu.CPUQuota != "90000 100000" || cpu.CPUWeight != 300 || cpu.Identity.Inode == 0 {
		t.Fatalf("CPU snapshot = %+v", cpu)
	}

	write(filepath.Join(root, "memory.current"), "4096\n")
	write(filepath.Join(root, "memory.high"), "2048\n")
	write(filepath.Join(root, "memory.max"), "8192\n")
	write(filepath.Join(root, "memory.swap.max"), "0\n")
	write(filepath.Join(root, "memory.events"), "low 0\nhigh 7\nmax 0\noom 0\noom_kill 0\n")
	memory, err := manager.GetMemoryAccountingSnapshot(1000)
	if err != nil {
		t.Fatalf("GetMemoryAccountingSnapshot() error = %v", err)
	}
	if memory.CurrentBytes != 4096 || memory.HighLimit != "2048" || memory.MaxLimit != "8192" || memory.SwapMax != "0" {
		t.Fatalf("memory snapshot = %+v", memory)
	}
	if memory.Events.High != 7 || memory.Events.Max != 0 || memory.Events.OOM != 0 || memory.Events.OOMKill != 0 {
		t.Fatalf("memory events = %+v", memory.Events)
	}
}

func TestCgroupCounterSnapshotRejectsIncompleteOrAmbiguousData(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("cpu.max", "max 100000")
	write("cpu.weight", "100")

	for _, tt := range []struct {
		name string
		data string
	}{
		{name: "missing required counter", data: "usage_usec 1\nnr_periods 2\nnr_throttled 0\n"},
		{name: "duplicate counter", data: "usage_usec 1\nusage_usec 2\nnr_periods 2\nnr_throttled 0\nthrottled_usec 0\n"},
		{name: "invalid counter", data: "usage_usec nope\nnr_periods 2\nnr_throttled 0\nthrottled_usec 0\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			write("cpu.stat", tt.data)
			if _, err := (&Manager{}).GetCPUPointsNodeSnapshot(root); err == nil {
				t.Fatal("GetCPUPointsNodeSnapshot() error = nil")
			}
		})
	}
}

func TestCgroupSnapshotIdentityConfirmationRejectsMixedLifetimes(t *testing.T) {
	root := t.TempDir()
	identity, err := readCgroupIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	identity.Inode++
	if err := confirmCgroupIdentity(root, identity); err == nil {
		t.Fatal("confirmCgroupIdentity() accepted a different cgroup lifetime")
	}
}
