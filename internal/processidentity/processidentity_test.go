package processidentity

import (
	"errors"
	"os"
	"path/filepath"
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
