package storage

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimJobReturnsNilWhenQueuePaused(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, t.TempDir())
	commit := createCommit(t, db, repo.ID, "abc123")
	_, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA, Agent: "test"})
	require.NoError(t, err)
	require.NoError(t, db.SetQueuePaused(true))

	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	assert.Nil(t, claimed)
}

func TestQueuePauseStateDefaultsAndPersists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(dbPath)
	require.NoError(t, err)

	paused, err := db.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)

	require.NoError(t, db.SetQueuePaused(true))
	paused, err = db.IsQueuePaused()
	require.NoError(t, err)
	assert.True(t, paused)
	require.NoError(t, db.Close())

	reopened, err := Open(dbPath)
	require.NoError(t, err)
	defer reopened.Close()

	paused, err = reopened.IsQueuePaused()
	require.NoError(t, err)
	assert.True(t, paused)

	require.NoError(t, reopened.SetQueuePaused(false))
	paused, err = reopened.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)
}
