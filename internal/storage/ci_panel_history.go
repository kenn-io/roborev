package storage

import "fmt"

// migrateCIPanelHistory replaces the original all-history uniqueness constraint
// with ownership of one active row. No tables reference ci_pr_panels, and it is
// not sync-replicated, so this rebuild needs no foreign-key or PostgreSQL changes.
func (db *DB) migrateCIPanelHistory() error {
	var oldConstraint int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_index_list('ci_pr_panels') WHERE origin = 'u'`).Scan(&oldConstraint); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if oldConstraint > 0 {
		if _, err := tx.Exec(`CREATE TABLE ci_pr_panels_history (
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
			outcome TEXT,
			first_attempt_at TEXT,
			attempt_count INTEGER,
			synthesis_agent TEXT,
			synthesis_model TEXT,
			allow_stale_post INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
			return err
		}
		for _, statement := range []string{
			`INSERT INTO ci_pr_panels_history (` + ciPanelColumns + `) SELECT ` + ciPanelColumns + ` FROM ci_pr_panels`,
			`DROP TABLE ci_pr_panels`,
			`ALTER TABLE ci_pr_panels_history RENAME TO ci_pr_panels`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("migrate CI panel history: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_ci_panels_active_head
		ON ci_pr_panels(github_repo, pr_number, head_sha) WHERE retired_at IS NULL`); err != nil {
		return err
	}
	return tx.Commit()
}
