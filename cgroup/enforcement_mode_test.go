package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/logging"
)

func TestDetectEnforcementStatusFailsClosedOnSystemdOrUnverifiableAuthority(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(t *testing.T, runtimePath, procRoot string)
		wantMode   EnforcementMode
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
			wantMode:   EnforcementModeObservationOnlySystemd,
			wantReason: EnforcementReasonSystemdOwnsHostWorkloads,
		},
		{
			name: "systemd runtime absent",
			prepare: func(t *testing.T, _ string, procRoot string) {
				t.Helper()
				writePIDOneComm(t, procRoot, "init")
			},
			wantMode:   EnforcementModeMigrationEnabled,
			wantReason: EnforcementReasonNoSystemdRuntime,
		},
		{
			name: "host PID namespace exposes systemd without the runtime mount",
			prepare: func(t *testing.T, _ string, procRoot string) {
				t.Helper()
				writePIDOneComm(t, procRoot, "systemd")
			},
			wantMode:   EnforcementModeObservationOnlySystemd,
			wantReason: EnforcementReasonSystemdOwnsHostWorkloads,
		},
		{
			name:       "systemd runtime and PID 1 identity unavailable",
			prepare:    func(*testing.T, string, string) {},
			wantMode:   EnforcementModeObservationOnlySystemd,
			wantReason: EnforcementReasonAuthorityUnverifiable,
		},
		{
			name: "authority path is not a directory",
			prepare: func(t *testing.T, path, _ string) {
				t.Helper()
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			},
			wantMode:   EnforcementModeObservationOnlySystemd,
			wantReason: EnforcementReasonAuthorityUnverifiable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "systemd")
			procRoot := filepath.Join(root, "proc")
			tt.prepare(t, path, procRoot)
			got := detectEnforcementStatus(path, procRoot)
			if got.Mode != tt.wantMode || got.Reason != tt.wantReason {
				t.Fatalf("DetectEnforcementStatus() = %+v, want mode=%q reason=%q", got, tt.wantMode, tt.wantReason)
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
	manager := &Manager{}
	status := manager.EnforcementStatus()
	if status.Mode != EnforcementModeObservationOnlySystemd || status.Reason != EnforcementReasonAuthorityUnverifiable {
		t.Fatalf("EnforcementStatus() = %+v, want fail-closed observation-only status", status)
	}
	var ownershipErr *SystemdOwnershipPreservationError
	if err := manager.requireMigrationEnforcement(1); !errors.As(err, &ownershipErr) {
		t.Fatalf("zero-value manager migration error = %v, want typed ownership-preservation refusal", err)
	}
}

func migrationEnabledTestManager(manager *Manager) *Manager {
	manager.enforcementStatus = EnforcementStatus{
		Mode:   EnforcementModeMigrationEnabled,
		Reason: EnforcementReasonNoSystemdRuntime,
	}
	return manager
}

func TestObservationOnlyBarrierRefusesBeforeManagedHierarchyMutation(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	manager := &Manager{
		cfg:    cfg,
		logger: logging.GetLogger(),
		enforcementStatus: EnforcementStatus{
			Mode:   EnforcementModeObservationOnlySystemd,
			Reason: EnforcementReasonSystemdOwnsHostWorkloads,
		},
	}
	weight, err := cpupoints.NewKernelCPUWeight(100)
	if err != nil {
		t.Fatal(err)
	}
	online, _ := cpupoints.NewOnlineCPUCount(1)
	reserve, _ := cpupoints.NewReservePoints(100)
	quota, err := cpupoints.PlanParentQuota(online, reserve.ParentPool())
	if err != nil {
		t.Fatal(err)
	}

	operations := []struct {
		name string
		run  func() error
	}{
		{name: "standalone user", run: func() error { return manager.CreateUserCgroup(1000) }},
		{name: "shared hierarchy", run: func() error { _, err := manager.CreateSharedCgroup(); return err }},
		{name: "shared user", run: func() error {
			_, err := manager.CreateUserSubCgroup(1000, filepath.Join(root, "resman", "limited"))
			return err
		}},
		{name: "placement", run: func() error { _, _, err := manager.EnsureUserCgroupPlacement(1000, "", normalCPUQuota); return err }},
		{name: "CPU Points hierarchy", run: func() error { _, err := manager.EnsureCPUPointsHierarchy(quota, weight); return err }},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			var ownershipErr *SystemdOwnershipPreservationError
			if !errors.As(err, &ownershipErr) {
				t.Fatalf("error = %v, want typed ownership-preservation refusal", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, "resman")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("managed hierarchy was mutated before refusal: %v", statErr)
			}
		})
	}
}

func TestObservationOnlyIngressRefusesBeforeOriginCaptureOrPIDWrite(t *testing.T) {
	manager, root := newOriginTestManager(t)
	manager.enforcementStatus = EnforcementStatus{
		Mode:   EnforcementModeObservationOnlySystemd,
		Reason: EnforcementReasonSystemdOwnsHostWorkloads,
	}
	origin := "/user.slice/user-1000.slice/session-7.scope"
	writeFakeProcess(t, manager, 101, 1, 101, 5000, 1000, origin)
	createFakeCgroup(t, root, origin)
	destination := createFakeCgroup(t, root, "/resman/limited/user_1000")
	persistCalled := false
	manager.persistOrigins = func() error {
		persistCalled = true
		return nil
	}
	writeCalled := false
	manager.writePID = func(string, int) error {
		writeCalled = true
		return nil
	}

	_, result, _, err := manager.moveProcessBatch([]int{101}, 1000, destination)
	var ownershipErr *SystemdOwnershipPreservationError
	if !errors.As(err, &ownershipErr) || ownershipErr.CandidateCount != 1 {
		t.Fatalf("error = %v, want typed refusal for one candidate", err)
	}
	if result.SystemdOwnershipRefused != 1 || result.Applied() {
		t.Fatalf("move result = %+v, want one refused process and no applied ingress", result)
	}
	if persistCalled || writeCalled || len(manager.snapshotProcessOrigins()) != 0 {
		t.Fatalf("refusal mutated ingress state: persist=%t write=%t origins=%d", persistCalled, writeCalled, len(manager.snapshotProcessOrigins()))
	}
}
