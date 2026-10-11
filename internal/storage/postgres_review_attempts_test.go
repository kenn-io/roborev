//go:build postgres

package storage

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresMigrationReviewAttempts(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	assert := assert.New(t)
	pool := openTestPgPoolRawAtVersion(t, 23)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	attempt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	completed, failed, archived := uuid.New(), uuid.New(), uuid.New()
	oldReview, currentReview, failedReview, archivedReview := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, row := range []struct {
		job    uuid.UUID
		status string
	}{{completed, "done"}, {failed, "failed"}, {archived, "done"}} {
		createTestJob(t, pool.Pool(), TestJobOpts{UUID: row.job, RepoID: repoID, CommitID: commitID, Status: row.status, EnqueuedAt: attempt})
	}
	for i, row := range []struct{ review, job uuid.UUID }{{oldReview, completed}, {currentReview, completed}, {failedReview, failed}} {
		_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, structured_output, updated_by_machine_id, created_at)
			VALUES ($1, $2, 'test', 'historical prompt', '', $3, $4, $5)`, row.review, row.job,
			string(reviewFixtureJSON("No issues found.")), defaultTestMachineID, attempt.Add(time.Duration(i)*time.Second))
		require.NoError(t, err)
	}
	_, err := pool.Pool().Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error)
		VALUES ($1, jsonb_build_object('uuid', $1::uuid, 'job_uuid', $2::uuid, 'agent', 'test', 'prompt', 'archived prompt',
		'output', 'A historical finding without enough detail.', 'closed', false, 'updated_by_machine_id', $3::uuid, 'created_at', $4::timestamptz), 'historical')`,
		archivedReview, archived, defaultTestMachineID, attempt)
	require.NoError(t, err)
	// Force failure at version recording. The association backfill must roll
	// back and remain retryable, even though restoration already finished.
	_, err = pool.Pool().Exec(ctx, `CREATE FUNCTION roborev.reject_v24() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.version = 24 THEN RAISE EXCEPTION 'synthetic version failure'; END IF; RETURN NEW; END $$`)
	require.NoError(t, err)
	_, err = pool.Pool().Exec(ctx, `CREATE TRIGGER reject_v24 BEFORE INSERT ON schema_version FOR EACH ROW EXECUTE FUNCTION roborev.reject_v24()`)
	require.NoError(t, err)
	require.ErrorContains(t, pool.EnsureSchema(ctx), "synthetic version failure")
	var version, associated int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version))
	assert.Equal(23, version)
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews WHERE job_enqueued_at IS NOT NULL`).Scan(&associated))
	assert.Zero(associated)
	_, err = pool.Pool().Exec(ctx, `DROP TRIGGER reject_v24 ON schema_version`)
	require.NoError(t, err)
	require.NoError(t, pool.EnsureSchema(ctx))
	for _, id := range []uuid.UUID{currentReview, archivedReview} {
		var producing time.Time
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT job_enqueued_at FROM reviews WHERE uuid = $1`, id).Scan(&producing))
		assert.Equal(attempt, producing.UTC())
	}
	for _, id := range []uuid.UUID{oldReview, failedReview} {
		var producing *time.Time
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT job_enqueued_at FROM reviews WHERE uuid = $1`, id).Scan(&producing))
		assert.Nil(producing)
	}
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version))
	assert.Equal(24, version)
	var original, archiveAttempt string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output', record->>'job_enqueued_at' FROM legacy_reviews WHERE uuid = $1`, archivedReview).Scan(&original, &archiveAttempt))
	assert.Equal("A historical finding without enough detail.", original)
	assert.NotEmpty(archiveAttempt)
	// Restoring this archive after a newer attempt must retain its original
	// identity. Normal opens must never repeat the historical backfill.
	_, err = pool.Pool().Exec(ctx, `UPDATE review_jobs SET enqueued_at = $1, status = 'done'`, attempt.Add(time.Hour))
	require.NoError(t, err)
	_, err = pool.Pool().Exec(ctx, `DELETE FROM reviews WHERE uuid = $1`, archivedReview)
	require.NoError(t, err)
	_, err = pool.Pool().Exec(ctx, `UPDATE legacy_reviews SET resolved_at = NULL WHERE uuid = $1`, archivedReview)
	require.NoError(t, err)
	require.NoError(t, pool.EnsureSchema(ctx))
	pulled, _, err := pool.PullReviews(ctx, uuid.New(), []uuid.UUID{completed, failed, archived}, "", 10)
	require.NoError(t, err)
	assert.Empty(pulled)
	assert.Empty(pulledJobPrompt(t, pool, archived), "obsolete archive cannot supply a new attempt's prompt")
	var count int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews`).Scan(&count))
	assert.Equal(4, count, "historical rows remain stored")
}
