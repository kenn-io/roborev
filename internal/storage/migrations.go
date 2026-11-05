package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// SQLite schema changes are numbered migrations recorded in
// schema_migrations. Rules:
//
//   - Append a new migration with the next version. Never edit, renumber,
//     or remove a migration that has reached main; fix mistakes with a new
//     migration.
//   - A migration is recorded only after it finishes, so an interrupted
//     migration runs again on the next start. Write it to be safe to rerun.
//   - A migration may manage its own transactions, for example to work in
//     batches or to run VACUUM, which SQLite does not allow in a transaction.
//
// The steps Open runs after the migrations are not numbered:
// the auto-design dedup index widening, which waits until duplicate rows are
// gone, and the data steps. Job and repository ID preservation run on every
// start. Historical review conversion, restoration, and verdict backfill run
// on each start until they complete once.

// schemaMigration is one numbered step of the SQLite schema.
type schemaMigration struct {
	version int
	name    string
	apply   func(ctx context.Context, db *DB) error
}

// schemaMigrations lists every SQLite schema migration in version order.
var schemaMigrations = []schemaMigration{
	{1, "baseline", migrateBaseline},
	{2, "compress job content", migrateJobContent},
	{3, "CI panel retirement reasons", migrateCIPanelRetireReason},
	{4, "CI panel PR head index", migrateCIPanelPRHeadIndex},
	{5, "token cost started-at index", migrateTokenCostStartedIndex},
	{6, "summary repo window index", migrateSummaryRepoWindowIndex},
	{7, "review export completion index", migrateReviewExportCompletedIndex},
	{8, "search change log", migrateSearchChanges},
	{9, "cost aggregate index", migrateCostAggregateIndex},
	{10, "open failed reviews index", migrateOpenFailedReviewsIndex},
	{11, "queue list position indexes", migrateQueueListPositionIndexes},
}

