package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type countingCloser struct {
	closed atomic.Int32
}

func (c *countingCloser) Close() error {
	c.closed.Add(1)
	return nil
}

func TestCloseOnContextDoneClosesOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	closer := &countingCloser{}
	stop := closeOnContextDone(ctx, closer)
	defer stop()

	cancel()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if closer.closed.Load() == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.Equal(t, int32(1), closer.closed.Load(), "expected closer to be closed after context cancellation")
}

func TestCloseOnContextDoneStopPreventsClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	closer := &countingCloser{}
	stop := closeOnContextDone(ctx, closer)
	stop()
	cancel()
	time.Sleep(20 * time.Millisecond)

	require.Equal(t, int32(0), closer.closed.Load(), "closer should not be closed after stop()")
}

func TestCloseOnContextDoneBackgroundIsNoop(t *testing.T) {
	closer := &countingCloser{}
	stop := closeOnContextDone(context.Background(), closer)
	stop()

	require.Equal(t, int32(0), closer.closed.Load(), "background context should not close the closer")
}

func TestContextProcessError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tracker := &subprocessTracker{}
	tracker.canceledByContext.Store(true)

	require.NoError(t, contextProcessError(ctx, tracker, errors.New("agent failed"), nil), "real subprocess error should be preserved")
	require.ErrorIs(t, contextProcessError(ctx, tracker, exec.ErrWaitDelay, nil), context.Canceled, "expected context cancellation for wait delay")
	require.ErrorIs(t, contextProcessError(ctx, tracker, nil, fs.ErrClosed), context.Canceled, "expected context cancellation for closed pipe parse error")
	require.NoError(t, contextProcessError(ctx, tracker, errors.New("agent failed"), fs.ErrClosed), "real subprocess error should not be masked by closed pipe parse error")
}

func TestContextProcessErrorParseOnlyPathWouldMaskRealWaitErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tracker := &subprocessTracker{}
	tracker.canceledByContext.Store(true)
	waitErr := errors.New("exit status 1")

	require.NoError(t, contextProcessError(ctx, tracker, waitErr, fs.ErrClosed))
	require.ErrorIs(t, contextProcessError(ctx, tracker, nil, fs.ErrClosed), context.Canceled)
}

func TestContextProcessErrorRunPathCancellation(t *testing.T) {
	skipIfWindows(t)

	prev := subprocessWaitDelay
	subprocessWaitDelay = 50 * time.Millisecond
	t.Cleanup(func() { subprocessWaitDelay = prev })

	// Use a shell script (not a bare binary) to exercise the realistic
	// process tree: sh forks sleep as a child, so killing sh leaves an
	// orphan — matching what happens with real agent subprocesses.
	// The etxtbsy guard prevents Go 1.25's ETXTBSY probe from running
	// the full script in a second child process that is never killed.
	cmdPath := writeTempCommand(t, "#!/bin/sh\ncase \"$1\" in *etxtbsy*) exit 0;; esac\nsleep 5\n")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, cmdPath)
	tracker := configureSubprocess(cmd)

	err := cmd.Run()
	require.Error(t, err, "expected command cancellation")

	require.EqualError(t, contextProcessError(ctx, tracker, err, nil), context.DeadlineExceeded.Error(), "expected deadline exceeded")
}

func TestContextProcessErrorDoesNotMaskSignalExitAfterContextDone(t *testing.T) {
	skipIfWindows(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "kill -KILL $$")
	tracker := configureSubprocess(cmd)

	err := cmd.Run()
	require.Error(t, err, "expected signal exit")

	cancel()

	got := contextProcessError(ctx, tracker, err, nil)
	if got != nil {
		require.Equal(t, err, got, "signal exit should not be rewritten as context error")
	}
}

func TestConfigureSubprocessSetsOptionalLocks(t *testing.T) {
	skipIfWindows(t)

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo $GIT_OPTIONAL_LOCKS")
	configureSubprocess(cmd)

	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "0\n", string(out),
		"configureSubprocess should set GIT_OPTIONAL_LOCKS=0")
}

func TestConfigureSubprocessPreservesExistingEnv(t *testing.T) {
	skipIfWindows(t)

	cmd := exec.CommandContext(context.Background(),
		"sh", "-c", "echo $MY_TEST_VAR:$GIT_OPTIONAL_LOCKS")
	cmd.Env = append(os.Environ(), "MY_TEST_VAR=hello")
	configureSubprocess(cmd)

	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "hello:0\n", string(out),
		"configureSubprocess should preserve existing env and add GIT_OPTIONAL_LOCKS=0")
}

func TestConfigureSubprocessPreservesPWD(t *testing.T) {
	skipIfWindows(t)

	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo $PWD")
	cmd.Dir = dir
	configureSubprocess(cmd)

	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, dir+"\n", string(out),
		"configureSubprocess should preserve PWD matching cmd.Dir")
}

func TestConfigureCapabilityProbePreservesRelativeCommandPath(t *testing.T) {
	skipIfWindows(t)

	repoDir := t.TempDir()
	binDir := filepath.Join(repoDir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0o755))
	commandPath := filepath.Join(binDir, "codex")
	require.NoError(t, os.WriteFile(commandPath, []byte("#!/bin/sh\necho probe-ok\n"), 0o755))
	t.Chdir(repoDir)

	cmd := exec.CommandContext(context.Background(), "./bin/codex", "--help")
	configureCapabilityProbe(cmd)

	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "probe-ok\n", string(out))
	require.Equal(t, os.TempDir(), cmd.Dir)
}

func TestConfigureSubprocessDoesNotMarkCanceledWhenProcessAlreadyExited(t *testing.T) {
	skipIfWindows(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 0")
	tracker := configureSubprocess(cmd)

	require.NoError(t, cmd.Run())
	require.NotNil(t, cmd.Cancel, "expected wrapped cancel")

	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		require.ErrorIs(t, err, os.ErrProcessDone, "expected os.ErrProcessDone, got %v", err)
	}
	require.False(t, tracker.canceledByContext.Load(), "tracker should stay false when cancel runs after process exit")
}
