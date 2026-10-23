//go:build linux

package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/testenv"
)

func TestUpdateIgnoresDeadSandbox(t *testing.T) {
	if writeDeadSandboxRuntimeForUpdate(t) {
		return
	}
	stubRestartVars(t)
	listAllRuntimes = daemon.ListAllRuntimes
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	isPIDAliveForUpdate = kitdaemon.ProcessAlive
	runtimes, err := daemon.ListAllRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 1)

	info, err := discoverDaemonForUpdate()
	require.NoError(t, err)
	assert.Nil(t, info)
	assert.NoFileExists(t, runtimes[0].SourcePath)
}

func TestUpdateDoesNotAdoptDeadSandboxPID(t *testing.T) {
	if writeDeadSandboxRuntimeForUpdate(t) {
		return
	}
	stubRestartVars(t)
	listAllRuntimes = daemon.ListAllRuntimes
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	isPIDAliveForUpdate = kitdaemon.ProcessAlive

	pids, err := runtimePIDSet()
	require.NoError(t, err)
	assert.Empty(t, pids)
	assert.Zero(t, replacementRuntimePID(0))
	pid, err := waitForDaemonExitContext(t.Context(), 0)
	require.NoError(t, err)
	assert.Zero(t, pid)
}

// writeDeadSandboxRuntimeForUpdate reexecutes the test in a PID namespace
// and leaves its real runtime record behind after that namespace exits.
func writeDeadSandboxRuntimeForUpdate(t *testing.T) bool {
	t.Helper()
	if dataDir := os.Getenv("ROBOREV_TEST_UPDATE_PIDNS_DATA"); dataDir != "" {
		t.Setenv("ROBOREV_DATA_DIR", dataDir)
		require.Equal(t, 2, os.Getpid())
		require.NoError(t, daemon.WriteRuntime(daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}, nil, "test", nil))
		return true
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("requires bubblewrap for PID namespaces")
	}
	args := []string{"--unshare-pid", "--bind", "/", "/", "--proc", "/proc", "--"}
	if output, err := exec.CommandContext(t.Context(), bwrap, append(args, "true")...).CombinedOutput(); err != nil {
		t.Skipf("PID namespaces unavailable: %v: %s", err, output)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("ROBOREV_TEST_UPDATE_PIDNS_DATA", testenv.SetDataDir(t))
	executable, err := os.Executable()
	require.NoError(t, err)
	output, err := exec.CommandContext(t.Context(), bwrap, append(args, executable, "-test.run=^"+t.Name()+"$", "-test.timeout=30s")...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	records, err := daemon.RuntimeStore().List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, kitdaemon.ProcessScopeGone, kitdaemon.RuntimeProcessScope(records[0]))
	require.True(t, kitdaemon.ProcessAlive(records[0].PID), "host must have an unrelated process at the sandbox PID")
	return false
}
