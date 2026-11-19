//go:build linux || darwin

package proctrack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test binary doubles as a helper process: it allocates memory or leaves
// its session, writes its PID to a file, and sleeps.
func TestMain(m *testing.M) {
	switch os.Getenv("PROCTRACK_HELPER") {
	case "":
		os.Exit(m.Run())
	case "setsid":
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(2)
		}
	case "setsid-later":
		// Stay in the group long enough for samples to see it, then leave.
		if err := os.WriteFile(os.Getenv("PROCTRACK_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		time.Sleep(400 * time.Millisecond)
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("PROCTRACK_PIDFILE")+".left", nil, 0o600); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if err := os.WriteFile(os.Getenv("PROCTRACK_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	mib, _ := strconv.Atoi(os.Getenv("PROCTRACK_MIB"))
	buf := make([]byte, mib<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	time.Sleep(time.Minute)
	runtime.KeepAlive(buf)
	os.Exit(0)
}

// helper returns a shell snippet that runs the helper in mode and the file it
// writes its PID to.
func helper(t *testing.T, mode string, mib int) (string, string) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	exe, err := os.Executable()
	require.NoError(t, err)
	return fmt.Sprintf("PROCTRACK_HELPER=%s PROCTRACK_MIB=%d PROCTRACK_PIDFILE=%s %s", mode, mib, pidFile, exe), pidFile
}

func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil || len(data) == 0 {
			return false
		}
		pid, err = strconv.Atoi(string(data))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	return pid
}

// gone reports whether pid has exited. A zombie reparented to init counts as
// gone once init reaps it.
func requireGone(t *testing.T, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, 5*time.Second, 10*time.Millisecond, "process %d still running", pid)
}

func killOnCleanup(t *testing.T, pid int) {
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
}

func recordGroupKills(t *testing.T) func() []int {
	var mu sync.Mutex
	var got []int
	orig := killGroup
	killGroup = func(pgid int) error {
		mu.Lock()
		got = append(got, pgid)
		mu.Unlock()
		return orig(pgid)
	}
	t.Cleanup(func() { killGroup = orig })
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), got...)
	}
}

func TestStartExitBeforeWatch(t *testing.T) {
	orig := startWatch
	startWatch = func(r *root) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			r.watch()
		}()
	}
	t.Cleanup(func() { startWatch = orig })
	kills := recordGroupKills(t)

	ctx, s := New(t.Context(), 0)
	p, err := Start(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", "exit 3"))
	require.NoError(t, err)
	err = p.Wait()
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	require.True(t, ok, "want exit status, got %v", err)
	assert.Equal(t, 3, exitErr.ExitCode())
	assert.Equal(t, []int{p.cmd.Process.Pid}, kills())
	require.NoError(t, s.Close())
}

