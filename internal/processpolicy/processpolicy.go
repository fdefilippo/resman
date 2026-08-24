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
	Name            string
	Enforceable     bool
	IdentityTrusted bool
}

// Evaluate returns one canonical process identity and whether that process may
// contribute to decision inputs and be moved into a limited cgroup.
func Evaluate(cfg *config.Config, executable, comm string) Selection {
	name, trusted := executableName(executable)
	if !trusted {
		return Selection{
			Name:            CanonicalName("", comm),
			Enforceable:     true,
			IdentityTrusted: false,
		}
	}
	enforceable := cfg == nil || !cfg.IsProcessExcluded(name)
	return Selection{Name: name, Enforceable: enforceable, IdentityTrusted: true}
}

// CanonicalName returns the executable basename when it is available and the
// kernel comm value otherwise. The fallback is display-only: Evaluate never
// uses it to exclude a process from enforcement.
func CanonicalName(executable, comm string) string {
	if name, trusted := executableName(executable); trusted {
		return name
	}
	if name := strings.TrimSpace(comm); name != "" {
		return name
	}
	return "unknown"
}

func executableName(executable string) (string, bool) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return "", false
	}
	executable = strings.TrimSuffix(executable, " (deleted)")
	name := filepath.Base(executable)
	if name == "." || name == string(filepath.Separator) {
		return "", false
	}
	return name, true
}
