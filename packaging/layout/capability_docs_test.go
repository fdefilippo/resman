package layout

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCapabilityDocumentationCoversStartupAndReloadOutcomes(t *testing.T) {
	root := repositoryRoot(t)
	tests := []struct {
		path     string
		required []string
	}{
		{
			path: "docs/resman.8",
			required: []string{
				"A missing required interface causes startup to fail",
				"Capability discovery is a startup snapshot",
				"publishes the effective configuration with the feature still disabled",
				"restart resman to discover the repaired interface",
				"merely disabled in the startup configuration can be enabled by reload without a restart",
			},
		},
		{
			path: "docs/TECHNICAL-SPECIFICATION.md",
			required: []string{
				"A missing required interface aborts startup",
				"Capability discovery is a startup snapshot",
				"publishes the effective configuration with the feature still disabled",
				"resman must be restarted to discover the repaired interface",
				"merely disabled in the startup configuration",
				"a reload can enable the feature without a restart",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			content := strings.Join(strings.Fields(readTextFile(t, filepath.Join(root, tt.path))), " ")
			for _, required := range tt.required {
				if !strings.Contains(content, required) {
					t.Errorf("%s is missing capability contract %q", tt.path, required)
				}
			}
		})
	}
}
