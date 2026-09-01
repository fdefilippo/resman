package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestCgroupSnapshotReadersRejectDirectoryReplacementDuringInterfaceReads(t *testing.T) {
	t.Run("CPU Points node", func(t *testing.T) {
		runSnapshotDirectoryReplacement(t, "cpu.stat",
			"usage_usec 123\nnr_periods 10\nnr_throttled 4\nthrottled_usec 19\n",
			func(path string) {
				writeSnapshotFixture(t, filepath.Join(path, "cpu.max"), "90000 100000\n")
				writeSnapshotFixture(t, filepath.Join(path, "cpu.weight"), "300\n")
			},
			func(path string) error {
				_, err := (&Manager{}).GetCPUPointsNodeSnapshot(path)
				return err
			},
		)
	})

	t.Run("RAM node", func(t *testing.T) {
		const uid = 1000
		runSnapshotDirectoryReplacement(t, "memory.current", "4096\n",
			func(path string) {
				writeSnapshotFixture(t, filepath.Join(path, "memory.high"), "2048\n")
				writeSnapshotFixture(t, filepath.Join(path, "memory.max"), "8192\n")
				writeSnapshotFixture(t, filepath.Join(path, "memory.swap.max"), "0\n")
				writeSnapshotFixture(t, filepath.Join(path, "memory.events"), "high 7\nmax 0\noom 0\noom_kill 0\n")
			},
			func(path string) error {
				manager := &Manager{createdCgroups: map[int]string{uid: path}}
				_, err := manager.GetMemoryAccountingSnapshot(uid)
				return err
			},
		)
	})
}

type fifoOpenResult struct {
	file *os.File
	err  error
}

func runSnapshotDirectoryReplacement(t *testing.T, blockedFile, blockedValue string, populateReplacement func(string), readSnapshot func(string) error) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "current")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(path, blockedFile)
	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		t.Fatalf("create %s FIFO: %v", blockedFile, err)
	}

	readResult := make(chan error, 1)
	go func() {
		readResult <- readSnapshot(path)
	}()
	writerResult := make(chan fifoOpenResult, 1)
	go func() {
		file, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
		writerResult <- fifoOpenResult{file: file, err: err}
	}()

	var writer *os.File
	select {
	case result := <-writerResult:
		if result.err != nil {
			t.Fatalf("open %s FIFO writer: %v", blockedFile, result.err)
		}
		writer = result.file
	case err := <-readResult:
		t.Fatalf("snapshot reader returned before opening %s FIFO: %v", blockedFile, err)
	case <-time.After(5 * time.Second):
		t.Fatalf("snapshot reader did not open %s FIFO", blockedFile)
	}

	retiredPath := filepath.Join(root, "retired")
	if err := os.Rename(path, retiredPath); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	populateReplacement(path)
	if _, err := writer.WriteString(blockedValue); err != nil {
		_ = writer.Close()
		t.Fatalf("write %s FIFO: %v", blockedFile, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close %s FIFO: %v", blockedFile, err)
	}

	select {
	case err := <-readResult:
		if err == nil || !strings.Contains(err.Error(), "changed identity") {
			t.Fatalf("snapshot reader error = %v, want changed identity", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot reader did not complete after FIFO release")
	}
}

func writeSnapshotFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
