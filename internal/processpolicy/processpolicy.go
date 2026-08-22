// Package processpolicy defines the process identity and exclusion decision
// shared by metrics accounting and cgroup placement.
package processpolicy

import (
	"path/filepath"
	"strings"

	"github.com/fdefilippo/resman/config"
)

// Selection is the normalized process policy result consumed by accounting
// and enforcement.
type Selection struct {
	Name        string
	Enforceable bool
}

// Evaluate returns one canonical process identity and whether that process may
// contribute to decision inputs and be moved into a limited cgroup.
func Evaluate(cfg *config.Config, executable, comm string) Selection {
	name := CanonicalName(executable, comm)
	enforceable := cfg == nil || !cfg.IsProcessExcluded(name)
	return Selection{Name: name, Enforceable: enforceable}
}

// CanonicalName prefers the basename resolved from /proc/PID/exe and falls
// back to the kernel comm value. It deliberately ignores argv[0] and PID
// decorations so a process cannot spoof an excluded identity through cmdline.
func CanonicalName(executable, comm string) string {
	if executable = strings.TrimSpace(executable); executable != "" {
		executable = strings.TrimSuffix(executable, " (deleted)")
		if name := filepath.Base(executable); name != "." && name != string(filepath.Separator) {
			return name
		}
	}
	if name := strings.TrimSpace(comm); name != "" {
		return name
	}
	return "unknown"
}
