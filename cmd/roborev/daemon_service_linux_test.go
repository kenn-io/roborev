package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		outcome        string
		probeErr       error
		wantError      string
		stops, starts  int
		queueCommand   bool
		paused         bool
		failPause      bool
		failDiscovery  bool
		recheckCurrent bool
	}{
		{outcome: "replacement current"},
		{outcome: "live current after probe failure", probeErr: probeErr, recheckCurrent: true},
		{outcome: "pause write failure", queueCommand: true, paused: true, failPause: true, wantError: "pause queue before shutdown"},
		{outcome: "failed version probe", queueCommand: true, paused: true, probeErr: probeErr, stops: 1, starts: 1},
		{outcome: "failed rediscovery", queueCommand: true, paused: true, failDiscovery: true, stops: 1, starts: 1},
		{outcome: "unpause", queueCommand: true, stops: 1, starts: 1},
		{outcome: "child responsive"},
		{outcome: "child failed", probeErr: probeErr, wantError: "service child requires attention: probe failed; run systemctl --user restart fixture.service"},
		{outcome: "replacement outdated", stops: 1, starts: 1},
		{outcome: "vanished owned", starts: 1},
		{outcome: "failed", probeErr: probeErr, stops: 1, starts: 1},
		{outcome: "foreign responsive"},
		{outcome: "foreign failed", probeErr: probeErr, wantError: "probe failed; run systemctl --user restart fixture.service"},
		{outcome: "remain after exit", wantError: "RemainAfterExit=yes requires manual restart; run systemctl --user restart fixture.service"},
		{outcome: "transient", wantError: "transient units require manual restart; run systemctl --user restart fixture.service"},
		{outcome: "forking", wantError: "unsupported service Type=forking; run systemctl --user restart fixture.service"},
		{outcome: "uncertain"},
		{outcome: "unreadable"},
		{outcome: "uncertain failed", probeErr: probeErr, wantError: "properties unavailable"},
		{outcome: "start failure", wantError: "start rejected", stops: 1, starts: 1},
		{outcome: "drain failure", wantError: "drain rejected", stops: 1},
		{outcome: "wrong PID", wantError: "replacement is outside service", stops: 1, starts: 1},
		{outcome: "readiness failure", wantError: "replacement readiness", stops: 1, starts: 1},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			stubServiceRecovery(t)
			var progress bytes.Buffer
			lifecycleOut, verbose = &progress, false
			original := &daemon.RuntimeInfo{PID: 1234, Version: version.Version, Network: "tcp", Address: "127.0.0.1:1"}
			var events []string
			if tc.queueCommand {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					events = append(events, r.URL.Path)
					if tc.failPause {
						http.Error(w, "queue unavailable", http.StatusInternalServerError)
						return
					}
					_, _ = w.Write([]byte(`{"queue_paused":true}`))
				}))
				t.Cleanup(server.Close)
				original.Address = strings.TrimPrefix(server.URL, "http://")
				original.Version = "v-old"
				pendingStartPause = &tc.paused
			}
			oldProbe := probeDaemonForEnsure
			t.Cleanup(func() { probeDaemonForEnsure = oldProbe })
			probeDaemonForEnsure = func(ep daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
				assert.Equal(t, original.Endpoint(), ep)
				liveVersion := "v-old"
				if tc.recheckCurrent {
					liveVersion = version.Version
				}
				return &daemon.PingInfo{Version: liveVersion}, nil
			}
			current := original
			if strings.HasPrefix(tc.outcome, "replacement") {
				current = &daemon.RuntimeInfo{PID: 5678, Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}
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
			owner := func(pid int) *daemonService {
				service := &daemonService{unit: "fixture.service", executable: executable, mainPID: pid, serviceType: "notify"}
				switch tc.outcome {
				case "remain after exit":
					service.remainAfterExit = true
				case "transient":
					service.transient = true
				case "forking":
					service.serviceType = "forking"
				}
				if strings.HasPrefix(tc.outcome, "child") {
					service.mainPID = 9999
				}
				return service
			}
			started := false
			lookups := 0
			findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
				lookups++
				if lookups == 1 {
					assert.Equal(t, original.PID, pid)
					return owner(pid), nil
				}
				assert.Equal(t, current.PID, pid, "ownership must use the daemon discovered under the lock")
				switch {
				case tc.outcome == "vanished owned":
					return nil, os.ErrNotExist
				case strings.HasPrefix(tc.outcome, "uncertain"):
					return nil, &managedRecoveryError{"fixture.service", errors.New("properties unavailable")}
				}
				return owner(pid), nil
			}
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
				if !started && (tc.probeErr != nil && !tc.queueCommand && !tc.recheckCurrent || tc.failDiscovery || tc.outcome == "vanished owned") || started && tc.outcome == "readiness failure" {
					return nil, os.ErrNotExist
				}
				return current, nil
			}
			serviceMainPID = func(_ context.Context, unit string) (int, error) {
				assert.Equal(t, "fixture.service", unit)
				if tc.outcome == "wrong PID" {
					return 9999, nil
				}
				return current.PID, nil
			}
			stops, starts, detached := 0, 0, 0
			startDaemonForEnsure = func() error { detached++; return nil }
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error {
				stops++
				if tc.queueCommand {
					events = append(events, "drain")
				}
				assert.Equal(t, selectedPID, info.PID)
				if tc.outcome == "drain failure" {
					return errors.New("drain rejected")
				}
				return nil
			}
			startDaemonService = func(_ context.Context, unit string) error {
				assert.Equal(t, "fixture.service", unit)
				starts++
				started = true
				current = &daemon.RuntimeInfo{PID: 9012, Version: version.Version, Network: "tcp", Address: "127.0.0.1:1"}
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
			if tc.queueCommand {
				var want []string
				if tc.paused {
					want = append(want, "/api/queue/pause")
				}
				if tc.stops != 0 {
					want = append(want, "drain")
				}
				assert.Equal(t, want, events)
			}
			assert.Equal(t, tc.stops, stops)
			assert.Equal(t, tc.starts, starts)
			if reason := map[string]string{
				"uncertain":          "properties unavailable",
				"unreadable":         "cannot confirm executable",
				"foreign responsive": "uses another installation",
				"child responsive":   "runs the daemon as a child process",
			}[tc.outcome]; reason != "" {
				assert.Contains(t, progress.String(), "Daemon recovery skipped;")
				assert.Contains(t, progress.String(), reason)
			}
			assert.Zero(t, detached)
			release, acquired, err := daemon.RuntimeStore().TryAcquireStartLock(t.Context())
			require.NoError(t, err)
			require.True(t, acquired, "recovery must release the start lock on every outcome")
			release()
		})
	}
}