// migrateQueueListPositionIndexes's triggers keep review_jobs.review_closed equal to the job's reviews.closed.
func migrateQueueListPositionIndexes(ctx context.Context, db *DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_branch_position
		ON review_jobs(branch, `+sqliteNormalizedTimestampExpr("enqueued_at")+` DESC, id DESC)`); err != nil {
		return err
	}
	exists, err := hasColumn(ctx, tx, "review_jobs", "review_closed")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE review_jobs ADD COLUMN review_closed INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE review_jobs SET review_closed = 1 WHERE id IN (SELECT job_id FROM reviews WHERE closed = 1);
		CREATE TRIGGER IF NOT EXISTS review_closed_insert AFTER INSERT ON reviews BEGIN
			UPDATE review_jobs SET review_closed = NEW.closed WHERE id = NEW.job_id AND review_closed <> NEW.closed;
		END;
		CREATE TRIGGER IF NOT EXISTS review_closed_update AFTER UPDATE OF closed ON reviews BEGIN
			UPDATE review_jobs SET review_closed = NEW.closed WHERE id = NEW.job_id AND review_closed <> NEW.closed;
		END;
		CREATE TRIGGER IF NOT EXISTS review_closed_delete AFTER DELETE ON reviews BEGIN
			UPDATE review_jobs SET review_closed = 0 WHERE id = OLD.job_id AND review_closed <> 0;
		END;
		CREATE INDEX IF NOT EXISTS idx_review_jobs_closed_position
		ON review_jobs(`+sqliteNormalizedTimestampExpr("enqueued_at")+` DESC, id DESC) WHERE review_closed = 1;
	`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func migrateCostAggregateIndex(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_cost
		ON review_jobs(repo_id, branch, CASE WHEN started_at IS NOT NULL AND finished_at IS NOT NULL AND status IN ('done','applied','rebased','failed','canceled','skipped') AND (agent_invoked = 1 OR (json_valid(token_usage) AND (json_extract(token_usage, '$.has_cost') OR json_extract(token_usage, '$.total_output_tokens') > 0 OR json_extract(token_usage, '$.peak_context_tokens') > 0 OR json_extract(token_usage, '$.cache_creation_tokens') > 0 OR json_extract(token_usage, '$.cost_usd') > 0))) THEN 1 ELSE 0 END,
			CASE WHEN started_at IS NOT NULL AND finished_at IS NOT NULL AND status IN ('done','applied','rebased','failed','canceled','skipped') AND (agent_invoked = 1 OR (json_valid(token_usage) AND (json_extract(token_usage, '$.has_cost') OR json_extract(token_usage, '$.total_output_tokens') > 0 OR json_extract(token_usage, '$.peak_context_tokens') > 0 OR json_extract(token_usage, '$.cache_creation_tokens') > 0 OR json_extract(token_usage, '$.cost_usd') > 0))) AND json_valid(token_usage) AND COALESCE(json_extract(token_usage, '$.has_cost'), 0) AND json_extract(token_usage, '$.cost_usd') IS NOT NULL AND COALESCE(json_extract(token_usage, '$.expected_provider_sessions'), 0) <= COALESCE(json_array_length(token_usage, '$.provider_session_ids'), 0) THEN 1 ELSE 0 END,
			CASE WHEN started_at IS NOT NULL AND finished_at IS NOT NULL AND status IN ('done','applied','rebased','failed','canceled','skipped') AND (agent_invoked = 1 OR (json_valid(token_usage) AND (json_extract(token_usage, '$.has_cost') OR json_extract(token_usage, '$.total_output_tokens') > 0 OR json_extract(token_usage, '$.peak_context_tokens') > 0 OR json_extract(token_usage, '$.cache_creation_tokens') > 0 OR json_extract(token_usage, '$.cost_usd') > 0))) AND json_valid(token_usage) AND COALESCE(json_extract(token_usage, '$.has_cost'), 0) AND json_extract(token_usage, '$.cost_usd') IS NOT NULL THEN json_extract(token_usage, '$.cost_usd') ELSE 0 END
		)`)
	return err
}

// ErrSchemaTooNew reports a database written by a newer roborev release.
var ErrSchemaTooNew = errors.New("database schema is newer than this roborev release")

// migrateBaseline brings a database that predates numbered migrations to the
// schema of the release that introduced them. A new database takes the same
// path. Its steps check the current schema before each change, so it also
// accepts every older layout. It is frozen: later schema changes are new
// migrations, not edits to schema, migrate(), or the helpers migrate() calls.
func migrateBaseline(_ context.Context, db *DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	return db.migrate()
}

// runSchemaMigrations applies every migration newer than the database's
// recorded version, in order.
func (db *DB) runSchemaMigrations(ctx context.Context, migrations []schemaMigration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	latest := migrations[len(migrations)-1].version
	if current > latest {
		return fmt.Errorf("%w: the database is at schema version %d and this release supports up to %d; upgrade roborev",
			ErrSchemaTooNew, current, latest)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		log.Printf("Database migration %d: %s", m.version, m.name)
		if err := m.apply(ctx, db); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
	}
	return nil
}

// migrateCIPanelRetireReason leaves historical retirement causes unspecified.
// CI panel state is local-only and is not replicated to PostgreSQL.
func migrateCIPanelRetireReason(ctx context.Context, db *DB) error {
	exists, err := hasColumn(ctx, db, "ci_pr_panels", "retire_reason")
	if err != nil || exists {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE ci_pr_panels ADD COLUMN retire_reason TEXT`)
	return err
}

func migrateOpenFailedReviewsIndex(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_reviews_open_failed
		ON reviews(job_id) WHERE closed = 0 AND verdict_bool = 0`)
	return err
}

func migrateCIPanelPRHeadIndex(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_ci_panels_pr_head
		ON ci_pr_panels(github_repo, pr_number, head_sha)`)
	return err
}

func migrateTokenCostStartedIndex(ctx context.Context, db *DB) error {
	// Token-cost queries must use the same normalized started-at expression.
	_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_source_started
		ON review_jobs(source_machine_id, `+sqliteNormalizedTimestampExpr("started_at")+`)`)
	return err
}

func migrateSummaryRepoWindowIndex(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_repo_enqueued
		ON review_jobs(repo_id, `+sqliteNormalizedTimestampExpr("enqueued_at")+`)`)
	return err
}

