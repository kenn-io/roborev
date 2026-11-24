package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	queuePausedStateKey      = "queue_paused"
	shutdownDrainingStateKey = "shutdown_draining"
)

// IsQueuePaused returns whether daemon workers should stop claiming new jobs.
func (db *DB) IsQueuePaused() (bool, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM daemon_state WHERE key = ?`, queuePausedStateKey).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("get queue paused state: %w", err)
	}
	return value == "true" || value == "1", nil
}

// SetQueuePaused persists whether daemon workers should stop claiming new jobs.
func (db *DB) SetQueuePaused(paused bool) error {
	return db.setDaemonBoolState(queuePausedStateKey, paused, "queue paused")
}

// IsShutdownDraining returns whether workers must stop claiming jobs while a
// daemon finishes a graceful shutdown.
func (db *DB) IsShutdownDraining() (bool, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM daemon_state WHERE key = ?`, shutdownDrainingStateKey).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("get shutdown draining state: %w", err)
	}
	return value == "true" || value == "1", nil
}

// SetShutdownDraining persists the recoverable claim gate used only while a
// daemon finishes a graceful shutdown.
func (db *DB) SetShutdownDraining(draining bool) error {
	return db.setDaemonBoolState(shutdownDrainingStateKey, draining, "shutdown draining")
}

// SetShutdownDrainingContext updates the shutdown claim gate within a caller's
// cleanup deadline.
func (db *DB) SetShutdownDrainingContext(ctx context.Context, draining bool) error {
	value := "false"
	if draining {
		value = "true"
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO daemon_state (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, shutdownDrainingStateKey, value)
	if err != nil {
		return fmt.Errorf("set shutdown draining state: %w", err)
	}
	return nil
}

func (db *DB) setDaemonBoolState(key string, enabled bool, label string) error {
	value := "false"
	if enabled {
		value = "true"
	}
	_, err := db.Exec(`
		INSERT INTO daemon_state (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, key, value)
	if err != nil {
		return fmt.Errorf("set %s state: %w", label, err)
	}
	return nil
}

// AnalysisScheduleLastRun returns the last schedule pass for a repo.
func (db *DB) AnalysisScheduleLastRun(ctx context.Context, repoID int64) (time.Time, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM daemon_state WHERE key = ?`, fmt.Sprintf("analysis_schedule_last_run:%d", repoID)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get analysis schedule last run: %w", err)
	}
	last, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse analysis schedule last run: %w", err)
	}
	return last, nil
}

// SetAnalysisScheduleLastRun records a schedule pass.
func (db *DB) SetAnalysisScheduleLastRun(ctx context.Context, repoID int64, last time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO daemon_state (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, fmt.Sprintf("analysis_schedule_last_run:%d", repoID), last.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("set analysis schedule last run: %w", err)
	}
	return nil
}

// AnalysisScheduleEntry watches a scheduled job or holds files to retry.
type AnalysisScheduleEntry struct {
	Type  string   `json:"type"`
	Files []string `json:"files"`
	Job   int64    `json:"job"`
}

type analysisScheduleEntryJSON struct {
	Type  string          `json:"type"`
	Files json.RawMessage `json:"files"`
	Job   int64           `json:"job"`
}

// AnalysisScheduleRetry returns watched jobs and retries for a repo.
func (db *DB) AnalysisScheduleRetry(ctx context.Context, repoID int64) ([]AnalysisScheduleEntry, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM daemon_state WHERE key = ?`, fmt.Sprintf("analysis_schedule_retry:%d", repoID)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get analysis schedule retry: %w", err)
	}
	var encoded []analysisScheduleEntryJSON
	if err := json.Unmarshal([]byte(value), &encoded); err != nil {
		return nil, fmt.Errorf("parse analysis schedule retry: %w", err)
	}
	var entries []AnalysisScheduleEntry
	for _, entry := range encoded {
		entries = append(entries, AnalysisScheduleEntry{Type: entry.Type, Files: decodeFileList(string(entry.Files)), Job: entry.Job})
	}
	return entries, nil
}

// SetAnalysisScheduleRetry records watched jobs and retries for a repo.
func (db *DB) SetAnalysisScheduleRetry(ctx context.Context, repoID int64, entries []AnalysisScheduleEntry) error {
	var encoded []analysisScheduleEntryJSON
	for _, entry := range entries {
		files, err := encodeAnalysisFiles(entry.Files)
		if err != nil {
			return fmt.Errorf("encode analysis schedule retry: %w", err)
		}
		if files == "" {
			files = "null"
		}
		encoded = append(encoded, analysisScheduleEntryJSON{Type: entry.Type, Files: json.RawMessage(files), Job: entry.Job})
	}
	value, err := json.Marshal(encoded)
	if err != nil {
		return fmt.Errorf("encode analysis schedule retry: %w", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO daemon_state (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, fmt.Sprintf("analysis_schedule_retry:%d", repoID), string(value))
	if err != nil {
		return fmt.Errorf("set analysis schedule retry: %w", err)
	}
	return nil
}
