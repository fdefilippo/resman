package cgroup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/config"
)

func TestDetectEnforcementStatusAlwaysFailsClosedWithoutSystemdNativeAdapter(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(t *testing.T, runtimePath, procRoot string)
		wantReason string
	}{
		{
			name: "systemd runtime directory",
			prepare: func(t *testing.T, path, _ string) {
				t.Helper()
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: EnforcementReasonSystemdOwnsHostWorkloads,
		},
		{
			name: "non-systemd PID 1",
			prepare: func(t *testing.T, _, procRoot string) {
				t.Helper()
				writePIDOneComm(t, procRoot, "init")
			},
			wantReason: EnforcementReasonSystemdRuntimeAbsent,
		},
		{
			name:       "unverifiable PID 1",
			prepare:    func(*testing.T, string, string) {},
			wantReason: EnforcementReasonAuthorityUnverifiable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			runtimePath := filepath.Join(root, "systemd")
			procRoot := filepath.Join(root, "proc")
			tt.prepare(t, runtimePath, procRoot)
			got := detectEnforcementStatus(runtimePath, procRoot)
			if got.Mode != EnforcementModeObservationOnly || got.Reason != tt.wantReason {
				t.Fatalf("status = %+v, want observation_only reason=%q", got, tt.wantReason)
			}
		})
	}
}

func writePIDOneComm(t *testing.T, procRoot, comm string) {
	t.Helper()
	path := filepath.Join(procRoot, "1")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "comm"), []byte(comm+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestZeroEnforcementStatusFailsClosed(t *testing.T) {
	status := (&Manager{}).EnforcementStatus()
	if status.Mode != EnforcementModeObservationOnly || status.Reason != EnforcementReasonAuthorityUnverifiable {
		t.Fatalf("status = %+v, want fail-closed observation-only status", status)
	}
}

func TestEnforcementCycleStateUsesOnlyBoundedPublicVocabulary(t *testing.T) {
	tests := []struct {
		reason string
		want   EnforcementBlockReason
	}{
		{EnforcementReasonSystemdRuntimeAbsent, EnforcementBlockReasonSystemdRuntimeAbsent},
		{EnforcementReasonSystemdOwnsHostWorkloads, EnforcementBlockReasonSystemdOwnsWorkloads},
		{EnforcementReasonAuthorityUnverifiable, EnforcementBlockReasonAuthorityUnverifiable},
		{"unbounded detail with PID 1234", EnforcementBlockReasonAuthorityUnverifiable},
	}
	for _, tt := range tests {
		if got := BoundedEnforcementBlockReason(tt.reason); got != tt.want {
			t.Errorf("BoundedEnforcementBlockReason(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}

	got := NormalizedEnforcementCycleState(EnforcementCycleState{
		Mode:            "unexpected",
		RequestedIntent: "pid=1234",
		AppliedAction:   "executed_maybe",
		BlockReason:     "path=/sys/fs/cgroup/private",
	}, EnforcementStatus{Mode: EnforcementModeSystemdNative})
	want := EnforcementCycleState{
		Mode:            EnforcementModeSystemdNative,
		RequestedIntent: EnforcementPolicyIntentNone,
		AppliedAction:   AppliedEnforcementActionNone,
		BlockReason:     EnforcementBlockReasonNone,
	}
	if got != want {
		t.Fatalf("normalized state = %+v, want %+v", got, want)
	}
}

func TestManagerStartupDoesNotCreateManagedHierarchy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory io\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	if status := manager.EnforcementStatus(); status.Mode != EnforcementModeObservationOnly {
		t.Fatalf("startup mode = %q, want observation_only", status.Mode)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cgroup.controllers" {
		t.Fatalf("managed hierarchy was created during startup: %v", entries)
	}
}
