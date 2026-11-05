package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

func exitedDaemonPID(t *testing.T) int {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	child := exec.CommandContext(t.Context(), binary, "-test.run=^$")
	require.NoError(t, child.Run())
	return child.Process.Pid
}

// stubServiceRecovery restores the recovery seams each test replaces.
func stubServiceRecovery(t *testing.T) {
	t.Helper()
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	oldOut, oldVerbose := lifecycleOut, verbose
	oldFind, oldMainPID, oldStart, oldDrain := findDaemonService, serviceMainPID, startDaemonService, drainManagedDaemon
	oldCgroup := daemonCgroupUnit
	oldDiscovery, oldEnsure, oldDetached := getAnyRunningDaemonForStart, startDaemonForEnsure, startDaemonDetached
	oldPause := pendingStartPause
	oldExecutable := cliExecutableInfo
	t.Cleanup(func() {
		lifecycleOut, verbose = oldOut, oldVerbose
		findDaemonService, serviceMainPID, startDaemonService, drainManagedDaemon = oldFind, oldMainPID, oldStart, oldDrain
		daemonCgroupUnit = oldCgroup
		getAnyRunningDaemonForStart, startDaemonForEnsure, startDaemonDetached = oldDiscovery, oldEnsure, oldDetached
		pendingStartPause = oldPause
		cliExecutableInfo = oldExecutable
	})
}

func TestManagedRecoveryBlocksCompetingColdStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateDaemonSelection(t)
		stubServiceRecovery(t)
		oldList := listAllRuntimes
		t.Cleanup(func() { listAllRuntimes = oldList })
		binary, err := os.Executable()
		require.NoError(t, err)
		cliExecutableInfo = func() (os.FileInfo, error) { return os.Stat(binary) }
		var progress bytes.Buffer
		lifecycleOut = &progress
		old := &daemon.RuntimeInfo{PID: os.Getpid(), Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}
		var current atomic.Pointer[daemon.RuntimeInfo]
		current.Store(old)
		drained, resume := make(chan struct{}), make(chan struct{})
		finishDrain := make(chan struct{})
		findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
			return &daemonService{unit: "fixture.service", executable: binary, mainPID: pid, serviceType: "notify"}, nil
		}
		getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
			if current.Load() == nil {
				return nil, os.ErrNotExist
			}
			return current.Load(), nil
		}
		getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return getAnyRunningDaemonForStart(t.Context()) }
		listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return []*daemon.RuntimeInfo{old}, nil }
		serviceMainPID = func(context.Context, string) (int, error) { return current.Load().PID, nil }
		drains := 0
		drainManagedDaemon = func(*daemon.RuntimeInfo) error {
			drains++
			current.Store(nil)
			close(drained)
			<-finishDrain
			time.Sleep(daemonStartTimeout + time.Minute)
			return nil
		}
		startDaemonService = func(ctx context.Context, _ string) error {
			assert.NoError(t, ctx.Err(), "startup gets a fresh budget after draining") //nolint:testifylint // Keep running so the goroutine sends its recovery result.
			<-resume
			current.Store(&daemon.RuntimeInfo{PID: 5678, Version: version.Version, Network: "tcp", Address: "127.0.0.1:1"})
			return nil
		}
		detached := 0
		startDaemonDetached = func(context.Context, detachedDaemonOptions) error { detached++; return nil }
		recovery, coldStart := make(chan error, 1), make(chan error, 1)
		competingRecovery, ensure := make(chan error, 1), make(chan error, 1)
		go func() { recovery <- recoverDaemon(old, nil, false) }()
		<-drained
		synctest.Wait()
		assert.Contains(t, progress.String(), "Waiting for daemon shutdown; no new reviews will start, and any running reviews will finish first...\n")
		assert.Empty(t, recovery, "recovery must remain blocked while reviews drain")
		go func() { coldStart <- startDaemon() }()
		progress.Reset()
		go func() { competingRecovery <- recoverDaemon(old, nil, false) }()
		go func() { ensure <- ensureDaemon() }()
		synctest.Wait()
		assert.Contains(t, progress.String(), "Waiting for daemon recovery lock; another command may be waiting for running reviews to finish...\n")
		assert.Empty(t, coldStart, "cold start must wait while the unit replaces its daemon")
		assert.Zero(t, detached)
		time.Sleep(daemonStartTimeout + time.Minute)
		require.ErrorIs(t, <-coldStart, context.DeadlineExceeded)
		synctest.Wait()
		assert.Empty(t, recovery, "the owning recovery must keep waiting for the active review")
		assert.Empty(t, competingRecovery, "a competing recovery waits for the active review instead of failing")
		assert.Empty(t, ensure, "ensureDaemon must wait beyond the startup budget while recovery drains")
		close(finishDrain)
		close(resume)
		require.NoError(t, <-recovery)
		require.NoError(t, <-competingRecovery, "a competing recovery reuses the replacement")
		require.NoError(t, <-ensure, "ensureDaemon reuses the responsive replacement")
		release, acquired, err := daemon.RuntimeStore().TryAcquireStartLock(t.Context())
		require.NoError(t, err)
		require.True(t, acquired, "ensureDaemon must release the recovery lock")
		release()
		assert.Equal(t, 1, drains)
		assert.Zero(t, detached)
	})
}

