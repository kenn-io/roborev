package storage

import (
	"context"
	"database/sql"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobAttemptRejectsPromptBuiltBeforeReplacement(t *testing.T) {
	t.Parallel()
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "before registration", true: "during prompt build"}[registered], func(t *testing.T) {
			t.Parallel()
			env := setupJobEnv(t, t.TempDir(), "superseded-prompt")
			job := claimJob(t, env.db, "old-worker")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if registered {
				release, err := env.db.StartJobAttempt(ctx, job, "old-worker", cancel)
				require.NoError(t, err)
				defer release()
			}
			remote := PulledJob{
				UUID: *job.UUID, SourceMachineID: testUUID("new-owner"),
				GitRef: job.GitRef, Agent: "test", JobType: job.JobType, Status: "done",
				Prompt: "new attempt prompt", EnqueuedAt: job.EnqueuedAt.Add(time.Hour), UpdatedAt: time.Now(),
			}
			require.NoError(t, env.db.UpsertPulledJob(remote, job.RepoID, job.CommitID))
			if registered {
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			} else {
				_, err := env.db.StartJobAttempt(ctx, job, "old-worker", cancel)
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
			saved, err := env.db.SaveJobPromptForAttempt(job, "old-worker", "old built prompt")
			require.NoError(t, err)
			assert.False(t, saved)
			called := false
			ran, err := env.db.RunJobAttempt(job, "old-worker", func() { called = true })
			require.NoError(t, err)
			assert.False(t, ran)
			assert.False(t, called, "obsolete attempt ran completion effects")
			current, err := env.db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, "new attempt prompt", current.Prompt)
		})
	}
}

func TestJobAttemptPromptRequiresClaimIdentity(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "claim-identity")
	job := claimJob(t, env.db, "worker")
	saved, err := env.db.SaveJobPromptForAttempt(job, "other-worker", "wrong owner")
	require.NoError(t, err)
	assert.False(t, saved)
	stale := *job
	stale.EnqueuedAt = stale.EnqueuedAt.Add(-time.Microsecond)
	saved, err = env.db.SaveJobPromptForAttempt(&stale, "worker", "old attempt")
	require.NoError(t, err)
	assert.False(t, saved)
	saved, err = env.db.SaveJobPromptForAttempt(job, "worker", "current prompt")
	require.NoError(t, err)
	assert.True(t, saved)
	current, err := env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "current prompt", current.Prompt)
}

func TestPulledCanceledAttemptReleasesUnregisteredClaim(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "unregistered-claim")
	job := claimJob(t, env.db, "old-worker")
	require.NoError(t, env.db.UpsertPulledJob(PulledJob{
		UUID: *job.UUID, SourceMachineID: testUUID("new-owner"), GitRef: job.GitRef,
		Agent: "test", JobType: job.JobType, Status: "canceled",
		EnqueuedAt: job.EnqueuedAt.Add(time.Hour), UpdatedAt: time.Now(),
	}, job.RepoID, job.CommitID))
	current, err := env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, current.WorkerID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = env.db.StartJobAttempt(ctx, job, "old-worker", cancel)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, env.db.ReenqueueJob(job.ID, ReenqueueOpts{}))
}

func TestJobAttemptObservesCancelWhileWaitingForCleanup(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "waiting-claim")
	job := claimJob(t, env.db, "old-worker")
	oldCtx, oldCancel := context.WithCancel(t.Context())
	defer oldCancel()
	releaseOld, err := env.db.StartJobAttempt(oldCtx, job, "old-worker", oldCancel)
	require.NoError(t, err)
	changed, err := env.db.FailoverJob(job.ID, "old-worker", "test", "")
	require.NoError(t, err)
	require.True(t, changed)
	next := claimJob(t, env.db, "new-worker")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type registration struct {
		release func()
		err     error
	}
	registered := make(chan registration, 1)
	go func() {
		release, err := env.db.StartJobAttempt(ctx, next, "new-worker", cancel)
		registered <- registration{release, err}
	}()
	require.NoError(t, env.db.CancelJob(job.ID))
	// The pool still has the previous worker's handler during cleanup.
	oldCancel()
	releaseOld()
	result := <-registered
	require.NoError(t, result.err)
	result.release()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// A slow completion transition must not keep a newly claimed job from timing
// out: the job registers for cancellation and timeout only after
// StartJobAttempt returns.
func TestStartJobAttemptStopsWaitingForTransitionWhenContextEnds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()
		repo := createRepo(t, db, t.TempDir())
		for _, sha := range []string{"slow-sha", "next-sha"} {
			enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, sha).ID, sha)
		}
		slow := claimJob(t, db, "worker-a")
		next := claimJob(t, db, "worker-b")

		entered, finish, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(finished)
			ran, err := db.RunJobAttempt(slow, "worker-a", func() {
				close(entered)
				<-finish
			})
			assert.NoError(t, err)
			assert.True(t, ran)
		}()
		<-entered

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		type registration struct {
			release func()
			err     error
		}
		registered := make(chan registration, 1)
		go func() {
			release, err := db.StartJobAttempt(ctx, next, "worker-b", cancel)
			registered <- registration{release, err}
		}()
		synctest.Wait()

		// A transition queued behind the waiting attempt runs once that
		// attempt gives up, while the slow transition still holds the lock.
		queuedRan := make(chan bool, 1)
		go func() {
			ran, err := db.RunJobAttempt(next, "worker-b", func() {})
			assert.NoError(t, err)
			queuedRan <- ran
		}()
		synctest.Wait()
		assert.Empty(t, queuedRan, "a waiting attempt must hold back new transitions")

		result := <-registered
		require.ErrorIs(t, result.err, context.DeadlineExceeded)
		assert.Nil(t, result.release)
		assert.True(t, <-queuedRan)

		close(finish)
		<-finished
		ctx, cancel = context.WithCancel(t.Context())
		defer cancel()
		release, err := db.StartJobAttempt(ctx, next, "worker-b", cancel)
		require.NoError(t, err)
		release()
	})
}
