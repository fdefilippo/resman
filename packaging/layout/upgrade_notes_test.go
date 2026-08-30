package layout

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUpgradeGuideCoversBreakingContracts(t *testing.T) {
	root := repositoryRoot(t)
	guide := readTextFile(t, filepath.Join(root, "docs/UPGRADING.md"))

	required := []string{
		"# Upgrading from ResMan 1.25.x to ResMan 1.30.8",
		"/etc/" + "resman.conf.rpmsave",
		"/etc/" + "resman.conf.backup_*",
		"/var/lib/resman/metrics.db",
		"schema version is 3",
		"CPU_QUOTA_LIMITED",
		"PROMETHEUS_METRICS_BIND_HOST",
		"METRICS_CACHE_TTL",
		"revision `2026-07-28`",
		"`MCP_TLS_CA_FILE`",
		"`/health`",
		"`set_user_include_list`",
		"`observed_users_cpu_usage`",
		"`cpu_max_available`",
		"`permission_denied`",
		"constant `hostname`",
		"`resman_cpu_eligible_users_*`",
		"ResManCPULimitsNotActivating",
		"resman_procfs_unavailable_processes",
		"RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT",
		"`process_membership/origin_unavailable`",
		"`PROCESS_EXCLUDE_LIST`",
		"`POLLING_INTERVAL`",
		"`IO_READ_BPS`",
		"`IO_READ_IOPS`",
		"continuous eligible-user churn",
		"`MIN_ACTIVE_TIME`",
		"`memory.max`",
		"status 78",
		"`CGROUP_OPERATION_TIMEOUT`",
		"rootful Podman",
	}
	for _, text := range required {
		if !strings.Contains(guide, text) {
			t.Errorf("upgrade guide is missing contract %q", text)
		}
	}

	visible := strings.Count(guide, "**Visible change.**")
	causes := strings.Count(guide, "**Cause.**")
	actions := strings.Count(guide, "**Action.**")
	if visible < 20 {
		t.Errorf("upgrade guide has only %d structured discontinuities, want at least 20", visible)
	}
	if visible != causes || visible != actions {
		t.Errorf("upgrade guide structure differs: visible=%d causes=%d actions=%d", visible, causes, actions)
	}
	if strings.Contains(guide, "resman-4pw") {
		t.Error("operator upgrade guide must describe contracts, not tracker issue numbers")
	}
}

func TestUpgradeGuideIsReferencedAndPackaged(t *testing.T) {
	root := repositoryRoot(t)
	tests := []struct {
		path     string
		required []string
	}{
		{
			path:     "README.md",
			required: []string{"docs/UPGRADING.md", "Upgrading from 1.25.x to 1.30.8 is intentionally breaking"},
		},
		{
			path:     "docs/resman.8",
			required: []string{"/usr/share/doc/resman/UPGRADING.md", "Upgrade from 1.25.x to 1.30.8"},
		},
		{
			path:     "packaging/deb/control.in",
			required: []string{"Read /usr/share/doc/resman/UPGRADING.md before upgrading from 1.25.x to 1.30.8"},
		},
		{
			path:     "packaging/rpm/resman.spec",
			required: []string{"Read /usr/share/doc/resman/UPGRADING.md before upgrading from 1.25.x to 1.30.8"},
		},
		{
			path: "packaging/deb/prepare-package.sh",
			required: []string{
				`install -m 0644 "$project_dir/docs/UPGRADING.md"`,
				`"$package_dir/usr/share/doc/resman/UPGRADING.md"`,
			},
		},
		{
			path: "packaging/layout/verify-package-layout.sh",
			required: []string{
				`assert_entry '/usr/share/doc/resman/UPGRADING.md' '-rw-r--r--' 'root/root'`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			content := readTextFile(t, filepath.Join(root, tt.path))
			for _, required := range tt.required {
				if !strings.Contains(content, required) {
					t.Errorf("%s is missing upgrade reference %q", tt.path, required)
				}
			}
		})
	}
}
