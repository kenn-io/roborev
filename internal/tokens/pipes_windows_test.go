package tokens

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/roborev/internal/procutil"
	"go.kenn.io/roborev/internal/testenv"
)

// The copied test executable acts as a native launcher, its program, and a
// grandchild holding the output pipes. It needs neither Scoop nor a shell.
func TestMain(m *testing.M) {
	if mode := os.Getenv("ROBOREV_TOKEN_PIPE_HELPER"); mode != "" {
		os.Exit(runTokenPipeHelper(mode))
	}
	os.Exit(testenv.RunIsolatedMain(m))
}

func runTokenPipeHelper(mode string) int {
	dir := os.Getenv("ROBOREV_TOKEN_PIPE_DIR")
	if mode == "launcher" {
		if err := os.WriteFile(filepath.Join(dir, mode+".pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 1
		}
	}
	if mode == "holder" {
		event, err := windows.CreateEvent(nil, 0, 0, nil)
		if err != nil {
			return 1
		}
		defer windows.CloseHandle(event)
		_, _ = windows.WaitForSingleObject(event, windows.INFINITE)
		return 0
	}
	next := "worker"
	if mode == "worker" {
		next = "holder"
	}
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	// The helper process runs outside any test, so it has no test context.
	cmd := exec.CommandContext(context.Background(), exe)
	cmd.Env = append(os.Environ(), "ROBOREV_TOKEN_PIPE_HELPER="+next)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	procutil.HideConsole(cmd)
	if err := cmd.Start(); err != nil {
		return 1
	}
	// The parent records the child, so cancellation cannot interrupt a child
	// publishing its own PID and leave cleanup unable to identify it.
	if err := os.WriteFile(filepath.Join(dir, next+".pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 1
	}
	if mode == "worker" && os.Getenv("ROBOREV_TOKEN_PIPE_SUCCESS") == "1" {
		fmt.Println(`{"session_id":"s","total_output_tokens":42,"peak_context_tokens":900,"cost_usd":0.07,"has_cost":true}`)
		return 0
	}
	if err := cmd.Wait(); err != nil {
		return 1
	}
	return 0
}

func installPipeHoldingAgentsview(t *testing.T, returnsUsage bool) {
	t.Helper()
	assert := assert.New(t)
	dir := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(exe)
	require.NoError(t, err)
	bin := filepath.Join(dir, "agentsview.exe")
	require.NoError(t, os.WriteFile(bin, data, 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ROBOREV_TOKEN_PIPE_HELPER", "launcher")
	t.Setenv("ROBOREV_TOKEN_PIPE_DIR", dir)
	t.Setenv("ROBOREV_TOKEN_PIPE_SUCCESS", "0")
	if returnsUsage {
		t.Setenv("ROBOREV_TOKEN_PIPE_SUCCESS", "1")
	}
	t.Cleanup(func() {
		for _, mode := range []string{"holder", "worker", "launcher"} {
			data, err := os.ReadFile(filepath.Join(dir, mode+".pid"))
			assert.NoError(err, "%s must have started to exercise inherited pipes", mode)
			pid, err := strconv.ParseUint(string(data), 10, 32)
			if !assert.NoError(err) {
				continue
			}
			process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				if returnsUsage {
					assert.NotEqual("holder", mode, "successful lookup terminated its background helper")
				}
				continue // The process has exited and its object was released.
			}
			if !assert.NoError(err) {
				continue
			}
			state, waitErr := windows.WaitForSingleObject(process, 0)
			if returnsUsage && mode == "holder" {
				assert.NoError(waitErr)
				assert.Equal(uint32(windows.WAIT_TIMEOUT), state, "successful lookup terminated its background helper")
			}
			if waitErr == nil && state == windows.WAIT_OBJECT_0 {
				assert.NoError(windows.CloseHandle(process))
				continue
			}
			var path [windows.MAX_LONG_PATH]uint16
			size := uint32(len(path))
			err = windows.QueryFullProcessImageName(process, 0, &path[0], &size)
			if err != nil {
				// The process may have terminated since the zero-time wait.
				state, waitErr = windows.WaitForSingleObject(process, 0)
				assert.NoError(waitErr)
				assert.Equal(uint32(windows.WAIT_OBJECT_0), state)
				assert.NoError(windows.CloseHandle(process))
				continue
			}
			if !strings.EqualFold(bin, windows.UTF16ToString(path[:size])) {
				// A recycled PID belongs to another process; never terminate it.
				assert.NoError(windows.CloseHandle(process))
				continue
			}
			if !returnsUsage {
				// Wall-clock wait: Windows process termination is asynchronous.
				assert.Eventually(func() bool {
					state, err := windows.WaitForSingleObject(process, 0)
					return err == nil && state == windows.WAIT_OBJECT_0
				}, 10*time.Second, 10*time.Millisecond, "timed-out lookup left %s alive", mode)
			}
			// Successful agentsview calls may start a server. Clean up this
			// fixture's holder explicitly, including when an assertion fails.
			_ = windows.TerminateProcess(process, 1)
			state, err = windows.WaitForSingleObject(process, 10_000)
			assert.NoError(err)
			assert.Equal(uint32(windows.WAIT_OBJECT_0), state)
			assert.NoError(windows.CloseHandle(process))
		}
	})
}

func TestAgentsviewCommandPreservesWindowsStderr(t *testing.T) {
	cmd := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	_, err := runAgentsviewCommand(t.Context(), 10*time.Second, cmd,
		"/d", "/c", "echo usage lookup failed 1>&2 & exit /b 1")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Contains(t, string(exitErr.Stderr), "usage lookup failed")
}
