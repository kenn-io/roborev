//go:build postgres

package storage

import (
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationLegacyReviewMigration(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobID := uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	reviewID := uuid.New()
	_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, updated_by_machine_id)
 VALUES ($1, $2, 'test', 'prompt', 'Legacy finding with missing details', $3)`, reviewID, jobID, defaultTestMachineID)
	require.NoError(t, err)
	require.NoError(t, pool.migrateLegacyReviews(ctx))
	var original, reason string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output', migration_error FROM legacy_reviews WHERE uuid = $1`, reviewID).Scan(&original, &reason))
	assert.Equal(t, "Legacy finding with missing details", original)
	assert.Contains(t, reason, "AI conversion required")
	var active int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews WHERE uuid = $1`, reviewID).Scan(&active))
	assert.Zero(t, active)
	require.NoError(t, pool.migrateLegacyReviews(ctx))
	raw := json.RawMessage(`{"schema_version":2,"summary":"Converted review.","verdict":"pass","findings":[]}`)
	require.NoError(t, pool.UpsertReview(ctx, SyncableReview{
		UUID: reviewID, JobUUID: jobID,
		Agent: "test", Prompt: "prompt", StructuredOutput: raw, UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
	}))
	var resolved bool
	var output string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT resolved_at IS NOT NULL FROM legacy_reviews WHERE uuid = $1`, reviewID).Scan(&resolved))
	assert.True(t, resolved)
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT output FROM reviews WHERE uuid = $1`, reviewID).Scan(&output))
	assert.Empty(t, output)
}

func TestIntegrationLegacyReviewExplicitConversion(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobID, reviewID := uuid.New(), uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	runID := uuid.New()
	_, err := pool.Pool().Exec(ctx, `UPDATE review_jobs SET job_type = 'synthesis', panel_run_uuid = $1, panel_role = 'synthesis' WHERE uuid = $2`, runID, jobID)
	require.NoError(t, err)
	for i, status := range []string{"failed", "done"} {
		memberID := uuid.New()
		createTestJob(t, pool.Pool(), TestJobOpts{UUID: memberID, RepoID: repoID, CommitID: commitID, Status: status})
		_, err := pool.Pool().Exec(ctx, `UPDATE review_jobs SET panel_run_uuid = $1, panel_role = 'member', panel_member_index = $2, review_type = 'security' WHERE uuid = $3`, runID, i, memberID)
		require.NoError(t, err)
		if status == "done" {
			require.NoError(t, pool.UpsertReview(ctx, SyncableReview{UUID: uuid.New(), JobUUID: memberID, Agent: "test", Output: "No issues found.", UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now()}))
		}
	}
	incoming := SyncableReview{UUID: reviewID, JobUUID: jobID, Agent: "test", Prompt: "original prompt", Output: "Legacy finding", UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now()}
	require.NoError(t, pool.UpsertReview(ctx, incoming))
	records, err := pool.UnresolvedLegacyReviews(ctx)
	require.NoError(t, err)
	var record PostgresLegacyReview
	for _, candidate := range records {
		if candidate.ID == reviewID {
			record = candidate
		}
	}
	require.Len(t, record.Sources, 1)
	assert.Equal(t, 1, record.Sources[0].Number)
	assert.Equal(t, "test (security)", record.Sources[0].Agent)
	assert.Equal(t, reviewID, record.ID)
	assert.Equal(t, "Legacy finding", record.Output)
	raw := json.RawMessage(`{"schema_version":2,"summary":"Converted review.","verdict":"pass","findings":[]}`)
	require.Error(t, pool.ResolveLegacyReview(ctx, reviewID, json.RawMessage(`invalid`)))
	require.NoError(t, pool.ResolveLegacyReview(ctx, reviewID, raw))
	var output, prompt, stored, original string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT output, prompt, structured_output::text FROM reviews WHERE uuid = $1`, reviewID).Scan(&output, &prompt, &stored))
	assert.Empty(t, output)
	assert.Equal(t, "original prompt", prompt)
	assert.Contains(t, stored, `"source_labels": ["test (security)"]`)
	require.NoError(t, pool.UpsertReview(ctx, incoming))
	records, err = pool.UnresolvedLegacyReviews(ctx)
	require.NoError(t, err)
	for _, record := range records {
		assert.NotEqual(t, reviewID, record.ID)
	}
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output' FROM legacy_reviews WHERE uuid = $1`, reviewID).Scan(&original))
	assert.Equal(t, "Legacy finding", original)
}
