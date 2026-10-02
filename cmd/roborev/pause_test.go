package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
)

func TestDaemonRunAppliesInitialPauseBeforeWorkers(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(map[bool]string{true: "pause", false: "unpause"}[paused], func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			original := openDaemonSearchIndex
			stop := errors.New("stop before starting workers")
			openDaemonSearchIndex = func(context.Context, string) (*searchindex.Index, error) { return nil, stop }
			t.Cleanup(func() { openDaemonSearchIndex = original })
			cmd := daemonRunCmd()
			cmd.SetArgs([]string{"--db", storage.DefaultDBPath(), "--queue-paused=" + map[bool]string{true: "true", false: "false"}[paused]})
			require.ErrorIs(t, cmd.Execute(), stop)
			db, err := storage.OpenReadOnly(storage.DefaultDBPath())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			got, err := db.IsQueuePaused()
			require.NoError(t, err)
			assert.Equal(t, paused, got)
		})
	}
}

func TestPauseCmdPostsQueuePause(t *testing.T) {
	var called bool
	md := NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.Method != http.MethodPost || r.URL.Path != "/api/queue/pause" {
				return false
			}
			called = true
			_ = json.NewEncoder(w).Encode(map[string]bool{"queue_paused": true})
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
			_ = json.NewEncoder(w).Encode(map[string]bool{"queue_paused": false})
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
