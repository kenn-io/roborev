//go:build !windows

package gitfixture

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

func runFixtureGit(_ context.Context, realGit string, args []string) int {
	// Replacement preserves the PID, so caller cancellation targets Git itself.
	args = append([]string{realGit}, args...)
	if err := syscall.Exec(realGit, args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