func TestUnmanagedRecoveryDrainsDiscoveredRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		drainErr error
		listErr  error
	}{
		{name: "drain every runtime in the snapshot"},
		{name: "drain failure", drainErr: errors.New("drain rejected")},
		{name: "list failure", listErr: errors.New("runtime directory unreadable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubServiceRecovery(t)
			var progress bytes.Buffer
			lifecycleOut = &progress
			runtimes := []*daemon.RuntimeInfo{{PID: 1234, Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}, {PID: 2345, Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}}
			original := runtimes[0]
			initial := append([]*daemon.RuntimeInfo(nil), runtimes...)
			oldList := listAllRuntimes
			t.Cleanup(func() { listAllRuntimes = oldList })
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
				if tc.listErr != nil {
					return nil, tc.listErr
				}
				return initial, nil
			}
			findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
				if len(runtimes) == 0 {
					return nil, os.ErrNotExist
				}
				return runtimes[0], nil
			}
			var drained []int
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error {
				drained = append(drained, info.PID)
				if tc.drainErr != nil {
					return tc.drainErr
				}
				runtimes = runtimes[1:]
				return nil
			}
			starts, ensures := 0, 0
			startDaemonForEnsure = func() error { ensures++; return startDaemon() }
			startDaemonDetached = func(context.Context, detachedDaemonOptions) error {
				starts++
				runtimes = []*daemon.RuntimeInfo{{PID: 5678, Version: version.Version, Network: "tcp", Address: "127.0.0.1:1"}}
				return nil
			}
			err := recoverDaemon(original, nil, false)
			if tc.listErr != nil {
				require.ErrorIs(t, err, tc.listErr)
				assert.Empty(t, drained)
				assert.Zero(t, ensures)
				return
			}
			assert.Equal(t, []int{1234, 2345}, drained, "drain every runtime in the initial snapshot")
			assert.Equal(t, 2, bytes.Count(progress.Bytes(), []byte("Waiting for daemon shutdown;")))
			if tc.drainErr != nil {
				require.ErrorIs(t, err, tc.drainErr)
				assert.Zero(t, ensures)
				assert.Zero(t, starts)
			} else {
				require.NoError(t, err)
				assert.Equal(t, 1, ensures)
				assert.Equal(t, 1, starts)
			}
		})
	}
}

func TestRecoverDaemonLockFilesystemFailure(t *testing.T) {
	stubServiceRecovery(t)
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	t.Setenv("ROBOREV_DATA_DIR", path)
	findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
	err := recoverDaemon(&daemon.RuntimeInfo{PID: 1234}, nil, false)
	require.ErrorIs(t, err, errRecoveryLock)
	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr)
	assert.NotContains(t, err.Error(), "running reviews")
	assert.NotContains(t, err.Error(), "2m0s")
}
