package systemd

import (
	"os"
	"strings"
	"testing"
)

func TestServiceDelegatesControllerSetupToResourceManager(t *testing.T) {
	contents := readService(t)

	for _, forbidden := range []string{"cgroup.subtree_control", `echo "+cpuset"`} {
		if strings.Contains(contents, forbidden) {
			t.Errorf("resman.service contains %q; controller capability setup belongs to the daemon", forbidden)
		}
	}

	const start = "ExecStart=/usr/bin/resman --config /etc/resman/resman.conf"
	if !strings.Contains(contents, start) {
		t.Fatalf("resman.service does not start the capability-aware daemon with %q", start)
	}
	const mounts = "RequiresMountsFor=/usr/bin/resman /etc/resman/resman.conf /var/lib/resman"
	if !strings.Contains(contents, mounts) {
		t.Errorf("resman.service does not contain required layout contract %q", mounts)
	}
}

func TestServiceContainsNoIneffectiveLayoutOrHardeningTemplate(t *testing.T) {
	contents := readService(t)

	for _, directive := range []string{"RuntimeDirectory", "ReadWritePaths"} {
		if containsActiveDirective(contents, directive) {
			t.Errorf("resman.service contains active %s although no shipped path consumes or enables it", directive)
		}
	}
	for _, inertTemplate := range []string{"#ProtectSystem=", "#NoNewPrivileges=", "#CPUAccounting="} {
		if strings.Contains(contents, inertTemplate) {
			t.Errorf("resman.service retains inert commented template %q", inertTemplate)
		}
	}
}

func containsActiveDirective(contents, directive string) bool {
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, directive+"=") {
			return true
		}
	}
	return false
}

func TestContainsActiveDirectiveIgnoresIndentedComments(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     bool
	}{
		{name: "active", contents: "  ReadWritePaths=/var/lib/resman", want: true},
		{name: "commented", contents: "  # ReadWritePaths=/var/lib/resman"},
		{name: "different directive", contents: "RuntimeDirectory=resman"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsActiveDirective(tt.contents, "ReadWritePaths"); got != tt.want {
				t.Fatalf("containsActiveDirective() = %t, want %t", got, tt.want)
			}
		})
	}
}

func readService(t *testing.T) string {
	t.Helper()
	unit, err := os.ReadFile("resman.service")
	if err != nil {
		t.Fatalf("read resman.service: %v", err)
	}
	return string(unit)
}
