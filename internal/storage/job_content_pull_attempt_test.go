package storage

import (
	"database/sql"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPulledContentKeepsNewerLocalAttempt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status JobStatus
		offset time.Duration
	}{
		{"done", JobStatusDone, 0},
		{"canceled", JobStatusCanceled, 0},
		{"clock behind imported attempt", JobStatusDone, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				status := tc.status
				assert := assert.New(t)
				db := openTestDB(t)
				defer db.Close()
				repo := createRepo(t, db, t.TempDir())
				machineID, err := db.GetMachineID()
				require.NoError(t, err)
				remote := PulledJob{
					UUID: testUUID("imported-rerun"), SourceMachineID: testUUID("remote-machine"),
					GitRef: "a..b", Agent: "test", JobType: JobTypeRange, Status: "done",
					Prompt: "remote attempt prompt", EnqueuedAt: time.Now().Add(tc.offset), UpdatedAt: time.Now(),
				}
				require.NoError(t, db.UpsertPulledJob(remote, repo.ID, nil))
				var id int64
				require.NoError(t, db.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, remote.UUID).Scan(&id))
				// Reruns must advance at PostgreSQL's microsecond precision,
				// even in the same clock tick or behind the imported clock.
				require.NoError(t, db.ReenqueueJob(id, ReenqueueOpts{}))
				job, err := db.ClaimJob("local-worker")
				require.NoError(t, err)
				require.NotNil(t, job)
				require.Equal(t, id, job.ID)
				require.Greater(t, job.EnqueuedAt.Truncate(time.Microsecond).UnixMicro(), remote.EnqueuedAt.UnixMicro())
				require.NoError(t, db.SaveJobPrompt(id, "local rerun prompt"))
				if status == JobStatusDone {
					require.NoError(t, db.CompleteJobResult(id, "test", ReviewCompletion{
						StructuredOutput: reviewFixtureJSON("Local rerun result."),
					}))
				} else {
					require.NoError(t, db.CancelJob(id))
					released, err := db.ReleaseCanceledJob(id, "local-worker")
					require.NoError(t, err)
					require.True(t, released)
				}
				// Startup clears completed worker IDs too, so worker ownership
				// cannot identify the attempt whose prompt must survive.
				require.NoError(t, db.ResetStaleJobs())
				before, err := db.GetJobByID(id)
				require.NoError(t, err)
				assert.Empty(before.WorkerID)

				changed, err := db.upsertPulledJob(remote, repo.ID, nil)
				require.NoError(t, err)
				assert.False(changed, "an old attempt replay must not replace a local rerun")
				after, err := db.GetJobByID(id)
				require.NoError(t, err)
				assert.Equal(status, after.Status)
				assert.Equal("local rerun prompt", after.Prompt)
				assert.Equal(before.EnqueuedAt, after.EnqueuedAt)

				// The local rerun must upload its current prompt before its
				// review and comments can follow the parent's sync marker.
				comment, err := db.AddCommentToJob(id, "reviewer", "Local follow-up.")
				require.NoError(t, err)
				jobs, err := db.GetJobsToSync(machineID, 10)
				require.NoError(t, err)
				require.Len(t, jobs, 1)
				assert.Equal("local rerun prompt", jobs[0].Prompt)
				require.NoError(t, db.MarkJobsSynced([]JobSyncMark{NewJobSyncMark(jobs[0])}))
				comments, err := db.GetCommentsToSync(machineID, 10)
				require.NoError(t, err)
				require.Len(t, comments, 1)
				assert.Equal(comment.ID, comments[0].ID)
				pendingReviews, err := db.GetReviewsToSync(machineID, 10)
				require.NoError(t, err)
				if status == JobStatusDone {
					require.Len(t, pendingReviews, 1)
					assert.Equal(id, pendingReviews[0].JobID)
				} else {
					assert.Empty(pendingReviews)
				}

				// Even a later edit to the remote review still belongs to the
				// older job attempt. It must not replace or resurrect a review.
				oldReview := PulledReview{
					UUID: testUUID("old-remote-review"), JobUUID: remote.UUID, Agent: "test",
					JobPrompt: remote.Prompt, JobEnqueuedAt: remote.EnqueuedAt, JobSourceMachineID: remote.SourceMachineID,
					StructuredOutput: reviewFixtureJSON("Remote result."),
					CreatedAt:        remote.EnqueuedAt, UpdatedAt: time.Now().Add(time.Hour),
					UpdatedByMachineID: remote.SourceMachineID,
				}
				changed, err = db.upsertPulledReview(oldReview)
				require.NoError(t, err)
				assert.False(changed)
				// Even if an old review claims the current attempt's identity,
				// it cannot replace or resurrect this executor's result.
				oldReview.JobEnqueuedAt = before.EnqueuedAt
				oldReview.JobSourceMachineID = *before.SourceMachineID
				changed, err = db.upsertPulledReview(oldReview)
				require.NoError(t, err)
				assert.False(changed)
				after, err = db.GetJobByID(id)
				require.NoError(t, err)
				assert.Equal("local rerun prompt", after.Prompt)
				var reviews int
				require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE job_id = ?`, id).Scan(&reviews))
				if status == JobStatusDone {
					assert.Equal(1, reviews)
					review, err := db.GetReviewByJobID(id)
					require.NoError(t, err)
					assert.Equal("Local rerun result.", review.StructuredOutput["summary"])
					// A peer can still close the review of this same attempt.
					// PostgreSQL drops its sub-microsecond timestamp digits.
					changed, err = db.upsertPulledReview(PulledReview{
						UUID: *review.UUID, JobUUID: remote.UUID, Agent: "test", Closed: true,
						JobPrompt: "local rerun prompt", JobEnqueuedAt: before.EnqueuedAt.Truncate(time.Microsecond), JobSourceMachineID: *before.SourceMachineID,
						StructuredOutput: reviewFixtureJSON("Local rerun result."),
						CreatedAt:        review.CreatedAt, UpdatedAt: time.Now().Add(time.Hour),
						UpdatedByMachineID: remote.SourceMachineID,
					})
					require.NoError(t, err)
					assert.True(changed)
					review, err = db.GetReviewByJobID(id)
					require.NoError(t, err)
					assert.True(review.Closed)
				} else {
					assert.Zero(reviews)
				}
			})
		})
	}
}

func TestPulledContentAcceptsNewerRemoteAttempt(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, t.TempDir())
	_, err := db.GetMachineID()
	require.NoError(t, err)
	first := time.Date(2026, 9, 1, 12, 0, 0, 1000, time.UTC)
	remote := PulledJob{
		UUID: testUUID("remote-rerun"), SourceMachineID: testUUID("remote-machine"),
		GitRef: "a..b", Agent: "test", JobType: JobTypeRange, Status: "failed",
		Prompt: "first prompt", EnqueuedAt: first, UpdatedAt: first,
	}
	require.NoError(t, db.UpsertPulledJob(remote, repo.ID, nil))
	remote.EnqueuedAt, remote.UpdatedAt = first.Add(time.Microsecond), first.Add(time.Second)
	remote.Status, remote.Prompt = "done", "second prompt"
	require.NoError(t, db.UpsertPulledJob(remote, repo.ID, nil))
	var id int64
	require.NoError(t, db.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, remote.UUID).Scan(&id))
	job, err := db.GetJobByID(id)
	require.NoError(t, err)
	assert.Equal("second prompt", job.Prompt)
	assert.Equal(remote.EnqueuedAt, job.EnqueuedAt)

	review := PulledReview{
		UUID: testUUID("remote-rerun-review"), JobUUID: remote.UUID, Agent: "test",
		JobPrompt: "complete second prompt", JobEnqueuedAt: remote.EnqueuedAt, JobSourceMachineID: remote.SourceMachineID,
		StructuredOutput: reviewFixtureJSON("No issues found."),
		CreatedAt:        remote.EnqueuedAt, UpdatedAt: remote.UpdatedAt.Add(time.Second),
		UpdatedByMachineID: remote.SourceMachineID,
	}
	require.NoError(t, db.UpsertPulledReview(review))
	job, err = db.GetJobByID(id)
	require.NoError(t, err)
	assert.Equal("complete second prompt", job.Prompt)

	review.JobEnqueuedAt, review.JobPrompt = first, "stale prompt"
	review.UpdatedAt = review.UpdatedAt.Add(time.Hour)
	changed, err := db.upsertPulledReview(review)
	require.NoError(t, err)
	assert.False(changed, "a later review edit cannot bring back an older attempt's prompt")
	job, err = db.GetJobByID(id)
	require.NoError(t, err)
	assert.Equal("complete second prompt", job.Prompt)
}

func TestReviewAttemptRepairWithinSameSecond(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		env := setupJobEnv(t, t.TempDir(), "same-second-upgrade")
		claimJob(t, env.db, "worker")
		require.NoError(t, completeReviewFixture(env.db, env.job.ID, "test", "prompt", "Current review."))
		machine, err := env.db.GetMachineID()
		require.NoError(t, err)
		// Old releases acknowledged reviews at whole-second precision. The
		// migration's parent upload can finish in that very same second.
		legacyMark := time.Now().UTC().Format(time.RFC3339)
		_, err = env.db.Exec(`UPDATE reviews SET updated_at = ?, synced_at = ? WHERE job_id = ?`, legacyMark, legacyMark, env.job.ID)
		require.NoError(t, err)
		require.NoError(t, env.db.MarkJobSynced(env.job.ID))
		reviews, err := env.db.GetReviewsToSync(machine, 10)
		require.NoError(t, err)
		require.Len(t, reviews, 1)
		assert.True(t, reviews[0].MetadataOnly)
		require.NoError(t, env.db.MarkReviewsSynced(reviews[:1]))
		reviews, err = env.db.GetReviewsToSync(machine, 10)
		require.NoError(t, err)
		assert.Empty(t, reviews)
	})
}

func TestNewerAttemptReplacesLaterAppliedState(t *testing.T) {
	t.Parallel()
	for _, status := range []JobStatus{JobStatusApplied, JobStatusRebased} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			env := setupJobEnv(t, t.TempDir(), "applied-before-pull")
			oldTime := env.job.EnqueuedAt
			_, err := env.db.Exec(`UPDATE review_jobs SET job_type = 'fix', status = ?, updated_at = ? WHERE id = ?`, status, preciseTimestampAt(oldTime.Add(2*time.Hour)), env.job.ID)
			require.NoError(t, err)
			incoming := PulledJob{
				UUID: *env.job.UUID, SourceMachineID: testUUID("new-executor"), GitRef: env.job.GitRef,
				Agent: "test", JobType: JobTypeFix, Status: "done", Prompt: "new fix prompt",
				EnqueuedAt: oldTime.Add(time.Hour), UpdatedAt: oldTime.Add(time.Hour),
			}
			require.NoError(t, env.db.UpsertPulledJob(incoming, env.repo.ID, nil))
			require.NoError(t, env.db.UpsertPulledReview(PulledReview{
				UUID: testUUID("new-fix-review"), JobUUID: incoming.UUID, Agent: "test", Output: "New fix result.",
				JobEnqueuedAt: incoming.EnqueuedAt, JobSourceMachineID: incoming.SourceMachineID,
				CreatedAt: incoming.UpdatedAt, UpdatedAt: incoming.UpdatedAt, UpdatedByMachineID: incoming.SourceMachineID,
			}))
			current, err := env.db.GetJobByID(env.job.ID)
			require.NoError(t, err)
			assert.Equal(t, JobStatusDone, current.Status)
			assert.Equal(t, "new fix prompt", current.Prompt)
			review, err := env.db.GetReviewByJobID(env.job.ID)
			require.NoError(t, err)
			assert.Equal(t, "New fix result.", review.Output)
		})
	}
}

func TestReviewAcknowledgmentKeepsReplacementAndEditsPending(t *testing.T) {
	t.Parallel()
	for _, rerun := range []bool{true, false} {
		t.Run(map[bool]string{true: "rerun reuses numeric ID", false: "closure in same clock tick"}[rerun], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				h := newSyncTestHelper(t)
				job := h.createCompletedJob("review-acknowledgment")
				require.NoError(t, h.db.MarkJobSynced(job.ID))
				pushed, err := h.db.GetReviewsToSync(h.machineID, 10)
				require.NoError(t, err)
				require.Len(t, pushed, 1)
				if rerun {
					require.NoError(t, h.db.ReenqueueJob(job.ID, ReenqueueOpts{}))
					claimJob(t, h.db, "rerun-worker")
					require.NoError(t, completeReviewFixture(h.db, job.ID, "test", "rerun prompt", "New result."))
					require.NoError(t, h.db.MarkJobSynced(job.ID))
					current, err := h.db.GetReviewByJobID(job.ID)
					require.NoError(t, err)
					require.Equal(t, pushed[0].ID, current.ID)
					require.NotEqual(t, pushed[0].UUID, *current.UUID)
				} else {
					require.NoError(t, h.db.MarkReviewClosed(pushed[0].ID, true))
				}
				require.NoError(t, h.db.MarkReviewsSynced(pushed))
				var mark sql.NullString
				require.NoError(t, h.db.QueryRow(`SELECT synced_at FROM reviews WHERE job_id = ?`, job.ID).Scan(&mark))
				assert.False(t, mark.Valid, "the old upload must not acknowledge the new state")
				pending, err := h.db.GetReviewsToSync(h.machineID, 10)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.NoError(t, h.db.MarkReviewsSynced(pending))
				pending, err = h.db.GetReviewsToSync(h.machineID, 10)
				require.NoError(t, err)
				assert.Empty(t, pending)
			})
		})
	}
}

func TestReviewClosureAfterAcknowledgmentIsPending(t *testing.T) {
	t.Parallel()
	for _, byJob := range []bool{false, true} {
		t.Run(map[bool]string{false: "review ID", true: "job ID"}[byJob], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				h := newSyncTestHelper(t)
				job := h.createCompletedJob("close-after-upload")
				require.NoError(t, h.db.MarkJobSynced(job.ID))
				pushed, err := h.db.GetReviewsToSync(h.machineID, 10)
				require.NoError(t, err)
				require.Len(t, pushed, 1)
				require.NoError(t, h.db.MarkReviewsSynced(pushed))
				if byJob {
					require.NoError(t, h.db.MarkReviewClosedByJobID(job.ID, true))
				} else {
					require.NoError(t, h.db.MarkReviewClosed(pushed[0].ID, true))
				}
				pending, err := h.db.GetReviewsToSync(h.machineID, 10)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				assert.True(t, pending[0].Closed)
				assert.False(t, pending[0].MetadataOnly)
			})
		})
	}
}

func TestPulledReviewKeepsSubsecondEditOrder(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("subsecond-edit-order")
	review, err := h.db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	first := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	incoming := PulledReview{
		UUID: *review.UUID, JobUUID: *job.UUID, Agent: "test",
		JobEnqueuedAt: storedJobEnqueuedAt(t, h.db, job.ID), JobSourceMachineID: *job.SourceMachineID,
		StructuredOutput: reviewFixtureJSON("Review result."),
		CreatedAt:        review.CreatedAt, UpdatedByMachineID: testUUID("peer-editor"),
	}
	for _, edit := range []struct {
		offset  time.Duration
		closed  bool
		changed bool
	}{{time.Microsecond, false, true}, {2 * time.Microsecond, true, true}, {time.Microsecond, false, false}} {
		incoming.UpdatedAt, incoming.Closed = first.Add(edit.offset), edit.closed
		changed, err := h.db.upsertPulledReview(incoming)
		require.NoError(t, err)
		assert.Equal(t, edit.changed, changed)
	}
	current, err := h.db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.True(t, current.Closed)
}
