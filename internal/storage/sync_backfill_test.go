package storage

import (
	"database/sql"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackfillSourceMachineID(t *testing.T) {
	h := newSyncTestHelper(t)

	job := h.createPendingJob("abc123")

	// Verify source_machine_id is initially NULL (simulating legacy data)
	var sourceMachineID *string
	err := h.db.QueryRow(`SELECT source_machine_id FROM review_jobs WHERE id = ?`, job.ID).Scan(&sourceMachineID)
	require.NoError(t, err, "Query failed")
	// After migration, backfill runs automatically, so it may already have a value
	// Let's clear it to test the backfill
	h.clearSourceMachineID(job.ID)

	// Run backfill
	err = h.db.BackfillSourceMachineID()
	require.NoError(t, err, "BackfillSourceMachineID failed")

	// Verify source_machine_id is now set
	var newSourceMachineID string
	err = h.db.QueryRow(`SELECT source_machine_id FROM review_jobs WHERE id = ?`, job.ID).Scan(&newSourceMachineID)
	require.NoError(t, err, "Query after backfill failed")

	machineID, _ := h.db.GetMachineID()
	require.Equal(t, machineID, newSourceMachineID,
		"Expected source_machine_id %q, got %q", machineID, newSourceMachineID)
}

func TestBackfillRepoIdentities_LocalRepoFallback(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create a git repo without a remote configured
	tempDir := t.TempDir()
	cmd := exec.Command("git", "init", tempDir)
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed (git not available?): %v", err)
	}

	repo, err := db.GetOrCreateRepo(tempDir)
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	// Clear identity to simulate legacy repo
	h := &syncTestHelper{t: t, db: db}
	h.clearRepoIdentity(repo.ID)

	// Backfill should use local: prefix with repo name (git repo, no remote)
	count, err := db.BackfillRepoIdentities()
	require.NoError(t, err, "BackfillRepoIdentities failed: %v")

	assert.Equal(t, 1, count)

	// Verify identity was set with local: prefix
	var identity string
	err = db.QueryRow(`SELECT identity FROM repos WHERE id = ?`, repo.ID).Scan(&identity)
	require.NoError(t, err, "Query identity failed: %v")

	expectedPrefix := "local:"
	assert.True(t, strings.HasPrefix(identity, expectedPrefix))

	// The identity should contain the repo name (last component of path)
	assert.Contains(t, identity, repo.Name)
}

func TestBackfillRepoIdentities_SkipsNonGitRepos(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create a repo pointing to a directory that is NOT a git repository
	tempDir := t.TempDir() // Just a plain directory, no git init
	repo, err := db.GetOrCreateRepo(tempDir)
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	// Clear identity to simulate legacy repo
	h := &syncTestHelper{t: t, db: db}
	h.clearRepoIdentity(repo.ID)

	// Backfill should set a local:// identity for non-git repos
	count, err := db.BackfillRepoIdentities()
	require.NoError(t, err, "BackfillRepoIdentities failed: %v")

	assert.Equal(t, 1, count)

	// Verify identity is set to local:// prefix
	var identity sql.NullString
	err = db.QueryRow(`SELECT identity FROM repos WHERE id = ?`, repo.ID).Scan(&identity)
	require.NoError(t, err, "Query identity failed: %v")

	assert.False(t, !identity.Valid || !strings.HasPrefix(identity.String, "local://"))
}

func TestBackfillRepoIdentities_SkipsReposWithIdentity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create a repo and set its identity
	tempDir := t.TempDir()
	repo, err := db.GetOrCreateRepo(tempDir)
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	err = db.SetRepoIdentity(repo.ID, "https://github.com/user/existing.git")
	require.NoError(t, err, "SetRepoIdentity failed: %v")

	// Backfill should not change existing identity
	count, err := db.BackfillRepoIdentities()
	require.NoError(t, err, "BackfillRepoIdentities failed: %v")

	assert.Equal(t, 0, count)

	// Verify identity unchanged
	var identity string
	err = db.QueryRow(`SELECT identity FROM repos WHERE id = ?`, repo.ID).Scan(&identity)
	require.NoError(t, err, "Query identity failed: %v")

	assert.Equal(t, "https://github.com/user/existing.git", identity)
}

