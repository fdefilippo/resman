package app

import "errors"

// permanentStartupError marks a startup rejection that requires operator action.
type permanentStartupError struct {
	err error
}

func (e *permanentStartupError) Error() string {
	return e.err.Error()
}

func (e *permanentStartupError) Unwrap() error {
	return e.err
}

// NewPermanentStartupError marks err as non-restartable by the service manager.
func NewPermanentStartupError(err error) error {
	if err == nil {
		return nil
	}
	return &permanentStartupError{err: err}
}

// IsPermanentStartupError reports whether startup failed for a reason that
// requires an operator to change configuration or host capabilities.
func IsPermanentStartupError(err error) bool {
	var target *permanentStartupError
	return errors.As(err, &target)
}
