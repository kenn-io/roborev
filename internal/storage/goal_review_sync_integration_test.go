//go:build postgres

package storage

import (
	"encoding/json/jsontext"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationGoalReviewSyncRoundTrip(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	assert := assert.New(t)
	env := newIntegrationEnv(t)
	const identity = "https://example.com/goal-review.git"
	source := env.openDB("source.db")
	repo, err := source.GetOrCreateRepo(filepath.Join(env.TmpDir, "source"), identity)
	require.NoError(t, err)
	job, err := source.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, Agent: "test", GitRef: "snapshot-digest", JobType: JobTypeGoalReview,
		ReviewType: "goal", Prompt: "frozen prompt", PromptPrebuilt: true,
	})
	require.NoError(t, err)
	_, err = source.ClaimJob("worker")
	require.NoError(t, err)
	require.NoError(t, source.CompleteJobResult(job.ID, "test", job.Prompt, ReviewCompletion{
		Output:  "- medium: plan.md:4: The plan omits a required outcome. Fix: Add the missing outcome.",
		Verdict: VerdictFail, StructuredOutput: jsontext.Value(goalReviewDocument),
	}))
	sourceWorker := startSyncWorkerNoSync(t, source, env.pgURL, "goal-source", "1h")
	stats, err := sourceWorker.SyncNow()
	require.NoError(t, err)
	assert.Equal(1, stats.PushedReviews)

	target := env.openDB("target.db")
	_, err = target.GetOrCreateRepo(filepath.Join(env.TmpDir, "target"), identity)
	require.NoError(t, err)
	targetWorker := startSyncWorkerNoSync(t, target, env.pgURL, "goal-target", "1h")
	stats, err = targetWorker.SyncNow()
	require.NoError(t, err)
	assert.Equal(1, stats.PulledReviews)

	jobs, err := target.ListJobs("", "", 10, 0, WithJobType(JobTypeGoalReview))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(new("F"), jobs[0].Verdict)
	review, err := target.GetReviewByJobID(jobs[0].ID)
	require.NoError(t, err)
	assert.Equal(goalReviewMarkdown, review.Output)
	assert.Equal(VerdictFail, review.Verdict())
	assert.Equal([]any{map[string]any{
		"severity": "medium", "problem": "The plan omits a required outcome.",
		"fix": "Add the missing outcome.", "location": "plan.md:4",
	}}, review.StructuredOutput["findings"])
}

func TestIntegrationGoalReviewSyncRequiresDocument(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobID, reviewID := uuid.New(), uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	_, err := pool.Pool().Exec(ctx, `UPDATE review_jobs SET job_type = 'goal_review' WHERE uuid = $1`, jobID)
	require.NoError(t, err)
	require.NoError(t, pool.UpsertReview(ctx, SyncableReview{
		UUID: reviewID, JobUUID: jobID, Agent: "test", Prompt: "frozen prompt", Output: "No issues found.",
		UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
	}))
	var count int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews WHERE uuid = $1`, reviewID).Scan(&count))
	assert.Zero(t, count, "prose-only goal reviews must not be pushed")

	_, err = pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, updated_by_machine_id)
 VALUES ($1, $2, 'test', 'frozen prompt', 'No issues found.', $3)`, uuid.New(), jobID, defaultTestMachineID)
	require.NoError(t, err)
	pulled, _, err := pool.PullReviews(ctx, uuid.New(), []uuid.UUID{jobID}, "", 10)
	require.NoError(t, err)
	assert.Empty(t, pulled, "prose-only goal reviews must not be pulled")
}
