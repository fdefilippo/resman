package systemd

import (
	"os"
	"strings"
	"testing"
)

func TestServiceDelegatesControllerSetupToResourceManager(t *testing.T) {
	unit, err := os.ReadFile("resman.service")
	if err != nil {
		t.Fatalf("read resman.service: %v", err)
	}
	contents := string(unit)

	forbidden := []struct {
		name  string
		value string
	}{
		{name: "direct subtree control write", value: "cgroup.subtree_control"},
		{name: "mandatory cpuset activation", value: `echo "+cpuset"`},
	}
	for _, tt := range forbidden {
		t.Run(tt.name, func(t *testing.T) {
			if strings.Contains(contents, tt.value) {
				t.Fatalf("resman.service contains %q; controller capability setup belongs to the daemon", tt.value)
			}
		})
	}

	const start = "ExecStart=/usr/bin/resman --config /etc/resman/resman.conf"
	if !strings.Contains(contents, start) {
		t.Fatalf("resman.service does not start the capability-aware daemon with %q", start)
	}
	for _, required := range []string{
		"RequiresMountsFor=/usr/bin/resman /etc/resman/resman.conf /var/lib/resman",
		"ReadWritePaths=/etc/resman",
		"ReadWritePaths=/var/lib/resman",
	} {
		if !strings.Contains(contents, required) {
			t.Errorf("resman.service does not contain required layout contract %q", required)
		}
	}
}
