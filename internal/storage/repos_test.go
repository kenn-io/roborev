package storage

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupDBAndRepo is a helper that opens a test database and creates a repository.
func setupDBAndRepo(t *testing.T, name string) (*DB, *Repo) {
	t.Helper()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, filepath.Join(t.TempDir(), name))
	return db, repo
}

// completeTestJob is a helper that claims and completes a job.
func completeTestJob(t *testing.T, db *DB, jobID int64, output string) {
	t.Helper()
	claimJob(t, db, "worker-1")
	if err := db.CompleteJob(jobID, "codex", "prompt", output); err != nil {
		require.NoError(t, err, "CompleteJob failed: %v")
	}
}

func TestEnqueuePromptJob(t *testing.T) {
	tests := []struct {
		name        string
		opts        EnqueueOpts
		wantJob     func(*testing.T, *ReviewJob)
		checkClaim  bool
		wantClaimed func(*testing.T, *ReviewJob)
		checkSQL    func(*testing.T, *DB, int64)
	}{
		{
			name: "creates job with custom prompt",
			opts: EnqueueOpts{
				Agent:     "claude-code",
				Reasoning: "thorough",
				Prompt:    "Explain the architecture of this codebase",
			},
			wantJob: func(t *testing.T, j *ReviewJob) {
				assert.Equal(t, "prompt", j.GitRef)
				assert.Equal(t, "claude-code", j.Agent)
				assert.Equal(t, "thorough", j.Reasoning)
				assert.Equal(t, "Explain the architecture of this codebase", j.Prompt)
				assert.Equal(t, JobStatusQueued, j.Status)
			},
			checkSQL: func(t *testing.T, db *DB, jobID int64) {
				var gitRef string
				err := db.QueryRow("SELECT git_ref FROM review_jobs WHERE id = ?", jobID).Scan(&gitRef)
				require.NoError(t, err, "Failed to query git_ref: %v")

				assert.Equal(t, "prompt", gitRef)
			},
		},
		{
			name: "defaults reasoning to thorough",
			opts: EnqueueOpts{
				Agent:  "codex",
				Prompt: "test prompt",
			},
			wantJob: func(t *testing.T, j *ReviewJob) {
				assert.Equal(t, "thorough", j.Reasoning)
			},
		},
		{
			name: "claimed job has prompt loaded",
			opts: EnqueueOpts{
				Agent:     "claude-code",
				Reasoning: "standard",
				Prompt:    "Find security issues in the codebase",
			},
			checkClaim: true,
			wantClaimed: func(t *testing.T, j *ReviewJob) {
				assert.Equal(t, "prompt", j.GitRef)
				assert.Equal(t, "Find security issues in the codebase", j.Prompt)
			},
		},
		{
			name: "agentic flag persists and is claimed correctly",
			opts: EnqueueOpts{
				Agent:   "claude-code",
				Prompt:  "Test agentic prompt",
				Agentic: true,
			},
			wantJob: func(t *testing.T, j *ReviewJob) {
				assert.True(t, j.Agentic)
			},
			checkClaim: true,
			wantClaimed: func(t *testing.T, j *ReviewJob) {
				assert.True(t, j.Agentic)
			},
			checkSQL: func(t *testing.T, db *DB, jobID int64) {
				var agentic bool
				err := db.QueryRow("SELECT agentic FROM review_jobs WHERE id = ?", jobID).Scan(&agentic)
				require.NoError(t, err, "Failed to query agentic: %v")

				assert.True(t, agentic, "DB agentic = false, want true")
			},
		},
		{
			name: "agentic flag defaults to false",
			opts: EnqueueOpts{
				Agent:     "codex",
				Reasoning: "standard",
				Prompt:    "Non-agentic prompt",
			},
			wantJob: func(t *testing.T, j *ReviewJob) {
				assert.False(t, j.Agentic)
			},
			checkClaim: true,
			wantClaimed: func(t *testing.T, j *ReviewJob) {
				assert.False(t, j.Agentic)
			},
			checkSQL: func(t *testing.T, db *DB, jobID int64) {
				var agentic bool
				err := db.QueryRow("SELECT agentic FROM review_jobs WHERE id = ?", jobID).Scan(&agentic)
				require.NoError(t, err, "Failed to query agentic: %v")

				assert.False(t, agentic, "DB agentic = true, want false")
			},
		},
		{
			name: "ClaimJob loads output_prefix",
			opts: EnqueueOpts{
				Agent:        "test",
				Prompt:       "compact prompt",
				OutputPrefix: "## Compact Analysis\n\n---\n\n",
			},
			checkClaim: true,
			wantClaimed: func(t *testing.T, j *ReviewJob) {
				want := "## Compact Analysis\n\n---\n\n"
				assert.Equal(t, want, j.OutputPrefix)
			},
		},
		{
			name: "ClaimJob returns empty OutputPrefix when not set",
			opts: EnqueueOpts{
				Agent:  "test",
				Prompt: "plain prompt",
			},
			checkClaim: true,
			wantClaimed: func(t *testing.T, j *ReviewJob) {
				assert.Empty(t, j.OutputPrefix)
			},
		},
		{
			name: "custom label sets git_ref",
			opts: EnqueueOpts{
				Agent:  "test",
				Prompt: "Test prompt",
				Label:  "test-fixtures",
			},
			wantJob: func(t *testing.T, j *ReviewJob) {
				assert.Equal(t, "test-fixtures", j.GitRef)
			},
			checkClaim: true,
			wantClaimed: func(t *testing.T, j *ReviewJob) {
				assert.Equal(t, "test-fixtures", j.GitRef)
			},
			checkSQL: func(t *testing.T, db *DB, jobID int64) {
				var gitRef string
				err := db.QueryRow("SELECT git_ref FROM review_jobs WHERE id = ?", jobID).Scan(&gitRef)
				require.NoError(t, err, "Failed to query git_ref: %v")

				assert.Equal(t, "test-fixtures", gitRef)
			},
		},
		{
			name: "empty label defaults to prompt",
			opts: EnqueueOpts{
				Agent:  "test",
				Prompt: "Test prompt",
			},
			wantJob: func(t *testing.T, j *ReviewJob) {
				assert.Equal(t, "prompt", j.GitRef)
			},
		},
		{
			name: "run label",
			opts: EnqueueOpts{
				Agent:  "test",
				Prompt: "Test prompt",
				Label:  "run",
			},
			wantJob: func(t *testing.T, j *ReviewJob) {
				assert.Equal(t, "run", j.GitRef)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoName := "prompt-test-" + strings.ReplaceAll(tt.name, " ", "-")
			db, repo := setupDBAndRepo(t, repoName)

			opts := tt.opts
			opts.RepoID = repo.ID
			job := mustEnqueuePromptJob(t, db, opts)

			if tt.wantJob != nil {
				tt.wantJob(t, job)
			}

			if tt.checkSQL != nil {
				tt.checkSQL(t, db, job.ID)
			}

			if tt.checkClaim {
				claimed := claimJob(t, db, "test-worker")
				if tt.wantClaimed != nil {
					tt.wantClaimed(t, claimed)
				}
			}
		})
	}
}

