//go:build codexeval && (aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package skills

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func prepareEvalCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
	}
}

func TestEvalCommandTerminatesProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 & wait")
	prepareEvalCommand(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	started := time.Now()
	err := cmd.Run()
	require.Error(t, err)
	assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 3*time.Second)
}

func TestRoborevStubWritesPerCaseSentinelWhenOutputRedirected(t *testing.T) {
	stub := createUniqueRoborevStub(t)
	sentinelPath := roborevExecutionSentinelPath(stub.Dir, stub.Marker)
	cmd := exec.Command(stub.Path, "status")
	cmd.Stdout = io.Discard
	require.NoError(t, cmd.Run())
	_, err := os.Stat(sentinelPath)
	require.NoError(t, err, "stub execution must remain observable when stdout is redirected")

	nextMarker := rewriteRoborevStub(t, stub.Path)
	nextSentinelPath := roborevExecutionSentinelPath(stub.Dir, nextMarker)
	assert.NotEqual(t, sentinelPath, nextSentinelPath)
	_, err = os.Stat(nextSentinelPath)
	assert.ErrorIs(t, err, os.ErrNotExist, "each rewritten case must start without prior execution evidence")
}
