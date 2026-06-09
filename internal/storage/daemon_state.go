package storage

import (
	"database/sql"
	"fmt"
)

const queuePausedStateKey = "queue_paused"

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
	value := "false"
	if paused {
		value = "true"
	}
	_, err := db.Exec(`
		INSERT INTO daemon_state (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, queuePausedStateKey, value)
	if err != nil {
		return fmt.Errorf("set queue paused state: %w", err)
	}
	return nil
}
