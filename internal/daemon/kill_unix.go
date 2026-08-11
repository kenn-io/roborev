//go:build !windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// processIdentity represents the result of identifying a process.
type processIdentity int

const (
	processUnknown    processIdentity = iota // Can't determine identity
	processIsRoborev                         // Confirmed roborev daemon
	processNotRoborev                        // Confirmed NOT roborev daemon
)

// identifyProcess checks if a process is a roborev daemon.
// Returns processIsRoborev, processNotRoborev, or processUnknown.
// This prevents killing unrelated processes if a PID was reused.
var identifyProcess = identifyProcessImpl

func identifyProcessImpl(pid int) processIdentity {
	// Try reading /proc/<pid>/cmdline (Linux)
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err == nil {
		// cmdline uses null bytes as separators
		cmdStr := strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
		if cmdStr == "" {
			// Empty cmdline (e.g., kernel thread or permission issue) - unknown
			return processUnknown
		}
		if isRoborevDaemonCommand(cmdStr) {
			return processIsRoborev
		}
		// We got cmdline but it's not roborev daemon
		return processNotRoborev
	}

	// Fall back to ps (macOS/BSD)
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=")
	output, err := cmd.Output()
	if err != nil {
		// Can't determine - could be permissions, missing ps, etc.
		return processUnknown
	}
	cmdStr := strings.TrimSpace(string(output))
	if cmdStr == "" {
		// Empty output - can't determine identity
		return processUnknown
	}
	if isRoborevDaemonCommand(cmdStr) {
		return processIsRoborev
	}
	// We got ps output but it's not roborev daemon
	return processNotRoborev
}

// isRoborevDaemonCommand checks if a command line is a roborev daemon process.
// Requires "daemon" followed by "run" as the first subcommand to distinguish from
// CLI commands like "roborev daemon status" or "roborev daemon status --output run".
func isRoborevDaemonCommand(cmdStr string) bool {
	// Must contain roborev somewhere (binary name or path)
	if !strings.Contains(cmdStr, "roborev") {
		return false
	}
	// Tokenize and look for "daemon" followed by "run" as first subcommand
	fields := strings.Fields(cmdStr)
	foundDaemon := false
	for _, field := range fields {
		if !foundDaemon {
			// Look for "daemon" token (or path ending in /daemon)
			if field == "daemon" || strings.HasSuffix(field, "/daemon") {
				foundDaemon = true
			}
			continue
		}
		// Skip flags
		if strings.HasPrefix(field, "-") {
			continue
		}
		// Skip tokens that look like flag values (paths, numbers, key=value)
		if looksLikeFlagValue(field) {
			continue
		}
		// First subcommand-like token after "daemon" - must be "run"
		return field == "run"
	}
	return false
}

// looksLikeFlagValue returns true if the token looks like a flag value rather
// than a subcommand. This helps distinguish "daemon --config /etc/foo run" from
// "daemon status --output run".
func looksLikeFlagValue(token string) bool {
	// Paths contain separators
	if strings.ContainsAny(token, "/\\") {
		return true
	}
	// Windows drive letters or URLs contain colons
	if strings.Contains(token, ":") {
		return true
	}
	// Key=value pairs
	if strings.Contains(token, "=") {
		return true
	}
	// Numbers (port numbers, timeouts, etc.)
	if len(token) > 0 && token[0] >= '0' && token[0] <= '9' {
		return true
	}
	// File extensions suggest paths
	if strings.Contains(token, ".") {
		return true
	}
	return false
}

// isProcessAlive checks whether a process with the given PID exists.
// It uses signal 0 which doesn't actually send a signal but checks for existence.
func isProcessAlive(pid int) bool {
	process, _ := os.FindProcess(pid)
	err := process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ProcessExists reports whether pid appears to name a live process.
func ProcessExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	return isProcessAlive(pid)
}
