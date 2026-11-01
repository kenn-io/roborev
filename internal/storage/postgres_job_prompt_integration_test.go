//go:build postgres

package storage

import (
	"database/sql"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationJobPromptFollowsLaterAttempts(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	machineID := uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, machineID, "test"))
	repoID, err := pool.GetOrCreateRepo(ctx, "test-repo-prompt-"+uuid.New().String())
	require.NoError(t, err)
	job := SyncableJob{
		UUID: uuid.New(), GitRef: "a..b", Agent: "test", Status: "failed",
		SourceMachineID: machineID, EnqueuedAt: time.Now(), Prompt: "first attempt",
	}
	promptInPostgres := func() string {
		var prompt string
		var enqueuedAt time.Time
		var owner uuid.UUID
		require.NoError(t, pool.pool.QueryRow(ctx, `SELECT COALESCE(prompt, ''), enqueued_at, source_machine_id FROM review_jobs WHERE uuid = $1`, job.UUID).Scan(&prompt, &enqueuedAt, &owner))
		assert.Equal(t, job.SourceMachineID, owner)
		assert.Equal(t, job.EnqueuedAt.UTC().Truncate(time.Microsecond), enqueuedAt.UTC())
		return prompt
	}

	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	older := job
	job.SourceMachineID = uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, job.SourceMachineID, "rerun"))
	job.Status, job.Prompt = "done", "second attempt"
	job.EnqueuedAt = job.EnqueuedAt.Add(time.Millisecond)
	_, err = pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: job, PgRepoID: repoID}})
	require.NoError(t, err)
	assert.Equal(t, "second attempt", promptInPostgres())

	job.Prompt = "third attempt"
	job.SourceMachineID = machineID
	job.EnqueuedAt = job.EnqueuedAt.Add(time.Millisecond)
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	assert.Equal(t, "third attempt", promptInPostgres())

	// A delayed push from the previous owner cannot reclaim the job.
	require.NoError(t, pool.UpsertJob(ctx, older, repoID, nil))
	assert.Equal(t, "third attempt", promptInPostgres())
	_, err = pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: older, PgRepoID: repoID}})
	require.NoError(t, err)
	assert.Equal(t, "third attempt", promptInPostgres())

	// A machine whose retention removed the prompt does not erase it here.
	job.Prompt = ""
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	assert.Equal(t, "third attempt", promptInPostgres())
}

