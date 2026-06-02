package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupOldSchemaDB(t *testing.T, dbPath string, schema string, seedData string) {
	t.Helper()
	rawDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to open raw DB: %v", err)
	}
	defer rawDB.Close()

	if _, err := rawDB.Exec(schema); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to create old schema: %v", err)
	}
	if seedData != "" {
		if _, err := rawDB.Exec(seedData); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Failed to insert seed data: %v", err)
		}
	}
}

const legacyReviewJobSchema = `
	CREATE TABLE repos (
		id INTEGER PRIMARY KEY,
		root_path TEXT UNIQUE NOT NULL,
		name TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE commits (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id),
		sha TEXT UNIQUE NOT NULL,
		author TEXT NOT NULL,
		subject TEXT NOT NULL,
		timestamp TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE review_jobs (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id),
		commit_id INTEGER REFERENCES commits(id),
		git_ref TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT 'codex',
		status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed')) DEFAULT 'queued',
		enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
		started_at TEXT,
		finished_at TEXT,
		worker_id TEXT,
		error TEXT,
		prompt TEXT,
		retry_count INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE reviews (
		id INTEGER PRIMARY KEY,
		job_id INTEGER UNIQUE NOT NULL REFERENCES review_jobs(id),
		agent TEXT NOT NULL,
		prompt TEXT NOT NULL,
		output TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		addressed INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE responses (
		id INTEGER PRIMARY KEY,
		commit_id INTEGER NOT NULL REFERENCES commits(id),
		responder TEXT NOT NULL,
		response TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE INDEX idx_review_jobs_status ON review_jobs(status);
`

const legacyReviewJobSeed = `
	INSERT INTO repos (id, root_path, name) VALUES (1, '/tmp/test', 'test');
	INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES (1, 1, 'abc123', 'Author', 'Subject', '2024-01-01');
	INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, enqueued_at)
		VALUES (1, 1, 1, 'abc123', 'codex', 'done', '2024-01-01');
	INSERT INTO reviews (id, job_id, agent, prompt, output)
		VALUES (1, 1, 'codex', 'test prompt', 'test output');
`

const legacyReviewJobSchemaWithoutReasoning = `
	CREATE TABLE repos (
		id INTEGER PRIMARY KEY,
		root_path TEXT UNIQUE NOT NULL,
		name TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE commits (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id),
		sha TEXT UNIQUE NOT NULL,
		author TEXT NOT NULL,
		subject TEXT NOT NULL,
		timestamp TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE review_jobs (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id),
		commit_id INTEGER REFERENCES commits(id),
		git_ref TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT 'codex',
		status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed')) DEFAULT 'queued',
		enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
		started_at TEXT,
		finished_at TEXT,
		worker_id TEXT,
		error TEXT,
		prompt TEXT,
		retry_count INTEGER NOT NULL DEFAULT 0
	);
`

const legacyReviewJobSeedWithoutReasoning = `
	INSERT INTO repos (id, root_path, name) VALUES (1, '/tmp/test', 'test');
	INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES (1, 1, 'abc123', 'Author', 'Subject', '2024-01-01');
	INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, enqueued_at)
		VALUES (1, 1, 1, 'abc123', 'codex', 'done', '2024-01-01');
`

const legacyReviewJobSchemaWithReasoning = `
	CREATE TABLE repos (
		id INTEGER PRIMARY KEY,
		root_path TEXT UNIQUE NOT NULL,
		name TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE commits (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id),
		sha TEXT UNIQUE NOT NULL,
		author TEXT NOT NULL,
		subject TEXT NOT NULL,
		timestamp TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE review_jobs (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id),
		commit_id INTEGER REFERENCES commits(id),
		git_ref TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT 'codex',
		reasoning TEXT NOT NULL DEFAULT 'thorough',
		status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed')) DEFAULT 'queued',
		enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
		started_at TEXT,
		finished_at TEXT,
		worker_id TEXT,
		error TEXT,
		prompt TEXT,
		retry_count INTEGER NOT NULL DEFAULT 0
	);
`

const legacyReviewJobSeedWithReasoning = `
	INSERT INTO repos (id, root_path, name) VALUES (1, '/tmp/test', 'test');
	INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES (1, 1, 'abc123', 'Author', 'Subject', '2024-01-01');
	INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, reasoning, status, enqueued_at)
		VALUES (1, 1, 1, 'abc123', 'codex', 'fast', 'done', '2024-01-01');
`

