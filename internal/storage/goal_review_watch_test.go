package storage

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLatestCompletedGoalReviewScopesCheckoutAndExcludesCandidates(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	linked := filepath.Join(t.TempDir(), "linked")
	var expectedMain, expectedLinked int64
	for _, tc := range []struct {
		ref, worktree, source string
		complete              bool
	}{
		{ref: "older", complete: true},
		{ref: "latest", complete: true},
		{ref: "linked", worktree: linked, complete: true},
		{ref: "candidate", source: "goal_gate", complete: true},
		{ref: "pending"},
	} {
		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repo.ID, Agent: "test", GitRef: tc.ref, JobType: JobTypeGoalReview,
			Prompt: "Frozen " + tc.ref, PromptPrebuilt: true, WorktreePath: tc.worktree, Source: tc.source,
		})
		require.NoError(t, err)
		if tc.complete {
			claimed, err := db.ClaimJob("worker")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, job.ID, claimed.ID)
			require.NoError(t, db.CompleteJob(job.ID, "test", job.Prompt, "No findings."))
		}
		if tc.ref == "latest" {
			expectedMain = job.ID
		}
		if tc.ref == "linked" {
			expectedLinked = job.ID
		}
	}
	mainJob, err := db.LatestCompletedGoalReview(filepath.FromSlash(repo.RootPath))
	require.NoError(t, err)
	require.NotNil(t, mainJob)
	assert.Equal(t, expectedMain, mainJob.ID)
	assert.Equal(t, "Frozen latest", mainJob.Prompt)
	linkedJob, err := db.LatestCompletedGoalReview(linked)
	require.NoError(t, err)
	require.NotNil(t, linkedJob)
	assert.Equal(t, expectedLinked, linkedJob.ID)
	missing, err := db.LatestCompletedGoalReview(filepath.Join(t.TempDir(), "other"))
	require.NoError(t, err)
	assert.Nil(t, missing)
}
