//go:build darwin && integration

package main

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func assertSuspendedManagedDaemon(t *testing.T, original *daemon.RuntimeInfo, unit string, mainPID int) {
	t.Helper()
	record, err := daemon.ReadRuntimeForPID(original.PID)
	require.NoError(t, err)
	process, err := os.FindProcess(original.PID)
	require.NoError(t, err)
	require.NoError(t, process.Signal(syscall.SIGSTOP))
	defer func() { require.NoError(t, process.Signal(syscall.SIGCONT)) }()

	oldStart := startDaemonForEnsure
	defer func() { startDaemonForEnsure = oldStart }()
	startDaemonForEnsure = func() error { return nil }
	_ = ensureDaemon()
	runtimes, err := daemon.ListJudgedRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 1)
	assert.Equal(t, record, runtimes[0])
	pid, err := userServiceMainPID(t.Context(), unit)
	require.NoError(t, err)
	assert.Equal(t, mainPID, pid)
	require.NoError(t, process.Signal(syscall.SIGCONT))
	resumed, err := daemon.GetAnyRunningDaemonContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, original.PID, resumed.PID)
}
