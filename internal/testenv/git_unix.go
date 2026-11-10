//go:build !windows

package testenv

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

func runFixtureGit(_ context.Context, realGit string) int {
	// Replacement preserves the PID, so caller cancellation targets Git itself.
	args := append([]string{realGit}, os.Args[1:]...)
	if err := syscall.Exec(realGit, args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