func TestIntegrationImportedRerunSyncsContent(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	assert := assert.New(t)
	env := newIntegrationEnv(t)
	a := env.setupNode("source", "rerun-repository", "1h")
	b := env.setupNode("rerun", "rerun-repository", "1h")
	job, originalReview := createCompletedReview(t, a.DB, a.Repo.ID, "abc123", "Author", "Subject", "original prompt", "Original review.")
	// The importing machine's clock is behind the original executor's.
	_, err := a.DB.ExecContext(t.Context(), `UPDATE review_jobs SET enqueued_at = ? WHERE id = ?`, preciseTimestampAt(time.Now().Add(time.Hour)), job.ID)
	require.NoError(t, err)
	_, err = a.DB.ExecContext(t.Context(), `UPDATE reviews SET reviewed_file_count = 3, excluded_file_count = 1 WHERE id = ?`, originalReview.ID)
	require.NoError(t, err)
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	machineA, err := a.DB.GetMachineID()
	require.NoError(t, err)
	machineB, err := b.DB.GetMachineID()
	require.NoError(t, err)
	remote, _, err := env.Pool.PullJobs(t.Context(), machineB, "", 10)
	require.NoError(t, err)
	require.Len(t, remote, 1)
	var localID int64
	require.NoError(t, b.DB.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&localID))
	require.NoError(t, b.DB.ReenqueueJob(localID, ReenqueueOpts{Agent: "second-agent"}))
	claimed, err := b.DB.ClaimJob("rerun-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, localID, claimed.ID)
	require.NoError(t, b.DB.SaveJobPrompt(localID, "complete rerun prompt"))
	require.NoError(t, b.DB.CompleteJobResult(localID, "second-agent", ReviewCompletion{
		StructuredOutput: reviewFixtureJSON("Rerun review."),
	}))
	comment, err := b.DB.AddCommentToJob(localID, "reviewer", "Rerun follow-up.")
	require.NoError(t, err)
	// Force the equal-second history case: PostgreSQL's later row ID must
	// select the rerun even when both review creation timestamps are equal.
	_, err = b.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at = ? WHERE job_id = ?`, originalReview.CreatedAt.Format(time.RFC3339), localID)
	require.NoError(t, err)
	changed, err := b.DB.upsertPulledJob(remote[0], b.Repo.ID, nil)
	require.NoError(t, err)
	assert.False(changed)
	pending, err := b.DB.GetJobsToSync(machineB, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal("complete rerun prompt", pending[0].Prompt)

	stats, err := b.Worker.SyncNow()
	require.NoError(t, err)
	assert.Equal(1, stats.PushedJobs)
	assert.Equal(1, stats.PushedReviews)
	assert.Equal(1, stats.PushedResponses)
	for range 2 {
		_, err = a.Worker.SyncNow()
		require.NoError(t, err)
		_, err = b.Worker.SyncNow()
		require.NoError(t, err)
	}
	for _, db := range []*DB{a.DB, b.DB} {
		var id int64
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&id))
		current, err := db.GetJobByID(id)
		require.NoError(t, err)
		assert.Equal("complete rerun prompt", current.Prompt)
		assert.Equal("second-agent", current.Agent)
		require.NotNil(t, current.StartedAt)
		assert.Equal(claimed.StartedAt.UTC().Truncate(time.Microsecond), current.StartedAt.UTC().Truncate(time.Microsecond))
		require.NotNil(t, current.SourceMachineID)
		assert.Equal(machineB, *current.SourceMachineID)
		review, err := db.GetReviewByJobID(id)
		require.NoError(t, err)
		assert.Equal("Rerun review.", review.StructuredOutput["summary"])
		assert.Equal("second-agent", review.Agent)
		var reviewed, excluded sql.NullInt64
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT reviewed_file_count, excluded_file_count FROM reviews WHERE id = ?`, review.ID).Scan(&reviewed, &excluded))
		assert.False(reviewed.Valid)
		assert.False(excluded.Valid)
		comments, err := db.GetCommentsForJob(id)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		assert.Equal(comment.Response, comments[0].Response)
		jobs, err := db.GetJobsToSync(machineA, 10)
		require.NoError(t, err)
		assert.Empty(jobs, "the previous owner must not republish the older attempt")
	}
}

// pulledJobPrompt pages through PullJobs until it finds the job.
func pulledJobPrompt(t *testing.T, pool *PgPool, jobUUID uuid.UUID) string {
	t.Helper()
	cursor := ""
	for {
		jobs, next, err := pool.PullJobs(t.Context(), uuid.New(), cursor, 500)
		require.NoError(t, err)
		for _, pulled := range jobs {
			if pulled.UUID == jobUUID {
				return pulled.Prompt
			}
		}
		require.NotEmpty(t, jobs, "pulled every job without finding the test job")
		cursor = next
	}
}

