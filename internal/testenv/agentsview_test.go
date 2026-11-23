package testenv

import (
	"context"
	"fmt"
	"io"
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
	if len(os.Args) > 1 && os.Args[1] == "session" {
		if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(filepath.Join(dir, "record"), []byte("running"), 0o600) != nil {
			return 1
		}
		if os.Getenv("TEST_DATABASE_ONLY") == "" {
			runtimeDir := dir
			if os.Getenv("TEST_RUNTIME_SUBDIR") != "" {
				runtimeDir = filepath.Join(dir, "run")
			}
			store := daemon.RuntimeStore{Dir: runtimeDir}
			_, err := store.Write(daemon.RuntimeRecord{PID: os.Getppid(), Address: "127.0.0.1:1"})
			if err != nil {
				return 1
			}
		}
		return 0
	}
	if len(os.Args) != 3 || os.Args[1] != "serve" || os.Args[2] != "stop" {
		return 1
	}
	_, dirErr := os.Stat(dir)
	_, homeErr := os.Stat(os.Getenv("HOME"))
	line := fmt.Sprintf("%s|%t|%t\n", dir, dirErr == nil, homeErr == nil)
	events, err := os.OpenFile(os.Getenv("TEST_EVENTS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 1
	}
	_, err = events.WriteString(line)
	closeErr := events.Close()
	if err != nil || closeErr != nil {
		return 1
	}
	if os.Getenv("TEST_STOP_FAIL") != "" {
		return 1
	}
	if os.Remove(filepath.Join(dir, "record")) != nil {
		return 1
	}
	return 0
}

func TestRunIsolatedMainStopsAgentsView(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		stop, fail, preserve bool
		suiteCode            int
	}{
		{"cleanup", true, false, false, 0},
		{"override", true, false, false, 0},
		{"path changed", true, false, false, 0},
		{"missing", false, false, false, 0},
		{"stop fails", true, true, true, 0},
		{"suite and stop fail", true, true, true, 7},
		{"no lookup", false, false, false, 0},
		{"database only", false, false, false, 0},
		{"runtime subdir", true, false, false, 0},
		{"panic", true, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The runner changes the process environment as a real TestMain does.
			for _, key := range []string{"HOME", "USERPROFILE", "GOENV", "GOPATH", "GOCACHE", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_STATE_DIR", "MISE_CACHE_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME", "GIT_TERMINAL_PROMPT"} {
				t.Setenv(key, os.Getenv(key))
			}
			oldUserEnv, oldAgentEnv := userEnv, userAgentEnv
			t.Cleanup(func() { userEnv, userAgentEnv = oldUserEnv, oldAgentEnv })
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			require.NoError(t, os.Mkdir(bin, 0o755))
			exe, err := os.Executable()
			require.NoError(t, err)
			name := "agentsview"
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			if tc.name != "missing" {
				src, err := os.Open(exe)
				require.NoError(t, err)
				dst, err := os.OpenFile(filepath.Join(bin, name), os.O_CREATE|os.O_WRONLY, 0o755)
				require.NoError(t, err)
				_, err = io.Copy(dst, src)
				require.NoError(t, err)
				require.NoError(t, dst.Close())
				require.NoError(t, src.Close())
			}
			events := filepath.Join(dir, "events")
			other := filepath.Join(dir, "other")
			t.Setenv("PATH", bin)
			t.Setenv("TEST_EVENTS", events)
			t.Setenv("TEST_STOP_FAIL", "")
			t.Setenv("TEST_DATABASE_ONLY", "")
			t.Setenv("TEST_RUNTIME_SUBDIR", "")
			if tc.name == "database only" {
				t.Setenv("TEST_DATABASE_ONLY", "1")
			}
			if tc.name == "runtime subdir" {
				t.Setenv("TEST_RUNTIME_SUBDIR", "1")
			}
			inheritedDir := ""
			if tc.name == "override" {
				inheritedDir = other
			}
			t.Setenv("AGENTSVIEW_DATA_DIR", inheritedDir)
			if tc.name == "override" {
				cmd := exec.CommandContext(t.Context(), "agentsview", "session", "usage-batch", "x", "--format", "json", "--no-sync")
				require.NoError(t, cmd.Run())
			}
			if tc.fail {
				t.Setenv("TEST_STOP_FAIL", "1")
			}
			stderr, err := os.Create(filepath.Join(dir, "stderr"))
			require.NoError(t, err)
			oldStderr := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() { os.Stderr = oldStderr; _ = stderr.Close() })
			var home string
			run := func() int {
				home = os.Getenv("HOME")
				if tc.name != "missing" && tc.name != "no lookup" {
					cmd := exec.CommandContext(t.Context(), "agentsview", "session", "usage-batch", "x", "--format", "json", "--no-sync")
					assert.NoError(t, cmd.Run())
				}
				if tc.name == "path changed" || tc.name == "override" {
					assert.NoError(t, os.Setenv("PATH", dir))
				}
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
			os.Stderr = oldStderr
			require.NoError(t, stderr.Close())
			output, err := os.ReadFile(filepath.Join(dir, "stderr"))
			require.NoError(t, err)
			expectedCode := tc.suiteCode
			if tc.fail && expectedCode == 0 {
				expectedCode = 1
			}
			assert.Equal(t, expectedCode, code)
			assert.Equal(t, inheritedDir, os.Getenv("AGENTSVIEW_DATA_DIR"))
			if tc.preserve {
				t.Cleanup(func() { _ = os.RemoveAll(home) })
				assert.Contains(t, string(output), "failed to stop agentsview")
				assert.Contains(t, string(output), home)
				assert.FileExists(t, filepath.Join(home, ".agentsview", "record"))
			} else {
				assert.NoDirExists(t, home)
			}
			if tc.stop {
				data, err := os.ReadFile(events)
				require.NoError(t, err)
				assert.Equal(t, filepath.Join(home, ".agentsview")+"|true|true\n", string(data))
			} else {
				assert.NoFileExists(t, events)
			}
			if tc.name == "override" {
				assert.FileExists(t, filepath.Join(other, "record"))
			} else {
				assert.NoDirExists(t, other)
			}
		})
	}
	t.Run("real agentsview", func(t *testing.T) {
		binary, err := exec.LookPath("agentsview")
		if err != nil {
			t.Skip("agentsview is not installed")
		}
		for _, args := range [][]string{{"session", "usage-batch", "--help"}, {"serve", "stop", "--help"}} {
			cmd := exec.CommandContext(t.Context(), binary, args...)
			output, err := cmd.Output()
			if err != nil || (args[0] == "session" && (!strings.Contains(string(output), "--no-sync") || !strings.Contains(string(output), "--format"))) {
				t.Skip("agentsview does not support batch lookup and serve stop")
			}
		}
		for _, key := range []string{"HOME", "USERPROFILE", "GOENV", "GOPATH", "GOCACHE", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_STATE_DIR", "MISE_CACHE_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME", "GIT_TERMINAL_PROMPT"} {
			t.Setenv(key, os.Getenv(key))
		}
		oldUserEnv, oldAgentEnv := userEnv, userAgentEnv
		t.Cleanup(func() { userEnv, userAgentEnv = oldUserEnv, oldAgentEnv })
		other := filepath.Join(t.TempDir(), "other")
		t.Setenv("AGENTSVIEW_DATA_DIR", other)
		lookup := func(dir string) error {
			cmd := exec.CommandContext(t.Context(), binary, "session", "usage-batch", "missing-session", "--format", "json", "--no-sync")
			cmd.Env = append(os.Environ(), "AGENTSVIEW_DATA_DIR="+dir)
			return cmd.Run()
		}
		records := func(dir string) []daemon.RuntimeRecord {
			var all []daemon.RuntimeRecord
			for _, path := range []string{dir, filepath.Join(dir, "run")} {
				if _, err := os.Stat(path); err != nil {
					continue
				}
				found, err := (daemon.RuntimeStore{Dir: path}).List()
				assert.NoError(t, err)
				all = append(all, found...)
			}
			return all
		}
		// The lookup starts a detached server, so stop it even when an assertion fails.
		cleanup := func(dir string) {
			t.Cleanup(func() {
				live, err := liveAgentsviewRecords(dir)
				if !assert.NoError(t, err) || !live {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, "serve", "stop")
				cmd.Env = append(os.Environ(), "AGENTSVIEW_DATA_DIR="+dir)
				assert.NoError(t, cmd.Run())
			})
		}
		cleanup(other)
		require.NoError(t, lookup(other))
		otherRecords := records(other)
		require.NotEmpty(t, otherRecords)
		require.True(t, daemon.RuntimeProcessAlive(otherRecords[0]))
		var isolatedRecords []daemon.RuntimeRecord
		code := runIsolatedMain(func() int {
			dir := os.Getenv("AGENTSVIEW_DATA_DIR")
			cleanup(dir)
			if !assert.NoError(t, lookup(dir)) {
				return 1
			}
			isolatedRecords = records(dir)
			if !assert.NotEmpty(t, isolatedRecords) {
				return 1
			}
			assert.True(t, daemon.RuntimeProcessAlive(isolatedRecords[0]))
			return 0
		})
		assert.Zero(t, code)
		if len(isolatedRecords) > 0 {
			assert.False(t, daemon.RuntimeProcessAlive(isolatedRecords[0]))
		}
		assert.True(t, daemon.RuntimeProcessAlive(otherRecords[0]))
	})
}
