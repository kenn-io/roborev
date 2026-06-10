//go:build !windows

package main

import (
	"os/exec"
	"time"
)

// killAllDaemonsPlatform kills roborev daemon processes by command line.
// Use -f to match against the full command line.
func killAllDaemonsPlatform() {
	_ = exec.Command("pkill", "-f", "roborev daemon run").Run()
	time.Sleep(100 * time.Millisecond)
	// Force kill any remaining
	_ = exec.Command("pkill", "-9", "-f", "roborev daemon run").Run()
}
