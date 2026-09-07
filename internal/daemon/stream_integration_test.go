//go:build integration

package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestStreamEventsMethodNotAllowed(t *testing.T) {
	db := testutil.OpenTestDB(t)

	cfg := config.DefaultConfig()
	server := NewServer(db, cfg, "")

	req := httptest.NewRequest("POST", "/api/stream/events", nil)
	rec := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		assert.Condition(t, func() bool {
			return false
		}, "Expected status 405, got %d", rec.Code)
	}
}

func setupBroadcaster(t *testing.T) (Broadcaster, <-chan Event) {
	t.Helper()
	broadcaster := NewBroadcaster()
	_, eventCh := broadcaster.Subscribe("")
	return broadcaster, eventCh
}

func setupRepoAndJob(t *testing.T, db *storage.DB, tmpDir string) *storage.ReviewJob {
	t.Helper()
	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)
	sha := testutil.GetHeadSHA(t, repoDir)

	repo, err := db.GetOrCreateRepo(repoDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, sha, testutil.GitUserName, "test commit", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:         repo.ID,
		CommitID:       commit.ID,
		GitRef:         sha,
		Agent:          "test",
		Prompt:         "prebuilt review prompt",
		PromptPrebuilt: true,
		JobType:        storage.JobTypeReview,
	})
	require.NoError(t, err)
	return job
}

func TestBroadcasterIntegrationWithWorker(t *testing.T) {
	db, tmpDir := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()

	broadcaster, eventCh := setupBroadcaster(t)
	job := setupRepoAndJob(t, db, tmpDir)

	pool := NewWorkerPool(db, NewStaticConfig(cfg), 1, broadcaster, nil, nil)
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	pool.processJob(testWorkerID, claimed)

	finalJob, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, finalJob.Status)

	// processJob has returned, so all completion events have been published.
	var completed Event
	for len(eventCh) > 0 {
		event := <-eventCh
		if event.Type == "review.completed" {
			completed = event
		}
	}
	require.Equal(t, "review.completed", completed.Type)
	assert.Equal(t, job.ID, completed.JobID)
	assert.Equal(t, "test", completed.Agent)
	assert.NotEmpty(t, completed.Verdict)
}
