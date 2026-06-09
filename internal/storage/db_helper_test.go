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

// setJobAgentInvoked marks a job as having actually invoked an agent, which is
// what cost eligibility keys on. Cost-eligible test jobs must set it; no-agent
// rows leave it unset. It writes the marker directly (like the other seed
// helpers) rather than through MarkJobAgentInvoked: that method scopes the write
// to the running attempt (status='running' + worker_id), but cost tests seed
// terminal rows that no longer satisfy that scope. The scoping itself is covered
// by TestMarkJobAgentInvoked_StaleWorkerIgnored.
func setJobAgentInvoked(t *testing.T, db *DB, jobID int64) {
	t.Helper()
	_, err := db.Exec(
		`UPDATE review_jobs SET command_line = ?, agent_invoked = 1 WHERE id = ?`,
		fmt.Sprintf("test-agent review %d", jobID), jobID)
	require.NoError(t, err, "failed to mark job agent-invoked")
}

// getJobAgentInvoked reads the raw agent_invoked marker for a job. The
// ReviewJob model does not expose it (it gates cost eligibility, it is not a
// display field), so attempt-reset tests read the column directly.
func getJobAgentInvoked(t *testing.T, db *DB, jobID int64) bool {
	t.Helper()
	var invoked int
	require.NoError(t, db.QueryRow(
		`SELECT agent_invoked FROM review_jobs WHERE id = ?`, jobID).Scan(&invoked),
		"failed to read agent_invoked")
	return invoked == 1
}

// seedCost prices a job the way the worker does: an agent ran (agent_invoked
// set) and token usage is written scoped to the attempt's captured session, so
// the helper assigns a session id (via setJobSession, defined in
// reviews_test.go) and stores the usage blob against it.
func seedCost(t *testing.T, db *DB, jobID int64, tokenUsageJSON string) {
	t.Helper()
	setJobAgentInvoked(t, db, jobID)
	sessionID := fmt.Sprintf("sess-%d", jobID)
	setJobSession(t, db, jobID, sessionID)
	require.NoError(t, db.SaveJobTokenUsage(jobID, sessionID, tokenUsageJSON))
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
