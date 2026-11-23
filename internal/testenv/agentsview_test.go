package testenv

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeAgentsview() int {
	dir := os.Getenv("AGENTSVIEW_DATA_DIR")
	if len(os.Args) > 1 && os.Args[1] == "session" {
		if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(filepath.Join(dir, "record"), []byte("running"), 0o600) != nil {
			return 1
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
			code := runIsolatedMain(func() int {
				home = os.Getenv("HOME")
				if tc.name != "missing" && tc.name != "no lookup" {
					cmd := exec.CommandContext(t.Context(), "agentsview", "session", "usage-batch", "x", "--format", "json", "--no-sync")
					assert.NoError(t, cmd.Run())
				}
				if tc.name == "path changed" {
					assert.NoError(t, os.Setenv("PATH", dir))
				}
				return tc.suiteCode
			})
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
}
