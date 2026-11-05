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
		{name: "vanished", vanished: true},
		{name: "stale", stale: true},
		{name: "unmanaged", unmanaged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubServiceRecovery(t)
			restartThroughService = true
			serviceRestartCommand = "launchctl kickstart -k"
			path, err := os.Executable()
			require.NoError(t, err)
			cliExecutableInfo = func() (os.FileInfo, error) { return os.Stat(path) }
			owner := &daemonService{unit: "gui/501/com.example.roborev", mainPID: os.Getpid(), executable: path, serviceType: "exec"}
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
			assertLocked := func() {
				release, acquired, err := daemon.RuntimeStore().TryAcquireStartLock(t.Context())
				require.NoError(t, err)
				if release != nil {
					release()
				}
				assert.False(t, acquired)
			}
			var calls []string
			startDaemonService = func(_ context.Context, unit string) error {
				assert.Equal(t, owner.unit, unit)
				assertLocked()
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
					if !tc.unmanaged && !tc.vanished && !tc.stale {
						assertLocked()
					}
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
			if tc.unmanaged || tc.vanished || tc.stale {
				assert.Equal(t, []string{"stop", "ensure"}, calls)
			} else {
				assert.Equal(t, []string{"stop", "kickstart"}, calls)
			}
		})
	}
}
