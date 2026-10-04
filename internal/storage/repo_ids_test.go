package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoIDsSurviveDeletionAndReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reviews.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, db.DeleteRepo(repo.ID, false))
	replacement, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	assert.Greater(t, replacement.ID, repo.ID)

	require.NoError(t, db.DeleteRepo(replacement.ID, false))
	require.NoError(t, db.Close())
	db, err = Open(path)
	require.NoError(t, err)
	next, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	assert.Greater(t, next.ID, replacement.ID)
}

func TestMigrateRepoIDsPreservesHistory(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "reviews.db")
	conn, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db := &DB{conn}
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	// Build the shipped schema before the forward repository ID migration.
	_, err = db.Exec(schema)
	require.NoError(t, err)
	require.NoError(t, db.migrate())
	repoPath := filepath.ToSlash(filepath.Join(t.TempDir(), "original"))
	_, err = db.Exec(`INSERT INTO repos (id, root_path, name, identity, created_at)
		VALUES (42, ?, 'Custom name', 'https://example.com/team/repo', '2025-01-02 03:04:05')`, repoPath)
	require.NoError(t, err)
	repo, err := db.GetRepoByID(42)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	require.NoError(t, err)
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test"})
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE repo_renames (repo_id INTEGER, name TEXT);
		CREATE TRIGGER record_repo_rename AFTER UPDATE OF name ON repos
		BEGIN INSERT INTO repo_renames VALUES (new.id, new.name); END;`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(path)
	require.NoError(t, err)
	gotRepo, err := db.GetRepoByID(42)
	require.NoError(t, err)
	assert.Equal(repo, gotRepo)
	gotCommit, err := db.GetCommitByID(commit.ID)
	require.NoError(t, err)
	assert.Equal(commit, gotCommit)
	gotJob, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(int64(42), gotJob.RepoID)
	assert.Equal(commit.ID, *gotJob.CommitID)

	// The identity index, path uniqueness constraint, and trigger must survive
	// the table replacement, together with the foreign keys on child tables.
	var indexedID int64
	require.NoError(t, db.QueryRow(`SELECT id FROM repos INDEXED BY idx_repos_identity
		WHERE identity = 'https://example.com/team/repo'`).Scan(&indexedID))
	assert.Equal(int64(42), indexedID)
	_, err = db.Exec(`INSERT INTO repos (root_path, name) VALUES (?, 'duplicate')`, repoPath)
	require.ErrorContains(t, err, "UNIQUE constraint failed")
	_, err = db.Exec(`UPDATE repos SET name = 'Renamed' WHERE id = 42`)
	require.NoError(t, err)
	var renamed string
	require.NoError(t, db.QueryRow(`SELECT name FROM repo_renames WHERE repo_id = 42`).Scan(&renamed))
	assert.Equal("Renamed", renamed)
	_, err = db.Exec(`INSERT INTO commits (repo_id, sha, author, subject, timestamp)
		VALUES (999, 'orphan', 'Author', 'Subject', '2025-01-02')`)
	require.ErrorContains(t, err, "FOREIGN KEY constraint failed")
	var violations int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations))
	assert.Zero(violations)

	require.NoError(t, db.DeleteRepo(repo.ID, true))
	require.NoError(t, db.Close())
	db, err = Open(path)
	require.NoError(t, err)
	replacement, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	assert.Greater(replacement.ID, int64(42))
}