func TestPromptJobOutputProcessing(t *testing.T) {
	t.Run("output_prefix is prepended to review output", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "output-prefix-test")

		outputPrefix := "## Test Analysis\n\n**Files:**\n- file1.go\n- file2.go\n\n---\n\n"
		job := mustEnqueuePromptJob(t, db, EnqueueOpts{
			RepoID:       repo.ID,
			Agent:        "test",
			Prompt:       "Test prompt",
			OutputPrefix: outputPrefix,
		})

		agentOutput := "No issues found."
		completeTestJob(t, db, job.ID, agentOutput)

		// Fetch the review and verify prefix was prepended
		review, err := db.GetReviewByJobID(job.ID)
		require.NoError(t, err, "GetReviewByJobID failed: %v")

		expectedOutput := outputPrefix + agentOutput
		assert.Equal(t, expectedOutput, review.Output)
	})

	t.Run("empty output_prefix leaves output unchanged", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "empty-prefix-test")

		job := mustEnqueuePromptJob(t, db, EnqueueOpts{
			RepoID:       repo.ID,
			Agent:        "test",
			Prompt:       "Test prompt",
			OutputPrefix: "", // Empty prefix
		})

		agentOutput := "Analysis complete."
		completeTestJob(t, db, job.ID, agentOutput)

		// Fetch the review and verify output is unchanged
		review, err := db.GetReviewByJobID(job.ID)
		require.NoError(t, err, "GetReviewByJobID failed: %v")

		assert.Equal(t, review.Output, agentOutput)
	})
}

func TestRenameRepo(t *testing.T) {
	db, repo := setupDBAndRepo(t, "rename-test")
	initialPath := repo.RootPath

	t.Run("rename by path", func(t *testing.T) {
		affected, err := db.RenameRepo(initialPath, "new-name")
		require.NoError(t, err, "RenameRepo failed: %v")

		assert.EqualValues(t, 1, affected)

		// Verify the rename
		updated, err := db.GetRepoByID(repo.ID)
		require.NoError(t, err, "GetRepoByID failed: %v")

		assert.Equal(t, "new-name", updated.Name)
	})

	t.Run("rename by name", func(t *testing.T) {
		affected, err := db.RenameRepo("new-name", "another-name")
		require.NoError(t, err, "RenameRepo failed: %v")

		assert.EqualValues(t, 1, affected)

		updated, err := db.GetRepoByID(repo.ID)
		require.NoError(t, err, "GetRepoByID failed: %v")

		assert.Equal(t, "another-name", updated.Name)
	})

	t.Run("rename nonexistent repo returns 0", func(t *testing.T) {
		affected, err := db.RenameRepo("nonexistent", "something")
		require.NoError(t, err, "RenameRepo failed: %v")

		assert.EqualValues(t, 0, affected)
	})
}