func TestIntegrationPullJobsPrefersNewestReviewPrompt(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	machineID := uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, machineID, "test"))
	repoID, err := pool.GetOrCreateRepo(ctx, "test-repo-newest-review-"+uuid.New().String())
	require.NoError(t, err)
	insertTypedJob := func(jobType, status, prompt string, reviewPrompts ...string) uuid.UUID {
		jobUUID := uuid.New()
		_, err := pool.pool.Exec(ctx, `
			INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, job_type, status, prompt, source_machine_id, enqueued_at, created_at, updated_at)
			VALUES ($1, $2, 'a..b', 'test', $3, $4, $5, $6, NOW(), NOW(), NOW())`, jobUUID, repoID, jobType, status, prompt, machineID)
		require.NoError(t, err)
		created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		for i, reviewPrompt := range reviewPrompts {
			_, err = pool.pool.Exec(ctx, `
				INSERT INTO reviews (uuid, job_uuid, job_enqueued_at, job_source_machine_id, agent, prompt, output, structured_output, updated_by_machine_id, created_at)
				VALUES ($1, $2, (SELECT enqueued_at FROM review_jobs WHERE uuid = $2), (SELECT source_machine_id FROM review_jobs WHERE uuid = $2), 'test', $3, '', $4, $5, $6)`,
				uuid.New(), jobUUID, reviewPrompt, string(reviewFixtureJSON("No issues found.")), machineID,
				created.Add(time.Duration(i)*time.Hour))
			require.NoError(t, err)
		}
		return jobUUID
	}
	insertJob := func(status, prompt string, reviewPrompts ...string) uuid.UUID {
		return insertTypedJob(JobTypeRange, status, prompt, reviewPrompts...)
	}
	// pulledReviewJobPrompts returns the job prompt each pulled review carries.
	pulledReviewJobPrompts := func(jobUUID uuid.UUID) []string {
		reviews, _, err := pool.PullReviews(ctx, uuid.New(), []uuid.UUID{jobUUID}, "", 10)
		require.NoError(t, err)
		var enqueuedAt time.Time
		require.NoError(t, pool.pool.QueryRow(ctx, `SELECT enqueued_at FROM review_jobs WHERE uuid = $1`, jobUUID).Scan(&enqueuedAt))
		var prompts []string
		for _, review := range reviews {
			assert.Equal(t, enqueuedAt, review.JobEnqueuedAt)
			prompts = append(prompts, review.JobPrompt)
		}
		return prompts
	}

	// An older client never updates the job's prompt on a rerun; the rerun's
	// review holds the prompt the agent received.
	olderRerun := insertJob("done", "first attempt", "first attempt with preamble", "second attempt with preamble")
	assert.Equal(t, "second attempt with preamble", pulledJobPrompt(t, pool, olderRerun))
	assert.Equal(t, []string{"second attempt with preamble"},
		pulledReviewJobPrompts(olderRerun))

	// A newer client keeps the prompt on the job and pushes reviews without one.
	newerRerun := insertJob("done", "second attempt", "first attempt with preamble", "")
	assert.Equal(t, "second attempt", pulledJobPrompt(t, pool, newerRerun))

	// A rerun that failed has no review of its own; the remaining review
	// belongs to the earlier attempt.
	failedRerun := insertJob("failed", "failed attempt", "first attempt with preamble")
	assert.Equal(t, "failed attempt", pulledJobPrompt(t, pool, failedRerun))
	failedWithoutPrompt := insertJob("failed", "", "first attempt with preamble")
	assert.Empty(t, pulledJobPrompt(t, pool, failedWithoutPrompt))

	// An older client sent an oversized prompt as a file handoff line and
	// stored that line on the review; the job kept the complete prompt.
	oversized := insertJob("done", "complete oversized prompt",
		"Read the complete task prompt from \"/tmp/roborev-snapshot/prompt.md\" and carry out its instructions. "+
			"Read the file in full before starting.\n")
	assert.Equal(t, "complete oversized prompt", pulledJobPrompt(t, pool, oversized))
	assert.Equal(t, []string{"complete oversized prompt"}, pulledReviewJobPrompts(oversized))

	// A fix job reuses its stored prompt, here a plan envelope, on a rerun;
	// its review holds only the implementation prompt the agent received.
	planFix := insertTypedJob(JobTypeFix, "done", "roborev-fix-plan-v1{}", "implementation prompt")
	assert.Equal(t, "roborev-fix-plan-v1{}", pulledJobPrompt(t, pool, planFix))
}

