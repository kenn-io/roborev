package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestStopDaemonWithRetryRepeatsPreparationFailures(t *testing.T) {
	attempts := 0
	stop := func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary database failure")
		}
		return nil
	}

	stopDaemonWithRetry(stop, 0)

	assert.Equal(t, 3, attempts)
}

func TestDaemonRunValidatesHookEnabledAutoDesignHeuristics(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmp)

	configPath := filepath.Join(tmp, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(`[auto_design_review]
hook_enabled = true
trigger_paths = ["["]
`), 0o644))

	cmd := daemonRunCmd()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{
		"--db", filepath.Join(tmp, "reviews.db"),
		"--config", configPath,
		"--addr", "127.0.0.1:0",
	})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid [auto_design_review] config")
	assert.Contains(t, err.Error(), "trigger_paths")
}

func TestDaemonRunHidesWebDevelopmentOrigin(t *testing.T) {
	cmd := daemonRunCmd()
	flag := cmd.Flags().Lookup("web-dev-origin")
	require.NotNil(t, flag)
	assert.True(t, flag.Hidden)
}

func TestDaemonStartShowsWebUIURL(t *testing.T) {
	withDaemonCommandDependencies(t,
		func() error { return nil },
		func() error { return nil },
		func() (*daemon.RuntimeInfo, error) {
			return &daemon.RuntimeInfo{WebOrigin: "http://127.0.0.1:7374"}, nil
		},
	)

	output := captureStdout(t, func() {
		cmd := daemonCmd()
		cmd.SetArgs([]string{"start"})
		require.NoError(t, cmd.Execute())
	})

	assert.Equal(t, "Daemon started\nWeb UI: http://127.0.0.1:7374\n", output)
}

func TestDaemonRestartShowsPublicWebUIURL(t *testing.T) {
	stubDaemonRestartOutput(t)
	withDaemonCommandDependencies(t,
		func() error { return nil },
		func() error { return nil },
		func() (*daemon.RuntimeInfo, error) {
			return &daemon.RuntimeInfo{WebOrigin: "https://reviews.example.com"}, nil
		},
	)

	output := captureStdout(t, func() {
		cmd := daemonCmd()
		cmd.SetArgs([]string{"restart"})
		require.NoError(t, cmd.Execute())
	})

	assert.Equal(t, "Daemon restarted\nWeb UI: https://reviews.example.com\n", output)
}

func TestDaemonRestartShowsPrefixedWebUIURL(t *testing.T) {
	stubDaemonRestartOutput(t)
	withDaemonCommandDependencies(t,
		func() error { return nil },
		func() error { return nil },
		func() (*daemon.RuntimeInfo, error) {
			return &daemon.RuntimeInfo{
				WebOrigin:   "https://reviews.example.com",
				WebBasePath: "/roborev-ci",
			}, nil
		},
	)

	output := captureStdout(t, func() {
		cmd := daemonCmd()
		cmd.SetArgs([]string{"restart"})
		require.NoError(t, cmd.Execute())
	})

	assert.Equal(t, "Daemon restarted\nWeb UI: https://reviews.example.com/roborev-ci/\n", output)
}

func TestDaemonRestartShowsUnavailableWhenBrowserIsDisabled(t *testing.T) {
	stubDaemonRestartOutput(t)
	withDaemonCommandDependencies(t,
		func() error { return nil },
		func() error { return nil },
		func() (*daemon.RuntimeInfo, error) {
			return nil, errors.New("browser metadata unavailable")
		},
	)

	output := captureStdout(t, func() {
		cmd := daemonCmd()
		cmd.SetArgs([]string{"restart"})
		require.NoError(t, cmd.Execute())
	})

	assert.Equal(t, "Daemon restarted\nWeb UI: unavailable\n", output)
}

func TestDaemonRestartExplainsWebDisabledReason(t *testing.T) {
	stubDaemonRestartOutput(t)
	cases := []struct {
		reason  string
		display string
	}{
		{
			reason:  daemon.WebDisabledReasonMissingAssets,
			display: "disabled (this build has no embedded web assets; reinstall from an official release)",
		},
		{
			reason:  daemon.WebDisabledReasonConfig,
			display: "disabled ([web] enabled = false)",
		},
	}
	for _, testCase := range cases {
		withDaemonCommandDependencies(t,
			func() error { return nil },
			func() error { return nil },
			func() (*daemon.RuntimeInfo, error) {
				return &daemon.RuntimeInfo{WebDisabledReason: testCase.reason}, nil
			},
		)

		output := captureStdout(t, func() {
			cmd := daemonCmd()
			cmd.SetArgs([]string{"restart"})
			require.NoError(t, cmd.Execute())
		})

		assert.Equal(t, "Daemon restarted\nWeb UI: "+testCase.display+"\n", output)
	}
}

