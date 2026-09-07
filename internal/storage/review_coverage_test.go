package storage

import (
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewFileCoverageContract(t *testing.T) {
	zero, one, twentySeven, four := 0, 1, 27, 4
	assert.Equal(t, "0 files reviewed, 27 excluded",
		(&ReviewFileCoverage{Reviewed: &zero, Excluded: &twentySeven}).FormatSummary())
	assert.Equal(t, "4 files reviewed", (&ReviewFileCoverage{Reviewed: &four}).FormatSummary())
	assert.Equal(t, "27 files excluded", (&ReviewFileCoverage{Excluded: &twentySeven}).FormatSummary())
	assert.Equal(t, "1 file reviewed, 1 excluded",
		(&ReviewFileCoverage{Reviewed: &one, Excluded: &one}).FormatSummary())
	assert.Empty(t, (*ReviewFileCoverage)(nil).FormatSummary())
	assert.Nil(t, NormalizeReviewFileCoverage(&ReviewFileCoverage{}))

	data, err := json.Marshal(Review{FileCoverage: &ReviewFileCoverage{Reviewed: &zero, Excluded: &twentySeven}})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"reviewed":0`)
	assert.Contains(t, string(data), `"excluded":27`)

	data, err = json.Marshal(Review{})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "file_coverage")
}

func TestReviewFileCoveragePersistenceMigrationAndCancellation(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, commit, job := createJobChain(t, db, "/tmp/coverage-roundtrip", "coverage-sha")
	setJobStatus(t, db, job.ID, JobStatusRunning)
	zero, excluded := 0, 27
	require.NoError(t, db.CompleteJobResult(job.ID, "test", "prompt", ReviewCompletion{
		Output:       "PASS",
		FileCoverage: &ReviewFileCoverage{Reviewed: &zero, Excluded: &excluded},
	}))
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, review.FileCoverage)
	assert.Equal(t, 0, *review.FileCoverage.Reviewed)
	assert.Equal(t, 27, *review.FileCoverage.Excluded)
	byID, err := db.GetReviewByID(review.ID)
	require.NoError(t, err)
	require.NotNil(t, byID.FileCoverage)
	assert.Equal(t, 0, *byID.FileCoverage.Reviewed)

	data, err := json.Marshal(review)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"reviewed":0`)

	unknown := enqueueJob(t, db, repo.ID, commit.ID, "unknown-sha")
	setJobStatus(t, db, unknown.ID, JobStatusRunning)
	require.NoError(t, db.CompleteJobResult(unknown.ID, "test", "prompt", ReviewCompletion{Output: "PASS"}))
	unknownReview, err := db.GetReviewByJobID(unknown.ID)
	require.NoError(t, err)
	assert.Nil(t, unknownReview.FileCoverage)
	unknownJSON, err := json.Marshal(unknownReview)
	require.NoError(t, err)
	assert.NotContains(t, string(unknownJSON), "file_coverage")

	canceled := enqueueJob(t, db, repo.ID, commit.ID, "canceled-sha")
	setJobStatus(t, db, canceled.ID, JobStatusCanceled)
	require.NoError(t, db.CompleteJobResult(canceled.ID, "test", "prompt", ReviewCompletion{
		Output:       "PASS",
		FileCoverage: &ReviewFileCoverage{Reviewed: &zero},
	}))
	_, err = db.GetReviewByJobID(canceled.ID)
	require.Error(t, err)

	legacy := prepareMigratedDB(t, "coverage-legacy.db", legacyReviewJobSchema, legacyReviewJobSeed)
	var columns int
	require.NoError(t, legacy.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('reviews')
		WHERE name IN ('reviewed_file_count', 'excluded_file_count')
	`).Scan(&columns))
	assert.Equal(t, 2, columns)
	legacyReview, err := legacy.GetReviewByJobID(1)
	require.NoError(t, err)
	assert.Nil(t, legacyReview.FileCoverage)
}

func TestReviewFileCoveragePartialAndSync(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/coverage-sync", "sync-sha")
	setJobStatus(t, db, job.ID, JobStatusRunning)
	reviewed := 4
	require.NoError(t, db.CompleteJobResult(job.ID, "test", "prompt", ReviewCompletion{
		Output:       "PASS",
		FileCoverage: &ReviewFileCoverage{Reviewed: &reviewed},
	}))
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Nil(t, review.FileCoverage.Excluded)
	partialJSON, err := json.Marshal(review)
	require.NoError(t, err)
	assert.Contains(t, string(partialJSON), `"reviewed":4`)
	assert.NotContains(t, string(partialJSON), `"excluded"`)

	machineID, err := db.GetMachineID()
	require.NoError(t, err)
	future := time.Now().Add(time.Hour)
	_, err = db.Exec(`UPDATE review_jobs SET synced_at = ?, updated_at = ? WHERE id = ?`,
		future.Format(time.RFC3339), future.Format(time.RFC3339), job.ID)
	require.NoError(t, err)
	pushed, err := db.GetReviewsToSync(machineID, 10)
	require.NoError(t, err)
	require.Len(t, pushed, 1)
	assert.NotNil(t, pushed[0].ReviewedFileCount)
	assert.Equal(t, 4, *pushed[0].ReviewedFileCount)
	assert.Nil(t, pushed[0].ExcludedFileCount)

	require.NotNil(t, job.UUID)
	require.NotNil(t, review.UUID)
	require.NoError(t, db.UpsertPulledReview(PulledReview{
		UUID:               *review.UUID,
		JobUUID:            *job.UUID,
		Agent:              "remote",
		Prompt:             "remote prompt",
		Output:             "PASS",
		CreatedAt:          future,
		UpdatedAt:          future,
		UpdatedByMachineID: uuid.New(),
	}))
	stored, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.FileCoverage)
	assert.Equal(t, 4, *stored.FileCoverage.Reviewed)
	assert.Nil(t, stored.FileCoverage.Excluded)

	newExcluded := 9
	future = future.Add(time.Hour)
	require.NoError(t, db.UpsertPulledReview(PulledReview{
		UUID:               *review.UUID,
		JobUUID:            *job.UUID,
		Agent:              "remote",
		Prompt:             "remote prompt",
		Output:             "PASS",
		CreatedAt:          future,
		UpdatedAt:          future,
		UpdatedByMachineID: uuid.New(),
		ReviewedFileCount:  &reviewed,
		ExcludedFileCount:  &newExcluded,
	}))
	stored, err = db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.FileCoverage)
	assert.Equal(t, 4, *stored.FileCoverage.Reviewed)
	assert.Equal(t, 9, *stored.FileCoverage.Excluded)
}

func TestIntegrationCoveragePostgres(t *testing.T) {
	pool := openTestPgPool(t)
	jobUUID := uuid.New()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	t.Cleanup(func() {
		ctx := t.Context()
		_, _ = pool.Pool().Exec(ctx, `DELETE FROM reviews WHERE job_uuid = $1`, jobUUID)
		_, _ = pool.Pool().Exec(ctx, `DELETE FROM review_jobs WHERE uuid = $1`, jobUUID)
		_, _ = pool.Pool().Exec(ctx, `DELETE FROM commits WHERE repo_id = $1`, repoID)
		_, _ = pool.Pool().Exec(ctx, `DELETE FROM repos WHERE id = $1`, repoID)
	})
	ctx := t.Context()
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobUUID, RepoID: repoID, CommitID: commitID})
	reviewUUID := uuid.New()
	zero := 0
	excluded := 3
	updatedBy := uuid.New()
	require.NoError(t, pool.UpsertReview(ctx, SyncableReview{
		UUID:               reviewUUID,
		JobUUID:            jobUUID,
		Agent:              "test",
		Prompt:             "prompt",
		Output:             "PASS",
		ReviewedFileCount:  &zero,
		ExcludedFileCount:  &excluded,
		UpdatedByMachineID: updatedBy,
		CreatedAt:          time.Now(),
	}))
	reviews, _, err := pool.PullReviews(ctx, uuid.New(), []uuid.UUID{jobUUID}, "", 10)
	require.NoError(t, err)
	require.Len(t, reviews, 1)
	assert.NotNil(t, reviews[0].ReviewedFileCount)
	assert.Equal(t, 0, *reviews[0].ReviewedFileCount)
	assert.Equal(t, 3, *reviews[0].ExcludedFileCount)

	require.NoError(t, pool.UpsertReview(ctx, SyncableReview{
		UUID:               reviewUUID,
		JobUUID:            jobUUID,
		Agent:              "test",
		Prompt:             "prompt",
		Output:             "PASS",
		UpdatedByMachineID: updatedBy,
		CreatedAt:          time.Now().Add(time.Minute),
	}))
	var gotReviewed, gotExcluded *int
	require.NoError(t, pool.Pool().QueryRow(ctx,
		`SELECT reviewed_file_count, excluded_file_count FROM reviews WHERE uuid = $1`, reviewUUID,
	).Scan(&gotReviewed, &gotExcluded))
	assert.Equal(t, 0, *gotReviewed)
	assert.Equal(t, 3, *gotExcluded)
}
