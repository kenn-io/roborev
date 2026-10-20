package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
)

const agentActivityKey = "telemetry.agent_activity"

type agentActivityState struct {
	Day   string `json:"day"`
	Calls uint64 `json:"calls"`
	// CallIDs holds SHA-256 hashes of the day's counted call IDs; the hash bounds a client-supplied ID.
	CallIDs []string `json:"call_ids,omitempty"`
}

// RecordAgentCall persists a daily count capped at 101 and returns zero when saturated.
// A non-empty callID counts once per day, even across daemon restarts; a repeat reports
// repeated without counting. Callers must serialize updates and set a deadline to bound
// SQLite contention.
func (db *DB) RecordAgentCall(ctx context.Context, day, callID string) (calls uint64, repeated bool, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close() }()
	stored, err := getSyncState(ctx, conn, agentActivityKey)
	if err != nil {
		return 0, false, err
	}
	var state agentActivityState
	if stored != "" && json.Unmarshal([]byte(stored), &state) != nil {
		state = agentActivityState{}
	}
	if state.Day != day {
		state = agentActivityState{Day: day}
	}
	if state.Calls >= 101 {
		return 0, false, nil
	}
	if callID != "" {
		call := fmt.Sprintf("%x", sha256.Sum256([]byte(callID)))
		if slices.Contains(state.CallIDs, call) {
			return state.Calls, true, nil
		}
		state.CallIDs = append(state.CallIDs, call)
	}
	state.Calls++
	value, err := json.Marshal(state)
	if err != nil {
		return 0, false, err
	}
	restore, err := limitSQLiteBusyTimeout(ctx, conn)
	if err != nil {
		return 0, false, err
	}
	defer restore()
	err = setSyncState(ctx, conn, agentActivityKey, string(value))
	if err != nil {
		return 0, false, err
	}
	return state.Calls, false, nil
}