// legacyReviewJobSchemaWithOutputPrefix has the old status CHECK constraint
// (without canceled/applied/rebased) that triggers the rebuild migration in
// db.go, and declares output_prefix so a non-empty value can be seeded into the
// source table the rebuild copies from.
const legacyReviewJobSchemaWithOutputPrefix = `
	CREATE TABLE repos (
		id INTEGER PRIMARY KEY,
		root_path TEXT UNIQUE NOT NULL,
		name TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE commits (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id),
		sha TEXT UNIQUE NOT NULL,
		author TEXT NOT NULL,
		subject TEXT NOT NULL,
		timestamp TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE review_jobs (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id),
		commit_id INTEGER REFERENCES commits(id),
		git_ref TEXT NOT NULL,
		agent TEXT NOT NULL DEFAULT 'codex',
		status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed')) DEFAULT 'queued',
		enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
		started_at TEXT,
		finished_at TEXT,
		worker_id TEXT,
		error TEXT,
		prompt TEXT,
		retry_count INTEGER NOT NULL DEFAULT 0,
		output_prefix TEXT
	);
`

const legacyReviewJobSeedWithOutputPrefix = `
	INSERT INTO repos (id, root_path, name) VALUES (1, '/tmp/test', 'test');
	INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES (1, 1, 'abc123', 'Author', 'Subject', '2024-01-01');
	INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, enqueued_at, output_prefix)
		VALUES (1, 1, 1, 'abc123', 'codex', 'done', '2024-01-01', 'PREFIX: design review');
`

// TestMigrationPreservesOutputPrefixDuringRebuild is a regression test for the
// legacy status-CHECK rebuild migration (db.go): the INSERT...SELECT that copies
// rows into review_jobs_new must carry output_prefix. The old CHECK constraint
// here triggers the rebuild path; the test asserts the seeded non-empty
// output_prefix value survives.
func TestMigrationPreservesOutputPrefixDuringRebuild(t *testing.T) {
	db := prepareMigratedDB(
		t,
		"output_prefix_rebuild.db",
		legacyReviewJobSchemaWithOutputPrefix,
		legacyReviewJobSeedWithOutputPrefix,
	)

	// The rebuild must have replaced the old CHECK constraint (proves the
	// rebuild path actually ran, so this is not a false pass).
	var tableSQL string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_jobs'`,
	).Scan(&tableSQL))
	assert.Contains(t, tableSQL, "'canceled'", "rebuild updated the status CHECK constraint")

	var outputPrefix string
	require.NoError(t, db.QueryRow(
		`SELECT output_prefix FROM review_jobs WHERE id = 1`,
	).Scan(&outputPrefix))
	assert.Equal(t, "PREFIX: design review", outputPrefix,
		"output_prefix preserved through the rebuild migration")
}

