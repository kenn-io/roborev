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
	if len(os.Args) != 3 || os.Args[1] != "daemon" || os.Args[2] != "stop" {
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

func TestAgentsviewChild(t *testing.T) {
	if os.Getenv("TEST_ISOLATED_CHILD") == "" {
		return
	}
	require.NoError(t, os.WriteFile(os.Getenv("TEST_HOME_FILE"), []byte(os.Getenv("HOME")), 0o600))
	if os.Getenv("TEST_NO_LOOKUP") == "" && os.Getenv("TEST_MISSING") == "" {
		cmd := exec.CommandContext(t.Context(), "agentsview", "session", "usage-batch", "x", "--format", "json", "--no-sync")
		require.NoError(t, cmd.Run())
	}
	if os.Getenv("TEST_CHANGE_PATH") != "" {
		require.NoError(t, os.Setenv("PATH", os.Getenv("TEST_EMPTY_PATH")))
	}
	if os.Getenv("TEST_SUITE_FAIL") != "" {
		assert.Equal(t, "passing", "failing")
	}
}

func TestRunIsolatedMainStopsAgentsView(t *testing.T) {
	if os.Getenv("TEST_ISOLATED_CHILD") != "" {
		return
	}
	for _, tc := range []struct {
		name                 string
		env                  []string
		stop, fail, preserve bool
	}{
		{"cleanup", nil, true, false, false},
		{"override", nil, true, false, false},
		{"path changed", []string{"TEST_CHANGE_PATH=1"}, true, false, false},
		{"missing", []string{"TEST_MISSING=1"}, false, false, false},
		{"stop fails", []string{"TEST_STOP_FAIL=1"}, true, true, true},
		{"suite and stop fail", []string{"TEST_STOP_FAIL=1", "TEST_SUITE_FAIL=1"}, true, true, true},
		{"no lookup", []string{"TEST_NO_LOOKUP=1"}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			events, homeFile := filepath.Join(dir, "events"), filepath.Join(dir, "home")
			other := filepath.Join(dir, "other")
			cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestAgentsviewChild$")
			cmd.Env = append(os.Environ(), "PATH="+bin, "TEST_ISOLATED_CHILD=1", "TEST_EVENTS="+events, "TEST_HOME_FILE="+homeFile, "TEST_EMPTY_PATH="+dir, "AGENTSVIEW_DATA_DIR=")
			if tc.name == "override" {
				cmd.Env = append(cmd.Env, "AGENTSVIEW_DATA_DIR="+other)
			}
			cmd.Env = append(cmd.Env, tc.env...)
			output, err := cmd.CombinedOutput()
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err, "%s", output)
			}
			homeBytes, err := os.ReadFile(homeFile)
			require.NoError(t, err)
			home := string(homeBytes)
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
			assert.NoDirExists(t, other)
		})
	}
}
