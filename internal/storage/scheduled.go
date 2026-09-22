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

type ScheduledAnalysisKey struct {
	Path string
	Type string
}

func (r ScheduledAnalysisRecord) FinishedAtOrEnqueued() time.Time {
	if r.FinishedAt != nil {
		return *r.FinishedAt
	}
	return r.EnqueuedAt
}

func (db *DB) ScheduledAnalysisHistoryForRepo(repoID int64) (map[ScheduledAnalysisKey][]ScheduledAnalysisRecord, error) {
	rows, err := db.Query(`
		SELECT j.id, j.status, j.analysis_type, json_each.value,
		       j.analysis_commit_sha, j.finished_at, j.enqueued_at
		FROM review_jobs j, json_each(j.analysis_files)
		WHERE j.repo_id = ?
		  AND j.analysis_type IS NOT NULL AND j.analysis_type != ''
		ORDER BY j.id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[ScheduledAnalysisKey][]ScheduledAnalysisRecord)
	for rows.Next() {
		var r ScheduledAnalysisRecord
		var analysisType, path string
		var status, commit, finished, enqueued sql.NullString
		if err := rows.Scan(&r.ID, &status, &analysisType, &path, &commit, &finished, &enqueued); err != nil {
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
		key := ScheduledAnalysisKey{Path: path, Type: analysisType}
		result[key] = append(result[key], r)
	}
	return result, rows.Err()
}
