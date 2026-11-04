package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	oldCgroup := daemonServiceUnit
	oldDiscovery, oldEnsure, oldDetached := getAnyRunningDaemonForStart, startDaemonForEnsure, startDaemonDetached
	oldPause := pendingStartPause
	t.Cleanup(func() {
		lifecycleOut, verbose = oldOut, oldVerbose
		findDaemonService, serviceMainPID, startDaemonService, drainManagedDaemon = oldFind, oldMainPID, oldStart, oldDrain
		daemonServiceUnit = oldCgroup
		getAnyRunningDaemonForStart, startDaemonForEnsure, startDaemonDetached = oldDiscovery, oldEnsure, oldDetached
		pendingStartPause = oldPause
	})
}

func TestUnresponsiveManagedDaemonStaysAlive(t *testing.T) {
	for _, discovery := range []string{"missed under lock", "failed reprobe", "missed initially"} {
		t.Run(discovery, func(t *testing.T) {
			stubServiceRecovery(t)
			oldDiscover, oldProbe, oldList, oldCleanup := getAnyRunningDaemon, probeDaemonForEnsure, listAllRuntimes, cleanupZombieDaemons
			t.Cleanup(func() {
				getAnyRunningDaemon, probeDaemonForEnsure, listAllRuntimes, cleanupZombieDaemons = oldDiscover, oldProbe, oldList, oldCleanup
			})
			info := &daemon.RuntimeInfo{PID: os.Getpid(), Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}
			executable, err := currentExecutable()
			require.NoError(t, err)
			owner := &daemonService{unit: "fixture.service", executable: executable, mainPID: info.PID, serviceType: "simple"}
			findDaemonService = func(context.Context, int) (*daemonService, error) { return owner, nil }
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
				if discovery == "failed reprobe" {
					return info, nil
				}
				return nil, os.ErrNotExist
			}
			probeErr := errors.New("probe timed out")
			probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) { return nil, probeErr }
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return []*daemon.RuntimeInfo{info}, nil }
			var events []string
			cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { events = append(events, "cleanup"); return 0 }
			startDaemonForEnsure = func() error { events = append(events, "detached"); return nil }
			drainManagedDaemon = func(*daemon.RuntimeInfo) error { events = append(events, "drain"); return nil }
			startDaemonService = func(context.Context, string) error {
				events = append(events, "start")
				getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return info, nil }
				return nil
			}
			serviceMainPID = func(context.Context, string) (int, error) { return info.PID, nil }
			paused := true
			pendingStartPause = &paused
			if discovery == "missed initially" {
				err = ensureDaemon()
			} else {
				err = recoverDaemon(info, probeErr, false)
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "alive but not responding")
			assert.Contains(t, err.Error(), serviceRestartHint(owner.unit))
			assert.Empty(t, events, "refuse before pause, drain, cleanup or service start")

			withStatusWebRuntime(t, func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist })
			oldEnsure := statusEnsureDaemon
			t.Cleanup(func() { statusEnsureDaemon = oldEnsure })
			recoveryErr := err
			statusEnsureDaemon = func() error { return recoveryErr }
			cmd := statusCmd()
			cmd.SetArgs([]string{"--json"})
			output := captureStdout(t, func() { require.NoError(t, cmd.Execute()) })
			var result statusJSONResult
			require.NoError(t, json.Unmarshal([]byte(output), &result))
			assert.True(t, result.Running, "report the specific live runtime even while HTTP is unavailable")
			assert.Nil(t, result.Daemon)
			assert.Nil(t, result.Health)
			assert.Empty(t, result.Jobs)
		})
	}
}

func TestRestartManagedDaemon(t *testing.T) {
	for _, outcome := range []string{"replace", "child", "foreign", "unsupported", "lookup error", "unresponsive"} {
		t.Run(outcome, func(t *testing.T) {
			stubServiceRecovery(t)
			executable, err := currentExecutable()
			require.NoError(t, err)
			current := &daemon.RuntimeInfo{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:1"}
			owner := &daemonService{unit: "fixture.service", executable: executable, mainPID: current.PID, serviceType: "simple"}
			if outcome == "child" {
				owner.mainPID++
			}
			if outcome == "foreign" {
				owner.executable = filepath.Join(t.TempDir(), "other")
				require.NoError(t, os.WriteFile(owner.executable, nil, 0o600))
			}
			if outcome == "unsupported" {
				owner.remainAfterExit = true
			}
			lifecycleOut = &bytes.Buffer{}
			findDaemonService = func(context.Context, int) (*daemonService, error) {
				if outcome == "lookup error" {
					return nil, errors.New("owner unavailable")
				}
				return owner, nil
			}
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
				if outcome == "unresponsive" {
					return nil, os.ErrNotExist
				}
				return current, nil
			}
			oldList := listAllRuntimes
			t.Cleanup(func() { listAllRuntimes = oldList })
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return []*daemon.RuntimeInfo{current}, nil }
			var events []string
			drainManagedDaemon = func(*daemon.RuntimeInfo) error { events = append(events, "drain"); return nil }
			startDaemonService = func(context.Context, string) error {
				events = append(events, "start")
				getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return current, nil }
				return nil
			}
			serviceMainPID = func(context.Context, string) (int, error) { return current.PID, nil }
			startDaemonForEnsure = func() error { events = append(events, "detached"); return nil }
			wasRunning, err := restartDaemon(t.Context())
			assert.True(t, wasRunning)
			if outcome == "replace" || outcome == "unresponsive" {
				require.NoError(t, err)
				assert.Equal(t, []string{"drain", "start"}, events)
			} else {
				require.Error(t, err)
				assert.Empty(t, events)
				if outcome == "unsupported" {
					assert.EqualError(t, err, serviceDescription(owner.unit)+": RemainAfterExit=yes requires manual restart; run "+serviceRestartHint(owner.unit))
				}
			}
		})
	}
}