func prepareMigratedDB(
	t *testing.T, dbName, schema, seedData string,
) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), dbName)
	setupOldSchemaDB(t, dbPath, schema, seedData)

	db, err := Open(dbPath)
	require.NoError(t, err, "Open() failed after migration")
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrationFromOldSchema(t *testing.T) {
	db := prepareMigratedDB(
		t, "old.db", legacyReviewJobSchema, legacyReviewJobSeed,
	)

	// Verify the old data is preserved
	review, err := db.GetReviewByJobID(1)
	require.NoError(t, err, "GetReviewByJobID failed")
	assert.Equal(t, "test output", review.Output)

	// Verify the new constraint allows 'canceled' status
	// Use raw SQL to insert a job, since the migration test's schema may not
	// have all columns that the high-level EnqueueJob function requires.
	repo, _ := db.GetOrCreateRepo("/tmp/test2")
	commit, _ := db.GetOrCreateCommit(repo.ID, "def456", "A", "S", time.Now())
	result, err := db.Exec(`INSERT INTO review_jobs (repo_id, commit_id, git_ref, agent, status) VALUES (?, ?, 'def456', 'codex', 'queued')`,
		repo.ID, commit.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to insert job: %v", err)
	}
	jobID, _ := result.LastInsertId()

	// Claim the job so we can cancel it
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'worker-1' WHERE id = ?`, jobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to claim job: %v", err)
	}

	// This should succeed with new schema (would fail with old constraint)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'canceled' WHERE id = ?`, jobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Setting canceled status failed after migration: %v", err)
	}

	// Verify the status was set correctly
	var status string
	err = db.QueryRow(`SELECT status FROM review_jobs WHERE id = ?`, jobID).Scan(&status)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to query job status: %v", err)
	}
	if status != "canceled" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected status 'canceled', got '%s'", status)
	}

	// Verify 'applied' and 'rebased' statuses work after migration
	_, err = db.Exec(`UPDATE review_jobs SET status = 'done' WHERE id = ?`, jobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to set done status: %v", err)
	}
	_, err = db.Exec(`UPDATE review_jobs SET status = 'applied' WHERE id = ?`, jobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Setting applied status failed after migration: %v", err)
	}
	_, err = db.Exec(`UPDATE review_jobs SET status = 'done' WHERE id = ?`, jobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to reset to done: %v", err)
	}
	_, err = db.Exec(`UPDATE review_jobs SET status = 'rebased' WHERE id = ?`, jobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Setting rebased status failed after migration: %v", err)
	}

	// Verify constraint still rejects invalid status
	_, err = db.Exec(`UPDATE review_jobs SET status = 'invalid' WHERE id = ?`, jobID)
	if err == nil {
		assert.Condition(t, func() bool {
			return false
		}, "Expected constraint violation for invalid status")
	}

	// Verify FK enforcement works after migration
	// Note: SQLite FKs are OFF by default per-connection, so we can't test that the migration
	// "left FKs enabled" in the pool. What we CAN verify is:
	// 1. The migration succeeded (which includes the FK check at the end)
	// 2. FK enforcement works when enabled on a connection
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to get connection: %v", err)
	}
	defer conn.Close()

	// Check FK pragma value on this connection before we modify it
	// This is informational - FKs are OFF by default in SQLite
	var fkEnabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fkEnabled); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to check foreign_keys pragma: %v", err)
	}
	t.Logf("foreign_keys pragma on pooled connection: %d", fkEnabled)

	// Enable FK enforcement and verify it works (proves schema is correct for FKs)
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to enable foreign keys: %v", err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO reviews (job_id, agent, prompt, output) VALUES (99999, 'test', 'p', 'o')`)
	if err == nil {
		assert.Condition(t, func() bool {
			return false
		}, "Expected foreign key violation for invalid job_id - FKs may not be working")
	}
}

func TestMigrationNormalizesWindowsRepoRootPathConflicts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "paths.db")
	db, err := Open(dbPath)
	require.NoError(t, err)

	const (
		sourceRepoID   = int64(1)
		targetRepoID   = int64(2)
		sourceCommitID = int64(10)
		targetCommitID = int64(20)
		sourceJobID    = int64(100)
		targetJobID    = int64(200)
	)
	_, err = db.Exec(`
		INSERT INTO repos (id, root_path, name, identity)
		VALUES
			(?, 'C:\Users\dev\workspace\repo', 'repo', 'https://github.com/test/repo.git'),
			(?, 'C:/Users/dev/workspace/repo', 'repo', NULL)
	`, sourceRepoID, targetRepoID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES
			(?, ?, 'abc123', 'A', 'source commit', '2024-01-01T00:00:00Z'),
			(?, ?, 'abc123', 'A', 'target commit', '2024-01-01T00:00:00Z')
	`, sourceCommitID, sourceRepoID, targetCommitID, targetRepoID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, review_type, source)
		VALUES
			(?, ?, ?, 'abc123', 'codex', 'done', 'design', 'auto_design'),
			(?, ?, ?, 'abc123', 'codex', 'done', 'design', 'auto_design')
	`, sourceJobID, sourceRepoID, sourceCommitID, targetJobID, targetRepoID, targetCommitID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO responses (id, commit_id, job_id, responder, response)
		VALUES (1, ?, ?, 'codex', 'source response')
	`, sourceCommitID, sourceJobID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	repo, err := db.GetRepoByPath(`C:\Users\dev\workspace\repo`)
	require.NoError(t, err)
	assert.Equal(t, targetRepoID, repo.ID)
	assert.Equal(t, "C:/Users/dev/workspace/repo", repo.RootPath)

	var identity string
	err = db.QueryRow(`SELECT COALESCE(identity, '') FROM repos WHERE id = ?`, targetRepoID).Scan(&identity)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/test/repo.git", identity)

	var repoCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM repos WHERE root_path IN (?, ?)`,
		`C:\Users\dev\workspace\repo`, `C:/Users/dev/workspace/repo`).Scan(&repoCount)
	require.NoError(t, err)
	assert.Equal(t, 1, repoCount)

	var sourceJobRepoID, sourceJobCommitID int64
	err = db.QueryRow(`SELECT repo_id, commit_id FROM review_jobs WHERE id = ?`, sourceJobID).
		Scan(&sourceJobRepoID, &sourceJobCommitID)
	require.NoError(t, err)
	assert.Equal(t, targetRepoID, sourceJobRepoID)
	assert.Equal(t, targetCommitID, sourceJobCommitID)

	var sourceCommitCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM commits WHERE id = ?`, sourceCommitID).
		Scan(&sourceCommitCount)
	require.NoError(t, err)
	assert.Equal(t, 0, sourceCommitCount)

	var responseCommitID int64
	err = db.QueryRow(`SELECT commit_id FROM responses WHERE id = 1`).Scan(&responseCommitID)
	require.NoError(t, err)
	assert.Equal(t, targetCommitID, responseCommitID)
}

func TestMigrationAddsVerdictBoolColumn(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Verify verdict_bool column exists
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('reviews') WHERE name = 'verdict_bool'`).Scan(&count)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to check verdict_bool column: %v", err)
	}
	if count != 1 {
		require.Condition(t, func() bool {
			return false
		}, "verdict_bool column not found in reviews table")
	}

	// Verify the index exists
	var indexCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_reviews_verdict_bool'`).Scan(&indexCount)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to check verdict_bool index: %v", err)
	}
	if indexCount != 1 {
		require.Condition(t, func() bool {
			return false
		}, "idx_reviews_verdict_bool index not found")
	}
}

