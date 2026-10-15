package processidentity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadReturnsTrustedExecutableAndDisplayName(t *testing.T) {
	procRoot := t.TempDir()
	processPath := filepath.Join(procRoot, "123")
	if err := os.Mkdir(processPath, 0755); err != nil {
		t.Fatalf("create process fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(processPath, "comm"), []byte("spoofed\n"), 0644); err != nil {
		t.Fatalf("write comm fixture: %v", err)
	}
	if err := os.Symlink("/usr/bin/worker", filepath.Join(processPath, "exe")); err != nil {
		t.Fatalf("create executable fixture: %v", err)
	}

	identity, err := Read(procRoot, 123)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if identity.Executable != "/usr/bin/worker" || identity.Comm != "spoofed\n" {
		t.Fatalf("Read() = %+v", identity)
	}
}

func TestReadReportsMissingExecutableWithoutPromotingComm(t *testing.T) {
	procRoot := t.TempDir()
	processPath := filepath.Join(procRoot, "456")
	if err := os.Mkdir(processPath, 0755); err != nil {
		t.Fatalf("create process fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(processPath, "comm"), []byte("systemd\n"), 0644); err != nil {
		t.Fatalf("write comm fixture: %v", err)
	}

	identity, err := Read(procRoot, 456)
	var unavailable *ExecutableUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("Read() error = %v, want ExecutableUnavailableError", err)
	}
	if identity.Executable != "" || identity.Comm != "systemd\n" {
		t.Fatalf("Read() = %+v, want display-only comm", identity)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read() error = %v, want missing executable cause", err)
	}
}

func TestVanishedRecognizesOnlyProcessExit(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error", err: nil, want: false},
		{name: "reaped PID directory", err: &os.PathError{Op: "open", Path: "/proc/42/cgroup", Err: syscall.ENOENT}, want: true},
		{name: "exiting or zombie task", err: &os.PathError{Op: "read", Path: "/proc/42/cgroup", Err: syscall.ESRCH}, want: true},
		{name: "wrapped zombie read", err: fmt.Errorf("read CPU authority process: %w", &os.PathError{Op: "read", Path: "/proc/42/cgroup", Err: syscall.ESRCH}), want: true},
		{name: "permission denied", err: &os.PathError{Op: "open", Path: "/proc/42/cgroup", Err: syscall.EACCES}, want: false},
		{name: "unrelated failure", err: errors.New("malformed cgroup line"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Vanished(tt.err); got != tt.want {
				t.Fatalf("Vanished(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}
