package daemon

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
	"uuid"

	"go.kenn.io/roborev/internal/storage"
)

// Version 2 cursors carry the listing's sort and the last row's sort keys.
// Version 1 cursors held only an enqueue time and are rejected.
const jobListCursorVersion = 2

type jobListCursor struct {
	Version    int                   `json:"version"`
	DatabaseID uuid.UUID             `json:"database_id"`
	Sort       storage.JobSortColumn `json:"sort"`
	Ascending  bool                  `json:"ascending"`
	// AsOf is the elapsed-time reference shared by every page of an
	// elapsed sort; other sorts leave it empty.
	AsOf  string `json:"as_of,omitzero"`
	Keys  []any  `json:"keys"`
	JobID int64  `json:"job_id"`
}

// encodeJobListCursor returns a cursor that resumes the sorted listing after
// job, which must come from a ListJobs call using sort.
func (s *Server) encodeJobListCursor(job storage.ReviewJob, sort storage.JobSort) (string, error) {
	if job.ID <= 0 {
		return "", errors.New("cannot encode job cursor without a job ID")
	}
	databaseID, err := s.db.GetDatabaseID()
	if err != nil {
		return "", fmt.Errorf("load database identity: %w", err)
	}
	cursor := jobListCursor{
		Version:    jobListCursorVersion,
		DatabaseID: databaseID,
		Sort:       sort.Column,
		Ascending:  sort.Ascending,
		Keys:       job.SortKey,
		JobID:      job.ID,
	}
	if sort.Column == storage.JobSortElapsed {
		cursor.AsOf = sort.AsOf.UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode job cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// decodeJobListCursor checks that a cursor was issued by this database for
// the requested sort, and returns its resume position and elapsed-time
// reference. An empty value returns a nil position.
func (s *Server) decodeJobListCursor(
	value string, sort storage.JobSort,
) (*storage.JobSortPosition, time.Time, error) {
	if value == "" {
		return nil, time.Time{}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("invalid jobs cursor: %w", err)
	}
	var cursor jobListCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil, time.Time{}, fmt.Errorf("invalid jobs cursor: %w", err)
	}
	if cursor.Version != jobListCursorVersion {
		return nil, time.Time{}, fmt.Errorf(
			"invalid jobs cursor: unsupported version %d", cursor.Version,
		)
	}
	if cursor.DatabaseID == uuid.Nil() || cursor.JobID <= 0 {
		return nil, time.Time{}, errors.New("invalid jobs cursor: missing fields")
	}
	if cursor.Sort != sort.Column || cursor.Ascending != sort.Ascending {
		return nil, time.Time{}, errors.New("invalid jobs cursor: it belongs to a different sort")
	}
	var asOf time.Time
	if sort.Column == storage.JobSortElapsed {
		asOf, err = time.Parse(time.RFC3339Nano, cursor.AsOf)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("invalid jobs cursor elapsed reference: %w", err)
		}
	}
	databaseID, err := s.db.GetDatabaseID()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("load database identity: %w", err)
	}
	if cursor.DatabaseID != databaseID {
		return nil, time.Time{}, errors.New("invalid jobs cursor: database identity changed")
	}
	return &storage.JobSortPosition{Keys: cursor.Keys, JobID: cursor.JobID}, asOf, nil
}