func TestMigrationAddsSessionIDColumn(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'session_id'`).Scan(&count)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to check session_id column: %v", err)
	}
	if count != 1 {
		require.Condition(t, func() bool {
			return false
		}, "session_id column not found in review_jobs table")
	}
}

func TestCompleteJobPopulatesVerdictBool(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/verdict-test")
	commit := createCommit(t, db, repo.ID, "verdict123")

	t.Run("pass review stores verdict_bool=1", func(t *testing.T) {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "verdict123", Agent: "codex"})
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "EnqueueJob: %v", err)
		}
		claimed := claimJob(t, db, "w1")
		if claimed == nil {
			require.Condition(t, func() bool {
				return false
			}, "no job to claim")
		}

		err = db.CompleteJob(job.ID, "codex", "prompt", "No issues found.")
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "CompleteJob: %v", err)
		}

		var verdictBool sql.NullInt64
		err = db.QueryRow(`SELECT verdict_bool FROM reviews WHERE job_id = ?`, job.ID).Scan(&verdictBool)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "query verdict_bool: %v", err)
		}
		if !verdictBool.Valid || verdictBool.Int64 != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "expected verdict_bool=1 (pass), got %v", verdictBool)
		}
	})

	t.Run("fail review stores verdict_bool=0", func(t *testing.T) {
		commit2 := createCommit(t, db, repo.ID, "verdict456")
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit2.ID, GitRef: "verdict456", Agent: "codex"})
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "EnqueueJob: %v", err)
		}
		claimed := claimJob(t, db, "w2")
		if claimed == nil {
			require.Condition(t, func() bool {
				return false
			}, "no job to claim")
		}

		err = db.CompleteJob(job.ID, "codex", "prompt", "- High — SQL injection in login handler")
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "CompleteJob: %v", err)
		}

		var verdictBool sql.NullInt64
		err = db.QueryRow(`SELECT verdict_bool FROM reviews WHERE job_id = ?`, job.ID).Scan(&verdictBool)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "query verdict_bool: %v", err)
		}
		if !verdictBool.Valid || verdictBool.Int64 != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "expected verdict_bool=0 (fail), got %v", verdictBool)
		}
	})
}

func TestMigrationQuotedTableWithOrphanedFK(t *testing.T) {
	// Regression test: after a prior migration rebuilds review_jobs via
	// ALTER TABLE ... RENAME, SQLite stores the table name quoted as
	// "review_jobs". The applied/rebased constraint migration must
	// handle this quoted form. Additionally, pre-existing orphaned FK
	// rows (referencing deleted repos) must not block the migration.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "quoted.db")

	rawDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to open raw DB: %v", err)
	}

	// Create base schema
	_, err = rawDB.Exec(`
		CREATE TABLE repos (
			id INTEGER PRIMARY KEY,
			root_path TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE commits (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			sha TEXT NOT NULL,
			author TEXT NOT NULL,
			subject TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE reviews (
			id INTEGER PRIMARY KEY,
			job_id INTEGER UNIQUE NOT NULL,
			agent TEXT NOT NULL,
			prompt TEXT NOT NULL,
			output TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			addressed INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE responses (
			id INTEGER PRIMARY KEY,
			commit_id INTEGER REFERENCES commits(id),
			responder TEXT NOT NULL,
			response TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
	`)
	if err != nil {
		rawDB.Close()
		require.Condition(t, func() bool {
			return false
		}, "Failed to create base schema: %v", err)
	}

	// Simulate a prior migration that rebuilt the table via RENAME,
	// which causes SQLite to quote the table name in sqlite_master.
	// Use the 'canceled' constraint (pre-applied/rebased).
	_, err = rawDB.Exec(`
		CREATE TABLE review_jobs_temp (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			commit_id INTEGER REFERENCES commits(id),
			git_ref TEXT NOT NULL,
			branch TEXT,
			agent TEXT NOT NULL DEFAULT 'codex',
			model TEXT,
			reasoning TEXT NOT NULL DEFAULT 'thorough',
			status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed','canceled')) DEFAULT 'queued',
			enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
			started_at TEXT,
			finished_at TEXT,
			worker_id TEXT,
			error TEXT,
			prompt TEXT,
			retry_count INTEGER NOT NULL DEFAULT 0,
			diff_content TEXT,
			agentic INTEGER NOT NULL DEFAULT 0,
			uuid TEXT,
			source_machine_id TEXT,
			updated_at TEXT,
			synced_at TEXT,
			output_prefix TEXT,
			job_type TEXT NOT NULL DEFAULT 'review',
			review_type TEXT NOT NULL DEFAULT '',
			patch_id TEXT
		);
		ALTER TABLE review_jobs_temp RENAME TO review_jobs;
		CREATE INDEX idx_review_jobs_status ON review_jobs(status);
		CREATE INDEX idx_review_jobs_repo ON review_jobs(repo_id);
		CREATE INDEX idx_review_jobs_git_ref ON review_jobs(git_ref);
		CREATE INDEX idx_review_jobs_branch ON review_jobs(branch);
	`)
	if err != nil {
		rawDB.Close()
		require.Condition(t, func() bool {
			return false
		}, "Failed to create renamed table: %v", err)
	}

	// Verify the table name is quoted in sqlite_master
	var tableSql string
	if err := rawDB.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_jobs'`,
	).Scan(&tableSql); err != nil {
		rawDB.Close()
		require.Condition(t, func() bool {
			return false
		}, "Failed to read table schema: %v", err)
	}
	if !strings.Contains(tableSql, `"review_jobs"`) {
		rawDB.Close()
		require.Condition(t, func() bool {
			return false
		}, "Expected quoted table name, got: %s",
			tableSql[:min(len(tableSql), 80)])

	}

	// Insert valid data
	_, err = rawDB.Exec(`
		INSERT INTO repos (id, root_path, name)
			VALUES (1, '/tmp/test', 'test');
		INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
			VALUES (1, 1, 'abc123', 'Author', 'Subject', '2024-01-01');
		INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, agentic)
			VALUES (1, 1, 1, 'abc123', 'codex', 'done', 1);
		INSERT INTO reviews (id, job_id, agent, prompt, output)
			VALUES (1, 1, 'codex', 'test', 'looks good');
	`)
	if err != nil {
		rawDB.Close()
		require.Condition(t, func() bool {
			return false
		}, "Failed to insert valid data: %v", err)
	}

	// Insert an orphaned row (repo_id=999 doesn't exist) to simulate
	// pre-existing FK violations
	_, err = rawDB.Exec(`
		INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status)
			VALUES (2, 999, NULL, 'orphan', 'codex', 'done');
	`)
	if err != nil {
		rawDB.Close()
		require.Condition(t, func() bool {
			return false
		}, "Failed to insert orphaned row: %v", err)
	}
	rawDB.Close()

	// Open with our migration code — must not fail
	db, err := Open(dbPath)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Open() failed on quoted-table DB: %v", err)
	}
	defer db.Close()

	// Verify data preserved
	review, err := db.GetReviewByJobID(1)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetReviewByJobID failed: %v", err)
	}
	if review.Output != "looks good" {
		assert.Condition(t, func() bool {
			return false
		}, "Review output not preserved: got %q", review.Output)
	}

	job, err := db.GetJobByID(1)
	require.NoError(t, err)
	assert.True(t, job.Agentic, "agentic flag should survive rebuild migration")

	// Verify applied/rebased statuses work
	_, err = db.Exec(
		`UPDATE review_jobs SET status = 'applied' WHERE id = 1`,
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Setting applied status failed: %v", err)
	}
	_, err = db.Exec(
		`UPDATE review_jobs SET status = 'rebased' WHERE id = 1`,
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Setting rebased status failed: %v", err)
	}

	// Verify orphaned row still exists (migration didn't drop data)
	var count int
	db.QueryRow(`SELECT count(*) FROM review_jobs WHERE id = 2`).Scan(&count)
	if count != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "Orphaned row was lost during migration")
	}
}

