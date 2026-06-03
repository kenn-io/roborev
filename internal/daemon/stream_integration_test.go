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

func waitForEventType(t *testing.T, ch <-chan Event, eventType string, timeout time.Duration) Event {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case e, ok := <-ch:
			if !ok {
				require.Condition(t, func() bool {
					return false
				}, "channel closed waiting for event type: %s", eventType)
				return Event{}
			}
			if e.Type == eventType {
				return e
			}
		case <-timer.C:
			require.Condition(t, func() bool {
				return false
			}, "timed out waiting for event type: %s", eventType)
			return Event{}
		}
	}
}

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

	// Create worker pool with our broadcaster
	pool := NewWorkerPool(db, NewStaticConfig(cfg), 1, broadcaster, nil, nil)
	pool.Start()
	defer pool.Stop()

	// Wait for job to finish (done or failed)
	finalJob := testutil.WaitForJobStatus(t, db, job.ID, 10*time.Second, storage.JobStatusDone, storage.JobStatusFailed)

	if finalJob.Status != storage.JobStatusDone {
		require.Condition(t, func() bool {
			return false
		}, "Expected job to succeed, got status %s", finalJob.Status)
	}

	// Drain events until we find the review.completed event (bounded to prevent misleading timeout errors)
	event := waitForEventType(t, eventCh, "review.completed", 2*time.Second)

	if event.JobID != job.ID {
		assert.Condition(t, func() bool {
			return false
		}, "Expected JobID %d, got %d", job.ID, event.JobID)
	}
	if event.Agent != "test" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected agent 'test', got %s", event.Agent)
	}
	if event.Verdict == "" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected verdict to be set")
	}
}
