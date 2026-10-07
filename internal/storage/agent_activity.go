package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const agentActivityKey = "telemetry.agent_activity"

type agentActivityState struct {
	Day   string `json:"day"`
	Calls uint64 `json:"calls"`
}

// RecordAgentCall persists a daily count capped at 101 and returns zero when saturated.
// Callers must serialize updates and set a deadline to bound SQLite contention.
func (db *DB) RecordAgentCall(ctx context.Context, day string) (uint64, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	stored, err := getSyncState(ctx, conn, agentActivityKey)
	if err != nil {
		return 0, err
	}
	var state agentActivityState
	if stored != "" && json.Unmarshal([]byte(stored), &state) != nil {
		state = agentActivityState{}
	}
	if state.Day != day {
		state = agentActivityState{Day: day}
	}
	if state.Calls >= 101 {
		return 0, nil
	}
	state.Calls++
	value, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	var busyTimeout int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		return 0, err
	}
	defer func() {
		if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout)); err != nil {
			discardConn(conn)
		}
	}()
	// A contended SQLite writer ignored a one-second context; cap its busy wait at that deadline.
	if deadline, ok := ctx.Deadline(); ok {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", max(0, min(int(time.Until(deadline).Milliseconds()), busyTimeout)))); err != nil {
			return 0, err
		}
	}
	err = setSyncState(ctx, conn, agentActivityKey, string(value))
	if err != nil {
		return 0, err
	}
	return state.Calls, nil
}
