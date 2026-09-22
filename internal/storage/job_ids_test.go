package storage

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobIDsSurviveRepositoryDeletion(t *testing.T) {
	env := setupJobEnv(t, t.TempDir(), "job-id-history")
	old := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "archived", "Preserve this original review text.", 0, false)
	require.NoError(t, env.db.migrateLegacyReviews())
	var archives int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM legacy_reviews WHERE job_id = ?`, old.jobID).Scan(&archives))
	require.Equal(t, 1, archives)
	require.NoError(t, env.db.DeleteRepo(env.repo.ID, true))
	repo, err := env.db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	job, err := env.db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "replacement", Agent: "test"})
	require.NoError(t, err)
	assert.Greater(t, job.ID, old.jobID)
	require.NoError(t, env.db.restoreLegacyReviews())
	_, err = env.db.GetReviewByJobID(job.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM legacy_reviews WHERE job_id = ?`, old.jobID).Scan(&archives))
	assert.Zero(t, archives)
}

func TestMigrateJobIDsPreservesHistory(t *testing.T) {
	// Build the schema before the forward ID migration, as a shipped database.
	path := filepath.Join(t.TempDir(), "reviews.db")
	conn, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db := &DB{conn}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(schema)
	require.NoError(t, err)
	require.NoError(t, db.migrate())
	require.NoError(t, db.migrateLegacyReviews())
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	original := seedLegacyMarkdownReview(t, db, repo.ID, "retained", "Original text stays attached to this job.", 0, true)
	orphan := seedLegacyMarkdownReview(t, db, repo.ID, "orphan", "An orphan archive reserves its old ID.", 0, false)
	require.NoError(t, db.migrateLegacyReviews())
	_, err = db.Exec(`DELETE FROM review_jobs WHERE id = ?`, orphan.jobID)
	require.NoError(t, err)
	var indexesBefore int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'review_jobs'`).Scan(&indexesBefore))
	require.NoError(t, db.Close())
	db, err = Open(path)
	require.NoError(t, err)
	review, err := db.GetReviewByJobID(original.jobID)
	require.NoError(t, err)
	assert.Equal(t, original.jobID, review.JobID)
	assert.Contains(t, review.Output, original.markdown)
	assert.True(t, review.Closed)
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "after-upgrade", Agent: "test"})
	require.NoError(t, err)
	assert.Greater(t, job.ID, orphan.jobID)
	var indexesAfter int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'review_jobs'`).Scan(&indexesAfter))
	assert.Equal(t, indexesBefore, indexesAfter)
	require.NoError(t, db.migrateJobIDs())
	next, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "second-open", Agent: "test"})
	require.NoError(t, err)
	assert.Greater(t, next.ID, job.ID)
}
