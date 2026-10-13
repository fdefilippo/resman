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
	EnforcementBlockReasonNone                 EnforcementBlockReason = "none"
	EnforcementBlockReasonSystemdRuntimeAbsent EnforcementBlockReason = "systemd_runtime_absent"
	EnforcementBlockReasonSystemdOwnsWorkloads EnforcementBlockReason = "systemd_owns_host_workloads"
	// EnforcementBlockReasonSystemdRuntimeUnreachable means systemd owns the
	// observed workloads but cannot be reached from this mount namespace.
	EnforcementBlockReasonSystemdRuntimeUnreachable EnforcementBlockReason = "systemd_runtime_unreachable"
	EnforcementBlockReasonAuthorityUnverifiable     EnforcementBlockReason = "systemd_authority_unverifiable"
	// EnforcementBlockReasonMandatoryCapability means the host cannot provide a
	// mandatory enforcement capability, so no limit can be applied there.
	EnforcementBlockReasonMandatoryCapability EnforcementBlockReason = "mandatory_capability_unavailable"
	// EnforcementBlockReasonOperatorObservation means the operator declared
	// that this host observes and never enforces. It is deliberately distinct
	// from an inability: a declared choice must never read as a defect.
	EnforcementBlockReasonOperatorObservation EnforcementBlockReason = "operator_requested_observation"
)

// EnforcementStatus is the bounded result of resolving the host ownership boundary.
type EnforcementStatus struct {
	Mode   EnforcementMode
	Reason string
}

const (
	EnforcementReasonSystemdRuntimeAbsent     = "systemd_runtime_absent"
	EnforcementReasonSystemdOwnsHostWorkloads = "systemd_owns_host_workloads"
	// EnforcementReasonSystemdRuntimeUnreachable is published when systemd is
	// PID 1 of the observed PID namespace but its runtime directory is not
	// visible from this mount namespace, as in a container that shares the host
	// PID namespace. The adapter cannot reach systemd from here, so the host is
	// observed instead of retrying a connection that can never succeed.
	EnforcementReasonSystemdRuntimeUnreachable = "systemd_runtime_unreachable"
	EnforcementReasonAuthorityUnverifiable     = "systemd_authority_unverifiable"
	EnforcementReasonSystemdNativeAdapter      = "systemd_native_adapter"
	// EnforcementReasonMandatoryCapabilityUnavailable is published when the
	// host refuses a mandatory enforcement capability and the operator asked
	// for observation rather than a refusal to start.
	EnforcementReasonMandatoryCapabilityUnavailable = "mandatory_capability_unavailable"
	// EnforcementReasonOperatorObservationOnly is published when the operator
	// declared observation in configuration, whatever the host could provide.
	EnforcementReasonOperatorObservationOnly = "operator_requested_observation"
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
	case EnforcementReasonSystemdRuntimeUnreachable:
		return EnforcementBlockReasonSystemdRuntimeUnreachable
	case EnforcementReasonAuthorityUnverifiable:
		return EnforcementBlockReasonAuthorityUnverifiable
	case EnforcementReasonMandatoryCapabilityUnavailable:
		return EnforcementBlockReasonMandatoryCapability
	case EnforcementReasonOperatorObservationOnly:
		return EnforcementBlockReasonOperatorObservation
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
	case EnforcementBlockReasonNone, EnforcementBlockReasonSystemdRuntimeAbsent,
		EnforcementBlockReasonSystemdOwnsWorkloads, EnforcementBlockReasonSystemdRuntimeUnreachable,
		EnforcementBlockReasonAuthorityUnverifiable,
		EnforcementBlockReasonMandatoryCapability, EnforcementBlockReasonOperatorObservation:
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
			// systemd creates its runtime directory before it starts any unit,
			// so on the host itself this branch is unreachable: PID 1 is
			// systemd, but this mount namespace cannot see it.
			return EnforcementStatus{
				Mode:   EnforcementModeObservationOnly,
				Reason: EnforcementReasonSystemdRuntimeUnreachable,
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
