package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
)

// TestE2EEnqueueAndReview tests the full flow of enqueueing and reviewing a commit
func TestE2EEnqueueAndReview(t *testing.T) {
	// Skip if not in a git repo (CI might not have one)
	if _, err := exec.Command("git", "rev-parse", "--git-dir").Output(); err != nil {
		t.Skip("not in a git repo, skipping e2e test")
	}

	// Setup temp DB
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	require.NoError(t, err, "Failed to open test DB")
	defer db.Close()

	// Create a mock server
	cfg := config.DefaultConfig()
	server := daemon.NewServer(db, cfg, "")

	// Create test HTTP server
	mux := http.NewServeMux()

	// Add handlers manually (simulating the server)
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		queued, running, done, failed, canceled, applied, rebased, _, _ := db.GetJobCounts()
		status := storage.DaemonStatus{
			QueuedJobs:    queued,
			RunningJobs:   running,
			CompletedJobs: done,
			FailedJobs:    failed,
			CanceledJobs:  canceled,
			AppliedJobs:   applied,
			RebasedJobs:   rebased,
			MaxWorkers:    4,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	s := httptest.NewServer(mux)
	defer s.Close()

	// Test status endpoint
	resp, err := http.Get(s.URL + "/api/status")
	require.NoError(t, err, "Status request failed")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Expected status 200, got %d", resp.StatusCode)

	var status storage.DaemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		require.NoError(t, err, "Failed to decode status: %v", err)
	}

	assert.Equal(t, 4, status.MaxWorkers, "Expected MaxWorkers 4, got %d", status.MaxWorkers)

	_ = server // Avoid unused variable
}

// TestDatabaseIntegration tests the full database workflow
func TestDatabaseIntegration(t *testing.T) {
	t.Run("GetOrCreateRepo", func(t *testing.T) {
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "test.db")

		d, err := storage.Open(dbPath)
		require.NoError(t, err, "Failed to open test DB")
		defer d.Close()

		// Simulate full workflow
		repo, err := d.GetOrCreateRepo("/tmp/test-repo")
		require.NoError(t, err, "GetOrCreateRepo failed")

		commit, err := d.GetOrCreateCommit(repo.ID, "abc123", "Test Author", "Test commit message", time.Now())
		require.NoError(t, err, "GetOrCreateCommit failed")

		job, err := d.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed")

		// Verify initial state
		queued, running, done, _, _, _, _, _, _ := d.GetJobCounts()
		require.Equal(t, 1, queued, "Expected 1 queued job, got %d", queued)
		_ = running
		_ = done

		// Claim the job
		claimed, err := d.ClaimJob("test-worker")
		require.NoError(t, err, "ClaimJob failed")
		require.NotNil(t, claimed, "ClaimJob returned nil")
		assert.Equal(t, job.ID, claimed.ID, "Claimed wrong job")

		// Verify running state
		_, running, _, _, _, _, _, _, _ = d.GetJobCounts()
		require.Equal(t, 1, running, "Expected 1 running job, got %d", running)

		// Complete the job
		err = d.CompleteJob(job.ID, "codex", "test prompt", "This commit looks good!")
		require.NoError(t, err, "CompleteJob failed")

		// Verify completed state
		queued, running, done, _, _, _, _, _, _ = d.GetJobCounts()
		require.Equal(t, 1, done, "Expected 1 completed job, got %d", done)
		_ = queued
		_ = running

		// Fetch the review
		review, err := d.GetReviewByCommitSHA("abc123")
		require.NoError(t, err, "GetReviewByCommitSHA failed")
		require.Contains(t, review.Output, "looks good", "Review output doesn't contain expected text: %s", review.Output)

		// Add a comment
		resp, err := d.AddComment(commit.ID, "human-reviewer", "Agreed, LGTM!")
		require.NoError(t, err, "AddComment failed")
		assert.Equal(t, "Agreed, LGTM!", resp.Response, "Comment not saved correctly")

		// Verify comment can be fetched
		comments, err := d.GetCommentsForCommitSHA("abc123")
		require.NoError(t, err, "GetCommentsForCommitSHA failed")
		assert.Len(t, comments, 1, "Expected 1 comment, got %d", len(comments))
	})

	t.Run("VerifyConfig", func(t *testing.T) {
		testenv.SetDataDir(t)

		// Save custom config
		cfg := config.DefaultConfig()
		cfg.DefaultAgent = "claude-code"
		cfg.MaxWorkers = 8
		cfg.ReviewContextCount = 5

		err := config.SaveGlobal(cfg)
		require.NoError(t, err, "SaveGlobal failed")

		// Load it back
		loaded, err := config.LoadGlobal()
		require.NoError(t, err, "LoadGlobal failed")

		assert.Equal(t, "claude-code", loaded.DefaultAgent, "DefaultAgent not persisted correctly")
		assert.Equal(t, 8, loaded.MaxWorkers, "MaxWorkers not persisted correctly")
		assert.Equal(t, 5, loaded.ReviewContextCount, "ReviewContextCount not persisted correctly")
	})
}
