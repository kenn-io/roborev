package storage

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
)

// Reply snapshots are local run evidence, not a delivery queue or sync source.
// They survive review reruns alongside the immutable finding snapshots.
func migrateKataReplySnapshots(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS kata_reply_snapshots (
 run_id TEXT NOT NULL,
 purpose TEXT NOT NULL,
 source_job_id INTEGER NOT NULL REFERENCES review_jobs(id) ON DELETE CASCADE,
 review_uuid TEXT NOT NULL,
 snapshot_json TEXT NOT NULL,
 PRIMARY KEY(run_id,source_job_id,review_uuid)
 )`)
	return err
}

// KataReplySnapshot returns the exact reply context consumed by this run.
// A missing snapshot returns nil, nil.
func (db *DB) KataReplySnapshot(ctx context.Context, runID string, jobID int64, reviewUUID string) (jsontext.Value, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT snapshot_json FROM kata_reply_snapshots WHERE run_id=? AND source_job_id=? AND review_uuid=?`, runID, jobID, reviewUUID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return jsontext.Value(raw), err
}

// SaveKataReplySnapshot records the first snapshot a run consumed. Concurrent
// readers receive that winner rather than replacing its evidence.
func (db *DB) SaveKataReplySnapshot(ctx context.Context, runID, purpose string, jobID int64, reviewUUID string, snapshot jsontext.Value) (jsontext.Value, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(purpose) == "" || jobID <= 0 || strings.TrimSpace(reviewUUID) == "" || !snapshot.IsValid() {
		return nil, fmt.Errorf("reply snapshot requires a run, purpose, source review and valid JSON")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO kata_reply_snapshots(run_id,purpose,source_job_id,review_uuid,snapshot_json) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, runID, purpose, jobID, reviewUUID, string(snapshot))
	if err != nil {
		return nil, err
	}
	return db.KataReplySnapshot(ctx, runID, jobID, reviewUUID)
}

// KataHandoffsForIssue reads existing frozen mappings. It never creates or
// reconciles an operation. Callers must exclude response operations and enforce
// the source job's repository authorization before exposing their context.
func (db *DB) KataHandoffsForIssue(ctx context.Context, ref string) ([]KataHandoff, error) {
	rows, err := db.QueryContext(ctx, kataHandoffSelect+` WHERE json_extract(state_json,'$.IssueRef')=? OR json_extract(state_json,'$.IssueUID')=? ORDER BY operation_id`, ref, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KataHandoff
	for rows.Next() {
		h, err := scanKataHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
