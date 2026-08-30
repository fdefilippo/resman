package cgroup

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestRemoveCgroupWithRetryUsing(t *testing.T) {
	busy := &os.PathError{Op: "remove", Path: "/cgroup/user", Err: syscall.EBUSY}
	absent := &os.PathError{Op: "remove", Path: "/cgroup/user", Err: syscall.ENOENT}
	denied := &os.PathError{Op: "remove", Path: "/cgroup/user", Err: syscall.EPERM}

	tests := []struct {
		name         string
		results      []error
		wantAttempts int
		wantBackoffs int
		wantRetried  bool
		wantErr      error
	}{
		{name: "first attempt succeeds", results: []error{nil}, wantAttempts: 1},
		{name: "already absent", results: []error{absent}, wantAttempts: 1},
		{name: "retry succeeds", results: []error{busy, nil}, wantAttempts: 2, wantBackoffs: 1, wantRetried: true},
		{name: "disappears before retry", results: []error{busy, absent}, wantAttempts: 2, wantBackoffs: 1, wantRetried: true},
		{name: "final failure is reported", results: []error{busy, denied}, wantAttempts: 2, wantBackoffs: 1, wantRetried: true, wantErr: syscall.EPERM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			backoffs := 0
			remove := func(string) error {
				result := tt.results[attempts]
				attempts++
				return result
			}
			result, err := removeCgroupWithRetryUsing("/cgroup/user", remove, func() { backoffs++ })

			if attempts != tt.wantAttempts {
				t.Errorf("removal attempts = %d, want %d", attempts, tt.wantAttempts)
			}
			if backoffs != tt.wantBackoffs {
				t.Errorf("backoffs = %d, want %d", backoffs, tt.wantBackoffs)
			}
			if result.retried != tt.wantRetried {
				t.Errorf("retried = %t, want %t", result.retried, tt.wantRetried)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("removeCgroupWithRetryUsing() error = %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("removeCgroupWithRetryUsing() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestRemoveManagedCgroupPathObservesOneBoundedRetrySignalWithoutStateLocks(t *testing.T) {
	denied := &os.PathError{Op: "remove", Path: "/cgroup/user_1000", Err: syscall.EPERM}
	tests := []struct {
		name        string
		result      cgroupRemovalResult
		removeErr   error
		wantSignals int
		wantErr     error
	}{
		{name: "immediate success emits no signal"},
		{name: "successful retry emits one signal", result: cgroupRemovalResult{retried: true}, wantSignals: 1},
		{name: "final failure emits one retry signal and returns one error", result: cgroupRemovalResult{retried: true}, removeErr: denied, wantSignals: 1, wantErr: syscall.EPERM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{}
			retrySignals := 0
			manager.removeManagedCgroup = func(string) (cgroupRemovalResult, error) {
				return tt.result, tt.removeErr
			}
			// The zero-argument observer cannot receive unbounded path, UID, PID, or raw-error labels.
			manager.observeRemovalRetry = func() {
				retrySignals++
				locks := []struct {
					name    string
					tryLock func() bool
					unlock  func()
				}{
					{name: "config", tryLock: manager.cfgMu.TryLock, unlock: manager.cfgMu.Unlock},
					{name: "managed cgroups", tryLock: manager.mu.TryLock, unlock: manager.mu.Unlock},
					{name: "process origins", tryLock: manager.originMu.TryLock, unlock: manager.originMu.Unlock},
					{name: "block I/O accounting", tryLock: manager.blockIOMu.TryLock, unlock: manager.blockIOMu.Unlock},
					{name: "username cache", tryLock: manager.usernameMu.TryLock, unlock: manager.usernameMu.Unlock},
				}
				for _, lock := range locks {
					if !lock.tryLock() {
						t.Errorf("retry observer ran while %s lock was held", lock.name)
						continue
					}
					lock.unlock()
				}
			}

			err := manager.removeManagedCgroupPath("/cgroup/user_1000")
			if tt.wantErr == nil && err != nil {
				t.Fatalf("removeManagedCgroupPath() error = %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("removeManagedCgroupPath() error = %v, want %v", err, tt.wantErr)
			}
			if retrySignals != tt.wantSignals {
				t.Fatalf("retry signals = %d, want %d", retrySignals, tt.wantSignals)
			}
		})
	}
}
