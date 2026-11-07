//go:build linux

package main

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestIsPIDAliveForUpdateRejectsUnreapedChild(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { require.NoError(t, cmd.Wait()) })

	// Observe the exit without reaping the child, as a container without init does.
	var waitErr error
	require.Eventually(t, func() bool {
		var info unix.Siginfo
		waitErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		return waitErr != nil || info.Signo != 0
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, waitErr)
	assert.False(t, isPIDAliveForUpdateDefault(cmd.Process.Pid))
}
