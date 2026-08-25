package layout

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPackageSourcesDeclareRestrictiveLayout(t *testing.T) {
	root := repositoryRoot(t)
	tests := []struct {
		path      string
		required  []string
		forbidden []string
	}{
		{
			path: "packaging/deb/prepare-package.sh",
			required: []string{
				`install -d -m 0700`,
				`"$package_dir/etc/resman"`,
				`"$package_dir/var/lib/resman"`,
				`install -m 0600 "$project_dir/config/resman.conf.example" "$package_dir/etc/resman/resman.conf"`,
			},
			forbidden: []string{`"$package_dir/etc/` + `resman.conf"`},
		},
		{
			path: "packaging/deb/conffiles",
			required: []string{
				"/etc/resman/resman.conf",
			},
		},
		{
			path: "packaging/rpm/resman.spec",
			required: []string{
				"%dir %attr(0700,root,root) %{_sysconfdir}/resman",
				"%config(noreplace) %attr(0600,root,root) %{_sysconfdir}/resman/resman.conf",
				"%dir %attr(0700,root,root) %{_sharedstatedir}/resman",
			},
		},
		{
			path: "Makefile",
			required: []string{
				"CONF_DIR = /etc/resman",
				"STATE_DIR = /var/lib/resman",
				"sudo install -d -m 0700 $(CONF_DIR) $(STATE_DIR)",
				"sudo install -m 0600 config/resman.conf.example $(CONF_DIR)/resman.conf",
			},
		},
		{
			path: "packaging/layout/verify-package-layout.sh",
			required: []string{
				"dpkg-deb --fsys-tarfile",
				"tar -tf -",
				"assert_absent_path '/etc/" + "resman.conf' \"$paths\"",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			content := readTextFile(t, filepath.Join(root, tt.path))
			for _, required := range tt.required {
				if !strings.Contains(content, required) {
					t.Errorf("%s is missing %q", tt.path, required)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(content, forbidden) {
					t.Errorf("%s retained forbidden layout %q", tt.path, forbidden)
				}
			}
		})
	}
}

func TestPackagePostInstallMessagesNameEveryLegacyArtifactAndRemedy(t *testing.T) {
	root := repositoryRoot(t)
	for _, path := range []string{"packaging/deb/postinst", "packaging/rpm/resman.spec"} {
		t.Run(path, func(t *testing.T) {
			content := readTextFile(t, filepath.Join(root, path))
			for _, required := range []string{
				"install -d -m 0700 -o root -g root /etc/resman /etc/resman/tls /var/lib/resman",
				"/etc/" + "resman.conf",
				"/etc/" + "resman.conf.rpmsave",
				"/etc/" + "resman.conf.backup",
				"/etc/" + "resman.conf.tmp",
				"/etc/" + "resman.conf.backup_*",
				"/etc/resman/" + "metrics.db",
				"/etc/resman/resman.conf",
				"/var/lib/resman/metrics.db",
				"securely remove",
				"Archive or delete",
			} {
				if !strings.Contains(content, required) {
					t.Errorf("%s does not name required legacy action %q", path, required)
				}
			}
		})
	}
}

func TestLiveTreeContainsOnlyAllowlistedLegacyLayoutReferences(t *testing.T) {
	root := repositoryRoot(t)
	legacyConfig := "/etc/" + "resman.conf"
	legacySaved := legacyConfig + ".rpmsave"
	legacyBackup := legacyConfig + ".backup"
	legacyTemp := legacyConfig + ".tmp"
	legacyTimestampedBackup := legacyConfig + ".backup_"
	legacyDB := "/etc/resman/" + "metrics.db"
	legacyRuntime := "/var/run/" + "resman-cgroups.txt"

	expected := map[string]map[string]int{
		"config/paths.go": {
			legacyConfig:            1,
			legacySaved:             1,
			legacyBackup:            1,
			legacyTemp:              1,
			legacyTimestampedBackup: 1,
			legacyDB:                1,
		},
		"docs/TECHNICAL-SPECIFICATION.md": {
			legacyConfig:            1,
			legacySaved:             1,
			legacyBackup:            1,
			legacyTemp:              1,
			legacyTimestampedBackup: 1,
			legacyDB:                1,
		},
		"docs/resman.8": {
			legacyConfig:            1,
			legacySaved:             1,
			legacyBackup:            1,
			legacyTemp:              1,
			legacyTimestampedBackup: 1,
			legacyDB:                1,
		},
		"packaging/deb/postinst": {
			legacyConfig:            2,
			legacySaved:             2,
			legacyBackup:            2,
			legacyTemp:              2,
			legacyTimestampedBackup: 2,
			legacyDB:                3,
		},
		"packaging/rpm/resman.spec": {
			legacyConfig:            2,
			legacySaved:             2,
			legacyBackup:            2,
			legacyTemp:              2,
			legacyTimestampedBackup: 2,
			legacyDB:                3,
		},
		"packaging/layout/verify-package-layout.sh": {
			legacyConfig:            1,
			legacySaved:             1,
			legacyBackup:            1,
			legacyTemp:              1,
			legacyTimestampedBackup: 1,
			legacyDB:                1,
		},
	}
	legacyPaths := []string{legacyConfig, legacySaved, legacyBackup, legacyTemp, legacyTimestampedBackup, legacyDB, legacyRuntime}
	observed := make(map[string]map[string]int)

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel == ".git" || rel == ".beads" || rel == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.IndexByte(string(content), 0) >= 0 {
			return nil
		}
		text := string(content)
		if rel == "packaging/rpm/resman.spec" {
			text = strings.SplitN(text, "%changelog", 2)[0]
		}
		for _, legacyPath := range legacyPaths {
			count := exactPathCount(text, legacyPath)
			if count == 0 {
				continue
			}
			if observed[rel] == nil {
				observed[rel] = make(map[string]int)
			}
			observed[rel][legacyPath] = count
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan repository layout references: %v", err)
	}

	for path, counts := range observed {
		for legacyPath, count := range counts {
			if want := expected[path][legacyPath]; count != want {
				t.Errorf("%s contains %d live references to %s, want %d", path, count, legacyPath, want)
			}
		}
	}
	for path, counts := range expected {
		for legacyPath, want := range counts {
			if got := observed[path][legacyPath]; got != want {
				t.Errorf("%s contains %d live references to %s, want %d", path, got, legacyPath, want)
			}
		}
	}
}

func exactPathCount(content, path string) int {
	count := strings.Count(content, path)
	legacyConfig := "/etc/" + "resman.conf"
	if path == legacyConfig {
		count -= strings.Count(content, legacyConfig+".backup")
		count -= strings.Count(content, legacyConfig+".rpmsave")
		count -= strings.Count(content, legacyConfig+".tmp")
	}
	if path == legacyConfig+".backup" {
		count -= strings.Count(content, legacyConfig+".backup_")
	}
	return count
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the layout test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) error = %v", path, err)
	}
	if strings.IndexByte(string(content), 0) >= 0 {
		t.Fatalf("%s is not a text file", path)
	}
	return string(content)
}
