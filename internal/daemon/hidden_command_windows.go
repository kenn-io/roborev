//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

// createNoWindow (CREATE_NO_WINDOW) keeps console-subsystem tools from
// opening a visible console window when the caller has no console of its own
// (detached daemon, mintty/Git Bash CLI).
const createNoWindow = 0x08000000

// HiddenCommand returns an exec.Cmd that will not flash a console window.
// Use it for every Windows process-tool spawn (taskkill, wmic, powershell).
func HiddenCommand(name string, arg ...string) *exec.Cmd {
	cmd := exec.Command(name, arg...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd
}
