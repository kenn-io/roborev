//go:build postgres

package storage

import (
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
		require.NoError(t, pool.pool.QueryRow(ctx, `SELECT COALESCE(prompt, '') FROM review_jobs WHERE uuid = $1`, job.UUID).Scan(&prompt))
		return prompt
	}

	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	job.Status, job.Prompt = "done", "second attempt"
	_, err = pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: job, PgRepoID: repoID}})
	require.NoError(t, err)
	assert.Equal(t, "second attempt", promptInPostgres())

	job.Prompt = "third attempt"
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	assert.Equal(t, "third attempt", promptInPostgres())

	// A machine whose retention removed the prompt does not erase it here.
	job.Prompt = ""
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	assert.Equal(t, "third attempt", promptInPostgres())
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
	insertJob := func(status, prompt string, reviewPrompts ...string) uuid.UUID {
		jobUUID := uuid.New()
		_, err := pool.pool.Exec(ctx, `
			INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, prompt, source_machine_id, enqueued_at, created_at, updated_at)
			VALUES ($1, $2, 'a..b', 'test', $3, $4, $5, NOW(), NOW(), NOW())`, jobUUID, repoID, status, prompt, machineID)
		require.NoError(t, err)
		created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		for i, reviewPrompt := range reviewPrompts {
			_, err = pool.pool.Exec(ctx, `
				INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, structured_output, updated_by_machine_id, created_at)
				VALUES ($1, $2, 'test', $3, '', $4, $5, $6)`,
				uuid.New(), jobUUID, reviewPrompt, string(reviewFixtureJSON("No issues found.")), machineID,
				created.Add(time.Duration(i)*time.Hour))
			require.NoError(t, err)
		}
		return jobUUID
	}
	// pulledReviewJobPrompts returns the job prompt each pulled review carries.
	pulledReviewJobPrompts := func(jobUUID uuid.UUID) []string {
		reviews, _, err := pool.PullReviews(ctx, uuid.New(), []uuid.UUID{jobUUID}, "", 10)
		require.NoError(t, err)
		var prompts []string
		for _, review := range reviews {
			prompts = append(prompts, review.JobPrompt)
		}
		return prompts
	}

	// An older client never updates the job's prompt on a rerun; the rerun's
	// review holds the prompt the agent received.
	olderRerun := insertJob("done", "first attempt", "first attempt with preamble", "second attempt with preamble")
	assert.Equal(t, "second attempt with preamble", pulledJobPrompt(t, pool, olderRerun))
	assert.Equal(t, []string{"second attempt with preamble", "second attempt with preamble"},
		pulledReviewJobPrompts(olderRerun))

	// A newer client keeps the prompt on the job and pushes reviews without one.
	newerRerun := insertJob("done", "second attempt", "first attempt with preamble", "")
	assert.Equal(t, "second attempt", pulledJobPrompt(t, pool, newerRerun))

	// A rerun that failed has no review of its own; the remaining review
	// belongs to the earlier attempt.
	failedRerun := insertJob("failed", "failed attempt", "first attempt with preamble")
	assert.Equal(t, "failed attempt", pulledJobPrompt(t, pool, failedRerun))
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
		INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, updated_by_machine_id)
		VALUES ($1, $2, 'test', 'prompt kept only on the review', 'No issues found.', $3)`, uuid.New(), jobUUID, machineID)
	require.NoError(t, err)

	assert.Equal(t, "prompt kept only on the review", pulledJobPrompt(t, pool, jobUUID))
}
