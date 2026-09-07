package state

import (
	"context"
	"errors"
)

// IsControlCycleCancellation recognizes only cancellation of the owning context.
// Joined independent failures must remain errors even if shutdown also occurred.
func IsControlCycleCancellation(ctx context.Context, err error) bool {
	return ctx != nil && ctx.Err() == context.Canceled && onlyCancellation(err)
}

func onlyCancellation(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyCancellation(wrapped.Unwrap())
	}
	return errors.Is(err, context.Canceled)
}
