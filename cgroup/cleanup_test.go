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
		wantErr      error
	}{
		{name: "first attempt succeeds", results: []error{nil}, wantAttempts: 1},
		{name: "already absent", results: []error{absent}, wantAttempts: 1},
		{name: "retry succeeds", results: []error{busy, nil}, wantAttempts: 2, wantBackoffs: 1},
		{name: "disappears before retry", results: []error{busy, absent}, wantAttempts: 2, wantBackoffs: 1},
		{name: "final failure is reported", results: []error{busy, denied}, wantAttempts: 2, wantBackoffs: 1, wantErr: syscall.EPERM},
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
			err := removeCgroupWithRetryUsing("/cgroup/user", remove, func() { backoffs++ })

			if attempts != tt.wantAttempts {
				t.Errorf("removal attempts = %d, want %d", attempts, tt.wantAttempts)
			}
			if backoffs != tt.wantBackoffs {
				t.Errorf("backoffs = %d, want %d", backoffs, tt.wantBackoffs)
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
