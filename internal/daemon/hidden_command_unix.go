//go:build !windows

package daemon

import "os/exec"

// HiddenCommand returns exec.Command(name, arg...). Console windows are a
// Windows-only concern.
func HiddenCommand(name string, arg ...string) *exec.Cmd {
	return exec.Command(name, arg...)
}
