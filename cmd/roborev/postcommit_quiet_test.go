package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestPostCommitQuietPeriodSchedulesAndFlushes(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	requests := make(chan daemon.EnqueueRequest, 10)
	schedules := make(chan struct{ RepoPath, Branch, Head string }, 10)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/api/post-commit/schedule", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RepoPath string `json:"repo_path"`
			Branch   string `json:"branch"`
			Head     string `json:"head"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		schedules <- struct{ RepoPath, Branch, Head string }{req.RepoPath, req.Branch, req.Head}
		w.WriteHeader(http.StatusNoContent)
	})
	writeRoborevConfig(t, repo, `post_commit_quiet_period = "5m"`)
	repo.CommitFile("base.txt", "base", "base")
	base := repo.Run("rev-parse", "HEAD")
	repo.CommitFile("one.txt", "one", "one")
	first := repo.Run("rev-parse", "HEAD")
	_, stderr, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	require.Empty(t, stderr)
	require.Len(t, schedules, 1)
	schedule := <-schedules
	assert.Equal(t, first, schedule.Head)
	assert.Equal(t, repo.Dir, schedule.RepoPath)
	assert.Empty(t, requests)

	repo.CommitFile("two.txt", "two", "two")
	second := repo.Run("rev-parse", "HEAD")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	require.Len(t, schedules, 1)
	assert.Equal(t, second, (<-schedules).Head)
	// A timer for the first commit must not flush the newer batch.
	_, _, err = executePostCommitCmd("--repo", repo.Dir, "--flush", "--flush-branch", schedule.Branch, "--quiet-head", first)
	require.NoError(t, err)
	assert.Empty(t, requests)
	// The current timer flushes the entire accumulated range exactly once.
	_, _, err = executePostCommitCmd("--repo", repo.Dir, "--flush", "--flush-branch", schedule.Branch, "--quiet-head", second)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, base+".."+second, (<-requests).GitRef)
	_, _, err = executePostCommitCmd("--repo", repo.Dir, "--flush", "--flush-branch", schedule.Branch, "--quiet-head", second)
	require.NoError(t, err)
	assert.Empty(t, requests)
}

func TestPostCommitQuietPeriodCountAndPushFlush(t *testing.T) {
	for _, trigger := range []string{"count", "push"} {
		t.Run(trigger, func(t *testing.T) {
			repo, mux := setupTestEnvironment(t)
			requests := make(chan daemon.EnqueueRequest, 10)
			mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
				var req daemon.EnqueueRequest
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				requests <- req
				w.WriteHeader(http.StatusCreated)
			})
			mux.HandleFunc("/api/post-commit/schedule", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			settings := `post_commit_quiet_period = "5m"`
			if trigger == "count" {
				settings += "\npost_commit_batch_size = 2"
			}
			writeRoborevConfig(t, repo, settings)
			repo.CommitFile("base.txt", "base", "base")
			base := repo.Run("rev-parse", "HEAD")
			repo.CommitFile("one.txt", "one", "one")
			_, _, err := executePostCommitCmd("--repo", repo.Dir)
			require.NoError(t, err)
			assert.Empty(t, requests)
			if trigger == "count" {
				repo.CommitFile("two.txt", "two", "two")
				_, _, err = executePostCommitCmd("--repo", repo.Dir)
			} else {
				branch := repo.Run("branch", "--show-current")
				head := repo.Run("rev-parse", "HEAD")
				flushPushedBranch(t.Context(), repo.Dir, branch, head)
			}
			require.NoError(t, err)
			require.Len(t, requests, 1)
			assert.Equal(t, base+".."+repo.Run("rev-parse", "HEAD"), (<-requests).GitRef)
		})
	}
}

func TestPostCommitQuietPeriodScheduleFailureEnqueuesRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	requests := mockEnqueue(t, mux)
	mux.HandleFunc("/api/post-commit/schedule", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	writeRoborevConfig(t, repo, `post_commit_quiet_period = "5m"`)
	repo.CommitFile("base.txt", "base", "base")
	base := repo.Run("rev-parse", "HEAD")
	repo.CommitFile("one.txt", "one", "one")
	_, stderr, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	require.Len(t, requests, 1)
	assert.Equal(t, base+".."+repo.Run("rev-parse", "HEAD"), (<-requests).GitRef)
}
