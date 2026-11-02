package storage

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/roborev/internal/provenance"
)

const jobProvenanceExpr = "zstd_decompress(jc.session_provenance)"

func encodeSessionProvenance(commits []provenance.Commit) (string, error) {
	if len(commits) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(commits)
	return string(data), err
}

func decodeSessionProvenance(raw string) ([]provenance.Commit, error) {
	if raw == "" {
		return nil, nil
	}
	var commits []provenance.Commit
	if err := json.Unmarshal([]byte(raw), &commits); err != nil {
		return nil, errors.New("invalid session provenance JSON")
	}
	if len(commits) == 0 {
		return nil, nil
	}
	return commits, nil
}

func migrateSessionProvenance(ctx context.Context, db *DB) error {
	exists, err := hasColumn(ctx, db, "job_content", "session_provenance")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := db.ExecContext(ctx, `ALTER TABLE job_content ADD COLUMN session_provenance BLOB`); err != nil {
			return err
		}
	}
	// Every reset uses a queued transition. Clear within that same transaction,
	// including retry/failover (which retain the synchronization attempt ID).
	// [] records a known empty capture; NULL remains pre-feature/unknown data.
	_, err = db.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS reset_session_provenance
 AFTER UPDATE OF status, enqueued_at ON review_jobs
 WHEN NEW.status='queued' AND (OLD.status!='queued' OR NEW.enqueued_at!=OLD.enqueued_at)
 BEGIN
  UPDATE job_content SET session_provenance=zstd_compress('[]') WHERE job_id=NEW.id;
 END`)
	return err
}