func TestListReposWithReviewCountsIncludesIdentity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	identity := "https://github.com/test/renamed-repo.git"
	repo, err := db.GetOrCreateRepo(
		filepath.Join(t.TempDir(), "renamed-repo"),
		identity,
	)
	require.NoError(t, err)

	repos, _, err := db.ListReposWithReviewCounts()
	require.NoError(t, err)
	require.Len(t, repos, 1)

	assert.Equal(t, repo.RootPath, repos[0].RootPath)
	assert.Equal(t, identity, repos[0].Identity)
}

func TestMoveRepo(t *testing.T) {
	t.Run("updates root_path", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "move-test")
		newPath := filepath.Join(t.TempDir(), "new-location")

		err := db.MoveRepo(repo.ID, newPath, "")
		require.NoError(t, err)

		updated, err := db.GetRepoByID(repo.ID)
		require.NoError(t, err)
		assert.Equal(t, filepath.ToSlash(newPath), updated.RootPath)
	})

	t.Run("updates identity when provided", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "move-identity-test")
		newPath := filepath.Join(t.TempDir(), "new-location")
		newIdentity := "local://" + filepath.ToSlash(newPath)

		err := db.MoveRepo(repo.ID, newPath, newIdentity)
		require.NoError(t, err)

		updated, err := db.GetRepoByID(repo.ID)
		require.NoError(t, err)
		assert.Equal(t, filepath.ToSlash(newPath), updated.RootPath)

		// Verify identity was updated
		fetched, err := db.GetRepoByIdentity(newIdentity)
		require.NoError(t, err)
		require.NotNil(t, fetched)
		assert.Equal(t, repo.ID, fetched.ID)
	})

	t.Run("preserves jobs after move", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "move-jobs-test")
		commit := createCommit(t, db, repo.ID, "abc123")
		_ = commit

		// Verify a job exists for this repo
		var beforeCount int
		err := db.QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, repo.ID).Scan(&beforeCount)
		require.NoError(t, err)
		assert.Equal(t, 1, beforeCount)

		newPath := filepath.Join(t.TempDir(), "moved-repo")
		err = db.MoveRepo(repo.ID, newPath, "")
		require.NoError(t, err)

		// Commits remain associated by repo_id
		var afterCount int
		err = db.QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, repo.ID).Scan(&afterCount)
		require.NoError(t, err)
		assert.Equal(t, 1, afterCount)
	})

	t.Run("returns ErrRepoPathConflict when another repo has the target path", func(t *testing.T) {
		db := openTestDB(t)
		t.Cleanup(func() { db.Close() })

		repoA := createRepo(t, db, filepath.Join(t.TempDir(), "repo-a"))
		repoB := createRepo(t, db, filepath.Join(t.TempDir(), "repo-b"))

		err := db.MoveRepo(repoA.ID, repoB.RootPath, "")
		require.ErrorIs(t, err, ErrRepoPathConflict)

		// Verify repo-a was not modified
		unchanged, err := db.GetRepoByID(repoA.ID)
		require.NoError(t, err)
		assert.Equal(t, repoA.RootPath, unchanged.RootPath)
	})

	t.Run("returns ErrRepoPathConflict for normalized Windows target path", func(t *testing.T) {
		db := openTestDB(t)
		t.Cleanup(func() { db.Close() })

		repoA := createRepo(t, db, filepath.Join(t.TempDir(), "repo-a"))
		_, err := db.Exec(
			`INSERT INTO repos (root_path, name) VALUES (?, ?)`,
			`C:/Users/dev/workspace/repo-b`,
			"repo-b",
		)
		require.NoError(t, err)

		err = db.MoveRepo(repoA.ID, `C:\Users\dev\workspace\repo-b`, "")
		require.ErrorIs(t, err, ErrRepoPathConflict)

		unchanged, err := db.GetRepoByID(repoA.ID)
		require.NoError(t, err)
		assert.Equal(t, repoA.RootPath, unchanged.RootPath)
	})

	t.Run("moving to same path is a no-op", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "move-same-path-test")

		err := db.MoveRepo(repo.ID, repo.RootPath, "")
		require.NoError(t, err)

		updated, err := db.GetRepoByID(repo.ID)
		require.NoError(t, err)
		assert.Equal(t, repo.RootPath, updated.RootPath)
	})
}

