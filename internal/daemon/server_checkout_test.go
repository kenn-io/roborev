package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestHandleEnqueueIsolationDisablesReuse(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	sha := repo.CommitFile("file.txt", "base\n", "base")
	cfg := server.configWatcher.Config()
	cfg.ReuseReviewSession = new(true)
	cfg.ReviewInWorktree = new(true)
	stored, err := db.GetOrCreateRepo(repo.Path(), config.ResolveRepoIdentity(repo.Path(), nil))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(stored.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	prior, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: stored.ID, CommitID: commit.ID, GitRef: sha, Branch: "main", Agent: "test", Reasoning: "thorough", ReviewType: config.ReviewTypeDefault})
	require.NoError(t, err)
	_, err = db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NoError(t, db.SaveJobSessionID(prior.ID, testWorkerID, "prior-session"))
	require.NoError(t, testutil.CompleteReviewFixture(db, prior.ID, "test", "prompt", "No issues found."))
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{RepoPath: repo.Path(), GitRef: "HEAD", Branch: "main", Agent: "test"})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	assert.Empty(t, job.SessionID)
	assert.Nil(t, job.ResumeSourceJobUUID)
	cfg.ReviewInWorktree = new(false)
	w = httptest.NewRecorder()
	req = testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{RepoPath: repo.Path(), GitRef: "HEAD", Branch: "main", Agent: "test"})
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var reused storage.ReviewJob
	testutil.DecodeJSON(t, w, &reused)
	assert.Equal(t, "prior-session", reused.SessionID, "control: same review resumes when isolation is disabled")
	assert.Equal(t, prior.UUID, reused.ResumeSourceJobUUID)
}
