package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	restartThroughService = false
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
		name             string
		unmanaged, stale bool
	}{
		{name: "owned"},
		{name: "stale", stale: true},
		{name: "unmanaged", unmanaged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubServiceRecovery(t)
			restartThroughService = true
			serviceRestartCommand = "launchctl kickstart -k"
			owner := ownedDaemonService(t)
			runtimeInfo := &daemon.RuntimeInfo{PID: os.Getpid()}
			if tc.stale {
				runtimeInfo.PID = 1 << 30
			}
			oldList := listAllRuntimes
			t.Cleanup(func() { listAllRuntimes = oldList })
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return []*daemon.RuntimeInfo{runtimeInfo}, nil }
			findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
				assert.Equal(t, os.Getpid(), pid)
				if tc.unmanaged {
					return nil, nil
				}
				return owner, nil
			}
			var calls []string
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error {
				assert.Same(t, runtimeInfo, info)
				assertRecoveryLock(t, tc.unmanaged || tc.stale)
				calls = append(calls, "drain")
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
			require.NoError(t, commandErr)
			assert.Contains(t, output, "Daemon restarted")
			if tc.unmanaged || tc.stale {
				assert.Equal(t, []string{"drain", "ensure"}, calls)
			} else {
				assert.Equal(t, []string{"drain", "kickstart"}, calls)
			}
		})
	}
}

func TestDaemonRestartChecksEveryRuntime(t *testing.T) {
	secondPID := controlledDaemonPID(t)
	for _, tc := range []struct {
		name, secondKind, wantError string
		single, firstUnmanaged      bool
	}{
		{name: "wrapper", single: true, secondKind: "wrapper", wantError: "child process"},
		{name: "foreign", single: true, secondKind: "foreign", wantError: "another installation"},
		{name: "unreadable", single: true, secondKind: "unreadable", wantError: "job unreadable"},
		{name: "vanished", single: true, secondKind: "vanished", wantError: "daemon exited during restart; run it again"},
		{name: "second wrapper", secondKind: "wrapper", wantError: "child process"},
		{name: "second foreign", secondKind: "foreign", wantError: "another installation"},
		{name: "second unreadable", secondKind: "unreadable", wantError: "job unreadable"},
		{name: "two owners", secondKind: "owned", wantError: "2 live daemons"},
		{name: "owned then unmanaged", secondKind: "unmanaged", wantError: "2 live daemons"},
		{name: "unmanaged then owned", firstUnmanaged: true, secondKind: "owned", wantError: "2 live daemons"},
		{name: "two unmanaged", firstUnmanaged: true, secondKind: "unmanaged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubServiceRecovery(t)
			restartThroughService = true
			serviceRestartCommand = "launchctl kickstart -k"
			first := ownedDaemonService(t)
			second := *first
			second.mainPID = secondPID
			if tc.single {
				second.mainPID = os.Getpid()
			} else {
				second.unit = "gui/501/com.example.second"
			}
			runtimes := []*daemon.RuntimeInfo{{PID: second.mainPID}}
			if !tc.single {
				runtimes = append([]*daemon.RuntimeInfo{{PID: first.mainPID}}, runtimes...)
			}
			switch tc.secondKind {
			case "wrapper":
				if tc.single {
					second.mainPID = secondPID
				} else {
					second.mainPID = first.mainPID
				}
			case "foreign":
				second.executable = filepath.Join(t.TempDir(), "roborev")
				require.NoError(t, os.WriteFile(second.executable, nil, 0o600))
			}
			oldList := listAllRuntimes
			t.Cleanup(func() { listAllRuntimes = oldList })
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return runtimes, nil }
			findDaemonService = func(_ context.Context, pid int) (*daemonService, error) {
				if !tc.single && pid == first.mainPID {
					if tc.firstUnmanaged {
						return nil, nil
					}
					return first, nil
				}
				assert.Equal(t, runtimes[len(runtimes)-1].PID, pid)
				switch tc.secondKind {
				case "unreadable":
					return nil, errors.New("job unreadable")
				case "vanished":
					return nil, os.ErrNotExist
				case "unmanaged":
					return nil, nil
				default:
					return &second, nil
				}
			}
			// Keep accidental restarts observable without waiting for a real daemon.
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return &daemon.RuntimeInfo{PID: 200}, nil }
			serviceMainPID = func(context.Context, string) (int, error) { return 200, nil }
			var stopped []*daemon.RuntimeInfo
			var calls []string
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error {
				stopped = append(stopped, info)
				calls = append(calls, "drain")
				return nil
			}
			startDaemonService = func(context.Context, string) error { calls = append(calls, "job start"); return nil }
			withDaemonCommandDependencies(t,
				func() error {
					assert.Equal(t, runtimes, stopped)
					calls = append(calls, "ensure")
					return nil
				},
				func() error { calls = append(calls, "detached stop"); return nil },
				func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist },
			)
			cmd := daemonCmd()
			cmd.SetArgs([]string{"restart"})
			err := cmd.ExecuteContext(t.Context())
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				assert.Empty(t, stopped)
				assert.Empty(t, calls)
				if tc.secondKind == "wrapper" || tc.secondKind == "foreign" {
					assert.Equal(t, 1, strings.Count(err.Error(), "run launchctl kickstart -k "+second.unit))
				}
				if tc.wantError == "2 live daemons" {
					require.ErrorContains(t, err, "stop the others or restart through each job")
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, runtimes, stopped)
				assert.Equal(t, []string{"drain", "drain", "ensure"}, calls)
			}
			assertRecoveryLock(t, true)
		})
	}
}

