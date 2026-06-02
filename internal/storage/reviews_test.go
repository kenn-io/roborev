package storage

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddCommentToJobAllStates verifies that comments can be added to jobs
// in any state: queued, running, done, failed, and canceled.
func TestAddCommentToJobAllStates(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	testCases := []struct {
		name   string
		status JobStatus
	}{
		{"queued job", JobStatusQueued},
		{"running job", JobStatusRunning},
		{"completed job", JobStatusDone},
		{"failed job", JobStatusFailed},
		{"canceled job", JobStatusCanceled},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			job := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
			setJobStatus(t, db, job.ID, tc.status)

			// Verify job is in expected state
			updatedJob, err := db.GetJobByID(job.ID)
			require.NoError(t, err, "Failed to verify job status: %v")

			assert.Equal(t, updatedJob.Status, tc.status)

			// Add a comment to the job
			comment := "Test comment for " + tc.name
			resp, err := db.AddCommentToJob(job.ID, "test-user", comment)
			require.NoError(t, err, "AddCommentToJob failed for %s: %v", tc.name)

			// Verify the comment was added
			assert.NotNil(t, resp)
			verifyComment(t, *resp, "test-user", comment)
			assert.False(t, resp.JobID == nil || *resp.JobID != job.ID)

			// Verify we can retrieve the comment
			comments, err := db.GetCommentsForJob(job.ID)
			require.NoError(t, err, "GetCommentsForJob failed: %v")

			assert.Len(t, comments, 1)
			verifyComment(t, comments[0], "test-user", comment)
		})
	}
}

// TestAddCommentToJobNonExistent verifies that adding a comment to a
// non-existent job returns an appropriate error.
func TestAddCommentToJobNonExistent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Try to add a comment to a job that doesn't exist
	_, err := db.AddCommentToJob(99999, "test-user", "This should fail")
	require.Error(t, err)
	assert.Equal(t, err, sql.ErrNoRows)
}

// TestAddCommentToJobMultipleComments verifies that multiple comments
// can be added to the same job.
func TestAddCommentToJobMultipleComments(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "abc123")
	setJobStatus(t, db, job.ID, JobStatusRunning)

	// Add multiple comments from different users
	comments := []struct {
		user    string
		message string
	}{
		{"alice", "First comment while job is running"},
		{"bob", "Second comment from another user"},
		{"alice", "Third comment from alice again"},
	}

	for _, c := range comments {
		_, err := db.AddCommentToJob(job.ID, c.user, c.message)
		require.NoError(t, err, "AddCommentToJob failed for %s: %v", c.user)

	}

	// Verify all comments were added
	retrieved, err := db.GetCommentsForJob(job.ID)
	require.NoError(t, err, "GetCommentsForJob failed: %v")

	assert.Len(t, comments, len(retrieved))

	// Verify comments are in order
	for i, c := range comments {
		verifyComment(t, retrieved[i], c.user, c.message)
	}
}

// TestAddCommentToJobWithNoReview verifies that comments can be added
// to jobs that have no review (i.e., job exists but has no review record yet).
func TestAddCommentToJobWithNoReview(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "abc123")

	// Verify no review exists for this job
	_, err := db.GetReviewByJobID(job.ID)
	require.Error(t, err)

	// Add a comment to the job (should succeed even without a review)
	resp, err := db.AddCommentToJob(job.ID, "test-user", "Comment on job without review")
	require.NoError(t, err, "AddCommentToJob failed: %v")

	assert.NotNil(t, resp)
	assert.Equal(t, "Comment on job without review", resp.Response)
}