func TestListRepos(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("empty database", func(t *testing.T) {
		repos, err := db.ListRepos()
		require.NoError(t, err, "ListRepos failed: %v")

		assert.Empty(t, repos)
	})

	// Create repos
	createRepo(t, db, filepath.Join(t.TempDir(), "repo-a"))
	createRepo(t, db, filepath.Join(t.TempDir(), "repo-b"))

	t.Run("lists repos in order", func(t *testing.T) {
		repos, err := db.ListRepos()
		require.NoError(t, err, "ListRepos failed: %v")

		assert.Len(t, repos, 2)
		// Should be ordered by name
		assert.False(t, len(repos) >= 2 && repos[0].Name > repos[1].Name)
	})
}

func TestGetRepoByID(t *testing.T) {
	db, repo := setupDBAndRepo(t, "getbyid-test")
	const identity = "https://github.com/acme/api.git"
	require.NoError(t, db.SetRepoIdentity(repo.ID, identity))

	t.Run("found", func(t *testing.T) {
		found, err := db.GetRepoByID(repo.ID)
		require.NoError(t, err, "GetRepoByID failed: %v")

		assert.Equal(t, found.ID, repo.ID)
		assert.Equal(t, found.RootPath, repo.RootPath)
		assert.Equal(t, identity, found.Identity)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := db.GetRepoByID(99999)
		require.Error(t, err)
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
}

func TestGetRepoByName(t *testing.T) {
	db, repo := setupDBAndRepo(t, "getbyname-test")

	t.Run("found", func(t *testing.T) {
		found, err := db.GetRepoByName("getbyname-test")
		require.NoError(t, err, "GetRepoByName failed: %v")

		assert.Equal(t, found.ID, repo.ID)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := db.GetRepoByName("nonexistent")
		require.Error(t, err)
	})
}

func TestFindRepo(t *testing.T) {
	db, repo := setupDBAndRepo(t, "findrepo-test")
	initialPath := repo.RootPath

	t.Run("find by path", func(t *testing.T) {
		found, err := db.FindRepo(initialPath)
		require.NoError(t, err, "FindRepo by path failed: %v")

		assert.Equal(t, found.ID, repo.ID)
	})

	t.Run("find by name", func(t *testing.T) {
		found, err := db.FindRepo("findrepo-test")
		require.NoError(t, err, "FindRepo by name failed: %v")

		assert.Equal(t, found.ID, repo.ID)
	})

	t.Run("created_at is populated", func(t *testing.T) {
		found, err := db.FindRepo(initialPath)
		require.NoError(t, err, "FindRepo failed: %v")

		assert.False(t, found.CreatedAt.IsZero(), "CreatedAt should not be zero (SQLite CURRENT_TIMESTAMP must be parsed)")
		// Should be recent (within the last minute)
		assert.LessOrEqual(t, time.Since(found.CreatedAt), time.Minute)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := db.FindRepo("nonexistent")
		require.Error(t, err)
	})
}

func TestGetRepoStats(t *testing.T) {
	t.Run("empty repo", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "stats-test")

		stats, err := db.GetRepoStats(repo.ID)
		require.NoError(t, err, "GetRepoStats failed: %v")

		assert.Equal(t, 0, stats.TotalJobs)
		assert.Equal(t, stats.Repo.ID, repo.ID)
	})

	t.Run("stats with jobs", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "stats-jobs-test")

		// Add some jobs
		commit1 := createCommit(t, db, repo.ID, "stats-sha1")
		job1 := enqueueJob(t, db, repo.ID, commit1.ID, "stats-sha1")

		commit2 := createCommit(t, db, repo.ID, "stats-sha2")
		job2 := enqueueJob(t, db, repo.ID, commit2.ID, "stats-sha2")

		commit3 := createCommit(t, db, repo.ID, "stats-sha3")
		job3 := enqueueJob(t, db, repo.ID, commit3.ID, "stats-sha3")

		// Complete job1 with PASS verdict
		completeTestJob(t, db, job1.ID, "**Verdict: PASS**\nLooks good!")

		// Complete job2 with FAIL verdict
		completeTestJob(t, db, job2.ID, "**Verdict: FAIL**\nIssues found.")

		// Fail job3
		claimJob(t, db, "worker-1")
		if _, err := db.FailJob(job3.ID, "", "agent error"); err != nil {
			require.NoError(t, err, "FailJob failed: %v")
		}

		stats, err := db.GetRepoStats(repo.ID)
		require.NoError(t, err, "GetRepoStats failed: %v")

		assert.Equal(t, 3, stats.TotalJobs)
		assert.Equal(t, 2, stats.CompletedJobs)
		assert.Equal(t, 1, stats.FailedJobs)
		assert.Equal(t, 1, stats.PassedReviews)
		assert.Equal(t, 1, stats.FailedReviews)
		// Both reviews should be open by default
		assert.Equal(t, 0, stats.ClosedReviews)
		assert.Equal(t, 2, stats.OpenReviews)
	})

	t.Run("closed reviews counted", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "stats-addressed-test")
		commit1 := createCommit(t, db, repo.ID, "stats-sha1")
		job1 := enqueueJob(t, db, repo.ID, commit1.ID, "stats-sha1")

		// Complete job1
		completeTestJob(t, db, job1.ID, "**Verdict: PASS**\nLooks good!")

		// Mark job1's review as closed
		review, err := db.GetReviewByJobID(job1.ID)
		require.NoError(t, err, "GetReviewByJobID failed: %v")

		if err := db.MarkReviewClosed(review.ID, true); err != nil {
			require.NoError(t, err, "MarkReviewClosed failed: %v")
		}

		// Create a second job that will be open
		commit2 := createCommit(t, db, repo.ID, "stats-sha2")
		job2 := enqueueJob(t, db, repo.ID, commit2.ID, "stats-sha2")

		// Complete job2
		completeTestJob(t, db, job2.ID, "**Verdict: PASS**\nAlso looks good!")

		stats, err := db.GetRepoStats(repo.ID)
		require.NoError(t, err, "GetRepoStats failed: %v")

		assert.Equal(t, 1, stats.ClosedReviews)
		assert.Equal(t, 1, stats.OpenReviews)
	})

	t.Run("nonexistent repo", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()
		_, err := db.GetRepoStats(99999)
		require.Error(t, err)
	})

	t.Run("prompt jobs excluded from verdict counts", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "stats-prompt-test")

		// Create a regular job with PASS verdict
		commit := createCommit(t, db, repo.ID, "stats-prompt-sha1")
		job1 := enqueueJob(t, db, repo.ID, commit.ID, "stats-prompt-sha1")
		completeTestJob(t, db, job1.ID, "**Verdict: PASS**\nLooks good!")

		// Create a prompt job with output that contains verdict-like text
		promptJob := mustEnqueuePromptJob(t, db, EnqueueOpts{RepoID: repo.ID, Agent: "codex", Prompt: "Test prompt"})
		// This has FAIL verdict text but should NOT count toward failed reviews
		completeTestJob(t, db, promptJob.ID, "**Verdict: FAIL**\nSome issues found")

		// Get stats - prompt job should be excluded from verdict counts
		stats, err := db.GetRepoStats(repo.ID)
		require.NoError(t, err, "GetRepoStats failed: %v")

		// Total jobs should include both
		assert.Equal(t, 2, stats.TotalJobs)
		assert.Equal(t, 2, stats.CompletedJobs)

		// Verdict counts should only reflect the regular job
		assert.Equal(t, 1, stats.PassedReviews)
		assert.Equal(t, 0, stats.FailedReviews)
	})

	t.Run("stored verdict_bool overrides output parsing", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "stats-stored-verdict-test")

		commit := createCommit(t, db, repo.ID, "stats-stored-verdict-sha")
		job := enqueueJob(t, db, repo.ID, commit.ID, "stats-stored-verdict-sha")
		completeTestJob(t, db, job.ID, "**Verdict: PASS**\nLooks good!")

		_, err := db.Exec(`UPDATE reviews SET verdict_bool = 0 WHERE job_id = ?`, job.ID)
		require.NoError(t, err, "force stored verdict_bool failed: %v")

		stats, err := db.GetRepoStats(repo.ID)
		require.NoError(t, err, "GetRepoStats failed: %v")

		assert.Equal(t, 0, stats.PassedReviews)
		assert.Equal(t, 1, stats.FailedReviews)
	})

	t.Run("legacy null verdict_bool still falls back to parsed output", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "stats-legacy-verdict-test")

		commit := createCommit(t, db, repo.ID, "stats-legacy-verdict-sha")
		job := enqueueJob(t, db, repo.ID, commit.ID, "stats-legacy-verdict-sha")
		completeTestJob(t, db, job.ID, "**Verdict: PASS**\nLooks good!")

		_, err := db.Exec(`UPDATE reviews SET verdict_bool = NULL WHERE job_id = ?`, job.ID)
		require.NoError(t, err, "clear stored verdict_bool failed: %v")

		stats, err := db.GetRepoStats(repo.ID)
		require.NoError(t, err, "GetRepoStats failed: %v")

		assert.Equal(t, 1, stats.PassedReviews)
		assert.Equal(t, 0, stats.FailedReviews)
	})
}

