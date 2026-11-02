//go:build !windows

package daemon

import (
	"context"
	"os/exec"
)

// HiddenCommand returns exec.CommandContext(ctx, name, arg...). Console windows are a
// Windows-only concern.
func HiddenCommand(ctx context.Context, name string, arg ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, arg...)
}
