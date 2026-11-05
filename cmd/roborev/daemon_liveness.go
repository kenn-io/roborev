package main

import (
	"errors"
	"fmt"

	"go.kenn.io/roborev/internal/daemon"
)

// unresponsiveDaemonError retains the process record when HTTP is unavailable.
type unresponsiveDaemonError struct {
	info  *daemon.RuntimeInfo
	cause error
}

func (e *unresponsiveDaemonError) Error() string { return e.cause.Error() }
func (e *unresponsiveDaemonError) Unwrap() error { return e.cause }

func managedUnresponsiveError(owner *daemonService, cause error) error {
	const message = "daemon is unresponsive"
	err := errors.New(message)
	if cause != nil {
		err = fmt.Errorf("%s: %w", message, cause)
	}
	return &managedRecoveryError{owner.unit, fmt.Errorf("%w; run %s %s", err, serviceRestartCommand, owner.unit)}
}

func liveDaemon(info *daemon.RuntimeInfo) bool {
	return info != nil && info.PID > 0 && !info.Stale()
}
