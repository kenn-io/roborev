package testenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func fakeAgentsview() int {
	dir := os.Getenv("AGENTSVIEW_DATA_DIR")
	mode := os.Getenv("TEST_CASE")
	store := daemon.RuntimeStore{Dir: dir}
	if mode == "runtime subdir" {
		store.Dir = filepath.Join(dir, "run")
	}
	if len(os.Args) > 1 && os.Args[1] == "session" {
		if os.MkdirAll(dir, 0o755) != nil {
			return 1
		}
		if mode == "database only" {
			return 0
		}
		if _, err := store.Write(daemon.RuntimeRecord{PID: os.Getppid(), Address: "127.0.0.1:1"}); err != nil {
			return 1
		}
		return 0
	}
	if len(os.Args) != 3 || os.Args[1] != "serve" || os.Args[2] != "stop" {
		return 1
	}
	_, statErr := os.Stat(dir)
	if os.WriteFile(os.Getenv("TEST_EVENTS"), fmt.Appendf(nil, "%s|%t\n", dir, statErr == nil), 0o600) != nil || mode == "stop fails" {
		return 1
	}
	if mode == "stop skips" {
		return 0
	}
	path, err := store.Path(os.Getppid())
	if err != nil || os.Remove(path) != nil {
		return 1
	}
	return 0
}

// restoreProcessState undoes the process changes runIsolatedMain makes, as a real TestMain's exit would.
func restoreProcessState(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "GOENV", "GOPATH", "GOCACHE", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_STATE_DIR", "MISE_CACHE_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME", "GIT_TERMINAL_PROMPT", "AGENTSVIEW_DATA_DIR"} {
		t.Setenv(key, os.Getenv(key))
	}
	oldUserEnv, oldAgentEnv := userEnv, userAgentEnv
	t.Cleanup(func() { userEnv, userAgentEnv = oldUserEnv, oldAgentEnv })
}

func fakeAgentsviewBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(exe)
	require.NoError(t, err)
	name := "agentsview"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(binary, data, 0o755))
	return binary
}

func TestRunIsolatedMainStopsAgentsView(t *testing.T) {
	for _, tc := range []struct {
		name, mode      string
		suiteCode, want int
	}{
		{"stop", "stop", 0, 0},
		{"stop fails", "stop fails", 0, 1},
		{"stop fails in failing suite", "stop fails", 7, 7},
		{"stop skips", "stop skips", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreProcessState(t)
			dir := t.TempDir()
			bin := filepath.Dir(fakeAgentsviewBinary(t))
			events := filepath.Join(dir, "events")
			other := filepath.Join(dir, "other")
			t.Setenv("PATH", bin)
			t.Setenv("TEST_EVENTS", events)
			t.Setenv("TEST_CASE", tc.mode)
			t.Setenv("AGENTSVIEW_DATA_DIR", other)
			stderrPath := filepath.Join(dir, "stderr")
			stderr, err := os.Create(stderrPath)
			require.NoError(t, err)
			oldStderr := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() { os.Stderr = oldStderr; _ = stderr.Close() })
			var home string
			run := func() int {
				home = os.Getenv("HOME")
				cmd := exec.CommandContext(t.Context(), "agentsview", "session", "usage-batch", "x", "--format", "json", "--no-sync")
				assert.NoError(t, cmd.Run())
				assert.NoError(t, os.Setenv("PATH", dir))
				assert.NoError(t, os.Setenv("AGENTSVIEW_DATA_DIR", other))
				return tc.suiteCode
			}
			code := runIsolatedMain(run)
			output, err := os.ReadFile(stderrPath)
			require.NoError(t, err)
			assert.Equal(t, tc.want, code)
			if tc.want != 0 {
				t.Cleanup(func() { _ = os.RemoveAll(home) })
				assert.Contains(t, string(output), "failed to stop agentsview")
				assert.Contains(t, string(output), home)
				assert.DirExists(t, home)
			} else {
				assert.NoDirExists(t, home)
			}
			data, err := os.ReadFile(events)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(home, ".agentsview")+"|true\n", string(data))
		})
	}
	t.Run("real agentsview", func(t *testing.T) {
		binary, err := exec.LookPath("agentsview")
		if err != nil {
			t.Skip("agentsview is not installed")
		}
		for _, args := range [][]string{{"session", "usage-batch", "--help"}, {"serve", "stop", "--help"}} {
			output, err := exec.CommandContext(t.Context(), binary, args...).Output()
			if err != nil || (args[0] == "session" && (!strings.Contains(string(output), "--no-sync") || !strings.Contains(string(output), "--format"))) {
				t.Skip("agentsview does not support batch lookup and serve stop")
			}
		}
		restoreProcessState(t)
		lookup := func(dir string) daemon.RuntimeRecord {
			// The lookup starts a detached server, so stop it even when an assertion fails.
			t.Cleanup(func() {
				output, err := stopAgentsview(context.WithoutCancel(t.Context()), binary, dir)
				assert.NoError(t, err, "%s", output)
			})
			cmd := exec.CommandContext(t.Context(), binary, "session", "usage-batch", "missing-session", "--format", "json", "--no-sync")
			cmd.Env = append(os.Environ(), "AGENTSVIEW_DATA_DIR="+dir)
			require.NoError(t, cmd.Run())
			records, err := agentsviewRecords(t.Context(), dir)
			require.NoError(t, err)
			require.NotEmpty(t, records)
			require.True(t, daemon.RuntimeProcessAlive(records[0]))
			return records[0]
		}
		other := lookup(filepath.Join(t.TempDir(), "other"))
		var isolated daemon.RuntimeRecord
		code := runIsolatedMain(func() int {
			isolated = lookup(os.Getenv("AGENTSVIEW_DATA_DIR"))
			return 0
		})
		assert.Zero(t, code)
		assert.False(t, daemon.RuntimeProcessAlive(isolated))
		assert.True(t, daemon.RuntimeProcessAlive(other))
	})
}

func TestStopAgentsview(t *testing.T) {
	for _, mode := range []string{"database only", "runtime subdir", "startup", "startup canceled"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			binary := fakeAgentsviewBinary(t)
			events := filepath.Join(t.TempDir(), "events")
			t.Setenv("TEST_EVENTS", events)
			t.Setenv("TEST_CASE", mode)
			store := daemon.RuntimeStore{Dir: dir}
			if mode == "runtime subdir" {
				store.Dir = filepath.Join(dir, "run")
			}
			ctx := t.Context()
			if mode == "startup" || mode == "startup canceled" {
				release, err := store.AcquireStartLock(ctx)
				require.NoError(t, err)
				if mode == "startup canceled" {
					defer release()
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
					defer cancel()
				} else {
					done := make(chan error, 1)
					go func() {
						time.Sleep(100 * time.Millisecond)
						_, err := store.Write(daemon.RuntimeRecord{PID: os.Getpid(), Address: "127.0.0.1:1"})
						release()
						done <- err
					}()
					defer func() { assert.NoError(t, <-done) }()
				}
			} else if mode != "database only" {
				_, err := store.Write(daemon.RuntimeRecord{PID: os.Getpid(), Address: "127.0.0.1:1"})
				require.NoError(t, err)
			}
			_, err := stopAgentsview(ctx, binary, dir)
			if mode == "startup canceled" {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.NoError(t, err)
			}
			if mode == "database only" || mode == "startup canceled" {
				assert.NoFileExists(t, events)
			} else {
				data, err := os.ReadFile(events)
				require.NoError(t, err)
				assert.Equal(t, dir+"|true\n", string(data))
			}
		})
	}
}
