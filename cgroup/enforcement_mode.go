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

// EnforcementPolicyIntent is the bounded policy transition requested by the
// latest control cycle, independently of whether it could be applied.
type EnforcementPolicyIntent string

// AppliedEnforcementAction is the bounded action completed by the latest
// control cycle. None means that no enforcement operation was acknowledged.
type AppliedEnforcementAction string

// EnforcementBlockReason is the bounded reason why policy intent could not be
// applied. It is distinct from the immutable host enforcement reason.
type EnforcementBlockReason string

// EnforcementCycleState separates policy intent from acknowledged action.
type EnforcementCycleState struct {
	Mode            EnforcementMode
	RequestedIntent EnforcementPolicyIntent
	AppliedAction   AppliedEnforcementAction
	BlockReason     EnforcementBlockReason
}

const (
	EnforcementModeObservationOnly EnforcementMode = "observation_only"
	EnforcementModeSystemdNative   EnforcementMode = "systemd_native"
)

const (
	EnforcementPolicyIntentNone       EnforcementPolicyIntent = "none"
	EnforcementPolicyIntentActivate   EnforcementPolicyIntent = "activate"
	EnforcementPolicyIntentDeactivate EnforcementPolicyIntent = "deactivate"
	EnforcementPolicyIntentMaintain   EnforcementPolicyIntent = "maintain"
)

const (
	AppliedEnforcementActionNone       AppliedEnforcementAction = "none"
	AppliedEnforcementActionActivate   AppliedEnforcementAction = "activate"
	AppliedEnforcementActionDeactivate AppliedEnforcementAction = "deactivate"
	AppliedEnforcementActionMaintain   AppliedEnforcementAction = "maintain"
)

const (
	EnforcementBlockReasonNone                  EnforcementBlockReason = "none"
	EnforcementBlockReasonSystemdRuntimeAbsent  EnforcementBlockReason = "systemd_runtime_absent"
	EnforcementBlockReasonSystemdOwnsWorkloads  EnforcementBlockReason = "systemd_owns_host_workloads"
	EnforcementBlockReasonAuthorityUnverifiable EnforcementBlockReason = "systemd_authority_unverifiable"
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

// InitialEnforcementCycleState returns the explicit state before the first
// control-cycle decision has been executed.
func InitialEnforcementCycleState(status EnforcementStatus) EnforcementCycleState {
	return EnforcementCycleState{
		Mode:            normalizedEnforcementMode(status.Mode),
		RequestedIntent: EnforcementPolicyIntentNone,
		AppliedAction:   AppliedEnforcementActionNone,
		BlockReason:     EnforcementBlockReasonNone,
	}
}

// BoundedEnforcementBlockReason maps the immutable host reason to the label
// vocabulary permitted on public observation surfaces.
func BoundedEnforcementBlockReason(reason string) EnforcementBlockReason {
	switch reason {
	case EnforcementReasonSystemdRuntimeAbsent:
		return EnforcementBlockReasonSystemdRuntimeAbsent
	case EnforcementReasonSystemdOwnsHostWorkloads:
		return EnforcementBlockReasonSystemdOwnsWorkloads
	case EnforcementReasonAuthorityUnverifiable:
		return EnforcementBlockReasonAuthorityUnverifiable
	default:
		return EnforcementBlockReasonAuthorityUnverifiable
	}
}

// NormalizedEnforcementCycleState fails closed to a bounded public state when
// a caller has not yet supplied every field.
func NormalizedEnforcementCycleState(state EnforcementCycleState, fallback EnforcementStatus) EnforcementCycleState {
	if !validEnforcementMode(state.Mode) {
		state.Mode = normalizedEnforcementMode(fallback.Mode)
	}
	if !validPolicyIntent(state.RequestedIntent) {
		state.RequestedIntent = EnforcementPolicyIntentNone
	}
	if !validAppliedAction(state.AppliedAction) {
		state.AppliedAction = AppliedEnforcementActionNone
	}
	if !validBlockReason(state.BlockReason) {
		state.BlockReason = EnforcementBlockReasonNone
	}
	return state
}

func validEnforcementMode(mode EnforcementMode) bool {
	return mode == EnforcementModeObservationOnly || mode == EnforcementModeSystemdNative
}

func normalizedEnforcementMode(mode EnforcementMode) EnforcementMode {
	switch mode {
	case EnforcementModeObservationOnly, EnforcementModeSystemdNative:
		return mode
	default:
		return EnforcementModeObservationOnly
	}
}

func validPolicyIntent(intent EnforcementPolicyIntent) bool {
	switch intent {
	case EnforcementPolicyIntentNone, EnforcementPolicyIntentActivate, EnforcementPolicyIntentDeactivate, EnforcementPolicyIntentMaintain:
		return true
	default:
		return false
	}
}

func validAppliedAction(action AppliedEnforcementAction) bool {
	switch action {
	case AppliedEnforcementActionNone, AppliedEnforcementActionActivate, AppliedEnforcementActionDeactivate, AppliedEnforcementActionMaintain:
		return true
	default:
		return false
	}
}

func validBlockReason(reason EnforcementBlockReason) bool {
	switch reason {
	case EnforcementBlockReasonNone, EnforcementBlockReasonSystemdRuntimeAbsent, EnforcementBlockReasonSystemdOwnsWorkloads, EnforcementBlockReasonAuthorityUnverifiable:
		return true
	default:
		return false
	}
}

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
