package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

func TestApplyCPULimitWaitsForTimedOutMoverToBecomeQuiescent(t *testing.T) {
	const uid = 1001
	userPath := filepath.Join(t.TempDir(), "user_1001")
	if err := os.Mkdir(userPath, 0755); err != nil {
		t.Fatalf("create user cgroup fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userPath, "cpu.max"), []byte("max 100000"), 0644); err != nil {
		t.Fatalf("create cpu.max fixture: %v", err)
	}

	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	var mutations atomic.Int32

	manager := &Manager{
		cfg:              config.DefaultConfig(),
		logger:           logging.GetLogger(),
		createdCgroups:   map[int]string{uid: userPath},
		operationTimeout: func() time.Duration { return 10 * time.Millisecond },
	}
	manager.moveUserProcesses = func(ctx context.Context, gotUID int) error {
		if gotUID != uid {
			return fmt.Errorf("move UID = %d, want %d", gotUID, uid)
		}
		close(started)
		<-ctx.Done()
		close(cancelObserved)
		<-release
		mutations.Add(1)
		return ctx.Err()
	}

	result := make(chan error, 1)
	go func() {
		result <- manager.ApplyCPULimit(uid, "50000 100000")
	}()

	<-started
	<-cancelObserved
	stateDone := make(chan []int, 1)
	go func() { stateDone <- manager.GetCreatedCgroups() }()
	select {
	case uids := <-stateDone:
		if len(uids) != 1 || uids[0] != uid {
			t.Fatalf("GetCreatedCgroups() = %v, want [%d]", uids, uid)
		}
	case <-time.After(time.Second):
		close(release)
		<-result
		t.Fatal("GetCreatedCgroups() blocked behind cgroup process migration")
	}
	select {
	case err := <-result:
		t.Fatalf("ApplyCPULimit returned before its mover stopped: %v", err)
	default:
	}

	close(release)
	err := <-result
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ApplyCPULimit error = %v, want context deadline exceeded", err)
	}
	if got := mutations.Load(); got != 1 {
		t.Fatalf("mover mutations at return = %d, want completed value 1", got)
	}
}

func TestApplyCPUWeightRejectsOutOfRangeWithoutCreatingACgroup(t *testing.T) {
	tests := []int{-1, 0, 10001}
	for _, weight := range tests {
		t.Run(fmt.Sprintf("weight_%d", weight), func(t *testing.T) {
			root := t.TempDir()
			cfg := config.DefaultConfig()
			cfg.CgroupRoot = root
			cfg.CgroupBase = "resman"
			manager := &Manager{
				cfg:            cfg,
				logger:         logging.GetLogger(),
				createdCgroups: make(map[int]string),
			}

			if err := manager.ApplyCPUWeight(1001, weight); err == nil {
				t.Fatalf("ApplyCPUWeight(%d) accepted", weight)
			}
			if _, err := os.Stat(manager.getUserCgroupPath(1001)); !os.IsNotExist(err) {
				t.Fatalf("invalid weight created a cgroup, stat error = %v", err)
			}
		})
	}
}

func TestApplyCPUWeightWritesExactKernelBoundaryValues(t *testing.T) {
	for _, weight := range []int{1, 10000} {
		t.Run(fmt.Sprintf("weight_%d", weight), func(t *testing.T) {
			root := t.TempDir()
			cfg := config.DefaultConfig()
			cfg.CgroupRoot = root
			cfg.CgroupBase = "resman"
			manager := &Manager{
				cfg:            cfg,
				logger:         logging.GetLogger(),
				createdCgroups: make(map[int]string),
			}

			if err := manager.ApplyCPUWeight(1001, weight); err != nil {
				t.Fatalf("ApplyCPUWeight(%d): %v", weight, err)
			}
			data, err := os.ReadFile(filepath.Join(manager.getUserCgroupPath(1001), "cpu.weight"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(data), fmt.Sprint(weight); got != want {
				t.Errorf("cpu.weight = %q, want %q", got, want)
			}
		})
	}
}
