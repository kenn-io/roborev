package storage

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateCIBranchReviewRunReviewsACommitOnce(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-branch-review-once")
	members := []EnqueueOpts{{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"}

	created, synth, err := db.CreateCIBranchReviewRun("o/r", "main", "landed", members, synthesis)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(JobSourceCIBranch, synth.Source)

	again, _, err := db.CreateCIBranchReviewRun("o/r", "main", "landed", members, synthesis)
	require.NoError(t, err)
	assert.False(again, "a second poll of the same commit must not queue another review")

	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Len(jobs, 2, "one member and one synthesis job")
	for _, job := range jobs {
		assert.Equal(JobSourceCIBranch, job.Source)
	}

	cursor, err := db.GetCIBranchCursor("o/r")
	require.NoError(t, err)
	require.NotNil(t, cursor)
	assert.Equal("landed", cursor.HeadSHA)

	review, err := db.GetCIBranchReviewBySynthesisJobID(synth.ID)
	require.NoError(t, err)
	require.NotNil(t, review)
	assert.Equal("landed", review.SHA)
}

func TestCIBranchReviewStatusIsSentOnce(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	require.NoError(t, db.RecordCIBranchCommitOutcome("o/r", "main", "landed", "error", "could not queue"))
	review, err := db.GetCIBranchReview("o/r", "landed")
	require.NoError(t, err)
	require.NotNil(t, review)

	claimed, err := db.ClaimCIBranchReviewStatus(review.ID, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	again, err := db.ClaimCIBranchReviewStatus(review.ID, time.Minute)
	require.NoError(t, err)
	assert.False(again, "a claimed status is not sent by a second poller")

	require.NoError(t, db.FinishCIBranchReviewStatus(review.ID, false))
	retry, err := db.ClaimCIBranchReviewStatus(review.ID, time.Minute)
	require.NoError(t, err)
	assert.True(retry, "a failed send is retried")
	require.NoError(t, db.FinishCIBranchReviewStatus(review.ID, true))
	done, err := db.ClaimCIBranchReviewStatus(review.ID, time.Minute)
	require.NoError(t, err)
	assert.False(done, "a sent status is not sent again")
}

func TestRerunCIBranchReviewPublishesReplacementResult(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-branch-review-rerun")
	members := []EnqueueOpts{{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"}
	_, original, err := db.CreateCIBranchReviewRun("o/r", "main", "landed", members, synthesis)
	require.NoError(t, err)
	require.NoError(t, db.SetCIBranchReviewFinalStatus(mustBranchReview(t, db).ID, "error", "Review failed"))
	review := mustBranchReview(t, db)
	require.True(t, mustClaim(t, db, review.ID))
	require.NoError(t, db.FinishCIBranchReviewStatus(review.ID, true))

	rerunMembers := []EnqueueOpts{{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test", Source: JobSourceCIBranch}}
	rerunSynthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test", Source: JobSourceCIBranch}
	_, replacement, _, err := db.EnqueuePanelRerun(rerunMembers, rerunSynthesis, uuid.New(), original.ID)
	require.NoError(t, err)

	review = mustBranchReview(t, db)
	require.NotNil(t, review.SynthesisJobID)
	assert.Equal(replacement.ID, *review.SynthesisJobID, "the rerun's result owns the commit status")
	assert.Equal(replacement.PanelRunUUID, review.PanelRunUUID)
	assert.Empty(review.FinalState, "the earlier failure no longer stands as the result")
	require.NoError(t, db.SetCIBranchReviewFinalStatus(review.ID, "success", "No issues found"))
	assert.True(mustClaim(t, db, review.ID), "the replacement result is published")
}

func TestGetTimedOutCIBranchReviewRunsSelectsHungUnfinishedReviews(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-branch-review-timeout")
	seed := func(sha string) *ReviewJob {
		t.Helper()
		members := []EnqueueOpts{{RepoID: repo.ID, GitRef: "p.." + sha, Agent: "test"}}
		_, synth, err := db.CreateCIBranchReviewRun("o/r", "main", sha, members,
			EnqueueOpts{RepoID: repo.ID, GitRef: "p.." + sha, Agent: "test"})
		require.NoError(t, err)
		return synth
	}
	hung, recent, finished := seed("hung"), seed("recent"), seed("finished")
	for _, run := range []struct {
		synth *ReviewJob
		age   string
	}{{hung, "-2 hours"}, {recent, "-1 minutes"}, {finished, "-2 hours"}} {
		_, err := db.ExecContext(t.Context(), `UPDATE review_jobs SET status = 'running',
			started_at = datetime('now', ?) WHERE panel_run_uuid = ? AND panel_role = 'member'`,
			run.age, run.synth.PanelRunUUID)
		require.NoError(t, err)
	}
	finishedReview, err := db.GetCIBranchReviewBySynthesisJobID(finished.ID)
	require.NoError(t, err)
	require.NoError(t, db.SetCIBranchReviewFinalStatus(finishedReview.ID, "error", "Review canceled"))

	runs, err := db.GetTimedOutCIBranchReviewRuns("o/r", time.Hour)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{*hung.PanelRunUUID}, runs)
}

func mustBranchReview(t *testing.T, db *DB) *CIBranchReview {
	t.Helper()
	review, err := db.GetCIBranchReview("o/r", "landed")
	require.NoError(t, err)
	return review
}

func mustClaim(t *testing.T, db *DB, id int64) bool {
	t.Helper()
	claimed, err := db.ClaimCIBranchReviewStatus(id, time.Minute)
	require.NoError(t, err)
	return claimed
}
