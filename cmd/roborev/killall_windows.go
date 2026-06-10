//go:build windows

package main

import (
	"os/exec"

	"go.kenn.io/roborev/internal/daemon"
)

// killAllDaemonsCommands returns the kill strategies in order: wmic first for
// older Windows, then a PowerShell CIM query because wmic has been removed
// from current Windows 11 builds. Both match the daemon by command line and
// run hidden so they cannot flash console windows. Split out for testing.
func killAllDaemonsCommands() []*exec.Cmd {
	const psScript = `Get-CimInstance Win32_Process |` +
		` Where-Object { $_.CommandLine -like '*roborev*daemon*run*' } |` +
		` ForEach-Object { Stop-Process -Id $_.ProcessId -Force }`
	return []*exec.Cmd{
		daemon.HiddenCommand(wmicPath(), "process", "where",
			"commandline like '%roborev%daemon%run%'",
			"call", "terminate"),
		daemon.HiddenCommand(powershellPath(),
			"-NoProfile", "-NonInteractive", "-Command", psScript),
	}
}

// killAllDaemonsPlatform kills roborev daemon processes by command line,
// stopping at the first strategy that succeeds.
func killAllDaemonsPlatform() {
	for _, cmd := range killAllDaemonsCommands() {
		if cmd.Run() == nil {
			return
		}
	}
}
