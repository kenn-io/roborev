package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"go.kenn.io/roborev/internal/config"
)

const schema = `
CREATE TABLE IF NOT EXISTS repos (
  id INTEGER PRIMARY KEY,
  root_path TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS commits (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  sha TEXT UNIQUE NOT NULL,
  author TEXT NOT NULL,
  subject TEXT NOT NULL,
  timestamp TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS review_jobs (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  commit_id INTEGER REFERENCES commits(id),
  git_ref TEXT NOT NULL,
  branch TEXT,
  ci_base_branch TEXT,
  session_id TEXT,
  agent TEXT NOT NULL DEFAULT 'codex',
  model TEXT,
  requested_model TEXT,
  requested_provider TEXT,
  reasoning TEXT NOT NULL DEFAULT 'thorough',
  status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed','canceled','applied','rebased','skipped')) DEFAULT 'queued',
  enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
  started_at TEXT,
  finished_at TEXT,
  worker_id TEXT,
  error TEXT,
  prompt TEXT,
  retry_count INTEGER NOT NULL DEFAULT 0,
  diff_content TEXT,
  dirty_files TEXT,
  output_prefix TEXT,
  job_type TEXT NOT NULL DEFAULT 'review',
  review_type TEXT NOT NULL DEFAULT '',
  provider TEXT,
  skip_reason TEXT,
  source TEXT,
  backup_agent TEXT NOT NULL DEFAULT '',
  backup_model TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS reviews (
  id INTEGER PRIMARY KEY,
  job_id INTEGER UNIQUE NOT NULL REFERENCES review_jobs(id),
  agent TEXT NOT NULL,
  prompt TEXT NOT NULL,
  output TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  closed INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS responses (
  id INTEGER PRIMARY KEY,
  commit_id INTEGER REFERENCES commits(id),
  responder TEXT NOT NULL,
  response TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS ci_pr_reviews (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  github_repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  head_sha TEXT NOT NULL,
  job_id INTEGER NOT NULL REFERENCES review_jobs(id),
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(github_repo, pr_number, head_sha)
);

-- The retired ci_pr_batches / ci_pr_batch_jobs tables (the panel-based
-- predecessor's tracking) are deliberately NOT created here. Existing
-- databases have them dropped by drainAndDropOldCIBatchTables (F14).

-- ci_pr_panels maps a PR HEAD to the panel run that reviews it and to that
-- run's synthesis job. It is the panel-based successor to ci_pr_batches.
-- synthesis_job_id is nullable so the creating transaction (CreateCIPanelRun)
-- can backfill it after enqueuing the run.
CREATE TABLE IF NOT EXISTS ci_pr_panels (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  github_repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  head_sha TEXT NOT NULL,
  panel_run_uuid TEXT NOT NULL,
  synthesis_job_id INTEGER,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  posting_claimed_at TIMESTAMP,
  posted_at TIMESTAMP,
  retired_at TIMESTAMP,
  UNIQUE(github_repo, pr_number, head_sha)
);

-- ci_pr_review_attempts holds local CI-poller retry state keyed by
-- (github_repo, pr_number, head_sha). It is the durable source of truth for
-- whether a HEAD is being reviewed and, when an AI-provider outage defers it,
-- when to retry. One row per reviewed HEAD. next_attempt_at is NULL while a
-- run is in-flight or pending and set once deferred. state is one of
-- 'pending', 'deferred', or 'done'. This table is created in both the SQLite
-- and Postgres backends for schema parity per the design, but it is NOT
-- registered in any sync cursor (not sync-replicated). Avoid inline
-- semicolons in this comment -- pgSchemaStatements splits the embedded
-- Postgres schema on semicolons, so a literal one here would fragment the
-- comment into a bad statement.
CREATE TABLE IF NOT EXISTS ci_pr_review_attempts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  github_repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  head_sha TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 1,
  first_attempt_at TEXT NOT NULL,
  next_attempt_at TEXT,
  last_error_class TEXT NOT NULL DEFAULT '',
  consecutive_genuine_attempts INTEGER NOT NULL DEFAULT 0,
  last_error_excerpt TEXT NOT NULL DEFAULT '',
  last_panel_run_uuid TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending',
  updated_at TEXT NOT NULL,
  UNIQUE(github_repo, pr_number, head_sha)
);

CREATE TABLE IF NOT EXISTS daemon_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_review_jobs_status ON review_jobs(status);
CREATE INDEX IF NOT EXISTS idx_review_jobs_repo ON review_jobs(repo_id);
CREATE INDEX IF NOT EXISTS idx_review_jobs_git_ref ON review_jobs(git_ref);
CREATE INDEX IF NOT EXISTS idx_commits_sha ON commits(sha);
-- Partial unique indexes for auto-design dedup are created by
-- migrateReviewJobsConstraintsForAutoDesign — placing them here would
-- break legacy-schema migrations where the source column doesn't yet exist.
`

type DB struct {
	*sql.DB
}

// DefaultDBPath returns the default database path
func DefaultDBPath() string {
	return filepath.Join(config.DataDir(), "reviews.db")
}

// Open opens or creates the database at the given path
func Open(dbPath string) (*DB, error) {
	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	// Open with WAL mode and busy timeout.
	// 30s busy_timeout gives enough headroom for concurrent writers
	// (worker pool + sync worker) to wait for locks rather than failing.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	wrapped := &DB{db}

	// Initialize schema (CREATE IF NOT EXISTS is idempotent)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}

	// Run migrations for existing databases
	if err := wrapped.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return wrapped, nil
}

