package layout

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReloadOperatorSurfacesNameOutcomeMetricsAndSystemdBoundary(t *testing.T) {
	root := repositoryRoot(t)
	requiredByPath := map[string][]string{
		"docs/UPGRADING.md": {
			"systemctl reload resman",
			"confirms only that systemd delivered `SIGHUP`",
			"resman_config_reload_state",
			"resman_config_reload_last_attempt_timestamp_seconds",
			"resman_config_reload_last_success_timestamp_seconds",
			"the file on disk is pending while the previous acknowledged epoch remains active",
		},
		"docs/resman.8": {
			"resman_config_reload_state{state}",
			"resman_config_reload_last_attempt_timestamp_seconds",
			"prior epoch remains active",
		},
	}
	for path, required := range requiredByPath {
		content := readTextFile(t, filepath.Join(root, path))
		for _, fragment := range required {
			if !strings.Contains(content, fragment) {
				t.Errorf("%s is missing reload contract %q", path, fragment)
			}
		}
	}
}
