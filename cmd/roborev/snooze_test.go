package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestSnoozeCommandSetsAndClearsCurrentWorkspace(t *testing.T) {
	assert := assert.New(t)
	repo := NewGitTestRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	repo.Run("checkout", "-b", "feature/snooze")

	requests := make(chan daemon.AgentHookSnoozeRequest, 2)
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/agent-hook/snooze" {
				return false
			}
			var req daemon.AgentHookSnoozeRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			requests <- req
			respondJSON(w, http.StatusOK, map[string]any{"snoozed": req.Enabled})
			return true
		},
	})
	chdir(t, repo.Dir)

	started := time.Now()
	var onOutput bytes.Buffer
	on := snoozeCmd()
	on.SetOut(&onOutput)
	on.SetArgs([]string{"on"})
	require.NoError(t, on.Execute())
	onRequest := <-requests
	assert.True(onRequest.Enabled)
	assert.Equal(repo.Dir, onRequest.RepoPath)
	assert.Equal(repo.Dir, onRequest.WorktreePath)
	assert.Equal("feature/snooze", onRequest.Branch)
	assert.WithinDuration(started.Add(defaultAgentHookSnooze), onRequest.SnoozedUntil, 5*time.Second)
	assert.Contains(onOutput.String(), "Reviews will continue to run")

	var offOutput bytes.Buffer
	off := snoozeCmd()
	off.SetOut(&offOutput)
	off.SetArgs([]string{"off"})
	require.NoError(t, off.Execute())
	offRequest := <-requests
	assert.False(offRequest.Enabled)
	assert.Equal("feature/snooze", offRequest.Branch)
	assert.True(offRequest.SnoozedUntil.IsZero())
	assert.Contains(offOutput.String(), "Agent hook resumed")
}