func stubDaemonRestartOutput(t *testing.T) {
	t.Helper()
	stubServiceRecovery(t)
	restartThroughService = true
	oldList := listAllRuntimes
	t.Cleanup(func() { listAllRuntimes = oldList })
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{{PID: os.Getpid()}}, nil
	}
	findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
	drainManagedDaemon = func(*daemon.RuntimeInfo) error { return nil }
}

func withDaemonCommandDependencies(
	t *testing.T,
	ensure func() error,
	stop func() error,
	discover func() (*daemon.RuntimeInfo, error),
) {
	t.Helper()
	originalEnsure := daemonEnsure
	originalStop := daemonStop
	originalDiscover := daemonDiscover
	daemonEnsure = ensure
	daemonStop = stop
	daemonDiscover = discover
	t.Cleanup(func() {
		daemonEnsure = originalEnsure
		daemonStop = originalStop
		daemonDiscover = originalDiscover
	})
}

func TestDaemonRestartThroughLaunchdJob(t *testing.T) {
	for _, tc := range []struct {
		name, wantError                                        string
		unmanaged, vanished, stale, child, foreign, unreadable bool
	}{
		{name: "owned"},
		{name: "wrapper", child: true, wantError: "child process"},
		{name: "foreign", foreign: true, wantError: "another installation"},
		{name: "unreadable", unreadable: true, wantError: "job unreadable"},
		{name: "vanished", vanished: true, wantError: "daemon exited during restart; run it again"},
		{name: "stale", stale: true},
		{name: "unmanaged", unmanaged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubServiceRecovery(t)
			restartThroughService = true
			serviceRestartCommand = "launchctl kickstart -k"
			owner := ownedDaemonService(t)
			if tc.child {
				owner.mainPID++
			}
			if tc.foreign {
				owner.executable = filepath.Join(t.TempDir(), "roborev")
				require.NoError(t, os.WriteFile(owner.executable, nil, 0o600))
			}
			runtimeInfo := &daemon.RuntimeInfo{PID: os.Getpid()}
			if tc.stale {
				runtimeInfo.PID = 1 << 30
			}
			oldList := listAllRuntimes
			t.Cleanup(func() { listAllRuntimes = oldList })
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return []*daemon.RuntimeInfo{runtimeInfo}, nil }
			findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
				assert.Equal(t, os.Getpid(), pid)
				if tc.unreadable {
					return nil, errors.New("job unreadable")
				}
				if tc.vanished {
					return nil, os.ErrNotExist
				}
				if tc.unmanaged {
					return nil, nil
				}
				return owner, nil
			}
			var calls []string
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error {
				assert.Same(t, runtimeInfo, info)
				assertRecoveryLock(t, tc.unmanaged || tc.stale)
				calls = append(calls, "stop")
				return nil
			}
			startDaemonService = func(_ context.Context, unit string) error {
				assert.Equal(t, owner.unit, unit)
				assertRecoveryLock(t, false)
				calls = append(calls, "kickstart")
				return nil
			}
			serviceMainPID = func(_ context.Context, unit string) (int, error) {
				assert.Equal(t, owner.unit, unit)
				return 200, nil
			}
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return &daemon.RuntimeInfo{PID: 200}, nil }
			withDaemonCommandDependencies(t,
				func() error { calls = append(calls, "ensure"); return nil },
				func() error {
					assertRecoveryLock(t, tc.unmanaged || tc.stale)
					calls = append(calls, "stop")
					return nil
				},
				func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist },
			)
			var commandErr error
			output := captureStdout(t, func() {
				cmd := daemonCmd()
				cmd.SetArgs([]string{"restart"})
				commandErr = cmd.ExecuteContext(t.Context())
			})
			assertRecoveryLock(t, true)
			if tc.wantError != "" {
				require.ErrorContains(t, commandErr, tc.wantError)
				assert.Empty(t, calls)
				if tc.child || tc.foreign {
					assert.Equal(t, 1, strings.Count(commandErr.Error(), "run launchctl kickstart -k "+owner.unit))
				}
				return
			}
			require.NoError(t, commandErr)
			assert.Contains(t, output, "Daemon restarted")
			if tc.unmanaged || tc.stale {
				assert.Equal(t, []string{"stop", "ensure"}, calls)
			} else {
				assert.Equal(t, []string{"stop", "kickstart"}, calls)
			}
		})
	}
}

