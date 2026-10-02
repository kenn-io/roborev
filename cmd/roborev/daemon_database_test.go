package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestDaemonDatabaseHasOneOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	owner, err := lockDaemonDatabase(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	_, err = lockDaemonDatabase(path)
	require.ErrorContains(t, err, "already owned by a daemon")
	require.NoError(t, owner.Close())
	next, err := lockDaemonDatabase(path)
	require.NoError(t, err)
	require.NoError(t, next.Close())
}

func TestDaemonRunRejectsOtherOwnerBeforeMigration(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	path := storage.DefaultDBPath()
	db, err := storage.Open(path)
	require.NoError(t, err)
	_, err = db.Exec("DELETE FROM sync_state WHERE key = 'repo_names_from_identity'")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	owner, err := lockDaemonDatabase(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	cmd := daemonRunCmd()
	cmd.SetArgs([]string{"--db", path})
	require.ErrorContains(t, cmd.Execute(), "already owned by a daemon")
	read, err := storage.OpenReadOnly(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })
	marker, err := read.GetSyncState("repo_names_from_identity")
	require.NoError(t, err)
	require.Empty(t, marker, "rejected startup must not run the pending migration")
}
