package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
		for _, installation := range []string{"shared", "foreign", "unmanaged", "uncertain", "unreadable"} {
			t.Run(trigger+"/"+installation, func(t *testing.T) {
				t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
				var warning bytes.Buffer
				oldFind, oldRun, oldDrain := findDaemonService, runDaemonService, drainManagedDaemon
				oldDiscovery, oldStartDiscovery := getAnyRunningDaemon, getAnyRunningDaemonForStart
				oldRestart, oldOut := restartDaemonForEnsure, lifecycleOut
				t.Cleanup(func() {
					findDaemonService, runDaemonService, drainManagedDaemon = oldFind, oldRun, oldDrain
					getAnyRunningDaemon, getAnyRunningDaemonForStart = oldDiscovery, oldStartDiscovery
					restartDaemonForEnsure, lifecycleOut = oldRestart, oldOut
				})
				lifecycleOut = &warning
				calls := []string{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "/api/ping", r.URL.Path)
					switch trigger {
					case "failed":
						http.Error(w, "busy", http.StatusServiceUnavailable)
					case "unknown":
						fmt.Fprint(w, `{"ok":true,"service":"roborev","version":""}`)
					default:
						fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"v-old"}`)
					}
				}))
				defer server.Close()
				replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"v-advanced"}`)
				}))
				defer replacement.Close()
				info := &daemon.RuntimeInfo{PID: 1234, Address: strings.TrimPrefix(server.URL, "http://")}
				getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return info, nil }
				getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
					if trigger == "failed" && info.PID == 1234 {
						return nil, os.ErrNotExist
					}
					return info, nil
				}
				selected := filepath.Join(t.TempDir(), "roborev")
				if installation == "shared" {
					require.NoError(t, os.Symlink("/proc/self/exe", selected))
				} else if installation != "unreadable" {
					require.NoError(t, os.WriteFile(selected, []byte("different executable with the same name"), 0o700))
				}
				findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
					assert.Equal(t, info.PID, pid)
					if installation == "unmanaged" {
						return nil, nil
					}
					if installation == "uncertain" {
						return nil, &managedRecoveryError{"fixture.service", errors.New("manager unavailable")}
					}
					return &daemonService{"fixture.service", selected}, nil
				}
				restartDaemonForEnsure = func() error { calls = append(calls, "detached"); return nil }
				assertLocked := func() {
					release, acquired, err := daemon.RuntimeStore().TryAcquireStartLock(context.Background())
					require.NoError(t, err)
					if acquired {
						release()
					}
					assert.False(t, acquired, "cold starts must wait through drain and readiness")
				}
				drainManagedDaemon = func(got *daemon.RuntimeInfo) error {
					assert.Equal(t, 1234, got.PID)
					assertLocked()
					calls = append(calls, "drain")
					return nil
				}
				runDaemonService = func(_ context.Context, unit string) error {
					assert.Equal(t, "fixture.service", unit)
					assertLocked()
					calls = append(calls, "unit start")
					// The service may follow a newer selection while reviews drain.
					info = &daemon.RuntimeInfo{PID: 5678, Address: strings.TrimPrefix(replacement.URL, "http://")}
					return nil
				}
				oldAttempts := ensureProbeAttempts
				ensureProbeAttempts = 1
				t.Cleanup(func() { ensureProbeAttempts = oldAttempts })
				err := ensureDaemon()
				switch installation {
				case "shared":
					require.NoError(t, err)
					assert.Equal(t, []string{"drain", "unit start"}, calls)
				case "unmanaged":
					require.NoError(t, err)
					assert.Equal(t, []string{"detached"}, calls)
				case "foreign":
					if trigger == "failed" {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
						assert.Contains(t, warning.String(), "fixture.service")
						assert.Contains(t, warning.String(), selected)
						assert.Contains(t, warning.String(), os.Args[0])
					}
					assert.Empty(t, calls)
				default:
					require.Error(t, err)
					assert.Empty(t, calls)
				}
				release, acquired, lockErr := daemon.RuntimeStore().TryAcquireStartLock(context.Background())
				require.NoError(t, lockErr)
				require.True(t, acquired, "recovery must release the start lock")
				release()
			})
		}
	}
}

