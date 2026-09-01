package cgroup

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
)

func writeTestIOStat(t *testing.T, path string, readOps, writeOps uint64) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	data := []byte("8:0 rios=" + uintString(readOps) + " wios=" + uintString(writeOps) + " rbytes=0 wbytes=0\n")
	if err := os.WriteFile(filepath.Join(path, "io.stat"), data, 0644); err != nil {
		t.Fatalf("write io.stat: %v", err)
	}
}

func writeFakeCgroupFiles(t *testing.T, path string, readOps, writeOps uint64) {
	t.Helper()
	writeTestIOStat(t, path, readOps, writeOps)
	if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), nil, 0644); err != nil {
		t.Fatalf("write cgroup.procs: %v", err)
	}
}

func uintString(value uint64) string {
	return fmt.Sprintf("%d", value)
}

func TestLogicalBlockIOCountersRemainMonotonicAcrossPlacementTransition(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "user_1000")
	newPath := filepath.Join(root, "limited", "user_1000")
	writeTestIOStat(t, oldPath, 15, 25)
	writeTestIOStat(t, newPath, 100, 200)

	manager := &Manager{
		createdCgroups: map[int]string{1000: oldPath},
		blockIOAccounting: map[int]blockIOAccountingState{
			1000: {
				path: oldPath,
				base: blockIOCounters{readOps: 10, writeOps: 20},
			},
		},
	}
	before, err := manager.logicalBlockIOCounters(1000)
	if err != nil {
		t.Fatalf("logicalBlockIOCounters(old): %v", err)
	}
	if before.readOps != 5 || before.writeOps != 5 {
		t.Fatalf("old logical counters = %+v, want 5/5 ops", before)
	}

	manager.createdCgroups[1000] = newPath
	manager.blockIOAccounting[1000] = blockIOAccountingState{
		path:   newPath,
		base:   blockIOCounters{readOps: 100, writeOps: 200},
		offset: before,
	}
	writeTestIOStat(t, newPath, 107, 211)
	after, err := manager.logicalBlockIOCounters(1000)
	if err != nil {
		t.Fatalf("logicalBlockIOCounters(new): %v", err)
	}
	if after.readOps != 12 || after.writeOps != 16 {
		t.Fatalf("new logical counters = %+v, want 12/16 ops", after)
	}
}

func TestCPUPointsIngressDoesNotRequireIOStatWhenIOLimitingIsDisabled(t *testing.T) {
	manager, root := newOriginTestManager(t)
	manager.cfg.IOEnabled = false
	manager.blockIOAccounting = make(map[int]blockIOAccountingState)
	manager.scanProcessIDs = func() (map[int][]int, error) { return map[int][]int{}, nil }
	if err := os.MkdirAll(filepath.Dir(manager.createdCgroupsFile), 0700); err != nil {
		t.Fatalf("create cgroup state directory: %v", err)
	}

	domain := filepath.Join(root, "resman", "limited", cpuPointsGuaranteedDomain)
	leaf := filepath.Join(domain, "user_1000")
	if err := os.MkdirAll(leaf, 0755); err != nil {
		t.Fatalf("create CPU Points leaf fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leaf, "cgroup.procs"), nil, 0644); err != nil {
		t.Fatalf("create cgroup.procs fixture: %v", err)
	}
	weight, err := cpupoints.NewKernelCPUWeight(300)
	if err != nil {
		t.Fatal(err)
	}

	got, result, err := manager.EnsureCPUPointsUserPlacement(1000, domain, weight)
	if err != nil {
		t.Fatalf("EnsureCPUPointsUserPlacement() error = %v", err)
	}
	if got != leaf || result.Applied() {
		t.Fatalf("placement = %q result=%+v, want empty-workload leaf %q", got, result, leaf)
	}
	accounting, ok := manager.blockIOAccounting[1000]
	if !ok || !accounting.unavailable || accounting.path != leaf {
		t.Fatalf("block I/O accounting = %+v, %t; want unavailable state at %s", accounting, ok, leaf)
	}
	if _, _, _, _, err := manager.GetIOStats(1000); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GetIOStats() error = %v, want explicit unavailable io.stat", err)
	}
}

func TestUnavailableBlockIOPlacementUsesFirstReadableSampleAsBaseline(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IOEnabled = false
	manager := &Manager{
		cfg: cfg,
		blockIOAccounting: map[int]blockIOAccountingState{
			1000: {path: "/managed/user_1000", offset: blockIOCounters{readOps: 7}, unavailable: true},
		},
	}
	current := uint64(100)
	manager.readBlockIOStats = func(string) (blockIOCounters, error) {
		return blockIOCounters{readOps: current}, nil
	}

	baseline, err := manager.logicalBlockIOCounters(1000)
	if err != nil {
		t.Fatalf("first readable logicalBlockIOCounters() error: %v", err)
	}
	if baseline.readOps != 7 {
		t.Fatalf("first readable logical counter = %d, want preserved offset 7", baseline.readOps)
	}
	current = 105
	measured, err := manager.logicalBlockIOCounters(1000)
	if err != nil {
		t.Fatalf("second logicalBlockIOCounters() error: %v", err)
	}
	if measured.readOps != 12 {
		t.Fatalf("second logical counter = %d, want offset 7 plus measured delta 5", measured.readOps)
	}
}

