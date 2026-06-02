package storage

import (
	"database/sql"
	"time"
)

// HasCIReview checks if a PR has already been reviewed at the given HEAD SHA.
func (db *DB) HasCIReview(githubRepo string, prNumber int, headSHA string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM ci_pr_reviews WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`,
		githubRepo, prNumber, headSHA).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// RecordCIReview records that a PR was reviewed at a given HEAD SHA
func (db *DB) RecordCIReview(githubRepo string, prNumber int, headSHA string, jobID int64) error {
	_, err := db.Exec(`INSERT INTO ci_pr_reviews (github_repo, pr_number, head_sha, job_id) VALUES (?, ?, ?, ?)`,
		githubRepo, prNumber, headSHA, jobID)
	return err
}

// BatchReviewResult holds the output of a single review job within a panel run.
type BatchReviewResult struct {
	JobID           int64  `json:"job_id"`
	Agent           string `json:"agent"`
	ReviewType      string `json:"review_type"`
	PanelMemberName string `json:"panel_member_name,omitempty"`
	Output          string `json:"output"`
	Status          string `json:"status"` // "done", "failed", "skipped", etc.
	Error           string `json:"error"`
	SkipReason      string `json:"skip_reason,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	TokenUsage      string `json:"token_usage,omitempty"`
}

// CancelJobWithError cancels a queued or running job and sets an error
// message explaining why it was canceled. Returns sql.ErrNoRows if the
// job is already terminal.
func (db *DB) CancelJobWithError(jobID int64, errMsg string) error {
	now := time.Now().Format(time.RFC3339)
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'canceled', error = ?, finished_at = ?, updated_at = ?
		WHERE id = ? AND status IN ('queued', 'running')
	`, errMsg, now, now, jobID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}