func TestDeleteRepo(t *testing.T) {
	t.Run("delete empty repo", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "delete-empty")

		err := db.DeleteRepo(repo.ID, false)
		require.NoError(t, err, "DeleteRepo failed: %v")

		// Verify deleted
		_, err = db.GetRepoByID(repo.ID)
		require.Error(t, err)
	})

	t.Run("delete repo with jobs without cascade returns error", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "delete-with-jobs")
		commit := createCommit(t, db, repo.ID, "delete-sha")
		enqueueJob(t, db, repo.ID, commit.ID, "delete-sha")

		// Without cascade, delete should return ErrRepoHasJobs
		err := db.DeleteRepo(repo.ID, false)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrRepoHasJobs)

		// Verify repo still exists
		_, err = db.GetRepoByID(repo.ID)
		require.NoError(t, err)
	})

	t.Run("delete repo with cascade", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "delete-cascade")
		commit := createCommit(t, db, repo.ID, "cascade-sha")
		job := enqueueJob(t, db, repo.ID, commit.ID, "cascade-sha")
		completeTestJob(t, db, job.ID, "output")

		// Add a comment
		db.AddCommentToJob(job.ID, "user", "comment")

		err := db.DeleteRepo(repo.ID, true)
		require.NoError(t, err, "DeleteRepo with cascade failed: %v")

		// Verify repo deleted
		_, err = db.GetRepoByID(repo.ID)
		require.Error(t, err)

		// Verify jobs deleted
		jobs, _ := db.ListJobs("", "", 100, 0)
		for _, j := range jobs {
			assert.NotEqual(t, j.RepoID, repo.ID)
		}
	})

	t.Run("delete nonexistent repo", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		err := db.DeleteRepo(99999, false)
		require.Error(t, err)
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
}

