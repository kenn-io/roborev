package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimJobSerializesSharedSession(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, t.TempDir())
	enqueue := func(session string) *ReviewJob {
		j, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, Agent: "test", SessionID: session, Source: JobSourceRebuttal, JobType: JobTypeTask, Prompt: "task"})
		require.NoError(t, err)
		return j
	}
	first, second, independent := enqueue("shared"), enqueue("shared"), enqueue("other")
	claimed, err := db.ClaimJob("one")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, first.ID, claimed.ID)
	claimed, err = db.ClaimJob("two")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, independent.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(first.ID, "test", "done"))
	claimed, err = db.ClaimJob("three")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, second.ID, claimed.ID)
}

func TestClaimJobWaitsForCanceledSessionWriter(t *testing.T) {
	t.Parallel()
	for _, sources := range []struct{ writer, next string }{
		{JobSourceRebuttal, JobSourceRebuttal},
		{"", JobSourceRebuttal},
		{JobSourceRebuttal, ""},
	} {
		t.Run(sources.writer+"/"+sources.next, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			db := openTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			repo := createRepo(t, db, t.TempDir())
			enqueue := func(session, source string) *ReviewJob {
				job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, Agent: "test", SessionID: session, Source: source, JobType: JobTypeTask, Prompt: "task"})
				require.NoError(t, err)
				return job
			}
			writer := enqueue("shared", sources.writer)
			claimed, err := db.ClaimJob("one")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, writer.ID, claimed.ID)
			next := enqueue("shared", sources.next)
			independent := enqueue("other", "")
			claimed, err = db.ClaimJob("two")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			assert.Equal(independent.ID, claimed.ID)
			claimed, err = db.ClaimJob("three")
			require.NoError(t, err)
			assert.Nil(claimed, "the running worker owns the shared session")
			require.NoError(t, db.CancelJob(writer.ID))
			claimed, err = db.ClaimJob("three")
			require.NoError(t, err)
			assert.Nil(claimed, "the canceled worker still owns the shared session")
			released, err := db.ReleaseCanceledJob(writer.ID, "one")
			require.NoError(t, err)
			require.True(t, released)
			claimed, err = db.ClaimJob("three")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			assert.Equal(next.ID, claimed.ID)
			assert.Equal("shared", claimed.SessionID)
		})
	}
}
