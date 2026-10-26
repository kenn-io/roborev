package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/testutil"
)

func enableKataConnection(server *Server, fake *katafake.Server) {
	server.configWatcher.Config().Integrations.Kata = config.KataIntegrationConfig{
		Enabled: true, Endpoint: fake.URL(), APIKey: "synthetic-kata-credential",
		DefaultProject: "reviews", Teammate: "roborev",
	}
}

func readRequests(fake *katafake.Server) (gets, writes int) {
	for _, r := range fake.Requests() {
		if r.Method == http.MethodGet {
			gets++
		} else {
			writes++
		}
	}
	return gets, writes
}

// A goal review enqueued while the connection is on captures the open Kata
// graph through the API, from the default project when the repository has no
// binding, and never starts the kata CLI.
func TestGoalEnqueueCapturesTheOpenGraphThroughTheConnection(t *testing.T) {
	marker := kataOnPathMarker(t)
	fake := katafake.New(t)
	fake.AddIssue(7, "ab12", "Open requirement", "Body of the requirement")
	closed := fake.AddIssue(7, "cd34", "Finished", "")
	closed.Out.Status = "closed"

	server, _, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	enableKataConnection(server, fake)
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())

	job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), ReviewType: config.ReviewTypeGoal, Agent: "pi"})
	snapshot, err := goalreview.ParseSnapshot(job.Prompt)
	require.NoError(t, err)
	assert.Equal(t, "reviews", snapshot.Project)
	require.Len(t, snapshot.Issues, 1)
	assert.Equal(t, "ab12", snapshot.Issues[0].ShortID)
	assert.Equal(t, "Body of the requirement", snapshot.Issues[0].Body)
	gets, writes := readRequests(fake)
	assert.Positive(t, gets)
	assert.Zero(t, writes, "capturing the graph writes nothing")
	assert.NoFileExists(t, marker)
}

func TestGoalGateCapturesTheOpenGraphThroughTheConnection(t *testing.T) {
	marker := kataOnPathMarker(t)
	fake := katafake.New(t)
	fake.AddIssue(7, "ab12", "Open requirement", "Body")

	server, _, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	enableKataConnection(server, fake)
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_agent = \"pi\"\n"), 0o600))
	var seen goalreview.Snapshot
	server.workerPool.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, snapshot goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
		seen = snapshot
		return nil, nil
	})
	server.workerPool.Start()

	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/goal-review", GoalGateRequest{RepoPath: repo.Path()}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "reviews", seen.Project)
	require.Len(t, seen.Issues, 1)
	assert.Equal(t, "ab12", seen.Issues[0].ShortID)
	assert.NoFileExists(t, marker)
}

// An enabled but unusable connection must stop the goal review, which fails
// closed when a bound ledger is unavailable, instead of reading another Kata.
func TestGoalEnqueueFailsWhenTheConnectionIsBroken(t *testing.T) {
	marker := kataOnPathMarker(t)
	server, _, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	server.configWatcher.Config().Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "ftp://kata.example.com", Teammate: "roborev"}
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())

	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{RepoPath: repo.Path(), ReviewType: config.ReviewTypeGoal, Agent: "pi"}))
	assert.GreaterOrEqual(t, w.Code, http.StatusBadRequest, w.Body.String())
	assert.Contains(t, w.Body.String(), "endpoint")
	assert.NoFileExists(t, marker)
}
