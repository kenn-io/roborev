//go:build linux

package testutil

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// StartLivenessChild starts a child identified as a roborev daemon. The returned
// function lets it exit and observes the exit without reaping it until cleanup.
func StartLivenessChild(t *testing.T) (int, func()) {
	t.Helper()
	// Cleanup closes stdin and waits after the test context is canceled.
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "sh", "-c", "read -r line || true", "roborev", "daemon", "run")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		require.NoError(t, cmd.Wait())
	})

	return cmd.Process.Pid, func() {
		t.Helper()
		require.NoError(t, stdin.Close())
		var waitErr error
		require.Eventually(t, func() bool {
			var info unix.Siginfo
			waitErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
			return waitErr != nil || info.Signo != 0
		}, 5*time.Second, 10*time.Millisecond)
		require.NoError(t, waitErr)
	}
}