func TestRecoverDaemonWaitsForVanishedOwner(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		replacementAppears, managerUnavailable bool
	}{
		{name: "replacement appears", replacementAppears: true},
		{name: "replacement absent"},
		{name: "manager unavailable", managerUnavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				stubServiceRecovery(t)
				oldRun := runSystemctl
				t.Cleanup(func() { runSystemctl = oldRun })
				runSystemctl = func(_ context.Context, args ...string) ([]byte, error) {
					assert.Equal(t, []string{"show", "--property=Version"}, args)
					if tc.managerUnavailable {
						return nil, errors.New("user manager unavailable")
					}
					return nil, nil
				}
				original := &daemon.RuntimeInfo{PID: 1234, Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}
				started := time.Now()
				findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
					if pid == original.PID {
						return nil, &managedRecoveryError{"unknown", os.ErrNotExist}
					}
					assert.Equal(t, 5678, pid)
					return &daemonService{unit: "fixture.service", mainPID: pid, executable: "/proc/self/exe", serviceType: "notify"}, nil
				}
				getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
					// Model the user manager's RestartSec gap after the old PID exits.
					if !tc.replacementAppears || time.Since(started) < time.Second {
						return nil, os.ErrNotExist
					}
					return &daemon.RuntimeInfo{PID: 5678, Version: version.Version, Network: "tcp", Address: "127.0.0.1:1"}, nil
				}
				detached, starts, drains := 0, 0, 0
				startDaemonForEnsure = func() error { detached++; return nil }
				startDaemonService = func(context.Context, string) error { starts++; return nil }
				drainManagedDaemon = func(*daemon.RuntimeInfo) error { drains++; return nil }
				err := recoverDaemon(original, os.ErrNotExist)
				if tc.managerUnavailable {
					require.NoError(t, err)
					assert.Equal(t, 1, detached, "request ordinary startup without a user manager")
					assert.Less(t, time.Since(started), daemonStartTimeout)
				} else if tc.replacementAppears {
					require.NoError(t, err)
					assert.GreaterOrEqual(t, time.Since(started), time.Second)
				} else {
					require.ErrorContains(t, err, "ownership uncertain")
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
				if !tc.managerUnavailable {
					assert.Zero(t, detached)
				}
				assert.Zero(t, starts)
				assert.Zero(t, drains)
			})
		})
	}
}

func TestManagedRecoveryBlocksCompetingColdStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stubServiceRecovery(t)
		var progress bytes.Buffer
		lifecycleOut = &progress
		old := &daemon.RuntimeInfo{PID: 1234, Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}
		var current atomic.Pointer[daemon.RuntimeInfo]
		current.Store(old)
		drained, resume := make(chan struct{}), make(chan struct{})
		finishDrain := make(chan struct{})
		findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
			return &daemonService{unit: "fixture.service", executable: "/proc/self/exe", mainPID: pid, serviceType: "notify"}, nil
		}
		getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
			if current.Load() == nil {
				return nil, os.ErrNotExist
			}
			return current.Load(), nil
		}
		serviceMainPID = func(context.Context, string) (int, error) { return current.Load().PID, nil }
		drainManagedDaemon = func(*daemon.RuntimeInfo) error {
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
		competingRecovery := make(chan error, 1)
		go func() { recovery <- recoverDaemon(old, nil) }()
		<-drained
		synctest.Wait()
		assert.Contains(t, progress.String(), "Waiting for daemon shutdown; no new reviews will start, and any running reviews will finish first...\n")
		assert.Empty(t, recovery, "recovery must remain blocked while reviews drain")
		go func() { coldStart <- startDaemon() }()
		progress.Reset()
		go func() { competingRecovery <- recoverDaemon(old, nil) }()
		synctest.Wait()
		assert.Contains(t, progress.String(), "Waiting for daemon recovery lock; another command may be waiting for running reviews to finish...\n")
		assert.Empty(t, coldStart, "cold start must wait while the unit replaces its daemon")
		assert.Zero(t, detached)
		time.Sleep(daemonStartTimeout + time.Minute)
		require.ErrorIs(t, <-coldStart, context.DeadlineExceeded)
		competingErr := <-competingRecovery
		require.ErrorIs(t, competingErr, context.DeadlineExceeded)
		assert.Empty(t, recovery, "the owning recovery must keep waiting for the active review")
		close(finishDrain)
		close(resume)
		require.NoError(t, <-recovery)
		require.ErrorIs(t, competingErr, errRecoveryLock, "status must distinguish recovery still in progress from a missing daemon")
		assert.Zero(t, detached)
	})
}

// stubServiceProcesses serves /proc files and systemctl output for lookups.
func stubServiceProcesses(t *testing.T, cgroup string, parents map[int]int, show string, showErr error) *[][]string {
	t.Helper()
	oldRead, oldRun := readServiceProcessFile, runSystemctl
	t.Cleanup(func() { readServiceProcessFile, runSystemctl = oldRead, oldRun })
	readServiceProcessFile = func(path string) ([]byte, error) {
		var pid int
		if _, err := fmt.Sscanf(path, "/proc/%d/cgroup", &pid); err == nil {
			return []byte("0::" + cgroup + "\n"), nil
		}
		_, err := fmt.Sscanf(path, "/proc/%d/stat", &pid)
		require.NoError(t, err, path)
		return fmt.Appendf(nil, "%d (roborev daemon) S %d 1 1", pid, parents[pid]), nil
	}
	var calls [][]string
	runSystemctl = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte(show), showErr
	}
	return &calls
}

