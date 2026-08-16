package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestAgentHookSessionsLoadsExistingSnapshot(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(agenthook.StatePath()), 0o700))
	body, err := json.Marshal(agenthook.Snapshot{Sessions: map[string]agenthook.SessionState{
		"session-1": {Count: 7, ReminderPromptCount: 2},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(agenthook.StatePath(), body, 0o600))
	server, _, _ := newTestServer(t)

	got := serveHuma(t, server, http.MethodGet, "/api/agent-hook/sessions", nil)

	require.Equal(t, http.StatusOK, got.Code)
	var output AgentHookSessionsOutput
	require.NoError(t, json.Unmarshal(got.Body.Bytes(), &output.Body))
	assert.Equal(t, 7, output.Body.Sessions["session-1"].Count)
	assert.Equal(t, 2, output.Body.Sessions["session-1"].ReminderPromptCount)
}

func TestAgentHookEventUsesDatabaseReviewState(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("main.go", "package main\n", "initial")
	storedRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(
		storedRepo.ID, head, "Test Author", "initial", time.Now(),
	)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:   storedRepo.ID,
		CommitID: commit.ID,
		GitRef:   head,
		Branch:   "main",
		Agent:    "test",
	})
	require.NoError(t, err)
	setJobStatus(t, db, job.ID, storage.JobStatusRunning)
	require.NoError(t, db.CompleteJob(
		job.ID, "test", "review", "- High — unsafe query construction",
	))

	body, err := json.Marshal(agenthook.Request{
		Event: agenthook.Input{
			SessionID:     "session-1",
			CWD:           repo.Path(),
			HookEventName: "Stop",
		},
		Threshold:             100,
		FailedReviewThreshold: 1,
		Instruction:           "Resolve failed reviews.",
	})
	require.NoError(t, err)
	got := serveHuma(t, server, http.MethodPost, "/api/agent-hook/event", body)

	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	var response agenthook.Response
	require.NoError(t, json.Unmarshal(got.Body.Bytes(), &response))
	assert.True(t, response.Triggered)
	assert.Equal(t, "failed_reviews", response.TriggeredBy)

	persisted, err := os.ReadFile(agenthook.StatePath())
	require.NoError(t, err)
	var snapshot agenthook.Snapshot
	require.NoError(t, json.Unmarshal(persisted, &snapshot))
	assert.Equal(t, 1, snapshot.Sessions["session-1"].ReminderPromptCount)
}

func TestAgentHookResetPersistsSelectedSession(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(agenthook.StatePath()), 0o700))
	body, err := json.Marshal(agenthook.Snapshot{Sessions: map[string]agenthook.SessionState{
		"session-1": {Count: 1},
		"session-2": {Count: 2},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(agenthook.StatePath(), body, 0o600))
	server, _, _ := newTestServer(t)
	resetBody, err := json.Marshal(AgentHookResetRequest{SessionID: "session-1"})
	require.NoError(t, err)

	reset := serveHuma(
		t, server, http.MethodPost, "/api/agent-hook/reset", resetBody,
	)

	require.Equal(t, http.StatusOK, reset.Code, reset.Body.String())
	sessions := serveHuma(
		t, server, http.MethodGet, "/api/agent-hook/sessions", nil,
	)
	require.Equal(t, http.StatusOK, sessions.Code)
	var output AgentHookSessionsOutput
	require.NoError(t, json.Unmarshal(sessions.Body.Bytes(), &output.Body))
	assert.NotContains(t, output.Body.Sessions, "session-1")
	assert.Equal(t, 2, output.Body.Sessions["session-2"].Count)

	persisted, err := os.ReadFile(agenthook.StatePath())
	require.NoError(t, err)
	var snapshot agenthook.Snapshot
	require.NoError(t, json.Unmarshal(persisted, &snapshot))
	assert.NotContains(t, snapshot.Sessions, "session-1")
	assert.Equal(t, 2, snapshot.Sessions["session-2"].Count)
}

func TestAgentHookCorruptStateDisablesOnlyAgentHookEndpoints(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(agenthook.StatePath()), 0o700))
	corrupt := []byte(`{"sessions":`)
	require.NoError(t, os.WriteFile(agenthook.StatePath(), corrupt, 0o600))
	server, _, _ := newTestServer(t)

	status := serveHuma(t, server, http.MethodGet, "/api/status", nil)
	require.Equal(t, http.StatusOK, status.Code, status.Body.String())

	eventBody, err := json.Marshal(agenthook.Request{
		Event: agenthook.Input{SessionID: "session-1", HookEventName: "Stop"},
	})
	require.NoError(t, err)
	resetBody, err := json.Marshal(AgentHookResetRequest{All: true})
	require.NoError(t, err)
	requests := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodPost, "/api/agent-hook/event", eventBody},
		{http.MethodGet, "/api/agent-hook/sessions", nil},
		{http.MethodPost, "/api/agent-hook/reset", resetBody},
	}
	for _, request := range requests {
		got := serveHuma(
			t, server, request.method, request.path, request.body,
		)
		assert.Equal(t, http.StatusServiceUnavailable, got.Code, request.path)
		assert.Contains(t, got.Body.String(), "Agent Hook state is unavailable")
	}

	persisted, err := os.ReadFile(agenthook.StatePath())
	require.NoError(t, err)
	assert.Equal(t, corrupt, persisted)
}

func TestAgentHookMutationEndpointsRejectMissingSessionTarget(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server, _, _ := newTestServer(t)
	eventBody, err := json.Marshal(agenthook.Request{
		Event: agenthook.Input{HookEventName: "Stop"},
	})
	require.NoError(t, err)
	requests := []struct {
		path string
		body []byte
	}{
		{"/api/agent-hook/event", eventBody},
		{"/api/agent-hook/reset", []byte(`{}`)},
	}

	for _, request := range requests {
		got := serveHuma(t, server, http.MethodPost, request.path, request.body)
		assert.Equal(t, http.StatusBadRequest, got.Code, request.path)
	}
}

func TestAgentHookEventAcceptsLargeToolResponse(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server, _, _ := newTestServer(t)
	body, err := json.Marshal(agenthook.Request{
		Event: agenthook.Input{
			SessionID:     "session-1",
			HookEventName: "Notification",
			ToolResponse: json.RawMessage(
				`{"output":"` + strings.Repeat("x", 1<<20) + `"}`,
			),
		},
	})
	require.NoError(t, err)

	got := serveHuma(t, server, http.MethodPost, "/api/agent-hook/event", body)

	assert.Equal(t, http.StatusOK, got.Code, got.Body.String())
}
