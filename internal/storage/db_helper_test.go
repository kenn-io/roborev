package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	templateOnce sync.Once
	templatePath string
	templateErr  error
)

func getTemplatePath() (string, error) {
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "roborev-test-template-*")
		if err != nil {
			templateErr = err
			return
		}
		p := filepath.Join(dir, "template.db")
		db, err := Open(p)
		if err != nil {
			templateErr = err
			return
		}
		db.Close()
		templatePath = p
	})
	return templatePath, templateErr
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	tmpl, err := getTemplatePath()
	require.NoError(t, err, "Failed to create template DB: %v")

	data, err := os.ReadFile(tmpl)
	require.NoError(t, err, "Failed to read template DB: %v")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	err = os.WriteFile(dbPath, data, 0o644)
	require.NoError(t, err, "Failed to write test DB: %v")

	db, err := Open(dbPath)
	require.NoError(t, err, "Failed to open test DB: %v")

	return db
}

func createRepo(t *testing.T, db *DB, path string) *Repo {
	t.Helper()
	repo, err := db.GetOrCreateRepo(path)
	require.NoError(t, err, "Failed to create repo: %v")

	return repo
}

func createCommit(t *testing.T, db *DB, repoID int64, sha string) *Commit {
	t.Helper()
	commit, err := db.GetOrCreateCommit(repoID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err, "Failed to create commit: %v")

	return commit
}

func enqueueJob(t *testing.T, db *DB, repoID, commitID int64, sha string) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repoID, CommitID: commitID, GitRef: sha, Agent: "codex"})
	require.NoError(t, err, "Failed to enqueue job: %v")

	return job
}

func claimJob(t *testing.T, db *DB, workerID string) *ReviewJob {
	t.Helper()
	job, err := db.ClaimJob(workerID)
	require.NoError(t, err, "Failed to claim job: %v")
	assert.NotNil(t, job, "Expected to claim a job, got nil")

	return job
}

func mustEnqueuePromptJob(t *testing.T, db *DB, opts EnqueueOpts) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(opts)
	require.NoError(t, err, "Failed to enqueue prompt job: %v")

	return job
}

func setJobStatus(t *testing.T, db *DB, jobID int64, status JobStatus) {
	t.Helper()
	var query string
	switch status {
	case JobStatusQueued:
		query = `UPDATE review_jobs SET status = 'queued', started_at = NULL, finished_at = NULL, error = NULL WHERE id = ?`
	case JobStatusRunning:
		query = `UPDATE review_jobs SET status = 'running', started_at = datetime('now') WHERE id = ?`
	case JobStatusDone:
		query = `UPDATE review_jobs SET status = 'done', started_at = datetime('now'), finished_at = datetime('now') WHERE id = ?`
	case JobStatusFailed:
		query = `UPDATE review_jobs SET status = 'failed', started_at = datetime('now'), finished_at = datetime('now'), error = 'test error' WHERE id = ?`
	case JobStatusCanceled:
		query = `UPDATE review_jobs SET status = 'canceled', started_at = datetime('now'), finished_at = datetime('now') WHERE id = ?`
	default:
		require.Condition(t, func() bool {
			return false
		}, "Unknown job status: %s", status)
	}
	res, err := db.Exec(query, jobID)
	require.NoError(t, err, "Failed to set job status to %s: %v", status)

	rows, err := res.RowsAffected()
	require.NoError(t, err, "Failed to get rows affected: %v")

	assert.EqualValues(t, 1, rows)
}

func backdateJobStart(t *testing.T, db *DB, jobID int64, d time.Duration) {
	t.Helper()
	startTime := time.Now().Add(-d).UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', started_at = ? WHERE id = ?`, startTime, jobID)
	require.NoError(t, err, "failed to backdate job: %v")
}

func backdateJobStartWithOffset(t *testing.T, db *DB, jobID int64, d time.Duration, loc *time.Location) {
	t.Helper()
	startTime := time.Now().Add(-d).In(loc).Format(time.RFC3339)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', started_at = ? WHERE id = ?`, startTime, jobID)
	require.NoError(t, err, "failed to backdate job with offset: %v")
}

func setJobBranch(t *testing.T, db *DB, jobID int64, branch string) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET branch = ? WHERE id = ?`, branch, jobID)
	require.NoError(t, err, "failed to set job branch: %v")
}

func createJobChain(t *testing.T, db *DB, repoPath, sha string) (*Repo, *Commit, *ReviewJob) {
	t.Helper()
	repo := createRepo(t, db, repoPath)
	commit := createCommit(t, db, repo.ID, sha)
	job := enqueueJob(t, db, repo.ID, commit.ID, sha)
	return repo, commit, job
}

// seedJobs creates a repo at repoPath and enqueues n jobs for it,
// returning the repo and the list of created jobs.
func seedJobs(t *testing.T, db *DB, repoPath string, n int) (*Repo, []*ReviewJob) {
	t.Helper()
	repo := createRepo(t, db, repoPath)
	jobs := make([]*ReviewJob, n)
	for i := range n {
		sha := fmt.Sprintf("%s-sha%d", filepath.Base(repoPath), i)
		commit := createCommit(t, db, repo.ID, sha)
		jobs[i] = enqueueJob(t, db, repo.ID, commit.ID, sha)
	}
	return repo, jobs
}
