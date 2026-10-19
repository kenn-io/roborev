package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

// stubServiceRecovery restores the recovery seams each test replaces.
func stubServiceRecovery(t *testing.T) {
	t.Helper()
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	oldOut, oldVerbose := lifecycleOut, verbose
	oldFind, oldMainPID, oldStart, oldDrain := findDaemonService, serviceMainPID, startDaemonService, drainManagedDaemon
	oldDiscovery, oldEnsure, oldDetached := getAnyRunningDaemonForStart, startDaemonForEnsure, startDaemonDetached
	oldPause := pendingStartPause
	t.Cleanup(func() {
		lifecycleOut, verbose = oldOut, oldVerbose
		findDaemonService, serviceMainPID, startDaemonService, drainManagedDaemon = oldFind, oldMainPID, oldStart, oldDrain
		getAnyRunningDaemonForStart, startDaemonForEnsure, startDaemonDetached = oldDiscovery, oldEnsure, oldDetached
		pendingStartPause = oldPause
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
			err := recoverDaemon(original, nil)
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
	err := recoverDaemon(&daemon.RuntimeInfo{PID: 1234}, nil)
	require.ErrorIs(t, err, errRecoveryLock)
	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr)
	assert.NotContains(t, err.Error(), "running reviews")
	assert.NotContains(t, err.Error(), "2m0s")
}
