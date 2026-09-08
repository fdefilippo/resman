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
				filepath.Join(topdir, "SOURCES", "resman-1.35.0.tar.gz"),
				"cp scripts/sendmail.sh scripts/resman-sendmail-hook.sh resman-1.35.0/scripts/",
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

func TestRPMBuildRequiresExpandedSystemdMacros(t *testing.T) {
	root := repositoryRoot(t)
	for _, tt := range []struct {
		name    string
		invalid string
	}{
		{name: "all macros available"},
		{name: "unit directory unresolved", invalid: "unitdir"},
		{name: "post macro unresolved", invalid: "systemd_post"},
		{name: "preun macro unresolved", invalid: "systemd_preun"},
		{name: "postun macro unresolved", invalid: "systemd_postun_with_restart"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeRPM := filepath.Join(t.TempDir(), "rpm")
			script := `#!/bin/sh
case "$2" in
'%{_unitdir}') value=/usr/lib/systemd/system ;;
'%systemd_post resman.service') value='if [ "$1" -eq 1 ]; then :; fi' ;;
'%systemd_preun resman.service') value='if [ "$1" -eq 0 ]; then :; fi' ;;
'%systemd_postun_with_restart resman.service') value='if [ "$1" -ge 1 ]; then :; fi' ;;
*) exit 2 ;;
esac
`
			if tt.invalid != "" {
				script += `case "$2" in *'` + tt.invalid + `'* ) value="$2" ;; esac
`
			}
			script += `printf '%s\n' "$value"
`
			if err := os.WriteFile(fakeRPM, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("make", "--no-print-directory", "-f", filepath.Join(root, "Makefile"),
				"rpm-check", "RPM="+fakeRPM, "RPMBUILD=true")
			cmd.Dir = root
			output, err := cmd.CombinedOutput()
			if tt.invalid == "" && err != nil {
				t.Fatalf("valid macros rejected: %v\n%s", err, output)
			}
			if tt.invalid != "" && err == nil {
				t.Fatalf("unresolved %s was accepted", tt.invalid)
			}
		})
	}
}
