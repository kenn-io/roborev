package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewInvocationsRemainUnfinishedAfterRestartAndCascadeWithJob(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reviews.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db := &DB{DB: raw}
	require.NoError(t, registerContentFunctions())
	// Upgrade a real prior schema containing a queued job.
	require.NoError(t, db.runSchemaMigrations(t.Context(), schemaMigrations[:len(schemaMigrations)-1]))
	t.Cleanup(func() { _ = db.Close() })
	repo := createRepo(t, db, "/tmp/invocation-repo")
	commit := createCommit(t, db, repo.ID, "abc123")
	job := enqueueJobBeforeSessionCLIMigration(t, db, repo.ID, commit.ID, "abc123")
	require.NoError(t, db.runSchemaMigrations(t.Context(), schemaMigrations))
	claimed := claimJob(t, db, "worker")
	report, err := db.BeginReviewInvocation(t.Context(), claimed, "test")
	require.NoError(t, err)
	// A process exit between start and completion leaves no trustworthy outcome.
	require.NoError(t, db.Close())
	db, err = Open(path)
	require.NoError(t, err)
	reports, err := db.ListReviewInvocations(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, report, reports[0])
	assert.Equal(t, "unfinished", reports[0].Status)
	assert.Nil(t, reports[0].Usage)
	// Late completion addresses this execution, and cannot replace an accepted result.
	report.Status = "succeeded"
	report.FinishedAt = new(time.Now().UTC())
	report.RawOutput = "saved output"
	require.NoError(t, db.FinishReviewInvocation(t.Context(), report))
	report.RawOutput = "replacement"
	require.Error(t, db.FinishReviewInvocation(t.Context(), report))
	reports, err = db.ListReviewInvocations(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "saved output", reports[0].RawOutput)
	require.NoError(t, db.DeleteRepo(job.RepoID, true))
	reports, err = db.ListReviewInvocations(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Empty(t, reports)
}