func TestIntegrationPullJobsUsesReviewPromptForOlderHistory(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	machineID := uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, machineID, "test"))
	repoID, err := pool.GetOrCreateRepo(ctx, "test-repo-history-"+uuid.New().String())
	require.NoError(t, err)
	jobUUID := uuid.New()
	_, err = pool.pool.Exec(ctx, `
		INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, source_machine_id, enqueued_at, created_at, updated_at)
		VALUES ($1, $2, 'a..b', 'test', 'done', $3, NOW(), NOW(), NOW())`, jobUUID, repoID, machineID)
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `
		INSERT INTO reviews (uuid, job_uuid, job_enqueued_at, job_source_machine_id, agent, prompt, output, updated_by_machine_id)
		VALUES ($1, $2, (SELECT enqueued_at FROM review_jobs WHERE uuid = $2), (SELECT source_machine_id FROM review_jobs WHERE uuid = $2), 'test', 'prompt kept only on the review', 'No issues found.', $3)`, uuid.New(), jobUUID, machineID)
	require.NoError(t, err)

	assert.Equal(t, "prompt kept only on the review", pulledJobPrompt(t, pool, jobUUID))
}

func TestIntegrationMigrationRepushesLegacyRerunPrompt(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	for _, status := range []string{"done", "applied", "rebased"} {
		t.Run(status, func(t *testing.T) {
			assert := assert.New(t)
			pool := openTestPgPool(t)
			ctx := t.Context()
			path, db := openLegacyContentDB(t)
			repo := createRepo(t, db, t.TempDir())
			machineID, jobUUID, reviewUUID := uuid.New(), uuid.New(), uuid.New()
			finished := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			const completePrompt = "complete prompt of the successful rerun"
			const handoff = "Read the complete task prompt from \"/tmp/snapshot/prompt.md\" and carry out its instructions. " +
				"Read the file in full before starting.\n"
			require.NoError(t, pool.RegisterMachine(ctx, machineID, "test"))
			pgRepoID, err := pool.GetOrCreateRepo(ctx, "test-repo-migrated-prompt-"+uuid.New().String())
			require.NoError(t, err)
			// This is the state left by an older client's successful oversized rerun:
			// the job's first prompt remains in PostgreSQL and its review only names
			// the temporary file. SQLite has the complete prompt, marked as pushed.
			require.NoError(t, pool.UpsertJob(ctx, SyncableJob{
				UUID: jobUUID, GitRef: "a..b", Agent: "test", Status: "done", JobType: JobTypeRange,
				SourceMachineID: machineID, EnqueuedAt: finished.Add(-time.Hour), FinishedAt: &finished,
				Prompt: "prompt of the first attempt",
			}, pgRepoID, nil))
			_, err = pool.pool.Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, job_enqueued_at, job_source_machine_id, agent, prompt, output, structured_output, updated_by_machine_id)
		VALUES ($1, $2, (SELECT enqueued_at FROM review_jobs WHERE uuid = $2), (SELECT source_machine_id FROM review_jobs WHERE uuid = $2), 'test', $3, '', $4, $5)`, reviewUUID, jobUUID, handoff,
				string(reviewFixtureJSON("No issues found.")), machineID)
			require.NoError(t, err)
			assert.Equal("prompt of the first attempt", pulledJobPrompt(t, pool, jobUUID))

			_, err = db.ExecContext(t.Context(), `INSERT OR REPLACE INTO sync_state (key, value) VALUES ('machine_id', ?)`, machineID)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, job_type, prompt,
		source_machine_id, updated_at, synced_at, finished_at)
		VALUES (?, ?, 'a..b', 'test', 'done', 'range', ?, ?, ?, ?, ?)`,
				jobUUID, repo.ID, completePrompt, machineID, finished.Format(time.RFC3339),
				finished.Format(time.RFC3339), finished.Format(time.RFC3339))
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO reviews (uuid, synced_at, updated_at, updated_by_machine_id, job_id, agent, prompt, output, structured_output)
		SELECT ?, ?, ?, ?, id, 'test', ?, '', ? FROM review_jobs WHERE uuid = ?`, reviewUUID, finished.Format(time.RFC3339), finished.Format(time.RFC3339), machineID, handoff,
				string(reviewFixtureJSON("No issues found.")), jobUUID)
			require.NoError(t, err)
			jobType := JobTypeRange
			if status != "done" {
				jobType = JobTypeFix
			}
			_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET status = ?, job_type = ? WHERE uuid = ?`, status, jobType, jobUUID)
			require.NoError(t, err)
			_, err = pool.pool.Exec(ctx, `UPDATE reviews SET closed = TRUE, updated_by_machine_id = $1, updated_at = clock_timestamp() WHERE uuid = $2`, uuid.New(), reviewUUID)
			require.NoError(t, err)
			require.NoError(t, db.Close())

			db, err = Open(path)
			require.NoError(t, err)
			defer db.Close()
			removed, err := db.PruneJobPrompts(ctx, finished.AddDate(0, 0, 2), true)
			require.NoError(t, err)
			assert.Zero(removed, "the complete prompt must survive retention until its re-upload")
			jobs, err := db.GetJobsToSync(machineID, 10)
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			assert.Equal(completePrompt, jobs[0].Prompt)
			_, err = pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: jobs[0], PgRepoID: pgRepoID}})
			require.NoError(t, err)
			require.NoError(t, db.MarkJobsSynced([]JobSyncMark{NewJobSyncMark(jobs[0])}))
			reviews, err := db.GetReviewsToSync(machineID, 10)
			require.NoError(t, err)
			require.Len(t, reviews, 1, "a previously synced rerun must republish its producing attempt")
			_, err = pool.BatchUpsertReviews(ctx, reviews)
			require.NoError(t, err)
			pulled, _, err := pool.PullReviews(ctx, uuid.New(), []uuid.UUID{jobUUID}, "", 10)
			require.NoError(t, err)
			require.Len(t, pulled, 1)
			assert.Equal(reviewUUID, pulled[0].UUID)
			assert.True(pulled[0].Closed, "attempt repair must retain a newer peer closure")
			require.True(t, reviews[0].MetadataOnly)
			require.NoError(t, db.MarkReviewsSynced(reviews[:1]))
			ownerPull, _, err := pool.PullReviews(ctx, machineID, []uuid.UUID{jobUUID}, "", 10)
			require.NoError(t, err)
			require.Len(t, ownerPull, 1, "the owner must learn the preserved peer closure")
			require.NoError(t, db.UpsertPulledReview(ownerPull[0]))
			currentReview, err := db.GetReviewByJobID(jobs[0].ID)
			require.NoError(t, err)
			assert.True(currentReview.Closed)
			pendingReviews, err := db.GetReviewsToSync(machineID, 10)
			require.NoError(t, err)
			assert.Empty(pendingReviews, "acknowledging the repair must stop re-uploading it")
			removed, err = db.PruneJobPrompts(ctx, finished.AddDate(0, 0, 2), true)
			require.NoError(t, err)
			if status == "done" {
				assert.EqualValues(1, removed)
			} else {
				assert.Zero(removed)
			}
			local, err := db.GetJobByID(jobs[0].ID)
			require.NoError(t, err)
			assert.Equal(JobStatus(status), local.Status)
			var remoteStatus string
			require.NoError(t, pool.pool.QueryRow(ctx, `SELECT status FROM review_jobs WHERE uuid = $1`, jobUUID).Scan(&remoteStatus))
			assert.Equal("done", remoteStatus)
			pending, err := db.GetJobsToSync(machineID, 10)
			require.NoError(t, err)
			assert.Empty(pending)
			_, err = db.AddCommentToJob(local.ID, "reviewer", "Follow-up after upgrade.")
			require.NoError(t, err)
			comments, err := db.GetCommentsToSync(machineID, 10)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			_, err = pool.BatchInsertResponses(ctx, comments)
			require.NoError(t, err)
			assert.Equal(completePrompt, pulledJobPrompt(t, pool, jobUUID))
		})
	}
}
