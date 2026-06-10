//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strconv"

	"go.kenn.io/roborev/internal/daemon"
)

// isPIDAliveForUpdateDefault returns true when pid exists.
// Uses the Win32 API for the existence check; spawning tasklist here would
// flash a console window on every update wait-loop poll.
func isPIDAliveForUpdateDefault(pid int) bool {
	if pid <= 0 {
		return false
	}

	if !daemon.ProcessExists(pid) {
		return false
	}

	// PID exists. If identity lookup confirms it's not roborev daemon
	// anymore, treat as exited (covers PID reuse after shutdown).
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
	pidStr := strconv.Itoa(pid)

	// Try WMIC first for older Windows environments.
	if cmdLine := getCommandLineWmicForUpdate(pidStr); cmdLine != "" {
		return classifyCommandLineForUpdate(cmdLine)
	}
	// Fall back to PowerShell CIM query on newer systems.
	if cmdLine := getCommandLinePowerShellForUpdate(pidStr); cmdLine != "" {
		return classifyCommandLineForUpdate(cmdLine)
	}

	return updatePIDUnknown
}

func getCommandLineWmicForUpdate(pidStr string) string {
	// Use an absolute path to avoid PATH/CWD binary hijacking.
	cmd := daemon.HiddenCommand(
		wmicPath(), "process", "where", "ProcessId="+pidStr, "get", "commandline",
	)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseWmicOutputForUpdate(output)
}

func getCommandLinePowerShellForUpdate(pidStr string) string {
	// Force UTF-8 output to avoid UTF-16LE capture issues.
	script := `[Console]::OutputEncoding=[Text.Encoding]::UTF8;` +
		`(Get-CimInstance Win32_Process -Filter "ProcessId=` + pidStr + `").CommandLine`
	cmd := daemon.HiddenCommand(
		powershellPath(), "-NoProfile", "-NonInteractive", "-Command", script,
	)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return normalizeCommandLineBytesForUpdate(output)
}

func classifyCommandLineForUpdate(cmdLine string) updatePIDIdentity {
	cmdLine = normalizeCommandLineForUpdate(cmdLine)
	if cmdLine == "" {
		return updatePIDUnknown
	}
	if isRoborevDaemonCommandForUpdate(cmdLine) {
		return updatePIDRoborev
	}
	return updatePIDNotRoborev
}

func systemRootForUpdate() string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = os.Getenv("WINDIR")
	}
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return systemRoot
}

func wmicPath() string {
	return filepath.Join(systemRootForUpdate(), "System32", "wbem", "wmic.exe")
}

func powershellPath() string {
	return filepath.Join(
		systemRootForUpdate(),
		"System32", "WindowsPowerShell", "v1.0", "powershell.exe",
	)
}
