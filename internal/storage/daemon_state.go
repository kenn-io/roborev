package storage

import (
	"context"
	"database/sql"
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
