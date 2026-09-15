package ioweights

// ActivationState is the bounded weighted-I/O lifecycle vocabulary shared by
// state, persistence, Prometheus, and MCP projections.
type ActivationState string

// MechanismState is the bounded aggregate mechanism vocabulary.
type MechanismState string

const (
	MechanismNone   MechanismState = "none"
	MechanismBFQ    MechanismState = "bfq"
	MechanismIOCost MechanismState = "io_cost"
	MechanismMixed  MechanismState = "mixed"
)

// ValidMechanismState reports whether value is a public mechanism state.
func ValidMechanismState(value string) bool {
	switch MechanismState(value) {
	case MechanismNone, MechanismBFQ, MechanismIOCost, MechanismMixed:
		return true
	default:
		return false
	}
}

// VerificationState distinguishes absence of an attempt from its outcome.
type VerificationState string

const (
	VerificationNotAttempted VerificationState = "not_attempted"
	VerificationConfirmed    VerificationState = "confirmed"
	VerificationFailed       VerificationState = "failed"
	VerificationReleased     VerificationState = "released"
)

// ValidVerificationState reports whether value is a public verification state.
func ValidVerificationState(value string) bool {
	switch VerificationState(value) {
	case VerificationNotAttempted, VerificationConfirmed, VerificationFailed, VerificationReleased:
		return true
	default:
		return false
	}
}

// AuthorityCoverage is the bounded plan-level authority vocabulary.
type AuthorityCoverage string

const (
	AuthorityComplete    AuthorityCoverage = "complete"
	AuthorityPartial     AuthorityCoverage = "partial"
	AuthorityUnavailable AuthorityCoverage = "unavailable"
)

// ValidAuthorityCoverage reports whether value is a public authority state.
func ValidAuthorityCoverage(value string) bool {
	switch AuthorityCoverage(value) {
	case AuthorityComplete, AuthorityPartial, AuthorityUnavailable:
		return true
	default:
		return false
	}
}

const (
	ActivationDisabled             ActivationState = "disabled"
	ActivationRequestedPending     ActivationState = "requested_pending"
	ActivationReleasePending       ActivationState = "release_pending"
	ActivationRefusedObservation   ActivationState = "refused_observation"
	ActivationRefusedIntervention  ActivationState = "refused_intervention"
	ActivationProbeCandidate       ActivationState = "probe_candidate"
	ActivationFunctionallyAccepted ActivationState = "functionally_accepted"
)

// ValidActivationState reports whether value belongs to the public lifecycle.
func ValidActivationState(value string) bool {
	switch ActivationState(value) {
	case ActivationDisabled, ActivationRequestedPending, ActivationReleasePending,
		ActivationRefusedObservation, ActivationRefusedIntervention,
		ActivationProbeCandidate, ActivationFunctionallyAccepted:
		return true
	default:
		return false
	}
}

const (
	// DeliveryNotMeasured is truthful until controlled-contention qualification
	// provides an observed-delivery producer.
	DeliveryNotMeasured = "not_measured"
	// ReasonUnknown is the bounded fail-closed projection for a producer defect.
	ReasonUnknown = "unknown"
)

// ValidDeliveryState reports whether value belongs to the current public
// delivery vocabulary.
func ValidDeliveryState(value string) bool { return value == DeliveryNotMeasured }

// ValidReason reports whether value is a bounded lifecycle or adapter reason.
// The empty reason is valid only as the absence of a failure; callers enforce
// its relationship with the lifecycle state.
func ValidReason(value string) bool {
	switch value {
	case "", ReasonUnknown,
		"adapter_unavailable", "ambiguous_topology", "authority_unavailable", "cancelled_generation", "configuration_changed",
		"apply_unavailable", "capability_changed", "device_identity_changed", "device_missing", "duplicate_device",
		"empty_enabled_plan", "evidence_unavailable", "invalid_assignment", "invalid_classifier_input", "invalid_policy_plan", "invalid_selector",
		"io_cost_changed", "mechanism_ambiguous", "mechanism_unsupported", "no_active_mechanism",
		"probe_failed", "readback_unavailable", "scheduler_changed", "topology_changed",
		"topology_unavailable", "unsafe_restore", "adapter_closed", "authorization_denied",
		"bus_unavailable", "capability_probe_failed", "external_property_conflict", "invalid_value",
		"kernel_verification_failed", "lease_store_failed", "malformed_reply", "property_not_allowed",
		"readback_mismatch", "required_capability_unavailable", "timeout", "unit_file_verification_failed",
		"unit_missing", "unit_recreated":
		return true
	default:
		return false
	}
}
