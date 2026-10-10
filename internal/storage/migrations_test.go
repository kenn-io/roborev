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
	_, err := db.Exec(`DROP TABLE schema_migrations`)
	require.NoError(t, err)
}

func appliedVersions(t *testing.T, db *DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
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
	_, err = previous.Exec(schema)
	require.NoError(t, err)
	require.NoError(t, previous.migrate())
	_, err = previous.Exec(`INSERT INTO repos (root_path, name) VALUES ('/synthetic/repo', 'repo')`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	db, err := Open(path)
	require.NoError(t, err)
	defer db.Close()
	assert.Equal(t, []int{1}, appliedVersions(t, db))
	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM repos WHERE root_path = '/synthetic/repo'`).Scan(&name))
	assert.Equal(t, "repo", name)
}
