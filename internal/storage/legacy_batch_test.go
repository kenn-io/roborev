package storage

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyMigrationCommitsBatchesAndResumes(t *testing.T) {
	env := setupJobEnv(t, t.TempDir(), "batch-migration")
	// More than two batches, including a partial final batch.
	const count = 205
	var blockedJob int64
	for i := range count {
		fixture := seedLegacyMarkdownReview(t, env.db, env.repo.ID, fmt.Sprintf("batch-%d", i), "Keep this original historical text.", 0, true)
		if i == 100 {
			blockedJob = fixture.jobID
		}
	}
	// Fail a write in the second batch. The first batch must already be
	// committed, while the failing batch must retain its original active rows.
	_, err := env.db.Exec(fmt.Sprintf(`CREATE TRIGGER stop_archive_batch BEFORE DELETE ON reviews
 WHEN OLD.job_id = %d BEGIN SELECT RAISE(ABORT, 'stop second batch'); END`, blockedJob))
	require.NoError(t, err)
	require.ErrorContains(t, env.db.migrateLegacyReviews(), "stop second batch")
	var archived, active int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM legacy_reviews`).Scan(&archived))
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM reviews`).Scan(&active))
	assert.Equal(t, 100, archived)
	assert.Equal(t, 105, active)
	_, err = env.db.Exec(`DROP TRIGGER stop_archive_batch`)
	require.NoError(t, err)
	require.NoError(t, env.db.migrateLegacyReviews())
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM legacy_reviews`).Scan(&archived))
	assert.Equal(t, count, archived)

	first, err := env.db.unresolvedLegacyReviews(0, 100, true)
	require.NoError(t, err)
	require.Len(t, first, 100)
	second, err := env.db.unresolvedLegacyReviews(first[99].ID, 100, true)
	require.NoError(t, err)
	require.Len(t, second, 100)
	assert.Greater(t, second[0].ID, first[99].ID)
	last, err := env.db.unresolvedLegacyReviews(second[99].ID, 100, true)
	require.NoError(t, err)
	require.Len(t, last, 5)

	_, err = env.db.Exec(fmt.Sprintf(`CREATE TRIGGER stop_restore_batch BEFORE INSERT ON reviews
 WHEN NEW.job_id = %d BEGIN SELECT RAISE(ABORT, 'stop restoration'); END`, blockedJob))
	require.NoError(t, err)
	require.ErrorContains(t, env.db.restoreLegacyReviews(), "stop restoration")
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM reviews`).Scan(&active))
	assert.Equal(t, 100, active)
	missing, err := env.db.unresolvedLegacyReviews(0, 100, true)
	require.NoError(t, err)
	require.NotEmpty(t, missing)
	assert.Equal(t, blockedJob, missing[0].JobID)
	_, err = env.db.Exec(`DROP TRIGGER stop_restore_batch`)
	require.NoError(t, err)
	require.NoError(t, env.db.restoreLegacyReviews())
	require.NoError(t, env.db.restoreLegacyReviews())
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM reviews r JOIN legacy_reviews l ON r.uuid = l.uuid
 WHERE r.id = l.id AND r.job_id = l.job_id AND r.closed = l.closed
 AND json_extract(r.structured_output, '$.legacy.markdown') = l.output`).Scan(&active))
	assert.Equal(t, count, active)
	missing, err = env.db.unresolvedLegacyReviews(0, 100, true)
	require.NoError(t, err)
	assert.Empty(t, missing)
	// Paging startup restoration must not truncate the public conversion export.
	exported, err := env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	assert.Len(t, exported, count)
}

func TestVerdictBackfillContinuesPastUnchangedBatch(t *testing.T) {
	env := setupJobEnv(t, t.TempDir(), "verdict-batches")
	for i := range 105 {
		fixture := seedLegacyMarkdownReview(t, env.db, env.repo.ID, fmt.Sprintf("unknown-%d", i), "Unknown historical verdict.", nil, false)
		_, err := env.db.Exec(`UPDATE reviews SET output = '', structured_output = '{"schema_version":0,"legacy":{"markdown":"Unknown historical verdict.","recorded_verdict":null}}' WHERE job_id = ?`, fixture.jobID)
		require.NoError(t, err)
	}
	last := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "structured-last", "", nil, false)
	_, err := env.db.Exec(`UPDATE reviews SET structured_output = '{"schema_version":2,"summary":"Clean review.","verdict":"pass","findings":[]}' WHERE job_id = ?`, last.jobID)
	require.NoError(t, err)
	updated, err := env.db.BackfillVerdictBool()
	require.NoError(t, err)
	assert.Equal(t, 1, updated)
	var passed bool
	require.NoError(t, env.db.QueryRow(`SELECT verdict_bool FROM reviews WHERE job_id = ?`, last.jobID).Scan(&passed))
	assert.True(t, passed)
	updated, err = env.db.BackfillVerdictBool()
	require.NoError(t, err)
	assert.Zero(t, updated)
}