func TestMigrationCleansUpStaleTemp(t *testing.T) {
	// If a prior migration attempt failed and left review_jobs_new
	// behind, the next attempt should clean it up and succeed.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "stale.db")

	rawDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to open raw DB: %v", err)
	}

	_, err = rawDB.Exec(`
		CREATE TABLE repos (
			id INTEGER PRIMARY KEY,
			root_path TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE commits (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			sha TEXT NOT NULL,
			author TEXT NOT NULL,
			subject TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE reviews (
			id INTEGER PRIMARY KEY,
			job_id INTEGER UNIQUE NOT NULL,
			agent TEXT NOT NULL,
			prompt TEXT NOT NULL,
			output TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			addressed INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE responses (
			id INTEGER PRIMARY KEY,
			commit_id INTEGER REFERENCES commits(id),
			responder TEXT NOT NULL,
			response TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE review_jobs_temp (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			commit_id INTEGER REFERENCES commits(id),
			git_ref TEXT NOT NULL,
			branch TEXT,
			agent TEXT NOT NULL DEFAULT 'codex',
			model TEXT,
			reasoning TEXT NOT NULL DEFAULT 'thorough',
			status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed','canceled')) DEFAULT 'queued',
			enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
			started_at TEXT,
			finished_at TEXT,
			worker_id TEXT,
			error TEXT,
			prompt TEXT,
			retry_count INTEGER NOT NULL DEFAULT 0,
			diff_content TEXT,
			agentic INTEGER NOT NULL DEFAULT 0,
			uuid TEXT,
			source_machine_id TEXT,
			updated_at TEXT,
			synced_at TEXT,
			output_prefix TEXT,
			job_type TEXT NOT NULL DEFAULT 'review',
			review_type TEXT NOT NULL DEFAULT '',
			patch_id TEXT
		);
		ALTER TABLE review_jobs_temp RENAME TO review_jobs;
		CREATE INDEX idx_review_jobs_status ON review_jobs(status);

		-- Simulate stale temp table from prior failed migration
		CREATE TABLE review_jobs_new (id INTEGER PRIMARY KEY);

		INSERT INTO repos (id, root_path, name)
			VALUES (1, '/tmp/test', 'test');
		INSERT INTO review_jobs (id, repo_id, git_ref, agent, status)
			VALUES (1, 1, 'abc', 'codex', 'done');
	`)
	if err != nil {
		rawDB.Close()
		require.Condition(t, func() bool {
			return false
		}, "Failed to create schema: %v", err)
	}
	rawDB.Close()

	db, err := Open(dbPath)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Open() failed with stale temp table: %v", err)
	}
	defer db.Close()

	// Verify stale temp table was cleaned up
	var tempCount int
	err = db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='review_jobs_new'`,
	).Scan(&tempCount)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to check for stale temp table: %v", err)
	}
	if tempCount != 0 {
		assert.Condition(t, func() bool {
			return false
		}, "review_jobs_new should not exist after migration")
	}

	// Verify applied works and row was preserved
	res, err := db.Exec(
		`UPDATE review_jobs SET status = 'applied' WHERE id = 1`,
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Setting applied status failed: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "RowsAffected: %v", err)
	}
	if affected != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected 1 row affected, got %d", affected)
	}

	// Confirm status was actually set
	var status string
	err = db.QueryRow(
		`SELECT status FROM review_jobs WHERE id = 1`,
	).Scan(&status)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to read back status: %v", err)
	}
	if status != "applied" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected status 'applied', got %q", status)
	}
}

func TestMigrationWithAlterTableColumnOrder(t *testing.T) {
	// Test that migration works when columns were added via ALTER TABLE,
	// which puts them at the end of the table (different from CREATE TABLE order)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "altered.db")

	// Create a very old schema WITHOUT prompt and retry_count columns
	setupOldSchemaDB(t, dbPath, `
		CREATE TABLE repos (
			id INTEGER PRIMARY KEY,
			root_path TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE commits (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			sha TEXT UNIQUE NOT NULL,
			author TEXT NOT NULL,
			subject TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE review_jobs (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			commit_id INTEGER REFERENCES commits(id),
			git_ref TEXT NOT NULL,
			agent TEXT NOT NULL DEFAULT 'codex',
			status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed')) DEFAULT 'queued',
			enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
			started_at TEXT,
			finished_at TEXT,
			worker_id TEXT,
			error TEXT
		);
		CREATE TABLE reviews (
			id INTEGER PRIMARY KEY,
			job_id INTEGER UNIQUE NOT NULL REFERENCES review_jobs(id),
			agent TEXT NOT NULL,
			prompt TEXT NOT NULL,
			output TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			addressed INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE responses (
			id INTEGER PRIMARY KEY,
			commit_id INTEGER NOT NULL REFERENCES commits(id),
			responder TEXT NOT NULL,
			response TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
	`, `ALTER TABLE review_jobs ADD COLUMN prompt TEXT;
ALTER TABLE review_jobs ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;
INSERT INTO repos (id, root_path, name) VALUES (1, '/tmp/altered', 'altered');
INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES (1, 1, 'alter123', 'Author', 'Subject', '2024-01-01');
INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, enqueued_at, prompt, retry_count)
		VALUES (1, 1, 1, 'alter123', 'codex', 'done', '2024-01-01', 'my prompt', 2);
INSERT INTO reviews (id, job_id, agent, prompt, output)
		VALUES (1, 1, 'codex', 'test prompt', 'test output');`)

	// Open with our Open() function - should trigger CHECK constraint migration
	// This tests that the explicit column naming in INSERT works correctly
	// even when column order differs from schema definition
	db, err := Open(dbPath)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Open() failed after migration: %v", err)
	}
	defer db.Close()

	// Verify job data is preserved with correct values
	job, err := db.GetJobByID(1)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetJobByID failed: %v", err)
	}
	if job.GitRef != "alter123" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected git_ref 'alter123', got '%s'", job.GitRef)
	}
	if job.Agent != "codex" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected agent 'codex', got '%s'", job.Agent)
	}

	// Verify retry_count was preserved (query directly since GetJobByID doesn't load it)
	var retryCount int
	err = db.QueryRow(`SELECT retry_count FROM review_jobs WHERE id = 1`).Scan(&retryCount)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to query retry_count: %v", err)
	}
	if retryCount != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected retry_count 2, got %d", retryCount)
	}

	// Verify prompt was preserved
	var prompt string
	err = db.QueryRow(`SELECT prompt FROM review_jobs WHERE id = 1`).Scan(&prompt)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to query prompt: %v", err)
	}
	if prompt != "my prompt" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected prompt 'my prompt', got '%s'", prompt)
	}

	// Verify review data is preserved
	review, err := db.GetReviewByJobID(1)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetReviewByJobID failed: %v", err)
	}
	if review.Output != "test output" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected output 'test output', got '%s'", review.Output)
	}

	// Verify new constraint works by creating and canceling a job.
	// Use raw SQL to insert/update the job, since the migration test's schema may
	// not have all columns that the high-level EnqueueJob function requires.
	repo2, err := db.GetOrCreateRepo("/tmp/test2")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}
	commit2, err := db.GetOrCreateCommit(repo2.ID, "newsha", "A", "S", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	result2, err := db.Exec(`INSERT INTO review_jobs (repo_id, commit_id, git_ref, agent, status) VALUES (?, ?, 'newsha', 'codex', 'queued')`,
		repo2.ID, commit2.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to insert job: %v", err)
	}
	newJobID, _ := result2.LastInsertId()

	// Claim the job
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'worker-1' WHERE id = ?`, newJobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to claim job: %v", err)
	}

	// Verify the claimed status
	var claimedStatus string
	err = db.QueryRow(`SELECT status FROM review_jobs WHERE id = ?`, newJobID).Scan(&claimedStatus)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to query job status: %v", err)
	}
	if claimedStatus != "running" {
		require.Condition(t, func() bool {
			return false
		}, "Expected job status 'running', got '%s'", claimedStatus)
	}

	// Cancel - this should succeed with new schema (would fail with old constraint)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'canceled' WHERE id = ?`, newJobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Setting canceled status failed after migration: %v", err)
	}
}

