package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

func TestRecoverDaemonServiceRecovery(t *testing.T) {
	probeErr := errors.New("probe failed")
	for _, tc := range []struct {
		outcome       string
		probeErr      error
		wantError     string
		stops, starts int
	}{
		{outcome: "replacement current"},
		{outcome: "child responsive"},
		{outcome: "child failed", probeErr: probeErr, wantError: "service child requires attention"},
		{outcome: "replacement outdated", stops: 1, starts: 1},
		{outcome: "missing PID"},
		{outcome: "vanished owned", starts: 1},
		{outcome: "failed", probeErr: probeErr, stops: 1, starts: 1},
		{outcome: "foreign failed", probeErr: probeErr, wantError: "probe failed"},
		{outcome: "uncertain", wantError: "properties unavailable"},
		{outcome: "unreadable", wantError: "cannot confirm executable"},
		{outcome: "bus responsive"},
		{outcome: "bus failed", probeErr: probeErr, wantError: "user bus unavailable"},
		{outcome: "start failure", wantError: "start rejected", stops: 1, starts: 1},
		{outcome: "wrong PID", wantError: "replacement is outside service", stops: 1, starts: 1},
		{outcome: "readiness failure", wantError: "replacement readiness", stops: 1, starts: 1},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			oldOut, oldVerbose := lifecycleOut, verbose
			var progress bytes.Buffer
			lifecycleOut, verbose = &progress, tc.outcome == "bus responsive"
			t.Cleanup(func() { lifecycleOut, verbose = oldOut, oldVerbose })
			oldFind, oldDrain, oldRead := findDaemonService, drainManagedDaemon, readServiceProcessFile
			oldDiscovery, oldRestart := getAnyRunningDaemonForStart, startDaemonForEnsure
			t.Cleanup(func() {
				findDaemonService, drainManagedDaemon, readServiceProcessFile = oldFind, oldDrain, oldRead
				getAnyRunningDaemonForStart, startDaemonForEnsure = oldDiscovery, oldRestart
			})
			original := &daemon.RuntimeInfo{PID: 1234, Version: version.Version}
			current := original
			if strings.HasPrefix(tc.outcome, "replacement") {
				current = &daemon.RuntimeInfo{PID: 5678, Version: "v-old"}
				if tc.outcome == "replacement current" {
					current.Version = version.Version
				}
			}
			selectedPID := current.PID
			executable := "/proc/self/exe"
			if strings.HasPrefix(tc.outcome, "foreign") {
				executable = filepath.Join(t.TempDir(), "roborev")
				require.NoError(t, os.WriteFile(executable, []byte("another installation"), 0o700))
			} else if tc.outcome == "unreadable" {
				executable = filepath.Join(t.TempDir(), "missing")
			}
			started := false
			manager := &daemonServiceManager{close: func() {}}
			manager.lookup = func(_ context.Context, pid int) (*daemonService, error) {
				assert.Equal(t, current.PID, pid, "ownership must use the daemon discovered under the lock")
				if strings.HasPrefix(tc.outcome, "bus") {
					return nil, &managedRecoveryError{"fixture.service", errUserBusUnavailable}
				}
				if tc.outcome == "uncertain" {
					return nil, &managedRecoveryError{"fixture.service", errors.New("properties unavailable")}
				}
				if started && tc.outcome == "wrong PID" {
					return &daemonService{unit: "fixture.service", mainPID: 9999}, nil
				}
				mainPID := pid
				if strings.HasPrefix(tc.outcome, "child") {
					mainPID = 9999
				}
				return &daemonService{unit: "fixture.service", executable: executable, mainPID: mainPID, manager: manager}, nil
			}
			lookups := 0
			findDaemonService = func(ctx context.Context, pid int) (*daemonService, error) {
				lookups++
				if lookups == 1 {
					assert.Equal(t, original.PID, pid)
					return &daemonService{unit: "fixture.service", executable: executable, mainPID: pid, manager: manager}, nil
				}
				if tc.outcome == "vanished owned" {
					return nil, os.ErrNotExist
				}
				return manager.lookup(ctx, pid)
			}
			if tc.outcome == "missing PID" {
				findDaemonService = lookupDaemonService
				readServiceProcessFile = func(path string) ([]byte, error) {
					assert.Equal(t, "/proc/1234/cgroup", path)
					return nil, os.ErrNotExist
				}
			}
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
				if !started && tc.probeErr != nil || (tc.outcome == "missing PID" || tc.outcome == "vanished owned") && !started || started && tc.outcome == "readiness failure" {
					return nil, os.ErrNotExist
				}
				return current, nil
			}
			stops, starts, detached := 0, 0, 0
			startDaemonForEnsure = func() error { detached++; return nil }
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error { stops++; assert.Equal(t, selectedPID, info.PID); return nil }
			manager.start = func(_ context.Context, unit string) error {
				assert.Equal(t, "fixture.service", unit)
				starts++
				started = true
				current = &daemon.RuntimeInfo{PID: 9012}
				if tc.outcome == "start failure" {
					return errors.New("start rejected")
				}
				return nil
			}
			var err error
			if tc.outcome == "readiness failure" || tc.outcome == "wrong PID" {
				// Readiness polls until its startup budget expires.
				synctest.Test(t, func(*testing.T) { err = recoverDaemon(original, tc.probeErr) })
			} else {
				err = recoverDaemon(original, tc.probeErr)
			}
			if tc.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantError)
				var managed *managedRecoveryError
				require.ErrorAs(t, err, &managed)
			}
			assert.Equal(t, tc.stops, stops)
			assert.Equal(t, tc.starts, starts)
			if tc.outcome == "bus responsive" {
				assert.Contains(t, progress.String(), "Daemon recovery skipped; user bus unavailable")
			}
			if tc.outcome == "missing PID" {
				assert.Equal(t, 1, detached)
			} else {
				assert.Zero(t, detached)
			}
		})
	}
}