func TestBlockIOCountersAddRejectsOverflowInEveryDimension(t *testing.T) {
	tests := []struct {
		name  string
		left  blockIOCounters
		right blockIOCounters
	}{
		{"read bytes", blockIOCounters{readBytes: math.MaxUint64}, blockIOCounters{readBytes: 1}},
		{"write bytes", blockIOCounters{writeBytes: math.MaxUint64}, blockIOCounters{writeBytes: 1}},
		{"read operations", blockIOCounters{readOps: math.MaxUint64}, blockIOCounters{readOps: 1}},
		{"write operations", blockIOCounters{writeOps: math.MaxUint64}, blockIOCounters{writeOps: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.left.add(tt.right); !errors.Is(err, errCounterOverflow) {
				t.Fatalf("add() error = %v, want %v", err, errCounterOverflow)
			}
		})
	}
}

func TestLogicalBlockIOCountersRejectsOffsetOverflow(t *testing.T) {
	manager := &Manager{
		blockIOAccounting: map[int]blockIOAccountingState{
			1000: {
				path:   "/managed/user_1000",
				offset: blockIOCounters{readOps: math.MaxUint64},
			},
		},
		readBlockIOStats: func(string) (blockIOCounters, error) {
			return blockIOCounters{readOps: 1}, nil
		},
	}

	if _, err := manager.logicalBlockIOCounters(1000); !errors.Is(err, errCounterOverflow) {
		t.Fatalf("logicalBlockIOCounters() error = %v, want %v", err, errCounterOverflow)
	}
}

func TestReadIOStatsFileRejectsCounterOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.stat")
	content := "8:0 rbytes=18446744073709551615\n8:1 rbytes=1\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write io.stat fixture: %v", err)
	}
	if _, _, _, _, err := readIOStatsFile(path); !errors.Is(err, errCounterOverflow) {
		t.Fatalf("readIOStatsFile() error = %v, want %v", err, errCounterOverflow)
	}
}

func TestTransitionUserCgroupRollsBackLogicalCounterOverflow(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	basePath := filepath.Join(root, cfg.CgroupBase)
	oldPath := filepath.Join(basePath, "user_1000")
	newPath := filepath.Join(basePath, "limited", "user_1000")
	writeFakeCgroupFiles(t, oldPath, 1, 0)
	writeFakeCgroupFiles(t, newPath, 100, 0)

	manager := &Manager{
		cfg:                cfg,
		createdCgroups:     map[int]string{1000: oldPath},
		createdCgroupsFile: filepath.Join(root, "cgroups.txt"),
		processOrigins:     make(map[int]processOrigin),
		processOriginsFile: filepath.Join(root, "origins.json"),
		blockIOAccounting: map[int]blockIOAccountingState{
			1000: {path: oldPath, offset: blockIOCounters{readOps: math.MaxUint64}},
		},
		scanProcessIDs: func() (map[int][]int, error) { return map[int][]int{}, nil },
		removeManagedCgroup: func(path string) (cgroupRemovalResult, error) {
			for _, name := range []string{"cgroup.procs", "io.stat", "cpu.weight"} {
				if err := os.Remove(filepath.Join(path, name)); err != nil && !os.IsNotExist(err) {
					return cgroupRemovalResult{}, err
				}
			}
			return cgroupRemovalResult{}, os.Remove(path)
		},
	}

	_, err := manager.transitionUserCgroup(1000, oldPath, newPath, normalCPUQuota, nil)
	if !errors.Is(err, errCounterOverflow) {
		t.Fatalf("transitionUserCgroup() error = %v, want %v", err, errCounterOverflow)
	}
	if got, ok := manager.getCgroupPath(1000); !ok || got != oldPath {
		t.Fatalf("tracked path after rollback = %q, %t; want %q, true", got, ok, oldPath)
	}
}

