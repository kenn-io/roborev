package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

// Direct daemon startup uses --queue-paused separately from cross-process recovery intent.
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

func TestLocalQueueCommandCompletesOnlyItsOwnIntent(t *testing.T) {
	for _, scenario := range []string{"success", "API failure", "recovery failure"} {
		t.Run(scenario, func(t *testing.T) {
			isolateDaemonSelection(t)
			t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
			marker := filepath.Join(config.DataDir(), "queue-intent.json")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/queue/pause", r.URL.Path)
				assert.NotEmpty(t, r.Header.Get(daemon.QueueIntentHeader))
				if scenario == "API failure" {
					http.Error(w, "queue unavailable", http.StatusInternalServerError)
					return
				}
				// Older daemons accept this update without consuming the token.
				_, _ = w.Write([]byte(`{"queue_paused":true}`))
			}))
			defer server.Close()
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
				if scenario == "recovery failure" {
					return nil, daemon.ErrDaemonAccessDenied
				}
				return &daemon.RuntimeInfo{Address: strings.TrimPrefix(server.URL, "http://"), Version: version.Version}, nil
			}
			probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
				return &daemon.PingInfo{Version: version.Version}, nil
			}
			err := setQueuePaused(t.Context(), true)
			if strings.HasSuffix(scenario, "failure") {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "pending")
				assert.NoFileExists(t, marker)
				return
			}
			require.NoError(t, err)
			assert.NoFileExists(t, marker)
		})
	}
}
