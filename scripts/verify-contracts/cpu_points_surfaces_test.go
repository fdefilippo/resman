package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFlatCPUPointsContractRejectsRemovedPublicNames(t *testing.T) {
	for _, stale := range []string{"guaranteed_domain_cpu_weight", "best_effort_domain_cpu_usage_usec_delta", "guaranteed_priority", "cpu_points_lending_state"} {
		for _, surface := range []string{"README.md", "docs/dashboard.json", "producer.go"} {
			t.Run(stale+"/"+surface, func(t *testing.T) {
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, "docs"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("flat CPU Points"), 0600); err != nil {
					t.Fatal(err)
				}
				content := stale
				if surface == "producer.go" {
					content = "package example\nconst field = \"" + stale + "\"\n"
				}
				if err := os.WriteFile(filepath.Join(root, surface), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				sources, failures := loadGoFiles(root)
				if len(failures) != 0 {
					t.Fatal(failures)
				}
				if got := checkCPUPointsSurfaces(root, sources); len(got.findings) == 0 {
					t.Fatal("removed public contract was accepted")
				}
			})
		}
	}
}