func TestStatusManagedRecoveryFailure(t *testing.T) {
	old := statusEnsureDaemon
	statusEnsureDaemon = func() error { return &managedRecoveryError{"fixture.service", errors.New("start rejected")} }
	t.Cleanup(func() { statusEnsureDaemon = old })
	cmd := statusCmd()
	cmd.SetArgs([]string{"--json"})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	stdout := captureStdout(t, func() { require.ErrorContains(t, cmd.Execute(), "daemon status unavailable") })
	assert.Empty(t, stdout)
	assert.Contains(t, stderr.String(), "fixture.service")
}

func TestManagedRecoveryBlocksCompetingColdStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldOut := lifecycleOut
		var progress bytes.Buffer
		lifecycleOut = &progress
		t.Cleanup(func() { lifecycleOut = oldOut })
		oldFind, oldDrain := findDaemonService, drainManagedDaemon
		manager := &daemonServiceManager{close: func() {}}
		oldDiscovery, oldDetached := getAnyRunningDaemonForStart, startDaemonDetached
		t.Cleanup(func() {
			findDaemonService, drainManagedDaemon = oldFind, oldDrain
			getAnyRunningDaemonForStart, startDaemonDetached = oldDiscovery, oldDetached
		})
		old := &daemon.RuntimeInfo{PID: 1234}
		var current atomic.Pointer[daemon.RuntimeInfo]
		current.Store(old)
		drained, resume := make(chan struct{}), make(chan struct{})
		finishDrain := make(chan struct{})
		manager.lookup = func(_ context.Context, pid int) (*daemonService, error) {
			return &daemonService{unit: "fixture.service", executable: "/proc/self/exe", mainPID: pid, manager: manager}, nil
		}
		findDaemonService = manager.lookup
		getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
			if current.Load() == nil {
				return nil, os.ErrNotExist
			}
			return current.Load(), nil
		}
		drainManagedDaemon = func(*daemon.RuntimeInfo) error {
			current.Store(nil)
			close(drained)
			<-finishDrain
			time.Sleep(daemonStartTimeout + time.Minute)
			return nil
		}
		manager.start = func(ctx context.Context, _ string) error {
			require.NoError(t, ctx.Err(), "startup gets a fresh budget after draining")
			<-resume
			current.Store(&daemon.RuntimeInfo{PID: 5678, Version: version.Version})
			return nil
		}
		detached := 0
		startDaemonDetached = func(context.Context, detachedDaemonOptions) error { detached++; return nil }
		recovery, coldStart := make(chan error, 1), make(chan error, 1)
		competingRecovery := make(chan error, 1)
		go func() { recovery <- recoverDaemon(old, nil) }()
		<-drained
		synctest.Wait()
		assert.Contains(t, progress.String(), "Waiting for daemon shutdown; no new reviews will start, and any running reviews will finish first...\n")
		assert.Empty(t, recovery, "recovery must remain blocked while reviews drain")
		close(finishDrain)
		go func() { coldStart <- startDaemon() }()
		progress.Reset()
		go func() { competingRecovery <- recoverDaemon(old, nil) }()
		synctest.Wait()
		assert.Contains(t, progress.String(), "Waiting for daemon recovery lock; another command may be waiting for running reviews to finish...\n")
		assert.Empty(t, coldStart, "cold start must wait while the unit replaces its daemon")
		assert.Zero(t, detached)
		time.Sleep(daemonStartTimeout + time.Minute)
		require.Error(t, <-coldStart)
		close(resume)
		require.NoError(t, <-recovery)
		require.ErrorIs(t, <-competingRecovery, context.DeadlineExceeded, "waiting recovery must have a bound")
		assert.Zero(t, detached)
	})
}