// controlledDaemonPID keeps a child alive until cleanup closes its input.
func controlledDaemonPID(t *testing.T) int {
	t.Helper()
	// Git is already required by this package and reads stdin until EOF.
	// Cleanup closes stdin and waits after the test context is canceled.
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "git", "hash-object", "--stdin")
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		assert.NoError(t, input.Close())
		assert.NoError(t, cmd.Wait())
	})
	return cmd.Process.Pid
}

func TestDaemonRestartOnlyStopsValidatedRuntimes(t *testing.T) {
	stubServiceRecovery(t)
	restartThroughService = true
	owner := ownedDaemonService(t)
	validated := &daemon.RuntimeInfo{PID: os.Getpid()}
	late := &daemon.RuntimeInfo{PID: controlledDaemonPID(t)}
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

func TestDaemonRestartRunningPolicy(t *testing.T) {
	t.Run("arguments", func(t *testing.T) {
		stubDaemonRestartOutput(t)
		oldList, oldInterrupt := listAllRuntimes, interruptDaemon
		t.Cleanup(func() { listAllRuntimes, interruptDaemon = oldList, oldInterrupt })
		listCalls, interruptCalls, stopCalls := 0, 0, 0
		listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { listCalls++; return nil, nil }
		interruptDaemon = func(*daemon.RuntimeInfo) error { interruptCalls++; return nil }
		withDaemonCommandDependencies(t, func() error { return nil }, func() error { stopCalls++; return nil }, func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist })
		cmd := daemonCmd()
		cmd.SetArgs([]string{"restart", "--running=invalid"})
		require.Error(t, cmd.ExecuteContext(t.Context()))
		assert.Zero(t, listCalls)
		assert.Zero(t, stopCalls)
		cmd = daemonCmd()
		cmd.SetArgs([]string{"restart", "--running=interrupt", "42"})
		require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "--running=interrupt takes no arguments")
		assert.Zero(t, listCalls)
		assert.Zero(t, interruptCalls)
		assert.Zero(t, stopCalls)
		cmd = daemonCmd()
		cmd.SetArgs([]string{"restart", "extra"})
		require.NoError(t, cmd.ExecuteContext(t.Context()))
		assert.Zero(t, interruptCalls)
	})
	for _, service := range []bool{false, true} {
		t.Run(fmt.Sprintf("service=%t", service), func(t *testing.T) {
			stubDaemonRestartOutput(t)
			restartThroughService = service
			info := &daemon.RuntimeInfo{PID: os.Getpid()}
			oldList, oldInterrupt := listAllRuntimes, interruptDaemon
			t.Cleanup(func() { listAllRuntimes, interruptDaemon = oldList, oldInterrupt })
			listCalls, interruptCalls, stopCalls := 0, 0, 0
			listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { listCalls++; return []*daemon.RuntimeInfo{info}, nil }
			interruptDaemon = func(got *daemon.RuntimeInfo) error { assert.Same(t, info, got); interruptCalls++; return nil }
			if service {
				owner := ownedDaemonService(t)
				findDaemonService = func(context.Context, int) (*daemonService, error) { return owner, nil }
				startDaemonService = func(context.Context, string) error { return nil }
				serviceMainPID = func(context.Context, string) (int, error) { return 200, nil }
				getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return &daemon.RuntimeInfo{PID: 200}, nil }
			}
			withDaemonCommandDependencies(t, func() error { return nil }, func() error { stopCalls++; return nil }, func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist })
			cmd := daemonCmd()
			cmd.SetArgs([]string{"restart", "--running=interrupt"})
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			assert.Equal(t, 1, interruptCalls)
			assert.Zero(t, stopCalls)
		})
	}
}