func TestTransitionUserCgroupCarriesFinalSourceCountersIntoDestination(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	basePath := filepath.Join(root, cfg.CgroupBase)
	oldPath := filepath.Join(basePath, "user_1000")
	newPath := filepath.Join(basePath, "limited", "user_1000")
	writeFakeCgroupFiles(t, oldPath, 15, 25)
	writeFakeCgroupFiles(t, newPath, 100, 200)

	manager := &Manager{
		cfg:                cfg,
		createdCgroups:     map[int]string{1000: oldPath},
		createdCgroupsFile: filepath.Join(root, "cgroups.txt"),
		processOrigins:     make(map[int]processOrigin),
		processOriginsFile: filepath.Join(root, "origins.json"),
		blockIOAccounting:  map[int]blockIOAccountingState{1000: {path: oldPath, base: blockIOCounters{readOps: 10, writeOps: 20}}},
		scanProcessIDs:     func() (map[int][]int, error) { return map[int][]int{}, nil },
		removeManagedCgroup: func(path string) (cgroupRemovalResult, error) {
			for _, name := range []string{"cgroup.procs", "io.stat", "cpu.weight"} {
				if err := os.Remove(filepath.Join(path, name)); err != nil && !os.IsNotExist(err) {
					return cgroupRemovalResult{}, err
				}
			}
			return cgroupRemovalResult{}, os.Remove(path)
		},
	}

	if _, err := manager.transitionUserCgroup(1000, oldPath, newPath, normalCPUQuota, nil); err != nil {
		t.Fatalf("transitionUserCgroup() error: %v", err)
	}
	if got, ok := manager.getCgroupPath(1000); !ok || got != newPath {
		t.Fatalf("tracked path = %q, %t; want %q, true", got, ok, newPath)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path still exists or returned unexpected error: %v", err)
	}
	writeTestIOStat(t, newPath, 107, 211)
	logical, err := manager.logicalBlockIOCounters(1000)
	if err != nil {
		t.Fatalf("logicalBlockIOCounters() error: %v", err)
	}
	if logical.readOps != 12 || logical.writeOps != 16 {
		t.Fatalf("logical counters after real transition = %+v, want 12/16 ops", logical)
	}
}

func TestLogicalBlockIOCountersRetriesReadFailureAfterPlacementChange(t *testing.T) {
	oldPath := "/old/user_1000"
	newPath := "/new/user_1000"
	manager := &Manager{
		blockIOAccounting: map[int]blockIOAccountingState{
			1000: {path: oldPath, base: blockIOCounters{readOps: 10}},
		},
	}
	reads := 0
	manager.readBlockIOStats = func(path string) (blockIOCounters, error) {
		reads++
		if path == oldPath {
			manager.blockIOMu.Lock()
			manager.blockIOAccounting[1000] = blockIOAccountingState{
				path:   newPath,
				base:   blockIOCounters{readOps: 100},
				offset: blockIOCounters{readOps: 5},
			}
			manager.blockIOMu.Unlock()
			return blockIOCounters{}, &os.PathError{Op: "read", Path: filepath.Join(oldPath, "io.stat"), Err: os.ErrNotExist}
		}
		return blockIOCounters{readOps: 107}, nil
	}

	logical, err := manager.logicalBlockIOCounters(1000)
	if err != nil {
		t.Fatalf("logicalBlockIOCounters() error: %v", err)
	}
	if reads != 2 || logical.readOps != 12 {
		t.Fatalf("reads=%d logical=%+v, want 2 reads and 12 read operations", reads, logical)
	}
}

func TestCleanupAlternateUserCgroupDefersPopulatedSplitAndRetries(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	basePath := filepath.Join(root, cfg.CgroupBase)
	desiredPath := filepath.Join(basePath, "user_1000")
	alternatePath := filepath.Join(basePath, "limited", "user_1000")
	writeFakeCgroupFiles(t, desiredPath, 0, 0)
	writeFakeCgroupFiles(t, alternatePath, 0, 0)
	if err := os.WriteFile(filepath.Join(alternatePath, "cgroup.procs"), []byte("123\n"), 0644); err != nil {
		t.Fatalf("write populated alternate cgroup.procs: %v", err)
	}
	manager := &Manager{
		cfg: cfg,
		removeManagedCgroup: func(path string) (cgroupRemovalResult, error) {
			for _, name := range []string{"cgroup.procs", "io.stat", "cpu.weight"} {
				if err := os.Remove(filepath.Join(path, name)); err != nil && !os.IsNotExist(err) {
					return cgroupRemovalResult{}, err
				}
			}
			return cgroupRemovalResult{}, os.Remove(path)
		},
	}

	err := manager.cleanupAlternateUserCgroup(1000, desiredPath)
	var incomplete *UserCgroupPlacementIncompleteError
	if !errors.As(err, &incomplete) || incomplete.Processes != 1 {
		t.Fatalf("cleanup error = %v, want one-process placement-incomplete error", err)
	}
	if _, err := os.Stat(alternatePath); err != nil {
		t.Fatalf("populated alternate was removed instead of deferred: %v", err)
	}

	if err := os.WriteFile(filepath.Join(alternatePath, "cgroup.procs"), nil, 0644); err != nil {
		t.Fatalf("clear alternate cgroup.procs: %v", err)
	}
	if err := manager.cleanupAlternateUserCgroup(1000, desiredPath); err != nil {
		t.Fatalf("cleanup retry error: %v", err)
	}
	if _, err := os.Stat(alternatePath); !os.IsNotExist(err) {
		t.Fatalf("empty alternate still exists or returned unexpected error: %v", err)
	}
}