func TestLookupDaemonService(t *testing.T) {
	userCgroup := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/app.slice/fixture.service", os.Getuid(), os.Getuid())
	t.Run("cgroup read failure", func(t *testing.T) {
		stubServiceProcesses(t, userCgroup, nil, "", nil)
		readServiceProcessFile = func(path string) ([]byte, error) {
			assert.Equal(t, "/proc/1234/cgroup", path)
			return nil, os.ErrNotExist
		}
		owner, err := lookupDaemonService(t.Context(), 1234)
		assert.Nil(t, owner)
		require.ErrorIs(t, err, os.ErrNotExist)
		var managed *managedRecoveryError
		require.ErrorAs(t, err, &managed)
		assert.Equal(t, "unknown", managed.unit)
	})
	for _, cgroup := range []string{
		"/system.slice/cron.service",
		fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/app.slice/fixture.service", os.Getuid()+1, os.Getuid()+1),
	} {
		t.Run(cgroup, func(t *testing.T) {
			calls := stubServiceProcesses(t, cgroup, nil, "", nil)
			owner, err := lookupDaemonService(t.Context(), 1234)
			require.NoError(t, err)
			assert.Nil(t, owner)
			assert.Empty(t, *calls)
		})
	}
	t.Run("manager unavailable", func(t *testing.T) {
		stubServiceProcesses(t, userCgroup, nil, "", errors.New("connection refused"))
		_, err := lookupDaemonService(t.Context(), 1234)
		require.ErrorContains(t, err, "connection refused")
		var managed *managedRecoveryError
		require.ErrorAs(t, err, &managed)
		assert.Equal(t, "fixture.service", managed.unit)
	})
	t.Run("direct", func(t *testing.T) {
		assert := assert.New(t)
		calls := stubServiceProcesses(t, userCgroup, nil, "MainPID=1234\nType=notify\nRemainAfterExit=yes\nTransient=no\nExecStart={ path=/opt/roborev current/bin/roborev ; argv[]=/opt/roborev current/bin/roborev daemon run ; ignore_errors=no ; }\n", nil)
		owner, err := lookupDaemonService(t.Context(), 1234)
		require.NoError(t, err)
		require.NotNil(t, owner)
		assert.Equal([][]string{{"show", "--property=MainPID,Type,RemainAfterExit,Transient,ExecStart", "--", "fixture.service"}}, *calls)
		assert.Equal(&daemonService{unit: "fixture.service", executable: "/opt/roborev current/bin/roborev", mainPID: 1234, serviceType: "notify", remainAfterExit: true}, owner)
	})
	t.Run("wrapper child", func(t *testing.T) {
		stubServiceProcesses(t, userCgroup, map[int]int{1234: 1200, 1200: 1100}, "MainPID=1100\nType=exec\nRemainAfterExit=no\nTransient=no\nExecStart={ path=/opt/roborev current/bin/roborev ; argv[]=/opt/roborev current/bin/roborev daemon run ; ignore_errors=no ; }\n", nil)
		owner, err := lookupDaemonService(t.Context(), 1234)
		require.NoError(t, err)
		require.NotNil(t, owner)
		assert.Equal(t, 1100, owner.mainPID)
		require.NoError(t, owner.supported())
	})
	t.Run("unrelated process in service cgroup", func(t *testing.T) {
		stubServiceProcesses(t, userCgroup, map[int]int{1234: 900, 900: 1}, "MainPID=1100\nType=exec\nRemainAfterExit=no\nTransient=no\nExecStart={ path=/opt/roborev current/bin/roborev ; argv[]=/opt/roborev current/bin/roborev daemon run ; ignore_errors=no ; }\n", nil)
		owner, err := lookupDaemonService(t.Context(), 1234)
		require.NoError(t, err)
		assert.Nil(t, owner)
	})
}

func TestUserServiceCommandsNeverRestart(t *testing.T) {
	calls := stubServiceProcesses(t, "", nil, "MainPID=5678\n", nil)
	require.NoError(t, startUserService(t.Context(), "fixture.service"))
	mainPID, err := userServiceMainPID(t.Context(), "fixture.service")
	require.NoError(t, err)
	assert.Equal(t, 5678, mainPID)
	assert.Equal(t, [][]string{{"start", "--", "fixture.service"}, {"show", "--property=MainPID", "--", "fixture.service"}}, *calls)
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
