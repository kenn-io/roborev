package storage

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/tokens"
)

// ReviewInvocation is execution-local evidence, independent of the mutable job
// row. Unfinished means no outcome was recorded; it does not assert a live agent.
type ReviewInvocation struct {
	ID               int64          `json:"id"`
	JobID            int64          `json:"job_id"`
	EnqueuedAt       time.Time      `json:"enqueued_at"`
	StartedAt        time.Time      `json:"started_at"`
	FinishedAt       *time.Time     `json:"finished_at"`
	Status           string         `json:"status"`
	Agent            string         `json:"agent"`
	Model            string         `json:"model"`
	Provider         string         `json:"provider"`
	Reasoning        string         `json:"reasoning"`
	GitRef           string         `json:"git_ref"`
	RetryCount       int            `json:"retry_count"`
	SessionID        string         `json:"session_id"`
	RawOutput        string         `json:"raw_output"`
	Output           string         `json:"output"`
	StructuredOutput jsontext.Value `json:"structured_output,omitempty"`
	Error            string         `json:"error"`
	Usage            *tokens.Usage  `json:"usage"`
	UsageError       string         `json:"usage_error,omitempty"`
}

// TransformSchema preserves arbitrary review properties and distinguishes
// missing usage from a measured zero. Huma does not infer nullable objects.
func (ReviewInvocation) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	s.Properties["structured_output"] = (StructuredOutput{}).Schema(r)
	s.Properties["usage"] = &huma.Schema{AnyOf: []*huma.Schema{
		s.Properties["usage"], {Type: "null"},
	}}
	return s
}

func migrateReviewInvocations(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS review_invocations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES review_jobs(id) ON DELETE CASCADE,
  finished_at TEXT,
  report BLOB NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_review_invocations_job ON review_invocations(job_id,id);`)
	return err
}

// BeginReviewInvocation must run under the caller's current job-attempt fence.
func (db *DB) BeginReviewInvocation(ctx context.Context, job *ReviewJob, agentName string) (ReviewInvocation, error) {
	report := ReviewInvocation{JobID: job.ID, EnqueuedAt: job.EnqueuedAt, StartedAt: time.Now().UTC(), Status: "unfinished", Agent: agentName, Model: job.Model, Provider: job.Provider, Reasoning: job.Reasoning, GitRef: job.GitRef, RetryCount: job.RetryCount}
	data, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	err = db.QueryRowContext(ctx, `INSERT INTO review_invocations(job_id,report)
 SELECT id,zstd_compress(?) FROM review_jobs WHERE id=? AND status='running' RETURNING id`, string(data), job.ID).Scan(&report.ID)
	return report, err
}

// FinishReviewInvocation records the outcome even if sync or cancellation has
// replaced the job row. The invocation ID belongs only to this execution.
// Subprocess diagnostics need not be UTF-8; replace invalid bytes when encoding
// strings so a diagnostic cannot discard evidence or block retry/failover.
func (db *DB) FinishReviewInvocation(ctx context.Context, report ReviewInvocation) error {
	if report.FinishedAt == nil || (report.Status != "succeeded" && report.Status != "failed" && report.Status != "canceled") {
		return fmt.Errorf("invalid invocation completion")
	}
	data, err := json.Marshal(report, jsontext.AllowInvalidUTF8(true))
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE review_invocations SET finished_at=?, report=zstd_compress(?) WHERE id=? AND job_id=? AND finished_at IS NULL`, report.FinishedAt.Format(time.RFC3339Nano), string(data), report.ID, report.JobID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

func (db *DB) ListReviewInvocations(ctx context.Context, jobID int64) ([]ReviewInvocation, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,zstd_decompress(report) FROM review_invocations WHERE job_id=? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reports := []ReviewInvocation{}
	for rows.Next() {
		var id int64
		var data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var report ReviewInvocation
		if err := json.Unmarshal([]byte(data), &report); err != nil {
			return nil, fmt.Errorf("decode invocation %d: %w", id, err)
		}
		report.ID = id
		reports = append(reports, report)
	}
	return reports, rows.Err()
}
