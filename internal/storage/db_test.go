package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAndClose(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := Open(dbPath)
	require.NoError(t, err, "Open failed")
	defer db.Close()

	// Verify file exists
	_, err = os.Stat(dbPath)
	require.NoError(t, err, "Database file was not created")
}

func TestMigration19_SkippedStatusAndClassifyType(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	_, err := db.ExecContext(context.Background(),
		`INSERT INTO repos (root_path, name) VALUES ('/tmp/r', 'r')`)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO commits (repo_id, sha, author, subject, timestamp)
		 VALUES (1, 'deadbeef', 'a', 's', '2026-04-17T00:00:00Z')`)
	require.NoError(t, err)

	// status=skipped accepted
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO review_jobs (repo_id, commit_id, git_ref, status,
		 review_type, skip_reason) VALUES (1, 1, 'deadbeef', 'skipped',
		 'design', 'trivial diff')`)
	require.NoError(t, err)

	// job_type=classify accepted
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO review_jobs (repo_id, commit_id, git_ref, status,
		 job_type) VALUES (1, 1, 'deadbeef', 'queued', 'classify')`)
	require.NoError(t, err)

	// skip_reason column round-trip
	var reason string
	err = db.QueryRowContext(context.Background(),
		`SELECT skip_reason FROM review_jobs WHERE status = 'skipped'`,
	).Scan(&reason)
	require.NoError(t, err)
	assert.Equal(t, "trivial diff", reason)
}

func TestAutoDesignDedup_AutoRowsDedup(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-dedup").ID
	commitID := createCommit(t, db, repoID, "beef").ID

	_, err := db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
		VALUES (?, ?, 'beef', 'queued', 'design', 'auto_design')
	`, repoID, commitID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
		VALUES (?, ?, 'beef', 'queued', 'design', 'auto_design')
	`, repoID, commitID)
	require.Error(t, err, "second auto_design insert must collide with the unique index")

	res, err := db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
		VALUES (?, ?, 'beef', 'queued', 'design', 'auto_design')
		ON CONFLICT DO NOTHING
	`, repoID, commitID)
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	assert.EqualValues(t, 0, n)
}

func TestAutoDesignDedup_ExplicitRowsNotAffected(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-dedup-explicit").ID
	commitID := createCommit(t, db, repoID, "cafe").ID

	_, err := db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source, skip_reason)
		VALUES (?, ?, 'cafe', 'skipped', 'design', 'auto_design', 'trivial')
	`, repoID, commitID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
		VALUES (?, ?, 'cafe', 'queued', 'design')
	`, repoID, commitID)
	require.NoError(t, err, "explicit design insert must not collide with auto_design row")

	_, err = db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
		VALUES (?, ?, 'cafe', 'queued', 'design')
	`, repoID, commitID)
	require.NoError(t, err, "explicit rerun must not collide")
}

func TestAutoDesignDedup_CommitlessIndex(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-dedup-commitless").ID

	_, err := db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
		VALUES (?, NULL, 'dirty', 'queued', 'design', 'auto_design')
	`, repoID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
		VALUES (?, NULL, 'dirty', 'queued', 'design', 'auto_design')
	`, repoID)
	require.Error(t, err, "second commit-less auto_design insert must collide")

	res, err := db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
		VALUES (?, NULL, 'dirty', 'queued', 'design', 'auto_design')
		ON CONFLICT DO NOTHING
	`, repoID)
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	assert.EqualValues(t, 0, n)

	_, err = db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
		VALUES (?, NULL, 'dirty', 'queued', 'design')
	`, repoID)
	require.NoError(t, err, "explicit commit-less insert must not collide with auto_design")

	_, err = db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
		VALUES (?, NULL, 'dirty-other', 'queued', 'design', 'auto_design')
	`, repoID)
	require.NoError(t, err, "different git_ref must not collide")
}

func TestMigration19_Idempotent(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	// First run happens implicitly at openTestDB; run again manually.
	require.NoError(t, db.migrateReviewJobsConstraintsForAutoDesign())
	require.NoError(t, db.migrateReviewJobsConstraintsForAutoDesign())

	// Still accepts skipped + classify.
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO repos (root_path, name) VALUES ('/tmp/r2', 'r2')`)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO commits (repo_id, sha, author, subject, timestamp)
		 VALUES (1, 'abc', 'a', 's', '2026-04-17T00:00:00Z')`)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO review_jobs (repo_id, commit_id, git_ref, status)
		 VALUES (1, 1, 'abc', 'skipped')`)
	require.NoError(t, err)
}