// migrate runs any needed migrations for existing databases
func (db *DB) migrate() error {
	// Migration: add prompt column to review_jobs if missing
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'prompt'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check prompt column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN prompt TEXT`)
		if err != nil {
			return fmt.Errorf("add prompt column: %w", err)
		}
	}

	// Migration: add closed column to reviews if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('reviews') WHERE name = 'closed'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check closed column: %w", err)
	}
	if count == 0 {
		// Check if old 'addressed' column exists and rename it
		var hasAddressed int
		_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('reviews') WHERE name = 'addressed'`).Scan(&hasAddressed)
		if hasAddressed > 0 {
			_, err = db.Exec(`ALTER TABLE reviews RENAME COLUMN addressed TO closed`)
		} else {
			_, err = db.Exec(`ALTER TABLE reviews ADD COLUMN closed INTEGER NOT NULL DEFAULT 0`)
		}
		if err != nil {
			return fmt.Errorf("add closed column: %w", err)
		}
	}

	// Migration: add retry_count column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'retry_count'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check retry_count column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`)
		if err != nil {
			return fmt.Errorf("add retry_count column: %w", err)
		}
	}

	// Migration: add diff_content column to review_jobs if missing (for dirty reviews)
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'diff_content'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check diff_content column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN diff_content TEXT`)
		if err != nil {
			return fmt.Errorf("add diff_content column: %w", err)
		}
	}

	// Migration: add dirty_files column to review_jobs if missing (for dirty review metadata)
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'dirty_files'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check dirty_files column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN dirty_files TEXT`)
		if err != nil {
			return fmt.Errorf("add dirty_files column: %w", err)
		}
	}

	// Migration: add reasoning column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'reasoning'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check reasoning column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN reasoning TEXT NOT NULL DEFAULT 'thorough'`)
		if err != nil {
			return fmt.Errorf("add reasoning column: %w", err)
		}
	}

	// Migration: add agentic column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'agentic'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check agentic column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN agentic INTEGER NOT NULL DEFAULT 0`)
		if err != nil {
			return fmt.Errorf("add agentic column: %w", err)
		}
	}

	// Migration: add model column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'model'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check model column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN model TEXT`)
		if err != nil {
			return fmt.Errorf("add model column: %w", err)
		}
	}

	// Migration: add branch column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'branch'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check branch column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN branch TEXT`)
		if err != nil {
			return fmt.Errorf("add branch column: %w", err)
		}
	}

	// Migration: add session_id column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'session_id'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check session_id column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN session_id TEXT`)
		if err != nil {
			return fmt.Errorf("add session_id column: %w", err)
		}
	}

	// Migration: add output_prefix column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'output_prefix'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check output_prefix column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN output_prefix TEXT`)
		if err != nil {
			return fmt.Errorf("add output_prefix column: %w", err)
		}
	}

	// Migration: update CHECK constraint to include 'canceled' status
	// SQLite requires table recreation to modify CHECK constraints
	var tableSql string
	err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_jobs'`).Scan(&tableSql)
	if err != nil {
		return fmt.Errorf("check review_jobs schema: %w", err)
	}
	// Only migrate if the old constraint exists (doesn't include 'canceled')
	if strings.Contains(tableSql, "CHECK(status IN ('queued','running','done','failed'))") {
		// Use a dedicated connection for the entire migration since PRAGMA is connection-scoped
		// This ensures FK disable/enable and the transaction all use the same connection
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("get connection for migration: %w", err)
		}
		defer conn.Close()

		// Disable foreign keys for table rebuild (reviews references review_jobs)
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return fmt.Errorf("disable foreign keys: %w", err)
		}
		// Ensure FKs are re-enabled even if we return early due to error
		// This prevents returning a connection to the pool with FKs disabled
		defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

		// Recreate table with updated constraint in a transaction for safety
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration transaction: %w", err)
		}
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				return
			}
		}()

		_, err = tx.Exec(`
			CREATE TABLE review_jobs_new (
				id INTEGER PRIMARY KEY,
				repo_id INTEGER NOT NULL REFERENCES repos(id),
				commit_id INTEGER REFERENCES commits(id),
				git_ref TEXT NOT NULL,
				branch TEXT,
				session_id TEXT,
				agent TEXT NOT NULL DEFAULT 'codex',
				model TEXT,
				provider TEXT,
				requested_model TEXT,
				requested_provider TEXT,
				reasoning TEXT NOT NULL DEFAULT 'thorough',
				status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed','canceled','applied','rebased')) DEFAULT 'queued',
				enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
				started_at TEXT,
				finished_at TEXT,
				worker_id TEXT,
				error TEXT,
				prompt TEXT,
				retry_count INTEGER NOT NULL DEFAULT 0,
				diff_content TEXT,
				dirty_files TEXT,
				agentic INTEGER NOT NULL DEFAULT 0,
				output_prefix TEXT
			)
		`)
		if err != nil {
			return fmt.Errorf("create new review_jobs table: %w", err)
		}

		// Check which optional columns exist in source table
		var hasDiffContent, hasDirtyFiles, hasReasoning, hasAgentic, hasModel, hasProvider, hasRequestedModel, hasRequestedProvider, hasBranch, hasSessionID, hasOutputPrefix bool
		checkRows, checkErr := tx.Query(`SELECT name FROM pragma_table_info('review_jobs') WHERE name IN ('diff_content', 'dirty_files', 'reasoning', 'agentic', 'model', 'provider', 'requested_model', 'requested_provider', 'branch', 'session_id', 'output_prefix')`)
		if checkErr == nil {
			for checkRows.Next() {
				var colName string
				_ = checkRows.Scan(&colName)
				switch colName {
				case "diff_content":
					hasDiffContent = true
				case "dirty_files":
					hasDirtyFiles = true
				case "reasoning":
					hasReasoning = true
				case "agentic":
					hasAgentic = true
				case "model":
					hasModel = true
				case "provider":
					hasProvider = true
				case "requested_model":
					hasRequestedModel = true
				case "requested_provider":
					hasRequestedProvider = true
				case "branch":
					hasBranch = true
				case "session_id":
					hasSessionID = true
				case "output_prefix":
					hasOutputPrefix = true
				}
			}
			checkRows.Close()
		}

		// Build INSERT statement based on which columns exist
		// We need to handle all combinations of optional columns
		var insertSQL string
		// Base columns that always exist
		baseCols := []string{"id", "repo_id", "commit_id", "git_ref"}
		if hasBranch {
			baseCols = append(baseCols, "branch")
		}
		if hasSessionID {
			baseCols = append(baseCols, "session_id")
		}
		baseCols = append(baseCols, "agent")
		if hasModel {
			baseCols = append(baseCols, "model")
		}
		if hasProvider {
			baseCols = append(baseCols, "provider")
		}
		if hasRequestedModel {
			baseCols = append(baseCols, "requested_model")
		}
		if hasRequestedProvider {
			baseCols = append(baseCols, "requested_provider")
		}
		if hasReasoning {
			baseCols = append(baseCols, "reasoning")
		}
		baseCols = append(baseCols, "status", "enqueued_at", "started_at", "finished_at", "worker_id", "error", "prompt", "retry_count")
		if hasDiffContent {
			baseCols = append(baseCols, "diff_content")
		}
		if hasDirtyFiles {
			baseCols = append(baseCols, "dirty_files")
		}
		if hasAgentic {
			baseCols = append(baseCols, "agentic")
		}
		if hasOutputPrefix {
			baseCols = append(baseCols, "output_prefix")
		}
		cols := strings.Join(baseCols, ", ")
		insertSQL = fmt.Sprintf(`INSERT INTO review_jobs_new (%s) SELECT %s FROM review_jobs`, cols, cols)
		_, err = tx.Exec(insertSQL)
		if err != nil {
			return fmt.Errorf("copy review_jobs data: %w", err)
		}

		_, err = tx.Exec(`DROP TABLE review_jobs`)
		if err != nil {
			return fmt.Errorf("drop old review_jobs table: %w", err)
		}

		_, err = tx.Exec(`ALTER TABLE review_jobs_new RENAME TO review_jobs`)
		if err != nil {
			return fmt.Errorf("rename review_jobs table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE INDEX IF NOT EXISTS idx_review_jobs_status ON review_jobs(status);
			CREATE INDEX IF NOT EXISTS idx_review_jobs_repo ON review_jobs(repo_id);
			CREATE INDEX IF NOT EXISTS idx_review_jobs_git_ref ON review_jobs(git_ref)
		`)
		if err != nil {
			return fmt.Errorf("recreate review_jobs indexes: %w", err)
		}

		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit migration transaction: %w", err)
		}

		// Re-enable foreign keys explicitly before checking (defer will also run, harmlessly)
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			return fmt.Errorf("re-enable foreign keys: %w", err)
		}

		// Verify foreign key integrity after migration
		// Use PRAGMA foreign_key_check (not table-valued function) for older SQLite compatibility
		rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return fmt.Errorf("foreign key check failed: %w", err)
		}
		defer rows.Close()
		if rows.Next() {
			return fmt.Errorf("foreign key violations detected after migration")
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("foreign key check iteration failed: %w", err)
		}
	}

	// Migration: add index on branch column if missing
	// This must be after the table recreation migration above (which drops and recreates the table)
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_review_jobs_branch ON review_jobs(branch)`)
	if err != nil {
		return fmt.Errorf("create branch index: %w", err)
	}

	// Migration: update CHECK constraint to include 'applied' and 'rebased' statuses
	// Re-read the table SQL since the previous migration may have rebuilt it
	err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_jobs'`).Scan(&tableSql)
	if err != nil {
		return fmt.Errorf("check review_jobs schema for applied/rebased: %w", err)
	}
	if !strings.Contains(tableSql, "'applied'") {
		if err := db.migrateJobStatusConstraint(); err != nil {
			return fmt.Errorf("migrate job status constraint: %w", err)
		}
	}

	// Migration: make commit_id nullable in responses table (for job-based responses)
	// Check if commit_id is NOT NULL by examining the schema
	var responsesSql string
	err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='responses'`).Scan(&responsesSql)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check responses schema: %w", err)
	}
	// Only migrate if commit_id is NOT NULL (old schema)
	if strings.Contains(responsesSql, "commit_id INTEGER NOT NULL") {
		// Rebuild table to make commit_id nullable
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("get connection for responses migration: %w", err)
		}
		defer conn.Close()

		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return fmt.Errorf("disable foreign keys: %w", err)
		}
		defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin responses migration transaction: %w", err)
		}
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				return
			}
		}()

		// Check if job_id column already exists in old table
		var hasJobID bool
		checkRows, _ := tx.Query(`SELECT COUNT(*) FROM pragma_table_info('responses') WHERE name = 'job_id'`)
		if checkRows != nil {
			if checkRows.Next() {
				var cnt int
				_ = checkRows.Scan(&cnt)
				hasJobID = cnt > 0
			}
			checkRows.Close()
		}

		_, err = tx.Exec(`
			CREATE TABLE responses_new (
				id INTEGER PRIMARY KEY,
				commit_id INTEGER REFERENCES commits(id),
				job_id INTEGER REFERENCES review_jobs(id),
				responder TEXT NOT NULL,
				response TEXT NOT NULL,
				created_at TEXT NOT NULL DEFAULT (datetime('now'))
			)
		`)
		if err != nil {
			return fmt.Errorf("create new responses table: %w", err)
		}

		if hasJobID {
			_, err = tx.Exec(`
				INSERT INTO responses_new (id, commit_id, job_id, responder, response, created_at)
				SELECT id, commit_id, job_id, responder, response, created_at FROM responses
			`)
		} else {
			_, err = tx.Exec(`
				INSERT INTO responses_new (id, commit_id, responder, response, created_at)
				SELECT id, commit_id, responder, response, created_at FROM responses
			`)
		}
		if err != nil {
			return fmt.Errorf("copy responses data: %w", err)
		}

		_, err = tx.Exec(`DROP TABLE responses`)
		if err != nil {
			return fmt.Errorf("drop old responses table: %w", err)
		}

		_, err = tx.Exec(`ALTER TABLE responses_new RENAME TO responses`)
		if err != nil {
			return fmt.Errorf("rename responses table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_responses_job_id ON responses(job_id)`)
		if err != nil {
			return fmt.Errorf("create idx_responses_job_id: %w", err)
		}

		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit responses migration: %w", err)
		}

		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			return fmt.Errorf("re-enable foreign keys: %w", err)
		}
	} else {
		// Table already has nullable commit_id, just add job_id if missing
		err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('responses') WHERE name = 'job_id'`).Scan(&count)
		if err != nil {
			return fmt.Errorf("check job_id column in responses: %w", err)
		}
		if count == 0 {
			_, err = db.Exec(`ALTER TABLE responses ADD COLUMN job_id INTEGER REFERENCES review_jobs(id)`)
			if err != nil {
				return fmt.Errorf("add job_id column to responses: %w", err)
			}
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_responses_job_id ON responses(job_id)`)
			if err != nil {
				return fmt.Errorf("create idx_responses_job_id: %w", err)
			}
		}
	}

	// Migration: add job_type column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'job_type'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check job_type column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN job_type TEXT NOT NULL DEFAULT 'review'`)
		if err != nil {
			return fmt.Errorf("add job_type column: %w", err)
		}
		// Backfill job_type for existing rows
		_, err = db.Exec(`UPDATE review_jobs SET job_type = 'dirty' WHERE (git_ref = 'dirty' OR diff_content IS NOT NULL) AND job_type = 'review'`)
		if err != nil {
			return fmt.Errorf("backfill job_type dirty: %w", err)
		}
		_, err = db.Exec(`UPDATE review_jobs SET job_type = 'range' WHERE git_ref LIKE '%..%' AND commit_id IS NULL AND job_type = 'review'`)
		if err != nil {
			return fmt.Errorf("backfill job_type range: %w", err)
		}
		_, err = db.Exec(`UPDATE review_jobs SET job_type = 'task' WHERE commit_id IS NULL AND diff_content IS NULL AND git_ref != 'dirty' AND git_ref NOT LIKE '%..%' AND git_ref != '' AND job_type = 'review'`)
		if err != nil {
			return fmt.Errorf("backfill job_type task: %w", err)
		}
	}

	// Migration: add review_type column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'review_type'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check review_type column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN review_type TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add review_type column: %w", err)
		}
	}

	// Migration: add patch_id column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'patch_id'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check patch_id column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN patch_id TEXT`)
		if err != nil {
			return fmt.Errorf("add patch_id column: %w", err)
		}
		_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_review_jobs_patch_id ON review_jobs(patch_id)`)
		if err != nil {
			return fmt.Errorf("create idx_review_jobs_patch_id: %w", err)
		}
	}

	// Migration: rename addressed index to closed
	_, _ = db.Exec(`DROP INDEX IF EXISTS idx_reviews_addressed`)
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_reviews_closed ON reviews(closed)`)
	if err != nil {
		return fmt.Errorf("create idx_reviews_closed: %w", err)
	}

	// Migration: add parent_job_id column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'parent_job_id'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check parent_job_id column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN parent_job_id INTEGER`)
		if err != nil {
			return fmt.Errorf("add parent_job_id column: %w", err)
		}
	}

	// Migration: add patch column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'patch'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check patch column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN patch TEXT`)
		if err != nil {
			return fmt.Errorf("add patch column: %w", err)
		}
	}

	// Migration: add verdict_bool column to reviews if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('reviews') WHERE name = 'verdict_bool'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check verdict_bool column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE reviews ADD COLUMN verdict_bool INTEGER`)
		if err != nil {
			return fmt.Errorf("add verdict_bool column: %w", err)
		}
		_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_reviews_verdict_bool ON reviews(verdict_bool)`)
		if err != nil {
			return fmt.Errorf("create idx_reviews_verdict_bool: %w", err)
		}
	}

	// Migration: add provider column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'provider'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check provider column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN provider TEXT`)
		if err != nil {
			return fmt.Errorf("add provider column: %w", err)
		}
	}

	// Migration: add requested_model column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'requested_model'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check requested_model column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN requested_model TEXT`)
		if err != nil {
			return fmt.Errorf("add requested_model column: %w", err)
		}
	}

	// Migration: add requested_provider column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'requested_provider'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check requested_provider column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN requested_provider TEXT`)
		if err != nil {
			return fmt.Errorf("add requested_provider column: %w", err)
		}
	}

	// Migration: add token_usage column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'token_usage'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check token_usage column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN token_usage TEXT`)
		if err != nil {
			return fmt.Errorf("add token_usage column: %w", err)
		}
	}

	// Migration: add worktree_path column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'worktree_path'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check worktree_path column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN worktree_path TEXT DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add worktree_path column: %w", err)
		}
	}

	// Migration: add prompt_prebuilt column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'prompt_prebuilt'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check prompt_prebuilt column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN prompt_prebuilt INTEGER NOT NULL DEFAULT 0`)
		if err != nil {
			return fmt.Errorf("add prompt_prebuilt column: %w", err)
		}
	}

	// Migration: add command_line column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'command_line'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check command_line column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN command_line TEXT`)
		if err != nil {
			return fmt.Errorf("add command_line column: %w", err)
		}
	}

	// Migration: add agent_invoked column to review_jobs if missing.
	// agent_invoked is the authoritative "an agent actually ran this attempt"
	// signal for cost eligibility: the worker sets it immediately before the agent
	// call (after all pre-agent gates) and it syncs across machines. Rows that
	// predate the column keep the default 0 and are not backfilled — a historical
	// run that recorded token usage is still counted via the token_usage fallback
	// in costEligible, and command_line is too unreliable a signal to seed from
	// (it is written before some pre-agent failures, so it would re-introduce the
	// over-count this marker exists to avoid).
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'agent_invoked'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check agent_invoked column: %w", err)
	}
	if count == 0 {
		if _, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN agent_invoked INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add agent_invoked column: %w", err)
		}
	}

	// Migration: add min_severity column to review_jobs if missing
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'min_severity'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check min_severity column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN min_severity TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add min_severity column: %w", err)
		}
	}

	// Migration: add backup_agent column to review_jobs if missing.
	// Job-level failover override (F7): when set, the worker prefers this over
	// the workflow-resolved backup agent for this job's failover.
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'backup_agent'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check backup_agent column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN backup_agent TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add backup_agent column: %w", err)
		}
	}

	// Migration: add backup_model column to review_jobs if missing (F7).
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'backup_model'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check backup_model column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN backup_model TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add backup_model column: %w", err)
		}
	}

	// Migration: add retry_not_before column to review_jobs if missing.
	// ClaimJob skips jobs whose retry_not_before is in the future so the
	// retry backoff is enforced at the queue level, regardless of which
	// worker happened to fail the prior attempt.
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'retry_not_before'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check retry_not_before column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN retry_not_before TIMESTAMP`)
		if err != nil {
			return fmt.Errorf("add retry_not_before column: %w", err)
		}
	}

	// Migration: add panel columns to review_jobs if missing.
	// Subagent review panels: a panel run is N member jobs + 1 synthesis
	// job sharing panel_run_uuid. Six columns sync as ordinary job
	// columns (the min_severity template); claim_blocked is local-only —
	// it gates ClaimJob while a synthesis job waits on its members and
	// never needs to cross machines.
	for _, col := range []struct {
		name string
		def  string
	}{
		{"panel_run_uuid", "TEXT"},
		{"panel_role", "TEXT"},
		{"panel_name", "TEXT"},
		{"panel_member_name", "TEXT"},
		{"panel_member_index", "INTEGER"},
		{"panel_member_config_json", "TEXT"},
		{"claim_blocked", "INTEGER NOT NULL DEFAULT 0"},
	} {
		err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = ?`, col.name).Scan(&count)
		if err != nil {
			return fmt.Errorf("check %s column: %w", col.name, err)
		}
		if count == 0 {
			_, err = db.Exec(fmt.Sprintf(`ALTER TABLE review_jobs ADD COLUMN %s %s`, col.name, col.def))
			if err != nil {
				return fmt.Errorf("add %s column: %w", col.name, err)
			}
		}
	}

	// Migration: add ci_base_branch column to review_jobs if missing.
	// CI reviews record the PR base (target) branch here for event/hook
	// branch matching only. It is deliberately separate from branch, which
	// stays empty for CI jobs so branch-scoped local flows (fix/refine
	// discovery, fix-ref selection, session reuse) never treat a CI review
	// as local work on the base branch.
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'ci_base_branch'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check ci_base_branch column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE review_jobs ADD COLUMN ci_base_branch TEXT`)
		if err != nil {
			return fmt.Errorf("add ci_base_branch column: %w", err)
		}
	}

	// The panel composite index is created later, after
	// migrateReviewJobsConstraintsForAutoDesign: that migration rebuilds
	// review_jobs via DROP+RENAME on legacy DBs, which would drop an index
	// created here. The panel COLUMNS are added above (before the rebuild),
	// so the rebuild preserves them through the live schema; only the index
	// must wait until after the rebuild.

	// Migration: add retired_at column to ci_pr_panels if missing. Retired rows
	// are old active CI panel mappings canceled during supersede/throttle
	// cleanup; they remain for throttle memory but are not postable.
	err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ci_pr_panels') WHERE name = 'retired_at'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check retired_at column: %w", err)
	}
	if count == 0 {
		_, err = db.Exec(`ALTER TABLE ci_pr_panels ADD COLUMN retired_at TIMESTAMP`)
		if err != nil {
			return fmt.Errorf("add retired_at column: %w", err)
		}
	}

	// Run sync-related migrations
	if err := db.migrateSyncColumns(); err != nil {
		return err
	}

	// Auto design review support: extends status CHECK constraint,
	// adds skip_reason column. (job_type has no CHECK constraint;
	// 'classify' is accepted as-is.)
	if err := db.migrateReviewJobsConstraintsForAutoDesign(); err != nil {
		return fmt.Errorf("migrate review_jobs constraints for auto design: %w", err)
	}

	// Panel composite index — created AFTER the rebuild above so a legacy-DB
	// table rebuild (DROP+RENAME) cannot drop it. Used to fetch a run's
	// members in order and locate its synthesis row.
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_review_jobs_panel ON review_jobs(panel_run_uuid, panel_role, panel_member_index)`); err != nil {
		return fmt.Errorf("create idx_review_jobs_panel: %w", err)
	}

	// Partial index for the safety sweep: locate stuck synthesis rows (still
	// claim_blocked) cheaply. claim_blocked is local-only, so this index is
	// SQLite-only. Created here, after the legacy rebuild, for the same reason
	// as idx_review_jobs_panel above.
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_review_jobs_synth_blocked
		ON review_jobs(panel_run_uuid)
		WHERE panel_role = 'synthesis' AND claim_blocked = 1`); err != nil {
		return fmt.Errorf("create idx_review_jobs_synth_blocked: %w", err)
	}

	// Retire the old CI batch subsystem (F14): cancel any in-flight
	// batch jobs, then drop ci_pr_batch_jobs and ci_pr_batches. Runs
	// every Open() and is a no-op once the tables are gone. Placed last
	// so it observes the final review_jobs state after the rebuilds above.
	if err := db.drainAndDropOldCIBatchTables(); err != nil {
		return err
	}

	return nil
}

// drainAndDropOldCIBatchTables retires the legacy CI batch tracking
// subsystem (finding F14). It is a one-way migration: ci_pr_batches and
// ci_pr_batch_jobs are dropped permanently in favor of ci_pr_panels.
//
// Before dropping, any batch jobs still queued or running are marked
// canceled so they don't keep occupying a worker for a workflow that no
// longer posts results. Worker-side cancellation of an already-running
// job is best-effort: the migration only flips the DB status; the worker
// observes that flip on its next cancellation check and stops.
//
// The method guards on table existence, so once the tables are gone it is
// a clean no-op — safe to call on every Open().
func (db *DB) drainAndDropOldCIBatchTables() error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("drain old CI batch tables: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	hasBatchJobs, err := sqliteTableExistsTx(tx, "ci_pr_batch_jobs")
	if err != nil {
		return fmt.Errorf("drain old CI batch tables: %w", err)
	}
	if hasBatchJobs {
		if _, err := tx.Exec(`UPDATE review_jobs SET status='canceled', error='superseded by panel migration'
		 WHERE status IN ('queued','running') AND id IN (SELECT job_id FROM ci_pr_batch_jobs)`); err != nil {
			return fmt.Errorf("drain old CI batch tables: %w", err)
		}
	}

	stmts := []string{
		`DROP TABLE IF EXISTS ci_pr_batch_jobs`,
		`DROP TABLE IF EXISTS ci_pr_batches`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("drain old CI batch tables: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("drain old CI batch tables: %w", err)
	}
	return nil
}

func sqliteTableExistsTx(tx *sql.Tx, name string) (bool, error) {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// hasUniqueIndexOnShaOnly checks if commits table has a unique constraint on just sha
// (not the composite repo_id, sha constraint). Uses PRAGMA index_list/index_info for robustness.
func (db *DB) hasUniqueIndexOnShaOnly() (bool, error) {
	// Get all indexes on commits table
	rows, err := db.Query(`PRAGMA index_list('commits')`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return false, err
		}
		if unique == 0 {
			continue // Not a unique index
		}
		// Check if this unique index is on sha only
		// PRAGMA doesn't support parameterized queries, so we escape quotes in the name
		safeName := strings.ReplaceAll(name, "'", "''")
		infoRows, err := db.Query(fmt.Sprintf(`PRAGMA index_info('%s')`, safeName))
		if err != nil {
			return false, err
		}
		var cols []string
		for infoRows.Next() {
			var seqno, cid int
			var colName string
			if err := infoRows.Scan(&seqno, &cid, &colName); err != nil {
				infoRows.Close()
				return false, err
			}
			cols = append(cols, colName)
		}
		if err := infoRows.Err(); err != nil {
			infoRows.Close()
			return false, err
		}
		infoRows.Close()
		// If this unique index only has sha, we need to migrate
		if len(cols) == 1 && cols[0] == "sha" {
			return true, nil
		}
	}
	return false, rows.Err()
}

// migrateJobStatusConstraint rebuilds the review_jobs table to update the
// CHECK constraint to include 'applied' and 'rebased' statuses.
func (db *DB) migrateJobStatusConstraint() error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			return
		}
	}()

	// Clean up temp table from any prior failed migration attempt
	if _, err := tx.Exec(`DROP TABLE IF EXISTS review_jobs_new`); err != nil {
		return fmt.Errorf("cleanup stale temp table: %w", err)
	}

	// Read existing columns dynamically
	rows, err := tx.Query(`SELECT name FROM pragma_table_info('review_jobs')`)
	if err != nil {
		return fmt.Errorf("read columns: %w", err)
	}
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		cols = append(cols, name)
	}
	rows.Close()

	// Read the current CREATE TABLE SQL and replace the old constraint
	var origSQL string
	if err := tx.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_jobs'`,
	).Scan(&origSQL); err != nil {
		return err
	}

	// Replace old constraint with new one including applied and rebased
	newSQL := strings.Replace(origSQL,
		"CHECK(status IN ('queued','running','done','failed','canceled'))",
		"CHECK(status IN ('queued','running','done','failed','canceled','applied','rebased'))",
		1)

	// Rename to temp table. After ALTER TABLE ... RENAME, SQLite
	// stores the name quoted, so handle both forms.
	replaced := false
	for _, pattern := range []string{
		`CREATE TABLE "review_jobs"`,
		`CREATE TABLE review_jobs`,
	} {
		if strings.Contains(newSQL, pattern) {
			newSQL = strings.Replace(
				newSQL, pattern,
				`CREATE TABLE review_jobs_new`, 1,
			)
			replaced = true
			break
		}
	}
	if !replaced {
		return fmt.Errorf(
			"cannot find CREATE TABLE statement in schema: %s",
			origSQL[:min(len(origSQL), 80)],
		)
	}

	if _, err := tx.Exec(newSQL); err != nil {
		return fmt.Errorf("create new table: %w", err)
	}

	colList := strings.Join(cols, ", ")
	copySQL := fmt.Sprintf(
		`INSERT INTO review_jobs_new (%s) SELECT %s FROM review_jobs`,
		colList, colList,
	)
	if _, err := tx.Exec(copySQL); err != nil {
		return fmt.Errorf("copy data: %w", err)
	}

	if _, err := tx.Exec(`DROP TABLE review_jobs`); err != nil {
		return fmt.Errorf("drop old table: %w", err)
	}

	if _, err := tx.Exec(
		`ALTER TABLE review_jobs_new RENAME TO review_jobs`,
	); err != nil {
		return fmt.Errorf("rename table: %w", err)
	}

	// Recreate indexes
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_review_jobs_status ON review_jobs(status)`,
		`CREATE INDEX IF NOT EXISTS idx_review_jobs_repo ON review_jobs(repo_id)`,
		`CREATE INDEX IF NOT EXISTS idx_review_jobs_git_ref ON review_jobs(git_ref)`,
		`CREATE INDEX IF NOT EXISTS idx_review_jobs_branch ON review_jobs(branch)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_uuid ON review_jobs(uuid)`,
	} {
		if _, err := tx.Exec(idx); err != nil {
			return fmt.Errorf("recreate index: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Log pre-existing FK violations but don't fail — this migration
	// only changes a CHECK constraint and copies data 1:1, so any
	// violations existed before the migration ran.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("re-enable foreign keys: %w", err)
	}
	checkRows, err := conn.QueryContext(
		ctx, `PRAGMA foreign_key_check('review_jobs')`,
	)
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer checkRows.Close()
	var violations int
	for checkRows.Next() {
		violations++
	}
	if violations > 0 {
		log.Printf(
			"warning: %d pre-existing foreign key violations in review_jobs (not caused by migration)",
			violations,
		)
	}
	return checkRows.Err()
}

// migrateSyncColumns adds columns needed for PostgreSQL sync functionality.
// These migrations are idempotent - they check if columns exist before adding.
func (db *DB) migrateSyncColumns() error {
	// Helper to check if a column exists
	hasColumn := func(table, column string) (bool, error) {
		var count int
		err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count)
		return count > 0, err
	}

	// Migration: Add sync columns to review_jobs
	for _, col := range []struct {
		name string
		def  string
	}{
		{"uuid", "TEXT"},
		{"source_machine_id", "TEXT"},
		{"updated_at", "TEXT"},
		{"synced_at", "TEXT"},
	} {
		has, err := hasColumn("review_jobs", col.name)
		if err != nil {
			return fmt.Errorf("check %s column in review_jobs: %w", col.name, err)
		}
		if !has {
			_, err = db.Exec(fmt.Sprintf(`ALTER TABLE review_jobs ADD COLUMN %s %s`, col.name, col.def))
			if err != nil {
				return fmt.Errorf("add %s column to review_jobs: %w", col.name, err)
			}
		}
	}

	// Backfill UUIDs for review_jobs
	_, err := db.Exec(`UPDATE review_jobs SET uuid = ` + sqliteUUIDExpr + ` WHERE uuid IS NULL`)
	if err != nil {
		return fmt.Errorf("backfill review_jobs uuid: %w", err)
	}

	// Backfill updated_at for review_jobs (use finished_at or enqueued_at)
	_, err = db.Exec(`UPDATE review_jobs SET updated_at = COALESCE(finished_at, enqueued_at) WHERE updated_at IS NULL`)
	if err != nil {
		return fmt.Errorf("backfill review_jobs updated_at: %w", err)
	}

	// Create unique index on review_jobs.uuid
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_uuid ON review_jobs(uuid)`)
	if err != nil {
		return fmt.Errorf("create idx_review_jobs_uuid: %w", err)
	}

	// Migration: Add sync columns to reviews
	for _, col := range []struct {
		name string
		def  string
	}{
		{"uuid", "TEXT"},
		{"updated_at", "TEXT"},
		{"updated_by_machine_id", "TEXT"},
		{"synced_at", "TEXT"},
	} {
		has, err := hasColumn("reviews", col.name)
		if err != nil {
			return fmt.Errorf("check %s column in reviews: %w", col.name, err)
		}
		if !has {
			_, err = db.Exec(fmt.Sprintf(`ALTER TABLE reviews ADD COLUMN %s %s`, col.name, col.def))
			if err != nil {
				return fmt.Errorf("add %s column to reviews: %w", col.name, err)
			}
		}
	}

	// Backfill UUIDs for reviews
	_, err = db.Exec(`UPDATE reviews SET uuid = ` + sqliteUUIDExpr + ` WHERE uuid IS NULL`)
	if err != nil {
		return fmt.Errorf("backfill reviews uuid: %w", err)
	}

	// Backfill updated_at for reviews (use created_at)
	_, err = db.Exec(`UPDATE reviews SET updated_at = created_at WHERE updated_at IS NULL`)
	if err != nil {
		return fmt.Errorf("backfill reviews updated_at: %w", err)
	}

	// Create unique index on reviews.uuid
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_reviews_uuid ON reviews(uuid)`)
	if err != nil {
		return fmt.Errorf("create idx_reviews_uuid: %w", err)
	}

	// Migration: Add sync columns to responses
	for _, col := range []struct {
		name string
		def  string
	}{
		{"uuid", "TEXT"},
		{"source_machine_id", "TEXT"},
		{"synced_at", "TEXT"},
	} {
		has, err := hasColumn("responses", col.name)
		if err != nil {
			return fmt.Errorf("check %s column in responses: %w", col.name, err)
		}
		if !has {
			_, err = db.Exec(fmt.Sprintf(`ALTER TABLE responses ADD COLUMN %s %s`, col.name, col.def))
			if err != nil {
				return fmt.Errorf("add %s column to responses: %w", col.name, err)
			}
		}
	}

	// Backfill UUIDs for responses
	_, err = db.Exec(`UPDATE responses SET uuid = ` + sqliteUUIDExpr + ` WHERE uuid IS NULL`)
	if err != nil {
		return fmt.Errorf("backfill responses uuid: %w", err)
	}

	// Create unique index on responses.uuid
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_responses_uuid ON responses(uuid)`)
	if err != nil {
		return fmt.Errorf("create idx_responses_uuid: %w", err)
	}

	// Create index for GetCommentsToSync query pattern (source_machine_id + synced_at)
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_responses_sync ON responses(source_machine_id, synced_at)`)
	if err != nil {
		return fmt.Errorf("create idx_responses_sync: %w", err)
	}

	// Migration: Add identity column to repos
	has, err := hasColumn("repos", "identity")
	if err != nil {
		return fmt.Errorf("check identity column in repos: %w", err)
	}
	if !has {
		_, err = db.Exec(`ALTER TABLE repos ADD COLUMN identity TEXT`)
		if err != nil {
			return fmt.Errorf("add identity column to repos: %w", err)
		}
	}

	// Normalize empty strings to NULL (treat empty as "unset")
	_, err = db.Exec(`UPDATE repos SET identity = NULL WHERE identity = ''`)
	if err != nil {
		return fmt.Errorf("normalize empty identities to NULL: %w", err)
	}

	// Create non-unique index on repos.identity for query performance.
	// Note: identity is NOT unique because multiple local clones of the same repo
	// (e.g., ~/project-1 and ~/project-2 both cloned from the same remote)
	// should be allowed and will share the same identity.
	// See: https://github.com/roborev-dev/roborev/issues/131

	// Migration: If an old UNIQUE index exists, drop it first
	var indexSQL sql.NullString
	err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_repos_identity'`).Scan(&indexSQL)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check existing idx_repos_identity: %w", err)
	}
	if indexSQL.Valid && strings.Contains(strings.ToUpper(indexSQL.String), "UNIQUE") {
		// Drop the old unique index
		_, err = db.Exec(`DROP INDEX idx_repos_identity`)
		if err != nil {
			return fmt.Errorf("drop old unique idx_repos_identity: %w", err)
		}
	}

	// Create non-unique index (or recreate after dropping unique)
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_repos_identity ON repos(identity) WHERE identity IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("create idx_repos_identity: %w", err)
	}

	// Migration: Create sync_state table for tracking sync status
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sync_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("create sync_state table: %w", err)
	}

	// Migration: Create daemon_state table for durable local daemon controls.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS daemon_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`)
	if err != nil {
		return fmt.Errorf("create daemon_state table: %w", err)
	}

	// Migration: Align commits uniqueness to UNIQUE(repo_id, sha) instead of just UNIQUE(sha)
	// Check if we need to migrate by checking for a unique index on just sha (not repo_id, sha)
	needsCommitsMigration, err := db.hasUniqueIndexOnShaOnly()
	if err != nil {
		return fmt.Errorf("check commits unique constraint: %w", err)
	}

	if needsCommitsMigration {
		// Need to rebuild table. Use a dedicated connection since PRAGMA is connection-scoped.
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("get connection for commits migration: %w", err)
		}
		defer conn.Close()

		// Disable foreign keys OUTSIDE transaction (SQLite ignores inside tx)
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return fmt.Errorf("disable foreign keys for commits: %w", err)
		}
		defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

		// Run rebuild in a transaction for atomicity
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin commits migration transaction: %w", err)
		}
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				return
			}
		}()

		// Step 1: Create backup
		_, err = tx.Exec(`CREATE TABLE commits_backup AS SELECT * FROM commits`)
		if err != nil {
			return fmt.Errorf("create commits_backup: %w", err)
		}

		// Step 2: Drop original
		_, err = tx.Exec(`DROP TABLE commits`)
		if err != nil {
			return fmt.Errorf("drop commits: %w", err)
		}

		// Step 3: Create new table with UNIQUE(repo_id, sha)
		_, err = tx.Exec(`
			CREATE TABLE commits (
				id INTEGER PRIMARY KEY,
				repo_id INTEGER NOT NULL REFERENCES repos(id),
				sha TEXT NOT NULL,
				author TEXT NOT NULL,
				subject TEXT NOT NULL,
				timestamp TEXT NOT NULL,
				created_at TEXT NOT NULL DEFAULT (datetime('now')),
				UNIQUE(repo_id, sha)
			)
		`)
		if err != nil {
			return fmt.Errorf("create new commits table: %w", err)
		}

		// Step 4: Copy data from backup
		_, err = tx.Exec(`INSERT INTO commits SELECT * FROM commits_backup`)
		if err != nil {
			return fmt.Errorf("copy commits data: %w", err)
		}

		// Step 5: Drop backup
		_, err = tx.Exec(`DROP TABLE commits_backup`)
		if err != nil {
			return fmt.Errorf("drop commits_backup: %w", err)
		}

		// Step 6: Recreate index
		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_commits_sha ON commits(sha)`)
		if err != nil {
			return fmt.Errorf("recreate idx_commits_sha: %w", err)
		}

		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit commits migration: %w", err)
		}

		// Re-enable foreign keys and verify
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			return fmt.Errorf("re-enable foreign keys: %w", err)
		}

		// Verify foreign key integrity
		rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return fmt.Errorf("foreign key check failed: %w", err)
		}
		defer rows.Close()
		if rows.Next() {
			return fmt.Errorf("foreign key violations detected after commits migration")
		}
	}

	if err := db.migrateRepoRootPathNormalization(); err != nil {
		return err
	}

	return nil
}

type repoRootPathMigrationRow struct {
	id         int64
	rootPath   string
	normalized string
}

func (db *DB) migrateRepoRootPathNormalization() error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin repo root_path normalization: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.Query(`SELECT id, root_path FROM repos ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query repo root_paths: %w", err)
	}
	groups := map[string][]repoRootPathMigrationRow{}
	for rows.Next() {
		var r repoRootPathMigrationRow
		if err := rows.Scan(&r.id, &r.rootPath); err != nil {
			rows.Close()
			return fmt.Errorf("scan repo root_path: %w", err)
		}
		r.normalized = normalizeStoredRepoPath(r.rootPath)
		groups[r.normalized] = append(groups[r.normalized], r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate repo root_paths: %w", err)
	}
	rows.Close()

	for normalized, group := range groups {
		if len(group) == 1 {
			if group[0].rootPath != normalized {
				if _, err := tx.Exec(`UPDATE repos SET root_path = ? WHERE id = ?`, normalized, group[0].id); err != nil {
					return fmt.Errorf("normalize repo root_path %q: %w", group[0].rootPath, err)
				}
			}
			continue
		}

		target := chooseRepoRootPathMigrationTarget(normalized, group)
		if target.rootPath != normalized {
			if _, err := tx.Exec(`UPDATE repos SET root_path = ? WHERE id = ?`, normalized, target.id); err != nil {
				return fmt.Errorf("normalize target repo root_path %q: %w", target.rootPath, err)
			}
		}
		for _, source := range group {
			if source.id == target.id {
				continue
			}
			if err := mergeRepoRootPathConflict(tx, source.id, target.id); err != nil {
				return fmt.Errorf("merge repo root_path conflict %d into %d: %w", source.id, target.id, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit repo root_path normalization: %w", err)
	}
	committed = true
	return nil
}

func chooseRepoRootPathMigrationTarget(
	normalized string,
	rows []repoRootPathMigrationRow,
) repoRootPathMigrationRow {
	target := rows[0]
	for _, r := range rows {
		if r.rootPath == normalized {
			return r
		}
		if r.id < target.id {
			target = r
		}
	}
	return target
}

func mergeRepoRootPathConflict(tx *sql.Tx, sourceRepoID, targetRepoID int64) error {
	if _, err := tx.Exec(`
		UPDATE repos
		SET identity = (SELECT identity FROM repos WHERE id = ?)
		WHERE id = ?
		  AND (identity IS NULL OR identity = '')
		  AND COALESCE((SELECT identity FROM repos WHERE id = ?), '') != ''
	`, sourceRepoID, targetRepoID, sourceRepoID); err != nil {
		return fmt.Errorf("copy repo identity: %w", err)
	}

	rows, err := tx.Query(`
		SELECT sc.id, tc.id
		FROM commits sc
		JOIN commits tc ON tc.repo_id = ? AND tc.sha = sc.sha
		WHERE sc.repo_id = ?
	`, targetRepoID, sourceRepoID)
	if err != nil {
		return fmt.Errorf("query duplicate commits: %w", err)
	}
	type commitPair struct {
		sourceID int64
		targetID int64
	}
	var duplicateCommits []commitPair
	for rows.Next() {
		var pair commitPair
		if err := rows.Scan(&pair.sourceID, &pair.targetID); err != nil {
			rows.Close()
			return fmt.Errorf("scan duplicate commit: %w", err)
		}
		duplicateCommits = append(duplicateCommits, pair)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate duplicate commits: %w", err)
	}
	rows.Close()

	for _, pair := range duplicateCommits {
		if _, err := tx.Exec(`UPDATE review_jobs SET commit_id = ? WHERE commit_id = ?`, pair.targetID, pair.sourceID); err != nil {
			return fmt.Errorf("remap review job commit %d to %d: %w", pair.sourceID, pair.targetID, err)
		}
		if _, err := tx.Exec(`UPDATE responses SET commit_id = ? WHERE commit_id = ?`, pair.targetID, pair.sourceID); err != nil {
			return fmt.Errorf("remap response commit %d to %d: %w", pair.sourceID, pair.targetID, err)
		}
		if _, err := tx.Exec(`DELETE FROM commits WHERE id = ?`, pair.sourceID); err != nil {
			return fmt.Errorf("delete duplicate commit %d: %w", pair.sourceID, err)
		}
	}

	if _, err := tx.Exec(`UPDATE commits SET repo_id = ? WHERE repo_id = ?`, targetRepoID, sourceRepoID); err != nil {
		return fmt.Errorf("move commits: %w", err)
	}
	if err := demoteConflictingAutoDesignJobs(tx, sourceRepoID, targetRepoID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE review_jobs SET repo_id = ? WHERE repo_id = ?`, targetRepoID, sourceRepoID); err != nil {
		return fmt.Errorf("move review jobs: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM repos WHERE id = ?`, sourceRepoID); err != nil {
		return fmt.Errorf("delete duplicate repo: %w", err)
	}
	return nil
}

func demoteConflictingAutoDesignJobs(tx *sql.Tx, sourceRepoID, targetRepoID int64) error {
	var hasSource int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = 'source'`).Scan(&hasSource); err != nil {
		return fmt.Errorf("check review_jobs source column: %w", err)
	}
	if hasSource == 0 {
		return nil
	}

	_, err := tx.Exec(`
		UPDATE review_jobs AS sj
		SET source = 'auto_design_duplicate'
		WHERE sj.repo_id = ?
		  AND sj.source = 'auto_design'
		  AND EXISTS (
			SELECT 1
			FROM review_jobs AS tj
			WHERE tj.repo_id = ?
			  AND tj.source = 'auto_design'
			  AND tj.review_type = sj.review_type
			  AND (
				tj.git_ref = sj.git_ref
				OR (
					tj.commit_id IS NOT NULL
					AND sj.commit_id IS NOT NULL
					AND tj.commit_id = sj.commit_id
				)
			  )
		  )
	`, sourceRepoID, targetRepoID)
	if err != nil {
		return fmt.Errorf("demote duplicate auto-design jobs: %w", err)
	}
	return nil
}

// ResetStaleJobs marks all running jobs as queued (for daemon restart).
// session_id and token_usage are cleared so a job interrupted mid-run (which
// may have streamed a session id, or had a late usage write land on it) does
// not carry the prior attempt's cost into the requeued run, matching the
// other requeue paths (ReenqueueJob, RetryJob, FailoverJob).
func (db *DB) ResetStaleJobs() error {
	if _, err := db.Exec(`
		UPDATE ci_pr_panels
		SET posting_claimed_at = NULL
		WHERE posted_at IS NULL
		  AND retired_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM review_jobs j
			WHERE j.panel_run_uuid = ci_pr_panels.panel_run_uuid
			  AND j.status IN ('queued', 'running')
		  )
	`); err != nil {
		return err
	}
	_, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'queued', worker_id = NULL, started_at = NULL,
		    session_id = NULL, token_usage = NULL, command_line = NULL, agent_invoked = 0, synced_at = NULL
		WHERE status = 'running'
	`)
	return err
}

// CountStalledJobs returns the number of jobs that have been running longer than the threshold
func (db *DB) CountStalledJobs(threshold time.Duration) (int, error) {
	// Use threshold in seconds for SQLite datetime arithmetic
	// This avoids timezone issues with RFC3339 string comparison
	thresholdSecs := int64(threshold.Seconds())

	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs
		WHERE status = 'running'
		AND started_at IS NOT NULL
		AND datetime(started_at) < datetime('now', ? || ' seconds')
	`, -thresholdSecs).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// migrateReviewJobsConstraintsForAutoDesign rebuilds review_jobs to:
//   - Add 'skipped' to the status CHECK constraint
//   - Add the skip_reason and source TEXT columns
//   - Create the auto-design dedup partial unique indexes
//
// job_type has no CHECK constraint in the existing schema, so 'classify'
// is accepted without a rebuild.
// Per-feature idempotency: every step probes for its own presence so a
// partially-migrated DB still converges. CREATE INDEX IF NOT EXISTS is
// naturally idempotent.
func (db *DB) migrateReviewJobsConstraintsForAutoDesign() error {
	ctx := context.Background()

	var origSQL string
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_jobs'`,
	).Scan(&origSQL); err != nil {
		return fmt.Errorf("read review_jobs schema: %w", err)
	}
	needsRebuild := !strings.Contains(origSQL, "'skipped'") ||
		!strings.Contains(origSQL, "skip_reason")

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return
		}
	}()

	if needsRebuild {
		if _, err := tx.Exec(`DROP TABLE IF EXISTS review_jobs_new`); err != nil {
			return fmt.Errorf("cleanup stale temp table: %w", err)
		}

		rows, err := tx.Query(`SELECT name FROM pragma_table_info('review_jobs')`)
		if err != nil {
			return fmt.Errorf("read columns: %w", err)
		}
		var cols []string
		hasSkipReason := false
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			cols = append(cols, name)
			if name == "skip_reason" {
				hasSkipReason = true
			}
		}
		rows.Close()

		newSQL := strings.Replace(origSQL,
			"CHECK(status IN ('queued','running','done','failed','canceled','applied','rebased'))",
			"CHECK(status IN ('queued','running','done','failed','canceled','applied','rebased','skipped'))",
			1)
		if !hasSkipReason {
			lastParen := strings.LastIndex(newSQL, ")")
			if lastParen < 0 {
				return fmt.Errorf("malformed review_jobs schema")
			}
			newSQL = newSQL[:lastParen] + ",\n  skip_reason TEXT" + newSQL[lastParen:]
		}

		replaced := false
		for _, pattern := range []string{
			`CREATE TABLE "review_jobs"`,
			`CREATE TABLE review_jobs`,
		} {
			if strings.Contains(newSQL, pattern) {
				newSQL = strings.Replace(newSQL, pattern, `CREATE TABLE review_jobs_new`, 1)
				replaced = true
				break
			}
		}
		if !replaced {
			return fmt.Errorf("cannot find CREATE TABLE statement in schema: %s",
				origSQL[:min(len(origSQL), 80)])
		}

		if _, err := tx.Exec(newSQL); err != nil {
			return fmt.Errorf("create new table: %w", err)
		}

		colList := strings.Join(cols, ", ")
		copySQL := fmt.Sprintf(`INSERT INTO review_jobs_new (%s) SELECT %s FROM review_jobs`,
			colList, colList)
		if _, err := tx.Exec(copySQL); err != nil {
			return fmt.Errorf("copy data: %w", err)
		}

		if _, err := tx.Exec(`DROP TABLE review_jobs`); err != nil {
			return fmt.Errorf("drop old table: %w", err)
		}
		if _, err := tx.Exec(`ALTER TABLE review_jobs_new RENAME TO review_jobs`); err != nil {
			return fmt.Errorf("rename table: %w", err)
		}

		for _, idx := range []string{
			`CREATE INDEX IF NOT EXISTS idx_review_jobs_status ON review_jobs(status)`,
			`CREATE INDEX IF NOT EXISTS idx_review_jobs_repo ON review_jobs(repo_id)`,
			`CREATE INDEX IF NOT EXISTS idx_review_jobs_git_ref ON review_jobs(git_ref)`,
			`CREATE INDEX IF NOT EXISTS idx_review_jobs_branch ON review_jobs(branch)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_uuid ON review_jobs(uuid)`,
		} {
			if _, err := tx.Exec(idx); err != nil {
				return fmt.Errorf("recreate index: %w", err)
			}
		}
	}

	var hasSource int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name='source'`,
	).Scan(&hasSource); err != nil {
		return fmt.Errorf("probe source column: %w", err)
	}
	if hasSource == 0 {
		if _, err := tx.Exec(`ALTER TABLE review_jobs ADD COLUMN source TEXT`); err != nil {
			return fmt.Errorf("add source column: %w", err)
		}
	}

	if _, err := tx.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_auto_design_dedup
		ON review_jobs(repo_id, commit_id, review_type)
		WHERE source = 'auto_design'
	`); err != nil {
		return fmt.Errorf("create auto-design dedup index: %w", err)
	}

	// The narrow form (commit_id IS NULL only) prevents two commitless
	// rows but lets a commitless row coexist with a commit-backed row
	// for the same git_ref. Widen to cover ALL auto_design rows so
	// the cross-case race is enforced at the storage layer instead of
	// only via the read-side HasAutoDesignSlotForCommit pre-check.
	//
	// If existing duplicates would block the wider index, log and skip;
	// the narrow index keeps current correctness guarantees and the
	// LEFT JOIN in HasAutoDesignSlotForCommit catches the common case.
	var dupes int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT 1 FROM review_jobs WHERE source = 'auto_design'
			GROUP BY repo_id, git_ref, review_type HAVING COUNT(*) > 1
		)
	`).Scan(&dupes); err != nil {
		return fmt.Errorf("count auto-design duplicates: %w", err)
	}
	if dupes == 0 {
		if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_review_jobs_auto_design_dedup_ref`); err != nil {
			return fmt.Errorf("drop narrow auto-design dedup ref index: %w", err)
		}
		if _, err := tx.Exec(`
			CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_auto_design_dedup_ref
			ON review_jobs(repo_id, git_ref, review_type)
			WHERE source = 'auto_design'
		`); err != nil {
			return fmt.Errorf("create wider auto-design dedup ref index: %w", err)
		}
	} else {
		log.Printf("auto-design: %d duplicate (repo, git_ref, review_type) groups exist; "+
			"keeping narrow dedup_ref index until cleaned up", dupes)
		if _, err := tx.Exec(`
			CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_auto_design_dedup_ref
			ON review_jobs(repo_id, git_ref, review_type)
			WHERE source = 'auto_design' AND commit_id IS NULL
		`); err != nil {
			return fmt.Errorf("create narrow auto-design dedup ref index: %w", err)
		}
	}

	return tx.Commit()
}
