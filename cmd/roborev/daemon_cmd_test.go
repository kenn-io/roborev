package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/testutil"
)

func TestReviewRestartWithRetainedForeignRuntime(t *testing.T) {
	if dataDir := os.Getenv("ROBOREV_TEST_RESTART_DAEMON"); dataDir != "" {
		t.Setenv("ROBOREV_DATA_DIR", dataDir)
		cmd := daemonRunCmd()
		cmd.SetArgs([]string{"--addr", "127.0.0.1:0"})
		require.NoError(t, cmd.Execute())
		return
	}

	isolateDaemonSelection(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	executable, err := os.Executable()
	require.NoError(t, err)
	starts := 0
	start := func() error {
		cmd := exec.Command(executable, "-test.run=^TestReviewRestartWithRetainedForeignRuntime$", "daemon", "run")
		cmd.Env = append(os.Environ(), "ROBOREV_TEST_RESTART_DAEMON="+os.Getenv("ROBOREV_DATA_DIR"))
		if err := cmd.Start(); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			<-done
		})
		// Wall-clock wait: the child publishes its runtime and HTTP listener.
		require.Eventually(t, func() bool {
			info, err := daemon.GetAnyRunningDaemon()
			return err == nil && info.PID == cmd.Process.Pid
		}, 30*time.Second, 20*time.Millisecond)
		starts++
		return nil
	}
	require.NoError(t, start())
	original, err := daemon.GetAnyRunningDaemon()
	require.NoError(t, err)
	rec := testutil.UnjudgedRuntimeRecord(t, "tcp", "127.0.0.1:1")
	path, err := daemon.RuntimeStore().Write(rec)
	require.NoError(t, err)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	t.Setenv("ROBOREV_TEST_RUNTIME_WRITER", "")
	originalEnsure := daemonEnsure
	daemonEnsure = start
	t.Cleanup(func() { daemonEnsure = originalEnsure })

	cmd := daemonCmd()
	cmd.SetArgs([]string{"restart"})
	err = cmd.Execute()
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(2, starts)
	replacement, err := daemon.GetAnyRunningDaemon()
	require.NoError(t, err)
	assert.NotEqual(original.PID, replacement.PID)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(before, after)
}

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
	originalRestartStop := daemonRestartStop
	originalDiscover := daemonDiscover
	daemonEnsure = ensure
	daemonStop = stop
	daemonRestartStop = stop
	daemonDiscover = discover
	t.Cleanup(func() {
		daemonEnsure = originalEnsure
		daemonStop = originalStop
		daemonRestartStop = originalRestartStop
		daemonDiscover = originalDiscover
	})
}