func TestDaemonServiceCgroupManager(t *testing.T) {
	oldRead := readServiceProcessFile
	t.Cleanup(func() { readServiceProcessFile = oldRead })
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "missing-bus"))
	for _, path := range []string{
		"/system.slice/cron.service",
		fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/app.slice/fixture.service", os.Getuid()+1, os.Getuid()+1),
		fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/app.slice/fixture.service", os.Getuid(), os.Getuid()),
	} {
		t.Run(path, func(t *testing.T) {
			readServiceProcessFile = func(string) ([]byte, error) { return []byte("0::" + path + "\n"), nil }
			owner, err := lookupDaemonService(context.Background(), 1234)
			assert.Nil(t, owner)
			if strings.Contains(path, fmt.Sprintf("/user@%d.service/", os.Getuid())) {
				require.ErrorIs(t, err, errUserBusUnavailable)
				var managed *managedRecoveryError
				require.ErrorAs(t, err, &managed)
				assert.Equal(t, "fixture.service", managed.unit)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDaemonServiceSamePathReplacement(t *testing.T) {
	path, err := os.Executable()
	require.NoError(t, err)
	if os.Getenv("ROBOREV_TEST_EXECUTABLE_REPLACEMENT") == "1" {
		require.NoError(t, os.WriteFile(path+".next", []byte("replacement executable"), 0o700))
		require.NoError(t, os.Rename(path+".next", path))
		same, err := (&daemonService{executable: path}).sameInstallation()
		require.NoError(t, err)
		assert.True(t, same)
		return
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	copyPath := filepath.Join(t.TempDir(), "roborev-test")
	require.NoError(t, os.WriteFile(copyPath, data, 0o700))
	cmd := exec.Command(copyPath, "-test.run=^TestDaemonServiceSamePathReplacement$")
	cmd.Env = append(os.Environ(), "ROBOREV_TEST_EXECUTABLE_REPLACEMENT=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestUserServiceStalledAuthentication(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "bus"))
	require.NoError(t, err)
	defer listener.Close()
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+listener.Addr().String())
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	oldRead, oldDiscovery, oldBudget := readServiceProcessFile, getAnyRunningDaemonForStart, daemonStartTimeout
	t.Cleanup(func() {
		readServiceProcessFile, getAnyRunningDaemonForStart, daemonStartTimeout = oldRead, oldDiscovery, oldBudget
	})
	readServiceProcessFile = func(string) ([]byte, error) {
		return []byte(fmt.Sprintf("0::/user.slice/user-%d.slice/user@%d.service/app.slice/fixture.service\n", os.Getuid(), os.Getuid())), nil
	}
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	daemonStartTimeout = 30 * time.Millisecond
	// The Unix socket accepts connections but supplies no authentication response.
	err = recoverDaemon(&daemon.RuntimeInfo{PID: 1234}, errors.New("probe failed"))
	require.ErrorIs(t, err, errUserBusUnavailable)
	release, acquired, err := daemon.RuntimeStore().TryAcquireStartLock(t.Context())
	require.NoError(t, err)
	require.True(t, acquired)
	release()
}

func TestUnmanagedRecoveryReusesReplacementAfterDrain(t *testing.T) {
	oldFind, oldDrain := findDaemonService, drainManagedDaemon
	oldDiscovery, oldStart, oldDetached := getAnyRunningDaemonForStart, startDaemonForEnsure, startDaemonDetached
	t.Cleanup(func() {
		findDaemonService, drainManagedDaemon = oldFind, oldDrain
		getAnyRunningDaemonForStart, startDaemonForEnsure, startDaemonDetached = oldDiscovery, oldStart, oldDetached
	})
	current := &daemon.RuntimeInfo{PID: 1234}
	findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return current, nil }
	drainManagedDaemon = func(*daemon.RuntimeInfo) error { return nil }
	startDaemonForEnsure = func() error {
		current = &daemon.RuntimeInfo{PID: 5678, Version: version.Version}
		return startDaemon()
	}
	detached := 0
	startDaemonDetached = func(context.Context, detachedDaemonOptions) error { detached++; return nil }
	require.NoError(t, recoverDaemon(current, nil))
	assert.Zero(t, detached)
	assert.Equal(t, 5678, current.PID)
}

func TestStatusRecoveryLockTimeout(t *testing.T) {
	oldEnsure, oldFind, oldTimeout := statusEnsureDaemon, findDaemonService, daemonStartTimeout
	t.Cleanup(func() { statusEnsureDaemon, findDaemonService, daemonStartTimeout = oldEnsure, oldFind, oldTimeout })
	findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
	// The wait is on an external file lock rather than a Go channel.
	daemonStartTimeout = 20 * time.Millisecond
	release, err := daemon.RuntimeStore().AcquireStartLock(t.Context())
	require.NoError(t, err)
	defer release()
	statusEnsureDaemon = func() error { return recoverDaemon(&daemon.RuntimeInfo{PID: 1234}, nil) }
	command := statusCmd()
	command.SetArgs([]string{"--json"})
	stdout := captureStdout(t, func() {
		err := command.Execute()
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.ErrorContains(t, err, "daemon status unavailable")
	})
	assert.Empty(t, stdout, "a lock timeout cannot establish that the daemon stopped")
}
