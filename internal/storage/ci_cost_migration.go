package storage

import (
	"context"
	"fmt"
	"time"
)

// migrateCICostExportState installs the local, database-wide revision clock
// used by the CI cost cursor. SQLite serializes the trigger's counter update
// with the job mutation, so cost-affecting writes cannot share a revision even
// when their updated_at timestamps fall in the same second.
func (db *DB) migrateCICostExportState() error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection for CI cost migration: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin CI cost migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var hasRevision int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('review_jobs')
		 WHERE name = 'ci_cost_revision'`,
	).Scan(&hasRevision); err != nil {
		return fmt.Errorf("check CI cost revision column: %w", err)
	}
	if hasRevision == 0 {
		if _, err := conn.ExecContext(ctx,
			`ALTER TABLE review_jobs ADD COLUMN ci_cost_revision INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("add CI cost revision column: %w", err)
		}
	}

	// Early panel jobs predate the durable source field. Stamp ownership while
	// their mapping still exists and invalidate any synced snapshot carrying a
	// NULL source.
	if _, err := conn.ExecContext(ctx, `
		UPDATE review_jobs
		SET source = ?, updated_at = ?, synced_at = NULL
		WHERE (source IS NULL OR source = '')
		  AND EXISTS (
			SELECT 1 FROM ci_pr_panels p
			WHERE p.panel_run_uuid = review_jobs.panel_run_uuid
		  )
	`, JobSourceCI, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("backfill CI job ownership: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS ci_cost_revision_clock (
			singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
			revision INTEGER NOT NULL
		);
		INSERT OR IGNORE INTO ci_cost_revision_clock (singleton, revision)
		VALUES (1, 0);
		UPDATE review_jobs SET ci_cost_revision = id WHERE ci_cost_revision = 0;
		UPDATE ci_cost_revision_clock
		SET revision = MAX(revision,
			COALESCE((SELECT MAX(ci_cost_revision) FROM review_jobs), 0))
		WHERE singleton = 1;
		CREATE INDEX IF NOT EXISTS idx_review_jobs_ci_cost_revision
		ON review_jobs(ci_cost_revision, id);
	`); err != nil {
		return fmt.Errorf("initialize CI cost revisions: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `
		CREATE TRIGGER IF NOT EXISTS review_jobs_ci_cost_revision_insert
		AFTER INSERT ON review_jobs
		WHEN NEW.ci_cost_revision = 0
		BEGIN
			UPDATE ci_cost_revision_clock
			SET revision = revision + 1 WHERE singleton = 1;
			UPDATE review_jobs
			SET ci_cost_revision = (
				SELECT revision FROM ci_cost_revision_clock WHERE singleton = 1
			)
			WHERE id = NEW.id;
		END;

		CREATE TRIGGER IF NOT EXISTS review_jobs_ci_cost_revision_update
		AFTER UPDATE OF status, finished_at, agent, model, provider, source,
			panel_run_uuid, panel_role, agent_invoked, token_usage
		ON review_jobs
		WHEN OLD.status IS NOT NEW.status
			OR OLD.finished_at IS NOT NEW.finished_at
			OR OLD.agent IS NOT NEW.agent
			OR OLD.model IS NOT NEW.model
			OR OLD.provider IS NOT NEW.provider
			OR OLD.source IS NOT NEW.source
			OR OLD.panel_run_uuid IS NOT NEW.panel_run_uuid
			OR OLD.panel_role IS NOT NEW.panel_role
			OR OLD.agent_invoked IS NOT NEW.agent_invoked
			OR OLD.token_usage IS NOT NEW.token_usage
		BEGIN
			UPDATE ci_cost_revision_clock
			SET revision = revision + 1 WHERE singleton = 1;
			UPDATE review_jobs
			SET ci_cost_revision = (
				SELECT revision FROM ci_cost_revision_clock WHERE singleton = 1
			)
			WHERE id = NEW.id;
		END;
	`); err != nil {
		return fmt.Errorf("create CI cost revision triggers: %w", err)
	}

	// The mapping is lifecycle state; review_jobs.source is the durable record.
	// Preserve that record inside the same statement transaction before every
	// mapping deletion, including cleanup paths that issue raw SQL.
	if _, err := conn.ExecContext(ctx, `
		CREATE TRIGGER IF NOT EXISTS ci_pr_panels_preserve_job_source
		BEFORE DELETE ON ci_pr_panels
		BEGIN
			UPDATE review_jobs
			SET source = 'ci',
				updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'),
				synced_at = NULL
			WHERE panel_run_uuid = OLD.panel_run_uuid
			  AND (source IS NULL OR source = '');
		END;
	`); err != nil {
		return fmt.Errorf("create CI ownership preservation trigger: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit CI cost migration: %w", err)
	}
	committed = true
	return nil
}
