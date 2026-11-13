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

	claimed, err := db.ClaimCIBranchReviewStatus(*review, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	again, err := db.ClaimCIBranchReviewStatus(*review, time.Minute)
	require.NoError(t, err)
	assert.False(again, "a claimed status is not sent by a second poller")

	require.NoError(t, db.FinishCIBranchReviewStatus(*review, false))
	retry, err := db.ClaimCIBranchReviewStatus(*review, time.Minute)
	require.NoError(t, err)
	assert.True(retry, "a failed send is retried")
	require.NoError(t, db.FinishCIBranchReviewStatus(*review, true))
	done, err := db.ClaimCIBranchReviewStatus(*review, time.Minute)
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
	publishBranchResult(t, db, "error", "Review failed")
	replacement := rerunBranchReview(t, db, repo.ID, original.ID)

	review := mustBranchReview(t, db)
	require.NotNil(t, review.SynthesisJobID)
	assert.Equal(replacement.ID, *review.SynthesisJobID, "the rerun's result owns the commit status")
	assert.Equal(replacement.PanelRunUUID, review.PanelRunUUID)
	assert.Empty(review.FinalState, "the earlier failure no longer stands as the result")
	require.NoError(t, db.SetCIBranchReviewFinalStatus(*review, "success", "No issues found"))
	assert.True(mustClaim(t, db, *review), "the replacement result is published")
}

func TestReplacedCIBranchReviewRunCannotPublish(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-branch-review-replaced")
	_, original, err := db.CreateCIBranchReviewRun("o/r", "main", "landed",
		[]EnqueueOpts{{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"}},
		EnqueueOpts{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"})
	require.NoError(t, err)
	publishBranchResult(t, db, "success", "No issues found")
	stale := mustBranchReview(t, db)
	rerunBranchReview(t, db, repo.ID, original.ID)

	// The first run's late result and publisher both act on the old run.
	require.NoError(t, db.SetCIBranchReviewFinalStatus(*stale, "error", "Review failed"))
	assert.Empty(mustBranchReview(t, db).FinalState, "a replaced run cannot record the rerun's result")
	claimed, err := db.ClaimCIBranchReviewStatus(*stale, time.Minute)
	require.NoError(t, err)
	assert.False(claimed, "a replaced run's result is not published")

	current := mustBranchReview(t, db)
	require.NoError(t, db.SetCIBranchReviewFinalStatus(*current, "success", "No issues found"))
	require.ErrorIs(t, db.FinishCIBranchReviewStatus(*stale, true), ErrCIBranchReviewReplaced)
	assert.True(mustClaim(t, db, *current), "the rerun's result is still unpublished")
}

func TestRerunEarlierCIBranchReviewRun(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-branch-review-earlier-run")
	_, original, err := db.CreateCIBranchReviewRun("o/r", "main", "landed",
		[]EnqueueOpts{{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"}},
		EnqueueOpts{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"})
	require.NoError(t, err)
	publishBranchResult(t, db, "error", "Review failed")
	rerunBranchReview(t, db, repo.ID, original.ID)

	members := []EnqueueOpts{{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test", Source: JobSourceCIBranch}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test", Source: JobSourceCIBranch}
	_, _, _, err = db.EnqueuePanelRerun(members, synthesis, uuid.New(), original.ID)
	require.ErrorIs(t, err, ErrCIPanelActive, "an earlier run cannot take over while the current run is unpublished")

	publishBranchResult(t, db, "success", "No issues found")
	again := rerunBranchReview(t, db, repo.ID, original.ID)
	review := mustBranchReview(t, db)
	require.NotNil(t, review.SynthesisJobID)
	assert.Equal(again.ID, *review.SynthesisJobID, "a published review can be rerun from any of its runs")
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
	require.NoError(t, db.SetCIBranchReviewFinalStatus(*finishedReview, "error", "Review canceled"))

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

func mustClaim(t *testing.T, db *DB, review CIBranchReview) bool {
	t.Helper()
	claimed, err := db.ClaimCIBranchReviewStatus(review, time.Minute)
	require.NoError(t, err)
	return claimed
}

// publishBranchResult records and publishes the current run's result.
func publishBranchResult(t *testing.T, db *DB, state, description string) {
	t.Helper()
	require.NoError(t, db.SetCIBranchReviewFinalStatus(*mustBranchReview(t, db), state, description))
	review := mustBranchReview(t, db)
	require.True(t, mustClaim(t, db, *review))
	require.NoError(t, db.FinishCIBranchReviewStatus(*review, true))
}

func rerunBranchReview(t *testing.T, db *DB, repoID, sourceJobID int64) *ReviewJob {
	t.Helper()
	members := []EnqueueOpts{{RepoID: repoID, GitRef: "parent..landed", Agent: "test", Source: JobSourceCIBranch}}
	synthesis := EnqueueOpts{RepoID: repoID, GitRef: "parent..landed", Agent: "test", Source: JobSourceCIBranch}
	_, replacement, _, err := db.EnqueuePanelRerun(members, synthesis, uuid.New(), sourceJobID)
	require.NoError(t, err)
	return replacement
}