func TestRestartUnmanagedReleasesStartLock(t *testing.T) {
	stubServiceRecovery(t)
	oldStop, oldEnsure := daemonStop, daemonEnsure
	t.Cleanup(func() { daemonStop, daemonEnsure = oldStop, oldEnsure })
	findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return &daemon.RuntimeInfo{PID: 42}, nil }
	daemonStop = func() error { return nil }
	acquired := false
	daemonEnsure = func() error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		release, err := daemon.RuntimeStore().AcquireStartLock(ctx)
		if err == nil {
			acquired = true
			release()
		}
		return err
	}
	wasRunning, err := restartDaemon(t.Context())
	require.NoError(t, err)
	assert.True(t, wasRunning)
	assert.True(t, acquired)
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

func TestRecoverVanishedManagedDaemonCanStart(t *testing.T) {
	stubServiceRecovery(t)
	info := &daemon.RuntimeInfo{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:1"}
	executable, err := currentExecutable()
	require.NoError(t, err)
	owner := &daemonService{unit: "fixture.service", executable: executable, mainPID: info.PID, serviceType: "simple"}
	lookups := 0
	findDaemonService = func(context.Context, int) (*daemonService, error) {
		lookups++
		if lookups == 1 {
			return owner, nil
		}
		return nil, os.ErrNotExist
	}
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	var events []string
	drainManagedDaemon = func(*daemon.RuntimeInfo) error { events = append(events, "drain"); return nil }
	startDaemonForEnsure = func() error { events = append(events, "detached"); return nil }
	startDaemonService = func(context.Context, string) error {
		events = append(events, "start")
		getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return info, nil }
		return nil
	}
	serviceMainPID = func(context.Context, string) (int, error) { return info.PID, nil }
	require.NoError(t, recoverDaemon(info, errors.New("probe failed"), false))
	assert.Equal(t, []string{"start"}, events)
}

func TestEnsureUnresponsiveOwnershipFailure(t *testing.T) {
	for _, stage := range []string{"initial discovery", "recovery lookup"} {
		t.Run(stage, func(t *testing.T) {
			stubServiceRecovery(t)
			oldDiscover, oldList, oldCleanup := getAnyRunningDaemon, listAllRuntimes, cleanupZombieDaemons
			t.Cleanup(func() { getAnyRunningDaemon, listAllRuntimes, cleanupZombieDaemons = oldDiscover, oldList, oldCleanup })
			info := &daemon.RuntimeInfo{PID: os.Getpid()}
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return []*daemon.RuntimeInfo{info}, nil }
			ownerErr := &managedRecoveryError{"fixture.service", errors.New("owner unavailable")}
			findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, ownerErr }
			var events []string
			cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { events = append(events, "cleanup"); return 0 }
			startDaemonForEnsure = func() error { events = append(events, "detached"); return nil }
			var err error
			if stage == "initial discovery" {
				err = ensureDaemon()
			} else {
				err = recoverDaemon(info, errors.New("probe unavailable"), false)
			}
			require.ErrorIs(t, err, ownerErr)
			assert.Empty(t, events)
			withStatusWebRuntime(t, func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist })
			oldEnsure := statusEnsureDaemon
			t.Cleanup(func() { statusEnsureDaemon = oldEnsure })
			statusEnsureDaemon = func() error { return err }
			cmd := statusCmd()
			cmd.SetArgs([]string{"--json"})
			output := captureStdout(t, func() { require.NoError(t, cmd.Execute()) })
			var result statusJSONResult
			require.NoError(t, json.Unmarshal([]byte(output), &result))
			assert.True(t, result.Running)
			assert.Contains(t, result.Error, "owner unavailable")
			assert.Nil(t, result.Daemon)
			assert.Nil(t, result.Health)
			assert.Empty(t, result.Jobs)
			info.PID = 1 << 30
			assert.False(t, managedRecoveryStatus(err).Running, "recheck process exit before rendering status")
		})
	}
}
