package main

import (
	"bytes"
	"context"
	"errors"
	"os"
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
		name       string
		respawnPID int
		drainErr   error
		listErr    error
	}{
		{name: "initial runtimes"},
		{name: "new PID", respawnPID: 3456},
		{name: "drain failure", drainErr: errors.New("drain rejected")},
		{name: "access denied", drainErr: daemon.ErrDaemonAccessDenied},
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
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return initial, tc.listErr }
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
				release, acquired, err := daemon.RuntimeStore().TryAcquireStartLock(t.Context())
				require.NoError(t, err)
				if acquired {
					release()
				}
				assert.False(t, acquired, "recovery holds the start lock through every drain")
				if tc.drainErr != nil {
					return tc.drainErr
				}
				if tc.respawnPID != 0 {
					runtimes = []*daemon.RuntimeInfo{{PID: tc.respawnPID, Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}}
				} else {
					runtimes = runtimes[1:]
				}
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
				require.ErrorContains(t, err, "failed to list daemon runtimes")
				assert.Empty(t, drained)
				assert.Zero(t, ensures)
				return
			}
			assert.Equal(t, []int{1234, 2345}, drained, "drain the finite initial snapshot even if a launcher publishes another daemon")
			assert.Equal(t, 2, bytes.Count(progress.Bytes(), []byte("Waiting for daemon shutdown;")))
			if tc.drainErr != nil {
				require.ErrorIs(t, err, tc.drainErr)
				require.ErrorContains(t, err, "failed to stop daemon (pid 2345)")
				if errors.Is(tc.drainErr, daemon.ErrDaemonAccessDenied) {
					assert.ErrorContains(t, err, "restore the daemon's startup auth_key or stop PID 2345 manually")
				}
				assert.Zero(t, ensures)
				assert.Zero(t, starts)
			} else {
				require.NoError(t, err)
				assert.Equal(t, 1, ensures)
				if tc.respawnPID != 0 {
					assert.Zero(t, starts)
					assert.Equal(t, tc.respawnPID, runtimes[0].PID, "ordinary startup may reuse the launcher's responsive daemon")
				} else {
					assert.Equal(t, 1, starts)
				}
			}
		})
	}
}
