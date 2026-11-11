package storage

import (
	"context"
	"database/sql"
	"errors"
	"uuid"
)

var ErrPanelRequestConflict = errors.New("request_id was already used with different panel input")

// PanelEnqueueRequest identifies one caller submission independently of its
// resolved revision and execution defaults. Hash covers the RFC 8785 canonical
// JSON request body, independently of the server's request types.
type PanelEnqueueRequest struct {
	ID   uuid.UUID
	Hash string
}

func migratePanelEnqueueRequests(ctx context.Context, db *DB) error {
	// Keep receipts after repository deletion so a delayed retry cannot execute
	// the same request again. No prompt, output, or repository path is retained.
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS panel_enqueue_requests (
  request_id TEXT PRIMARY KEY,
  request_hash TEXT NOT NULL,
  result_job_id INTEGER NOT NULL,
  panel_run_uuid TEXT NOT NULL
 )`)
	return err
}

// GetPanelEnqueueRequest returns the original panel identity before callers
// resolve Git refs or execution settings again. Its jobs may have been deleted.
func (db *DB) GetPanelEnqueueRequest(ctx context.Context, request PanelEnqueueRequest) (*ReviewJob, bool, error) {
	return lookupPanelEnqueueRequest(ctx, db, request)
}

func lookupPanelEnqueueRequest(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, request PanelEnqueueRequest,
) (*ReviewJob, bool, error) {
	var hash string
	var run uuid.UUID
	var job ReviewJob
	err := q.QueryRowContext(ctx, `SELECT request_hash, result_job_id, panel_run_uuid
 FROM panel_enqueue_requests WHERE request_id = ?`, request.ID).Scan(&hash, &job.ID, &run)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if hash != request.Hash {
		return nil, false, ErrPanelRequestConflict
	}
	job.PanelRunUUID = &run
	return &job, true, nil
}

// EnqueuePanelRunRequest commits the jobs and receipt together. Identical
// concurrent submissions return one panel; different input conflicts.
func (db *DB) EnqueuePanelRunRequest(ctx context.Context, members []EnqueueOpts, synthesis EnqueueOpts, request PanelEnqueueRequest) ([]*ReviewJob, *ReviewJob, bool, error) {
	if request.ID == uuid.Nil() || request.Hash == "" {
		return nil, nil, false, errors.New("panel request ID and hash are required")
	}
	return db.enqueuePanelRun(ctx, members, synthesis, false, uuid.Nil(), 0, &request)
}