func TestNoSignalAfterHandoff(t *testing.T) {
	kills := recordGroupKills(t)
	ctx, s := New(t.Context(), 0)
	p, err := Start(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0"))
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	pid := p.cmd.Process.Pid
	require.NoError(t, s.Close())
	// Only the watcher signals the group, before handing the root to Wait.
	assert.Equal(t, []int{pid}, kills())
}

func TestOutputAfterRootExitGroupHolder(t *testing.T) {
	ctx, s := New(t.Context(), 0)
	defer s.Close()
	run, pidFile := helper(t, "sleep", 0)
	// Wait for the holder before exiting, so it holds stdout.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", fmt.Sprintf("%s & while [ ! -s %s ]; do sleep 0.01; done; echo result", run, pidFile))
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	p, err := Start(ctx, cmd)
	require.NoError(t, err)
	holder := readPID(t, pidFile)
	killOnCleanup(t, holder)

	began := time.Now()
	out, err := io.ReadAll(stdout)
	require.NoError(t, err)
	assert.Equal(t, "result\n", string(out))
	require.NoError(t, p.Wait())
	assert.Less(t, time.Since(began), 5*time.Second)
	requireGone(t, holder)
}

func TestCancelStopsGrandchild(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	ctx, s := New(parent, 0)
	defer s.Close()
	run, pidFile := helper(t, "sleep", 0)
	p, err := Start(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", run+" & wait"))
	require.NoError(t, err)
	grandchild := readPID(t, pidFile)
	killOnCleanup(t, grandchild)
	cancel()
	_ = p.Wait()
	requireGone(t, grandchild)
}

func TestCloseStopsRunningRoot(t *testing.T) {
	ctx, s := New(t.Context(), 0)
	run, pidFile := helper(t, "sleep", 0)
	p, err := Start(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", run))
	require.NoError(t, err)
	child := readPID(t, pidFile)
	killOnCleanup(t, child)
	require.NoError(t, s.Close())
	_ = p.Wait()
	requireGone(t, child)
}

func TestStartRefusedAfterClose(t *testing.T) {
	ctx, s := New(t.Context(), 0)
	require.NoError(t, s.Close())
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := Start(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", "touch "+marker))
	require.ErrorIs(t, err, ErrClosed)
	time.Sleep(100 * time.Millisecond)
	assert.NoFileExists(t, marker)
}

func TestStopOperationReplacesGroupKill(t *testing.T) {
	kills := recordGroupKills(t)
	ctx, s := New(t.Context(), 0)
	stopped := make(chan struct{}, 4)
	p, err := Start(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0"), WithStop(func() { stopped <- struct{}{} }))
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	require.NoError(t, s.Close())
	assert.Empty(t, kills())
	assert.NotEmpty(t, stopped)
}

func TestUntrackedStartWaits(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "echo plain")
	var out strings.Builder
	cmd.Stdout = &out
	require.NoError(t, Run(t.Context(), cmd))
	assert.Equal(t, "plain\n", out.String())
	assert.Nil(t, cmd.SysProcAttr, "untracked launch must not change the process group")
}

func fastSampling(t *testing.T) {
	orig := sampleInterval
	sampleInterval = 50 * time.Millisecond
	t.Cleanup(func() { sampleInterval = orig })
}

func TestBreachStopsAndCancelsAttempt(t *testing.T) {
	fastSampling(t)
	ctx, s := New(t.Context(), 64<<20)
	defer s.Close()
	run, pidFile := helper(t, "alloc", 256)
	// The intermediate subshell exits right after starting the allocator, so
	// the allocator is found through its group, not its parent.
	p, err := Start(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", "("+run+" &); sleep 60"))
	require.NoError(t, err)
	alloc := readPID(t, pidFile)
	killOnCleanup(t, alloc)

	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("attempt not canceled after exceeding the limit")
	}
	b, ok := errors.AsType[*Breach](context.Cause(ctx))
	require.True(t, ok, "cause: %v", context.Cause(ctx))
	assert.Equal(t, s.Breach(), b)
	assert.Equal(t, alloc, b.Largest.PID)
	assert.Greater(t, b.RSS, uint64(64<<20))
	_ = p.Wait()
	requireGone(t, alloc)
}

func TestBreachInPhaseCancelsOnlyThePhase(t *testing.T) {
	fastSampling(t)
	ctx, attempt := New(t.Context(), 64<<20)
	defer attempt.Close()
	phaseCtx, phase := OpenScope(ctx)
	run, pidFile := helper(t, "alloc", 256)
	p, err := Start(phaseCtx, exec.CommandContext(ctx, "/bin/sh", "-c", run))
	require.NoError(t, err)
	killOnCleanup(t, readPID(t, pidFile))

	select {
	case <-phaseCtx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("phase not canceled after exceeding the limit")
	}
	_ = p.Wait()
	require.NoError(t, phase.Close())
	assert.NotNil(t, phase.Breach())
	assert.Nil(t, attempt.Breach())
	assert.NoError(t, ctx.Err())

	// The attempt can still start processes after the phase ends.
	require.NoError(t, Run(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0")))
}

func TestCloseStopsProcessThatLeftItsGroupAfterSampling(t *testing.T) {
	fastSampling(t)
	ctx, s := New(t.Context(), 1<<40) // sample, never breach
	run, pidFile := helper(t, "setsid-later", 0)
	p, err := Start(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", run+" & wait"))
	require.NoError(t, err)
	pid := readPID(t, pidFile)
	killOnCleanup(t, pid)
	require.Eventually(t, func() bool {
		_, err := os.Stat(pidFile + ".left")
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)

	err = s.Close()
	_ = p.Wait()
	if runtime.GOOS == "linux" {
		require.NoError(t, err)
		requireGone(t, pid)
	} else {
		// No pidfd: the process is reported, never signaled by number.
		require.ErrorContains(t, err, fmt.Sprintf("(pid %d)", pid))
	}
}

func TestSampleSkipsRootRegisteredDuringScan(t *testing.T) {
	orig := killGroup
	killGroup = func(int) error { return nil }
	t.Cleanup(func() { killGroup = orig })
	ctx, s := New(t.Context(), 64<<20)
	defer s.Close()
	scanned := time.Now()
	// The snapshot still lists an older, large process under the PID that a
	// root registered after the scan began now uses.
	const pid = 1 << 22
	table := map[int]Proc{pid: {PID: pid, PGID: pid, RSS: 1 << 30, Name: "old"}}
	r := &root{scope: s, pid: pid, registered: scanned.Add(time.Millisecond), handedOff: make(chan struct{})}
	close(r.handedOff)
	s.t.mu.Lock()
	s.roots[pid] = r
	s.t.mu.Unlock()

	s.t.sample(table, nil, scanned)
	assert.Nil(t, s.Breach())

	// The same snapshot taken after registration counts the root.
	s.t.sample(table, nil, scanned.Add(time.Second))
	assert.NotNil(t, s.Breach())
	<-ctx.Done() // the breach's cleanup has finished
}
