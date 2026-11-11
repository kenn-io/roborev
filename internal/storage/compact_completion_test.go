package storage

import (
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompactCompletionRollsBackAndRecovers(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, "/tmp/compact-completion")
	result := ReviewCompletion{StructuredOutput: jsontext.Value(`{"schema_version":3,"summary":"Compact result","verdict":"pass","findings":[]}`)}
	var sources []CompactSourceIdentity
	for i := range 2 {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: fmt.Sprintf("source-%d", i), Agent: "test"})
		require.NoError(t, err)
		claimJob(t, db, "worker")
		require.NoError(t, db.CompleteJobResult(job.ID, "test", result))
		row, err := db.GetReviewByJobID(job.ID)
		require.NoError(t, err)
		require.NotNil(t, row.UUID)
		sources = append(sources, CompactSourceIdentity{JobID: job.ID, ReviewUUID: *row.UUID})
	}
	compact, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "compact", Agent: "test", JobType: JobTypeCompact})
	require.NoError(t, err)
	claimJob(t, db, "worker")
	_, err = db.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_second_closure BEFORE UPDATE OF closed ON reviews WHEN OLD.job_id = %d BEGIN SELECT RAISE(ABORT, 'synthetic closure failure'); END`, sources[1].JobID))
	require.NoError(t, err)
	_, err = db.CompleteCompactJobResult(t.Context(), compact.ID, "test", result, sources)
	require.ErrorContains(t, err, "synthetic closure failure")
	for _, source := range sources {
		row, err := db.GetReviewByJobID(source.JobID)
		require.NoError(t, err)
		assert.False(t, row.Closed)
	}
	job, err := db.GetJobByID(compact.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, job.Status)
	_, err = db.GetReviewByJobID(compact.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_second_closure`)
	require.NoError(t, err)
	closed, err := db.CompleteCompactJobResult(t.Context(), compact.ID, "test", result, sources)
	require.NoError(t, err)
	assert.Equal(t, []int64{sources[0].JobID, sources[1].JobID}, closed)
	for _, source := range sources {
		row, err := db.GetReviewByJobID(source.JobID)
		require.NoError(t, err)
		assert.True(t, row.Closed)
	}
	job, err = db.GetJobByID(compact.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusDone, job.Status)
	_, err = db.GetReviewByJobID(compact.ID)
	require.NoError(t, err)
}

func TestCompactCompletionGuardsSourcesAndCanceledJob(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, "/tmp/compact-completion-guards")
	result := ReviewCompletion{StructuredOutput: jsontext.Value(`{"schema_version":3,"summary":"Review","verdict":"pass","findings":[]}`)}
	source, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "source", Agent: "test"})
	require.NoError(t, err)
	claimJob(t, db, "worker")
	require.NoError(t, db.CompleteJobResult(source.ID, "test", result))
	row, err := db.GetReviewByJobID(source.ID)
	require.NoError(t, err)
	require.NotNil(t, row.UUID)
	identity := CompactSourceIdentity{JobID: source.ID, ReviewUUID: *row.UUID}
	compact, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "compact", Agent: "test", JobType: JobTypeCompact})
	require.NoError(t, err)
	claimJob(t, db, "worker")
	stale := identity
	stale.ReviewUUID = uuid.New()
	closed, err := db.CompleteCompactJobResult(t.Context(), compact.ID, "test", result, []CompactSourceIdentity{stale, {JobID: 999999, ReviewUUID: uuid.New()}})
	require.NoError(t, err)
	assert.Empty(t, closed)
	row, err = db.GetReviewByJobID(source.ID)
	require.NoError(t, err)
	assert.False(t, row.Closed)
	canceled, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "canceled", Agent: "test", JobType: JobTypeCompact})
	require.NoError(t, err)
	claimJob(t, db, "worker")
	require.NoError(t, db.CancelJob(canceled.ID))
	closed, err = db.CompleteCompactJobResult(t.Context(), canceled.ID, "test", result, []CompactSourceIdentity{identity})
	require.NoError(t, err)
	assert.Nil(t, closed)
	row, err = db.GetReviewByJobID(source.ID)
	require.NoError(t, err)
	assert.False(t, row.Closed)
	_, err = db.GetReviewByJobID(canceled.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)

	require.NoError(t, db.MarkReviewClosedByJobID(source.ID, true))
	alreadyClosed, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "already-closed", Agent: "test", JobType: JobTypeCompact})
	require.NoError(t, err)
	claimJob(t, db, "worker")
	closed, err = db.CompleteCompactJobResult(t.Context(), alreadyClosed.ID, "test", result, []CompactSourceIdentity{identity})
	require.NoError(t, err)
	assert.NotNil(t, closed)
	assert.Empty(t, closed)
}
