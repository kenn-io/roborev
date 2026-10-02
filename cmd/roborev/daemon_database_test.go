package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"

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

func TestDaemonDatabaseAliasesHaveOneOwner(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprint(relative), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "reviews.db")
			alias := filepath.Join(dir, "alias.db")
			destination := target
			if relative {
				destination = filepath.Base(target)
			}
			if err := os.Symlink(destination, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			owner, err := lockDaemonDatabase(alias)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, owner.Close()) })
			for _, created := range []bool{false, true} {
				if created {
					db, err := storage.Open(alias)
					require.NoError(t, err)
					require.NoError(t, db.Close())
				}
				other, err := lockDaemonDatabase(target)
				if other != nil {
					require.NoError(t, other.Close())
				}
				require.ErrorContains(t, err, "already owned by a daemon")
			}
		})
	}
}

func TestDaemonDatabaseDirectoryAliasesHaveOneOwner(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	_, err := fslink.LinkDir(dir, alias)
	require.NoError(t, err)

	owner, err := lockDaemonDatabase(filepath.Join(alias, "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })

	other, err := lockDaemonDatabase(filepath.Join(dir, "reviews.db"))
	if other != nil {
		require.NoError(t, other.Close())
	}
	require.ErrorContains(t, err, "already owned by a daemon")
}