func TestManagedRecoveryFailuresAndReplacement(t *testing.T) {
	for _, outcome := range []string{"start failure", "wrong PID", "timeout", "already replaced"} {
		t.Run(outcome, func(t *testing.T) {
			oldFind, oldRun, oldDrain := findDaemonService, runDaemonService, drainManagedDaemon
			oldDiscovery, oldTimeout := getAnyRunningDaemonForStart, daemonStartTimeout
			t.Cleanup(func() {
				findDaemonService, runDaemonService, drainManagedDaemon = oldFind, oldRun, oldDrain
				getAnyRunningDaemonForStart, daemonStartTimeout = oldDiscovery, oldTimeout
			})
			daemonStartTimeout = 100 * time.Millisecond
			old := &daemon.RuntimeInfo{PID: 1234}
			current := old
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"v-other"}`)
			}))
			defer server.Close()
			if outcome == "already replaced" {
				current = &daemon.RuntimeInfo{PID: 5678, Address: strings.TrimPrefix(server.URL, "http://")}
			}
			started := false
			findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
				if started && outcome == "wrong PID" {
					return nil, &managedRecoveryError{"fixture.service", errors.New("MainPID mismatch")}
				}
				return &daemonService{"fixture.service", "/proc/self/exe"}, nil
			}
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
				if started && outcome == "timeout" {
					return nil, os.ErrNotExist
				}
				return current, nil
			}
			stops, starts := 0, 0
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error { stops++; assert.Same(t, old, info); return nil }
			runDaemonService = func(_ context.Context, unit string) error {
				starts++
				started = true
				if outcome == "start failure" {
					return errors.New("start rejected")
				}
				return nil
			}
			err := recoverDaemon(old, nil)
			if outcome == "already replaced" {
				require.NoError(t, err)
				assert.Zero(t, stops)
				assert.Zero(t, starts)
			} else {
				var managed *managedRecoveryError
				require.ErrorAs(t, err, &managed)
				assert.Equal(t, "fixture.service", managed.unit)
				assert.Equal(t, 1, stops)
				assert.Equal(t, 1, starts)
			}
		})
	}
}

func TestStatusManagedRecoveryFailure(t *testing.T) {
	old := statusEnsureDaemon
	statusEnsureDaemon = func() error { return &managedRecoveryError{"fixture.service", errors.New("start rejected")} }
	t.Cleanup(func() { statusEnsureDaemon = old })
	for _, args := range [][]string{nil, {"--json"}} {
		cmd := statusCmd()
		cmd.SetArgs(args)
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		stdout := captureStdout(t, func() { require.ErrorContains(t, cmd.Execute(), "daemon status unavailable") })
		assert.Empty(t, stdout)
		assert.Contains(t, stderr.String(), "fixture.service")
		assert.NotContains(t, stderr.String(), "daemon start")
	}
}

func TestManagedRecoveryBlocksCompetingColdStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldFind, oldRun, oldDrain := findDaemonService, runDaemonService, drainManagedDaemon
		oldDiscovery, oldDetached := getAnyRunningDaemonForStart, startDaemonDetached
		t.Cleanup(func() {
			findDaemonService, runDaemonService, drainManagedDaemon = oldFind, oldRun, oldDrain
			getAnyRunningDaemonForStart, startDaemonDetached = oldDiscovery, oldDetached
		})
		old := &daemon.RuntimeInfo{PID: 1234}
		var current atomic.Pointer[daemon.RuntimeInfo]
		current.Store(old)
		drained, resume := make(chan struct{}), make(chan struct{})
		findDaemonService = func(context.Context, int) (*daemonService, error) {
			return &daemonService{"fixture.service", "/proc/self/exe"}, nil
		}
		getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
			if current.Load() == nil {
				return nil, os.ErrNotExist
			}
			return current.Load(), nil
		}
		drainManagedDaemon = func(*daemon.RuntimeInfo) error { current.Store(nil); close(drained); return nil }
		runDaemonService = func(context.Context, string) error {
			<-resume
			current.Store(&daemon.RuntimeInfo{PID: 5678})
			return nil
		}
		detached := 0
		startDaemonDetached = func(context.Context, detachedDaemonOptions) error { detached++; return nil }
		recovery, coldStart := make(chan error, 1), make(chan error, 1)
		go func() { recovery <- recoverDaemon(old, nil) }()
		<-drained
		go func() { coldStart <- startDaemon() }()
		synctest.Wait()
		assert.Empty(t, coldStart, "cold start must wait while the unit replaces its daemon")
		assert.Zero(t, detached)
		close(resume)
		require.NoError(t, <-recovery)
		require.NoError(t, <-coldStart)
		assert.Zero(t, detached)
	})
}
