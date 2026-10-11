package storage

import (
	"context"
	"database/sql"
	"testing"
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
