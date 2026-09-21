package storage

import (
	"database/sql"
	"time"
)

type ScheduledAnalysisRecord struct {
	Status     JobStatus
	Commit     string
	FinishedAt *time.Time
	EnqueuedAt time.Time
	ID         int64
}

func (r ScheduledAnalysisRecord) FinishedAtOrEnqueued() time.Time {
	if r.FinishedAt != nil {
		return *r.FinishedAt
	}
	return r.EnqueuedAt
}

func (db *DB) ScheduledAnalysisHistory(repoID int64, path, analysisType string) ([]ScheduledAnalysisRecord, error) {
	rows, err := db.Query(`
		SELECT j.id, j.status, j.analysis_commit_sha, j.finished_at, j.enqueued_at
		FROM review_jobs j, json_each(j.analysis_files)
		WHERE j.repo_id = ? AND j.analysis_type = ? AND json_each.value = ?
		ORDER BY j.id`, repoID, analysisType, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ScheduledAnalysisRecord
	for rows.Next() {
		var r ScheduledAnalysisRecord
		var status, commit, finished, enqueued sql.NullString
		if err := rows.Scan(&r.ID, &status, &commit, &finished, &enqueued); err != nil {
			return nil, err
		}
		r.Status = JobStatus(status.String)
		r.Commit = commit.String
		if finished.Valid {
			t := parseSQLiteTime(finished.String)
			r.FinishedAt = &t
		}
		if enqueued.Valid {
			r.EnqueuedAt = parseSQLiteTime(enqueued.String)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