func TestMigrationReasoningColumn(t *testing.T) {
	t.Run("missing reasoning gets default", func(t *testing.T) {
		db := prepareMigratedDB(
			t,
			"missing_reasoning.db",
			legacyReviewJobSchemaWithoutReasoning,
			legacyReviewJobSeedWithoutReasoning,
		)

		var reasoning string
		err := db.QueryRow(
			`SELECT reasoning FROM review_jobs WHERE id = 1`,
		).Scan(&reasoning)
		require.NoError(t, err, "Failed to read reasoning")
		assert.Equal(t, "thorough", reasoning)
	})

	t.Run("preserves existing reasoning during rebuild", func(t *testing.T) {
		db := prepareMigratedDB(
			t,
			"preserve_reasoning.db",
			legacyReviewJobSchemaWithReasoning,
			legacyReviewJobSeedWithReasoning,
		)

		var reasoning string
		err := db.QueryRow(
			`SELECT reasoning FROM review_jobs WHERE id = 1`,
		).Scan(&reasoning)
		require.NoError(t, err, "Failed to read reasoning")
		assert.Equal(t, "fast", reasoning)
	})
}

// seedLegacyBatchWithJobs recreates the retired ci_pr_batches /
// ci_pr_batch_jobs tables (the live schema no longer creates them) and
// seeds a batch linked to a queued and a running job, plus an unlinked
// done job. Only the columns the drain query touches are declared.
func seedLegacyBatchWithJobs(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.Exec(`
		CREATE TABLE ci_pr_batches (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			github_repo TEXT,
			pr_number INTEGER,
			head_sha TEXT,
			total_jobs INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE ci_pr_batch_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id INTEGER,
			job_id INTEGER
		);
	`)
	require.NoError(t, err, "create legacy batch tables")

	repo := createRepo(t, db, "/tmp/legacy-batch-drain")
	queuedCommit := createCommit(t, db, repo.ID, "sha-queued")
	runningCommit := createCommit(t, db, repo.ID, "sha-running")
	doneCommit := createCommit(t, db, repo.ID, "sha-done")
	queued := enqueueJob(t, db, repo.ID, queuedCommit.ID, "sha-queued")
	running := enqueueJob(t, db, repo.ID, runningCommit.ID, "sha-running")
	done := enqueueJob(t, db, repo.ID, doneCommit.ID, "sha-done")
	setJobStatus(t, db, running.ID, JobStatusRunning)
	setJobStatus(t, db, done.ID, JobStatusDone)

	res, err := db.Exec(
		`INSERT INTO ci_pr_batches (github_repo, pr_number, head_sha, total_jobs)
		 VALUES ('owner/repo', 1, 'headsha', 2)`)
	require.NoError(t, err, "insert legacy batch")
	batchID, err := res.LastInsertId()
	require.NoError(t, err)

	for _, jobID := range []int64{queued.ID, running.ID} {
		_, err = db.Exec(
			`INSERT INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`, batchID, jobID)
		require.NoError(t, err, "link batch job %d", jobID)
	}
}

