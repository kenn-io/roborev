package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
)

func TestEnsureDaemonServiceRecovery(t *testing.T) {
	for _, trigger := range []string{"mismatch", "unknown", "failed"} {
		t.Run(trigger+"/shared", func(t *testing.T) {
			t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
			oldFind, oldDrain := findDaemonService, drainManagedDaemon
			oldRestart := restartDaemonForEnsure
			manager := &daemonServiceManager{close: func() {}}
			oldProbe, oldStartDiscovery, oldDiscovery := probeDaemonForEnsure, getAnyRunningDaemonForStart, getAnyRunningDaemon
			t.Cleanup(func() {
				findDaemonService, drainManagedDaemon = oldFind, oldDrain
				restartDaemonForEnsure = oldRestart
				probeDaemonForEnsure, getAnyRunningDaemonForStart, getAnyRunningDaemon = oldProbe, oldStartDiscovery, oldDiscovery
			})
			calls := []string{}
			restartDaemonForEnsure = func() error { calls = append(calls, "detached"); return nil }
			probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
				if trigger == "failed" {
					return nil, errors.New("probe failed")
				}
				if trigger == "unknown" {
					return &daemon.PingInfo{}, nil
				}
				return &daemon.PingInfo{Version: "v-old"}, nil
			}
			info := &daemon.RuntimeInfo{PID: 1234}
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return info, nil }
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
				if trigger == "failed" && info.PID == 1234 {
					return nil, os.ErrNotExist
				}
				return info, nil
			}
			manager.lookup = func(_ context.Context, pid int) (*daemonService, error) {
				assert.Equal(t, info.PID, pid)
				return &daemonService{unit: "fixture.service", executable: "/proc/self/exe", mainPID: pid, manager: manager}, nil
			}
			findDaemonService = manager.lookup
			drainManagedDaemon = func(got *daemon.RuntimeInfo) error {
				assert.Equal(t, 1234, got.PID)
				calls = append(calls, "drain")
				return nil
			}
			manager.start = func(_ context.Context, unit string) error {
				assert.Equal(t, "fixture.service", unit)
				calls = append(calls, "unit start")
				info = &daemon.RuntimeInfo{PID: 5678}
				return nil
			}
			require.NoError(t, ensureDaemon())
			assert.Equal(t, []string{"drain", "unit start"}, calls)
		})
	}
}

func TestManagedRecoveryFailuresAndReplacement(t *testing.T) {
	probeErr := errors.New("probe failed")
	for _, tc := range []struct {
		outcome       string
		probeErr      error
		wantError     string
		stops, starts int
	}{
		{outcome: "foreign responsive"},
		{outcome: "foreign failed", probeErr: probeErr, wantError: "probe failed"},
		{outcome: "uncertain", wantError: "properties unavailable"},
		{outcome: "unreadable", wantError: "cannot confirm executable"},
		{outcome: "bus responsive"},
		{outcome: "bus failed", probeErr: probeErr, wantError: "user bus unavailable"},
		{outcome: "start failure", wantError: "start rejected", stops: 1, starts: 1},
		{outcome: "wrong PID", wantError: "replacement is outside service", stops: 1, starts: 1},
		{outcome: "readiness failure", wantError: "replacement readiness", stops: 1, starts: 1},
		{outcome: "already replaced"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			oldFind, oldDrain := findDaemonService, drainManagedDaemon
			manager := &daemonServiceManager{close: func() {}}
			oldDiscovery, oldRestart := getAnyRunningDaemonForStart, restartDaemonForEnsure
			t.Cleanup(func() {
				findDaemonService, drainManagedDaemon = oldFind, oldDrain
				getAnyRunningDaemonForStart, restartDaemonForEnsure = oldDiscovery, oldRestart
			})
			old := &daemon.RuntimeInfo{PID: 1234}
			current := old
			if tc.outcome == "already replaced" {
				current = &daemon.RuntimeInfo{PID: 5678}
			}
			executable := "/proc/self/exe"
			if strings.HasPrefix(tc.outcome, "foreign") {
				executable = filepath.Join(t.TempDir(), "roborev")
				require.NoError(t, os.WriteFile(executable, []byte("another installation"), 0o700))
			} else if tc.outcome == "unreadable" {
				executable = filepath.Join(t.TempDir(), "missing")
			}
			started := false
			lookups := 0
			manager.lookup = func(_ context.Context, pid int) (*daemonService, error) {
				wantPID := current.PID
				if lookups == 0 {
					wantPID = old.PID
				}
				assert.Equal(t, wantPID, pid)
				lookups++
				if strings.HasPrefix(tc.outcome, "bus") {
					return nil, &managedRecoveryError{"fixture.service", errUserBusUnavailable}
				}
				if tc.outcome == "uncertain" {
					return nil, &managedRecoveryError{"fixture.service", errors.New("properties unavailable")}
				}
				if started && tc.outcome == "wrong PID" {
					return &daemonService{unit: "fixture.service", mainPID: 9999}, nil
				}
				return &daemonService{unit: "fixture.service", executable: executable, mainPID: pid, manager: manager}, nil
			}
			findDaemonService = manager.lookup
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
				if started && tc.outcome == "readiness failure" {
					return nil, os.ErrNotExist
				}
				return current, nil
			}
			stops, starts, detached := 0, 0, 0
			restartDaemonForEnsure = func() error { detached++; return nil }
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error { stops++; assert.Same(t, old, info); return nil }
			manager.start = func(_ context.Context, unit string) error {
				assert.Equal(t, "fixture.service", unit)
				starts++
				started = true
				current = &daemon.RuntimeInfo{PID: 5678}
				if tc.outcome == "start failure" {
					return errors.New("start rejected")
				}
				return nil
			}
			var err error
			// Readiness polls until its startup budget expires.
			synctest.Test(t, func(*testing.T) { err = recoverDaemon(old, tc.probeErr) })
			if tc.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantError)
				if tc.outcome == "foreign failed" {
					require.ErrorIs(t, err, probeErr)
				} else {
					var managed *managedRecoveryError
					require.ErrorAs(t, err, &managed)
					assert.Equal(t, "fixture.service", managed.unit)
				}
			}
			assert.Equal(t, tc.stops, stops)
			assert.Equal(t, tc.starts, starts)
			assert.Zero(t, detached)
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
			current.Store(&daemon.RuntimeInfo{PID: 5678})
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
		require.NoError(t, <-competingRecovery, "automatic recovery must wait through the active review drain")
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
