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
	const mounts = "RequiresMountsFor=/usr/bin/resman /etc/resman/resman.conf /etc/resman/cpu-points.map /var/lib/resman"
	if !strings.Contains(contents, mounts) {
		t.Errorf("resman.service does not contain required layout contract %q", mounts)
	}
}

func TestServiceContainsNoIneffectiveLayoutOrHardeningTemplate(t *testing.T) {
	contents := readService(t)

	if containsActiveDirective(contents, "RuntimeDirectory") {
		t.Error("resman.service contains active RuntimeDirectory although no shipped path consumes or enables it")
	}
	for _, inertTemplate := range []string{"#ProtectSystem=", "#NoNewPrivileges=", "#CPUAccounting="} {
		if strings.Contains(contents, inertTemplate) {
			t.Errorf("resman.service retains inert commented template %q", inertTemplate)
		}
	}
}

// TestServiceBoundsThePrivilegeSurface pins the sandboxing the daemon runs
// under. ResMan stays root because systemd authorizes unit-property mutation
// by uid through polkit rather than by a Linux capability, so the bounding set
// and the read-only file system are the boundary that remains.
func TestServiceBoundsThePrivilegeSurface(t *testing.T) {
	contents := readService(t)
	tests := []struct {
		name      string
		directive string
		want      string
	}{
		{
			name:      "capabilities the enabled features use",
			directive: "CapabilityBoundingSet",
			want:      "CAP_SYS_PTRACE CAP_DAC_READ_SEARCH CAP_SETUID CAP_SETGID CAP_KILL CAP_NET_BIND_SERVICE CAP_SYS_RESOURCE",
		},
		{name: "read-only usr, boot and etc", directive: "ProtectSystem", want: "full"},
		{name: "single writable path below etc", directive: "ReadWritePaths", want: "/etc/resman"},
		{name: "no home visibility", directive: "ProtectHome", want: "yes"},
		{name: "private temporary directories", directive: "PrivateTmp", want: "yes"},
		{name: "no module loading", directive: "ProtectKernelModules", want: "yes"},
		{name: "no kernel log access", directive: "ProtectKernelLogs", want: "yes"},
		{name: "no clock changes", directive: "ProtectClock", want: "yes"},
		{name: "no realtime scheduling", directive: "RestrictRealtime", want: "yes"},
		{name: "no namespace creation", directive: "RestrictNamespaces", want: "yes"},
		{name: "locked personality", directive: "LockPersonality", want: "yes"},
		{name: "native syscall architecture", directive: "SystemCallArchitectures", want: "native"},
		{
			name:      "address families the daemon dials",
			directive: "RestrictAddressFamilies",
			want:      "AF_UNIX AF_INET AF_INET6 AF_NETLINK",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := activeDirectiveValues(contents, "Service", tt.directive)
			if len(values) != 1 || values[0] != tt.want {
				t.Fatalf("Service.%s values = %v, want exactly [%s]", tt.directive, values, tt.want)
			}
		})
	}
}

// TestServiceKeepsObservationSandboxExclusions guards the four sandboxing
// directives that would silently disable a shipped feature. They are refused
// here rather than carried as inert comments, so the exclusion survives an edit
// without reintroducing an ineffective template.
func TestServiceKeepsObservationSandboxExclusions(t *testing.T) {
	contents := readService(t)
	exclusions := []struct {
		directive string
		reason    string
	}{
		{
			directive: "PrivateDevices",
			reason:    "weighted-I/O device resolution stats entries in /dev and reads /sys/block",
		},
		{
			directive: "ProtectControlGroups",
			reason:    "the PSI watcher opens the cgroup pressure files read-write to register a poll trigger",
		},
		{
			directive: "ProtectProc",
			reason:    "the collector reads /proc/PID/io, /proc/PID/smaps_rollup and /proc/PID/exe for every user",
		},
		{
			directive: "ProtectKernelTunables",
			reason:    "it mounts /sys read-only, which would silently downgrade PSI event mode to polling",
		},
	}
	for _, exclusion := range exclusions {
		if containsActiveDirective(contents, exclusion.directive) {
			t.Errorf("resman.service activates %s: %s", exclusion.directive, exclusion.reason)
		}
	}
}

func TestServiceRestartContractDistinguishesPermanentAndTransientFailures(t *testing.T) {
	contents := readService(t)
	tests := []struct {
		name      string
		section   string
		directive string
		want      string
	}{
		{name: "bounded retry interval", section: "Unit", directive: "StartLimitIntervalSec", want: "60"},
		{name: "bounded retry burst", section: "Unit", directive: "StartLimitBurst", want: "3"},
		{name: "retry failures only", section: "Service", directive: "Restart", want: "on-failure"},
		{name: "retry delay", section: "Service", directive: "RestartSec", want: "10"},
		{name: "permanent startup status", section: "Service", directive: "RestartPreventExitStatus", want: "78"},
		{name: "outer shutdown deadline", section: "Service", directive: "TimeoutStopSec", want: "75s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := activeDirectiveValues(contents, tt.section, tt.directive)
			if len(values) != 1 || values[0] != tt.want {
				t.Fatalf("%s.%s values = %v, want exactly [%s]", tt.section, tt.directive, values, tt.want)
			}
		})
	}
}

func TestServiceStartWaitsForDaemonReadiness(t *testing.T) {
	contents := readService(t)
	tests := []struct {
		name      string
		directive string
		want      string
	}{
		{name: "notification service type", directive: "Type", want: "notify"},
		{name: "only main process may notify", directive: "NotifyAccess", want: "main"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := activeDirectiveValues(contents, "Service", tt.directive)
			if len(values) != 1 || values[0] != tt.want {
				t.Fatalf("Service.%s values = %v, want exactly [%s]", tt.directive, values, tt.want)
			}
		})
	}
}

func activeDirectiveValues(contents, section, directive string) []string {
	currentSection := ""
	var values []string
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}
		if currentSection != section {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == directive {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
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
