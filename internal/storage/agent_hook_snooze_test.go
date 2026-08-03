package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentHookSnoozeIsScopedAndExpires(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repoPath := filepath.Join(t.TempDir(), "repo")
	_, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)
	worktreePath := filepath.Join(t.TempDir(), "worktree")
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	until := now.Add(8 * time.Hour)

	set, err := db.SetAgentHookSnooze(
		repoPath, worktreePath, "feature/snooze", until,
	)
	require.NoError(t, err)
	assert.Equal("feature/snooze", set.Branch)
	assert.Equal(until, set.SnoozedUntil)

	active, err := db.ActiveAgentHookSnooze(
		repoPath, worktreePath, "feature/snooze", now,
	)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(until, active.SnoozedUntil)

	otherBranch, err := db.ActiveAgentHookSnooze(
		repoPath, worktreePath, "other", now,
	)
	require.NoError(t, err)
	assert.Nil(otherBranch, "a snooze must not follow the worktree to another branch")

	expired, err := db.ActiveAgentHookSnooze(
		repoPath, worktreePath, "feature/snooze", until,
	)
	require.NoError(t, err)
	assert.Nil(expired)

	require.NoError(t, db.ClearAgentHookSnooze(
		repoPath, worktreePath, "feature/snooze",
	))
	cleared, err := db.ActiveAgentHookSnooze(
		repoPath, worktreePath, "feature/snooze", now,
	)
	require.NoError(t, err)
	assert.Nil(cleared)
}

func TestOpenAddsAgentHookSnoozesToExistingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "existing.db")
	db, err := Open(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`DROP TABLE agent_hook_snoozes`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repoPath := filepath.Join(t.TempDir(), "repo")
	_, err = db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		repoPath, repoPath, "main", time.Now().Add(time.Hour),
	)
	require.NoError(t, err)
}
