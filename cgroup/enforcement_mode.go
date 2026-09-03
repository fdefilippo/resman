package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultSystemdRuntimePath = "/run/systemd/system"

// EnforcementMode describes whether ResMan may migrate processes into its own cgroups.
type EnforcementMode string

const (
	EnforcementModeMigrationEnabled       EnforcementMode = "migration_enabled"
	EnforcementModeObservationOnlySystemd EnforcementMode = "observation_only_systemd"
)

// EnforcementStatus is the bounded result of resolving the host ownership boundary.
type EnforcementStatus struct {
	Mode   EnforcementMode
	Reason string
}

const (
	EnforcementReasonNoSystemdRuntime         = "systemd_runtime_absent"
	EnforcementReasonSystemdOwnsHostWorkloads = "systemd_owns_host_workloads"
	EnforcementReasonAuthorityUnverifiable    = "systemd_authority_unverifiable"
)

// SystemdOwnershipPreservationError reports ingress refused to preserve the
// authoritative systemd ownership of the candidate workload.
type SystemdOwnershipPreservationError struct {
	CandidateCount int
}

func (e *SystemdOwnershipPreservationError) Error() string {
	return fmt.Sprintf(
		"refusing ResMan-owned cgroup ingress for %d candidate processes: systemd host ownership must be preserved",
		e.CandidateCount,
	)
}

// DetectEnforcementStatus recognizes a systemd-booted host through its runtime
// directory or the visible host PID 1. The second check preserves containment
// when a host-PID container does not mount the host's /run hierarchy.
func DetectEnforcementStatus(systemdRuntimePath string) EnforcementStatus {
	return detectEnforcementStatus(systemdRuntimePath, "/proc")
}

func detectEnforcementStatus(systemdRuntimePath, procRoot string) EnforcementStatus {
	if systemdRuntimePath == "" {
		systemdRuntimePath = defaultSystemdRuntimePath
	}
	info, err := os.Stat(systemdRuntimePath)
	switch {
	case err == nil && info.IsDir():
		return EnforcementStatus{
			Mode:   EnforcementModeObservationOnlySystemd,
			Reason: EnforcementReasonSystemdOwnsHostWorkloads,
		}
	case errors.Is(err, os.ErrNotExist):
		pidOneComm, readErr := os.ReadFile(filepath.Join(procRoot, "1", "comm"))
		if readErr != nil {
			return EnforcementStatus{
				Mode:   EnforcementModeObservationOnlySystemd,
				Reason: EnforcementReasonAuthorityUnverifiable,
			}
		}
		if strings.TrimSpace(string(pidOneComm)) == "systemd" {
			return EnforcementStatus{
				Mode:   EnforcementModeObservationOnlySystemd,
				Reason: EnforcementReasonSystemdOwnsHostWorkloads,
			}
		}
		return EnforcementStatus{
			Mode:   EnforcementModeMigrationEnabled,
			Reason: EnforcementReasonNoSystemdRuntime,
		}
	default:
		return EnforcementStatus{
			Mode:   EnforcementModeObservationOnlySystemd,
			Reason: EnforcementReasonAuthorityUnverifiable,
		}
	}
}

func (s EnforcementStatus) migrationAllowed() bool {
	return s.Mode == "" || s.Mode == EnforcementModeMigrationEnabled
}

func (m *Manager) requireMigrationEnforcement(candidateCount int) error {
	if m.EnforcementStatus().migrationAllowed() {
		return nil
	}
	return &SystemdOwnershipPreservationError{CandidateCount: candidateCount}
}
