//go:build windows

package daemon

import (
	"context"
	"os/exec"

	"go.kenn.io/roborev/internal/procutil"
)

// HiddenCommand returns an exec.Cmd that will not flash a console window.
func HiddenCommand(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...)
	procutil.HideConsole(cmd)
	return cmd
}
