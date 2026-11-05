package main

import "go.kenn.io/roborev/internal/daemon"

// unresponsiveDaemonError retains the process record when HTTP is unavailable.
type unresponsiveDaemonError struct {
	info  *daemon.RuntimeInfo
	cause error
}

func (e *unresponsiveDaemonError) Error() string { return e.cause.Error() }
func (e *unresponsiveDaemonError) Unwrap() error { return e.cause }

func liveDaemon(info *daemon.RuntimeInfo) bool {
	return info != nil && info.PID > 0 && !info.Stale()
}