func TestBackfillRepoIdentities_SkipsMissingPaths(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create a repo pointing to a non-existent path (subpath of temp dir that doesn't exist)
	nonExistentPath := filepath.Join(t.TempDir(), "this-subdir-does-not-exist", "nested")
	_, err := db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES (?, ?, NULL)`,
		nonExistentPath, "missing-repo")
	require.NoError(t, err, "Insert repo failed: %v")

	// Get the repo ID
	var repoID int64
	err = db.QueryRow(`SELECT id FROM repos WHERE root_path = ?`, nonExistentPath).Scan(&repoID)
	require.NoError(t, err, "Get repo ID failed: %v")

	// Backfill should still set a local:// identity for missing paths
	// (better to have an identity than none, even for stale entries)
	count, err := db.BackfillRepoIdentities()
	require.NoError(t, err, "BackfillRepoIdentities failed: %v")

	assert.Equal(t, 1, count)

	// Verify identity is set to local:// prefix
	var identity sql.NullString
	err = db.QueryRow(`SELECT identity FROM repos WHERE id = ?`, repoID).Scan(&identity)
	require.NoError(t, err, "Query identity failed: %v")

	assert.False(t, !identity.Valid || !strings.HasPrefix(identity.String, "local://"))
}

func TestUpsertPulledJob_BackfillsModel(t *testing.T) {
	// This test verifies that upserting a pulled job with a model value backfills
	// an existing job that has NULL model (COALESCE behavior in SQLite)
	db := openTestDB(t)
	defer db.Close()

	// Create a repo
	repo, err := db.GetOrCreateRepo("/test/repo")
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	// Insert a job with NULL model using EnqueueJob (which sets model to empty string by default)
	// We need to directly insert with NULL model to test the COALESCE behavior
	jobUUID := "test-uuid-backfill-" + time.Now().Format("20060102150405")
	_, err = db.Exec(`
		INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, enqueued_at)
		VALUES (?, ?, 'HEAD', 'test-agent', 'done', datetime('now'))
	`, jobUUID, repo.ID)
	require.NoError(t, err, "Failed to insert job with NULL model: %v")

	// Verify model is NULL
	var modelBefore sql.NullString
	err = db.QueryRow(`SELECT model FROM review_jobs WHERE uuid = ?`, jobUUID).Scan(&modelBefore)
	require.NoError(t, err, "Failed to query model before: %v")

	assert.False(t, modelBefore.Valid)

	// Upsert with a model value - should backfill
	pulledJob := PulledJob{
		UUID:            jobUUID,
		RepoIdentity:    "/test/repo",
		GitRef:          "HEAD",
		Agent:           "test-agent",
		Model:           "gpt-4", // Now providing a model
		Status:          "done",
		SourceMachineID: "test-machine",
		EnqueuedAt:      time.Now(),
		UpdatedAt:       time.Now(),
	}
	err = db.UpsertPulledJob(pulledJob, repo.ID, nil)
	require.NoError(t, err, "UpsertPulledJob failed: %v")

	// Verify model was backfilled
	var modelAfter sql.NullString
	err = db.QueryRow(`SELECT model FROM review_jobs WHERE uuid = ?`, jobUUID).Scan(&modelAfter)
	require.NoError(t, err, "Failed to query model after: %v")

	if !modelAfter.Valid {
		assert.True(t, modelAfter.Valid, "Expected model to be backfilled, but it's still NULL")
		return
	}
	assert.Equal(t, "gpt-4", modelAfter.String)

	// Upserting with empty model should clear the existing effective model so
	// synced reruns can propagate implicit/default state.
	pulledJob.Model = ""
	err = db.UpsertPulledJob(pulledJob, repo.ID, nil)
	require.NoError(t, err, "UpsertPulledJob (empty model) failed: %v")

	var modelCleared sql.NullString
	err = db.QueryRow(`SELECT model FROM review_jobs WHERE uuid = ?`, jobUUID).Scan(&modelCleared)
	require.NoError(t, err, "Failed to query model cleared: %v")
	assert.False(t, modelCleared.Valid)
}

func TestGetJobsToSync_IncludesWorktreePath(t *testing.T) {
	h := newSyncTestHelper(t)

	// Enqueue a job with a worktree path
	commit, err := h.db.GetOrCreateCommit(h.repo.ID, "wt-sync-abc", "Author", "Subject", time.Now())
	require.NoError(t, err)

	job, err := h.db.EnqueueJob(EnqueueOpts{
		RepoID:       h.repo.ID,
		CommitID:     commit.ID,
		GitRef:       "wt-sync-abc",
		Agent:        "test",
		WorktreePath: "/worktrees/feature-branch",
	})
	require.NoError(t, err)

	// Complete the job so it becomes syncable
	claimed, err := h.db.ClaimJob("w-sync-wt")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, h.db.CompleteJob(job.ID, "test", "prompt", "PASS"))

	jobs, err := h.db.GetJobsToSync(h.machineID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, jobs)

	var found *SyncableJob
	for i := range jobs {
		if jobs[i].UUID == job.UUID {
			found = &jobs[i]
			break
		}
	}
	require.NotNil(t, found, "expected job %s in sync results", job.UUID)
	assert.Equal(t, "/worktrees/feature-branch", found.WorktreePath)
}

func TestGetJobsToSync_IncludesSource(t *testing.T) {
	h := newSyncTestHelper(t)

	commit, err := h.db.GetOrCreateCommit(h.repo.ID, "source-sync-abc", "Author", "Subject", time.Now())
	require.NoError(t, err)

	job, err := h.db.EnqueueJob(EnqueueOpts{
		RepoID:   h.repo.ID,
		CommitID: commit.ID,
		GitRef:   "source-sync-abc",
		Agent:    "test",
		Source:   JobSourceCI,
	})
	require.NoError(t, err)

	claimed, err := h.db.ClaimJob("w-sync-source")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, h.db.CompleteJob(job.ID, "test", "prompt", "PASS"))

	jobs, err := h.db.GetJobsToSync(h.machineID, 10)
	require.NoError(t, err)

	var found *SyncableJob
	for i := range jobs {
		if jobs[i].UUID == job.UUID {
			found = &jobs[i]
			break
		}
	}
	require.NotNil(t, found, "expected job %s in sync results", job.UUID)
	assert.Equal(t, JobSourceCI, found.Source)
}

// TestUpsertPulledJob_TerminalRerunClearsStaleCost guards the priced-to-unpriced
// rerun across sync. A cleared token_usage binds as NULL, so a plain COALESCE
// upsert would preserve a prior attempt's cost; since command_line is not synced,
// the cost fallback in costEligible would then count that stale value. A terminal
// pull must overwrite token_usage, while a non-terminal (requeued) pull preserves
// it until the re-attempt completes.
func TestUpsertPulledJob_TerminalRerunClearsStaleCost(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/test/repo-cost-sync")
	require.NoError(t, err)

	base := time.Now().UTC()
	uuid := "cost-sync-rerun-uuid"
	priced := PulledJob{
		UUID:            uuid,
		RepoIdentity:    "/test/repo-cost-sync",
		GitRef:          "HEAD",
		Agent:           "codex",
		Status:          string(JobStatusDone),
		TokenUsage:      `{"cost_usd":2.50,"has_cost":true}`,
		SourceMachineID: "machine-a",
		EnqueuedAt:      base,
		UpdatedAt:       base,
	}
	require.NoError(t, db.UpsertPulledJob(priced, repo.ID, nil))

	tokenUsage := func() string {
		var tu string
		require.NoError(t, db.QueryRow(
			`SELECT COALESCE(token_usage, '') FROM review_jobs WHERE uuid = ?`, uuid).Scan(&tu))
		return tu
	}
	require.Contains(t, tokenUsage(), "2.50", "priced cost stored on first pull")

	// A newer non-terminal (requeued) pull must not wipe a recorded cost: the
	// re-attempt has not completed, so the prior cost stands until it does.
	requeued := priced
	requeued.Status = string(JobStatusQueued)
	requeued.TokenUsage = ""
	requeued.UpdatedAt = base.Add(1 * time.Minute)
	require.NoError(t, db.UpsertPulledJob(requeued, repo.ID, nil))
	assert.Contains(tokenUsage(), "2.50", "queued rerun pull preserves the prior cost")

	// A newer terminal pull that reports no cost clears the stale value.
	rerunDone := priced
	rerunDone.TokenUsage = ""
	rerunDone.UpdatedAt = base.Add(2 * time.Minute)
	require.NoError(t, db.UpsertPulledJob(rerunDone, repo.ID, nil))
	assert.Empty(tokenUsage(), "terminal unpriced rerun pull clears the stale cost")
}

func TestUpsertPulledJob_PreservesWorktreePath(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/test/repo-wt-sync")
	require.NoError(t, err)

	jobUUID := "test-uuid-wt-" + time.Now().Format("20060102150405")

	pulledJob := PulledJob{
		UUID:            jobUUID,
		RepoIdentity:    "/test/repo-wt-sync",
		GitRef:          "HEAD",
		Agent:           "test-agent",
		Status:          "done",
		WorktreePath:    "/worktrees/my-branch",
		SourceMachineID: "test-machine",
		EnqueuedAt:      time.Now(),
		UpdatedAt:       time.Now(),
	}
	err = db.UpsertPulledJob(pulledJob, repo.ID, nil)
	require.NoError(t, err)

	// Verify worktree_path was stored
	var wt string
	err = db.QueryRow(
		`SELECT COALESCE(worktree_path, '') FROM review_jobs WHERE uuid = ?`, jobUUID,
	).Scan(&wt)
	require.NoError(t, err)
	assert.Equal(t, "/worktrees/my-branch", wt)

	// Upsert with empty worktree_path should not clear existing value
	pulledJob.WorktreePath = ""
	err = db.UpsertPulledJob(pulledJob, repo.ID, nil)
	require.NoError(t, err)

	err = db.QueryRow(
		`SELECT COALESCE(worktree_path, '') FROM review_jobs WHERE uuid = ?`, jobUUID,
	).Scan(&wt)
	require.NoError(t, err)
	assert.Equal(t, "/worktrees/my-branch", wt)
}

func TestUpsertPulledJob_PreservesSource(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/test/repo-source-sync")
	require.NoError(t, err)

	jobUUID := "test-uuid-source-" + time.Now().Format("20060102150405")
	pulledJob := PulledJob{
		UUID:            jobUUID,
		RepoIdentity:    "/test/repo-source-sync",
		GitRef:          "HEAD",
		Agent:           "test-agent",
		Status:          "done",
		Source:          JobSourceCI,
		SourceMachineID: "test-machine",
		EnqueuedAt:      time.Now(),
		UpdatedAt:       time.Now(),
	}
	require.NoError(t, db.UpsertPulledJob(pulledJob, repo.ID, nil))

	var source string
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(source, '') FROM review_jobs WHERE uuid = ?`, jobUUID,
	).Scan(&source))
	assert.Equal(t, JobSourceCI, source)

	pulledJob.Source = ""
	require.NoError(t, db.UpsertPulledJob(pulledJob, repo.ID, nil))
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(source, '') FROM review_jobs WHERE uuid = ?`, jobUUID,
	).Scan(&source))
	assert.Equal(t, JobSourceCI, source)
}

func TestUpsertPulledJob_ClearsModelAndProviderFields(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/test/repo-sync-clear")
	require.NoError(t, err)

	jobUUID := "test-uuid-clear-" + time.Now().Format("20060102150405")
	pulledJob := PulledJob{
		UUID:              jobUUID,
		RepoIdentity:      "/test/repo-sync-clear",
		GitRef:            "HEAD",
		Agent:             "test-agent",
		Model:             "gpt-4",
		Provider:          "openai",
		RequestedModel:    "gpt-4",
		RequestedProvider: "openai",
		Status:            "done",
		SourceMachineID:   "test-machine",
		EnqueuedAt:        time.Now(),
		UpdatedAt:         time.Now(),
	}
	require.NoError(t, db.UpsertPulledJob(pulledJob, repo.ID, nil))

	pulledJob.Model = ""
	pulledJob.Provider = ""
	pulledJob.RequestedModel = ""
	pulledJob.RequestedProvider = ""
	pulledJob.UpdatedAt = time.Now().Add(time.Second)
	require.NoError(t, db.UpsertPulledJob(pulledJob, repo.ID, nil))

	var model, provider, requestedModel, requestedProvider *string
	err = db.QueryRow(`SELECT model, provider, requested_model, requested_provider FROM review_jobs WHERE uuid = ?`, jobUUID).Scan(&model, &provider, &requestedModel, &requestedProvider)
	require.NoError(t, err)
	assert.Nil(t, model)
	assert.Nil(t, provider)
	assert.Nil(t, requestedModel)
	assert.Nil(t, requestedProvider)
}
