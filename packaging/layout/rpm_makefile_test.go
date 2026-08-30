package layout

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRPMBuildDirectoryControlsEveryRPMBuildPath(t *testing.T) {
	root := repositoryRoot(t)
	tests := []struct {
		name     string
		override bool
	}{
		{name: "explicit override", override: true},
		{name: "default below home"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			topdir := filepath.Join(home, "rpmbuild")
			args := []string{"--no-print-directory", "--dry-run", "-f", filepath.Join(root, "Makefile"), "rpm"}
			if tt.override {
				topdir = filepath.Join(t.TempDir(), "custom-rpmbuild")
				args = append(args, "RPMBUILD_DIR="+topdir)
			}

			cmd := exec.Command("make", args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make --dry-run rpm failed: %v\n%s", err, output)
			}

			buildPlan := string(output)
			required := []string{
				filepath.Join(topdir, "BUILD"),
				filepath.Join(topdir, "RPMS"),
				filepath.Join(topdir, "SOURCES", "resman-1.30.8.tar.gz"),
				filepath.Join(topdir, "SPECS", "resman.spec"),
				filepath.Join(topdir, "SRPMS"),
				`rpmbuild --define "_topdir ` + topdir + `" -ba ` + filepath.Join(topdir, "SPECS", "resman.spec"),
			}
			for _, fragment := range required {
				if !strings.Contains(buildPlan, fragment) {
					t.Errorf("RPM build plan is missing %q:\n%s", fragment, buildPlan)
				}
			}

			if tt.override {
				defaultTopdir := filepath.Join(home, "rpmbuild")
				if strings.Contains(buildPlan, defaultTopdir) {
					t.Errorf("explicit RPMBUILD_DIR leaked default topdir %q:\n%s", defaultTopdir, buildPlan)
				}
			}
		})
	}
}
