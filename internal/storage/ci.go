package storage

import (
	"context"
	"database/sql"
	"time"
)

// HasCIReview checks if a PR has already been reviewed at the given HEAD SHA.
func (db *DB) HasCIReview(githubRepo string, prNumber int, headSHA string) (bool, error) {
	var count int
	err := db.bun.NewSelect().
		Model((*ciPRReviewRow)(nil)).
		ColumnExpr("COUNT(*)").
		Where("github_repo = ?", githubRepo).
		Where("pr_number = ?", prNumber).
		Where("head_sha = ?", headSHA).
		Scan(context.Background(), &count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// RecordCIReview records that a PR was reviewed at a given HEAD SHA
func (db *DB) RecordCIReview(githubRepo string, prNumber int, headSHA string, jobID int64) error {
	row := ciPRReviewRow{
		GithubRepo: githubRepo,
		PRNumber:   prNumber,
		HeadSHA:    headSHA,
		JobID:      jobID,
	}
	_, err := db.bun.NewInsert().
		Model(&row).
		Column("github_repo", "pr_number", "head_sha", "job_id").
		Exec(context.Background())
	return err
}

// BatchReviewResult holds the output of a single review job within a panel run.
type BatchReviewResult struct {
	JobID                 int64  `json:"job_id"`
	Agent                 string `json:"agent"`
	ReviewType            string `json:"review_type"`
	PanelMemberName       string `json:"panel_member_name,omitempty"`
	Output                string `json:"output"`
	Status                string `json:"status"` // "done", "failed", "skipped", etc.
	Error                 string `json:"error"`
	SkipReason            string `json:"skip_reason,omitempty"`
	PanelMemberConfigJSON string `json:"panel_member_config_json,omitempty"`
	StartedAt             string `json:"started_at,omitempty"`
	FinishedAt            string `json:"finished_at,omitempty"`
	TokenUsage            string `json:"token_usage,omitempty"`
}

// CancelJobWithError cancels a queued or running job and sets an error
// message explaining why it was canceled. Returns sql.ErrNoRows if the
// job is already terminal.
func (db *DB) CancelJobWithError(jobID int64, errMsg string) error {
	now := dbTimeFromValue(time.Now())
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("status = ?", JobStatusCanceled).
		Set("error = ?", errMsg).
		Set("finished_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", jobID).
		Where("status IN ('queued', 'running')").
		Exec(context.Background())
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
