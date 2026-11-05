//go:build darwin && integration

package main

import (
	"encoding/json"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func assertSuspendedManagedDaemon(t *testing.T, original *daemon.RuntimeInfo, unit string, mainPID int) {
	t.Helper()
	process, err := os.FindProcess(original.PID)
	require.NoError(t, err)
	require.NoError(t, process.Signal(syscall.SIGSTOP))
	defer func() { require.NoError(t, process.Signal(syscall.SIGCONT)) }()

	oldEnsure, oldStatusEnsure := startDaemonForEnsure, statusEnsureDaemon
	defer func() { startDaemonForEnsure, statusEnsureDaemon = oldEnsure, oldStatusEnsure }()
	detached := 0
	startDaemonForEnsure = func() error { detached++; return nil }
	err = ensureDaemon()
	var managed *managedRecoveryError
	require.ErrorAs(t, err, &managed)
	require.ErrorContains(t, err, "run "+serviceRestartCommand+" "+unit)
	statusEnsureDaemon = ensureDaemon
	cmd := statusCmd()
	cmd.SetArgs([]string{"--json"})
	output := captureStdout(t, func() { require.NoError(t, cmd.Execute()) })
	var result statusJSONResult
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	assert.True(t, result.Running)
	assert.Contains(t, result.Error, unit)
	assert.Contains(t, result.Error, "run "+serviceRestartCommand+" "+unit)
	assert.Nil(t, result.Daemon)
	assert.Nil(t, result.Health)
	assert.Empty(t, result.Jobs)
	assert.Zero(t, detached)
	runtimes, err := daemon.ListJudgedRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 1)
	assert.Equal(t, original.PID, runtimes[0].PID)
	pid, err := userServiceMainPID(t.Context(), unit)
	require.NoError(t, err)
	assert.Equal(t, mainPID, pid)
	require.NoError(t, process.Signal(syscall.SIGCONT))
	resumed, err := daemon.GetAnyRunningDaemonContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, original.PID, resumed.PID)
}
