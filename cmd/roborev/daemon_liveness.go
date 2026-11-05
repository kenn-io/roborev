package main

import (
	"errors"
	"fmt"
)

func managedUnresponsiveError(owner *daemonService, cause error) error {
	const message = "daemon is unresponsive"
	err := errors.New(message)
	if cause != nil {
		err = fmt.Errorf("%s: %w", message, cause)
	}
	return &managedRecoveryError{owner.unit, fmt.Errorf("%w; run %s %s", err, serviceRestartCommand, owner.unit)}
}
