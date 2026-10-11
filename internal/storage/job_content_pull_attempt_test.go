package storage

import (
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
					JobPrompt: remote.Prompt, JobEnqueuedAt: remote.EnqueuedAt,
					StructuredOutput: reviewFixtureJSON("Remote result."),
					CreatedAt:        remote.EnqueuedAt, UpdatedAt: time.Now().Add(time.Hour),
					UpdatedByMachineID: remote.SourceMachineID,
				}
				changed, err = db.upsertPulledReview(oldReview)
				require.NoError(t, err)
				assert.False(changed)
				// The job can reach PostgreSQL before its new review. Even if
				// that parent supplies the current enqueue time, the older review
				// cannot replace or resurrect this executor's result.
				oldReview.JobEnqueuedAt = before.EnqueuedAt
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
						JobPrompt: "local rerun prompt", JobEnqueuedAt: before.EnqueuedAt.Truncate(time.Microsecond),
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
		JobPrompt: "complete second prompt", JobEnqueuedAt: remote.EnqueuedAt,
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
		require.NoError(t, env.db.MarkReviewsSynced([]int64{reviews[0].ID}))
		reviews, err = env.db.GetReviewsToSync(machine, 10)
		require.NoError(t, err)
		assert.Empty(t, reviews)
	})
}
