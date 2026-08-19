package agent

import "errors"

type unavailableError struct {
	cause error
}

func (e *unavailableError) Error() string {
	return e.cause.Error()
}

func (e *unavailableError) Unwrap() error {
	return e.cause
}

// MarkUnavailable identifies an error that occurred before an agent produced
// valid protocol output. It preserves the original error for errors.Is and
// errors.As checks.
func MarkUnavailable(err error) error {
	if err == nil || IsUnavailable(err) {
		return err
	}
	return &unavailableError{cause: err}
}

// IsUnavailable reports whether err was marked as a pre-protocol agent
// availability failure.
func IsUnavailable(err error) bool {
	var target *unavailableError
	return errors.As(err, &target)
}