func TestMergeRepos(t *testing.T) {
	t.Run("merge repos moves jobs", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		source := createRepo(t, db, filepath.Join(t.TempDir(), "merge-source"))
		target := createRepo(t, db, filepath.Join(t.TempDir(), "merge-target"))

		// Create jobs in source
		commit1 := createCommit(t, db, source.ID, "merge-sha1")
		enqueueJob(t, db, source.ID, commit1.ID, "merge-sha1")
		commit2 := createCommit(t, db, source.ID, "merge-sha2")
		enqueueJob(t, db, source.ID, commit2.ID, "merge-sha2")

		// Create job in target
		commit3 := createCommit(t, db, target.ID, "merge-sha3")
		enqueueJob(t, db, target.ID, commit3.ID, "merge-sha3")

		moved, err := db.MergeRepos(source.ID, target.ID)
		require.NoError(t, err, "MergeRepos failed: %v")

		assert.EqualValues(t, 2, moved)

		// Verify source is deleted
		_, err = db.GetRepoByID(source.ID)
		require.Error(t, err)

		// Verify all jobs now belong to target
		jobs, _ := db.ListJobs("", target.RootPath, 100, 0)
		assert.Len(t, jobs, 3)
	})

	t.Run("merge same repo returns 0", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "merge-same")

		moved, err := db.MergeRepos(repo.ID, repo.ID)
		require.NoError(t, err, "MergeRepos failed: %v")

		assert.EqualValues(t, 0, moved)

		// Verify repo still exists
		_, err = db.GetRepoByID(repo.ID)
		require.NoError(t, err)
	})

	t.Run("merge empty source", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		source := createRepo(t, db, filepath.Join(t.TempDir(), "merge-empty-source"))
		target := createRepo(t, db, filepath.Join(t.TempDir(), "merge-empty-target"))

		moved, err := db.MergeRepos(source.ID, target.ID)
		require.NoError(t, err, "MergeRepos failed: %v")

		assert.EqualValues(t, 0, moved)

		// Source should be deleted even if empty
		_, err = db.GetRepoByID(source.ID)
		require.Error(t, err)
	})

	t.Run("merge moves commits to target", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		source := createRepo(t, db, filepath.Join(t.TempDir(), "merge-commits-source"))
		target := createRepo(t, db, filepath.Join(t.TempDir(), "merge-commits-target"))

		// Create commits in source
		commit1 := createCommit(t, db, source.ID, "commit-sha-1")
		commit2 := createCommit(t, db, source.ID, "commit-sha-2")
		enqueueJob(t, db, source.ID, commit1.ID, "commit-sha-1")
		enqueueJob(t, db, source.ID, commit2.ID, "commit-sha-2")

		// Verify commits belong to source before merge
		var sourceCommitCount int
		db.QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, source.ID).Scan(&sourceCommitCount)
		assert.Equal(t, 2, sourceCommitCount)

		_, err := db.MergeRepos(source.ID, target.ID)
		require.NoError(t, err, "MergeRepos failed: %v")

		// Verify commits now belong to target
		var targetCommitCount int
		db.QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, target.ID).Scan(&targetCommitCount)
		assert.Equal(t, 2, targetCommitCount)

		// Verify no commits remain with source ID (source is deleted)
		var orphanedCount int
		db.QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, source.ID).Scan(&orphanedCount)
		assert.Equal(t, 0, orphanedCount)
	})
}

