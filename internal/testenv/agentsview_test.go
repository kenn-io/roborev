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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func fakeAgentsview() int {
	dir := os.Getenv("AGENTSVIEW_DATA_DIR")
	mode := os.Getenv("TEST_CASE")
	if len(os.Args) > 1 && os.Args[1] == "session" {
		if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(filepath.Join(dir, "record"), []byte("running"), 0o600) != nil {
			return 1
		}
		if mode == "database only" {
			return 0
		}
		runtimeDir := dir
		if mode == "runtime subdir" {
			runtimeDir = filepath.Join(dir, "run")
		}
		if _, err := (daemon.RuntimeStore{Dir: runtimeDir}).Write(daemon.RuntimeRecord{PID: os.Getppid(), Address: "127.0.0.1:1"}); err != nil {
			return 1
		}
		return 0
	}
	if len(os.Args) != 3 || os.Args[1] != "serve" || os.Args[2] != "stop" {
		return 1
	}
	_, dirErr := os.Stat(dir)
	_, homeErr := os.Stat(os.Getenv("HOME"))
	events, err := os.OpenFile(os.Getenv("TEST_EVENTS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 1
	}
	_, err = fmt.Fprintf(events, "%s|%t|%t\n", dir, dirErr == nil, homeErr == nil)
	closeErr := events.Close()
	if err != nil || closeErr != nil || strings.HasPrefix(mode, "stop fails") {
		return 1
	}
	if strings.HasPrefix(mode, "stop skips") {
		return 0
	}
	runtimeDir := dir
	if mode == "runtime subdir" {
		runtimeDir = filepath.Join(dir, "run")
	}
	path, err := (daemon.RuntimeStore{Dir: runtimeDir}).Path(os.Getppid())
	if err != nil || os.Remove(path) != nil {
		return 1
	}
	return 0
}

// restoreProcessState undoes the process changes runIsolatedMain makes, as a real TestMain's exit would.
func restoreProcessState(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "GOENV", "GOPATH", "GOCACHE", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_STATE_DIR", "MISE_CACHE_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME", "GIT_TERMINAL_PROMPT"} {
		t.Setenv(key, os.Getenv(key))
	}
	oldUserEnv, oldAgentEnv := userEnv, userAgentEnv
	t.Cleanup(func() { userEnv, userAgentEnv = oldUserEnv, oldAgentEnv })
}

func TestRunIsolatedMainStopsAgentsView(t *testing.T) {
	for _, tc := range []struct {
		name            string
		suiteCode, want int
	}{
		{"stop", 0, 0},
		{"database only", 0, 0},
		{"runtime subdir", 0, 0},
		{"stop fails", 0, 1},
		{"stop fails in failing suite", 7, 7},
		{"stop skips", 0, 1},
		{"stop skips in failing suite", 7, 7},
		{"panic", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreProcessState(t)
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			require.NoError(t, os.Mkdir(bin, 0o755))
			exe, err := os.Executable()
			require.NoError(t, err)
			data, err := os.ReadFile(exe)
			require.NoError(t, err)
			name := "agentsview"
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			require.NoError(t, os.WriteFile(filepath.Join(bin, name), data, 0o755))
			events := filepath.Join(dir, "events")
			other := filepath.Join(dir, "other")
			t.Setenv("PATH", bin)
			t.Setenv("TEST_EVENTS", events)
			t.Setenv("TEST_CASE", tc.name)
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
				if tc.name == "panic" {
					panic("test panic")
				}
				return tc.suiteCode
			}
			code := 0
			if tc.name == "panic" {
				assert.Panics(t, func() { runIsolatedMain(run) })
			} else {
				code = runIsolatedMain(run)
			}
			output, err := os.ReadFile(stderrPath)
			require.NoError(t, err)
			assert.Equal(t, tc.want, code)
			assert.Equal(t, other, os.Getenv("AGENTSVIEW_DATA_DIR"))
			if strings.HasPrefix(tc.name, "stop fails") || strings.HasPrefix(tc.name, "stop skips") {
				t.Cleanup(func() { _ = os.RemoveAll(home) })
				assert.Contains(t, string(output), "failed to stop agentsview")
				assert.Contains(t, string(output), home)
				if strings.HasPrefix(tc.name, "stop skips") {
					assert.Contains(t, string(output), "still running")
				}
				assert.FileExists(t, filepath.Join(home, ".agentsview", "record"))
			} else {
				assert.NoDirExists(t, home)
			}
			if tc.name == "database only" {
				assert.NoFileExists(t, events)
			} else {
				data, err := os.ReadFile(events)
				require.NoError(t, err)
				assert.Equal(t, filepath.Join(home, ".agentsview")+"|true|true\n", string(data))
			}
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
			records, err := agentsviewRecords(dir)
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
