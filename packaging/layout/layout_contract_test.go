package layout

import (
	"fmt"
	"os"
	"os/exec"
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
				`install -m 0600 "$project_dir/config/cpu-points.map.example" "$package_dir/etc/resman/cpu-points.map"`,
				`install -m 0644 "$project_dir/docs/CONFIGURATION.md"`,
				`"$package_dir/usr/share/doc/resman/CONFIGURATION.md"`,
				`install -m 0644 "$project_dir/docs/UPGRADING.md"`,
				`"$package_dir/usr/share/doc/resman/UPGRADING.md"`,
				`install -m 0644 "$project_dir/docs/LIMIT-HOOKS.md"`,
				`"$package_dir/usr/share/doc/resman/LIMIT-HOOKS.md"`,
				`install -m 0755 "$project_dir/scripts/sendmail.sh"`,
				`"$package_dir/usr/share/doc/resman/scripts/sendmail.sh"`,
				`install -m 0644 "$project_dir/docs/scripts/README.md"`,
				`"$package_dir/usr/share/doc/resman/scripts/README.md"`,
			},
			forbidden: []string{`"$package_dir/etc/` + `resman.conf"`},
		},
		{
			path: "packaging/deb/conffiles",
			required: []string{
				"/etc/resman/resman.conf",
				"/etc/resman/cpu-points.map",
			},
		},
		{
			path: "packaging/rpm/resman.spec",
			required: []string{
				"%dir %attr(0700,root,root) %{_sysconfdir}/resman",
				"%config(noreplace) %attr(0600,root,root) %{_sysconfdir}/resman/resman.conf",
				"%config(noreplace) %attr(0600,root,root) %{_sysconfdir}/resman/cpu-points.map",
				"%dir %attr(0700,root,root) %{_sharedstatedir}/resman",
				"chmod a-x,o-rwx /var/log/resman.log",
				"install -m 0600 -o root -g root /dev/null /var/log/resman.log",
				"if [ -f /var/log/resman.log ] && [ ! -L /var/log/resman.log ]; then",
				"install -m 644 docs/CONFIGURATION.md %{buildroot}/%{_docdir}/%{name}/",
				"%doc %{_docdir}/%{name}/CONFIGURATION.md",
				"install -m 644 docs/UPGRADING.md %{buildroot}/%{_docdir}/%{name}/",
				"%doc %{_docdir}/%{name}/UPGRADING.md",
				"install -m 644 docs/LIMIT-HOOKS.md %{buildroot}/%{_docdir}/%{name}/",
				"%doc %{_docdir}/%{name}/LIMIT-HOOKS.md",
				"install -m 755 scripts/sendmail.sh %{buildroot}/%{_docdir}/%{name}/scripts/sendmail.sh",
				"install -m 644 docs/scripts/README.md %{buildroot}/%{_docdir}/%{name}/scripts/README.md",
			},
			forbidden: []string{"chmod 644 /var/log/resman.log"},
		},
		{
			path: "packaging/deb/postinst",
			required: []string{
				"chmod a-x,o-rwx /var/log/resman.log",
				"install -m 0600 -o root -g root /dev/null /var/log/resman.log",
				"if [ -f /var/log/resman.log ] && [ ! -L /var/log/resman.log ]; then",
			},
		},
		{
			path:     "packaging/syslog/resman.conf",
			required: []string{`fileCreateMode="0600"`},
			forbidden: []string{
				`fileCreateMode="0640"`,
				`fileCreateMode="0644"`,
			},
		},
		{
			path:      "packaging/syslog/resman",
			required:  []string{"create 0600 root root"},
			forbidden: []string{"create 0640", "create 0644"},
		},
		{
			path: "Makefile",
			required: []string{
				"CONF_DIR = /etc/resman",
				"STATE_DIR = /var/lib/resman",
				"sudo install -d -m 0700 $(CONF_DIR) $(STATE_DIR)",
				"sudo install -m 0600 config/resman.conf.example $(CONF_DIR)/resman.conf",
				"sudo test -e $(CONF_DIR)/cpu-points.map || sudo test -L $(CONF_DIR)/cpu-points.map",
				"sudo install -m 0600 config/cpu-points.map.example $(CONF_DIR)/cpu-points.map",
			},
		},
		{
			path: "packaging/layout/verify-package-layout.sh",
			required: []string{
				"dpkg-deb --fsys-tarfile",
				"tar -tf -",
				"assert_absent_path '/etc/" + "resman.conf' \"$paths\"",
				"assert_entry '/etc/resman/cpu-points.map' '-rw-------' 'root/root' \"$listing\"",
				"assert_entry '/usr/share/doc/resman/scripts/sendmail.sh' '-rwxr-xr-x' 'root/root' \"$listing\"",
				"assert_entry '/usr/share/doc/resman/scripts/README.md' '-rw-r--r--' 'root/root' \"$listing\"",
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

func TestPackagedSendmailGuideMatchesHelperContract(t *testing.T) {
	root := repositoryRoot(t)
	guide := readTextFile(t, filepath.Join(root, "docs/scripts/README.md"))
	helper := readTextFile(t, filepath.Join(root, "scripts/sendmail.sh"))

	for _, required := range []string{
		"/usr/share/doc/resman/scripts/sendmail.sh",
		"ResMan does not invoke",
		"curl` built with SMTP support",
		"`-f address`",
		"`-t address`",
		"`-T` requests STARTTLS",
		"forwarded to `curl` on its command line",
	} {
		if !strings.Contains(guide, required) {
			t.Errorf("sendmail guide is missing %q", required)
		}
	}

	if !strings.Contains(helper, "./sendmail.sh -f sender@example.com -t recipient@example.com [options]") {
		t.Error("sendmail helper usage does not name its required sender and recipient options")
	}
}

func TestLogrotateShellBlocksParse(t *testing.T) {
	root := repositoryRoot(t)
	content := readTextFile(t, filepath.Join(root, "packaging/syslog/resman"))
	if err := validateLogrotateShellBlocks(content); err != nil {
		t.Fatalf("packaging/syslog/resman contains invalid shell: %v", err)
	}
	if strings.Contains(content, "copytruncate") && strings.Contains(content, "postrotate") {
		t.Fatal("copytruncate must not retain a redundant postrotate hook")
	}
}

func TestLogrotateShellGateRejectsDanglingContinuation(t *testing.T) {
	content := "/var/log/resman.log {\npostrotate\nsystemctl reload rsyslog || \\\n\nendscript\n}\n"
	if err := validateLogrotateShellBlocks(content); err == nil {
		t.Fatal("logrotate shell gate accepted a dangling continuation")
	}
}

func validateLogrotateShellBlocks(content string) error {
	lines := strings.Split(content, "\n")
	for index := 0; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) != "postrotate" {
			continue
		}
		start := index + 2
		var script strings.Builder
		for index++; index < len(lines) && strings.TrimSpace(lines[index]) != "endscript"; index++ {
			script.WriteString(lines[index])
			script.WriteByte('\n')
		}
		if index == len(lines) {
			return fmt.Errorf("postrotate block at line %d has no endscript", start)
		}
		command := exec.Command("bash", "-n")
		command.Stdin = strings.NewReader(script.String())
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("postrotate block at line %d: %w: %s", start, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
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
				"protected archive",
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

	allowlist := map[string]map[string]string{
		"config/paths.go": {
			legacyConfig:            "authoritative rejected legacy configuration constant",
			legacySaved:             "authoritative RPM-saved configuration constant",
			legacyBackup:            "authoritative rolling legacy backup constant",
			legacyTemp:              "authoritative fixed legacy temporary-file constant",
			legacyTimestampedBackup: "authoritative legacy backup-prefix constant",
			legacyDB:                "authoritative rejected legacy database constant",
		},
		"docs/TECHNICAL-SPECIFICATION.md": {
			legacyConfig:            "operator migration and refusal contract",
			legacySaved:             "operator migration and refusal contract",
			legacyBackup:            "operator secure-removal contract",
			legacyTemp:              "operator secure-removal contract",
			legacyTimestampedBackup: "operator secure-removal contract",
			legacyDB:                "operator database-reset contract",
		},
		"docs/resman.8": {
			legacyConfig:            "operator migration and refusal guidance",
			legacySaved:             "operator migration and refusal guidance",
			legacyBackup:            "operator secure-removal guidance",
			legacyTemp:              "operator secure-removal guidance",
			legacyTimestampedBackup: "operator secure-removal guidance",
			legacyDB:                "operator database-reset guidance",
		},
		"docs/UPGRADING.md": {
			legacyConfig:            "operator migration and refusal guidance",
			legacySaved:             "operator migration and refusal guidance",
			legacyBackup:            "operator secure-removal guidance",
			legacyTemp:              "operator secure-removal guidance",
			legacyTimestampedBackup: "operator secure-removal guidance",
			legacyDB:                "operator database-reset guidance",
			legacyRuntime:           "operator runtime-path migration guidance",
		},
		"packaging/deb/postinst": {
			legacyConfig:            "post-upgrade detection and recovery message",
			legacySaved:             "post-upgrade detection and recovery message",
			legacyBackup:            "post-upgrade detection and secure-removal message",
			legacyTemp:              "post-upgrade detection and secure-removal message",
			legacyTimestampedBackup: "post-upgrade detection and secure-removal message",
			legacyDB:                "post-upgrade database-reset message",
		},
		"packaging/rpm/resman.spec": {
			legacyConfig:            "post-upgrade detection and recovery message",
			legacySaved:             "post-upgrade detection and recovery message",
			legacyBackup:            "post-upgrade detection and secure-removal message",
			legacyTemp:              "post-upgrade detection and secure-removal message",
			legacyTimestampedBackup: "post-upgrade detection and secure-removal message",
			legacyDB:                "post-upgrade database-reset message",
		},
		"packaging/layout/verify-package-layout.sh": {
			legacyConfig:            "negative package payload assertion",
			legacySaved:             "negative package payload assertion",
			legacyBackup:            "negative package payload assertion",
			legacyTemp:              "negative package payload assertion",
			legacyTimestampedBackup: "negative package payload assertion",
			legacyDB:                "negative package payload assertion",
		},
		".github/workflows/release.yml": {
			legacyConfig:            "intentional invalid DEB member used by the release mutation test",
			legacyTemp:              "intentional invalid DEB member used by the release mutation test",
			legacyTimestampedBackup: "intentional invalid DEB member used by the release mutation test",
		},
	}
	legacyPaths := []string{legacyConfig, legacySaved, legacyBackup, legacyTemp, legacyTimestampedBackup, legacyDB, legacyRuntime}
	observed := make(map[string]map[string]bool)

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
			if !containsExactPath(text, legacyPath) {
				continue
			}
			if observed[rel] == nil {
				observed[rel] = make(map[string]bool)
			}
			observed[rel][legacyPath] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan repository layout references: %v", err)
	}

	for path, paths := range observed {
		for legacyPath := range paths {
			reason, allowed := allowlist[path][legacyPath]
			if !allowed {
				t.Errorf("%s contains a non-allowlisted live reference to %s", path, legacyPath)
			} else if strings.TrimSpace(reason) == "" {
				t.Errorf("%s allowlists %s without a reason", path, legacyPath)
			}
		}
	}
	for path, paths := range allowlist {
		for legacyPath, reason := range paths {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s allowlists %s without a reason", path, legacyPath)
			}
			if !observed[path][legacyPath] {
				t.Errorf("%s allowlists %s but no live reference was found", path, legacyPath)
			}
		}
	}
}

func containsExactPath(content, path string) bool {
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
	return count > 0
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