func TestDaemonRestartChecksEveryRuntime(t *testing.T) {
	for _, reason := range []string{"child process", "another installation", "job unreadable"} {
		t.Run(reason, func(t *testing.T) {
			stubServiceRecovery(t)
			restartThroughService = true
			owner := ownedDaemonService(t)
			second := *owner
			second.mainPID = os.Getppid()
			switch reason {
			case "child process":
				second.mainPID = os.Getpid()
			case "another installation":
				second.executable = filepath.Join(t.TempDir(), "roborev")
				require.NoError(t, os.WriteFile(second.executable, nil, 0o600))
			}
			oldList := listAllRuntimes
			t.Cleanup(func() { listAllRuntimes = oldList })
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
				return []*daemon.RuntimeInfo{{PID: os.Getpid()}, {PID: os.Getppid()}}, nil
			}
			findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
				if pid == os.Getpid() {
					return owner, nil
				}
				assert.Equal(t, os.Getppid(), pid)
				if reason == "job unreadable" {
					return nil, errors.New(reason)
				}
				return &second, nil
			}
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return &daemon.RuntimeInfo{PID: 200}, nil }
			serviceMainPID = func(context.Context, string) (int, error) { return 200, nil }
			stops, starts := 0, 0
			drainManagedDaemon = func(*daemon.RuntimeInfo) error { stops++; return nil }
			startDaemonService = func(context.Context, string) error { starts++; return nil }
			withDaemonCommandDependencies(t,
				func() error { starts++; return nil },
				func() error { stops++; return nil },
				func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist },
			)
			cmd := daemonCmd()
			cmd.SetArgs([]string{"restart"})
			err := cmd.ExecuteContext(t.Context())
			require.ErrorContains(t, err, reason)
			assert.Zero(t, stops)
			assert.Zero(t, starts)
		})
	}
}

func TestDaemonRestartOnlyStopsValidatedRuntimes(t *testing.T) {
	stubServiceRecovery(t)
	restartThroughService = true
	owner := ownedDaemonService(t)
	validated := &daemon.RuntimeInfo{PID: os.Getpid()}
	late := &daemon.RuntimeInfo{PID: os.Getppid()}
	oldList := listAllRuntimes
	t.Cleanup(func() { listAllRuntimes = oldList })
	listCalls := 0
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		listCalls++
		if listCalls == 1 {
			return []*daemon.RuntimeInfo{validated}, nil
		}
		return []*daemon.RuntimeInfo{late}, nil
	}
	findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
		assert.Equal(t, validated.PID, pid)
		return owner, nil
	}
	var stopped []*daemon.RuntimeInfo
	drainManagedDaemon = func(info *daemon.RuntimeInfo) error {
		stopped = append(stopped, info)
		return nil
	}
	startDaemonService = func(context.Context, string) error { return nil }
	serviceMainPID = func(context.Context, string) (int, error) { return 200, nil }
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return &daemon.RuntimeInfo{PID: 200}, nil }
	withDaemonCommandDependencies(t,
		func() error { return errors.New("unexpected detached start") },
		func() error { return stopDaemonRuntimes(listAllRuntimes, drainManagedDaemon, os.Stderr) },
		func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist },
	)
	cmd := daemonCmd()
	cmd.SetArgs([]string{"restart"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.Equal(t, []*daemon.RuntimeInfo{validated}, stopped)
	assert.Equal(t, 1, listCalls)
	assertRecoveryLock(t, true)
}

func TestDaemonRestartMultipleRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		firstOwned, secondOwned bool
	}{
		{name: "two owners", firstOwned: true, secondOwned: true},
		{name: "owned then unmanaged", firstOwned: true},
		{name: "unmanaged then owned", secondOwned: true},
		{name: "two unmanaged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubServiceRecovery(t)
			restartThroughService = true
			first := ownedDaemonService(t)
			second := ownedDaemonService(t)
			second.unit = "gui/501/com.example.second"
			second.mainPID = os.Getppid()
			runtimes := []*daemon.RuntimeInfo{{PID: first.mainPID}, {PID: second.mainPID}}
			oldList := listAllRuntimes
			t.Cleanup(func() { listAllRuntimes = oldList })
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return runtimes, nil }
			findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
				if pid == first.mainPID {
					if tc.firstOwned {
						return first, nil
					}
					return nil, nil
				}
				assert.Equal(t, second.mainPID, pid)
				if tc.secondOwned {
					return second, nil
				}
				return nil, nil
			}
			var stopped []*daemon.RuntimeInfo
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error {
				stopped = append(stopped, info)
				return nil
			}
			var calls []string
			startDaemonService = func(context.Context, string) error { calls = append(calls, "job start"); return nil }
			withDaemonCommandDependencies(t,
				func() error {
					assert.Equal(t, runtimes, stopped)
					calls = append(calls, "ensure")
					return nil
				},
				func() error { calls = append(calls, "stop"); return nil },
				func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist },
			)
			cmd := daemonCmd()
			cmd.SetArgs([]string{"restart"})
			err := cmd.ExecuteContext(t.Context())
			if tc.firstOwned || tc.secondOwned {
				require.ErrorContains(t, err, "2 live daemons")
				assert.ErrorContains(t, err, "stop the others or restart through each job")
				assert.Empty(t, stopped)
				assert.Empty(t, calls)
			} else {
				require.NoError(t, err)
				assert.Equal(t, runtimes, stopped)
				assert.Equal(t, []string{"ensure"}, calls)
			}
			assertRecoveryLock(t, true)
		})
	}
}
