package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestWriteLocalQueuePaused(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())

	require.NoError(t, writeLocalQueuePaused(true))

	db, err := storage.Open(storage.DefaultDBPath())
	require.NoError(t, err)
	defer db.Close()

	paused, err := db.IsQueuePaused()
	require.NoError(t, err)
	assert.True(t, paused)
}

func TestStartDaemonPersistsPendingPauseState(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	if !isGoTestBinaryPath(exe) {
		t.Skipf("expected go test binary path, got %q", exe)
	}

	setupIsolatedDataDir(t)
	t.Setenv("ROBOREV_TEST_ALLOW_AUTOSTART", "")

	paused := true
	pendingStartPause = &paused
	t.Cleanup(func() { pendingStartPause = nil })

	// startDaemon refuses to spawn the ephemeral test binary, but it must
	// persist the pending pause state first, in the safe pre-launch window
	// (no daemon owns the DB), before its workers could claim jobs.
	_ = startDaemon()

	db, err := storage.Open(storage.DefaultDBPath())
	require.NoError(t, err)
	defer db.Close()

	got, err := db.IsQueuePaused()
	require.NoError(t, err)
	assert.True(t, got,
		"startDaemon must persist the pending pause flag before launch")
}

func TestPauseCmdPostsQueuePause(t *testing.T) {
	var called bool
	md := NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.Method != http.MethodPost || r.URL.Path != "/api/queue/pause" {
				return false
			}
			called = true
			_ = json.NewEncoder(w).Encode(queuePauseResponse{QueuePaused: true})
			return true
		},
	})
	defer md.Close()

	output := captureStdout(t, func() {
		cmd := pauseCmd()
		require.NoError(t, cmd.Execute())
	})

	assert.True(t, called)
	assert.Contains(t, output, "Queue paused")
}

func TestUnpauseCmdPostsQueueUnpause(t *testing.T) {
	var called bool
	md := NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.Method != http.MethodPost || r.URL.Path != "/api/queue/unpause" {
				return false
			}
			called = true
			_ = json.NewEncoder(w).Encode(queuePauseResponse{QueuePaused: false})
			return true
		},
	})
	defer md.Close()

	output := captureStdout(t, func() {
		cmd := unpauseCmd()
		require.NoError(t, cmd.Execute())
	})

	assert.True(t, called)
	assert.Contains(t, output, "Queue unpaused")
}
