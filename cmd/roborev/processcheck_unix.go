//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// isPIDAliveForUpdateDefault returns true when pid exists.
// Uses signal 0 so no signal is delivered.
func isPIDAliveForUpdateDefault(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	// PID exists. If it's clearly not a roborev daemon anymore, treat as exited
	// (covers PID reuse after daemon shutdown).
	switch identifyPIDForUpdate(pid) {
	case updatePIDNotRoborev:
		return false
	case updatePIDRoborev:
		return true
	default:
		// Unknown identity -> conservative (assume still alive).
		return true
	}
}

func identifyPIDForUpdate(pid int) updatePIDIdentity {
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err == nil {
		cmdStr := normalizeCommandLineForUpdate(string(cmdline))
		if cmdStr == "" {
			return updatePIDUnknown
		}
		if isRoborevDaemonCommandForUpdate(cmdStr) {
			return updatePIDRoborev
		}
		return updatePIDNotRoborev
	}

	// Callers check PID identity from daemon lifecycle and update paths that
	// do not carry a context; the probe ends on its own.
	// Use -ww to request untruncated command output on BSD/macOS ps.
	cmd := exec.CommandContext(context.Background(), "ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=")
	output, err := cmd.Output()
	if err != nil {
		return updatePIDUnknown
	}
	cmdStr := normalizeCommandLineForUpdate(string(output))
	if cmdStr == "" {
		return updatePIDUnknown
	}
	if isRoborevDaemonCommandForUpdate(cmdStr) {
		return updatePIDRoborev
	}
	return updatePIDNotRoborev
}
