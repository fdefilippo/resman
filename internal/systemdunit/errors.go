package systemdunit

import (
	"context"
	"errors"
	"fmt"

	godbus "github.com/godbus/dbus/v5"
)

// ErrorReason is a bounded public classification for adapter failures.
type ErrorReason string

const (
	ReasonBusUnavailable      ErrorReason = "bus_unavailable"
	ReasonUnitMissing         ErrorReason = "unit_missing"
	ReasonAuthorizationDenied ErrorReason = "authorization_denied"
	ReasonUnitRecreated       ErrorReason = "unit_recreated"
	ReasonMalformedReply      ErrorReason = "malformed_reply"
	ReasonPropertyNotAllowed  ErrorReason = "property_not_allowed"
	ReasonInvalidValue        ErrorReason = "invalid_value"
	ReasonReadbackMismatch    ErrorReason = "readback_mismatch"
	ReasonKernelVerification  ErrorReason = "kernel_verification_failed"
	ReasonExternalConflict    ErrorReason = "external_property_conflict"
	ReasonTimeout             ErrorReason = "timeout"
	ReasonClosed              ErrorReason = "adapter_closed"
)

// AdapterError carries a bounded reason while retaining the underlying error for diagnosis.
type AdapterError struct {
	Reason    ErrorReason
	Operation string
	Unit      string
	Property  PropertyName
	Err       error
}

func (e *AdapterError) Error() string {
	message := "systemd unit adapter"
	if e.Operation != "" {
		message += " " + e.Operation
	}
	if e.Unit != "" {
		message += " for " + e.Unit
	}
	if e.Property != "" {
		message += " property " + string(e.Property)
	}
	if e.Err == nil {
		return fmt.Sprintf("%s failed (%s)", message, e.Reason)
	}
	return fmt.Sprintf("%s failed (%s): %v", message, e.Reason, e.Err)
}

// Unwrap exposes the underlying transport, validation or verification error.
func (e *AdapterError) Unwrap() error { return e.Err }

// RestoreConflictError reports properties that changed outside ResMan after application.
type RestoreConflictError struct {
	Unit      string
	Conflicts []PropertyConflict
}

func (e *RestoreConflictError) Error() string {
	return fmt.Sprintf("systemd unit adapter restore for %s preserved %d externally changed properties", e.Unit, len(e.Conflicts))
}

func classifyTransportError(operation, unit string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &AdapterError{Reason: ReasonTimeout, Operation: operation, Unit: unit, Err: err}
	}
	var dbusErr *godbus.Error
	if errors.As(err, &dbusErr) {
		switch dbusErr.Name {
		case "org.freedesktop.systemd1.NoSuchUnit", "org.freedesktop.DBus.Error.UnknownObject":
			return &AdapterError{Reason: ReasonUnitMissing, Operation: operation, Unit: unit, Err: err}
		case "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.AuthFailed", "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired", "org.freedesktop.PolicyKit1.Error.Failed":
			return &AdapterError{Reason: ReasonAuthorizationDenied, Operation: operation, Unit: unit, Err: err}
		}
	}
	return &AdapterError{Reason: ReasonBusUnavailable, Operation: operation, Unit: unit, Err: err}
}