func TestDeleteRepoCascadeDeletesCommits(t *testing.T) {
	db, repo := setupDBAndRepo(t, "delete-commits-test")
	commit1 := createCommit(t, db, repo.ID, "del-commit-1")
	commit2 := createCommit(t, db, repo.ID, "del-commit-2")
	enqueueJob(t, db, repo.ID, commit1.ID, "del-commit-1")
	enqueueJob(t, db, repo.ID, commit2.ID, "del-commit-2")

	// Verify commits exist before delete
	var beforeCount int
	db.QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, repo.ID).Scan(&beforeCount)
	assert.Equal(t, 2, beforeCount)

	err := db.DeleteRepo(repo.ID, true)
	require.NoError(t, err, "DeleteRepo with cascade failed: %v")

	// Verify commits are deleted
	var afterCount int
	db.QueryRow(`SELECT COUNT(*) FROM commits WHERE repo_id = ?`, repo.ID).Scan(&afterCount)
	assert.Equal(t, 0, afterCount)
}

func TestDeleteRepoCascadeDeletesLegacyCommitResponses(t *testing.T) {
	db, repo := setupDBAndRepo(t, "delete-legacy-resp-test")
	commit := createCommit(t, db, repo.ID, "legacy-resp-commit")

	// Add legacy commit-based comment (not job-based)
	_, err := db.AddComment(commit.ID, "reviewer", "Legacy comment on commit")
	require.NoError(t, err, "AddComment failed: %v")

	// Verify comment exists
	var beforeCount int
	db.QueryRow(`SELECT COUNT(*) FROM responses WHERE commit_id = ?`, commit.ID).Scan(&beforeCount)
	assert.Equal(t, 1, beforeCount)

	err = db.DeleteRepo(repo.ID, true)
	require.NoError(t, err, "DeleteRepo with cascade failed: %v")

	// Verify legacy responses are deleted (by checking all responses - commit is gone)
	var afterCount int
	db.QueryRow(`SELECT COUNT(*) FROM responses WHERE commit_id = ?`, commit.ID).Scan(&afterCount)
	assert.Equal(t, 0, afterCount)
}

func TestVerdictSuppressionForPromptJobs(t *testing.T) {
	t.Run("prompt jobs do not get verdict computed", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "verdict-prompt-test")

		// Create a prompt job and complete it with output containing verdict-like text
		promptJob := mustEnqueuePromptJob(t, db, EnqueueOpts{RepoID: repo.ID, Agent: "codex", Prompt: "Test prompt"})
		// Output that would normally be parsed as FAIL
		completeTestJob(t, db, promptJob.ID, "Found issues:\n1. Problem A")

		// Fetch via ListJobs and check verdict is nil
		jobs, _ := db.ListJobs("", repo.RootPath, 100, 0)
		var found *ReviewJob
		for i := range jobs {
			if jobs[i].ID == promptJob.ID {
				found = &jobs[i]
				break
			}
		}

		assert.NotNil(t, found)

		assert.Nil(t, found.Verdict)
	})

	t.Run("regular jobs still get verdict computed", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "verdict-regular-test")
		commit := createCommit(t, db, repo.ID, "verdict-sha")

		// Create a regular job and complete it
		job := enqueueJob(t, db, repo.ID, commit.ID, "verdict-sha")
		claimJob(t, db, "worker-1")
		// Output that should be parsed as PASS
		db.CompleteJob(job.ID, "codex", "prompt", "No issues found in this commit.")

		// Fetch via ListJobs and check verdict is set
		jobs, _ := db.ListJobs("", repo.RootPath, 100, 0)
		var found *ReviewJob
		for i := range jobs {
			if jobs[i].ID == job.ID {
				found = &jobs[i]
				break
			}
		}

		assert.NotNil(t, found)
		if found != nil {
			assert.NotNil(t, found.Verdict)
			assert.Equal(t, "P", *found.Verdict)
		}
	})

	t.Run("branch named prompt with commit_id gets verdict", func(t *testing.T) {
		db, repo := setupDBAndRepo(t, "verdict-branch-prompt")
		// Create a commit for a branch literally named "prompt"
		commit := createCommit(t, db, repo.ID, "branch-prompt-sha")

		// Enqueue with git_ref = "prompt" but WITH a commit_id (simulating review of branch "prompt")
		result, _ := db.Exec(`INSERT INTO review_jobs (repo_id, commit_id, git_ref, agent, reasoning, status) VALUES (?, ?, 'prompt', 'codex', 'thorough', 'queued')`,
			repo.ID, commit.ID)
		jobID, _ := result.LastInsertId()

		claimJob(t, db, "worker-1")
		// Output that should be parsed as FAIL
		db.CompleteJob(jobID, "codex", "prompt", "Found issues:\n1. Bug found")

		// Fetch via ListJobs and check verdict IS computed (because commit_id is not NULL)
		jobs, _ := db.ListJobs("", repo.RootPath, 100, 0)
		var found *ReviewJob
		for i := range jobs {
			if jobs[i].ID == jobID {
				found = &jobs[i]
				break
			}
		}

		assert.NotNil(t, found)

		// This job has commit_id set, so it's NOT a prompt job - verdict should be computed
		if found != nil {
			assert.NotNil(t, found.Verdict)
			assert.Equal(t, "F", *found.Verdict)
		}
	})
}