func legacyTableExists(t *testing.T, db *DB, name string) bool {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count))
	return count > 0
}

func TestDrainAndDropOldCIBatchTables(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	seedLegacyBatchWithJobs(t, db)

	require.NoError(t, db.drainAndDropOldCIBatchTables())

	var canceled int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM review_jobs WHERE status='canceled' AND error='superseded by panel migration'`).Scan(&canceled))
	assert.Equal(2, canceled, "queued+running batch jobs are canceled")
	assert.False(legacyTableExists(t, db, "ci_pr_batches"))
	assert.False(legacyTableExists(t, db, "ci_pr_batch_jobs"))

	// Idempotent: a second run is a clean no-op (tables already gone).
	require.NoError(t, db.drainAndDropOldCIBatchTables())
}

func TestDrainAndDropOldCIBatchTablesHandlesMissingJoinTable(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	_, err := db.Exec(`
		CREATE TABLE ci_pr_batches (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			github_repo TEXT,
			pr_number INTEGER,
			head_sha TEXT,
			total_jobs INTEGER NOT NULL DEFAULT 0
		)
	`)
	require.NoError(t, err)

	require.NoError(t, db.drainAndDropOldCIBatchTables())
	assert.False(t, legacyTableExists(t, db, "ci_pr_batches"))
	assert.False(t, legacyTableExists(t, db, "ci_pr_batch_jobs"))
}

func TestPatchIDMigration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'patch_id'`).Scan(&count)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "check patch_id column: %v", err)
	}
	if count != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "expected patch_id column to exist, got count=%d", count)
	}
}