func migrateReviewExportCompletedIndex(ctx context.Context, db *DB) error {
	// Export's ORDER BY and cursor must use the same normalized expression.
	_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_reviews_export_completed
		ON reviews(`+sqliteNormalizedTimestampExpr("created_at")+`, uuid)`)
	return err
}

// migrateSearchChanges records local search feed writes, including deletes.
func migrateSearchChanges(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS search_changes (
		seq INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER, repo_id INTEGER, commit_id INTEGER
	);
	CREATE INDEX IF NOT EXISTS idx_review_jobs_commit_id ON review_jobs(commit_id);
	CREATE TRIGGER IF NOT EXISTS search_reviews_insert AFTER INSERT ON reviews BEGIN
		INSERT INTO search_changes(job_id) VALUES (NEW.job_id);
	END;
	CREATE TRIGGER IF NOT EXISTS search_reviews_update AFTER UPDATE OF id, job_id, uuid, closed, output, structured_output, verdict_bool ON reviews BEGIN
		INSERT INTO search_changes(job_id) VALUES (NEW.job_id);
	END;
	CREATE TRIGGER IF NOT EXISTS search_reviews_delete AFTER DELETE ON reviews BEGIN
		INSERT INTO search_changes(job_id) VALUES (OLD.job_id);
	END;
	CREATE TRIGGER IF NOT EXISTS search_jobs_update AFTER UPDATE OF
		uuid, repo_id, branch, git_ref, commit_id, review_type, panel_role,
		panel_run_uuid, agent, finished_at, status, job_type ON review_jobs BEGIN
		INSERT INTO search_changes(job_id) VALUES (NEW.id);
	END;
	CREATE TRIGGER IF NOT EXISTS search_jobs_delete AFTER DELETE ON review_jobs BEGIN
		INSERT INTO search_changes(job_id) VALUES (OLD.id);
	END;
	CREATE TRIGGER IF NOT EXISTS search_repos_update AFTER UPDATE OF name ON repos BEGIN
		INSERT INTO search_changes(repo_id) VALUES (NEW.id);
	END;
	CREATE TRIGGER IF NOT EXISTS search_responses_insert AFTER INSERT ON responses BEGIN
		INSERT INTO search_changes(job_id) SELECT NEW.job_id WHERE NEW.job_id IS NOT NULL;
		INSERT INTO search_changes(commit_id) SELECT NEW.commit_id WHERE NEW.job_id IS NULL AND NEW.commit_id IS NOT NULL;
	END;
	CREATE TRIGGER IF NOT EXISTS search_responses_update AFTER UPDATE OF id, job_id, commit_id, responder, response, created_at, uuid ON responses BEGIN
		INSERT INTO search_changes(job_id) SELECT NEW.job_id WHERE NEW.job_id IS NOT NULL;
		INSERT INTO search_changes(commit_id) SELECT NEW.commit_id WHERE NEW.job_id IS NULL AND NEW.commit_id IS NOT NULL;
	END;
	CREATE TRIGGER IF NOT EXISTS search_responses_delete AFTER DELETE ON responses BEGIN
		INSERT INTO search_changes(job_id) SELECT OLD.job_id WHERE OLD.job_id IS NOT NULL;
		INSERT INTO search_changes(commit_id) SELECT OLD.commit_id WHERE OLD.job_id IS NULL AND OLD.commit_id IS NOT NULL;
	END;
	CREATE TRIGGER IF NOT EXISTS search_job_content_insert AFTER INSERT ON job_content BEGIN
		INSERT INTO search_changes(job_id) VALUES (NEW.job_id);
	END;
	CREATE TRIGGER IF NOT EXISTS search_job_content_update AFTER UPDATE OF diff_content ON job_content BEGIN
		INSERT INTO search_changes(job_id) VALUES (NEW.job_id);
	END;
	CREATE TRIGGER IF NOT EXISTS search_job_content_delete AFTER DELETE ON job_content BEGIN
		INSERT INTO search_changes(job_id) VALUES (OLD.job_id);
	END;
	`)
	return err
}
