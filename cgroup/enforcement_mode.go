package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const defaultSystemdRuntimePath = "/run/systemd/system"

// EnforcementMode identifies the authoritative boundary used for enforcement.
type EnforcementMode string

const (
	EnforcementModeObservationOnly EnforcementMode = "observation_only"
	EnforcementModeSystemdNative   EnforcementMode = "systemd_native"
)

// EnforcementStatus is the bounded result of resolving the host ownership boundary.
type EnforcementStatus struct {
	Mode   EnforcementMode
	Reason string
}

const (
	EnforcementReasonSystemdRuntimeAbsent     = "systemd_runtime_absent"
	EnforcementReasonSystemdOwnsHostWorkloads = "systemd_owns_host_workloads"
	EnforcementReasonAuthorityUnverifiable    = "systemd_authority_unverifiable"
	EnforcementReasonSystemdNativeAdapter     = "systemd_native_adapter"
)

// DetectEnforcementStatus recognizes whether authoritative systemd enforcement
// may be initialized. Missing or unverifiable authority always fails closed.
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
			Mode:   EnforcementModeObservationOnly,
			Reason: EnforcementReasonSystemdOwnsHostWorkloads,
		}
	case errors.Is(err, os.ErrNotExist):
		pidOneComm, readErr := os.ReadFile(filepath.Join(procRoot, "1", "comm"))
		if readErr != nil {
			return EnforcementStatus{
				Mode:   EnforcementModeObservationOnly,
				Reason: EnforcementReasonAuthorityUnverifiable,
			}
		}
		if strings.TrimSpace(string(pidOneComm)) == "systemd" {
			return EnforcementStatus{
				Mode:   EnforcementModeObservationOnly,
				Reason: EnforcementReasonSystemdOwnsHostWorkloads,
			}
		}
		return EnforcementStatus{
			Mode:   EnforcementModeObservationOnly,
			Reason: EnforcementReasonSystemdRuntimeAbsent,
		}
	default:
		return EnforcementStatus{
			Mode:   EnforcementModeObservationOnly,
			Reason: EnforcementReasonAuthorityUnverifiable,
		}
	}
}
