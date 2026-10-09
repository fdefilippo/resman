package cgroup

import "testing"

func TestMandatoryCapabilityAbsenceIsItsOwnPublicReason(t *testing.T) {
	// Before this reason existed, any unmapped host reason was published as
	// systemd_authority_unverifiable, which would have described an unavailable
	// kernel interface as an unverifiable authority.
	got := BoundedEnforcementBlockReason(EnforcementReasonMandatoryCapabilityUnavailable)
	if got != EnforcementBlockReasonMandatoryCapability {
		t.Fatalf("BoundedEnforcementBlockReason(%q) = %q, want %q",
			EnforcementReasonMandatoryCapabilityUnavailable, got, EnforcementBlockReasonMandatoryCapability)
	}
}

func TestDeclaredObservationIsNeverPublishedAsAnInability(t *testing.T) {
	got := BoundedEnforcementBlockReason(EnforcementReasonOperatorObservationOnly)
	if got != EnforcementBlockReasonOperatorObservation {
		t.Fatalf("BoundedEnforcementBlockReason(%q) = %q, want %q",
			EnforcementReasonOperatorObservationOnly, got, EnforcementBlockReasonOperatorObservation)
	}
	if EnforcementBlockReasonOperatorObservation == EnforcementBlockReasonMandatoryCapability {
		t.Fatal("an operator declaration and a host inability share one public reason")
	}
}

func TestEveryPublishedBlockReasonSurvivesNormalization(t *testing.T) {
	for _, reason := range []EnforcementBlockReason{
		EnforcementBlockReasonNone,
		EnforcementBlockReasonSystemdRuntimeAbsent,
		EnforcementBlockReasonSystemdOwnsWorkloads,
		EnforcementBlockReasonAuthorityUnverifiable,
		EnforcementBlockReasonMandatoryCapability,
		EnforcementBlockReasonOperatorObservation,
	} {
		state := NormalizedEnforcementCycleState(EnforcementCycleState{
			Mode:            EnforcementModeObservationOnly,
			RequestedIntent: EnforcementPolicyIntentNone,
			AppliedAction:   AppliedEnforcementActionNone,
			BlockReason:     reason,
		}, EnforcementStatus{Mode: EnforcementModeObservationOnly})
		if state.BlockReason != reason {
			t.Fatalf("normalization replaced published reason %q with %q", reason, state.BlockReason)
		}
	}
}

func TestObservationAfterACapabilityRefusalStaysObservationOnly(t *testing.T) {
	status := EnforcementStatus{
		Mode:   EnforcementModeObservationOnly,
		Reason: EnforcementReasonMandatoryCapabilityUnavailable,
	}
	state := InitialEnforcementCycleState(status)
	if state.Mode != EnforcementModeObservationOnly {
		t.Fatalf("initial mode = %q, want observation_only", state.Mode)
	}
	if state.AppliedAction != AppliedEnforcementActionNone {
		t.Fatalf("initial applied action = %q, want none", state.AppliedAction)
	}
}
