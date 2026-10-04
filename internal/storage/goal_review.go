package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
)

// LatestCompletedGoalReview returns the most recently enqueued completed review
// of a checkout. Candidate-gate jobs describe hypothetical evidence, so they
// cannot establish what the automatic watcher has already reviewed.
func (db *DB) LatestCompletedGoalReview(checkout string) (*ReviewJob, error) {
	normalized, err := normalizeRepoPath(checkout)
	if err != nil {
		return nil, err
	}
	var id int64
	err = db.QueryRow(`
		SELECT j.id FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		WHERE j.job_type = ? AND j.status = 'done'
		  AND COALESCE(j.source, '') != 'goal_gate'
		  AND COALESCE(NULLIF(j.worktree_path, ''), r.root_path) IN (?, ?)
		ORDER BY `+sqliteNormalizedTimestampExpr("j.enqueued_at")+` DESC, j.id DESC
		LIMIT 1`, JobTypeGoalReview, normalized, filepath.FromSlash(normalized)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return db.GetJobByID(id)
}
