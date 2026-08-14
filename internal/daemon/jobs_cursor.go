package daemon

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

const jobListCursorVersion = 1

type jobListCursor struct {
	Version    int    `json:"version"`
	DatabaseID string `json:"database_id"`
	EnqueuedAt string `json:"enqueued_at"`
	JobID      int64  `json:"job_id"`
}

func (s *Server) encodeJobListCursor(job storage.ReviewJob) (string, error) {
	if job.ID <= 0 || job.EnqueuedAt.IsZero() {
		return "", errors.New("cannot encode job cursor without an enqueue position")
	}
	databaseID, err := s.db.GetDatabaseID()
	if err != nil {
		return "", fmt.Errorf("load database identity: %w", err)
	}
	data, err := json.Marshal(jobListCursor{
		Version:    jobListCursorVersion,
		DatabaseID: databaseID,
		EnqueuedAt: job.EnqueuedAt.UTC().Format(time.RFC3339Nano),
		JobID:      job.ID,
	})
	if err != nil {
		return "", fmt.Errorf("encode job cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (s *Server) decodeJobListCursor(value string) (*jobListCursor, time.Time, error) {
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
	if cursor.DatabaseID == "" || cursor.EnqueuedAt == "" || cursor.JobID <= 0 {
		return nil, time.Time{}, errors.New("invalid jobs cursor: missing fields")
	}
	enqueuedAt, err := time.Parse(time.RFC3339Nano, cursor.EnqueuedAt)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("invalid jobs cursor timestamp: %w", err)
	}
	databaseID, err := s.db.GetDatabaseID()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("load database identity: %w", err)
	}
	if cursor.DatabaseID != databaseID {
		return nil, time.Time{}, errors.New("invalid jobs cursor: database identity changed")
	}
	return &cursor, enqueuedAt.UTC(), nil
}
