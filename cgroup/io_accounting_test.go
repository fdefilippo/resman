package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/config"
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
		removeManagedCgroup: func(path string) error {
			for _, name := range []string{"cgroup.procs", "io.stat", "cpu.weight"} {
				if err := os.Remove(filepath.Join(path, name)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			return os.Remove(path)
		},
	}

	if err := manager.transitionUserCgroup(1000, oldPath, newPath, cfg.CPUQuotaNormal); err != nil {
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
		removeManagedCgroup: func(path string) error {
			for _, name := range []string{"cgroup.procs", "io.stat", "cpu.weight"} {
				if err := os.Remove(filepath.Join(path, name)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			return os.Remove(path)
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
