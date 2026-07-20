package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

const queuePausedStateKey = "queue_paused"

type keyValueTable string

const (
	daemonStateTable keyValueTable = "daemon_state"
	syncStateTable   keyValueTable = "sync_state"
)

func upsertKeyValue(ctx context.Context, db bun.IDB, table keyValueTable, key, value string) error {
	var query *bun.InsertQuery
	switch table {
	case daemonStateTable:
		query = db.NewInsert().Model(&daemonStateRow{Key: key, Value: value})
	case syncStateTable:
		query = db.NewInsert().Model(&syncStateRow{Key: key, Value: value})
	}
	query = query.
		Column("key", "value").
		On("CONFLICT (key) DO UPDATE").
		Set("value = EXCLUDED.value")
	if table == daemonStateTable {
		query = query.Set("updated_at = datetime('now')")
	}
	_, err := query.Exec(ctx)
	return err
}

// IsQueuePaused returns whether daemon workers should stop claiming new jobs.
func (db *DB) IsQueuePaused() (bool, error) {
	var row daemonStateRow
	err := db.bun.NewSelect().
		Model(&row).
		Column("value").
		Where("key = ?", queuePausedStateKey).
		Scan(context.Background())
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get queue paused state: %w", err)
	}
	return row.Value == "true" || row.Value == "1", nil
}

// SetQueuePaused persists whether daemon workers should stop claiming new jobs.
func (db *DB) SetQueuePaused(paused bool) error {
	value := "false"
	if paused {
		value = "true"
	}
	if err := upsertKeyValue(context.Background(), db.bun, daemonStateTable, queuePausedStateKey, value); err != nil {
		return fmt.Errorf("set queue paused state: %w", err)
	}
	return nil
}
