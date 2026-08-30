// Package processidentity resolves the executable identity used by process policy.
package processidentity

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Identity contains the trusted executable path and the display-only kernel name.
type Identity struct {
	Executable string
	Comm       string
}

// ExecutableUnavailableError reports that /proc/PID/exe could not be resolved.
// Comm remains available for diagnostics but must never become a policy identity.
type ExecutableUnavailableError struct {
	PID int
	Err error
}

func (e *ExecutableUnavailableError) Error() string {
	return fmt.Sprintf("trusted executable identity unavailable for PID %d: %v", e.PID, e.Err)
}

func (e *ExecutableUnavailableError) Unwrap() error {
	return e.Err
}

// Read resolves one process identity from procfs. Failure to read comm is not
// fatal when exe is trustworthy because comm is used only for diagnostics.
func Read(procRoot string, pid int) (Identity, error) {
	processPath := filepath.Join(procRoot, strconv.Itoa(pid))
	var identity Identity
	if data, err := os.ReadFile(filepath.Join(processPath, "comm")); err == nil {
		identity.Comm = string(data)
	}
	executable, err := os.Readlink(filepath.Join(processPath, "exe"))
	if err != nil {
		return identity, &ExecutableUnavailableError{PID: pid, Err: err}
	}
	identity.Executable = executable
	return identity, nil
}
