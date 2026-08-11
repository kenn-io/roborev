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

func TestListActiveAgentHookSnoozes(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	base := t.TempDir()
	now := time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)

	zetaPath := filepath.Join(base, "zeta")
	alphaPath := filepath.Join(base, "alpha")
	for _, repoPath := range []string{zetaPath, alphaPath} {
		_, err := db.GetOrCreateRepo(repoPath)
		require.NoError(t, err)
	}
	_, err := db.SetAgentHookSnooze(
		zetaPath, filepath.Join(base, "zeta-worktree"), "topic/z", now.Add(2*time.Hour),
	)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		alphaPath, alphaPath, "main", now.Add(time.Hour),
	)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		alphaPath, alphaPath, "release", now.Add(75*time.Minute),
	)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		alphaPath, filepath.Join(base, "alpha-worktree"), "topic/a", now.Add(90*time.Minute),
	)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		alphaPath, filepath.Join(base, "expired"), "old", now,
	)
	require.NoError(t, err)

	got, err := db.ListActiveAgentHookSnoozes(now)
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal("alpha", got[0].RepoName)
	assert.Equal(filepath.ToSlash(alphaPath), got[0].RepoPath)
	assert.Equal(filepath.ToSlash(alphaPath), got[0].WorktreePath)
	assert.Equal("main", got[0].Branch)
	assert.Equal(filepath.ToSlash(alphaPath), got[1].WorktreePath)
	assert.Equal("release", got[1].Branch)
	assert.Equal("alpha", got[2].RepoName)
	assert.Equal(filepath.ToSlash(filepath.Join(base, "alpha-worktree")), got[2].WorktreePath)
	assert.Equal("topic/a", got[2].Branch)
	assert.Equal("zeta", got[3].RepoName)
	assert.Equal("topic/z", got[3].Branch)
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
