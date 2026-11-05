package main

import (
	"errors"
	"fmt"

	"go.kenn.io/roborev/internal/daemon"
)

func managedUnresponsiveError(owner *daemonService, cause error) error {
	const message = "daemon is unresponsive"
	err := errors.New(message)
	if cause != nil {
		err = fmt.Errorf("%s: %w", message, cause)
	}
	return &managedRecoveryError{owner.unit, fmt.Errorf("%w; run %s %s", err, serviceRestartCommand, owner.unit)}
}

func liveDaemon(info *daemon.RuntimeInfo) bool {
	return !info.Stale()
}