func TestGetAllCommentsForJob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, commit, job := createJobChain(t, db, "/tmp/test-repo", "abc1234")
	machineID, _ := db.GetMachineID()

	// Insert with explicit timestamps to avoid flaky ordering
	t1 := "2026-03-15T10:00:00Z"
	t2 := "2026-03-15T11:00:00Z"
	t3 := "2026-03-15T12:00:00Z"

	_, err := db.Exec(
		`INSERT INTO responses (job_id, responder, response, uuid, source_machine_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		job.ID, "alice", "Job-based comment", GenerateUUID(), machineID, t1,
	)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO responses (commit_id, responder, response, uuid, source_machine_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		commit.ID, "bob", "Legacy commit comment", GenerateUUID(), machineID, t2,
	)
	require.NoError(t, err)

	t.Run("merges job and legacy commit comments", func(t *testing.T) {
		all, err := db.GetAllCommentsForJob(job.ID, commit.ID, "")
		require.NoError(t, err)
		require.Len(t, all, 2)
		assert.Equal(t, "alice", all[0].Responder)
		assert.Equal(t, "bob", all[1].Responder)
	})

	t.Run("skips legacy fallback when commitID is zero and no gitRef", func(t *testing.T) {
		all, err := db.GetAllCommentsForJob(job.ID, 0, "")
		require.NoError(t, err)
		require.Len(t, all, 1)
		assert.Equal(t, "alice", all[0].Responder)
	})

	t.Run("falls back to SHA when commitID is zero", func(t *testing.T) {
		all, err := db.GetAllCommentsForJob(job.ID, 0, "abc1234")
		require.NoError(t, err)
		require.Len(t, all, 2)
		assert.Equal(t, "alice", all[0].Responder)
		assert.Equal(t, "bob", all[1].Responder)
	})

	t.Run("dirty job skips base commit legacy comments", func(t *testing.T) {
		diff := "diff --git a/file.go b/file.go\n+dirty\n"
		dirtyJob, err := db.EnqueueJob(EnqueueOpts{
			RepoID:      job.RepoID,
			CommitID:    commit.ID,
			GitRef:      "dirty",
			Agent:       "test",
			JobType:     JobTypeDirty,
			DiffContent: diff,
		})
		require.NoError(t, err)
		_, err = db.Exec(
			`INSERT INTO responses (job_id, responder, response, uuid, source_machine_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			dirtyJob.ID, "dana", "Dirty job comment", GenerateUUID(), machineID, t3,
		)
		require.NoError(t, err)

		commitID, fallbackSHA := dirtyJob.LegacyCommentLookupTarget()
		all, err := db.GetAllCommentsForJob(dirtyJob.ID, commitID, fallbackSHA)
		require.NoError(t, err)
		require.Len(t, all, 1)
		assert.Equal(t, "dana", all[0].Responder)
	})

	t.Run("skips fallback when SHA is empty", func(t *testing.T) {
		// Callers pass "" when gitRef is not a valid SHA (e.g. ranges).
		all, err := db.GetAllCommentsForJob(job.ID, 0, "")
		require.NoError(t, err)
		require.Len(t, all, 1)
	})

	t.Run("deduplicates overlapping comments", func(t *testing.T) {
		// Insert a response linked to both job_id AND commit_id to
		// exercise the dedup branch — it appears in both queries.
		_, err := db.Exec(
			`INSERT INTO responses (job_id, commit_id, responder, response, uuid, source_machine_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			job.ID, commit.ID, "charlie", "Dual-linked comment",
			GenerateUUID(), machineID, t3,
		)
		require.NoError(t, err)

		all, err := db.GetAllCommentsForJob(job.ID, commit.ID, "")
		require.NoError(t, err)
		// alice (job), bob (commit), charlie (both) — charlie should
		// appear only once despite matching both queries.
		require.Len(t, all, 3)
		assert.Equal(t, "alice", all[0].Responder)
		assert.Equal(t, "bob", all[1].Responder)
		assert.Equal(t, "charlie", all[2].Responder)
	})

	t.Run("returns error on legacy lookup failure", func(t *testing.T) {
		// Use a hex string that looks like a SHA but doesn't exist in the
		// commits table to trigger a legacy lookup error.
		all, err := db.GetAllCommentsForJob(job.ID, 0, "deadbeefdeadbeef")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "legacy comment lookup")
		// Job-based comments should still be returned (alice + charlie
		// which was dual-linked in the dedup subtest above).
		require.Len(t, all, 2)
		assert.Equal(t, "alice", all[0].Responder)
		assert.Equal(t, "charlie", all[1].Responder)
	})
}

func TestGetReviewByJobIDIncludesModel(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")

	tests := []struct {
		name          string
		gitRef        string
		model         string
		expectedModel string
	}{
		{"model is populated when set", "abc123", "o3", "o3"},
		{"model is empty when not set", "def456", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := createCompletedJobWithOptions(t, db, EnqueueOpts{
				RepoID:    repo.ID,
				GitRef:    tt.gitRef,
				Agent:     "codex",
				Model:     tt.model,
				Reasoning: "thorough",
			}, "Test review output\n\n## Verdict: PASS")

			review, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err, "GetReviewByJobID failed: %v")

			assert.NotNil(t, review.Job)
			assert.Equal(t, tt.expectedModel, review.Job.Model)
		})
	}
}

func TestGetJobsWithReviewsByIDs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")

	// Job 1: with review
	job1 := createCompletedJobWithOptions(t, db, EnqueueOpts{RepoID: repo.ID, GitRef: "abc123"}, "output1")

	// Job 3: with review
	// Note: We create job3 before job2 so that the queue is empty when we claim/complete job3.
	// If job2 were created first, ClaimJob would pick it up instead.
	job3 := createCompletedJobWithOptions(t, db, EnqueueOpts{RepoID: repo.ID, GitRef: "ghi789"}, "output3")

	// Job 2: no review (still queued)
	job2 := enqueueJob(t, db, repo.ID, 0, "def456")

	// Job 4: does not exist
	nonExistentJobID := int64(9999)

	t.Run("fetch multiple jobs", func(t *testing.T) {
		jobIDs := []int64{job1.ID, job2.ID, job3.ID, nonExistentJobID}
		results, err := db.GetJobsWithReviewsByIDs(jobIDs)
		require.NoError(t, err, "GetJobsWithReviewsByIDs failed: %v")

		assert.Len(t, results, 3)

		// Check job 1 (with review)
		res1, ok := results[job1.ID]
		assert.True(t, ok)
		assert.Equal(t, job1.ID, res1.Job.ID)
		assert.NotNil(t, res1.Review, "Expected review for job 1, but got nil")
		if res1.Review != nil {
			assert.Equal(t, "output1", res1.Review.Output)
		}

		// Check job 2 (no review)
		res2, ok := results[job2.ID]
		assert.True(t, ok)
		assert.Equal(t, job2.ID, res2.Job.ID)
		assert.Nil(t, res2.Review)

		// Check job 3 (with review)
		res3, ok := results[job3.ID]
		assert.True(t, ok)
		assert.NotNil(t, res3.Review, "Expected review for job 3, but got nil")
		if res3.Review != nil {
			assert.Equal(t, "output3", res3.Review.Output)
		}

		// Check non-existent job
		_, ok = results[nonExistentJobID]
		assert.False(t, ok)
	})

	t.Run("empty id list", func(t *testing.T) {
		results, err := db.GetJobsWithReviewsByIDs([]int64{})
		require.NoError(t, err, "GetJobsWithReviewsByIDs with empty slice failed: %v")

		assert.Empty(t, results)
	})

	t.Run("only non-existent ids", func(t *testing.T) {
		results, err := db.GetJobsWithReviewsByIDs([]int64{999, 998, 997})
		require.NoError(t, err, "GetJobsWithReviewsByIDs with non-existent IDs failed: %v")

		assert.Empty(t, results)
	})
}

func TestGetJobsWithReviewsByIDsPreservesMinSeverity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/min-sev-batch-test")

	job := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:      repo.ID,
		GitRef:      "sev123",
		MinSeverity: "high",
	}, "No issues found.")

	results, err := db.GetJobsWithReviewsByIDs([]int64{job.ID})
	require.NoError(t, err)

	res, ok := results[job.ID]
	require.True(t, ok)
	assert.Equal(t, "high", res.Job.MinSeverity)
}

// TestGetJobsWithReviewsByIDsPreservesBackup verifies the batch getter hydrates
// backup_agent/backup_model (they were omitted from the SELECT entirely).
func TestGetJobsWithReviewsByIDsPreservesBackup(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/backup-batch-test")

	job := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:      repo.ID,
		GitRef:      "bkp123",
		Agent:       "codex",
		BackupAgent: "claude-code",
		BackupModel: "opus",
		MinSeverity: "high",
	}, "No issues found.")

	results, err := db.GetJobsWithReviewsByIDs([]int64{job.ID})
	require.NoError(t, err)

	res, ok := results[job.ID]
	require.True(t, ok)
	assert.Equal("claude-code", res.Job.BackupAgent)
	assert.Equal("opus", res.Job.BackupModel)
	assert.Equal("high", res.Job.MinSeverity)
}

// TestSingleReviewGettersPreserveBackupAndMinSeverity verifies that the
// single-review getters carry backup_agent/backup_model/min_severity through
// hydration. The columns are scanned into the scan-fields struct so
// applyReviewJobScan does not clobber them back to zero.
func TestSingleReviewGettersPreserveBackupAndMinSeverity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/backup-single-test")
	commit := createCommit(t, db, repo.ID, "bkp456")

	job := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:      repo.ID,
		CommitID:    commit.ID,
		GitRef:      "bkp456",
		Agent:       "codex",
		BackupAgent: "claude-code",
		BackupModel: "opus",
		MinSeverity: "high",
	}, "No issues found.")

	assertCarried := func(name string, rev *Review) {
		assert := assert.New(t)
		require.NotNil(t, rev.Job, name)
		assert.Equal("claude-code", rev.Job.BackupAgent, name)
		assert.Equal("opus", rev.Job.BackupModel, name)
		assert.Equal("high", rev.Job.MinSeverity, name)
	}

	byJob, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assertCarried("GetReviewByJobID", byJob)

	bySHA, err := db.GetReviewByCommitSHA("bkp456")
	require.NoError(t, err)
	assertCarried("GetReviewByCommitSHA", bySHA)
}

func TestGetJobsWithReviewsByIDsPopulatesVerdict(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/verdict-batch-test")

	// Create a job with a PASS verdict
	passJob := createCompletedJobWithOptions(t, db, EnqueueOpts{RepoID: repo.ID, GitRef: "pass111"}, "No issues found.\n\n## Verdict: PASS")

	// Create a job with a FAIL verdict
	failJob := createCompletedJobWithOptions(t, db, EnqueueOpts{RepoID: repo.ID, GitRef: "fail222"}, "- High — Critical bug found")

	results, err := db.GetJobsWithReviewsByIDs([]int64{passJob.ID, failJob.ID})
	require.NoError(t, err, "GetJobsWithReviewsByIDs failed: %v")

	cases := []struct {
		name        string
		jobID       int64
		wantVerdict string
		wantBool    int
	}{
		{"pass", passJob.ID, "P", 1},
		{"fail", failJob.ID, "F", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := results[tc.jobID]
			assert.True(t, ok)
			assert.NotNil(t, res.Job.Verdict)
			assert.Equal(t, tc.wantVerdict, *res.Job.Verdict)
			assert.NotNil(t, res.Review)
			if res.Review != nil {
				assert.NotNil(t, res.Review.VerdictBool)
				if res.Review.VerdictBool != nil {
					assert.Equal(t, tc.wantBool, *res.Review.VerdictBool)
				}
			}
		})
	}
}

func TestGetReviewByJobIDUsesStoredVerdict(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/verdict-read-test")
	commit := createCommit(t, db, repo.ID, "vread123")

	t.Run("new review uses stored verdict_bool", func(t *testing.T) {
		job := createCompletedJobWithOptions(t, db, EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "vread123",
			Agent:    "codex",
		}, "No issues found.")

		review, err := db.GetReviewByJobID(job.ID)
		require.NoError(t, err, "GetReviewByJobID: %v")

		assert.NotNil(t, review.VerdictBool)
		assert.Equal(t, 1, *review.VerdictBool)
		assert.False(t, review.Job == nil || review.Job.Verdict == nil || *review.Job.Verdict != "P")
	})

	t.Run("legacy review with NULL verdict_bool falls back to ParseVerdict", func(t *testing.T) {
		commit2 := createCommit(t, db, repo.ID, "vread456")
		job := createCompletedJobWithOptions(t, db, EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit2.ID,
			GitRef:   "vread456",
			Agent:    "codex",
		}, "No issues found.")

		// Simulate legacy row by setting verdict_bool to NULL
		if _, err := db.Exec(`UPDATE reviews SET verdict_bool = NULL WHERE job_id = ?`, job.ID); err != nil {
			require.NoError(t, err, "nullify verdict_bool: %v")
		}

		review, err := db.GetReviewByJobID(job.ID)
		require.NoError(t, err, "GetReviewByJobID: %v")

		assert.Nil(t, review.VerdictBool)
		// Should still get correct verdict via ParseVerdict fallback
		assert.False(t, review.Job == nil || review.Job.Verdict == nil || *review.Job.Verdict != "P")
	})
}

func TestGetReviewByCommitSHAUsesStoredVerdict(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/verdict-sha-test")
	commit := createCommit(t, db, repo.ID, "shav123")

	_ = createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "shav123",
		Agent:    "codex",
	}, "- High — Bug found")

	review, err := db.GetReviewByCommitSHA("shav123")
	require.NoError(t, err, "GetReviewByCommitSHA: %v")

	assert.False(t, review.VerdictBool == nil || *review.VerdictBool != 0)
	assert.False(t, review.Job == nil || review.Job.Verdict == nil || *review.Job.Verdict != "F")
}

// createCompletedJobWithOptions helper creates a job, claims it, and completes it.
func createCompletedJobWithOptions(t *testing.T, db *DB, opts EnqueueOpts, output string) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(opts)
	require.NoError(t, err, "EnqueueJob failed: %v")

	claimed, err := db.ClaimJob("test-worker")
	require.NoError(t, err, "ClaimJob failed: %v")

	assert.NotNil(t, claimed)
	assert.Equal(t, claimed.ID, job.ID)

	agent := opts.Agent
	if agent == "" {
		agent = "test-agent"
	}

	if err := db.CompleteJob(job.ID, agent, "prompt", output); err != nil {
		require.NoError(t, err, "CompleteJob failed: %v")
	}

	// Refresh job to get updated status/fields
	updatedJob, err := db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID failed: %v")

	assert.Equal(t, JobStatusDone, updatedJob.Status)
	return updatedJob
}

func TestGetReviewByJobIDIncludesBranch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")

	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"branch populated when set", "main", "main"},
		{"branch empty when not set", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := createCompletedJobWithOptions(t, db, EnqueueOpts{
				RepoID: repo.ID,
				GitRef: "sha-" + tt.name,
				Branch: tt.branch,
			}, "output")

			review, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			require.NotNil(t, review.Job)
			assert.Equal(t, tt.want, review.Job.Branch)
		})
	}
}

func TestGetReviewByCommitSHAIncludesBranch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")

	job := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID: repo.ID,
		GitRef: "branch-sha-test",
		Branch: "feature/x",
	}, "output")

	review, err := db.GetReviewByCommitSHA(job.GitRef)
	require.NoError(t, err)
	require.NotNil(t, review.Job)
	assert.Equal(t, "feature/x", review.Job.Branch)
}

// TestGetReviewByCommitSHAIgnoresNonReviewJobs verifies that a newer non-review
// job sharing the same git_ref (e.g. a fix job, which inherits the parent's ref)
// does not shadow an existing completed review. The lookup must still resolve the
// canonical SHA-review row, not return ErrNoRows.
func TestGetReviewByCommitSHAIgnoresNonReviewJobs(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/shadow-test")
	commit := createCommit(t, db, repo.ID, "shadow123")

	reviewJob := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "shadow123",
		Agent:    "codex",
	}, "No issues found.")

	// A newer fix job inherits the parent's git_ref. Without scoping the
	// lookup to review-producing job types, this shadows the real review.
	fixJob, err := db.EnqueueJob(EnqueueOpts{
		RepoID:      repo.ID,
		GitRef:      "shadow123",
		Agent:       "codex",
		JobType:     JobTypeFix,
		ParentJobID: reviewJob.ID,
	})
	require.NoError(t, err)
	assert.Greater(fixJob.ID, reviewJob.ID, "fix job is newer")

	review, err := db.GetReviewByCommitSHA("shadow123")
	require.NoError(t, err, "fix job must not shadow the review")
	require.NotNil(t, review.Job)
	assert.Equal(reviewJob.ID, review.JobID, "resolves the canonical review job")
	assert.Equal("No issues found.", review.Output)
}

// TestGetReviewByCommitSHAResolvesSynthesisOverMember verifies that for a panel
// run, a newer synthesis job (a review-producing type) is resolved as the
// canonical review for the SHA, never an individual member job.
func TestGetReviewByCommitSHAResolvesSynthesisOverMember(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/synthesis-canonical")
	runUUID := GenerateUUID()

	member := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:       repo.ID,
		GitRef:       "synth123",
		Agent:        "codex",
		JobType:      JobTypeReview,
		PanelRunUUID: runUUID,
		PanelRole:    PanelRoleMember,
	}, "member output")

	synth := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:       repo.ID,
		GitRef:       "synth123",
		Agent:        "codex",
		JobType:      JobTypeSynthesis,
		PanelRunUUID: runUUID,
		PanelRole:    PanelRoleSynthesis,
	}, "synthesis output")
	assert.Greater(synth.ID, member.ID, "synthesis is newer than the member")

	review, err := db.GetReviewByCommitSHA("synth123")
	require.NoError(t, err)
	assert.Equal(synth.ID, review.JobID, "synthesis is the canonical review")
	assert.Equal("synthesis output", review.Output)
}

func TestGetAllReviewsForGitRefExcludesPanelMembers(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/panel-previous-attempts")
	runUUID := GenerateUUID()

	member := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:       repo.ID,
		GitRef:       "panel-ref",
		Agent:        "codex",
		JobType:      JobTypeReview,
		PanelRunUUID: runUUID,
		PanelRole:    PanelRoleMember,
	}, "member output")
	synth := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:       repo.ID,
		GitRef:       "panel-ref",
		Agent:        "codex",
		JobType:      JobTypeSynthesis,
		PanelRunUUID: runUUID,
		PanelRole:    PanelRoleSynthesis,
	}, "synthesis output")

	reviews, err := db.GetAllReviewsForGitRef("panel-ref")
	require.NoError(t, err)

	require.Len(t, reviews, 1, "panel member reviews must not feed sibling prompts")
	assert.Equal(t, synth.ID, reviews[0].JobID)
	assert.NotEqual(t, member.ID, reviews[0].JobID)
}

func TestFindReusableSessionCandidatesExcludesPanelAndNonReviewJobs(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/session-candidates")
	branch := "feature/session"
	runUUID := GenerateUUID()

	normalCommit := createCommit(t, db, repo.ID, "session-normal")
	normal := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   normalCommit.ID,
		GitRef:     "session-normal",
		Branch:     branch,
		Agent:      "codex",
		ReviewType: "default",
		JobType:    JobTypeReview,
	}, "normal output")
	setJobSession(t, db, normal.ID, "session-normal")

	member := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:       repo.ID,
		GitRef:       "session-member",
		Branch:       branch,
		Agent:        "codex",
		ReviewType:   "default",
		JobType:      JobTypeReview,
		PanelRunUUID: runUUID,
		PanelRole:    PanelRoleMember,
	}, "member output")
	setJobSession(t, db, member.ID, "session-member")

	synth := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:       repo.ID,
		GitRef:       "session-synthesis",
		Branch:       branch,
		Agent:        "codex",
		ReviewType:   "default",
		JobType:      JobTypeSynthesis,
		PanelRunUUID: runUUID,
		PanelRole:    PanelRoleSynthesis,
	}, "synthesis output")
	setJobSession(t, db, synth.ID, "session-synthesis")

	fix := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:      repo.ID,
		GitRef:      "session-fix",
		Branch:      branch,
		Agent:       "codex",
		ReviewType:  "default",
		JobType:     JobTypeFix,
		ParentJobID: normal.ID,
	}, "fix output")
	setJobSession(t, db, fix.ID, "session-fix")

	candidates, err := db.FindReusableSessionCandidates(
		repo.ID, branch, "codex", "default", "", 10,
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(normal.ID, candidates[0].ID)
	assert.Equal("session-normal", candidates[0].SessionID)
	assert.NotEqual(member.ID, candidates[0].ID)
	assert.NotEqual(synth.ID, candidates[0].ID)
	assert.NotEqual(fix.ID, candidates[0].ID)
}

func TestFindReusableSessionCandidatesIncludesRangeReviewJobs(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/session-range-candidates")
	branch := "feature/session"
	rangeJob := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:     repo.ID,
		GitRef:     "base..head",
		Branch:     branch,
		Agent:      "codex",
		ReviewType: "default",
		JobType:    JobTypeRange,
	}, "range output")
	setJobSession(t, db, rangeJob.ID, "session-range")

	candidates, err := db.FindReusableSessionCandidates(
		repo.ID, branch, "codex", "default", "", 10,
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(rangeJob.ID, candidates[0].ID)
	assert.Equal("session-range", candidates[0].SessionID)
}

func TestFindReusableSessionCandidatesIncludesDirtyReviewJobs(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/session-dirty-candidates")
	commit := createCommit(t, db, repo.ID, "dirty-base-sha")
	branch := "feature/session"
	dirtyJob := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     "dirty",
		Branch:     branch,
		Agent:      "codex",
		ReviewType: "default",
		JobType:    JobTypeDirty,
		DiffContent: "diff --git a/file.go b/file.go\n" +
			"--- a/file.go\n" +
			"+++ b/file.go\n" +
			"@@ -1 +1 @@\n" +
			"-old\n" +
			"+new\n",
	}, "dirty output")
	setJobSession(t, db, dirtyJob.ID, "session-dirty")

	candidates, err := db.FindReusableSessionCandidates(
		repo.ID, branch, "codex", "default", "", 10,
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(dirtyJob.ID, candidates[0].ID)
	assert.Equal("session-dirty", candidates[0].SessionID)
	assert.Equal("dirty", candidates[0].GitRef)
}

func setJobSession(t *testing.T, db *DB, jobID int64, sessionID string) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET session_id = ? WHERE id = ?`, sessionID, jobID)
	require.NoError(t, err)
}

// verifyComment helper checks if a comment matches expected values.
func verifyComment(t *testing.T, actual Response, expectedUser, expectedMsg string) {
	t.Helper()
	assert.Equal(t, expectedUser, actual.Responder)
	assert.Equal(t, expectedMsg, actual.Response)
}
