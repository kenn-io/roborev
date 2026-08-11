package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

func stubRestartDrain(t *testing.T) {
	t.Helper()
	origPoll := restartDrainPollInterval
	origProcessExists := restartDrainProcessExists
	origStatus := restartDrainStatus
	origSetPaused := restartDrainSetPaused
	origEndpoint := restartDrainCurrentEndpoint
	origStderr := restartDrainStderr
	t.Cleanup(func() {
		restartDrainPollInterval = origPoll
		restartDrainProcessExists = origProcessExists
		restartDrainStatus = origStatus
		restartDrainSetPaused = origSetPaused
		restartDrainCurrentEndpoint = origEndpoint
		restartDrainStderr = origStderr
	})
	restartDrainPollInterval = time.Millisecond
}

func TestDrainAndRestartWaitsForRunningJobs(t *testing.T) {
	stubRestartDrain(t)

	ep := daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	statuses := []storage.DaemonStatus{
		{RunningJobs: 2, QueuePaused: false},
		{RunningJobs: 1, QueuePaused: true},
		{RunningJobs: 0, QueuePaused: true},
	}
	var calls []string
	restartDrainStatus = func(
		context.Context, daemon.DaemonEndpoint,
	) (storage.DaemonStatus, error) {
		status := statuses[0]
		statuses = statuses[1:]
		calls = append(calls, fmt.Sprintf("status:%d", status.RunningJobs))
		return status, nil
	}
	restartDrainSetPaused = func(
		_ context.Context, _ daemon.DaemonEndpoint, paused bool,
	) (bool, error) {
		calls = append(calls, fmt.Sprintf("pause:%t", paused))
		return paused, nil
	}
	restartDrainCurrentEndpoint = func() daemon.DaemonEndpoint { return ep }
	var stderr bytes.Buffer
	restartDrainStderr = &stderr

	err := drainAndRestart(t.Context(), restartDrainTarget{
		Endpoint: ep,
		PID:      123,
	}, func() error {
		calls = append(calls, "replace")
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, []string{
		"status:2",
		"pause:true",
		"status:1",
		"status:0",
		"replace",
		"pause:false",
	}, calls)
	assert.Contains(t, stderr.String(), "Waiting for 2 running reviews")
	assert.Contains(t, stderr.String(), "no new reviews will start")
	assert.Contains(t, stderr.String(), "Running reviews finished")
}

func TestDrainAndRestartCancellationRestoresTemporaryPause(t *testing.T) {
	stubRestartDrain(t)

	ctx, cancel := context.WithCancel(t.Context())
	statusCalls := 0
	restartDrainStatus = func(
		context.Context, daemon.DaemonEndpoint,
	) (storage.DaemonStatus, error) {
		statusCalls++
		if statusCalls == 1 {
			return storage.DaemonStatus{
				RunningJobs: 1,
				QueuePaused: false,
			}, nil
		}
		cancel()
		return storage.DaemonStatus{}, errors.New("temporarily unavailable")
	}
	var pauseCalls []bool
	restartDrainSetPaused = func(
		_ context.Context, _ daemon.DaemonEndpoint, paused bool,
	) (bool, error) {
		pauseCalls = append(pauseCalls, paused)
		return paused, nil
	}
	restartDrainCurrentEndpoint = func() daemon.DaemonEndpoint {
		return daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	}
	restartDrainStderr = &bytes.Buffer{}
	replaceCalls := 0

	err := drainAndRestart(ctx, restartDrainTarget{
		Endpoint: daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"},
		PID:      123,
	}, func() error {
		replaceCalls++
		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, replaceCalls)
	assert.Equal(t, []bool{true, false}, pauseCalls)
}

func TestDrainAndRestartReplacesAfterKnownProcessExits(t *testing.T) {
	stubRestartDrain(t)

	restartDrainStatus = func(
		context.Context, daemon.DaemonEndpoint,
	) (storage.DaemonStatus, error) {
		return storage.DaemonStatus{}, errors.New("connection refused")
	}
	restartDrainProcessExists = func(pid int) bool {
		assert.Equal(t, 123, pid)
		return false
	}
	pauseCalls := 0
	restartDrainSetPaused = func(
		context.Context, daemon.DaemonEndpoint, bool,
	) (bool, error) {
		pauseCalls++
		return false, nil
	}
	replaceCalls := 0

	err := drainAndRestart(t.Context(), restartDrainTarget{
		Endpoint: daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"},
		PID:      123,
	}, func() error {
		replaceCalls++
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, replaceCalls)
	assert.Zero(t, pauseCalls)
}

func TestDrainAndRestartPreservesExistingPause(t *testing.T) {
	stubRestartDrain(t)

	statuses := []storage.DaemonStatus{
		{RunningJobs: 1, QueuePaused: true},
		{RunningJobs: 0, QueuePaused: true},
	}
	restartDrainStatus = func(
		context.Context, daemon.DaemonEndpoint,
	) (storage.DaemonStatus, error) {
		status := statuses[0]
		statuses = statuses[1:]
		return status, nil
	}
	pauseCalls := 0
	restartDrainSetPaused = func(
		context.Context, daemon.DaemonEndpoint, bool,
	) (bool, error) {
		pauseCalls++
		return false, nil
	}
	restartDrainStderr = &bytes.Buffer{}
	replaceCalls := 0

	err := drainAndRestart(t.Context(), restartDrainTarget{
		Endpoint: daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"},
		PID:      123,
	}, func() error {
		replaceCalls++
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, replaceCalls)
	assert.Zero(t, pauseCalls)
}

func TestDrainAndRestartJoinsReplacementAndRestoreErrors(t *testing.T) {
	stubRestartDrain(t)

	statuses := []storage.DaemonStatus{
		{QueuePaused: false},
		{QueuePaused: true},
	}
	restartDrainStatus = func(
		context.Context, daemon.DaemonEndpoint,
	) (storage.DaemonStatus, error) {
		status := statuses[0]
		statuses = statuses[1:]
		return status, nil
	}
	restoreErr := errors.New("restore failed")
	restartDrainSetPaused = func(
		_ context.Context, _ daemon.DaemonEndpoint, paused bool,
	) (bool, error) {
		if !paused {
			return false, restoreErr
		}
		return true, nil
	}
	restartDrainCurrentEndpoint = func() daemon.DaemonEndpoint {
		return daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	}
	replaceErr := errors.New("replace failed")

	err := drainAndRestart(t.Context(), restartDrainTarget{
		Endpoint: daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"},
		PID:      123,
	}, func() error { return replaceErr })

	require.ErrorIs(t, err, replaceErr)
	assert.ErrorIs(t, err, restoreErr)
}
