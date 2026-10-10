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

	cursor := ""
	for {
		jobs, next, err := pool.PullJobs(ctx, uuid.New(), cursor, 500)
		require.NoError(t, err)
		for _, pulled := range jobs {
			if pulled.UUID == jobUUID {
				assert.Equal(t, "prompt kept only on the review", pulled.Prompt)
				return
			}
		}
		require.NotEmpty(t, jobs, "pulled every job without finding the test job")
		cursor = next
	}
}
