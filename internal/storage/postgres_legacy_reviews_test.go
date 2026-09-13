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