// TestRetriedReviewJobNotRoutedAsPromptJob verifies that when a review
// job is retried, the saved prompt from the first run does not cause
// the job to be misidentified as a prompt-native job (task/compact).
// This is the storage-level regression test for the UsesStoredPrompt gate.
func TestRetriedReviewJobNotRoutedAsPromptJob(t *testing.T) {
	tests := []struct {
		name               string
		setupJob           func(t *testing.T, db *DB, repoID int64) *ReviewJob
		manuallySavePrompt bool
		expectedJobType    string
		expectStoredPrompt bool
		expectedPrompt     string
	}{
		{
			name: "review job",
			setupJob: func(t *testing.T, db *DB, repoID int64) *ReviewJob {
				commit := createCommit(t, db, repoID, "retry-sha1")
				return enqueueJob(t, db, repoID, commit.ID, "retry-sha1")
			},
			manuallySavePrompt: true,
			expectedJobType:    JobTypeReview,
			expectStoredPrompt: false,
			expectedPrompt:     "Saved prompt...",
		},
		{
			name: "task job",
			setupJob: func(t *testing.T, db *DB, repoID int64) *ReviewJob {
				return mustEnqueuePromptJob(t, db, EnqueueOpts{
					RepoID: repoID,
					Agent:  "test",
					Prompt: "Analyze the codebase architecture",
				})
			},
			manuallySavePrompt: false,
			expectedJobType:    JobTypeTask,
			expectStoredPrompt: true,
			expectedPrompt:     "Analyze the codebase architecture",
		},
		{
			name: "compact job",
			setupJob: func(t *testing.T, db *DB, repoID int64) *ReviewJob {
				return mustEnqueuePromptJob(t, db, EnqueueOpts{
					RepoID:  repoID,
					Agent:   "test",
					Prompt:  "Verify these findings are still relevant...",
					JobType: JobTypeCompact,
					Label:   "compact",
				})
			},
			manuallySavePrompt: false,
			expectedJobType:    JobTypeCompact,
			expectStoredPrompt: true,
			expectedPrompt:     "Verify these findings are still relevant...",
		},
		{
			name: "dirty job",
			setupJob: func(t *testing.T, db *DB, repoID int64) *ReviewJob {
				return mustEnqueuePromptJob(t, db, EnqueueOpts{
					RepoID:      repoID,
					Agent:       "test",
					GitRef:      "dirty",
					DiffContent: "diff --git a/file.go b/file.go\n+new line",
				})
			},
			manuallySavePrompt: true,
			expectedJobType:    JobTypeDirty,
			expectStoredPrompt: false,
			expectedPrompt:     "Saved prompt...",
		},
		{
			name: "range job",
			setupJob: func(t *testing.T, db *DB, repoID int64) *ReviewJob {
				return mustEnqueuePromptJob(t, db, EnqueueOpts{
					RepoID: repoID,
					Agent:  "test",
					GitRef: "abc123..def456",
				})
			},
			manuallySavePrompt: true,
			expectedJobType:    JobTypeRange,
			expectStoredPrompt: false,
			expectedPrompt:     "Saved prompt...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, repo := setupDBAndRepo(t, "retry-"+strings.ReplaceAll(tt.name, " ", "-"))

			job := tt.setupJob(t, db, repo.ID)
			assert.Equal(t, tt.expectedJobType, job.JobType)

			claimed := claimJob(t, db, "worker-1")

			if tt.manuallySavePrompt {
				if err := db.SaveJobPrompt(claimed.ID, "Saved prompt..."); err != nil {
					require.NoError(t, err, "SaveJobPrompt failed: %v")
				}
			}

			retried, err := db.RetryJob(claimed.ID, "", 3, 0)
			require.NoError(t, err, "RetryJob failed: %v")

			assert.True(t, retried)

			reclaimed := claimJob(t, db, "worker-2")
			assert.Equal(t, reclaimed.UsesStoredPrompt(), tt.expectStoredPrompt)
			assert.Equal(t, tt.expectedPrompt, reclaimed.Prompt)
		})
	}
}
