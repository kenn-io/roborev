package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openUnmigratedDB(t *testing.T) *DB {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	return &DB{DB: raw}
}

// forgetSchemaVersion makes a database look like one written before numbered
// migrations existed, so the next Open runs the baseline migration again.
func forgetSchemaVersion(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `DROP TABLE schema_migrations`)
	require.NoError(t, err)
}

func appliedVersions(t *testing.T, db *DB) []int {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT version FROM schema_migrations ORDER BY version`)
	require.NoError(t, err)
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		versions = append(versions, v)
	}
	require.NoError(t, rows.Err())
	return versions
}

func TestSchemaMigrationVersionsAreContiguous(t *testing.T) {
	t.Parallel()
	for i, m := range schemaMigrations {
		assert.Equal(t, i+1, m.version, m.name)
	}
}

func TestRunSchemaMigrationsAppliesEachVersionOnce(t *testing.T) {
	t.Parallel()
	db := openUnmigratedDB(t)
	var ran []string
	step := func(name string) func(context.Context, *DB) error {
		return func(context.Context, *DB) error {
			ran = append(ran, name)
			return nil
		}
	}
	first := []schemaMigration{{1, "one", step("one")}}
	require.NoError(t, db.runSchemaMigrations(t.Context(), first))
	both := append(first, schemaMigration{2, "two", step("two")})
	require.NoError(t, db.runSchemaMigrations(t.Context(), both))
	require.NoError(t, db.runSchemaMigrations(t.Context(), both))

	assert.Equal(t, []string{"one", "two"}, ran)
	assert.Equal(t, []int{1, 2}, appliedVersions(t, db))
}

func TestRunSchemaMigrationsRetriesAFailedMigration(t *testing.T) {
	t.Parallel()
	db := openUnmigratedDB(t)
	attempts := 0
	migrations := []schemaMigration{{1, "flaky", func(context.Context, *DB) error {
		attempts++
		if attempts == 1 {
			return errors.New("interrupted")
		}
		return nil
	}}}

	require.ErrorContains(t, db.runSchemaMigrations(t.Context(), migrations), "interrupted")
	assert.Empty(t, appliedVersions(t, db))
	require.NoError(t, db.runSchemaMigrations(t.Context(), migrations))
	assert.Equal(t, []int{1}, appliedVersions(t, db))
}

func TestRunSchemaMigrationsRefusesNewerDatabase(t *testing.T) {
	t.Parallel()
	db := openUnmigratedDB(t)
	noop := func(context.Context, *DB) error { return nil }
	require.NoError(t, db.runSchemaMigrations(t.Context(),
		[]schemaMigration{{1, "one", noop}, {2, "two", noop}}))

	err := db.runSchemaMigrations(t.Context(), []schemaMigration{{1, "one", noop}})
	require.ErrorIs(t, err, ErrSchemaTooNew)
}

func TestOpenBaselinesDatabaseFromBeforeNumberedMigrations(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reviews.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	previous := &DB{DB: raw}
	_, err = previous.ExecContext(t.Context(), schema)
	require.NoError(t, err)
	require.NoError(t, previous.migrate())
	_, err = previous.ExecContext(t.Context(), `INSERT INTO repos (root_path, name) VALUES ('/synthetic/repo', 'repo')`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	db, err := Open(path)
	require.NoError(t, err)
	defer db.Close()
	assert.Equal(t, []int{1, 2, 3}, appliedVersions(t, db))
	var name string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT name FROM repos WHERE root_path = '/synthetic/repo'`).Scan(&name))
	assert.Equal(t, "repo", name)
}

func TestOpenWidensAutoDesignDedupIndexAfterDuplicatesAreRemoved(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reviews.db")
	db, err := Open(path)
	require.NoError(t, err)
	repoID := createRepo(t, db, "/synthetic/repo").ID
	commitID := createCommit(t, db, repoID, "beef").ID
	insertAutoDesign := func(db *DB, commitID any) error {
		_, err := db.ExecContext(t.Context(), `
			INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
			VALUES (?, ?, 'beef', 'queued', 'design', 'auto_design')
		`, repoID, commitID)
		return err
	}
	// Reproduce a database that reached the baseline with a commitless and a
	// commit-backed auto-design job for one git_ref, so only the narrow index fit.
	_, err = db.ExecContext(t.Context(), `DROP INDEX idx_review_jobs_auto_design_dedup_ref`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		CREATE UNIQUE INDEX idx_review_jobs_auto_design_dedup_ref
		ON review_jobs(repo_id, git_ref, review_type)
		WHERE source = 'auto_design' AND commit_id IS NULL
	`)
	require.NoError(t, err)
	require.NoError(t, insertAutoDesign(db, nil))
	require.NoError(t, insertAutoDesign(db, commitID))
	require.NoError(t, db.Close())

	db, err = Open(path)
	require.NoError(t, err, "duplicates must not block startup")
	var jobs int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM review_jobs`).Scan(&jobs))
	assert.Equal(t, 2, jobs)
	_, err = db.ExecContext(t.Context(), `DELETE FROM review_jobs WHERE commit_id IS NOT NULL`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(path)
	require.NoError(t, err)
	defer db.Close()
	require.Error(t, insertAutoDesign(db, commitID),
		"a commit-backed job must collide with the commitless job for its git_ref")
}

func TestCIPanelRetireReasonMigration(t *testing.T) {
	t.Parallel()
	db := openUnmigratedDB(t)
	require.NoError(t, db.runSchemaMigrations(t.Context(), schemaMigrations[:2]))
	_, err := db.ExecContext(t.Context(), `INSERT INTO ci_pr_panels
		(github_repo, pr_number, head_sha, panel_run_uuid, retired_at)
		VALUES ('acme/api', 7, 'old-head', '00000000-0000-4000-8000-000000000001', '2026-01-01 00:00:00')`)
	require.NoError(t, err)

	for range 2 {
		require.NoError(t, db.runSchemaMigrations(t.Context(), schemaMigrations))
		var reason sql.NullString
		var retiredAt string
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT CAST(retired_at AS TEXT), retire_reason FROM ci_pr_panels WHERE id = 1`).Scan(&retiredAt, &reason))
		assert.Equal(t, "2026-01-01 00:00:00", retiredAt)
		assert.False(t, reason.Valid, "historical retirement has no inferred cause")
		assert.Equal(t, []int{1, 2, 3}, appliedVersions(t, db))
		// Simulate interruption after the column was added, before recording it.
		_, err = db.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE version = 3`)
		require.NoError(t, err)
	}
}
